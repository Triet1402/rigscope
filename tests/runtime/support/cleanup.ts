import { after } from "node:test";

export const liveCleanups = new Set<() => unknown>();

after(async () => {
  for (const cleanup of [...liveCleanups]) {
    try {
      await cleanup();
    } catch {
      // best-effort: never mask the original test failure
    }
  }
  liveCleanups.clear();
});

export function registerCleanup(cleanup: () => unknown): void {
  liveCleanups.add(cleanup);
}

export function unregisterCleanup(cleanup: () => unknown): void {
  liveCleanups.delete(cleanup);
}
