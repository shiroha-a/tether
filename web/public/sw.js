// Service worker: caches the app shell only. API and WebSocket traffic always
// goes to the network so terminal and session state are never stale.
const CACHE = "tether-shell-v2";
const SHELL = ["/", "/manifest.webmanifest", "/icon.svg", "/icon-192.png"];

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)));
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);
  if (event.request.method !== "GET" || url.origin !== self.location.origin) return;
  if (url.pathname.startsWith("/api/") || url.pathname.startsWith("/ws/")) return;
  // Network first so deploys are picked up; fall back to cache when offline.
  event.respondWith(
    fetch(event.request)
      .then((res) => {
        if (res.ok && (url.pathname.startsWith("/assets/") || SHELL.includes(url.pathname))) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(event.request, copy));
        }
        return res;
      })
      .catch(() =>
        caches
          .match(event.request)
          .then((hit) => hit || (event.request.mode === "navigate" ? caches.match("/") : undefined)),
      ),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const session = event.notification.data && event.notification.data.session;
  const target = session ? "/?session=" + encodeURIComponent(session) : "/";
  event.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((wins) => {
      for (const w of wins) {
        if ("focus" in w) {
          w.postMessage({ type: "open-session", session });
          return w.focus();
        }
      }
      return self.clients.openWindow(target);
    }),
  );
});
