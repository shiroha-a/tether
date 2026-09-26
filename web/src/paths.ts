/** A file path candidate found in text. `start`/`end` are string offsets. */
export interface PathMatch {
  start: number;
  end: number;
  path: string;
}

/** An existing path confirmed by the server (see POST /api/fs/stat). */
export interface KnownPath {
  input: string;
  path: string;
  isDir: boolean;
  size: number;
}

// パスに使われるASCII文字。日本語などはパスの外とみなすので「report.mdに出力しました」でも切れる
// 「.tmp」「.env」のように「.」で始まる名前も含める（「..」だけのものは下の条件で除く）
const SEG = "[A-Za-z0-9_.@+%=~-]+";
const CANDIDATE = new RegExp(
  // 前がパスの文字や「/」なら途中から拾わない（URLの途中などを避ける）
  `(?<![A-Za-z0-9_./~@+%=:-])` +
    // 先頭は ~/ ./ ../ / のいずれか、または何も付かない
    `((?:~|\\.{1,2})?/?(?:${SEG}/)*${SEG})` +
    // 後ろにパスの文字が続く位置では終わらない（行番号の「:42」は続いてよい）
    `(?![A-Za-z0-9_/@+%=~-])`,
  "g",
);
const EXTENSION = /\.[A-Za-z0-9]*[A-Za-z][A-Za-z0-9]{0,9}$/;
const MAX_LEN = 300;

/**
 * Finds strings that look like file paths: absolute paths, "~/…", relative
 * paths with a slash, and file names with an extension. Trailing dots are
 * dropped and URLs are skipped. Candidates still have to be confirmed by the
 * server before they are shown as links.
 */
export function extractPaths(text: string): PathMatch[] {
  const out: PathMatch[] = [];
  for (const m of text.matchAll(CANDIDATE)) {
    let path = m[1];
    const start = m.index!;
    // 文末の「.」や「。」の前の「.」はパスの一部ではない
    path = path.replace(/[.]+$/, "");
    if (!path || path.length > MAX_LEN) continue;
    const hasSlash = path.includes("/");
    if (!hasSlash) {
      if (!EXTENSION.test(path)) continue;
      // 「e.g」のような略語を拾わないよう、名前か拡張子のどちらかは2文字以上とする
      const dot = path.lastIndexOf(".");
      if (dot < 2 && path.length - dot - 1 < 2) continue;
    }
    // 「/」だけ、「~」だけ、「..」だけのようなものは除く
    if (!/[A-Za-z0-9_]/.test(path)) continue;
    out.push({ start, end: start + path.length, path });
  }
  return out;
}

/** Unique candidate paths in the given texts, in first-seen order. */
export function collectPaths(texts: (string | undefined)[]): string[] {
  const seen = new Set<string>();
  for (const t of texts) {
    if (!t) continue;
    for (const m of extractPaths(t)) seen.add(m.path);
  }
  return [...seen];
}

/**
 * Wraps known paths in rendered HTML with <a class="file-link">. Text inside
 * existing links is left alone; code and pre blocks are included because
 * Claude often formats paths as code.
 */
export function linkifyHtml(html: string, known: Map<string, KnownPath>): string {
  if (known.size === 0) return html;
  const doc = new DOMParser().parseFromString(`<body>${html}</body>`, "text/html");
  const walker = doc.createTreeWalker(doc.body, NodeFilter.SHOW_TEXT);
  const nodes: Text[] = [];
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    if (!n.parentElement?.closest("a")) nodes.push(n as Text);
  }
  for (const node of nodes) {
    const text = node.data;
    const matches = extractPaths(text).filter((m) => known.has(m.path));
    if (matches.length === 0) continue;
    const frag = doc.createDocumentFragment();
    let pos = 0;
    for (const m of matches) {
      const k = known.get(m.path)!;
      frag.append(text.slice(pos, m.start));
      const a = doc.createElement("a");
      a.className = "file-link";
      a.setAttribute("href", "#");
      a.dataset.path = k.path;
      if (k.isDir) a.dataset.dir = "1";
      a.textContent = text.slice(m.start, m.end);
      frag.append(a);
      pos = m.end;
    }
    frag.append(text.slice(pos));
    node.replaceWith(frag);
  }
  return doc.body.innerHTML;
}
