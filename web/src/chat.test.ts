import { describe, expect, it } from "vitest";
import type { ChatItem } from "./api";
import { buildEntries, groupTools, mergeItems, toolBreakdown, enterAction, insertNewline } from "./chat";

const item = (id: string, kind: ChatItem["kind"], extra: Partial<ChatItem> = {}): ChatItem => ({
  id,
  kind,
  at: "2026-09-26T00:00:00Z",
  ...extra,
});

describe("mergeItems", () => {
  it("appends new items and skips ids that are already present", () => {
    const a = [item("1", "user"), item("2", "assistant")];
    const merged = mergeItems(a, [item("2", "assistant"), item("3", "user"), item("3", "user")]);
    expect(merged.map((i) => i.id)).toEqual(["1", "2", "3"]);
  });

  it("returns the same array when nothing is new (avoids re-rendering)", () => {
    const a = [item("1", "user")];
    expect(mergeItems(a, [])).toBe(a);
    expect(mergeItems(a, [item("1", "user")])).toBe(a);
  });
});

describe("buildEntries", () => {
  it("attaches each tool result to its call, even when it arrives later", () => {
    const items = [
      item("u", "user", { text: "ls" }),
      item("t1", "tool_use", { toolId: "A", toolName: "Bash" }),
      item("t2", "tool_use", { toolId: "B", toolName: "Read" }),
      item("r2", "tool_result", { toolId: "B", text: "file" }),
      item("a", "assistant", { text: "done" }),
      item("r1", "tool_result", { toolId: "A", text: "out", isError: true }),
    ];
    const entries = buildEntries(items);
    expect(
      entries.map((e) => (e.type === "tool" ? `tool:${e.use.toolName}:${e.result?.text ?? "-"}` : e.item.id)),
    ).toEqual(["u", "tool:Bash:out", "tool:Read:file", "a"]);
    const bash = entries[1];
    expect(bash.type === "tool" && bash.result?.isError).toBe(true);
  });

  it("leaves a call without result pending and drops orphan results", () => {
    const entries = buildEntries([
      item("r0", "tool_result", { toolId: "gone" }),
      item("t", "tool_use", { toolId: "X" }),
    ]);
    expect(entries).toHaveLength(1);
    expect(entries[0].type === "tool" && entries[0].result).toBeUndefined();
  });
});

describe("groupTools", () => {
  const tool = (id: string, name: string, done = true) => [
    item("u" + id, "tool_use", { toolId: id, toolName: name }),
    ...(done ? [item("r" + id, "tool_result", { toolId: id })] : []),
  ];
  const shape = (items: ChatItem[]) =>
    groupTools(buildEntries(items)).map((e) =>
      e.type === "group"
        ? `group(${e.entries.map((x) => (x.type === "tool" ? x.use.toolName : "think")).join(",")})`
        : e.type === "tool"
          ? `tool(${e.use.toolName})`
          : e.item.kind,
    );

  it("collapses consecutive tool calls, including the thinking between them", () => {
    const items = [
      item("q", "user"),
      item("t0", "thinking"),
      ...tool("1", "Read"),
      item("t1", "thinking"),
      ...tool("2", "Bash"),
      ...tool("3", "Bash"),
      item("t2", "thinking"),
      item("a", "assistant"),
    ];
    // 先頭の思考は最初のツールの前なのでまとまりに入る。末尾の思考は返答の前置きなので外に出す
    expect(shape(items)).toEqual(["user", "group(think,Read,think,Bash,Bash)", "thinking", "assistant"]);
  });

  it("keeps a single tool call as a normal card", () => {
    expect(shape([item("q", "user"), item("t", "thinking"), ...tool("1", "Edit"), item("a", "assistant")])).toEqual([
      "user",
      "thinking",
      "tool(Edit)",
      "assistant",
    ]);
  });

  it("splits runs at messages", () => {
    const items = [
      ...tool("1", "Read"),
      ...tool("2", "Read"),
      item("a", "assistant"),
      ...tool("3", "Bash"),
      ...tool("4", "Grep"),
    ];
    expect(shape(items)).toEqual(["group(Read,Read)", "assistant", "group(Bash,Grep)"]);
  });

  it("gives a group a stable id from its first tool", () => {
    const g = groupTools(buildEntries([...tool("1", "Read"), ...tool("2", "Read")]))[0];
    expect(g.type === "group" && g.id).toBe("group:u1");
  });
});

describe("toolBreakdown", () => {
  it("counts tools by name in order of first use", () => {
    const tools = groupTools(
      buildEntries(
        ["Bash", "Read", "Bash", "Bash", "Read", "Grep"].map((n, i) =>
          item("u" + i, "tool_use", { toolId: String(i), toolName: n }),
        ),
      ),
    )[0];
    expect(tools.type === "group" && toolBreakdown(tools.tools)).toBe("Bash×3、Read×2、Grep");
  });
});

describe("enterAction", () => {
  const k = (o: Partial<Record<"ctrl" | "meta" | "shift" | "alt" | "touch", boolean>>) =>
    enterAction({ ctrl: false, meta: false, shift: false, alt: false, touch: false, ...o });
  it("sends with Enter on a keyboard and with Ctrl/Cmd+Enter everywhere", () => {
    expect(k({})).toBe("send");
    expect(k({ ctrl: true })).toBe("send");
    expect(k({ meta: true })).toBe("send");
    expect(k({ touch: true, ctrl: true })).toBe("send");
    expect(k({ touch: true, meta: true })).toBe("send");
  });
  it("inserts a newline with Alt+Enter, even on touch devices or with other modifiers", () => {
    expect(k({ alt: true })).toBe("newline");
    expect(k({ alt: true, touch: true })).toBe("newline");
    expect(k({ alt: true, ctrl: true })).toBe("newline");
    expect(k({ alt: true, shift: true })).toBe("newline");
  });
  it("leaves Shift+Enter and Enter on touch devices to the browser (newline)", () => {
    expect(k({ shift: true })).toBe("default");
    expect(k({ touch: true })).toBe("default");
  });
});

describe("insertNewline", () => {
  it("inserts at the caret or replaces the selection", () => {
    expect(insertNewline("abcd", 2, 2)).toEqual({ text: "ab\ncd", caret: 3 });
    expect(insertNewline("abcd", 1, 3)).toEqual({ text: "a\nd", caret: 2 });
    expect(insertNewline("", 0, 0)).toEqual({ text: "\n", caret: 1 });
    expect(insertNewline("ab", 2, 2)).toEqual({ text: "ab\n", caret: 3 });
  });
});
