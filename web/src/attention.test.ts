import { describe, expect, it } from "vitest";
import type { SessionStatus } from "./api";
import { answeredChoices } from "./attention";

type Row = Pick<SessionStatus, "id" | "activity" | "activityAt">;

const at = (iso: string) => Date.parse(iso);

describe("answeredChoices", () => {
  const pending = new Map([
    ["a", at("2026-09-27T10:00:05+09:00")],
    ["b", at("2026-09-27T10:00:05+09:00")],
  ]);

  it("clears sessions that moved on from waiting after the notification", () => {
    const sessions: Row[] = [
      { id: "a", activity: "working", activityAt: "2026-09-27T10:00:08+09:00" },
      { id: "b", activity: "idle", activityAt: "2026-09-27T01:00:09Z" },
      { id: "c", activity: "working", activityAt: "2026-09-27T10:00:09+09:00" },
    ];
    expect(answeredChoices(sessions, pending)).toEqual(["a", "b"]);
  });

  it("keeps sessions that are still waiting or only have an older state", () => {
    const sessions: Row[] = [
      { id: "a", activity: "waiting", activityAt: "2026-09-27T10:00:09+09:00" },
      // 通知より前の状態（まだ待ちになる前の取得結果）
      { id: "b", activity: "working", activityAt: "2026-09-27T10:00:01+09:00" },
    ];
    expect(answeredChoices(sessions, pending)).toEqual([]);
  });

  it("treats the same instant as moved on, and ignores a missing time", () => {
    expect(
      answeredChoices([{ id: "a", activity: "working", activityAt: "2026-09-27T10:00:05+09:00" }], pending),
    ).toEqual(["a"]);
    expect(answeredChoices([{ id: "a", activity: "working" }], pending)).toEqual([]);
    // 通知を受けたセッションが一覧にない（削除された）場合
    expect(answeredChoices([], pending)).toEqual([]);
  });
});
