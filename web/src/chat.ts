import type { ChatItem, Hunk, Todo } from "./api";

/** A row in the chat view. Tool calls carry their result once it arrives. */
export type ChatEntry = { type: "message"; item: ChatItem } | { type: "tool"; use: ChatItem; result?: ChatItem };

/** Appends incoming items, ignoring ids already present (polls may overlap). */
export function mergeItems(existing: ChatItem[], incoming: ChatItem[]): ChatItem[] {
  const seen = new Set(existing.map((i) => i.id));
  const fresh = incoming.filter((i) => !seen.has(i.id) && seen.add(i.id));
  return fresh.length === 0 ? existing : [...existing, ...fresh];
}

/**
 * Turns items into display rows: each tool_result is attached to its tool_use
 * (they arrive on separate lines, possibly in later polls). Results whose call
 * is not loaded (e.g. cut off by the initial tail) are dropped.
 */
export function buildEntries(items: ChatItem[]): ChatEntry[] {
  const results = new Map<string, ChatItem>();
  for (const it of items) {
    if (it.kind === "tool_result" && it.toolId) results.set(it.toolId, it);
  }
  const out: ChatEntry[] = [];
  for (const it of items) {
    if (it.kind === "tool_result") continue;
    if (it.kind === "tool_use")
      out.push({ type: "tool", use: it, result: it.toolId ? results.get(it.toolId) : undefined });
    else out.push({ type: "message", item: it });
  }
  return out;
}

type ToolEntry = Extract<ChatEntry, { type: "tool" }>;
type ThinkingEntry = { type: "message"; item: ChatItem & { kind: "thinking" } };

/** A run of consecutive tool calls (and the thinking between them) shown as one block. */
export type ToolGroup = {
  type: "group";
  id: string;
  entries: (ToolEntry | ThinkingEntry)[];
  tools: ToolEntry[];
};

/** Consecutive notifications from the same background task shown as one card. */
export type EventGroup = { type: "events"; id: string; items: ChatItem[] };

export type DisplayEntry = ChatEntry | ToolGroup | EventGroup;

const isThinking = (e: ChatEntry): e is ThinkingEntry => e.type === "message" && e.item.kind === "thinking";

/**
 * Collapses runs of two or more tool calls into one group. Thinking between
 * the calls belongs to the run; a message from the user or Claude ends it.
 */
export function groupTools(entries: ChatEntry[]): (ChatEntry | ToolGroup)[] {
  const out: (ChatEntry | ToolGroup)[] = [];
  let run: (ToolEntry | ThinkingEntry)[] = [];
  const flush = () => {
    // 末尾の思考は次の返答の前置きなので、まとまりには含めず個別に出す
    let end = run.length;
    while (end > 0 && isThinking(run[end - 1])) end--;
    const body = run.slice(0, end);
    const tools = body.filter((e): e is ToolEntry => e.type === "tool");
    if (tools.length >= 2) out.push({ type: "group", id: "group:" + tools[0].use.id, entries: body, tools });
    else out.push(...body);
    out.push(...run.slice(end));
    run = [];
  };
  for (const e of entries) {
    if (e.type === "tool" || isThinking(e)) {
      run.push(e);
      continue;
    }
    flush();
    out.push(e);
  }
  flush();
  return out;
}

/** Tool names with counts in order of first use, e.g. "Bash×3、Read×2". */
export function toolBreakdown(tools: ToolEntry[]): string {
  const counts = new Map<string, number>();
  for (const t of tools) {
    const name = t.use.toolName || "ツール";
    counts.set(name, (counts.get(name) ?? 0) + 1);
  }
  return [...counts].map(([name, n]) => (n > 1 ? `${name}×${n}` : name)).join("、");
}

/** What Enter does in the chat input, given the modifier keys. */
export function enterAction(k: {
  ctrl: boolean;
  meta: boolean;
  shift: boolean;
  alt: boolean;
  touch: boolean;
}): "send" | "newline" | "default" {
  // Alt+Enterはブラウザが改行を入れないので、自分で入れる
  if (k.alt) return "newline";
  if (k.ctrl || k.meta) return "send";
  // スマホではEnterは改行にして、送信はボタンで行う。Shift+Enterはブラウザの改行に任せる
  if (k.touch || k.shift) return "default";
  return "send";
}

/** Replaces the selection with a newline and returns the new text and caret position. */
export function insertNewline(text: string, start: number, end: number): { text: string; caret: number } {
  return { text: text.slice(0, start) + "\n" + text.slice(end), caret: start + 1 };
}

/**
 * Rebuilds Claude Code's task list from the loaded items, the same way Claude
 * Code does: TaskCreate gets its number from the result ("Task #N created"),
 * TaskUpdate changes or deletes a task, and TodoWrite replaces the whole list.
 */
export function buildTasks(items: ChatItem[]): Todo[] {
  let tasks = new Map<string, Todo>();
  const creating = new Map<string, Todo>();
  for (const it of items) {
    if (it.kind === "tool_use" && it.toolName === "TodoWrite" && it.todos) {
      tasks = new Map(it.todos.map((t, i) => [String(i), { ...t, id: String(i + 1) }]));
    } else if (it.kind === "tool_use" && it.toolName === "TaskCreate" && it.task && it.toolId) {
      creating.set(it.toolId, it.task);
    } else if (it.kind === "tool_result" && it.toolId && it.taskId && creating.has(it.toolId)) {
      tasks.set(it.taskId, { ...creating.get(it.toolId), id: it.taskId });
      creating.delete(it.toolId);
    } else if (it.kind === "tool_use" && it.toolName === "TaskUpdate" && it.task?.id) {
      const { id, status, content, activeForm } = it.task;
      const cur = tasks.get(id);
      if (!cur) continue;
      if (status === "deleted") tasks.delete(id);
      else
        tasks.set(id, {
          ...cur,
          ...(status ? { status } : {}),
          ...(content ? { content } : {}),
          ...(activeForm ? { activeForm } : {}),
        });
    }
  }
  return [...tasks.values()];
}

export type BackgroundKind = "shell" | "agent" | "monitor";
export type BackgroundState = "running" | "completed" | "failed" | "stopped";

/** A shell, agent or monitor that Claude started and that may still be running. */
export interface BackgroundTask {
  /** tool_use id of the call that started it. */
  toolId: string;
  kind: BackgroundKind;
  description: string;
  /** Id Claude Code assigned (known once the call returns). */
  taskId?: string;
  startedAt: string;
  state: BackgroundState;
  /** Latest summary from a task notification. */
  summary?: string;
}

const AGENT_TOOLS = new Set(["Agent", "Task"]);

function eventState(status: string | undefined): BackgroundState | undefined {
  switch (status) {
    case "completed":
      return "completed";
    case "failed":
      return "failed";
    case "killed":
    case "stopped":
      return "stopped";
  }
  // Monitorのイベント通知は状態を持たず、監視は続いている
  return undefined;
}

/** Monitor reports its timeout as an event without a status. */
const MONITOR_EXPIRED = /^\[Monitor expired/;

/**
 * Lists the background work found in the loaded items: background shells and
 * monitors, and agents (background ones, and foreground ones until they
 * return). Completion comes from task notifications, TaskStop, or for
 * foreground agents their result.
 */
export function buildBackground(items: ChatItem[]): BackgroundTask[] {
  const byTool = new Map<string, BackgroundTask>();
  const byTask = new Map<string, BackgroundTask>();
  const stopping = new Map<string, string>();
  const foreground = new Set<string>();
  for (const it of items) {
    if (it.kind === "tool_use" && it.toolId) {
      const agent = AGENT_TOOLS.has(it.toolName ?? "");
      if (agent || it.background) {
        byTool.set(it.toolId, {
          toolId: it.toolId,
          kind: agent ? "agent" : it.toolName === "Monitor" ? "monitor" : "shell",
          description: it.summary || it.toolName || "",
          startedAt: it.at,
          state: "running",
        });
        if (agent && !it.background) foreground.add(it.toolId);
      }
      if (it.toolName === "TaskStop" && it.taskId) stopping.set(it.toolId, it.taskId);
      continue;
    }
    if (it.kind === "tool_result" && it.toolId) {
      const t = byTool.get(it.toolId);
      if (t) {
        if (it.taskId) {
          t.taskId = it.taskId;
          byTask.set(it.taskId, t);
        }
        // 前面で動くエージェントは結果が返ったら終わり。起動に失敗したものも終わり
        if (it.isError) t.state = "failed";
        else if (foreground.has(it.toolId)) t.state = "completed";
      }
      const stopped = stopping.get(it.toolId);
      if (stopped && !it.isError) {
        const target = byTask.get(stopped);
        if (target) target.state = "stopped";
      }
      continue;
    }
    if (it.kind === "task_event") {
      const t = (it.taskId && byTask.get(it.taskId)) || (it.toolId && byTool.get(it.toolId)) || undefined;
      if (!t) continue;
      t.state = eventState(it.status) ?? (MONITOR_EXPIRED.test(it.text ?? "") ? "completed" : t.state);
      if (it.summary) t.summary = it.summary;
    }
  }
  return [...byTool.values()];
}

/** One line of a diff with its line numbers (when the hunk position is known). */
export interface DiffRow {
  type: "add" | "del" | "ctx";
  text: string;
  oldNo?: number;
  newNo?: number;
}

/** Splits a hunk into rows and numbers them from the hunk's start lines. */
export function diffRows(h: Hunk): DiffRow[] {
  let oldNo = h.oldStart;
  let newNo = h.newStart;
  return h.lines.map((line) => {
    const text = line.slice(1);
    if (line.startsWith("+")) return { type: "add", text, newNo: newNo > 0 ? newNo++ : undefined };
    if (line.startsWith("-")) return { type: "del", text, oldNo: oldNo > 0 ? oldNo++ : undefined };
    return {
      type: "ctx",
      text,
      oldNo: oldNo > 0 ? oldNo++ : undefined,
      newNo: newNo > 0 ? newNo++ : undefined,
    };
  });
}

/** Time since start in a short Japanese form, e.g. "45秒", "12分", "1時間5分". */
export function elapsed(startIso: string, now: number): string {
  const sec = Math.max(0, Math.floor((now - new Date(startIso).getTime()) / 1000));
  if (Number.isNaN(sec)) return "";
  if (sec < 60) return `${sec}秒`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}分`;
  return `${Math.floor(min / 60)}時間${min % 60}分`;
}

/** What a tool card shows as its one-line summary. */
export function toolSummary(use: ChatItem): string {
  if (use.toolName === "TaskCreate" && use.task?.content) return use.task.content;
  if (use.toolName === "TaskUpdate" && use.task?.id) {
    const change = use.task.status ?? use.task.content ?? "";
    return `#${use.task.id} ${change}`.trim();
  }
  if (use.toolName === "TodoWrite" && use.todos) {
    const done = use.todos.filter((t) => t.status === "completed").length;
    return `${done}/${use.todos.length}`;
  }
  return use.summary ?? "";
}

/**
 * Merges runs of notifications from the same background task (a monitor
 * reports every event separately) into one entry.
 */
export function groupEvents(entries: DisplayEntry[]): DisplayEntry[] {
  const out: DisplayEntry[] = [];
  for (const e of entries) {
    const prev = out[out.length - 1];
    if (e.type === "message" && e.item.kind === "task_event" && e.item.taskId) {
      if (prev?.type === "events" && prev.items[0].taskId === e.item.taskId) {
        prev.items.push(e.item);
        continue;
      }
      if (prev?.type === "message" && prev.item.kind === "task_event" && prev.item.taskId === e.item.taskId) {
        out[out.length - 1] = { type: "events", id: "events:" + prev.item.id, items: [prev.item, e.item] };
        continue;
      }
    }
    out.push(e);
  }
  return out;
}
