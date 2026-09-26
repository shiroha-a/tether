import { useEffect, useState } from "react";
import { api, type Usage } from "../api";

const WINDOW_LABELS: Record<string, string> = {
  five_hour: "5時間",
  seven_day: "週間",
  seven_day_opus: "週間 Opus",
  seven_day_sonnet: "週間 Sonnet",
};

function resetIn(iso: string | null): string {
  if (!iso) return "";
  const ms = new Date(iso).getTime() - Date.now();
  if (ms <= 0) return "まもなくリセット";
  const h = Math.floor(ms / 3_600_000);
  const m = Math.floor((ms % 3_600_000) / 60_000);
  if (h >= 24) return `${Math.floor(h / 24)}日${h % 24}時間後にリセット`;
  return h > 0 ? `${h}時間${m}分後にリセット` : `${m}分後にリセット`;
}

/** Compact usage meters for the sidebar. Refreshes every 5 minutes. */
export default function UsageWidget() {
  const [usage, setUsage] = useState<Usage | null>(null);
  const [loading, setLoading] = useState(false);

  const load = (force = false) => {
    setLoading(true);
    api
      .usage(force)
      .then(setUsage)
      .catch(() => setUsage(null))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    load();
    const t = window.setInterval(() => load(), 5 * 60_000);
    return () => window.clearInterval(t);
  }, []);

  // APIは内部コードネームのような未知のフィールドも返すため、意味の分かるウィンドウだけを出す
  const order = (k: string) => (k in WINDOW_LABELS ? Object.keys(WINDOW_LABELS).indexOf(k) : 99);
  const windows = Object.entries(usage?.windows ?? {})
    .filter(([k]) => k === "five_hour" || k.startsWith("seven_day"))
    .sort(([a], [b]) => order(a) - order(b));

  return (
    <section className="usage">
      <header>
        <span>使用量{usage?.plan ? `（${usage.plan}）` : ""}</span>
        <button className="icon-btn" onClick={() => load(true)} disabled={loading} aria-label="使用量を更新">
          ↻
        </button>
      </header>
      {usage && !usage.available && <p className="muted small">取得できません: {usage.error}</p>}
      {windows.map(([key, w]) => {
        const pct = Math.round(w.utilization);
        const level = pct >= 90 ? "crit" : pct >= 70 ? "warn" : "ok";
        return (
          <div className="meter" key={key}>
            <div className="meter-row">
              <span>{WINDOW_LABELS[key] ?? key}</span>
              <span className="num">{pct}%</span>
            </div>
            <div className="bar" role="meter" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
              <div className={"fill " + level} style={{ width: `${Math.min(100, pct)}%` }} />
            </div>
            <div className="muted small">{resetIn(w.resets_at)}</div>
          </div>
        );
      })}
    </section>
  );
}
