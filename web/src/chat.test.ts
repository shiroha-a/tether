import { describe, expect, it } from "vitest";
import type { ChatItem } from "./api";
import {
  buildBackground,
  buildEntries,
  buildTasks,
  groupEvents,
  groupTools,
  mergeItems,
  toolBreakdown,
  enterAction,
  insertNewline,
} from "./chat";

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

describe("buildTasks", () => {
  const create = (id: string, subject: string) =>
    item(id, "tool_use", { toolId: id, toolName: "TaskCreate", task: { content: subject, status: "pending" } });
  const created = (id: string, n: string, isError = false) =>
    item("r" + id, "tool_result", { toolId: id, taskId: isError ? undefined : n, isError });
  const update = (id: string, task: ChatItem["task"]) =>
    item(id, "tool_use", { toolId: id, toolName: "TaskUpdate", task });

  it("numbers created tasks from their results and applies updates in order", () => {
    const tasks = buildTasks([
      create("c1", "設計"),
      created("c1", "1"),
      create("c2", "実装"),
      created("c2", "2"),
      update("u1", { id: "1", status: "completed" }),
      update("u2", { id: "2", status: "in_progress", activeForm: "実装しています" }),
      // 存在しないタスクの更新は無視する
      update("u3", { id: "9", status: "completed" }),
    ]);
    expect(tasks).toEqual([
      { id: "1", content: "設計", status: "completed" },
      { id: "2", content: "実装", status: "in_progress", activeForm: "実装しています" },
    ]);
  });

  it("drops deleted tasks and creations that failed or have not returned", () => {
    const tasks = buildTasks([
      create("c1", "A"),
      created("c1", "1"),
      create("c2", "B"),
      created("c2", "", true),
      create("c3", "C"),
      update("u1", { id: "1", status: "deleted" }),
    ]);
    expect(tasks).toEqual([]);
  });

  it("replaces the whole list on TodoWrite", () => {
    const tasks = buildTasks([
      create("c1", "古い"),
      created("c1", "1"),
      item("t", "tool_use", {
        toolName: "TodoWrite",
        todos: [
          { content: "A", status: "completed" },
          { content: "B", status: "in_progress", activeForm: "Bを実行中" },
        ],
      }),
    ]);
    expect(tasks.map((t) => `${t.id}:${t.content}:${t.status}`)).toEqual(["1:A:completed", "2:B:in_progress"]);
  });
});

describe("buildBackground", () => {
  const use = (id: string, toolName: string, extra: Partial<ChatItem> = {}) =>
    item(id, "tool_use", { toolId: id, toolName, summary: id + "の説明", ...extra });
  const result = (id: string, extra: Partial<ChatItem> = {}) => item("r" + id, "tool_result", { toolId: id, ...extra });
  const event = (taskId: string, status?: string, extra: Partial<ChatItem> = {}) =>
    item("e" + taskId + (status ?? ""), "task_event", { taskId, status, summary: `${taskId} ${status}`, ...extra });
  const states = (items: ChatItem[]) => buildBackground(items).map((t) => `${t.toolId}:${t.kind}:${t.state}`);

  it("tracks background shells, monitors and agents until they are reported done", () => {
    const items = [
      use("sh", "Bash", { background: true }),
      use("fg", "Bash"),
      result("sh", { taskId: "b1" }),
      use("mon", "Monitor", { background: true }),
      result("mon", { taskId: "m1" }),
      use("ag", "Agent", { background: true }),
      result("ag", { taskId: "a1" }),
    ];
    expect(states(items)).toEqual(["sh:shell:running", "mon:monitor:running", "ag:agent:running"]);
    const done = [
      ...items,
      event("b1", "completed"),
      // Monitorのイベントは状態を持たないので、実行中のまま
      event("m1"),
      event("a1", "failed"),
    ];
    expect(states(done)).toEqual(["sh:shell:completed", "mon:monitor:running", "ag:agent:failed"]);
    expect(buildBackground(done)[2].summary).toBe("a1 failed");
  });

  it("ends a monitor when it reports that it expired", () => {
    const items = [
      use("mon", "Monitor", { background: true }),
      result("mon", { taskId: "m1" }),
      event("m1", undefined, { text: "[Monitor expired after 30m with 9 events delivered]" }),
    ];
    expect(states(items)).toEqual(["mon:monitor:completed"]);
  });

  it("matches notifications by tool id when the task id is unknown", () => {
    const items = [use("sh", "Bash", { background: true }), event("zz", "killed", { toolId: "sh" })];
    expect(states(items)).toEqual(["sh:shell:stopped"]);
  });

  it("ends foreground agents with their result and stopped tasks with TaskStop", () => {
    const items = [
      use("ag", "Agent"),
      use("sh", "Bash", { background: true }),
      result("sh", { taskId: "b1" }),
      use("stop", "TaskStop", { taskId: "b1" }),
    ];
    expect(states(items)).toEqual(["ag:agent:running", "sh:shell:running"]);
    expect(states([...items, result("ag", { taskId: "a9" }), result("stop")])).toEqual([
      "ag:agent:completed",
      "sh:shell:stopped",
    ]);
    // 停止に失敗したら実行中のまま、起動に失敗したものは失敗
    expect(states([...items, result("stop", { isError: true })])).toEqual(["ag:agent:running", "sh:shell:running"]);
    expect(states([use("x", "Bash", { background: true }), result("x", { isError: true })])).toEqual([
      "x:shell:failed",
    ]);
  });
});

describe("groupEvents", () => {
  const ev = (id: string, taskId: string) => ({ type: "message" as const, item: item(id, "task_event", { taskId }) });
  const msg = (id: string) => ({ type: "message" as const, item: item(id, "assistant") });

  it("merges consecutive notifications of the same task only", () => {
    const out = groupEvents([ev("1", "m"), ev("2", "m"), ev("3", "m"), ev("4", "x"), msg("a"), ev("5", "m")]);
    expect(
      out.map((e) =>
        e.type === "events" ? "events:" + e.items.map((i) => i.id).join("") : e.type === "message" ? e.item.id : e.type,
      ),
    ).toEqual(["events:123", "4", "a", "5"]);
  });
});
