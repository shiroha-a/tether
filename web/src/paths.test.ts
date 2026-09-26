// @vitest-environment happy-dom
import { describe, expect, it } from "vitest";
import { collectPaths, extractPaths, linkifyHtml, type KnownPath } from "./paths";

const paths = (text: string) => extractPaths(text).map((m) => m.path);

describe("extractPaths", () => {
  it("finds absolute, home, relative and bare file names", () => {
    expect(paths("レポートを /home/u/dev/app/report.md に出力しました")).toEqual(["/home/u/dev/app/report.md"]);
    expect(paths("see ~/notes/todo.txt and ./out/a.html or ../b.csv")).toEqual([
      "~/notes/todo.txt",
      "./out/a.html",
      "../b.csv",
    ]);
    expect(paths("docs/report.md を更新")).toEqual(["docs/report.md"]);
    // 「.」で始まるフォルダやファイル（隠しファイル）
    expect(paths("wrote /home/u/app/.tmp/report.html and .env")).toEqual(["/home/u/app/.tmp/report.html", ".env"]);
    expect(paths("report.mdに出力しました")).toEqual(["report.md"]);
  });

  it("stops at Japanese text, trailing punctuation and line numbers", () => {
    expect(paths("「summary.md」を作りました。")).toEqual(["summary.md"]);
    expect(paths("Saved to out/result.json.")).toEqual(["out/result.json"]);
    expect(paths("see src/app.ts:42:7 for details")).toEqual(["src/app.ts"]);
    expect(paths("(web/src/main.tsx)")).toEqual(["web/src/main.tsx"]);
  });

  it("reports offsets that point at the path in the original text", () => {
    const text = "出力: `out/a.md` です";
    const [m] = extractPaths(text);
    expect(text.slice(m.start, m.end)).toBe("out/a.md");
    // 末尾の「.」を落とした場合も、範囲はパスの部分だけを指す
    const text2 = "Saved to out/a.md.";
    const [m2] = extractPaths(text2);
    expect(text2.slice(m2.start, m2.end)).toBe("out/a.md");
  });

  it("skips URLs, bare words, versions and lone symbols", () => {
    expect(paths("https://example.com/docs/a.md を参照")).toEqual([]);
    expect(paths("http://localhost:3100/api/health")).toEqual([]);
    expect(paths("e.g. this and that, version 1.2.3, v2.0")).toEqual([]);
    expect(paths("path / and ~ and .. alone")).toEqual([]);
    // 記号だけでできたパスらしきもの
    expect(paths("a -/- b and ~/~ c")).toEqual([]);
    expect(paths("plain words only")).toEqual([]);
    // 名前か拡張子が2文字以上なら拾う
    expect(paths("x.md and ab.c")).toEqual(["x.md", "ab.c"]);
  });

  it("does not start in the middle of a path-like token", () => {
    expect(paths("abc/def/ghi.txt")).toEqual(["abc/def/ghi.txt"]);
    expect(paths("key=value/path.md")).toEqual(["key=value/path.md"]);
  });
});

describe("collectPaths", () => {
  it("returns unique candidates in order", () => {
    expect(collectPaths(["a.md and b.md", undefined, "b.md again, c/d.txt"])).toEqual(["a.md", "b.md", "c/d.txt"]);
  });
});

describe("linkifyHtml", () => {
  const known = new Map<string, KnownPath>([
    ["report.md", { input: "report.md", path: "/home/u/app/report.md", isDir: false, size: 10 }],
    ["out", { input: "out", path: "/home/u/app/out", isDir: true, size: 0 }],
    ["docs/x.md", { input: "docs/x.md", path: "/home/u/app/docs/x.md", isDir: false, size: 1 }],
  ]);

  it("wraps only known paths and keeps the surrounding text", () => {
    const out = linkifyHtml("<p>report.mdに出力、missing.md は無し</p>", known);
    expect(out).toBe(
      '<p><a class="file-link" href="#" data-path="/home/u/app/report.md">report.md</a>に出力、missing.md は無し</p>',
    );
  });

  it("links paths inside code and marks directories", () => {
    const out = linkifyHtml("<p><code>docs/x.md</code></p>", known);
    expect(out).toContain('<code><a class="file-link" href="#" data-path="/home/u/app/docs/x.md">docs/x.md</a></code>');
    const dirs = new Map(known).set("app/out", { input: "app/out", path: "/home/u/app/out", isDir: true, size: 0 });
    expect(linkifyHtml("<p>see app/out</p>", dirs)).toBe(
      '<p>see <a class="file-link" href="#" data-path="/home/u/app/out" data-dir="1">app/out</a></p>',
    );
  });

  it("leaves existing links untouched", () => {
    const html = '<p><a href="https://example.com/report.md">report.md</a></p>';
    expect(linkifyHtml(html, known)).toBe(html);
  });

  it("returns the input when nothing is known", () => {
    expect(linkifyHtml("<p>report.md</p>", new Map())).toBe("<p>report.md</p>");
  });
});
