// xterm.js 6はタッチでのスクロールに対応していない（内部にジェスチャーの処理はあるが、スクロールにつながっていない）。
// 合成したホイールイベントはブラウザによって向きや量の解釈が変わるため、指の移動量を行数に直してxtermのAPIで動かす

/** Distance in px a finger must move before a drag counts as scrolling (so taps still focus the terminal). */
export const TOUCH_SLOP = 8;
const FRICTION = 0.95; // 1フレーム（約16ms）ごとの減速
const MIN_VELOCITY = 0.05; // px/ms。これより遅くなったら慣性を止める
const FRAME_MS = 16;
// 指を離す直前のこの時間内の動きから、慣性の初速を求める
const VELOCITY_WINDOW_MS = 100;

/**
 * Tracks one finger and reports vertical scroll deltas. Positive deltas scroll
 * toward newer output (the finger moves up), like a mouse wheel's deltaY.
 */
export class TouchScrollTracker {
  private startX = 0;
  private startY = 0;
  private lastY = 0;
  private scrolling = false;
  private samples: { y: number; t: number }[] = [];

  start(x: number, y: number, t: number) {
    this.startX = x;
    this.startY = this.lastY = y;
    this.scrolling = false;
    this.samples = [{ y, t }];
  }

  /** Returns the delta to scroll by, or null while the gesture is not (yet) a vertical scroll. */
  move(x: number, y: number, t: number): number | null {
    this.samples.push({ y, t });
    if (this.samples.length > 20) this.samples.shift();
    if (!this.scrolling) {
      const dx = Math.abs(x - this.startX);
      const dy = Math.abs(y - this.startY);
      // 横方向の動きが主なら、スクロールとして扱わない
      if (dy < TOUCH_SLOP || dy < dx) return null;
      this.scrolling = true;
    }
    const delta = this.lastY - y;
    this.lastY = y;
    return delta;
  }

  /** Ends the gesture and returns the release velocity in px/ms (0 when it was not a scroll). */
  end(t: number): number {
    if (!this.scrolling) return 0;
    this.scrolling = false;
    const recent = this.samples.filter((s) => t - s.t <= VELOCITY_WINDOW_MS);
    if (recent.length < 2) return 0;
    const first = recent[0];
    const last = recent[recent.length - 1];
    const dt = last.t - first.t;
    return dt > 0 ? (first.y - last.y) / dt : 0;
  }
}

/** Per-frame scroll deltas for a fling that starts at `velocity` px/ms and slows down. */
export function momentumDeltas(velocity: number): number[] {
  const out: number[] = [];
  let v = velocity;
  while (Math.abs(v) >= MIN_VELOCITY && out.length < 300) {
    out.push(v * FRAME_MS);
    v *= FRICTION;
  }
  return out;
}

/**
 * Converts pixel deltas into whole lines, carrying the remainder so slow drags
 * still scroll. Positive results scroll toward newer output.
 */
export class LineAccumulator {
  private rest = 0;

  add(px: number, lineHeight: number): number {
    if (!(lineHeight > 0)) return 0;
    this.rest += px;
    const lines = Math.trunc(this.rest / lineHeight);
    // 負の端数からは-0が出るので、0にそろえる
    if (lines === 0) return 0;
    this.rest -= lines * lineHeight;
    return lines;
  }
}

/**
 * Calls `onScroll` with pixel deltas for one-finger vertical drags on `host`,
 * followed by a short fling after release. Taps and horizontal drags are left
 * alone. Returns a cleanup function.
 */
export function attachTouchScroll(host: HTMLElement, onScroll: (px: number) => void): () => void {
  const tracker = new TouchScrollTracker();
  let frame = 0;
  const stopMomentum = () => {
    cancelAnimationFrame(frame);
    frame = 0;
  };

  const onStart = (e: TouchEvent) => {
    stopMomentum();
    if (e.touches.length !== 1) return;
    const t = e.touches[0];
    tracker.start(t.clientX, t.clientY, e.timeStamp);
  };
  const onMove = (e: TouchEvent) => {
    if (e.touches.length !== 1) return;
    const t = e.touches[0];
    const delta = tracker.move(t.clientX, t.clientY, e.timeStamp);
    if (delta === null) return;
    // ページ全体のスクロールや引っ張って再読み込みをさせない
    e.preventDefault();
    if (delta !== 0) onScroll(delta);
  };
  const onEnd = (e: TouchEvent) => {
    const deltas = momentumDeltas(tracker.end(e.timeStamp));
    let i = 0;
    const step = () => {
      if (i >= deltas.length) {
        frame = 0;
        return;
      }
      onScroll(deltas[i++]);
      frame = requestAnimationFrame(step);
    };
    if (deltas.length > 0) frame = requestAnimationFrame(step);
  };

  host.addEventListener("touchstart", onStart, { passive: true });
  host.addEventListener("touchmove", onMove, { passive: false });
  host.addEventListener("touchend", onEnd, { passive: true });
  host.addEventListener("touchcancel", onEnd, { passive: true });
  return () => {
    stopMomentum();
    host.removeEventListener("touchstart", onStart);
    host.removeEventListener("touchmove", onMove);
    host.removeEventListener("touchend", onEnd);
    host.removeEventListener("touchcancel", onEnd);
  };
}
