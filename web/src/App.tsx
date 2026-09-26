import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  api,
  getToken,
  isShell,
  sessionLabel,
  shortPath,
  setToken,
  setUnauthorizedHandler,
  wsUrl,
  type LaunchOptions,
  type Notice,
  type ServerConfig,
  type SessionStatus,
  type RemoteApproval,
} from "./api";
import DirBrowser from "./components/DirBrowser";
import Home from "./components/Home";
import Login from "./components/Login";
import ScheduleDialog from "./components/ScheduleDialog";
import SessionList from "./components/SessionList";
import TerminalView from "./components/TerminalView";
import ChatView from "./components/ChatView";
import PreviewDialog from "./components/PreviewDialog";
import type { KnownPath } from "./paths";
import { applyTheme, loadTheme, saveTheme, watchSystemTheme, type ThemePref } from "./theme";
import UsageWidget from "./components/UsageWidget";
import { notifyPermission, requestNotifyPermission, showSystemNotification } from "./notify";
import { loadRecentDirs, rememberDir } from "./prefs";
import { answeredChoices } from "./attention";
import ApprovalDialog from "./components/ApprovalDialog";
import RemoteView from "./components/RemoteView";

type Phase = "loading" | "login" | "ready" | "error";

interface Toast extends Notice {
  key: number;
}

const FONT_KEY = "tether.fontSize";
const SESSION_VIEW_KEY = "tether.sessionView";

type SessionView = "terminal" | "chat";

function readSessionView(): SessionView {
  try {
    return localStorage.getItem(SESSION_VIEW_KEY) === "chat" ? "chat" : "terminal";
  } catch {
    return "terminal";
  }
}

function readFontSize(): number {
  try {
    const v = Number(localStorage.getItem(FONT_KEY));
    if (v >= 9 && v <= 28) return v;
  } catch {
    // 既定値を使う
  }
  return window.innerWidth < 640 ? 12 : 14;
}

type View = "home" | "browser" | "session" | "remote";

/** Reads the current screen from the URL: /?session=<id>, /?browse=1 or / (home). */
function parseLocation(): { view: View; session: string | null } {
  const q = new URLSearchParams(location.search);
  const session = q.get("session");
  if (session) return { view: "session", session };
  if (q.get("browse")) return { view: "browser", session: null };
  if (q.get("remote")) return { view: "remote", session: null };
  return { view: "home", session: null };
}

function urlFor(view: View, session: string | null): string {
  const url = new URL(location.href);
  url.search = "";
  if (view === "session" && session) url.searchParams.set("session", session);
  if (view === "browser") url.searchParams.set("browse", "1");
  if (view === "remote") url.searchParams.set("remote", "1");
  return url.href;
}

export default function App() {
  const [phase, setPhase] = useState<Phase>("loading");
  const [config, setConfig] = useState<ServerConfig | null>(null);
  const [sessions, setSessions] = useState<SessionStatus[]>([]);
  const [activeId, setActiveId] = useState<string | null>(() => parseLocation().session);
  const [view, setView] = useState<View>(() => parseLocation().view);
  const [noticeVersion, setNoticeVersion] = useState(0);
  const [recentVersion, setRecentVersion] = useState(0);
  const [browserPath, setBrowserPath] = useState<string | undefined>();
  const [attention, setAttention] = useState<Set<string>>(new Set());
  // 選択待ちの通知を受けたセッションと、その通知の時刻（選択が済んだら要対応の印を外すため）
  const choiceNoticesRef = useRef(new Map<string, number>());
  const [toasts, setToasts] = useState<Toast[]>([]);
  const [fontSize, setFontSize] = useState(readFontSize);
  const [sessionView, setSessionView] = useState<SessionView>(readSessionView);
  const [theme, setTheme] = useState<ThemePref>(loadTheme);
  const [previewPath, setPreviewPath] = useState<KnownPath | null>(null);
  const themeRef = useRef(theme);
  themeRef.current = theme;

  useEffect(() => {
    applyTheme(theme);
    saveTheme(theme);
  }, [theme]);
  // 「自動」のときは端末のライト/ダーク切替に追従する
  useEffect(() => watchSystemTheme(() => themeRef.current), []);

  const changeSessionView = (v: SessionView) => {
    setSessionView(v);
    try {
      localStorage.setItem(SESSION_VIEW_KEY, v);
    } catch {
      // 保存できなくても今回の切り替えは有効
    }
  };
  const [scheduleFor, setScheduleFor] = useState<string | null>(null);
  const [scheduleVersion, setScheduleVersion] = useState(0);
  const [snippetsVersion, setSnippetsVersion] = useState(0);
  // 別のマシンとの連携（設定と承認待ち）
  const [remoteVersion, setRemoteVersion] = useState(0);
  const [approvals, setApprovals] = useState<RemoteApproval[]>([]);
  const [drawer, setDrawer] = useState(false);
  const [permission, setPermission] = useState(notifyPermission);
  const [menu, setMenu] = useState(false);
  const [authRequired, setAuthRequired] = useState(false);
  const stateRef = useRef({ activeId, view });
  stateRef.current = { activeId, view };

  const boot = useCallback(async () => {
    setPhase("loading");
    try {
      const health = await api.health();
      setAuthRequired(health.authRequired);
      if (health.authRequired && !getToken()) {
        setPhase("login");
        return;
      }
      const [cfg, list] = await Promise.all([api.config(), api.sessions()]);
      setConfig(cfg);
      setSessions(list);
      setPhase("ready");
    } catch (e) {
      if ((e as { status?: number }).status !== 401) setPhase("error");
    }
  }, []);

  useEffect(() => {
    setUnauthorizedHandler(() => {
      setToken("");
      setPhase("login");
    });
    boot();
  }, [boot]);

  const refreshSessions = useCallback(() => {
    api
      .sessions()
      .then(setSessions)
      .catch(() => {});
  }, []);

  const pushToast = useCallback((n: Notice) => {
    const t = { ...n, key: Date.now() + Math.random() };
    setToasts((prev) => [...prev.slice(-3), t]);
    window.setTimeout(() => setToasts((prev) => prev.filter((x) => x.key !== t.key)), 8000);
  }, []);

  // 通知・セッション状態変化のイベントストリーム
  useEffect(() => {
    if (phase !== "ready") return;
    let ws: WebSocket | null = null;
    let timer: number | undefined;
    let closed = false;
    let retry = 0;
    const connect = () => {
      ws = new WebSocket(wsUrl("/ws/events"));
      ws.onopen = () => {
        retry = 0;
        // 切断中に起きた変化を取りこぼさないよう、接続のたびに取り直す
        refreshSessions();
      };
      ws.onmessage = (ev) => {
        const msg = JSON.parse(ev.data);
        if (typeof msg.type !== "string") return;
        if (msg.type.startsWith("session.")) refreshSessions();
        if (msg.type === "schedule.changed") setScheduleVersion((v) => v + 1);
        if (msg.type === "snippets.changed") setSnippetsVersion((v) => v + 1);
        if (msg.type === "remote.changed") setRemoteVersion((v) => v + 1);
        if (msg.type === "notify") {
          setNoticeVersion((v) => v + 1);
          const n = msg as Notice;
          const { activeId: cur, view: v } = stateRef.current;
          const watching = cur === n.session && v === "session" && document.visibilityState === "visible";
          if (!watching) {
            if (n.session) setAttention((prev) => new Set(prev).add(n.session));
            if (n.session && n.kind === "attention")
              choiceNoticesRef.current.set(n.session, Date.parse(n.at) || Date.now());
            pushToast(n);
            showSystemNotification(n);
          }
        }
      };
      ws.onclose = () => {
        if (closed) return;
        timer = window.setTimeout(connect, Math.min(10000, 500 * 2 ** retry++));
      };
    };
    connect();
    return () => {
      closed = true;
      window.clearTimeout(timer);
      ws?.close();
    };
  }, [phase, refreshSessions, pushToast]);

  // 画面遷移はpushStateで履歴に積み、ブラウザやスマホの「戻る」で前の画面へ戻れるようにする
  const pushUrl = (v: View, session: string | null) => {
    const href = urlFor(v, session);
    if (href !== location.href) history.pushState(null, "", href);
  };
  // 削除したセッションに「戻る」で戻らないよう、履歴を積まずにホームへ置き換える
  const replaceWithHome = () => {
    history.replaceState(null, "", urlFor("home", null));
    setView("home");
    setActiveId(null);
  };

  // 承認待ちは起動時と変化の通知のたびに取り直す（未対応のサーバでは何もしない）
  useEffect(() => {
    if (phase !== "ready") return;
    api
      .remote()
      .then((st) => setApprovals(st.approvals))
      .catch(() => setApprovals([]));
  }, [phase, remoteVersion]);

  // 別の端末やターミナルで選択が済んだら、ホームや一覧の「入力待ち」の印を外す
  useEffect(() => {
    const done = answeredChoices(sessions, choiceNoticesRef.current);
    if (done.length === 0) return;
    for (const id of done) choiceNoticesRef.current.delete(id);
    setAttention((prev) => {
      const next = new Set(prev);
      for (const id of done) next.delete(id);
      return next;
    });
  }, [sessions]);

  const select = useCallback((id: string) => {
    pushUrl("session", id);
    setActiveId(id);
    setView("session");
    setDrawer(false);
    setAttention((prev) => {
      const next = new Set(prev);
      next.delete(id);
      return next;
    });
  }, []);

  // 通知クリックでService Workerから開くセッションを受け取る
  useEffect(() => {
    const onMsg = (e: MessageEvent) => e.data?.type === "open-session" && e.data.session && select(e.data.session);
    navigator.serviceWorker?.addEventListener("message", onMsg);
    return () => navigator.serviceWorker?.removeEventListener("message", onMsg);
  }, [select]);

  const goHome = useCallback(() => {
    pushUrl("home", null);
    setView("home");
    setDrawer(false);
  }, []);

  const openRemote = useCallback(() => {
    pushUrl("remote", null);
    setView("remote");
    setDrawer(false);
  }, []);

  /** Opens the folder browser, optionally at a given folder. */
  const openBrowser = useCallback((path?: string) => {
    pushUrl("browser", null);
    setBrowserPath(path);
    setView("browser");
    setDrawer(false);
  }, []);

  useEffect(() => {
    const onPop = () => {
      const loc = parseLocation();
      setDrawer(false);
      setView(loc.view);
      if (loc.session) setActiveId(loc.session);
    };
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  useEffect(() => {
    try {
      localStorage.setItem(FONT_KEY, String(fontSize));
    } catch {
      // 保存できなくても動作に支障はない
    }
  }, [fontSize]);

  const active = sessions.find((s) => s.id === activeId) ?? null;
  // 削除などで存在しないセッションを指していたらブラウザ画面に戻す
  useEffect(() => {
    if (phase === "ready" && view === "session" && activeId && !sessions.some((s) => s.id === activeId)) {
      replaceWithHome();
    }
  }, [phase, view, activeId, sessions]);

  // 起動履歴（このブラウザで起動したフォルダ）と既存セッションのフォルダを合わせて新しい順に並べる
  const recentDirs = useMemo(() => {
    const fromSessions = [...sessions]
      .sort((a, b) => Date.parse(b.lastActiveAt) - Date.parse(a.lastActiveAt))
      .map((s) => s.cwd);
    return [...new Set([...loadRecentDirs(), ...fromSessions])].slice(0, 6);
    // recentVersionは起動履歴の更新を反映するための依存
  }, [sessions, recentVersion]);

  const launch = async (opts: LaunchOptions) => {
    const s = await api.createSession(opts);
    rememberDir(opts.cwd);
    setRecentVersion((v) => v + 1);
    setSessions((prev) => [...prev.filter((x) => x.id !== s.id), s]);
    select(s.id);
  };

  const rename = async () => {
    if (!active) return;
    const label = prompt("セッション名", sessionLabel(active));
    if (label === null) return;
    await api.renameSession(active.id, label);
    refreshSessions();
  };

  const stopSession = async (target: SessionStatus) => {
    const msg = isShell(target)
      ? `「${sessionLabel(target)}」のターミナルを終了しますか？（実行中のコマンドも止まります）`
      : `「${sessionLabel(target)}」のClaude Codeを停止しますか？（会話はあとで再開できます）`;
    if (!confirm(msg)) return;
    await api.stopSession(target.id);
    refreshSessions();
  };

  const removeSession = async (target: SessionStatus) => {
    if (!confirm(`「${sessionLabel(target)}」を一覧から削除しますか？`)) return;
    await api.deleteSession(target.id);
    if (view === "session" && activeId === target.id) replaceWithHome();
    refreshSessions();
  };

  // チャットやターミナルに出てきたパスを押したとき。フォルダはフォルダ画面、ファイルはプレビューで開く
  const openPath = useCallback(
    (p: KnownPath) => {
      if (p.isDir) openBrowser(p.path);
      else setPreviewPath(p);
    },
    [openBrowser],
  );

  const actions = active
    ? [
        {
          label: "A-",
          title: "文字を小さく",
          className: "icon-btn",
          run: () => setFontSize((f) => Math.max(9, f - 1)),
        },
        {
          label: "A+",
          title: "文字を大きく",
          className: "icon-btn",
          run: () => setFontSize((f) => Math.min(28, f + 1)),
        },
        // シェルへの予約はサーバ側でも受け付けない
        ...(isShell(active)
          ? []
          : [{ label: "予約", title: "予約プロンプト", className: "btn small", run: () => setScheduleFor(active.id) }]),
        {
          label: "ファイル",
          title: "このフォルダのファイル",
          className: "btn small",
          run: () => openBrowser(active.cwd),
        },
        {
          label: "停止",
          title: "停止",
          className: "btn small",
          run: () => stopSession(active),
          disabled: !active.running,
        },
        { label: "削除", title: "削除", className: "btn small danger", run: () => removeSession(active) },
      ]
    : [];

  if (phase === "loading") return <div className="center muted">読み込み中...</div>;
  if (phase === "login") return <Login onDone={boot} />;
  if (phase === "error" || !config)
    return (
      <div className="center">
        <p>サーバに接続できません</p>
        <button className="btn" onClick={boot}>
          再試行
        </button>
      </div>
    );

  return (
    <div className={"app" + (drawer ? " drawer-open" : "")}>
      <aside className="sidebar">
        <button className="brand" onClick={goHome} title="ホームへ">
          <img src="/icon.svg" alt="" width={22} height={22} />
          <span>tether</span>
        </button>
        <nav className="side-nav" aria-label="画面">
          <button className={"side-nav-item" + (view === "home" ? " on" : "")} onClick={goHome}>
            <span className="side-nav-icon" aria-hidden="true">
              ⌂
            </span>
            ホーム
          </button>
          <button className={"side-nav-item" + (view === "browser" ? " on" : "")} onClick={() => openBrowser()}>
            <span className="side-nav-icon" aria-hidden="true">
              ▤
            </span>
            フォルダ
          </button>
          <button className={"side-nav-item" + (view === "remote" ? " on" : "")} onClick={openRemote}>
            <span className="side-nav-icon" aria-hidden="true">
              ⇄
            </span>
            マシンの連携
          </button>
        </nav>
        <SessionList
          sessions={sessions}
          activeId={view === "session" ? activeId : null}
          attention={attention}
          onSelect={select}
          onNew={() => openBrowser()}
        />
        <UsageWidget />
        <footer className="sidebar-foot">
          <label className="theme-select">
            <span className="muted small">テーマ</span>
            <select value={theme} onChange={(e) => setTheme(e.target.value as ThemePref)}>
              <option value="auto">自動（端末の設定）</option>
              <option value="light">ライト</option>
              <option value="dark">ダーク</option>
            </select>
          </label>
          {permission === "default" && (
            <button className="btn small" onClick={() => requestNotifyPermission().then(setPermission)}>
              通知を有効にする
            </button>
          )}
          {permission === "denied" && <span className="muted small">通知はブラウザでブロックされています</span>}
          {authRequired && (
            <button
              className="btn small ghost"
              onClick={() => {
                setToken("");
                setPhase("login");
              }}
            >
              ログアウト
            </button>
          )}
        </footer>
      </aside>
      <div className="scrim" onClick={() => setDrawer(false)} />

      <main className="main">
        <header className="topbar">
          <button className="icon-btn menu" onClick={() => setDrawer(true)} aria-label="メニュー">
            ☰{attention.size > 0 && <span className="menu-badge" />}
          </button>
          {view === "session" && active ? (
            <>
              <button className="icon-btn back" onClick={goHome} aria-label="ホームへ戻る" title="ホームへ戻る">
                ←
              </button>
              <button className="title" onClick={rename} title="名前を変更">
                <span className={"dot " + (active.running ? "running" : "stopped")} />
                <span className="title-text">{sessionLabel(active)}</span>
                <span className="title-cwd">{shortPath(active.cwd)}</span>
              </button>
              {!isShell(active) && (
                <div className="view-switch" role="tablist" aria-label="表示">
                  <button
                    role="tab"
                    aria-selected={sessionView === "terminal"}
                    className={"view-option" + (sessionView === "terminal" ? " on" : "")}
                    onClick={() => changeSessionView("terminal")}
                  >
                    ターミナル
                  </button>
                  <button
                    role="tab"
                    aria-selected={sessionView === "chat"}
                    className={"view-option" + (sessionView === "chat" ? " on" : "")}
                    onClick={() => changeSessionView("chat")}
                  >
                    チャット
                  </button>
                </div>
              )}
              <div className="topbar-actions">
                {actions.map((a) => (
                  <button
                    key={a.label}
                    className={a.className}
                    onClick={a.run}
                    disabled={a.disabled}
                    aria-label={a.title}
                  >
                    {a.label}
                  </button>
                ))}
              </div>
              <div className="more">
                <button
                  className="icon-btn"
                  onClick={() => setMenu((v) => !v)}
                  aria-label="操作メニュー"
                  aria-expanded={menu}
                >
                  ⋯
                </button>
                {menu && (
                  <div className="more-menu" role="menu">
                    {actions.map((a) => (
                      <button
                        key={a.label}
                        role="menuitem"
                        className={"more-item " + (a.className.includes("danger") ? "danger" : "")}
                        disabled={a.disabled}
                        onClick={() => {
                          setMenu(false);
                          a.run();
                        }}
                      >
                        {a.title ?? a.label}
                      </button>
                    ))}
                  </div>
                )}
              </div>
            </>
          ) : view === "browser" ? (
            <>
              <button className="icon-btn back" onClick={goHome} aria-label="ホームへ戻る" title="ホームへ戻る">
                ←
              </button>
              <div className="title">
                <span className="title-text">フォルダを選んで起動</span>
              </div>
            </>
          ) : view === "remote" ? (
            <>
              <button className="icon-btn back" onClick={goHome} aria-label="ホームへ戻る" title="ホームへ戻る">
                ←
              </button>
              <div className="title">
                <span className="title-text">マシンの連携</span>
              </div>
            </>
          ) : (
            <div className="title">
              <span className="title-text">tether</span>
            </div>
          )}
        </header>

        <div className="content">
          {view === "session" && active ? (
            sessionView === "chat" && !isShell(active) ? (
              <ChatView
                key={active.id}
                sessionId={active.id}
                running={active.running}
                activity={active.activity}
                activityDetail={active.activityDetail}
                cwd={active.cwd}
                onOpenPath={openPath}
                onOpenTerminal={() => changeSessionView("terminal")}
              />
            ) : (
              <TerminalView
                key={active.id}
                sessionId={active.id}
                shell={isShell(active)}
                fontSize={fontSize}
                cwd={active.cwd}
                onOpenPath={openPath}
                snippetsVersion={snippetsVersion}
              />
            )
          ) : view === "browser" ? (
            <DirBrowser config={config} initialPath={browserPath} recentDirs={recentDirs} onLaunch={launch} />
          ) : view === "remote" ? (
            <RemoteView version={remoteVersion} />
          ) : (
            <Home
              sessions={sessions}
              attention={attention}
              recentDirs={recentDirs}
              noticeVersion={noticeVersion}
              onOpen={select}
              onNew={() => openBrowser()}
              onBrowse={openBrowser}
              onLaunch={launch}
              onStop={stopSession}
              onDelete={removeSession}
            />
          )}
        </div>
      </main>

      {approvals.length > 0 && <ApprovalDialog approval={approvals[0]} more={approvals.length - 1} />}

      <div className="toasts" aria-live="polite">
        {toasts.map((t) => (
          <button
            key={t.key}
            className={"toast " + t.kind}
            onClick={() => {
              setToasts((prev) => prev.filter((x) => x.key !== t.key));
              if (t.session && sessions.some((s) => s.id === t.session)) select(t.session);
            }}
          >
            <strong>{t.label}</strong>
            <span>{t.message}</span>
          </button>
        ))}
      </div>

      {previewPath && (
        <PreviewDialog
          entry={{
            name: previewPath.path.split("/").pop() ?? previewPath.path,
            path: previewPath.path,
            isDir: false,
            isLink: false,
            size: previewPath.size,
            modTime: "",
          }}
          onClose={() => setPreviewPath(null)}
        />
      )}

      {scheduleFor && (
        <ScheduleDialog sessionId={scheduleFor} version={scheduleVersion} onClose={() => setScheduleFor(null)} />
      )}
    </div>
  );
}
