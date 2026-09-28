import { useEffect, useState } from "react";
import { api, type AgentProgress, type Hunk, type ImageRef, type Todo } from "../api";
import { diffRows, elapsed, type BackgroundKind, type BackgroundState, type BackgroundTask } from "../chat";
import { Modal } from "./Modal";

/** Unified diff of an Edit/MultiEdit/Write call. */
export function DiffView({ patch }: { patch: Hunk[] }) {
  return (
    <div className="diff">
      {patch.map((h, i) => (
        <div key={i} className="diff-hunk">
          {h.oldStart > 0 && (
            <div className="diff-head">
              @@ -{h.oldStart} +{h.newStart} @@
            </div>
          )}
          {diffRows(h).map((r, j) => (
            <div key={j} className={"diff-line " + r.type}>
              <span className="diff-no">{r.oldNo ?? ""}</span>
              <span className="diff-no">{r.newNo ?? ""}</span>
              <span className="diff-mark">{r.type === "add" ? "+" : r.type === "del" ? "-" : " "}</span>
              <span className="diff-text">{r.text}</span>
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

const TASK_MARK: Record<string, string> = { completed: "done", in_progress: "doing" };

/** Claude Code's task list as a checklist. */
export function TaskChecklist({ tasks }: { tasks: Todo[] }) {
  return (
    <ul className="task-list">
      {tasks.map((t, i) => (
        <li key={t.id ?? i} className={"task " + (TASK_MARK[t.status ?? ""] ?? "todo")}>
          <span className="task-mark" aria-hidden="true" />
          <span className="task-text">{t.status === "in_progress" ? t.activeForm || t.content : t.content}</span>
        </li>
      ))}
    </ul>
  );
}

const KIND_LABEL: Record<BackgroundKind, string> = { shell: "シェル", agent: "エージェント", monitor: "監視" };
const STATE_LABEL: Record<BackgroundState, string> = {
  running: "実行中",
  completed: "完了",
  failed: "失敗",
  stopped: "停止",
};

/** How many finished background tasks stay listed under the running ones. */
const RECENT_DONE = 5;

/**
 * The bar pinned above the chat: progress of the task list and the shells,
 * agents and monitors Claude started in the background.
 */
export function ChatStatusBar({
  tasks,
  background,
  agents,
}: {
  tasks: Todo[];
  background: BackgroundTask[];
  agents: Map<string, AgentProgress>;
}) {
  const [now, setNow] = useState(() => Date.now());
  const running = background.filter((b) => b.state === "running");
  const done = background
    .filter((b) => b.state !== "running")
    .slice(-RECENT_DONE)
    .reverse();
  // 経過時間の表示を進める
  useEffect(() => {
    if (running.length === 0) return;
    const t = window.setInterval(() => setNow(Date.now()), 10_000);
    return () => window.clearInterval(t);
  }, [running.length]);

  const completed = tasks.filter((t) => t.status === "completed").length;
  const current = tasks.find((t) => t.status === "in_progress");
  const showTasks = tasks.length > 0 && completed < tasks.length;
  if (!showTasks && running.length === 0) return null;

  return (
    <div className="chat-status">
      {showTasks && (
        <details className="chat-status-item">
          <summary>
            <span className="status-label">
              タスク {completed}/{tasks.length}
            </span>
            <span className="status-detail">{current ? current.activeForm || current.content : ""}</span>
          </summary>
          <TaskChecklist tasks={tasks} />
        </details>
      )}
      {running.length > 0 && (
        <details className="chat-status-item">
          <summary>
            <span className="status-label running">実行中 {running.length}</span>
            <span className="status-detail">{running.map((b) => b.description).join("、")}</span>
          </summary>
          <ul className="bg-list">
            {[...running, ...done].map((b) => {
              const a = agents.get(b.toolId);
              return (
                <li key={b.toolId} className={"bg-task " + b.state}>
                  <div className="bg-head">
                    <span className="bg-kind">{KIND_LABEL[b.kind]}</span>
                    <span className="bg-desc">{b.description}</span>
                    <span className="bg-state">
                      {STATE_LABEL[b.state]}
                      {b.state === "running" && ` ${elapsed(b.startedAt, now)}`}
                    </span>
                  </div>
                  {b.state === "running" && a && a.toolCount > 0 && (
                    <div className="bg-sub">
                      ツール{a.toolCount}回・{a.lastTool} {a.lastSummary}
                    </div>
                  )}
                  {b.state !== "running" && b.summary && <div className="bg-sub">{b.summary}</div>}
                </li>
              );
            })}
          </ul>
        </details>
      )}
    </div>
  );
}

/** Thumbnails of images in the transcript; tapping one enlarges it. */
export function TranscriptImages({ sessionId, refs }: { sessionId: string; refs: ImageRef[] }) {
  const [open, setOpen] = useState<string | null>(null);
  return (
    <div className="chat-images">
      {refs.map((r) => {
        const src = api.transcriptImageUrl(sessionId, r);
        return (
          <button key={`${r.line}:${r.index}:${r.sub}`} className="chat-thumb" onClick={() => setOpen(src)}>
            <img src={src} alt="画像" loading="lazy" />
          </button>
        );
      })}
      {open && <ImageViewer src={open} onClose={() => setOpen(null)} />}
    </div>
  );
}

export function ImageViewer({ src, onClose }: { src: string; onClose: () => void }) {
  return (
    <Modal title="画像" onClose={onClose} wide>
      <img className="preview-image" src={src} alt="画像" />
    </Modal>
  );
}

/** An image attached in the composer, waiting to be sent. */
export interface Attachment {
  file: File;
  url: string;
}

/** Thumbnails of attached images with a button to remove each. */
export function AttachmentStrip({ items, onRemove }: { items: Attachment[]; onRemove: (i: number) => void }) {
  if (items.length === 0) return null;
  return (
    <div className="chat-attachments">
      {items.map((a, i) => (
        <div key={a.url} className="chat-attachment">
          <img src={a.url} alt={a.file.name} />
          <button className="chat-attachment-remove" aria-label="添付を外す" onClick={() => onRemove(i)}>
            ×
          </button>
        </div>
      ))}
    </div>
  );
}
