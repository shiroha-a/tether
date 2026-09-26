import { describe, expect, it } from "vitest";
import { allowedOps, opLabel, remaining } from "./remote";

describe("opLabel", () => {
  it("names known operations in Japanese and passes others through", () => {
    expect(opLabel("exec")).toBe("コマンドの実行");
    expect(opLabel("delegate-status")).toBe("依頼の結果の確認");
    expect(opLabel("mystery")).toBe("mystery");
  });
});

describe("allowedOps", () => {
  it("lists allowed operations in a fixed order", () => {
    expect(allowedOps({ status: true, files: true, exec: false, delegate: false })).toEqual(["状態", "ファイル"]);
    expect(allowedOps({ status: false, files: false, exec: true, delegate: true })).toEqual(["コマンド実行", "依頼"]);
    expect(allowedOps({ status: false, files: false, exec: false, delegate: false })).toEqual([]);
    // それぞれの項目が自分の許可だけを見ている
    expect(allowedOps({ status: false, files: true, exec: false, delegate: false })).toEqual(["ファイル"]);
    expect(allowedOps({ status: true, files: false, exec: false, delegate: false })).toEqual(["状態"]);
  });
});

describe("remaining", () => {
  const now = Date.parse("2026-09-27T10:00:00Z");
  it("formats minutes and zero-padded seconds, rounding up", () => {
    expect(remaining("2026-09-27T10:05:00Z", now)).toBe("5:00");
    expect(remaining("2026-09-27T10:00:09.2Z", now)).toBe("0:10");
    expect(remaining("2026-09-27T10:01:05Z", now)).toBe("1:05");
  });
  it("never goes negative and tolerates bad input", () => {
    expect(remaining("2026-09-27T09:59:00Z", now)).toBe("0:00");
    expect(remaining("not a date", now)).toBe("0:00");
  });
});
