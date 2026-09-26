// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from "vitest";
import { attachTouchScroll, LineAccumulator, momentumDeltas, TOUCH_SLOP, TouchScrollTracker } from "./touchScroll";

describe("TouchScrollTracker", () => {
  it("ignores small movements so taps still work", () => {
    const t = new TouchScrollTracker();
    t.start(100, 100, 0);
    expect(t.move(100, 100 + TOUCH_SLOP - 1, 10)).toBeNull();
    expect(t.end(20)).toBe(0);
  });

  it("reports deltas once a vertical drag passes the slop (finger up scrolls to newer output)", () => {
    const t = new TouchScrollTracker();
    t.start(100, 300, 0);
    // 閾値を超えた最初の移動で、開始点からの移動量をまとめて返す
    expect(t.move(101, 300 - TOUCH_SLOP, 16)).toBe(TOUCH_SLOP);
    expect(t.move(101, 280, 32)).toBe(300 - TOUCH_SLOP - 280);
    expect(t.move(101, 300, 48)).toBe(-20);
  });

  it("does not scroll on mostly horizontal drags", () => {
    const t = new TouchScrollTracker();
    t.start(100, 100, 0);
    expect(t.move(140, 120, 16)).toBeNull();
    expect(t.move(200, 130, 32)).toBeNull();
  });

  it("measures the release velocity from the last 100ms only", () => {
    const t = new TouchScrollTracker();
    t.start(0, 1000, 0);
    t.move(0, 900, 100); // 古い速い動き（この区間は速度に含めない）
    t.move(0, 890, 200);
    t.move(0, 870, 250);
    t.move(0, 850, 300);
    // 200ms〜300msで40px上へ
    expect(t.end(300)).toBeCloseTo(0.4);
  });

  it("flings only once when the gesture ends twice (touchend and touchcancel)", () => {
    const t = new TouchScrollTracker();
    t.start(0, 500, 0);
    t.move(0, 400, 50);
    expect(t.end(60)).toBeGreaterThan(0);
    expect(t.end(61)).toBe(0);
  });

  it("has no fling when the finger stopped before release", () => {
    const t = new TouchScrollTracker();
    t.start(0, 500, 0);
    t.move(0, 400, 50);
    expect(t.end(500)).toBe(0);
  });
});

describe("momentumDeltas", () => {
  it("slows down and stops", () => {
    const d = momentumDeltas(1);
    expect(d[0]).toBeCloseTo(16);
    expect(d[1]).toBeCloseTo(16 * 0.95);
    expect(d.length).toBeGreaterThan(10);
    expect(Math.abs(d[d.length - 1])).toBeLessThan(1);
    expect(momentumDeltas(-1)[0]).toBeCloseTo(-16);
  });

  it("does nothing for slow releases and is bounded", () => {
    expect(momentumDeltas(0)).toEqual([]);
    expect(momentumDeltas(0.04)).toEqual([]);
    expect(momentumDeltas(1e9).length).toBeLessThanOrEqual(300);
  });
});

describe("LineAccumulator", () => {
  it("scrolls whole lines and keeps the remainder", () => {
    const a = new LineAccumulator();
    expect(a.add(10, 16)).toBe(0);
    expect(a.add(10, 16)).toBe(1); // 20px -> 1行、残り4px
    expect(a.add(28, 16)).toBe(2); // 32px -> 2行
    expect(a.add(-8, 16)).toBe(0);
    expect(a.add(-8, 16)).toBe(-1);
    expect(a.add(-40, 16)).toBe(-2); // 残り-8px
  });

  it("ignores an unknown line height", () => {
    const a = new LineAccumulator();
    expect(a.add(100, 0)).toBe(0);
    expect(a.add(100, NaN)).toBe(0);
    // 無視した分は持ち越さない
    expect(a.add(15, 16)).toBe(0);
  });
});

describe("attachTouchScroll", () => {
  afterEach(() => vi.unstubAllGlobals());

  const touch = (el: HTMLElement, type: string, points: [number, number][], timeStamp: number) => {
    const e = new Event(type, { cancelable: true, bubbles: true });
    Object.defineProperty(e, "touches", { value: points.map(([clientX, clientY]) => ({ clientX, clientY })) });
    Object.defineProperty(e, "timeStamp", { value: timeStamp });
    el.dispatchEvent(e);
    return e;
  };

  it("scrolls on vertical drags, blocks the page scroll, and flings after release", () => {
    const frames: FrameRequestCallback[] = [];
    vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => frames.push(cb));
    vi.stubGlobal("cancelAnimationFrame", () => {});
    const el = document.createElement("div");
    const got: number[] = [];
    const detach = attachTouchScroll(el, (px) => got.push(px));

    touch(el, "touchstart", [[50, 300]], 0);
    const small = touch(el, "touchmove", [[50, 297]], 10);
    expect(small.defaultPrevented).toBe(false);
    const big = touch(el, "touchmove", [[50, 280]], 20);
    expect(big.defaultPrevented).toBe(true);
    touch(el, "touchmove", [[50, 250]], 40);
    expect(got).toEqual([20, 30]);

    touch(el, "touchend", [], 50);
    expect(frames).toHaveLength(1);
    frames.shift()!(0);
    expect(got.length).toBe(3);
    expect(got[2]).toBeGreaterThan(0);

    detach();
    touch(el, "touchstart", [[50, 300]], 100);
    touch(el, "touchmove", [[50, 200]], 110);
    expect(got.length).toBe(3);
  });

  it("leaves taps and two-finger gestures alone", () => {
    vi.stubGlobal("requestAnimationFrame", () => 0);
    const el = document.createElement("div");
    const got: number[] = [];
    attachTouchScroll(el, (px) => got.push(px));
    touch(el, "touchstart", [[50, 300]], 0);
    touch(el, "touchend", [], 50);
    const pinch = touch(
      el,
      "touchmove",
      [
        [50, 200],
        [80, 400],
      ],
      60,
    );
    expect(pinch.defaultPrevented).toBe(false);
    expect(got).toEqual([]);
  });
});
