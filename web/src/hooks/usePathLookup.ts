import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../api";
import type { KnownPath } from "../paths";

// 存在しなかった候補を覚えておく時間。ファイルが後から作られる（許可確認の画面にパスが先に出る等）ことがあるので短めにする
const MISS_TTL_MS = 10_000;
// Claudeはファイルを書く前にWriteツールの入力としてパスを出すので、見つからなかった候補は少し待って確かめ直す
const RETRY_DELAYS_MS = [2_000, 4_000, 6_000];
const BATCH = 100;

type Asked = { at: number; tries: number };

/**
 * Confirms path candidates with the server (relative paths are resolved
 * against `base`) and remembers which ones exist. `request` is cheap to call
 * repeatedly: known paths and recent misses are not asked again, and misses
 * are re-checked a few times because files often appear shortly after their
 * path is first mentioned.
 */
export function usePathLookup(base: string) {
  const [known, setKnown] = useState<Map<string, KnownPath>>(() => new Map());
  const knownRef = useRef(known);
  knownRef.current = known;
  const asked = useRef(new Map<string, Asked>());
  const timers = useRef<number[]>([]);

  useEffect(() => {
    setKnown(new Map());
    asked.current = new Map();
    return () => {
      timers.current.forEach((t) => window.clearTimeout(t));
      timers.current = [];
    };
  }, [base]);

  const query = useCallback(
    (batch: string[], retry: boolean) => {
      const now = Date.now();
      for (const c of batch) {
        const prev = asked.current.get(c);
        asked.current.set(c, { at: now, tries: retry && prev ? prev.tries + 1 : 0 });
      }
      api
        .statPaths(base, batch)
        .then((res) => {
          if (res.length > 0) {
            setKnown((prev) => {
              const next = new Map(prev);
              for (const r of res) next.set(r.input, r);
              return next;
            });
          }
          const found = new Set(res.map((r) => r.input));
          const missing = batch.filter((c) => !found.has(c));
          const tries = missing.length ? (asked.current.get(missing[0])?.tries ?? 0) : RETRY_DELAYS_MS.length;
          if (missing.length && tries < RETRY_DELAYS_MS.length) {
            timers.current.push(window.setTimeout(() => query(missing, true), RETRY_DELAYS_MS[tries]));
          }
        })
        .catch(() => batch.forEach((c) => asked.current.delete(c)));
    },
    [base],
  );

  const request = useCallback(
    (candidates: string[]) => {
      const now = Date.now();
      const fresh = [...new Set(candidates)].filter((c) => {
        if (knownRef.current.has(c)) return false;
        const a = asked.current.get(c);
        return a === undefined || now - a.at > MISS_TTL_MS;
      });
      for (let i = 0; i < fresh.length; i += BATCH) query(fresh.slice(i, i + BATCH), false);
    },
    [query],
  );

  return { known, knownRef, request };
}
