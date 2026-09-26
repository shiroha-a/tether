import { useEffect, useLayoutEffect, useMemo, useRef, useState, type MouseEvent } from "react";
import { api, type Activity, type ChatItem, type InputKey } from "../api";
import {
  buildEntries,
  enterAction,
  groupTools,
  insertNewline,
  mergeItems,
  toolBreakdown,
  type ChatEntry,
  type ToolGroup,
} from "../chat";
import { useScreenMenu } from "../hooks/useScreenMenu";
import { keysToChoose } from "../screenMenu";
import { renderMarkdown } from "../markdown";
import { collectPaths, linkifyHtml, type KnownPath } from "../paths";
import { usePathLookup } from "../hooks/usePathLookup";
import PathText from "./PathText";

interface Props {
  sessionId: string;
  running: boolean;
  activity?: Activity;
  activityDetail?: string;
  /** Session folder; relative paths in messages are resolved against it. */
  cwd: string;
  onOpenTerminal: () => void;
  /** Opens a file preview (or the folder browser for directories). */
  onOpenPath: (p: KnownPath) => void;
}

const POLL_MS = 1500;

/** Keys offered while Claude Code waits for a choice (permission prompts, menus). */
const CHOICE_KEYS: { key: InputKey; label: string }[] = [
  { key: "up", label: "↑" },
  { key: "down", label: "↓" },
  { key: "1", label: "1" },
  { key: "2", label: "2" },
  { key: "3", label: "3" },
  { key: "enter", label: "Enter" },
  { key: "esc", label: "Esc" },
];

function Markdown({ text, known }: { text: string; known: Map<string, KnownPath> }) {
  const html = useMemo(() => linkifyHtml(renderMarkdown(text), known), [text, known]);
  return <div className="markdown chat-md" dangerouslySetInnerHTML={{ __html: html }} />;
}

type ToolEntry = Extract<ChatEntry, { type: "tool" }>;
type ToolState = "running" | "error" | "done";

const STATE_LABEL: Record<ToolState, string> = { running: "実行中", error: "エラー", done: "完了" };

function toolState(t: ToolEntry): ToolState {
  return !t.result ? "running" : t.result.isError ? "error" : "done";
}

type Paths = { known: Map<string, KnownPath>; onOpen: (p: KnownPath) => void };

function ToolCard({ t, paths }: { t: ToolEntry; paths: Paths }) {
  const state = toolState(t);
  return (
    <details className={"chat-tool " + state}>
      <summary>
        <span className="tool-name">{t.use.toolName}</span>
        <span className="tool-summary">
          <PathText text={t.use.summary ?? ""} {...paths} />
        </span>
        <span className="tool-state">{STATE_LABEL[state]}</span>
      </summary>
      <pre className="tool-io">
        <PathText text={t.use.text ?? ""} {...paths} />
      </pre>
      {t.result && (
        <pre className={"tool-io result" + (t.result.isError ? " error" : "")}>
          {t.result.text ? <PathText text={t.result.text} {...paths} /> : "（出力なし）"}
          {t.result.truncated && "\n…（省略）"}
        </pre>
      )}
    </details>
  );
}

function Thinking({ item }: { item: ChatItem }) {
  return (
    <details className="chat-thinking">
      <summary>思考</summary>
      <p className="plain">{item.text}</p>
    </details>
  );
}

/** Several consecutive tool calls collapsed into one card. */
function ToolGroupCard({ g, paths }: { g: ToolGroup; paths: Paths }) {
  const running = g.tools.find((t) => !t.result);
  const errors = g.tools.filter((t) => t.result?.isError).length;
  const state: ToolState = running ? "running" : errors > 0 ? "error" : "done";
  return (
    <details className={"chat-tool chat-tool-group " + state}>
      <summary>
        <span className="tool-name">ツール {g.tools.length}回</span>
        <span className="tool-summary">
          {/* 実行中は今何をしているかを、終わったら内訳を見出しに出す */}
          {running ? `${running.use.toolName} ${running.use.summary ?? ""}` : toolBreakdown(g.tools)}
        </span>
        <span className="tool-state">{state === "error" ? `エラー${errors}件` : STATE_LABEL[state]}</span>
      </summary>
      <div className="tool-group-body">
        {g.entries.map((e) =>
          e.type === "tool" ? (
            <ToolCard key={e.use.id} t={e} paths={paths} />
          ) : (
            <Thinking key={e.item.id} item={e.item} />
          ),
        )}
      </div>
    </details>
  );
}

function time(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

/**
 * Chat-style view of a Claude Code session, built from its transcript file.
 * It polls the server for new transcript lines while mounted and sends
 * prompts and choice keys through the session input API.
 */
export default function ChatView({
  sessionId,
  running,
  activity,
  activityDetail,
  cwd,
  onOpenTerminal,
  onOpenPath,
}: Props) {
  const [items, setItems] = useState<ChatItem[]>([]);
  const [truncated, setTruncated] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [draft, setDraft] = useState("");
  const [sending, setSending] = useState(false);
  const [pending, setPending] = useState<string | null>(null);
  const [choosing, setChoosing] = useState(false);
  const listRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  // Alt+Enterで改行を入れた後のカーソル位置。値を差し替えるとカーソルが末尾に移るので、描画直後に戻す
  const caretRef = useRef<number | null>(null);
  useLayoutEffect(() => {
    if (caretRef.current === null) return;
    inputRef.current?.setSelectionRange(caretRef.current, caretRef.current);
    caretRef.current = null;
  }, [draft]);
  const stickRef = useRef(true);
  // 選択肢は会話記録に出ず画面にだけ描かれるので、ターミナルの画面を読んで取り出す
  const { menu, busy } = useScreenMenu(sessionId, running);

  useEffect(() => {
    let offset = 0;
    let claudeId = "";
    let timer: number | undefined;
    let stopped = false;
    setItems([]);
    setLoaded(false);
    const poll = async () => {
      if (document.visibilityState === "visible") {
        try {
          const chunk = await api.transcript(sessionId, offset);
          if (stopped) return;
          // /clear等で会話が切り替わったら、最初から読み直す
          if (claudeId && chunk.claudeSessionId !== claudeId) {
            offset = 0;
            claudeId = chunk.claudeSessionId;
            setItems([]);
            timer = window.setTimeout(poll, 0);
            return;
          }
          claudeId = chunk.claudeSessionId;
          if (offset === 0) setTruncated(!!chunk.truncated);
          offset = chunk.offset;
          setItems((prev) => mergeItems(prev, chunk.items));
          setError("");
        } catch (e) {
          if (!stopped) setError((e as Error).message);
        } finally {
          if (!stopped) setLoaded(true);
        }
      }
      if (!stopped) timer = window.setTimeout(poll, POLL_MS);
    };
    poll();
    return () => {
      stopped = true;
      window.clearTimeout(timer);
    };
  }, [sessionId]);

  // 送信した発言が記録に現れたら、仮表示を消す
  useEffect(() => {
    if (pending && items.some((i) => i.kind === "user" && i.text?.trim() === pending.trim())) setPending(null);
  }, [items, pending]);

  // 一番下を見ているときだけ、新しい発言に合わせて追従スクロールする
  useLayoutEffect(() => {
    const el = listRef.current;
    if (el && stickRef.current) el.scrollTop = el.scrollHeight;
  }, [items, pending, activity]);

  const onScroll = () => {
    const el = listRef.current;
    if (el) stickRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
  };

  const entries = useMemo(() => groupTools(buildEntries(items)), [items]);

  // 発言やツールの入出力に出てきたパスを、実在するものだけリンクにする
  const { known, request } = usePathLookup(cwd);
  useEffect(() => {
    request(collectPaths(items.flatMap((i) => [i.text, i.summary])));
  }, [items, request]);
  const paths: Paths = useMemo(() => ({ known, onOpen: onOpenPath }), [known, onOpenPath]);

  // Markdownの中に差し込んだリンク（a.file-link）は、ここでまとめて受ける
  const onListClick = (e: MouseEvent) => {
    const a = (e.target as Element).closest?.("a.file-link") as HTMLAnchorElement | null;
    if (!a?.dataset.path) return;
    e.preventDefault();
    onOpenPath({ input: a.textContent ?? "", path: a.dataset.path, isDir: a.dataset.dir === "1", size: 0 });
  };

  const send = async () => {
    const text = draft.trim();
    if (!text || sending) return;
    setSending(true);
    setError("");
    stickRef.current = true;
    setPending(text);
    try {
      await api.sendText(sessionId, text);
      setDraft("");
    } catch (e) {
      setPending(null);
      setError((e as Error).message);
    } finally {
      setSending(false);
    }
  };

  const sendKey = (key: InputKey) => api.sendKey(sessionId, key).catch((e) => setError((e as Error).message));

  const choose = async (index: number) => {
    if (!menu || choosing) return;
    setChoosing(true);
    setError("");
    try {
      await api.sendKeys(sessionId, keysToChoose(menu, index));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setChoosing(false);
    }
  };

  const isTouch = typeof window !== "undefined" && window.matchMedia?.("(pointer: coarse)").matches;

  return (
    <div className="chat">
      <div className="chat-list" ref={listRef} onScroll={onScroll} onClick={onListClick}>
        {truncated && <p className="chat-chip muted">以前の会話は省略しています（全体はターミナルで確認できます）</p>}
        {loaded && entries.length === 0 && !pending && (
          <p className="chat-empty muted">まだ会話はありません。下の入力欄から話しかけてみましょう。</p>
        )}
        {entries.map((e) => {
          if (e.type === "group") return <ToolGroupCard key={e.id} g={e} paths={paths} />;
          if (e.type === "tool") return <ToolCard key={e.use.id} t={e} paths={paths} />;
          const it = e.item;
          switch (it.kind) {
            case "user":
              return (
                <div key={it.id} className="chat-row me">
                  <div className="bubble user">
                    <p className="plain">
                      <PathText text={it.text ?? ""} {...paths} />
                    </p>
                    <span className="chat-time">{time(it.at)}</span>
                  </div>
                </div>
              );
            case "assistant":
              return (
                <div key={it.id} className="chat-row">
                  <div className="bubble assistant">
                    <Markdown text={it.text ?? ""} known={known} />
                    <span className="chat-time">{time(it.at)}</span>
                  </div>
                </div>
              );
            case "thinking":
              return <Thinking key={it.id} item={it} />;
            default:
              return (
                <p key={it.id} className="chat-chip">
                  {it.text}
                </p>
              );
          }
        })}
        {pending && (
          <div className="chat-row me">
            <div className="bubble user sending">
              <p className="plain">{pending}</p>
              <span className="chat-time">{running ? "送信中…" : "再開して送信中…"}</span>
            </div>
          </div>
        )}
        {activity === "working" && (
          <div className="chat-typing" aria-live="polite">
            <span className="dots" aria-hidden="true">
              <i />
              <i />
              <i />
            </span>
            作業中
          </div>
        )}
      </div>

      {menu ? (
        <div className="chat-choice" role="group" aria-label="選択肢">
          <div className="chat-choice-head">
            <strong>{menu.question || "選択してください"}</strong>
            <button className="btn small" onClick={onOpenTerminal}>
              ターミナルで見る
            </button>
          </div>
          {/* 何を許可するのか（ツール、コマンドやファイル）を、端末の許可確認の枠からそのまま出す */}
          {menu.context.length > 0 && <pre className="chat-choice-context">{menu.context.join("\n")}</pre>}
          <div className="chat-options">
            {menu.options.map((o) => (
              <button
                key={o.index}
                className={"chat-option" + (o.selected ? " selected" : "")}
                disabled={choosing}
                onClick={() => choose(o.index)}
              >
                <span className="chat-option-label">{o.label}</span>
                {o.detail && <span className="chat-option-detail">{o.detail}</span>}
              </button>
            ))}
          </div>
          <div className="chat-choice-keys">
            <button className="key" disabled={choosing} onClick={() => sendKey("esc")}>
              Esc（キャンセル）
            </button>
          </div>
        </div>
      ) : (
        // 「入力待ち」の通知は選択肢が出てから数秒遅れて届き、ターミナルで選んだ後も次の応答完了まで残る。
        // 画面上でClaudeが作業中なら、もう選択は済んでいるので出さない
        activity === "waiting" &&
        !busy && (
          <div className="chat-choice" role="group" aria-label="選択が必要です">
            <div className="chat-choice-head">
              <strong>選択が必要です</strong>
              <button className="btn small" onClick={onOpenTerminal}>
                ターミナルで見る
              </button>
            </div>
            {/* 許可確認の間はツールの内容がまだ記録に出ないため、通知の本文で何を聞かれているかを示す */}
            {activityDetail && <p className="chat-choice-detail">{activityDetail}</p>}
            <div className="chat-choice-keys">
              {CHOICE_KEYS.map((k) => (
                <button
                  key={k.key}
                  className={"key" + (k.key === "enter" ? " enter" : "")}
                  onClick={() => sendKey(k.key)}
                >
                  {k.label}
                </button>
              ))}
            </div>
          </div>
        )
      )}

      {error && <p className="error chat-error">{error}</p>}
      <div className="chat-input">
        <textarea
          ref={inputRef}
          value={draft}
          rows={1}
          placeholder={isTouch ? "メッセージを入力" : "メッセージを入力（Enterで送信、Shift+EnterかAlt+Enterで改行）"}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key !== "Enter" || e.nativeEvent.isComposing) return;
            const action = enterAction({
              ctrl: e.ctrlKey,
              meta: e.metaKey,
              shift: e.shiftKey,
              alt: e.altKey,
              touch: isTouch,
            });
            if (action === "default") return;
            e.preventDefault();
            if (action === "send") {
              send();
              return;
            }
            const el = e.currentTarget;
            const next = insertNewline(draft, el.selectionStart, el.selectionEnd);
            caretRef.current = next.caret;
            setDraft(next.text);
          }}
        />
        <button className="btn primary" onClick={send} disabled={!draft.trim() || sending}>
          送信
        </button>
      </div>
    </div>
  );
}
