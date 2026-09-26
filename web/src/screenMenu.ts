/** One choice of a selection menu drawn by Claude Code in the terminal. */
export interface MenuOption {
  /** Position in the menu (0-based), used to move the cursor with ↑/↓. */
  index: number;
  label: string;
  /** Wrapped continuation lines or the option's description. */
  detail: string;
  selected: boolean;
}

export interface Menu {
  question: string;
  options: MenuOption[];
}

// 選択メニューの下に出る操作の案内（許可確認、信頼確認、AskUserQuestion等）
const FOOTER = /(esc to (cancel|exit|go back)|enter to (confirm|select|continue|submit))/i;
// 罫線だけの行（区切り線や枠）
const DIVIDER = /^[\s─━╌═┄┈\-│┃╭╮╰╯┌┐└┘·]*$/;
const MARKER = "❯";
const NUMBER = /^\d+\.\s+/;

const indentOf = (s: string) => s.length - s.trimStart().length;

// 直前の行が画面の右端からこの文字数以内まで埋まっていれば、次の行は折り返しの続きとみなす
const WRAP_MARGIN = 10;

/**
 * Finds the selection menu Claude Code is showing at the bottom of the screen.
 * The option under the cursor starts with "❯"; the other options start at the
 * same column, deeper-indented lines continue the previous option, and
 * shallower lines above the options form the question. Returns null when no
 * menu is visible (for example the normal "❯" prompt line).
 */
export function parseMenu(screen: string[], cols?: number): Menu | null {
  const lines = screen.map((l) => l.replace(/ /g, " ").replace(/\s+$/, ""));
  const width = cols ?? Math.max(0, ...lines.map((l) => l.length));
  let end = lines.length;
  while (end > 0 && lines[end - 1] === "") end--;

  // フッターは画面の下の方にあるはずなので、下から数行だけ探す
  let footer = -1;
  for (let i = end - 1; i >= Math.max(0, end - 12); i--) {
    if (FOOTER.test(lines[i])) {
      footer = i;
      break;
    }
  }

  // フッター（なければ画面末尾）の直上から、空行・区切り線までを1つのまとまりとして取る
  let bottom = footer >= 0 ? footer : end;
  while (bottom > 0 && lines[bottom - 1] === "") bottom--;
  let top = bottom;
  while (top > 0 && lines[top - 1] !== "" && !DIVIDER.test(lines[top - 1])) top--;
  const block = lines.slice(top, bottom);

  const selectedAt = block.findIndex((l) => l.trimStart().startsWith(MARKER));
  if (selectedAt < 0) return null;
  const sel = block[selectedAt];
  // 選択肢の文字が始まる列（「❯ 」の直後）。ほかの選択肢もこの列から始まる
  const afterMarker = sel.indexOf(MARKER) + MARKER.length;
  const textCol = afterMarker + indentOf(sel.slice(afterMarker));

  const options: MenuOption[] = [];
  const question: string[] = [];
  let current: MenuOption | null = null;
  let prevLen = 0;
  for (let i = 0; i < block.length; i++) {
    const line = block[i];
    const selected = i === selectedAt;
    const indent = selected ? textCol : indentOf(line);
    const text = selected ? line.slice(textCol) : line.trim();
    if (indent === textCol && (selected || !line.trimStart().startsWith(MARKER))) {
      current = { index: options.length, label: text.replace(NUMBER, "").trim(), detail: "", selected };
      options.push(current);
    } else if (indent > textCol && current) {
      // 字下げが深い行は、端末幅での折り返しの続きか、選択肢の説明（AskUserQuestion等）のどちらか。
      // 直前の行が右端近くまで埋まっていれば折り返しなので、直前の文（本文か説明）につなげる
      const wrapped = prevLen >= width - WRAP_MARGIN;
      if (wrapped && !current.detail) current.label += " " + text;
      else current.detail = (current.detail ? current.detail + " " : "") + text;
    } else if (!current) {
      question.push(text);
    } else {
      // 選択肢の後に浅い行が来たら、メニューではない（通常の出力）とみなす
      return null;
    }
    prevLen = line.length;
  }

  if (options.length < 2) return null;
  const numbered = options.every((_, i) =>
    block.some((l) => new RegExp(`^\\s*(${MARKER}\\s*)?${i + 1}\\.\\s`).test(l)),
  );
  // フッターがない場合は、入力欄の「❯」等の誤検出を避けるため番号付きのメニューだけを認める
  if (footer < 0 && !numbered) return null;
  return { question: question.join(" ").trim(), options };
}

/** Keys that move the cursor from the selected option to the target and confirm it. */
export function keysToChoose(menu: Menu, target: number): ("up" | "down" | "enter")[] {
  const from = menu.options.findIndex((o) => o.selected);
  const delta = target - (from < 0 ? 0 : from);
  const moves: ("up" | "down")[] = Array(Math.abs(delta)).fill(delta > 0 ? "down" : "up");
  return [...moves, "enter"];
}
