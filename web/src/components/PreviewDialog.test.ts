// @vitest-environment happy-dom
import { describe, expect, it } from "vitest";
import { resolveImages } from "./PreviewDialog";

const srcs = (html: string) => [...html.matchAll(/src="([^"]*)"/g)].map((m) => m[1].replaceAll("&amp;", "&"));
const dl = (p: string) => `/api/fs/download?path=${encodeURIComponent(p)}&inline=1`;

describe("resolveImages", () => {
  it("resolves relative image paths against the markdown file's folder", () => {
    const html = '<p><img src="icon.png"><img src="./img/a.png"><img src="../up.png"></p>';
    expect(srcs(resolveImages(html, "/home/u/docs/readme.md"))).toEqual([
      dl("/home/u/docs/icon.png"),
      dl("/home/u/docs/img/a.png"),
      dl("/home/u/up.png"),
    ]);
  });

  it("serves absolute server paths through the download API", () => {
    expect(srcs(resolveImages('<img src="/srv/pic.png">', "/home/u/readme.md"))).toEqual([dl("/srv/pic.png")]);
  });

  it("keeps URLs that already have a scheme or are protocol-relative", () => {
    const html =
      '<img src="https://example.com/a.png"><img src="data:image/png;base64,AAAA"><img src="//cdn.example/x.png">';
    expect(srcs(resolveImages(html, "/home/u/readme.md"))).toEqual([
      "https://example.com/a.png",
      "data:image/png;base64,AAAA",
      "//cdn.example/x.png",
    ]);
  });

  it("decodes percent-encoded and non-ASCII names once", () => {
    const html = '<img src="my%20pic.png"><img src="画像.png">';
    expect(srcs(resolveImages(html, "/home/u/メモ/readme.md"))).toEqual([
      dl("/home/u/メモ/my pic.png"),
      dl("/home/u/メモ/画像.png"),
    ]);
  });

  it("leaves other markup untouched", () => {
    const html = '<h1>t</h1><a href="other.md">link</a>';
    expect(resolveImages(html, "/home/u/readme.md")).toBe(html);
  });
});
