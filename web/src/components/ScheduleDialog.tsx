import { useCallback, useEffect, useState } from "react";
import { api, type ScheduleItem } from "../api";
import { Modal } from "./Modal";

const STATUS_LABEL: Record<ScheduleItem["status"], string> = {
  pending: "待機中",
  running: "送信中",
  done: "送信済み",
  failed: "失敗",
};

/** Formats a Date for <input type="datetime-local"> in the browser's timezone. */
function toLocalInput(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** Lists and creates scheduled prompts for one session. */
export default function ScheduleDialog({
  sessionId,
  version,
  onClose,
}: {
  sessionId: string;
  version: number;
  onClose: () => void;
}) {
  const [items, setItems] = useState<ScheduleItem[]>([]);
  const [prompt, setPrompt] = useState("");
  const [runAt, setRunAt] = useState(() => toLocalInput(new Date(Date.now() + 60 * 60_000)));
  const [error, setError] = useState("");

  const load = useCallback(() => {
    api
      .schedules(sessionId)
      .then(setItems)
      .catch((e) => setError(e.message));
  }, [sessionId]);
  useEffect(load, [load, version]);

  const add = async () => {
    setError("");
    // datetime-localはタイムゾーンを持たないので、ブラウザのローカル時刻として解釈してUTCで送る
    const when = new Date(runAt);
    if (Number.isNaN(when.getTime())) {
      setError("日時が正しくありません");
      return;
    }
    try {
      await api.addSchedule(sessionId, prompt, when);
      setPrompt("");
      load();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  return (
    <Modal title="予約プロンプト" onClose={onClose}>
      <div className="form">
        <label>
          <span>送信日時</span>
          <input type="datetime-local" value={runAt} onChange={(e) => setRunAt(e.target.value)} />
        </label>
        <label>
          <span>プロンプト</span>
          <textarea
            rows={4}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder="例: 続きをお願いします"
          />
        </label>
        <p className="muted small">停止中のセッションは、送信時刻に自動で再開してから送信します。</p>
        {error && <p className="error">{error}</p>}
        <button className="btn primary" onClick={add} disabled={!prompt.trim()}>
          予約する
        </button>
      </div>
      <ul className="schedule-list">
        {items.map((it) => (
          <li key={it.id} className={"schedule " + it.status}>
            <div className="schedule-meta">
              <span>{new Date(it.runAt).toLocaleString()}</span>
              <span className={"badge " + it.status}>{STATUS_LABEL[it.status]}</span>
              <button
                className="icon-btn"
                aria-label="削除"
                onClick={() =>
                  api
                    .deleteSchedule(it.id)
                    .then(load)
                    .catch((e) => setError(e.message))
                }
              >
                ✕
              </button>
            </div>
            <pre className="schedule-prompt">{it.prompt}</pre>
            {it.error && <p className="error small">{it.error}</p>}
          </li>
        ))}
        {items.length === 0 && <li className="muted small">予約はありません</li>}
      </ul>
    </Modal>
  );
}
