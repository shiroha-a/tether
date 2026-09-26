import type { PeerPolicy } from "./api";

/** Japanese names of the operations another machine can be asked to do. */
const OPS: Record<string, string> = {
  status: "サーバの状態の取得",
  list: "フォルダの一覧",
  read: "ファイルの読み取り",
  exec: "コマンドの実行",
  delegate: "作業の依頼",
  "delegate-status": "依頼の結果の確認",
};

export function opLabel(op: string): string {
  return OPS[op] ?? op;
}

/** Labels of the operations a policy allows, in a fixed order. */
export function allowedOps(p: PeerPolicy): string[] {
  const out: string[] = [];
  if (p.status) out.push("状態");
  if (p.files) out.push("ファイル");
  if (p.exec) out.push("コマンド実行");
  if (p.delegate) out.push("依頼");
  return out;
}

/** Remaining time such as "4:59" until `iso`, or "0:00" when it has passed. */
export function remaining(iso: string, now: number): string {
  const sec = Math.max(0, Math.ceil((Date.parse(iso) - now) / 1000));
  if (!Number.isFinite(sec)) return "0:00";
  return `${Math.floor(sec / 60)}:${String(sec % 60).padStart(2, "0")}`;
}
