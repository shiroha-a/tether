import { useEffect, useMemo, useState } from "react";
import {
  api,
  formatBytes,
  formatDuration,
  isShell,
  sessionLabel,
  shortPath,
  timeAgo,
  type LaunchOptions,
  type Notice,
  type SessionStatus,
  type SystemInfo,
} from "../api";
import { loadPrefs } from "../prefs";
import AboutDialog from "./AboutDialog";
import UsageWidget from "./UsageWidget";

interface Props {
  sessions: SessionStatus[];
  attention: Set<string>;
  recentDirs: string[];
  noticeVersion: number;
  onOpen: (id: string) => void;
  onNew: () => void;
  /** Opens the folder browser, optionally at a folder. */
  onBrowse: (path?: string) => void;
  onLaunch: (opts: LaunchOptions) => Promise<void>;
  onStop: (s: SessionStatus) => Promise<void>;
  onDelete: (s: SessionStatus) => Promise<void>;
}

const SEEN_KEY = "tether.noticesSeenAt";

type Badge = { text: string; tone: "working" | "waiting" | "idle" | "running" | "stopped" };

function badgeOf(s: SessionStatus, attention: boolean): Badge {
  if (!s.running) return { text: "停止中", tone: "stopped" };
  if (s.activity === "waiting" || attention) return { text: "入力待ち", tone: "waiting" };
  if (s.activity === "working") return { text: "作業中", tone: "working" };
  if (s.activity === "idle") return { text: "完了", tone: "idle" };
  return { text: "実行中", tone: "running" };
}

// 要対応のものを先頭に出し、同じ優先度なら最近動いたものから並べる
const PRIORITY: Record<Badge["tone"], number> = { waiting: 0, working: 1, idle: 2, running: 3, stopped: 4 };

// サーバの時刻は"+09:00"付き、ブラウザで作る時刻は"Z"付きなので、文字列ではなくミリ秒で比較する
const ms = (iso?: string) => (iso ? Date.parse(iso) || 0 : 0);

function lastTouched(s: SessionStatus): number {
  return Math.max(ms(s.activityAt), ms(s.lastActiveAt));
}

/** Dashboard shown at "/": sessions, quick launch, notifications, usage and host health. */
export default function Home(props: Props) {
  const { sessions, attention, recentDirs, noticeVersion, onOpen, onNew, onBrowse, onLaunch, onStop, onDelete } = props;
  const [notices, setNotices] = useState<Notice[]>([]);
  const [system, setSystem] = useState<SystemInfo | null>(null);
  const [menuFor, setMenuFor] = useState<string | null>(null);
  const [busyDir, setBusyDir] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [aboutOpen, setAboutOpen] = useState(false);
  // 開いた時点の既読時刻で未読を判定し、表示したら既読にする
  const [seenAt] = useState(() => {
    try {
      return localStorage.getItem(SEEN_KEY) ?? "";
    } catch {
      return "";
    }
  });

  useEffect(() => {
    api
      .notifications()
      .then(setNotices)
      .catch(() => {});
  }, [noticeVersion]);

  useEffect(() => {
    try {
      localStorage.setItem(SEEN_KEY, new Date().toISOString());
    } catch {
      // 既読管理ができなくても表示には影響しない
    }
  }, [notices]);

  useEffect(() => {
    const load = () =>
      api
        .system()
        .then(setSystem)
        .catch(() => {});
    load();
    const t = window.setInterval(load, 30_000);
    return () => window.clearInterval(t);
  }, []);

  useEffect(() => {
    if (!menuFor) return;
    const close = () => setMenuFor(null);
    window.addEventListener("click", close);
    return () => window.removeEventListener("click", close);
  }, [menuFor]);

  const sorted = useMemo(
    () =>
      [...sessions].sort((a, b) => {
        const pa = PRIORITY[badgeOf(a, attention.has(a.id)).tone];
        const pb = PRIORITY[badgeOf(b, attention.has(b.id)).tone];
        return pa - pb || lastTouched(b) - lastTouched(a);
      }),
    [sessions, attention],
  );

  const quickLaunch = async (cwd: string, kind: "claude" | "shell") => {
    const p = loadPrefs();
    setBusyDir(cwd + kind);
    setError("");
    try {
      await onLaunch(
        kind === "shell" ? { kind, cwd } : { kind, cwd, model: p.model, permissionMode: p.permissionMode },
      );
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusyDir(null);
    }
  };

  const sessionIds = new Set(sessions.map((s) => s.id));

  return (
    <div className="home">
      <div className="home-head">
        <div className="home-title">
          <h1>ホーム</h1>
          <button className="btn ghost small" onClick={() => setAboutOpen(true)}>
            About tether
          </button>
        </div>
        <div className="home-head-actions">
          <button className="btn" onClick={() => onBrowse()}>
            フォルダを開く
          </button>
          <button className="btn primary" onClick={onNew}>
            ＋ 新しいセッション
          </button>
        </div>
      </div>
      {error && <p className="error">{error}</p>}
      {aboutOpen && <AboutDialog onClose={() => setAboutOpen(false)} />}

      {recentDirs.length > 0 && (
        <section className="home-section">
          <h2>最近のフォルダ</h2>
          <ul className="quick-list">
            {recentDirs.map((d) => (
              <li key={d} className="quick-item">
                <button className="quick-dir" title={`${d} をフォルダ画面で開く`} onClick={() => onBrowse(d)}>
                  <strong>{d.split("/").filter(Boolean).pop() ?? d}</strong>
                  <span className="muted small">{shortPath(d)}</span>
                </button>
                <span className="quick-actions">
                  <button
                    className="btn small"
                    disabled={busyDir !== null}
                    onClick={() => quickLaunch(d, "claude")}
                    title="このフォルダでClaude Codeを起動"
                  >
                    ✻ Claude Code
                  </button>
                  <button
                    className="btn small"
                    disabled={busyDir !== null}
                    onClick={() => quickLaunch(d, "shell")}
                    title="このフォルダでターミナルを開く"
                  >
                    $ ターミナル
                  </button>
                </span>
              </li>
            ))}
          </ul>
        </section>
      )}

      <div className="home-grid">
        <section className="home-section">
          <h2>
            セッション <span className="muted small">{sessions.length}</span>
          </h2>
          {sorted.length === 0 && (
            <p className="muted">まだセッションはありません。「＋ 新しいセッション」から始めましょう。</p>
          )}
          <ul className="cards">
            {sorted.map((s) => {
              const badge = badgeOf(s, attention.has(s.id));
              return (
                <li key={s.id} className={"card" + (attention.has(s.id) ? " attention" : "")}>
                  <button className="card-main" onClick={() => onOpen(s.id)}>
                    <span className="card-top">
                      <span className={"kind-icon" + (isShell(s) ? " shell" : "")}>{isShell(s) ? "$" : "✻"}</span>
                      <span className="card-name">{sessionLabel(s)}</span>
                      <span className={"badge-state " + badge.tone}>{badge.text}</span>
                    </span>
                    <span className="card-cwd">{shortPath(s.cwd)}</span>
                    <span className="card-meta muted small">
                      {timeAgo(new Date(lastTouched(s)).toISOString())}
                      {s.clients > 0 && ` ・ ${s.clients}台が接続中`}
                      {s.model && ` ・ ${s.model}`}
                    </span>
                    {badge.tone === "waiting" && s.activityDetail && (
                      <span className="card-detail">{s.activityDetail}</span>
                    )}
                  </button>
                  <div className="card-more">
                    <button
                      className="icon-btn"
                      aria-label="操作メニュー"
                      aria-expanded={menuFor === s.id}
                      onClick={(e) => {
                        e.stopPropagation();
                        setMenuFor((v) => (v === s.id ? null : s.id));
                      }}
                    >
                      ⋯
                    </button>
                    {menuFor === s.id && (
                      <div className="more-menu" role="menu" onClick={(e) => e.stopPropagation()}>
                        <button role="menuitem" className="more-item" onClick={() => onOpen(s.id)}>
                          開く
                        </button>
                        <button
                          role="menuitem"
                          className="more-item"
                          disabled={!s.running}
                          onClick={() => {
                            setMenuFor(null);
                            onStop(s).catch((e) => setError(e.message));
                          }}
                        >
                          停止
                        </button>
                        <button
                          role="menuitem"
                          className="more-item danger"
                          onClick={() => {
                            setMenuFor(null);
                            onDelete(s).catch((e) => setError(e.message));
                          }}
                        >
                          削除
                        </button>
                      </div>
                    )}
                  </div>
                </li>
              );
            })}
          </ul>
        </section>

        <div className="home-side">
          <section className="home-section">
            <h2>最近の通知</h2>
            {notices.length === 0 && <p className="muted small">通知はまだありません</p>}
            <ul className="notices">
              {notices.slice(0, 12).map((n, i) => {
                const unread = !seenAt || ms(n.at) > ms(seenAt);
                const canOpen = sessionIds.has(n.session);
                return (
                  <li key={n.at + i}>
                    <button
                      className={"notice " + n.kind + (unread ? " unread" : "")}
                      disabled={!canOpen}
                      onClick={() => canOpen && onOpen(n.session)}
                    >
                      <span className="notice-top">
                        <strong>{n.label}</strong>
                        <span className="muted small">{timeAgo(n.at)}</span>
                      </span>
                      <span className="notice-msg">{n.message}</span>
                    </button>
                  </li>
                );
              })}
            </ul>
          </section>

          <section className="home-section home-usage">
            <UsageWidget />
          </section>

          {system && (
            <section className="home-section">
              <h2>サーバ {system.hostname && <span className="muted small">{system.hostname}</span>}</h2>
              <SystemMeters s={system} />
            </section>
          )}
        </div>
      </div>
    </div>
  );
}

function Meter({ label, pct, detail }: { label: string; pct: number; detail: string }) {
  const level = pct >= 90 ? "crit" : pct >= 75 ? "warn" : "ok";
  return (
    <div className="meter">
      <div className="meter-row">
        <span>{label}</span>
        <span className="num">{Math.round(pct)}%</span>
      </div>
      <div className="bar" role="meter" aria-valuenow={Math.round(pct)} aria-valuemin={0} aria-valuemax={100}>
        <div className={"fill " + level} style={{ width: `${Math.min(100, pct)}%` }} />
      </div>
      <div className="muted small">{detail}</div>
    </div>
  );
}

function SystemMeters({ s }: { s: SystemInfo }) {
  const memUsed = s.memTotal - s.memAvailable;
  const diskUsed = s.diskTotal - s.diskFree;
  return (
    <div className="system">
      {s.cpus > 0 && (
        <Meter
          label="CPU負荷"
          pct={(s.load[0] / s.cpus) * 100}
          detail={`ロードアベレージ ${s.load.map((l) => l.toFixed(2)).join(" / ")}（${s.cpus}コア）`}
        />
      )}
      {s.memTotal > 0 && (
        <Meter
          label="メモリ"
          pct={(memUsed / s.memTotal) * 100}
          detail={`${formatBytes(memUsed)} / ${formatBytes(s.memTotal)}`}
        />
      )}
      {s.diskTotal > 0 && (
        <Meter
          label="ディスク"
          pct={(diskUsed / s.diskTotal) * 100}
          detail={`空き ${formatBytes(s.diskFree)}（${shortPath(s.diskPath)}）`}
        />
      )}
      {s.uptimeSec > 0 && <p className="muted small uptime">稼働 {formatDuration(s.uptimeSec)}</p>}
    </div>
  );
}
