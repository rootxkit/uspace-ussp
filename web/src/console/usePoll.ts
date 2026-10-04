"use client";

// A console read refreshed on a period (a display period, not a
// threshold): the last answer with the browser time it arrived, and the
// last refusal or failure with the time since which the reads fail, so
// a page that cannot read says since when (SC-22: never an empty page
// that looks like an empty sky). It never retries faster than its period
// and stops when the page leaves.
import { useCallback, useEffect, useRef, useState } from "react";

export interface Poll<T> {
  data: T | null;
  /** Browser time of the last answer; null before the first. */
  okAtMs: number | null;
  error: unknown;
  /** Browser time of the first failure since the last answer; null while reads answer. */
  failingSinceMs: number | null;
  /** Reads again now. */
  reload(): void;
}

export function usePoll<T>(read: () => Promise<T>, periodMs: number): Poll<T> {
  const [data, setData] = useState<T | null>(null);
  const [okAtMs, setOkAt] = useState<number | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [failingSinceMs, setFailing] = useState<number | null>(null);
  const [tick, setTick] = useState(0);
  const readRef = useRef(read);
  useEffect(() => {
    readRef.current = read;
  }, [read]);
  useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const run = () => {
      readRef.current().then(
        (d) => {
          if (!live) return;
          setData(d);
          setOkAt(Date.now());
          setError(null);
          setFailing(null);
          timer = setTimeout(run, periodMs);
        },
        (e: unknown) => {
          if (!live) return;
          setError(e);
          setFailing((s) => s ?? Date.now());
          timer = setTimeout(run, periodMs);
        },
      );
    };
    run();
    return () => {
      live = false;
      if (timer !== null) clearTimeout(timer);
    };
  }, [periodMs, tick]);
  const reload = useCallback(() => setTick((n) => n + 1), []);
  return { data, okAtMs, error, failingSinceMs, reload };
}

/** The data of an openapi-fetch answer; a missing body is an error (the client rejects every non-2xx). */
export function body<T>(r: { data?: T }): T {
  if (r.data === undefined) throw new Error("the API answered without a body");
  return r.data;
}
