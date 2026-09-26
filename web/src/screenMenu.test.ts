import { describe, expect, it } from "vitest";
import { keysToChoose, parseMenu, parseScreen } from "./screenMenu";
// 実際のClaude Code（v2.1.280）の許可確認画面をそのまま保存したもの
import permissionScreen from "./testdata/permission-prompt.txt?raw";
// Bashの許可確認と、許可したあとコマンドを実行している最中の画面（v2.1.280、幅84）
import bashScreen from "./testdata/bash-permission.txt?raw";
import runningScreen from "./testdata/bash-running.txt?raw";

const permission = permissionScreen.split("\n");
const bash = bashScreen.split("\n");
const running = runningScreen.split("\n");

describe("parseMenu", () => {
  it("reads the real permission prompt", () => {
    const menu = parseMenu(permission);
    expect(menu).not.toBeNull();
    expect(menu!.question).toBe("Do you want to create tether-menu-probe.txt?");
    // 端末幅で折り返された行は、選択肢の本文につなげる（説明扱いにしない）
    expect(menu!.options.map((o) => o.label)).toEqual([
      "Yes",
      "Yes, and switch to accept edits (auto-approve file edits and common file commands) for this session; Yes, and always allow access to /tmp for this session (shift+tab)",
      "No",
    ]);
    expect(menu!.options.map((o) => o.detail)).toEqual(["", "", ""]);
    expect(menu!.options.map((o) => o.selected)).toEqual([true, false, false]);
  });

  it("reads what a real Bash permission prompt is about", () => {
    const menu = parseMenu(bash, 84)!;
    expect(menu.question).toBe("Do you want to proceed?");
    expect(menu.options.map((o) => [o.label, o.selected])).toEqual([
      ["Yes", true],
      ["No", false],
    ]);
    // 枠線から質問の手前まで。空行は除き、字下げは相対的に残す
    expect(menu.context).toEqual([
      "Bash command",
      "  python3 -c 'import time; time.sleep(25)'; echo tether-$RANDOM",
      "  実行ユーザーが指定したコマンドを実行",
      "Contains simple_expansion",
    ]);
  });

  it("reads the file of a real Write permission prompt, skipping diff dividers", () => {
    expect(parseMenu(permission)!.context).toEqual(["Create file", "../../../../tmp/tether-menu-probe.txt", " 1 ok"]);
  });

  it("returns no context without a panel border, and caps long context", () => {
    const menu = parseMenu(["● some output", "", " Pick one", " ❯ 1. A", "   2. B", " Esc to cancel"])!;
    expect(menu.context).toEqual([]);
    const long = [
      "────────────",
      ...Array.from({ length: 12 }, (_, i) => ` line ${i}`),
      "",
      " Go?",
      " ❯ 1. Yes",
      "   2. No",
      " Esc to cancel",
    ];
    const ctx = parseMenu(long)!.context;
    expect(ctx).toHaveLength(8);
    expect(ctx.slice(0, 2)).toEqual(["line 0", "line 1"]);
    expect(ctx[7]).toBe("…");
    // ちょうど上限なら省略しない
    const exact = [
      "────────────",
      ...Array.from({ length: 8 }, (_, i) => ` line ${i}`),
      "",
      " Go?",
      " ❯ 1. Yes",
      "   2. No",
      " Esc to cancel",
    ];
    expect(parseMenu(exact)!.context).toEqual(Array.from({ length: 8 }, (_, i) => `line ${i}`));
    // 枠線が探す範囲より上にあれば、関係のない出力を拾わないよう何も出さない
    const far = [
      "────────────",
      ...Array.from({ length: 40 }, () => " x"),
      "",
      " Go?",
      " ❯ 1. Yes",
      "   2. No",
      " Esc to cancel",
    ];
    expect(parseMenu(far)!.context).toEqual([]);
  });

  it("follows the cursor when another option is selected", () => {
    const screen = [
      " Do you want to proceed?",
      "   1. Yes",
      " ❯ 2. No, and tell Claude what to do differently",
      " Esc to cancel",
    ];
    const menu = parseMenu(screen)!;
    expect(menu.options.map((o) => [o.label, o.selected])).toEqual([
      ["Yes", false],
      ["No, and tell Claude what to do differently", true],
    ]);
  });

  it("reads unnumbered menus when a footer is present", () => {
    const screen = [
      "──────────────",
      " Quick safety check: Is this a project you created or one you trust?",
      "",
      " ❯ Yes, I trust this folder",
      "   No, exit",
      "",
      " Enter to confirm · Esc to cancel",
    ];
    // 質問文は空行で区切られていて別のまとまりなので、質問は空になり、選択肢だけを読む
    const menu = parseMenu(screen)!;
    expect(menu.options.map((o) => o.label)).toEqual(["Yes, I trust this folder", "No, exit"]);
    expect(menu.question).toBe("");
  });

  it("keeps option descriptions (AskUserQuestion style)", () => {
    const screen = [
      " Which database should we use?",
      " ❯ 1. PostgreSQL",
      "      Relational, strong consistency",
      "   2. SQLite",
      "      Single file, zero setup",
      " Enter to select · Esc to cancel",
    ];
    const menu = parseMenu(screen)!;
    expect(menu.options.map((o) => `${o.label}|${o.detail}`)).toEqual([
      "PostgreSQL|Relational, strong consistency",
      "SQLite|Single file, zero setup",
    ]);
  });

  it("uses the terminal width, not the longest visible line, to detect wrapping", () => {
    // 幅80の端末で、選択肢の行（38文字）は右端まで届いていないので、次の行は説明
    const screen = [
      " Choose a plan",
      " ❯ 1. Keep the current schema as it is",
      "      Safe, no migration needed",
      "   2. Migrate",
      " Enter to select · Esc to cancel",
    ];
    expect(parseMenu(screen, 80)!.options[0]).toMatchObject({
      label: "Keep the current schema as it is",
      detail: "Safe, no migration needed",
    });
  });

  it("joins a wrapped description to the description, not the label", () => {
    const screen = [
      " Pick one",
      " ❯ 1. Fast",
      "      Uses a cache that is rebuilt every", // 右端近くまで埋まった説明の1行目（38文字）
      "      night",
      "   2. Slow",
      " Enter to select · Esc to cancel",
    ];
    const menu = parseMenu(screen, 40)!;
    expect(menu.options.map((o) => `${o.label}|${o.detail}`)).toEqual([
      "Fast|Uses a cache that is rebuilt every night",
      "Slow|",
    ]);
  });

  it("ignores the normal input prompt and ordinary output", () => {
    const idle = [
      "● 了解しました。",
      "─────────────────",
      "❯ ",
      "─────────────────",
      "  ⏸ manual mode on · ? for shortcuts",
    ];
    expect(parseMenu(idle)).toBeNull();
    // 入力欄に番号付きの文字を打っていても、1行だけならメニューとみなさない
    expect(parseMenu(["─────", "❯ 1. fix the bug", "─────"])).toBeNull();
    // 選択肢が1つだけのものは選ぶ余地がないのでメニューとみなさない（入力欄の「❯ 1. …」等）
    expect(parseMenu(["❯ 1. fix the bug"])).toBeNull();
    expect(parseMenu([" Q?", " ❯ 1. Only", " Esc to cancel"])).toBeNull();
    // フッターがなく番号もない「❯」の並びはメニューとみなさない
    expect(parseMenu(["❯ foo", "  bar"])).toBeNull();
    expect(parseMenu([])).toBeNull();
  });

  it("rejects blocks where a shallower line follows the options", () => {
    const screen = [" Title", " ❯ 1. a", "   2. b", "tail at column 0", " Esc to cancel"];
    expect(parseMenu(screen)).toBeNull();
  });
});

describe("keysToChoose", () => {
  const menu = parseMenu([" Q?", "   1. A", " ❯ 2. B", "   3. C", "   4. D", " Esc to cancel"])!;

  it("moves from the selected option to the target and confirms", () => {
    expect(keysToChoose(menu, 1)).toEqual(["enter"]);
    expect(keysToChoose(menu, 3)).toEqual(["down", "down", "enter"]);
    expect(keysToChoose(menu, 0)).toEqual(["up", "enter"]);
  });
});

describe("parseScreen", () => {
  it("reports busy while Claude Code runs a command, with no menu", () => {
    expect(parseScreen(running, 84)).toEqual({ menu: null, busy: true });
  });

  it("is not busy while a menu is shown or when idle", () => {
    const s = parseScreen(bash, 84);
    expect(s.menu?.options).toHaveLength(2);
    expect(s.busy).toBe(false);
    const idle = ["● 完了しました。", "", "────────", "❯ ", "────────", "  ⏸ manual mode on · ← for agents"];
    expect(parseScreen(idle)).toEqual({ menu: null, busy: false });
  });

  it("only looks at the bottom of the screen for the busy status", () => {
    // 会話の本文に「esc to interrupt」と書かれていても作業中とはみなさない
    const text = [
      "● Press esc to interrupt a run.",
      "",
      "a",
      "b",
      "c",
      "────────",
      "❯ ",
      "────────",
      "  ⏸ manual mode on",
    ];
    expect(parseScreen(text).busy).toBe(false);
  });
});
