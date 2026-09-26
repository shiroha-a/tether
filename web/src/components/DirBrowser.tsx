import { useCallback, useEffect, useRef, useState } from "react";
import {
  api,
  formatBytes,
  type FsEntry,
  type FsListing,
  type LaunchOptions,
  type ServerConfig,
  type SessionKind,
} from "../api";
import { initialBrowsePath, loadBrowsePath, loadPrefs, savePrefs, saveBrowsePath } from "../prefs";
import PreviewDialog from "./PreviewDialog";

interface Props {
  config: ServerConfig;
  initialPath?: string;
  recentDirs: string[];
  onLaunch: (opts: LaunchOptions) => Promise<void>;
}

/** Directory browser with file operations and the "launch Claude Code / terminal here" panel. */
export default function DirBrowser({ config, initialPath, recentDirs, onLaunch }: Props) {
  const prefs = loadPrefs();
  const [path, setPath] = useState(() => initialBrowsePath(initialPath, loadBrowsePath(), config.root));
  // 記憶から復元したフォルダ。これが開けなかったときだけルートに戻す（自分で開いたフォルダのエラーはそのまま見せる）
  const restoredRef = useRef<string | null>(path !== config.root && !initialPath ? path : null);
  const [listing, setListing] = useState<FsListing | null>(null);
  const [hidden, setHidden] = useState(prefs.hidden);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [preview, setPreview] = useState<FsEntry | null>(null);
  const [kind, setKind] = useState<SessionKind>(prefs.kind);
  const [model, setModel] = useState(prefs.model);
  const [permissionMode, setPermissionMode] = useState(prefs.permissionMode);
  const [label, setLabel] = useState("");
  const [dragging, setDragging] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);

  const load = useCallback(() => {
    setError("");
    api
      .list(path, hidden)
      .then((l) => {
        setListing(l);
        // 開けたフォルダだけを記憶する（次にフォルダ画面を開いたときにここから表示する）
        saveBrowsePath(l.path);
      })
      .catch((e) => {
        // 記憶していたフォルダが消えた・開けなくなった場合は、ルートに戻して開き直す
        if (restoredRef.current === path && e.status !== 401) {
          restoredRef.current = null;
          setPath(config.root);
          return;
        }
        setError(e.message);
      });
  }, [path, hidden, config.root]);

  useEffect(load, [load]);
  useEffect(() => savePrefs({ kind, model, permissionMode, hidden }), [kind, model, permissionMode, hidden]);
  useEffect(() => {
    if (initialPath) setPath(initialPath);
  }, [initialPath]);

  const crumbs = (() => {
    const cur = listing?.path ?? path;
    const rel = cur.startsWith(config.root) ? cur.slice(config.root.length) : cur;
    const parts = rel.split("/").filter(Boolean);
    const out = [{ name: config.root.split("/").pop() || "/", path: config.root }];
    let acc = config.root;
    for (const p of parts) {
      acc = acc.replace(/\/$/, "") + "/" + p;
      out.push({ name: p, path: acc });
    }
    return out;
  })();

  const mkdir = async () => {
    const name = prompt("新しいフォルダ名");
    if (!name || !listing) return;
    try {
      const res = await api.mkdir(listing.path.replace(/\/$/, "") + "/" + name);
      setPath(res.path);
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const upload = async (files: FileList | File[] | null) => {
    if (!files || !listing || files.length === 0) return;
    setBusy(true);
    setError("");
    try {
      await api.upload(listing.path, files);
    } catch (e) {
      const err = e as { status?: number; message: string };
      if (err.status === 409 && confirm("同名のファイルがあります。上書きしますか？")) {
        await api.upload(listing.path, files, true).catch((e2) => setError(e2.message));
      } else {
        setError(err.message);
      }
    } finally {
      setBusy(false);
      load();
    }
  };

  const launch = async () => {
    if (!listing) return;
    if (
      kind === "claude" &&
      permissionMode === "bypassPermissions" &&
      !confirm("bypassPermissionsは、確認なしで全ての操作を実行します。起動しますか？")
    ) {
      return;
    }
    setBusy(true);
    try {
      await onLaunch(
        kind === "shell"
          ? { kind, cwd: listing.path, label }
          : { kind, cwd: listing.path, model, permissionMode, label },
      );
      setLabel("");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div
      className={"browser" + (dragging ? " dragging" : "")}
      onDragOver={(e) => {
        e.preventDefault();
        setDragging(true);
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={(e) => {
        e.preventDefault();
        setDragging(false);
        upload(e.dataTransfer.files);
      }}
    >
      <section className="launch-panel">
        <div className="launch-target" title={listing?.path}>
          <span className="muted">起動先</span>
          <strong>{listing?.path ?? path}</strong>
        </div>
        <div className="kind-switch" role="radiogroup" aria-label="起動するもの">
          {(
            [
              ["claude", "Claude Code"],
              ["shell", "ターミナル"],
            ] as const
          ).map(([k, text]) => (
            <button
              key={k}
              role="radio"
              aria-checked={kind === k}
              className={"kind-option" + (kind === k ? " on" : "")}
              onClick={() => setKind(k)}
            >
              {text}
            </button>
          ))}
        </div>
        <div className="launch-fields">
          {kind === "claude" && (
            <>
              <label>
                <span>モデル</span>
                <select value={model} onChange={(e) => setModel(e.target.value)}>
                  {config.models.map((m) => (
                    <option key={m} value={m}>
                      {m || "既定"}
                    </option>
                  ))}
                </select>
              </label>
              <label>
                <span>権限モード</span>
                <select value={permissionMode} onChange={(e) => setPermissionMode(e.target.value)}>
                  {config.permissionModes.map((m) => (
                    <option key={m} value={m}>
                      {m}
                    </option>
                  ))}
                </select>
              </label>
            </>
          )}
          <label className="grow">
            <span>名前（任意）</span>
            <input value={label} onChange={(e) => setLabel(e.target.value)} placeholder="例: API改修" />
          </label>
          <button className="btn primary" onClick={launch} disabled={busy || !listing}>
            {kind === "shell" ? "ここでターミナルを開く" : "ここでClaude Codeを起動"}
          </button>
        </div>
        {kind === "claude" && permissionMode === "bypassPermissions" && (
          <p className="warn">bypassPermissionsでは、ツールの実行確認がすべてスキップされます</p>
        )}
      </section>

      {recentDirs.length > 0 && (
        <div className="recent">
          <span className="muted">最近のフォルダ</span>
          {recentDirs.map((d) => (
            <button key={d} className="chip" onClick={() => setPath(d)} title={d}>
              {d.split("/").pop()}
            </button>
          ))}
        </div>
      )}

      <div className="browser-toolbar">
        <nav className="crumbs" aria-label="パス">
          {crumbs.map((c, i) => (
            <span key={c.path}>
              {i > 0 && <span className="sep">/</span>}
              <button className="crumb" onClick={() => setPath(c.path)}>
                {c.name}
              </button>
            </span>
          ))}
        </nav>
        <div className="toolbar-actions">
          <label className="toggle">
            <input type="checkbox" checked={hidden} onChange={(e) => setHidden(e.target.checked)} />
            隠しファイル
          </label>
          <button className="btn" onClick={mkdir}>
            フォルダ作成
          </button>
          <button className="btn" onClick={() => fileInput.current?.click()} disabled={busy}>
            アップロード
          </button>
          <input
            ref={fileInput}
            type="file"
            multiple
            hidden
            onChange={(e) => {
              upload(e.target.files);
              e.target.value = "";
            }}
          />
        </div>
      </div>

      {error && <p className="error">{error}</p>}

      <ul className="entries">
        {listing?.parent && (
          <li>
            <button className="entry" onClick={() => setPath(listing.parent)}>
              <span className="entry-icon">↰</span>
              <span className="entry-name">..</span>
            </button>
          </li>
        )}
        {listing?.entries.map((e) => (
          <li key={e.path}>
            <button
              className="entry"
              onClick={() => (e.isDir ? setPath(e.path) : setPreview(e))}
              title={e.isLink ? "シンボリックリンク" : undefined}
            >
              <span className="entry-icon">{e.isDir ? "▸" : "·"}</span>
              <span className="entry-name">
                {e.name}
                {e.isDir ? "/" : ""}
                {e.isLink && <span className="muted"> ↪</span>}
              </span>
              {!e.isDir && <span className="entry-size">{formatBytes(e.size)}</span>}
            </button>
          </li>
        ))}
        {listing && listing.entries.length === 0 && <li className="muted empty">空のフォルダです</li>}
      </ul>
      {preview && <PreviewDialog entry={preview} onClose={() => setPreview(null)} />}
    </div>
  );
}
