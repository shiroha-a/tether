// @vitest-environment happy-dom
import { beforeEach, describe, expect, it } from "vitest";
import { initialBrowsePath, loadBrowsePath, saveBrowsePath } from "./prefs";

describe("browse path memory", () => {
  beforeEach(() => localStorage.clear());

  it("saves and loads the last folder", () => {
    expect(loadBrowsePath()).toBe("");
    saveBrowsePath("/home/u/dev/app");
    expect(loadBrowsePath()).toBe("/home/u/dev/app");
  });

  it("prefers an explicitly requested folder over the remembered one", () => {
    expect(initialBrowsePath("/home/u/work", "/home/u/dev", "/home/u")).toBe("/home/u/work");
  });

  it("uses the remembered folder when nothing is requested", () => {
    expect(initialBrowsePath(undefined, "/home/u/dev", "/home/u")).toBe("/home/u/dev");
    expect(initialBrowsePath("", "/home/u", "/home/u")).toBe("/home/u");
  });

  it("falls back to root when nothing is remembered or it is outside root", () => {
    expect(initialBrowsePath(undefined, "", "/home/u")).toBe("/home/u");
    // 設定でrootを変えた後に、古い場所を記憶していた場合
    expect(initialBrowsePath(undefined, "/srv/other", "/home/u")).toBe("/home/u");
    // 名前の前方一致だけで配下とみなさない
    expect(initialBrowsePath(undefined, "/home/user2/x", "/home/u")).toBe("/home/u");
  });

  it("handles a root of / ", () => {
    expect(initialBrowsePath(undefined, "/etc", "/")).toBe("/etc");
  });
});
