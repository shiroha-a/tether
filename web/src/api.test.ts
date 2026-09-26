import { describe, expect, it } from "vitest";
import { formatDuration } from "./api";

describe("formatDuration", () => {
  it("uses the two largest units", () => {
    expect(formatDuration(3 * 86400 + 4 * 3600 + 59 * 60)).toBe("3日4時間");
    expect(formatDuration(5 * 3600 + 12 * 60 + 30)).toBe("5時間12分");
    expect(formatDuration(12 * 60 + 59)).toBe("12分");
  });

  it("handles boundaries and invalid input", () => {
    expect(formatDuration(86400)).toBe("1日0時間");
    expect(formatDuration(3600)).toBe("1時間0分");
    expect(formatDuration(59)).toBe("0分");
    // 端末の時計がサーバより遅れていると負になる
    expect(formatDuration(-30)).toBe("0分");
    expect(formatDuration(NaN)).toBe("0分");
  });
});
