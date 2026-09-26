import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import "./styles.css";
import { applyTheme, loadTheme } from "./theme";

// 旧名（ccdeck）で保存していたトークンや設定を、再ログインなしで新しいキーへ引き継ぐ
try {
  for (const key of Object.keys(localStorage)) {
    if (!key.startsWith("ccdeck.")) continue;
    const next = "tether." + key.slice("ccdeck.".length);
    const value = localStorage.getItem(key);
    if (value !== null && localStorage.getItem(next) === null) localStorage.setItem(next, value);
    localStorage.removeItem(key);
  }
} catch {
  // ストレージが使えない環境では引き継ぎを省略する（再ログインで復旧できる）
}

// 描画前にテーマを当てて、読み込み時に一瞬暗い画面が出ないようにする
applyTheme(loadTheme());

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);

if ("serviceWorker" in navigator && import.meta.env.PROD) {
  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/sw.js").catch(() => {
      // Service Workerは任意機能なので、登録に失敗してもアプリは動かす
    });
  });
}
