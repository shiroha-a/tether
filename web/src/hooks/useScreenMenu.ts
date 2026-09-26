import { useEffect, useState } from "react";
import { Terminal } from "@xterm/headless";
import { wsUrl } from "../api";
import { parseScreen, type ScreenState } from "../screenMenu";

/**
 * Mirrors a running session's terminal in a headless xterm and reports the
 * selection menu currently on screen (permission prompts etc.) and whether
 * Claude Code is busy.
 *
 * It attaches as a read-only viewer with cols/rows=0, so it never changes the
 * pty size and never sends input. It only connects while the session runs so
 * that opening the chat view does not resume a stopped session.
 */
const IDLE: ScreenState = { menu: null, busy: false };
// 描画が落ち着くのを待つ時間と、描画が続いていても読み直すまでの上限
const SETTLE_MS = 150;
const MAX_WAIT_MS = 600;

export function useScreenMenu(sessionId: string, running: boolean): ScreenState {
  const [screen, setScreen] = useState<ScreenState>(IDLE);

  useEffect(() => {
    setScreen(IDLE);
    if (!running) return;
    const term = new Terminal({ cols: 120, rows: 32, allowProposedApi: true, scrollback: 0 });
    let ws: WebSocket | null = null;
    let retry: number | undefined;
    let scan: number | undefined;
    // 最初の描画を受けてから、まだ画面を読んでいない場合のその時刻
    let pendingSince = 0;
    let closed = false;

    const read = () => {
      pendingSince = 0;
      const buf = term.buffer.active;
      const lines: string[] = [];
      for (let y = 0; y < term.rows; y++) lines.push(buf.getLine(buf.viewportY + y)?.translateToString(true) ?? "");
      const next = parseScreen(lines, term.cols);
      // 内容が同じなら同じオブジェクトを保ち、無駄な再描画を避ける
      setScreen((prev) => (JSON.stringify(prev) === JSON.stringify(next) ? prev : next));
    };

    const rescan = () => {
      // 描画は細かく何度も届くので、落ち着いてから画面を読む。
      // ただし作業中はスピナー等で描画が途切れないため、待ちすぎないよう上限を設ける
      const now = Date.now();
      if (!pendingSince) pendingSince = now;
      window.clearTimeout(scan);
      scan = window.setTimeout(read, Math.max(0, Math.min(SETTLE_MS, pendingSince + MAX_WAIT_MS - now)));
    };

    const connect = () => {
      ws = new WebSocket(wsUrl(`/ws/sessions/${encodeURIComponent(sessionId)}?cols=0&rows=0`));
      ws.binaryType = "arraybuffer";
      ws.onmessage = (ev) => {
        if (typeof ev.data !== "string") {
          term.write(new Uint8Array(ev.data as ArrayBuffer), rescan);
          return;
        }
        const msg = JSON.parse(ev.data);
        if (msg.type === "replay" || msg.type === "started") term.reset();
        if ((msg.type === "state" || msg.type === "size") && msg.cols > 0 && msg.rows > 0)
          term.resize(msg.cols, msg.rows);
        rescan();
      };
      ws.onclose = () => {
        if (!closed) retry = window.setTimeout(connect, 2000);
      };
    };
    connect();
    return () => {
      closed = true;
      window.clearTimeout(retry);
      window.clearTimeout(scan);
      ws?.close();
      term.dispose();
    };
  }, [sessionId, running]);

  return screen;
}
