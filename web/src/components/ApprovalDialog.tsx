import { useEffect, useState } from "react";
import { api, type RemoteApproval } from "../api";
import { opLabel, remaining } from "../remote";
import { Modal } from "./Modal";

/** Minutes a read-only grant lasts when the user picks "allow for a while". */
export const GRANT_MINUTES = 15;

/**
 * Asks the user to approve a Claude Code session's request to use another
 * machine. The dialog stays until the user answers or the request expires,
 * so a request is never approved or denied by accident.
 */
export default function ApprovalDialog({ approval, more }: { approval: RemoteApproval; more: number }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(t);
  }, []);

  useEffect(() => {
    setBusy(false);
    setError("");
  }, [approval.id]);

  const decide = async (allow: boolean, grantMinutes = 0) => {
    setBusy(true);
    setError("");
    try {
      await api.decideApproval(approval.id, allow, grantMinutes);
    } catch (e) {
      setError((e as Error).message);
      setBusy(false);
    }
  };

  return (
    // 誤って閉じて許可・拒否したことにならないよう、Escや背景のクリックでは閉じない
    <Modal title="別のマシンへの操作の承認" onClose={() => {}} closable={false}>
      <div className="approval">
        <p>
          セッション<strong>「{approval.sessionLabel}」</strong>のClaudeが、
          <strong>{approval.machine}</strong>で<strong>{opLabel(approval.op)}</strong>をしようとしています。
        </p>
        <pre className="approval-detail">{approval.detail}</pre>
        {!approval.read && (
          <p className="approval-warn">この操作は{approval.machine}の上で実行されます。内容を確認してください。</p>
        )}
        {error && <p className="error">{error}</p>}
        <div className="approval-actions">
          <button className="btn danger" disabled={busy} onClick={() => decide(false)}>
            拒否
          </button>
          {approval.read && (
            <button className="btn" disabled={busy} onClick={() => decide(true, GRANT_MINUTES)}>
              {GRANT_MINUTES}分間、読み取りを許可
            </button>
          )}
          <button className="btn primary" disabled={busy} onClick={() => decide(true)}>
            {approval.read ? "今回だけ許可" : "許可"}
          </button>
        </div>
        <p className="muted small">
          残り{remaining(approval.expiresAt, now)}で自動的に拒否します
          {more > 0 && `（ほかに${more}件の承認待ち）`}
        </p>
      </div>
    </Modal>
  );
}
