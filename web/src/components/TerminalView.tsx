import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import "@xterm/xterm/css/xterm.css";
import { wsUrl } from "../api";
import { bufferToText, copyText, decodeOsc52, readClipboard } from "../clipboard";
import { extractPaths, type KnownPath } from "../paths";
import { usePathLookup } from "../hooks/usePathLookup";
import CommandSheet from "./CommandSheet";
import { attachTouchScroll, LineAccumulator } from "../touchScroll";

type ConnState = "connecting" | "open" | "closed";

/** One-line preview of clipboard text requested via OSC 52. */
export function oscPreview(text: string): string {
  const oneLine = text.replace(/\s+/g, " ").trim();
  const lines = text.split("\n").length;
  const head = oneLine.length > 60 ? oneLine.slice(0, 60) + "…" : oneLine;
  return lines > 1 ? `${head}（${lines}行）` : head;
}

/** Opens an http(s) URL from terminal output in a new tab; other schemes are ignored. */
export function openInNewTab(uri: string): boolean {
  if (!/^https?:\/\//i.test(uri)) return false;
  window.open(uri, "_blank", "noopener,noreferrer");
  return true;
}

/** A short message above the key bar; `pending` is text the user can copy with a tap. */
interface ClipNotice {
  message: string;
  pending?: string;
}

interface Props {
  sessionId: string;
  /** True for plain shell sessions (changes the exit overlay wording). */
  shell: boolean;
  fontSize: number;
  /** Session folder; relative paths in the output are resolved against it. */
  cwd: string;
  /** Opens a file preview (or the folder browser for directories). */
  onOpenPath: (p: KnownPath) => void;
  /** Bumped when the saved commands change on the server. */
  snippetsVersion: number;
}

const theme = {
  background: "#1c1b19",
  foreground: "#e8e4dc",
  cursor: "#d97757",
  cursorAccent: "#1c1b19",
  selectionBackground: "#d9775755",
  black: "#1c1b19",
  brightBlack: "#6b665e",
};

type Key = { label: string; seq: string; title: string; className?: string };

/** Keys in the scrollable part of the helper bar (mainly for phones without Esc/Tab/arrows). */
const KEYS: Key[] = [
  { label: "Esc", seq: "\x1b", title: "Esc" },
  { label: "Tab", seq: "\t", title: "Tab" },
  { label: "⇧Tab", seq: "\x1b[Z", title: "モード切替 (Shift+Tab)" },
  { label: "^C", seq: "\x03", title: "中断 (Ctrl+C)" },
  { label: "←", seq: "\x1b[D", title: "左" },
  { label: "→", seq: "\x1b[C", title: "右" },
];

/**
 * Keys pinned to the right edge so they are always visible: choosing an option in
 * Claude Code's menus needs only ↑/↓ and Enter.
 */
const PINNED_KEYS: Key[] = [
  { label: "↑", seq: "\x1b[A", title: "上" },
  { label: "↓", seq: "\x1b[B", title: "下" },
  { label: "Enter", seq: "\r", title: "Enter（決定・送信）", className: "enter" },
];

/**
 * Terminal attached to a shared session over WebSocket. The pty size is owned
 * by the server (smallest viewport among all clients), so this component
 * reports its own preferred size and renders at whatever size the server announces.
 */
export default function TerminalView({ sessionId, shell, fontSize, cwd, onOpenPath, snippetsVersion }: Props) {
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const reportSizeRef = useRef<() => void>(() => {});
  const [conn, setConn] = useState<ConnState>("connecting");
  const [exited, setExited] = useState<number | null>(null);
  const [composer, setComposer] = useState(false);
  const [draft, setDraft] = useState("");
  const [copySheet, setCopySheet] = useState<{ all: string; visible: string } | null>(null);
  const [clip, setClip] = useState<ClipNotice | null>(null);
  const [commandsOpen, setCommandsOpen] = useState(false);
  const copyTextRef = useRef<HTMLPreElement>(null);
  // 出力に出てきたパスのリンク化。xtermのコールバックから最新の値を見られるようrefで渡す
  const lookup = usePathLookup(cwd);
  const lookupRef = useRef(lookup);
  lookupRef.current = lookup;
  const openPathRef = useRef(onOpenPath);
  openPathRef.current = onOpenPath;

  const send = (msg: object) => {
    const ws = wsRef.current;
    if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
  };
  const input = (data: string) => send({ type: "input", data });

  useEffect(() => {
    const host = hostRef.current!;
    const term = new Terminal({
      fontFamily: '"JetBrains Mono", "Cascadia Code", Menlo, Consolas, "Noto Sans Mono CJK JP", monospace',
      fontSize,
      scrollback: 5000,
      cursorBlink: true,
      macOptionIsMeta: true,
      theme,
      // 出力に埋め込まれたリンク（OSC 8）。既定では確認ダイアログが出るので、http/httpsはそのまま新しいタブで開く
      linkHandler: { activate: (_e, uri) => openInNewTab(uri) },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.loadAddon(new WebLinksAddon((_e, uri) => openInNewTab(uri)));
    term.open(host);
    // 指でのスクロール（xterm.js 6はタッチでスクロールできない）。
    // 通常の画面は履歴をスクロールし、全画面のアプリ（less、vim等）ではホイールと同じく↑↓キーを送る
    const touchLines = new LineAccumulator();
    const detachTouch = attachTouchScroll(host, (px) => {
      const screen = term.element?.querySelector(".xterm-screen");
      const lines = touchLines.add(px, screen ? screen.clientHeight / term.rows : 0);
      if (lines === 0) return;
      if (term.buffer.active.type === "normal") {
        term.scrollLines(lines);
        return;
      }
      const app = term.modes.applicationCursorKeysMode;
      const key = lines < 0 ? (app ? "\x1bOA" : "\x1b[A") : app ? "\x1bOB" : "\x1b[B";
      input(key.repeat(Math.abs(lines)));
    });
    termRef.current = term;

    // 行の文字列と、各文字が何番目のセルにあるか（全角は2セル）を作る
    const lineText = (y: number) => {
      const line = term.buffer.active.getLine(y);
      let text = "";
      const cells: number[] = [];
      if (!line) return { text, cells, line };
      for (let x = 0; x < term.cols; x++) {
        const cell = line.getCell(x);
        if (!cell) break;
        if (cell.getWidth() === 0) continue; // 全角文字の2セル目
        const ch = cell.getChars() || " ";
        for (let k = 0; k < ch.length; k++) cells.push(x);
        text += ch;
      }
      return { text, cells, line };
    };
    const linkSub = term.registerLinkProvider({
      provideLinks(y, callback) {
        const { text, cells, line } = lineText(y - 1);
        const matches = extractPaths(text);
        lookupRef.current.request(matches.map((m) => m.path));
        const known = lookupRef.current.knownRef.current;
        const links = matches
          .filter((m) => known.has(m.path))
          .map((m) => {
            const k = known.get(m.path)!;
            const last = cells[m.end - 1];
            return {
              // xtermの範囲は1始まりで、終わりの位置を含む。全角で終わる場合は2セル分
              range: {
                start: { x: cells[m.start] + 1, y },
                end: { x: last + (line?.getCell(last)?.getWidth() ?? 1), y },
              },
              text: text.slice(m.start, m.end),
              decorations: { pointerCursor: true, underline: true },
              activate: () => openPathRef.current(k),
            };
          });
        callback(links.length ? links : undefined);
      },
    });
    // スマホはマウスを乗せる操作がないので、出力が落ち着いたら表示中の行のパスを先に確かめておく
    let pathScan: number | undefined;
    const prefetchPaths = () => {
      window.clearTimeout(pathScan);
      pathScan = window.setTimeout(() => {
        const buf = term.buffer.active;
        const found: string[] = [];
        for (let y = buf.viewportY; y < buf.viewportY + term.rows; y++) {
          for (const m of extractPaths(lineText(y).text)) found.push(m.path);
        }
        lookupRef.current.request(found);
      }, 600);
    };
    // OSC 52: アプリ（Claude Code、tmux、vim等）からのクリップボード書き込み要求を受け取る
    const oscSub = term.parser.registerOscHandler(52, (data) => {
      const text = decodeOsc52(data);
      if (text !== null) {
        // 端末に表示された内容（悪意のあるファイルやコマンドの出力を含む）がクリップボードを勝手に書き換えて、
        // 貼り付けた先で別のコマンドを実行させることがないよう、自動ではコピーせず内容を見せてボタンで確認する
        setClip({ message: `コピーを求められています: ${oscPreview(text)}`, pending: text });
      }
      return true;
    });

    let disposed = false;
    let retry = 0;
    let retryTimer: number | undefined;
    // サーバが決めたptyサイズ。自分の希望サイズとは別に保持し、表示はこちらに合わせる
    let serverSize: { cols: number; rows: number } | null = null;

    const desired = () => fit.proposeDimensions() ?? { cols: 80, rows: 24 };
    const applySize = () => {
      const want = desired();
      const cols = serverSize ? Math.min(serverSize.cols, want.cols) : want.cols;
      const rows = serverSize ? Math.min(serverSize.rows, want.rows) : want.rows;
      if (cols > 0 && rows > 0 && (cols !== term.cols || rows !== term.rows)) term.resize(cols, rows);
    };
    const reportSize = () => {
      const { cols, rows } = desired();
      send({ type: "resize", cols, rows });
      applySize();
    };

    const connect = () => {
      setConn("connecting");
      const { cols, rows } = desired();
      const ws = new WebSocket(wsUrl(`/ws/sessions/${encodeURIComponent(sessionId)}?cols=${cols}&rows=${rows}`));
      ws.binaryType = "arraybuffer";
      wsRef.current = ws;
      ws.onopen = () => {
        retry = 0;
        setConn("open");
      };
      ws.onmessage = (ev) => {
        if (typeof ev.data !== "string") {
          term.write(new Uint8Array(ev.data as ArrayBuffer), prefetchPaths);
          return;
        }
        const msg = JSON.parse(ev.data);
        switch (msg.type) {
          case "replay":
            term.reset();
            break;
          case "state":
            setExited(msg.running ? null : msg.exitCode);
            serverSize = { cols: msg.cols, rows: msg.rows };
            applySize();
            break;
          case "size":
            serverSize = { cols: msg.cols, rows: msg.rows };
            applySize();
            break;
          case "started":
            setExited(null);
            term.reset();
            break;
          case "exit":
            setExited(msg.code);
            break;
        }
      };
      ws.onclose = () => {
        if (wsRef.current === ws) wsRef.current = null;
        if (disposed) return;
        setConn("closed");
        // スマホのスリープ復帰などで切れるので、指数バックオフで自動再接続する
        const delay = Math.min(10000, 500 * 2 ** retry++);
        retryTimer = window.setTimeout(connect, delay);
      };
    };

    reportSizeRef.current = reportSize;
    const dataSub = term.onData((d) => input(d));
    const binSub = term.onBinary((d) => input(d));
    const ro = new ResizeObserver(() => reportSize());
    ro.observe(host);
    const onVisible = () => {
      // 復帰直後に切断済みなら待たずに再接続する
      if (document.visibilityState === "visible" && !wsRef.current && !disposed) {
        window.clearTimeout(retryTimer);
        retry = 0;
        connect();
      }
    };
    document.addEventListener("visibilitychange", onVisible);
    connect();
    term.focus();

    return () => {
      disposed = true;
      window.clearTimeout(retryTimer);
      document.removeEventListener("visibilitychange", onVisible);
      ro.disconnect();
      dataSub.dispose();
      binSub.dispose();
      oscSub.dispose();
      linkSub.dispose();
      detachTouch();
      window.clearTimeout(pathScan);
      wsRef.current?.close();
      wsRef.current = null;
      term.dispose();
      termRef.current = null;
    };
    // fontSizeは下のeffectで反映するので、ここでは接続を張り直さない
  }, [sessionId]);

  useEffect(() => {
    const term = termRef.current;
    if (!term) return;
    term.options.fontSize = fontSize;
    // フォントサイズ変更でセル寸法が変わるため、希望サイズを送り直す
    reportSizeRef.current();
  }, [fontSize]);

  // 通知は数秒で消す。コピー待ちのテキストがあるときはユーザーが閉じるまで残す
  useEffect(() => {
    if (!clip || clip.pending) return;
    const t = window.setTimeout(() => setClip(null), 3000);
    return () => window.clearTimeout(t);
  }, [clip]);

  useEffect(() => {
    // シートを開いたら最新の出力が見えるよう末尾までスクロールする
    if (copySheet && copyTextRef.current) copyTextRef.current.scrollTop = copyTextRef.current.scrollHeight;
  }, [copySheet]);

  const openCopySheet = () => {
    const term = termRef.current;
    if (!term) return;
    const buf = term.buffer.active;
    setCopySheet({
      all: bufferToText(buf),
      visible: bufferToText(buf, buf.viewportY, buf.viewportY + term.rows),
    });
  };

  const doCopy = async (text: string, what: string) => {
    if (!text) {
      setClip({ message: `${what}が空です` });
      return;
    }
    const ok = await copyText(text);
    setClip(
      ok
        ? { message: `${what}をコピーしました` }
        : { message: "コピーできませんでした。長押しで選択してコピーしてください" },
    );
    if (ok) setCopySheet(null);
  };

  const pasteFromClipboard = async () => {
    const text = await readClipboard();
    if (text) {
      // term.paste()はアプリのbracketed pasteモードに従って送るので、複数行でも途中で実行されない
      termRef.current?.paste(text);
      return;
    }
    setComposer(true);
    setClip({ message: "この接続ではボタンから貼り付けできません。入力欄を長押しして貼り付け、送信してください" });
  };

  const submitDraft = () => {
    const term = termRef.current;
    if (!draft || !term) {
      input("\r");
      return;
    }
    term.paste(draft);
    window.setTimeout(() => input("\r"), 120);
    setDraft("");
  };

  const runCommand = (command: string) => {
    const term = termRef.current;
    if (!term) return;
    // 入力欄の送信と同じく、貼り付けとして送ってから少し置いてEnterを送る
    term.paste(command);
    window.setTimeout(() => input("\r"), 120);
    setCommandsOpen(false);
    term.focus();
  };

  return (
    <div className="term-wrap">
      <div className="term-host" ref={hostRef} onClick={() => termRef.current?.focus()} />
      {conn !== "open" && (
        <div className="term-banner">{conn === "connecting" ? "接続中..." : "切断されました。再接続しています..."}</div>
      )}
      {exited !== null && conn === "open" && (
        <div className="term-overlay">
          <p>
            {shell ? "シェル" : "Claude Code"}は終了しました（終了コード{exited}）
          </p>
          <button className="btn primary" onClick={() => send({ type: "restart" })}>
            {shell ? "新しいシェルを開く" : "会話を再開する"}
          </button>
        </div>
      )}
      {copySheet && (
        <div className="copy-sheet" role="dialog" aria-label="テキストをコピー">
          <div className="copy-head">
            <span className="muted small">長押しで範囲を選んでコピーできます</span>
            <button className="icon-btn" onClick={() => setCopySheet(null)} aria-label="閉じる">
              ✕
            </button>
          </div>
          <pre className="copy-text" ref={copyTextRef}>
            {copySheet.all}
          </pre>
          <div className="copy-actions">
            <button
              className="btn small"
              onClick={() => doCopy(window.getSelection()?.toString() ?? "", "選択範囲")}
              // ボタンを押したときに選択が解除されないようにする
              onMouseDown={(e) => e.preventDefault()}
            >
              選択範囲をコピー
            </button>
            <button className="btn small" onClick={() => doCopy(copySheet.visible, "表示中の画面")}>
              表示中の画面をコピー
            </button>
            <button className="btn small primary" onClick={() => doCopy(copySheet.all, "すべて")}>
              すべてコピー
            </button>
          </div>
        </div>
      )}
      {commandsOpen && (
        <CommandSheet
          version={snippetsVersion}
          disabled={conn !== "open" || exited !== null}
          onRun={runCommand}
          onClose={() => setCommandsOpen(false)}
        />
      )}
      {clip && (
        <div className="clip-notice" role="status">
          <span>{clip.message}</span>
          {clip.pending && (
            <button className="btn small primary" onClick={() => doCopy(clip.pending!, "テキスト")}>
              コピー
            </button>
          )}
          <button className="icon-btn" onClick={() => setClip(null)} aria-label="閉じる">
            ✕
          </button>
        </div>
      )}
      {composer && (
        <div className="composer">
          <textarea
            value={draft}
            autoFocus
            rows={3}
            placeholder="入力または長押しで貼り付け（Ctrl+Enterで送信）"
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
                e.preventDefault();
                submitDraft();
              }
            }}
          />
          <button className="btn primary" onClick={submitDraft}>
            送信
          </button>
        </div>
      )}
      <div className="keybar" role="toolbar" aria-label="補助キー">
        <div className="keybar-scroll">
          <button
            className={"key" + (composer ? " on" : "")}
            onClick={() => setComposer((v) => !v)}
            title="テキスト入力欄の表示切替"
          >
            ✎
          </button>
          {shell && (
            <button
              className={"key" + (commandsOpen ? " on" : "")}
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => setCommandsOpen((v) => !v)}
              title="よく使うコマンド"
            >
              コマンド
            </button>
          )}
          <button className="key" onClick={openCopySheet} title="画面のテキストをコピー">
            コピー
          </button>
          <button
            className="key"
            onMouseDown={(e) => e.preventDefault()}
            onClick={pasteFromClipboard}
            title="クリップボードから貼り付け"
          >
            貼付
          </button>
          {KEYS.map((k) => (
            <KeyButton key={k.label} k={k} onPress={input} />
          ))}
        </div>
        {/* 画面幅が狭くても選択肢の移動と決定ができるよう、↑↓とEnterは右端に固定して常に表示する */}
        <div className="keybar-pinned">
          {PINNED_KEYS.map((k) => (
            <KeyButton key={k.label} k={k} onPress={input} />
          ))}
        </div>
      </div>
    </div>
  );
}

function KeyButton({ k, onPress }: { k: Key; onPress: (seq: string) => void }) {
  return (
    <button
      className={"key" + (k.className ? " " + k.className : "")}
      title={k.title}
      aria-label={k.title}
      // ボタンを押してもターミナルからフォーカスを奪わない（スマホでキーボードが閉じるのを防ぐ）
      onMouseDown={(e) => e.preventDefault()}
      onClick={() => onPress(k.seq)}
    >
      {k.label}
    </button>
  );
}
