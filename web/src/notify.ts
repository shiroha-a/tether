import type { Notice } from "./api";

/** Current notification permission, or "unsupported". */
export function notifyPermission(): NotificationPermission | "unsupported" {
  return "Notification" in window ? Notification.permission : "unsupported";
}

export async function requestNotifyPermission(): Promise<NotificationPermission | "unsupported"> {
  if (!("Notification" in window)) return "unsupported";
  return Notification.requestPermission();
}

/** Shows a system notification. Uses the service worker when available because
 * mobile browsers (and installed PWAs) do not support `new Notification()`. */
export async function showSystemNotification(n: Notice) {
  if (notifyPermission() !== "granted") return;
  const title = `${n.label}: ${n.kind === "stop" ? "完了" : n.kind === "attention" ? "要対応" : "予約"}`;
  const opts: NotificationOptions = {
    body: n.message,
    tag: `tether-${n.session}`,
    icon: "/icon-192.png",
    data: { session: n.session },
  };
  try {
    const reg = await navigator.serviceWorker?.getRegistration();
    if (reg) {
      await reg.showNotification(title, opts);
      return;
    }
  } catch {
    // Service Workerが使えない場合は下のフォールバックに任せる
  }
  try {
    new Notification(title, opts);
  } catch {
    // 通知が使えない環境では何もしない（画面内トーストで代替する）
  }
}
