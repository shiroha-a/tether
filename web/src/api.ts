/** Shapes returned by the tether backend. */
export type SessionKind = "claude" | "shell";

export type Activity = "working" | "waiting" | "idle";

export interface SessionStatus {
  id: string;
  kind?: SessionKind;
  activity?: Activity;
  activityAt?: string;
  /** What the session is waiting for, e.g. "Claude needs your permission to use Write". */
  activityDetail?: string;
  claudeSessionId: string;
  cwd: string;
  model?: string;
  permissionMode?: string;
  label?: string;
  createdAt: string;
  lastActiveAt: string;
  running: boolean;
  clients: number;
  exitCode: number;
}

export interface FsEntry {
  name: string;
  path: string;
  isDir: boolean;
  isLink: boolean;
  size: number;
  modTime: string;
}

export interface FsListing {
  path: string;
  parent: string;
  root: string;
  entries: FsEntry[];
}

export interface Preview {
  path: string;
  name: string;
  size: number;
  kind: "markdown" | "text";
  content: string;
  truncated: boolean;
}

export interface UsageWindow {
  utilization: number;
  resets_at: string | null;
}

export interface Usage {
  available: boolean;
  error?: string;
  plan?: string;
  windows?: Record<string, UsageWindow>;
  fetchedAt: string;
}

export interface ScheduleItem {
  id: string;
  sessionId: string;
  prompt: string;
  runAt: string;
  status: "pending" | "running" | "done" | "failed";
  error?: string;
}

export interface ServerConfig {
  root: string;
  permissionModes: string[];
  models: string[];
}

export interface Notice {
  type: "notify";
  kind: "stop" | "attention" | "schedule";
  session: string;
  label: string;
  cwd: string;
  message: string;
  at: string;
}

export interface LaunchOptions {
  kind: SessionKind;
  cwd: string;
  model?: string;
  permissionMode?: string;
  label?: string;
}

/** Whether the session is a plain login shell rather than Claude Code. */
export function isShell(s: Pick<SessionStatus, "kind">): boolean {
  return s.kind === "shell";
}

export type ChatKind = "user" | "assistant" | "thinking" | "tool_use" | "tool_result" | "command" | "note";

/** One normalized transcript element (see internal/transcript). */
export interface ChatItem {
  id: string;
  kind: ChatKind;
  text?: string;
  toolId?: string;
  toolName?: string;
  summary?: string;
  isError?: boolean;
  truncated?: boolean;
  at: string;
}

export interface TranscriptChunk {
  items: ChatItem[];
  offset: number;
  claudeSessionId: string;
  truncated?: boolean;
}

/** Keys the chat view may send to a session (mirrors the server allowlist). */
export type InputKey =
  "up" | "down" | "enter" | "esc" | "tab" | "shift-tab" | "1" | "2" | "3" | "4" | "5" | "6" | "7" | "8" | "9";

export interface SystemInfo {
  hostname: string;
  cpus: number;
  load: [number, number, number];
  memTotal: number;
  memAvailable: number;
  diskPath: string;
  diskTotal: number;
  diskFree: number;
  uptimeSec: number;
}

/** Build and project information returned by GET /api/about. */
export interface AboutInfo {
  version: string;
  goVersion: string;
  platform: string;
  startedAt: string;
  repository: string;
  license: string;
}

const TOKEN_KEY = "tether.token";

/** Returns the saved API token ("" when none). */
export function getToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

export function setToken(token: string) {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    // ストレージが使えない環境（プライベートモード等）ではメモリ上のみで動かす
  }
}

/** Thrown for non-2xx responses. */
export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

let onUnauthorized: () => void = () => {};
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  // CSRF対策としてサーバは状態変更リクエストにこのヘッダを要求する（他サイトからは付けられない）
  const headers: Record<string, string> = { "X-Tether": "1" };
  const token = getToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  let payload: BodyInit | undefined;
  if (body instanceof FormData) {
    payload = body;
  } else if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body);
  }
  const res = await fetch(path, { method, headers, body: payload });
  if (res.status === 401) {
    onUnauthorized();
    throw new ApiError(401, "認証が必要です");
  }
  if (!res.ok) {
    throw new ApiError(res.status, (await res.text()).trim() || res.statusText);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

/** Appends the token as a query parameter for URLs used by <a>, <img> and WebSocket. */
export function withToken(url: string): string {
  const token = getToken();
  if (!token) return url;
  return url + (url.includes("?") ? "&" : "?") + "token=" + encodeURIComponent(token);
}

export function wsUrl(path: string): string {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return withToken(`${proto}//${location.host}${path}`);
}

const q = encodeURIComponent;

export const api = {
  health: () => request<{ ok: boolean; authRequired: boolean }>("GET", "/api/health"),
  config: () => request<ServerConfig>("GET", "/api/config"),
  sessions: () => request<SessionStatus[]>("GET", "/api/sessions"),
  createSession: (body: LaunchOptions) => request<SessionStatus>("POST", "/api/sessions", body),
  renameSession: (id: string, label: string) => request<void>("PATCH", `/api/sessions/${q(id)}`, { label }),
  stopSession: (id: string) => request<void>("POST", `/api/sessions/${q(id)}/stop`),
  deleteSession: (id: string) => request<void>("DELETE", `/api/sessions/${q(id)}`),
  list: (path: string, hidden: boolean) =>
    request<FsListing>("GET", `/api/fs/list?path=${q(path)}${hidden ? "&hidden=1" : ""}`),
  mkdir: (path: string) => request<{ path: string }>("POST", "/api/fs/mkdir", { path }),
  upload: (dir: string, files: FileList | File[], overwrite = false) => {
    const fd = new FormData();
    for (const f of Array.from(files)) fd.append("file", f, f.name);
    return request<{ saved: string[] }>("POST", `/api/fs/upload?dir=${q(dir)}${overwrite ? "&overwrite=1" : ""}`, fd);
  },
  statPaths: (base: string, paths: string[]) =>
    request<import("./paths").KnownPath[]>("POST", "/api/fs/stat", { base, paths }),
  preview: (path: string) => request<Preview>("GET", `/api/fs/preview?path=${q(path)}`),
  downloadUrl: (path: string, inline = false) =>
    withToken(`/api/fs/download?path=${q(path)}${inline ? "&inline=1" : ""}`),
  transcript: (id: string, offset: number) =>
    request<TranscriptChunk>("GET", `/api/sessions/${q(id)}/transcript?offset=${offset}`),
  sendText: (id: string, text: string) => request<void>("POST", `/api/sessions/${q(id)}/input`, { text }),
  sendKey: (id: string, key: InputKey) => request<void>("POST", `/api/sessions/${q(id)}/input`, { key }),
  sendKeys: (id: string, keys: InputKey[]) => request<void>("POST", `/api/sessions/${q(id)}/input`, { keys }),
  notifications: () => request<Notice[]>("GET", "/api/notifications"),
  system: () => request<SystemInfo>("GET", "/api/system"),
  about: () => request<AboutInfo>("GET", "/api/about"),
  usage: (force = false) => request<Usage>("GET", `/api/usage${force ? "?force=1" : ""}`),
  schedules: (session: string) => request<ScheduleItem[]>("GET", `/api/schedules?session=${q(session)}`),
  addSchedule: (sessionId: string, prompt: string, runAt: Date) =>
    request<ScheduleItem>("POST", "/api/schedules", { sessionId, prompt, runAt: runAt.toISOString() }),
  deleteSchedule: (id: string) => request<void>("DELETE", `/api/schedules/${q(id)}`),
};

/** Human-readable label for a session. */
export function sessionLabel(s: Pick<SessionStatus, "label" | "cwd">): string {
  return s.label || s.cwd.split("/").filter(Boolean).pop() || s.cwd;
}

/** Shortens /home/<user> to ~ for display. */
export function shortPath(p: string): string {
  return p.replace(/^\/home\/[^/]+/, "~");
}

/** Formats a byte count with binary units. */
export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

/** Japanese duration such as "3日4時間", "5時間12分" or "12分". */
export function formatDuration(sec: number): string {
  const s = Number.isFinite(sec) ? Math.max(0, Math.floor(sec)) : 0;
  const days = Math.floor(s / 86400);
  const hours = Math.floor((s % 86400) / 3600);
  const minutes = Math.floor((s % 3600) / 60);
  if (days > 0) return `${days}日${hours}時間`;
  if (hours > 0) return `${hours}時間${minutes}分`;
  return `${minutes}分`;
}

/** Japanese relative time such as "3分前". */
export function timeAgo(iso: string | undefined): string {
  if (!iso) return "";
  const sec = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (sec < 60) return "たった今";
  if (sec < 3600) return `${Math.floor(sec / 60)}分前`;
  if (sec < 86400) return `${Math.floor(sec / 3600)}時間前`;
  return `${Math.floor(sec / 86400)}日前`;
}
