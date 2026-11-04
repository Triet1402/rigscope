"use client";

/**
 * Small browser-persisted UI state, shared through an in-memory cache so a
 * value survives client-side navigation instantly and every subscriber of the
 * same key stays in sync. `local` survives reloads; `session` lasts for the
 * tab. The server snapshot is always the fallback so hydration never
 * mismatches; React swaps in the stored value right after hydrating.
 *
 * Writes go to memory immediately (cheap enough for drag-resizing) and are
 * flushed to Web Storage on a short debounce.
 */

import { useCallback, useSyncExternalStore } from "react";

export type StorageKind = "local" | "session";

const PREFIX = "rigscope:";
const FLUSH_MS = 250;

const memory = new Map<string, unknown>();
const listeners = new Map<string, Set<() => void>>();
const pendingFlush = new Map<string, { timer: ReturnType<typeof setTimeout>; flush: () => void }>();
let lifecycleListening = false;

function storageFor(kind: StorageKind): Storage | null {
  try {
    if (typeof window === "undefined") return null;
    return kind === "local" ? window.localStorage : window.sessionStorage;
  } catch {
    return null;
  }
}

function cacheKey(kind: StorageKind, key: string): string {
  return `${kind}:${key}`;
}

function load<T>(kind: StorageKind, key: string, fallback: T, parse: (raw: unknown) => T | null): T {
  const id = cacheKey(kind, key);
  if (memory.has(id)) return memory.get(id) as T;
  let value = fallback;
  const storage = storageFor(kind);
  if (storage !== null) {
    try {
      const raw = storage.getItem(PREFIX + key);
      if (raw !== null) {
        const parsed = parse(JSON.parse(raw));
        if (parsed !== null) value = parsed;
      }
    } catch {
      // Corrupt or unavailable storage: keep the fallback.
    }
  }
  memory.set(id, value);
  return value;
}

function scheduleFlush(kind: StorageKind, key: string): void {
  const id = cacheKey(kind, key);
  const existing = pendingFlush.get(id);
  if (existing !== undefined) clearTimeout(existing.timer);
  const flush = () => {
    const pending = pendingFlush.get(id);
    if (pending) clearTimeout(pending.timer);
    pendingFlush.delete(id);
    const storage = storageFor(kind);
    if (storage === null) return;
    try {
      storage.setItem(PREFIX + key, JSON.stringify(memory.get(id)));
    } catch {
      // Quota or privacy mode: the in-memory value still works this session.
    }
  };
  pendingFlush.set(id, { timer: setTimeout(flush, FLUSH_MS), flush });
  if (!lifecycleListening && typeof window !== "undefined") {
    lifecycleListening = true;
    const flushAll = () => pendingFlush.forEach((pending) => pending.flush());
    window.addEventListener("pagehide", flushAll);
    document.addEventListener("visibilitychange", () => {
      if (document.hidden) flushAll();
    });
  }
}

function subscribe(id: string, listener: () => void): () => void {
  let set = listeners.get(id);
  if (set === undefined) {
    set = new Set();
    listeners.set(id, set);
  }
  set.add(listener);
  return () => {
    set.delete(listener);
  };
}

/**
 * `fallback` and `parse` should be module-level constants (stable identity);
 * `parse` returns null for anything that is not a valid stored value.
 */
export function usePersistentState<T>(
  key: string,
  fallback: T,
  parse: (raw: unknown) => T | null,
  kind: StorageKind = "local",
): [T, (next: T | ((previous: T) => T)) => void] {
  const id = cacheKey(kind, key);
  const value = useSyncExternalStore(
    useCallback((listener: () => void) => subscribe(id, listener), [id]),
    () => load(kind, key, fallback, parse),
    () => fallback,
  );
  const setValue = useCallback(
    (next: T | ((previous: T) => T)) => {
      const previous = load(kind, key, fallback, parse);
      const resolved = typeof next === "function" ? (next as (previous: T) => T)(previous) : next;
      if (Object.is(resolved, previous)) return;
      memory.set(id, resolved);
      scheduleFlush(kind, key);
      listeners.get(id)?.forEach((listener) => listener());
    },
    [fallback, id, key, kind, parse],
  );
  return [value, setValue];
}
