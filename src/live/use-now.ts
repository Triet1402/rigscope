"use client";

import { useEffect, useState } from "react";

/**
 * Re-renders on a fixed interval so relative ages ("32s ago") stay honest.
 * Ticks pause with the interval of 0 (never pass 0). Uses one timer per
 * consumer; consumers are the few views that display ages.
 */
export function useNow(intervalMs = 1_000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs]);
  return now;
}
