import type { SessionStatus } from "./api";

/**
 * Sessions whose "choice needed" notification has been dealt with: the server
 * state moved on from waiting after the notification arrived (the choice was
 * made in some client, or the turn ended). `pending` maps session ids to the
 * time of their choice notification in milliseconds.
 */
export function answeredChoices(
  sessions: Pick<SessionStatus, "id" | "activity" | "activityAt">[],
  pending: ReadonlyMap<string, number>,
): string[] {
  const byId = new Map(sessions.map((s) => [s.id, s]));
  return [...pending]
    .filter(([id, at]) => {
      const s = byId.get(id);
      // 削除されたセッションは一覧にないので、ここでは扱わない
      if (!s || s.activity === "waiting") return false;
      // 通知より前の古い状態（取得が通知の到着と前後した場合）では解除しない
      return Date.parse(s.activityAt ?? "") >= at;
    })
    .map(([id]) => id);
}
