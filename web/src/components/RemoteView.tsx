import { useCallback, useEffect, useState } from "react";
import { api, timeAgo, type PeerAuditEntry, type PeerClient, type PeerPolicy, type RemoteState } from "../api";
import { copyText } from "../clipboard";
import { allowedOps, opLabel } from "../remote";

const POLICY_FIELDS: { key: "status" | "files" | "exec" | "delegate"; label: string; hint: string }[] = [
  { key: "status", label: "サーバの状態", hint: "負荷・メモリ・ディスク" },
  { key: "files", label: "ファイルの一覧と閲覧", hint: "このマシンのルート内のみ" },
  { key: "exec", label: "コマンドの実行", hint: "毎回、相手の画面で承認が必要" },
  { key: "delegate", label: "作業の依頼", hint: "このマシンにClaude Codeのセッションを作る" },
];

const DEFAULT_POLICY: PeerPolicy = { status: true, files: true, exec: false, delegate: false, delegateMode: "default" };

/** Settings for linking tether on other machines. `version` bumps on remote.changed. */
export default function RemoteView({ version }: { version: number }) {
  const [state, setState] = useState<RemoteState | null>(null);
  const [audit, setAudit] = useState<PeerAuditEntry[]>([]);
  const [error, setError] = useState("");

  const load = useCallback(() => {
    api
      .remote()
      .then(setState)
      .catch((e) => setError((e as Error).message));
    api
      .peerAudit()
      .then(setAudit)
      .catch(() => {});
  }, []);
  useEffect(load, [load, version]);

  const run = async (fn: () => Promise<unknown>) => {
    setError("");
    try {
      await fn();
      load();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  if (!state)
    return (
      <div className="remote">{error ? <p className="error">{error}</p> : <p className="muted">読み込み中...</p>}</div>
    );

  return (
    <div className="remote">
      <h1>マシンの連携</h1>
      <p className="muted small">
        別のマシンのtetherと連携すると、このマシンのClaudeが相手のマシンの状態を調べたり、コマンドを実行したりできます。
        Claudeが相手のマシンを使うたびに、この画面で承認を求めます。
      </p>
      {error && <p className="error">{error}</p>}

      <Remotes state={state} run={run} />
      <Clients state={state} run={run} />

      <section className="remote-section">
        <h2>まとめて許可中</h2>
        {state.grants.length === 0 ? (
          <p className="muted small">ありません</p>
        ) : (
          <ul className="remote-list">
            {state.grants.map((g) => (
              <li key={g.id} className="remote-item">
                <div>
                  <strong>{g.machine}</strong>の読み取り
                  <span className="muted small">
                    （セッション {g.session.slice(0, 8)}、{new Date(g.expiresAt).toLocaleTimeString()}まで）
                  </span>
                </div>
                <button className="btn small" onClick={() => run(() => api.revokeGrant(g.id))}>
                  取り消す
                </button>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="remote-section">
        <h2>このマシンへの操作の記録</h2>
        {audit.length === 0 ? (
          <p className="muted small">まだありません</p>
        ) : (
          <ul className="audit-list">
            {audit.slice(0, 50).map((e, i) => (
              <li key={i} className={e.ok ? "" : "failed"}>
                <span className="muted small">{timeAgo(e.at)}</span>
                <strong>{e.client}</strong>
                <span>{opLabel(e.op)}</span>
                {e.detail && <code>{e.detail}</code>}
                {!e.ok && <span className="audit-error">{e.error}</span>}
              </li>
            ))}
          </ul>
        )}
      </section>

      <p className="muted small remote-note">
        注意:
        tetherの認証（TETHER_AUTH）がoffのときは、同じマシンの同じユーザーのプロセスやtailnet内の端末が、画面と同じAPIを使えます。
        マシンごとに厳密に分けたい場合は、各マシンでTETHER_AUTH=onにしてください。
      </p>
    </div>
  );
}

type Run = (fn: () => Promise<unknown>) => Promise<void>;

function Remotes({ state, run }: { state: RemoteState; run: Run }) {
  const [name, setName] = useState("");
  const [code, setCode] = useState("");
  return (
    <section className="remote-section">
      <h2>このマシンから使うマシン（接続先）</h2>
      {state.remotes.length === 0 ? (
        <p className="muted small">
          まだありません。相手のマシンの「このマシンを使わせるマシン」で接続コードを作り、下に貼り付けてください。
        </p>
      ) : (
        <ul className="remote-list">
          {state.remotes.map((r) => (
            <li key={r.id} className="remote-item">
              <div className="remote-main">
                <strong>{r.name}</strong>
                <span className="muted small">{r.url}</span>
                <span className="chips">
                  {allowedOps(r.policy).map((o) => (
                    <span key={o} className="chip">
                      {o}
                    </span>
                  ))}
                  {allowedOps(r.policy).length === 0 && <span className="muted small">許可された操作はありません</span>}
                </span>
              </div>
              <div className="remote-actions">
                <button
                  className="btn small"
                  onClick={() => run(() => api.refreshRemote(r.id))}
                  title="相手の許可範囲を取り直す"
                >
                  更新
                </button>
                <button
                  className="btn small danger"
                  onClick={() => confirm(`${r.name}との連携を削除しますか？`) && run(() => api.deleteRemote(r.id))}
                >
                  削除
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
      <form
        className="remote-form"
        onSubmit={(e) => {
          e.preventDefault();
          run(async () => {
            await api.addRemote(name, code);
            setName("");
            setCode("");
          });
        }}
      >
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="名前（例: vps）"
          aria-label="接続先の名前"
        />
        <input
          className="mono"
          value={code}
          onChange={(e) => setCode(e.target.value)}
          placeholder="接続コード（tether-pair:...）"
          aria-label="接続コード"
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
        />
        <button className="btn primary" disabled={!name.trim() || !code.trim()}>
          接続先を追加
        </button>
      </form>
      <p className="muted small">
        接続先を追加・削除したら、Claude Codeのセッションを停止して開き直すと使えるようになります。
      </p>
    </section>
  );
}

function Clients({ state, run }: { state: RemoteState; run: Run }) {
  const [name, setName] = useState("");
  const [policy, setPolicy] = useState<PeerPolicy>(DEFAULT_POLICY);
  const [created, setCreated] = useState<{ name: string; code: string } | null>(null);
  const [copied, setCopied] = useState(false);

  return (
    <section className="remote-section">
      <h2>このマシンを使わせるマシン（接続元）</h2>
      {state.clients.length === 0 ? (
        <p className="muted small">まだありません。</p>
      ) : (
        <ul className="remote-list">
          {state.clients.map((c) => (
            <ClientItem key={c.id} client={c} modes={state.delegateModes} run={run} />
          ))}
        </ul>
      )}
      {created ? (
        <div className="pair-code">
          <p>
            <strong>{created.name}</strong>の画面（マシンの連携 →
            接続先）に、この接続コードを貼り付けてください。コードはこの1回だけ表示されます。
          </p>
          <code className="mono">{created.code}</code>
          <div className="remote-actions">
            <button className="btn small" onClick={async () => setCopied(await copyText(created.code))}>
              {copied ? "コピーしました" : "コピー"}
            </button>
            <button className="btn small primary" onClick={() => setCreated(null)}>
              閉じる
            </button>
          </div>
        </div>
      ) : (
        <form
          className="remote-form"
          onSubmit={(e) => {
            e.preventDefault();
            run(async () => {
              const res = await api.addPeerClient(name, policy, location.origin);
              setCreated({ name: res.client.name, code: res.code });
              setCopied(false);
              setName("");
              setPolicy(DEFAULT_POLICY);
            });
          }}
        >
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="相手のマシンの名前（例: laptop）"
            aria-label="接続元の名前"
          />
          <PolicyEditor policy={policy} modes={state.delegateModes} onChange={setPolicy} />
          <button className="btn primary" disabled={!name.trim()}>
            接続コードを作る
          </button>
        </form>
      )}
    </section>
  );
}

function ClientItem({ client, modes, run }: { client: PeerClient; modes: string[]; run: Run }) {
  return (
    <li className="remote-item column">
      <div className="remote-main">
        <strong>{client.name}</strong>
        <span className="muted small">
          {client.lastSeenAt ? `最終接続 ${timeAgo(client.lastSeenAt)}` : "まだ接続されていません"}
        </span>
      </div>
      <PolicyEditor policy={client.policy} modes={modes} onChange={(p) => run(() => api.setPeerPolicy(client.id, p))} />
      <div className="remote-actions">
        <button
          className="btn small danger"
          onClick={() =>
            confirm(`${client.name}からの接続を削除しますか？ 接続コードはすぐに使えなくなります。`) &&
            run(() => api.deletePeerClient(client.id))
          }
        >
          削除
        </button>
      </div>
    </li>
  );
}

function PolicyEditor({
  policy,
  modes,
  onChange,
}: {
  policy: PeerPolicy;
  modes: string[];
  onChange: (p: PeerPolicy) => void;
}) {
  return (
    <div className="policy">
      {POLICY_FIELDS.map((f) => (
        <label key={f.key} className="policy-item">
          <input
            type="checkbox"
            checked={policy[f.key]}
            onChange={(e) => onChange({ ...policy, [f.key]: e.target.checked })}
          />
          <span>
            {f.label}
            <span className="muted small">（{f.hint}）</span>
          </span>
        </label>
      ))}
      {policy.delegate && (
        <label className="policy-item">
          <span>依頼で作るセッションの許可モード</span>
          <select
            value={policy.delegateMode ?? "default"}
            onChange={(e) => onChange({ ...policy, delegateMode: e.target.value })}
          >
            {modes.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        </label>
      )}
    </div>
  );
}
