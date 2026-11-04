import assert from "node:assert/strict";
import test from "node:test";
import { ProcessDetailCache, type CachedProcessDetail } from "../src/processes/detail-cache";

function snapshot(subscriptionId: string, lastReceivedMs = 1_000): CachedProcessDetail {
  return {
    detail: {
      type: "detail.result",
      subscriptionId,
      capturedAt: new Date(lastReceivedMs).toISOString(),
      snapshot: { processes: [], totalProcesses: 0, inaccessible: 0, truncated: false },
    },
    intervalMs: 2_000,
    leaseMs: 15_000,
    lastReceivedMs,
  };
}

test("returning to a machine restores only that machine's last snapshot", () => {
  const cache = new ProcessDetailCache();
  const first = snapshot("first");
  const second = snapshot("second");
  cache.set("a", "session", first);
  cache.set("b", "session", second);
  assert.equal(cache.get("a", "session", 2_000), first);
  assert.equal(cache.get("b", "session", 2_000), second);
});

test("a new login or session loss discards cached process data", () => {
  const cache = new ProcessDetailCache();
  cache.set("a", "old-session", snapshot("old"));
  assert.equal(cache.get("a", "new-session", 2_000), undefined);
  assert.equal(cache.get("a", "old-session", 2_000), undefined);
  cache.set("a", "new-session", snapshot("new"));
  assert.equal(cache.get("a", null, 2_000), undefined);
  assert.equal(cache.get("a", "new-session", 2_000), undefined);
});

test("expired snapshots cannot reappear after later navigation", () => {
  const cache = new ProcessDetailCache(1_000);
  cache.set("a", "session", snapshot("old"));
  assert.ok(cache.get("a", "session", 1_999));
  assert.equal(cache.get("a", "session", 2_000), undefined);
  assert.equal(cache.get("a", "session", 1_999), undefined);
});

test("cache retention is bounded and access denial removes a machine's snapshot", () => {
  const cache = new ProcessDetailCache(10_000, 2);
  cache.set("a", "session", snapshot("a"));
  cache.set("b", "session", snapshot("b"));
  cache.set("a", "session", snapshot("a-refreshed"));
  cache.set("c", "session", snapshot("c"));
  assert.ok(cache.get("a", "session", 2_000));
  assert.equal(cache.get("b", "session", 2_000), undefined);
  cache.delete("a", "session");
  assert.equal(cache.get("a", "session", 2_000), undefined);
  assert.ok(cache.get("c", "session", 2_000));
});
