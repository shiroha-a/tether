import type { SessionKind } from "./api";

/** Launch preferences shared by the folder browser and the home quick-launch. */
export interface LaunchPrefs {
  kind: SessionKind;
  model: string;
  permissionMode: string;
  hidden: boolean;
}

const PREFS_KEY = "tether.launch";
const RECENT_KEY = "tether.recentDirs";
const RECENT_MAX = 8;

export function loadPrefs(): LaunchPrefs {
  const defaults: LaunchPrefs = { kind: "claude", model: "", permissionMode: "default", hidden: false };
  try {
    return { ...defaults, ...JSON.parse(localStorage.getItem(PREFS_KEY) ?? "{}") };
  } catch {
    return defaults;
  }
}

export function savePrefs(p: LaunchPrefs) {
  try {
    localStorage.setItem(PREFS_KEY, JSON.stringify(p));
  } catch {
    // 保存できなくても動作に支障はない
  }
}

/** Folders launched from this browser, newest first. */
export function loadRecentDirs(): string[] {
  try {
    const v = JSON.parse(localStorage.getItem(RECENT_KEY) ?? "[]");
    return Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
  } catch {
    return [];
  }
}

export function rememberDir(dir: string) {
  try {
    const next = [dir, ...loadRecentDirs().filter((d) => d !== dir)].slice(0, RECENT_MAX);
    localStorage.setItem(RECENT_KEY, JSON.stringify(next));
  } catch {
    // 保存できなくても動作に支障はない
  }
}

const BROWSE_KEY = "tether.browsePath";

/** The folder the browser showed last on this device ("" when none). */
export function loadBrowsePath(): string {
  try {
    return localStorage.getItem(BROWSE_KEY) ?? "";
  } catch {
    return "";
  }
}

export function saveBrowsePath(path: string) {
  try {
    localStorage.setItem(BROWSE_KEY, path);
  } catch {
    // 保存できなくても、次回ルートから開くだけ
  }
}

/**
 * Chooses the folder to open: an explicitly requested one (e.g. a session's
 * "Files" button) wins, then the remembered one if it is inside root, then root.
 */
export function initialBrowsePath(requested: string | undefined, remembered: string, root: string): string {
  if (requested) return requested;
  // ルートそのものを記憶していた場合は、この判定に当たらなくても結果はルートになる
  const inRoot = remembered.startsWith(root.replace(/\/$/, "") + "/");
  return remembered && inRoot ? remembered : root;
}
