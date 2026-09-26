/** Color theme preference: follow the device, or force light/dark. */
export type ThemePref = "auto" | "light" | "dark";

const KEY = "tether.theme";
const COLORS = { light: "#faf8f5", dark: "#1c1b19" };

export function loadTheme(): ThemePref {
  try {
    const v = localStorage.getItem(KEY);
    if (v === "light" || v === "dark" || v === "auto") return v;
  } catch {
    // 保存できない環境では自動にする
  }
  return "auto";
}

export function saveTheme(pref: ThemePref) {
  try {
    localStorage.setItem(KEY, pref);
  } catch {
    // 保存できなくても今回の表示には反映される
  }
}

/** The theme actually shown for a preference on this device. */
export function effectiveTheme(pref: ThemePref): "light" | "dark" {
  if (pref !== "auto") return pref;
  return window.matchMedia?.("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

/** Applies the preference to <html> and the browser/PWA chrome color. */
export function applyTheme(pref: ThemePref) {
  document.documentElement.dataset.theme = pref;
  // スマホのステータスバーやアドレスバーの色も画面に合わせる
  document.querySelector('meta[name="theme-color"]')?.setAttribute("content", COLORS[effectiveTheme(pref)]);
}

/** Re-applies the theme when the device setting changes while "auto" is selected. */
export function watchSystemTheme(get: () => ThemePref): () => void {
  const mq = window.matchMedia?.("(prefers-color-scheme: light)");
  if (!mq) return () => {};
  const onChange = () => {
    if (get() === "auto") applyTheme("auto");
  };
  mq.addEventListener("change", onChange);
  return () => mq.removeEventListener("change", onChange);
}
