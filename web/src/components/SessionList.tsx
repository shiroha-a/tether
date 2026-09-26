import { isShell, sessionLabel, shortPath, type SessionStatus } from "../api";

interface Props {
  sessions: SessionStatus[];
  activeId: string | null;
  attention: Set<string>;
  onSelect: (id: string) => void;
  onNew: () => void;
}

type DotState = "attention" | "working" | "running" | "stopped";

const DOT_LABEL: Record<DotState, string> = {
  attention: "要対応",
  working: "作業中",
  running: "実行中",
  stopped: "停止中",
};

function dotState(s: SessionStatus, attention: boolean): DotState {
  if (!s.running) return "stopped";
  if (attention || s.activity === "waiting") return "attention";
  return s.activity === "working" ? "working" : "running";
}

/** Sidebar list of sessions, newest activity first. */
export default function SessionList({ sessions, activeId, attention, onSelect, onNew }: Props) {
  const sorted = [...sessions].sort(
    (a, b) => Number(b.running) - Number(a.running) || b.createdAt.localeCompare(a.createdAt),
  );
  return (
    <section className="session-list">
      <header>
        <span>セッション</span>
        <button className="btn small primary" onClick={onNew}>
          ＋ 新規
        </button>
      </header>
      <ul>
        {sorted.map((s) => (
          <li key={s.id}>
            <button className={"session-item" + (s.id === activeId ? " active" : "")} onClick={() => onSelect(s.id)}>
              <span
                className={"dot " + dotState(s, attention.has(s.id))}
                aria-label={DOT_LABEL[dotState(s, attention.has(s.id))]}
              />
              <span
                className={"kind-icon" + (isShell(s) ? " shell" : "")}
                aria-label={isShell(s) ? "ターミナル" : "Claude Code"}
              >
                {isShell(s) ? "$" : "✻"}
              </span>
              <span className="session-text">
                <span className="session-name">{sessionLabel(s)}</span>
                <span className="session-cwd">{shortPath(s.cwd)}</span>
              </span>
              {s.clients > 1 && (
                <span className="clients" title="接続中の端末数">
                  {s.clients}
                </span>
              )}
            </button>
          </li>
        ))}
        {sessions.length === 0 && <li className="muted small pad">まだセッションはありません</li>}
      </ul>
    </section>
  );
}
