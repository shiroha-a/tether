import type { ChatItem } from "./api";

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

export type DisplayEntry = ChatEntry | ToolGroup;

const isThinking = (e: ChatEntry): e is ThinkingEntry => e.type === "message" && e.item.kind === "thinking";

/**
 * Collapses runs of two or more tool calls into one group. Thinking between
 * the calls belongs to the run; a message from the user or Claude ends it.
 */
export function groupTools(entries: ChatEntry[]): DisplayEntry[] {
  const out: DisplayEntry[] = [];
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
