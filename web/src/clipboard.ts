/** Minimal view of an xterm buffer, so the conversion can be tested without a DOM. */
export interface BufferLike {
  length: number;
  getLine(y: number): { isWrapped: boolean; translateToString(trimRight?: boolean): string } | undefined;
}

/**
 * Converts terminal buffer rows [start, end) to plain text. Soft-wrapped rows
 * are joined to the previous row, trailing spaces and trailing blank lines are removed.
 */
export function bufferToText(buf: BufferLike, start = 0, end = buf.length): string {
  const lines: string[] = [];
  for (let y = Math.max(0, start); y < Math.min(end, buf.length); y++) {
    const line = buf.getLine(y);
    if (!line) continue;
    // 右端の空白を削ってから連結すると、ちょうど空白の位置で折り返された行の空白が消えるため、
    // 行は空白を残したまま取り出し、論理行ができてから行末の空白を削る
    const text = line.translateToString(false);
    // 端末幅で折り返された行は、元は1行なので前の行につなげる（コピー後に余計な改行が入らないように）
    if (line.isWrapped && lines.length > 0) lines[lines.length - 1] += text;
    else lines.push(text);
  }
  while (lines.length > 0 && lines[lines.length - 1].trim() === "") lines.pop();
  return lines.map((l) => l.replace(/\s+$/, "")).join("\n");
}

/**
 * Parses the payload of an OSC 52 sequence ("<targets>;<base64>") and returns
 * the decoded UTF-8 text, or null for queries ("?") and malformed input.
 */
export function decodeOsc52(payload: string): string | null {
  const sep = payload.indexOf(";");
  if (sep < 0) return null;
  const data = payload.slice(sep + 1);
  if (data === "" || data === "?") return null;
  try {
    const bin = atob(data);
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
    return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    return null;
  }
}

/**
 * Copies text to the clipboard. Uses the async Clipboard API when available
 * (HTTPS or localhost) and falls back to execCommand, which also works over
 * plain HTTP but only inside a user gesture. Returns whether it succeeded.
 */
export async function copyText(text: string): Promise<boolean> {
  if (window.isSecureContext && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 権限がない等で失敗したら下の方法を試す
    }
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  // 画面外に置き、スマホでキーボードやズームが発生しないようにする
  ta.style.cssText = "position:fixed;top:0;left:-9999px;opacity:0;font-size:16px";
  document.body.appendChild(ta);
  ta.select();
  ta.setSelectionRange(0, text.length);
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch {
    ok = false;
  }
  ta.remove();
  return ok;
}

/** Reads text from the clipboard, or returns null when the API is unavailable or denied. */
export async function readClipboard(): Promise<string | null> {
  if (!window.isSecureContext || !navigator.clipboard?.readText) return null;
  try {
    return await navigator.clipboard.readText();
  } catch {
    return null;
  }
}
