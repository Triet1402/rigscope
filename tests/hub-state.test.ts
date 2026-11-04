/**
 * Unit tests for hub state behavior (src/hub/state.ts) and hub configuration
 * parsing (src/hub/config.ts). Run via the Node test runner through tsx.
 */

import { strict as assert } from "node:assert";
import { describe, it } from "node:test";
import { type MetricsRecord, parseHubSnapshot } from "../src/contracts/index";
import { HubConfigError, MAX_HUB_COLLECTORS_BYTES, parseHubConfig } from "../src/hub/config";
import { HubState, HubStateError } from "../src/hub/state";
import { LiveFeed } from "../src/hub/sse";

const CREDENTIALS = [
  { collectorId: "collector-a", machineId: "machine-a", token: "token-a" },
  { collectorId: "collector-b", machineId: "machine-b", token: "token-b" },
  { collectorId: "collector-c", machineId: "machine-c", token: "token-c" },
];

const tick = (): (() => Date) => {
  let clock = 0;
  return () => new Date((clock += 1000));
};

function makeRecord(collectorId: string, sequence: number, overrides: Partial<MetricsRecord> = {}): MetricsRecord {
  return {
    collectorId,
    runId: "run-1",
    bootId: "boot-1",
    sequence,
    capturedAt: new Date(1700000000000 + sequence * 1000).toISOString(),
    kind: "metrics",
    payload: {
      cpu: { status: "ok", usagePercent: sequence % 100, perCore: [sequence % 100] },
      memory: { status: "ok", usedBytes: sequence, totalBytes: 1000 },
      swap: { status: "ok", usedBytes: 0, totalBytes: 0 },
      gpus: { status: "unavailable", reason: "no GPUs detected" },
      disks: { status: "ok", filesystems: [], devices: [] },
      network: { status: "ok", interfaces: [] },
    },
    ...overrides,
  };
}

function helloSession(
  state: HubState,
  overrides: Partial<{
    sessionId: string;
    collectorId: string;
    runId: string;
    bootId: string;
    hostname: string;
    expectedIntervalMs: number | null;
  }> = {},
): string {
  const sessionId = overrides.sessionId ?? `session-${state.sessionCount() + 1}-${Math.random()}`;
  state.startSession({
    sessionId,
    collectorId: overrides.collectorId ?? "collector-a",
    runId: overrides.runId ?? "run-1",
    bootId: overrides.bootId ?? "boot-1",
    hostname: overrides.hostname ?? "mock-machine-a",
    expectedIntervalMs: overrides.expectedIntervalMs ?? null,
  });
  return sessionId;
}

describe("hub configuration", () => {
  it("accepts deployment connection budgets above one hundred and rejects invalid budgets", () => {
    assert.equal(parseHubConfig({HUB_MAX_COLLECTOR_CONNECTIONS: "500"}).maxCollectorConnections, 500);
    for (const value of ["", "0", "-1", "1.5", "Infinity", "9007199254740992"])
      assert.throws(() => parseHubConfig({HUB_MAX_COLLECTOR_CONNECTIONS: value}), HubConfigError);
  });
  it("defaults to loopback 127.0.0.1:8081 without credentials", () => {
    const config = parseHubConfig({});
    assert.equal(config.host, "127.0.0.1");
    assert.equal(config.port, 8081);
    assert.deepEqual(config.collectors, []);
    assert.equal(config.development, false);
  });

  it("accepts literal loopback hosts and port 0 for ephemeral tests", () => {
    assert.equal(parseHubConfig({ HUB_HOST: "::1", HUB_PORT: "0" }).port, 0);
    assert.equal(parseHubConfig({ HUB_HOST: "127.1.2.3" }).host, "127.1.2.3");
  });

  it("rejects non-loopback hosts even in development", () => {
    assert.throws(() => parseHubConfig({ HUB_HOST: "0.0.0.0" }), HubConfigError);
    assert.throws(() => parseHubConfig({ HUB_HOST: "localhost" }), HubConfigError);
    assert.throws(() => parseHubConfig({ HUB_HOST: "192.168.1.5", NODE_ENV: "development" }), HubConfigError);
    assert.throws(() => parseHubConfig({ HUB_HOST: "::" }), HubConfigError);
  });

  it("rejects bad ports", () => {
    assert.throws(() => parseHubConfig({ HUB_PORT: "-1" }), HubConfigError);
    assert.throws(() => parseHubConfig({ HUB_PORT: "8081.5" }), HubConfigError);
    assert.throws(() => parseHubConfig({ HUB_PORT: "65536" }), HubConfigError);
    assert.throws(() => parseHubConfig({ HUB_PORT: "" + (65535 + 1) }), HubConfigError);
  });

  it("parses valid collector credentials", () => {
    const config = parseHubConfig({
      HUB_COLLECTORS: JSON.stringify(CREDENTIALS),
      NODE_ENV: "development",
    });
    assert.deepEqual(config.collectors, CREDENTIALS);
    assert.equal(config.development, true);
  });

  it("treats a missing credential list as empty (but well-formed)", () => {
    assert.deepEqual(parseHubConfig({}).collectors, []);
    assert.deepEqual(parseHubConfig({ HUB_COLLECTORS: "" }).collectors, []);
  });

  it("rejects malformed or duplicate credentials", () => {
    assert.throws(() => parseHubConfig({ HUB_COLLECTORS: "[" }), HubConfigError);
    assert.throws(() => parseHubConfig({ HUB_COLLECTORS: "{}" }), HubConfigError);
    assert.throws(
      () => parseHubConfig({ HUB_COLLECTORS: JSON.stringify([{ collectorId: "c", machineId: "m" }]) }),
      "exactly",
    );
    assert.throws(
      () =>
        parseHubConfig({
          HUB_COLLECTORS: JSON.stringify([
            { collectorId: "c", machineId: "m", token: "t" },
            { collectorId: "c", machineId: "m2", token: "t2" },
          ]),
        }),
      "duplicate collectorId",
    );
    assert.throws(
      () =>
        parseHubConfig({
          HUB_COLLECTORS: JSON.stringify([
            { collectorId: "c1", machineId: "m", token: "t" },
            { collectorId: "c2", machineId: "m", token: "t2" },
          ]),
        }),
      "duplicate machineId",
    );
    assert.throws(
      () =>
        parseHubConfig({
          HUB_COLLECTORS: JSON.stringify([
            { collectorId: "c1", machineId: "m1", token: "t" },
            { collectorId: "c2", machineId: "m2", token: "t" },
          ]),
        }),
      "duplicate token",
    );
  });

  it("accepts more than fifty configured machines and bounds parse work by bytes", () => {
    const collectors = Array.from({ length: 75 }, (_, i) => ({
      collectorId: `c${i}`,
      machineId: `m${i}`,
      token: `t${i}`,
    }));
    const config = parseHubConfig({ HUB_COLLECTORS: JSON.stringify(collectors) });
    assert.equal(config.collectors.length, 75);
    const state = new HubState({ collectors: config.collectors, hubRunId: "hub-run-1", now: tick() });
    assert.equal(state.snapshot().machines.length, 75);

    const oversized = JSON.stringify([
      { collectorId: "c", machineId: "m", token: "x".repeat(MAX_HUB_COLLECTORS_BYTES) },
    ]);
    assert.throws(
      () => parseHubConfig({ HUB_COLLECTORS: oversized }),
      /at most .* bytes/,
    );
  });
});

describe("hub state: identity mapping", () => {
  it("maps bearer tokens to configured identities", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    assert.deepEqual(state.identityByToken("token-a"), CREDENTIALS[0]);
    assert.equal(state.identityByToken("nope"), null);
  });

  it("requires a nonempty hubRunId", () => {
    assert.throws(() => new HubState({ collectors: [], hubRunId: "", now: tick() }));
  });

  it("rejects duplicate identities/tokens in its constructor too", () => {
    assert.throws(() =>
      new HubState({
        collectors: [
          { collectorId: "c", machineId: "m1", token: "t1" },
          { collectorId: "c", machineId: "m2", token: "t2" },
        ],
        hubRunId: "hub-run-1",
      }),
    );
    assert.throws(() =>
      new HubState({
        collectors: [
          { collectorId: "c1", machineId: "m1", token: "t1" },
          { collectorId: "c2", machineId: "m1", token: "t2" },
        ],
        hubRunId: "hub-run-1",
      }),
    );
    assert.throws(() =>
      new HubState({
        collectors: [
          { collectorId: "c1", machineId: "m1", token: "same" },
          { collectorId: "c2", machineId: "m2", token: "same" },
        ],
        hubRunId: "hub-run-1",
      }),
    );
  });
});

describe("hub state: sessions", () => {
  it("starts a session after authenticated hello and reports connected", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const sessionId = helloSession(state);
    assert.equal(state.connectedCount(), 1);
    const snapshot = state.snapshot();
    assert.equal(snapshot.hubRunId, "hub-run-1");
    const machine = snapshot.machines.find((m) => m.machineId === "machine-a");
    assert.ok(machine);
    assert.equal(machine.connected, true);
    assert.equal(machine.hostname, "mock-machine-a");
    assert.equal(machine.lastReceivedAt, null);
    assert.equal(machine.metrics, undefined);
    void sessionId;
  });

  it("rejects unconfigured collector identities", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    assert.throws(
      () => helloSession(state, { collectorId: "ghost", runId: "run-1" }),
      (error: unknown) => error instanceof HubStateError && error.code === "unknown-collector",
    );
  });

  it("prevents conflicting simultaneous sessions for the same run", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    helloSession(state, { sessionId: "s1" });
    assert.throws(
      () => helloSession(state, { sessionId: "s2" }),
      (error: unknown) => error instanceof HubStateError && error.code === "session-conflict",
    );
    assert.equal(state.sessionCount(), 1);
  });

  it("a new run supersedes an old session only after its own hello", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const oldSession = helloSession(state, { sessionId: "s1", runId: "run-1" });
    state.applyMetrics(oldSession, makeRecord("collector-a", 1), new Date(1000).toISOString());
    const newSession = helloSession(state, { sessionId: "s2", runId: "run-2" });
    assert.equal(state.sessionCount(), 1);
    // The old session can no longer overwrite the replacement.
    state.applyMetrics(oldSession, makeRecord("collector-a", 2), new Date(2000).toISOString());
    const machine = state.snapshot().machines.find((m) => m.machineId === "machine-a");
    assert.ok(machine);
    // Old-session records are ignored entirely (run mismatch), the cleared
    // state from supersession stays intact until the new run's records land.
    assert.equal(machine?.metrics, undefined);
    assert.equal(machine?.connected, true);
    state.applyMetrics(
      newSession,
      makeRecord("collector-a", 10, { runId: "run-2" }),
      new Date(3000).toISOString(),
    );
    assert.equal(
      state.snapshot().machines.find((m) => m.machineId === "machine-a")?.metrics?.sequence,
      10,
    );
  });

  it("records must match hello identity (collector, run, boot)", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const sessionId = helloSession(state, { runId: "run-1", bootId: "boot-1" });
    state.applyMetrics(sessionId, makeRecord("collector-b", 1), new Date(1000).toISOString());
    state.applyMetrics(
      sessionId,
      makeRecord("collector-a", 2, { runId: "run-9" }),
      new Date(2000).toISOString(),
    );
    state.applyMetrics(
      sessionId,
      makeRecord("collector-a", 3, { bootId: "boot-9" }),
      new Date(3000).toISOString(),
    );
    assert.equal(state.snapshot().machines.find((m) => m.machineId === "machine-a")?.metrics, undefined);
    assert.equal(state.snapshot().machines.find((m) => m.machineId === "machine-a")?.lastReceivedAt, null);
  });

  it("records from an ended session are rejected", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const sessionId = helloSession(state);
    state.applyMetrics(sessionId, makeRecord("collector-a", 1), new Date(1000).toISOString());
    state.endSession(sessionId);
    state.applyMetrics(sessionId, makeRecord("collector-a", 2), new Date(2000).toISOString());
    // The receipt and latest record survive the disconnect, later records on
    // the closed session id change nothing.
    const machine = state.snapshot().machines.find((m) => m.machineId === "machine-a");
    assert.equal(machine?.metrics?.sequence, 1);
    assert.equal(machine?.lastReceivedAt, new Date(1000).toISOString());
  });

  it("endSession keeps the last metrics but marks the machine offline", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const sessionId = helloSession(state);
    state.applyMetrics(sessionId, makeRecord("collector-a", 1), new Date(1000).toISOString());
    state.endSession(sessionId);
    const machine = state.snapshot().machines.find((m) => m.machineId === "machine-a");
    assert.equal(machine?.connected, false);
    assert.ok(machine?.metrics);
    assert.ok(machine?.lastReceivedAt);
  });
});

describe("hub state: advertised delivery cadence", () => {
  it("stores the hello interval and emits it on every snapshot row", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    helloSession(state, { sessionId: "s1", expectedIntervalMs: 600 });
    helloSession(state, { sessionId: "s2", collectorId: "collector-b" });
    const machines = state.snapshot().machines;
    assert.equal(machines[0]?.expectedIntervalMs, 600);
    assert.equal(machines[1]?.expectedIntervalMs, null, "hello without an interval stores null, not a default");
    // Machines that never connected advertise an unknown cadence too.
    assert.equal(machines[2]?.expectedIntervalMs, null);
  });

  it("a same-run reconnect replaces the advertised cadence, including back to null", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    helloSession(state, { sessionId: "old", expectedIntervalMs: 600 });
    assert.equal(state.snapshot().machines[0]?.expectedIntervalMs, 600);
    state.endSession("old");
    helloSession(state, { sessionId: "new", expectedIntervalMs: 1_000 });
    assert.equal(state.snapshot().machines[0]?.expectedIntervalMs, 1_000);
    state.endSession("new");
    // Same run hello again, this time without the field: resets to null.
    helloSession(state, { sessionId: "newer" });
    assert.equal(state.snapshot().machines[0]?.expectedIntervalMs, null);
  });

  it("a cadence change on a same-run reconnect updates the snapshot value", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const first = helloSession(state, { sessionId: "s1", expectedIntervalMs: 600 });
    state.applyMetrics(first, makeRecord("collector-a", 1), new Date(1000).toISOString());
    state.endSession(first);
    const second = helloSession(state, { sessionId: "s2", expectedIntervalMs: 200 });
    assert.equal(second !== first, true);
    assert.equal(state.snapshot().machines[0]?.expectedIntervalMs, 200);
    // Last-known metrics are still retained across the reconnect.
    assert.equal(state.snapshot().machines[0]?.metrics?.sequence, 1);
  });

  it("disconnect retains the last-known cadence together with the last metrics", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const sessionId = helloSession(state, { expectedIntervalMs: 450 });
    state.applyMetrics(sessionId, makeRecord("collector-a", 1), new Date(1000).toISOString());
    state.endSession(sessionId);
    const machine = state.snapshot().machines.find((m) => m.machineId === "machine-a");
    assert.equal(machine?.connected, false);
    assert.equal(machine?.expectedIntervalMs, 450);
    assert.ok(machine?.metrics);
    assert.ok(machine?.lastReceivedAt);
  });

  it("a rejected conflicting session cannot change the advertised cadence", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    helloSession(state, { sessionId: "current", expectedIntervalMs: 600 });
    assert.throws(
      () => helloSession(state, { sessionId: "usurper", expectedIntervalMs: 200 }),
      (error: unknown) => error instanceof HubStateError && error.code === "session-conflict",
    );
    const machine = state.snapshot().machines.find((m) => m.machineId === "machine-a");
    assert.equal(machine?.expectedIntervalMs, 600);
    assert.equal(state.sessionCount(), 1);
  });

  it("a new run hello replaces the cadence and resets metric state", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const oldSession = helloSession(state, { sessionId: "old", expectedIntervalMs: 600 });
    state.applyMetrics(oldSession, makeRecord("collector-a", 1), new Date(1000).toISOString());
    helloSession(state, { sessionId: "new", runId: "run-2", expectedIntervalMs: 200 });
    const machine = state.snapshot().machines.find((m) => m.machineId === "machine-a");
    assert.equal(machine?.expectedIntervalMs, 200);
    assert.equal(machine?.metrics, undefined);
    assert.equal(machine?.lastReceivedAt, null);
    // And the new run's hello without the field resets an advertised one.
    helloSession(state, { sessionId: "newer", runId: "run-3" });
    assert.equal(state.snapshot().machines[0]?.expectedIntervalMs, null);
  });

  it("boot identity cannot change within a run even with a new cadence", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    helloSession(state, { sessionId: "s1", expectedIntervalMs: 600 });
    assert.throws(
      () => state.startSession({
        sessionId: "s2", collectorId: "collector-a", runId: "run-1",
        bootId: "boot-2", hostname: "mock-machine-a", expectedIntervalMs: 200,
      }),
      (error: unknown) => error instanceof HubStateError && error.code === "identity-mismatch",
    );
    // The rejected session left the advertised cadence untouched.
    assert.equal(state.snapshot().machines[0]?.expectedIntervalMs, 600);
  });

  it("snapshots round-trip through the shared snapshot parser", () => {
    // Epochs must be parseable by the snapshot contract (2000..2200 window).
    let clock = Date.parse("2026-11-02T12:00:00Z");
    const state = new HubState({
      collectors: CREDENTIALS, hubRunId: "hub-run-1", now: () => new Date((clock += 1000)),
    });
    const slow = helloSession(state, { collectorId: "collector-c", expectedIntervalMs: 7_200_000 });
    state.applyMetrics(slow, makeRecord("collector-c", 1), new Date(clock - 1000).toISOString());
    state.endSession(slow);
    helloSession(state, { expectedIntervalMs: 100 });
    const parsed = parseHubSnapshot(JSON.parse(JSON.stringify(state.snapshot())));
    const slowMachine = parsed.machines[2];
    assert.equal(slowMachine?.expectedIntervalMs, 7_200_000);
    assert.equal(slowMachine?.connected, false);
    assert.ok(slowMachine?.metrics);
    assert.equal(parsed.machines[0]?.expectedIntervalMs, 100);
  });
});

describe("hub state: safe latest-state ordering", () => {
  it("retains sequence ordering when the same run reconnects", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const oldSession = helloSession(state, { sessionId: "old" });
    state.applyMetrics(oldSession, makeRecord("collector-a", 9), new Date(1000).toISOString());
    state.endSession(oldSession);
    const newSession = helloSession(state, { sessionId: "new" });
    state.applyMetrics(newSession, makeRecord("collector-a", 2), new Date(2000).toISOString());
    // The same run's later hello may replace the session, but the record
    // ordering rule survives: sequence 2 cannot regress past sequence 9.
    assert.equal(state.snapshot().machines[0].metrics?.sequence, 9);
  });

  it("clears a previous run's metrics after a disconnected collector restarts", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const oldSession = helloSession(state, { sessionId: "old" });
    state.applyMetrics(oldSession, makeRecord("collector-a", 9), new Date(1000).toISOString());
    state.endSession(oldSession);
    helloSession(state, { sessionId: "new", runId: "run-2" });
    assert.equal(state.snapshot().machines[0].metrics, undefined);
    assert.equal(state.snapshot().machines[0].lastReceivedAt, null);
  });

  it("rejects an identity-mismatched record without changing latest state", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const sessionId = helloSession(state);
    state.applyMetrics(sessionId, makeRecord("collector-a", 1), new Date(1000).toISOString());
    state.applyMetrics(sessionId, makeRecord("collector-b", 3), new Date(2000).toISOString());
    assert.equal(state.snapshot().machines[0].metrics?.sequence, 1);
    assert.equal(state.snapshot().machines[0].lastReceivedAt, new Date(1000).toISOString());
  });

  it("accepts increasing sequences and drops duplicates or regressions", () => {
    const now = tick();
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now });
    const sessionId = helloSession(state);
    const at = (s: number) => new Date(s * 1000).toISOString();
    state.applyMetrics(sessionId, makeRecord("collector-a", 5), at(5));
    state.applyMetrics(sessionId, makeRecord("collector-a", 3), at(3));
    state.applyMetrics(sessionId, makeRecord("collector-a", 4), at(4));
    state.applyMetrics(sessionId, makeRecord("collector-a", 6), at(6));
    state.applyMetrics(sessionId, makeRecord("collector-a", 6), at(6));
    state.applyMetrics(sessionId, makeRecord("collector-a", 9), at(9));
    assert.equal(state.snapshot().machines.find((m) => m.machineId === "machine-a")?.metrics?.sequence, 9);

    // Across batches the same rule holds: regression never appears in state.
    state.applyMetrics(sessionId, makeRecord("collector-a", 4), at(10));
    assert.equal(
      state.snapshot().machines.find((m) => m.machineId === "machine-a")?.metrics?.sequence,
      9,
    );
  });

  it("records the receipt time and keeps the newest metrics", () => {
    const now = tick();
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now });
    const sessionId = helloSession(state);
    state.applyMetrics(sessionId, makeRecord("collector-a", 1), new Date(1000).toISOString());
    const machine = state.snapshot().machines.find((m) => m.machineId === "machine-a");
    assert.ok(machine?.metrics);
    assert.equal(machine.metrics.sequence, 1);
    assert.equal(machine.metrics.receivedAt, new Date(1000).toISOString());
    assert.equal(machine.lastReceivedAt, machine.metrics.receivedAt);
  });

  it("includes configured machines that never connected", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    helloSession(state, { collectorId: "collector-b" });
    const snapshot = state.snapshot();
    assert.equal(snapshot.machines.length, 3);
    const never = snapshot.machines.find((m) => m.machineId === "machine-c");
    assert.ok(never);
    assert.equal(never.connected, false);
    assert.equal(never.lastReceivedAt, null);
    assert.equal(never.metrics, undefined);
  });

  it("orders snapshot machines by machineId", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    assert.deepEqual(
      state.snapshot().machines.map((m) => m.machineId),
      ["machine-a", "machine-b", "machine-c"],
    );
  });

  it("notifies subscribers on change and lets them unsubscribe", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    let calls = 0;
    const unsubscribe = state.subscribe(() => {
      calls += 1;
    });
    const sessionId = helloSession(state);
    assert.equal(calls, 1);
    unsubscribe();
    state.applyMetrics(sessionId, makeRecord("collector-a", 1), new Date(1000).toISOString());
    assert.equal(calls, 1);
  });
});

describe("live feed (SSE)", () => {
  type FakeResponse = {
    headersSent: boolean;
    status: number | null;
    headers: Record<string, unknown> | null;
    chunks: string[];
    writableLength: number;
    writableEnded: boolean;
    destroyed: boolean;
    listeners: Map<string, Array<() => void>>;
    writeHead(status: number, headers: Record<string, unknown>): void;
    write(chunk: string): boolean;
    end(chunk?: string): void;
    destroy(): void;
    on(event: string, listener: () => void): void;
  };

  function fakeResponse(): FakeResponse {
    const res: FakeResponse = {
      headersSent: false,
      status: null,
      headers: null,
      chunks: [],
      writableLength: 0,
      writableEnded: false,
      destroyed: false,
      listeners: new Map(),
      writeHead(status, headers) {
        this.status = status;
        this.headers = headers;
        this.headersSent = true;
      },
      write(chunk) {
        if (this.destroyed) return false;
        this.chunks.push(chunk);
        return true;
      },
      end(chunk) {
        this.writableEnded = true;
        if (chunk !== undefined) this.chunks.push(chunk);
      },
      destroy() {
        this.destroyed = true;
        this.listeners.get("close")?.forEach((listener) => listener());
      },
      on(event, listener) {
        const list = this.listeners.get(event) ?? [];
        list.push(listener);
        this.listeners.set(event, list);
      },
    };
    return res;
  }

  function feedRequest(lastEventId?: string): { headers: Record<string, string> } {
    return { headers: lastEventId ? { "last-event-id": lastEventId } : {} };
  }

  function parseFrames(chunks: string[]): Array<{ id?: string; event?: string; data?: string }> {
    const text = chunks.join("");
    return text
      .split("\n\n")
      .filter((block) => block.length > 0 && !block.startsWith(":"))
      .map((block) => {
        const lines = block.split("\n");
        const value = (prefix: string): string | undefined => {
          const line = lines.find((candidate) => candidate.startsWith(prefix));
          return line === undefined ? undefined : line.slice(prefix.length);
        };
        return { id: value("id: "), event: value("event: "), data: value("data: ") };
      });
  }

  it("writes the initial snapshot synchronously with hubRunId cursor ids", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const feed = new LiveFeed(state, { coalesceMs: 0, keepaliveMs: 10_000 });
    const res = fakeResponse() as never as import("node:http").ServerResponse;
    feed.attach(feedRequest() as never as import("node:http").IncomingMessage, res, { sessionId: "test-viewer", scope: "*" });
    const frames = parseFrames((res as unknown as FakeResponse).chunks);
    assert.equal(frames.length, 1);
    assert.equal(frames[0].event, "snapshot");
    assert.match(frames[0].id!, /^hub-run-1:\d+$/);
    assert.ok(frames[0].data?.includes('"hubRunId":"hub-run-1"'));
  });

  it("sends reset instead of snapshot when Last-Event-ID reconnects", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const feed = new LiveFeed(state, { coalesceMs: 0, keepaliveMs: 10_000 });
    const res = fakeResponse() as never as import("node:http").ServerResponse;
    feed.attach(feedRequest("hub-run-1:0") as never as import("node:http").IncomingMessage, res, { sessionId: "test-viewer", scope: "*" });
    const frames = parseFrames((res as unknown as FakeResponse).chunks);
    assert.equal(frames[0].event, "reset");
  });

  it("hidden changes emit no frames and cannot advance a scoped viewer's cursor", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const feed = new LiveFeed(state, { coalesceMs: 0 });
    const fake = fakeResponse();
    feed.attach(feedRequest() as never as import("node:http").IncomingMessage,
      fake as never as import("node:http").ServerResponse,
      { sessionId: "viewer", scope: new Set(["machine-a"]) });
    helloSession(state, { collectorId: "collector-b" });
    state.applyMetrics(helloSession(state), makeRecord("collector-a", 1), new Date(1000).toISOString());
    const frames = parseFrames(fake.chunks);
    assert.equal(frames.length, 3, "initial snapshot plus visible hello and sample");
    const ids = frames.map(frame => BigInt(frame.id!.split(":").at(-1)!));
    assert.equal(ids[1], ids[0] + 1n);
    assert.equal(ids[2], ids[0] + 2n);
    feed.stop();
  });

  it("broadcasts full HubSnapshots on state change", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const feed = new LiveFeed(state, { coalesceMs: 0, keepaliveMs: 10_000 });
    const fake = fakeResponse();
    const res = fake as never as import("node:http").ServerResponse;
    feed.attach(feedRequest() as never as import("node:http").IncomingMessage, res, { sessionId: "test-viewer", scope: "*" });
    const sessionId = helloSession(state);
    state.applyMetrics(sessionId, makeRecord("collector-a", 1), new Date(1000).toISOString());
    const frames = parseFrames(fake.chunks);
    assert.equal(frames.length, 3); // snapshot, update (hello), update (sample)
    assert.equal(frames[1].event, "update");
    // Every update carries a full snapshot.
    for (const frame of frames) {
      assert.ok(frame.data);
      const parsed = JSON.parse(frame.data) as { hubRunId: string; machines: unknown[] };
      assert.equal(parsed.hubRunId, "hub-run-1");
      assert.equal(parsed.machines.length, 3);
    }
  });

  it("filters a snapshot beyond fifty machines to an explicit viewer scope", () => {
    const collectors = Array.from({ length: 60 }, (_, i) => ({
      collectorId: `collector-${i}`,
      machineId: `machine-${i}`,
      token: `token-${i}`,
    }));
    const state = new HubState({ collectors, hubRunId: "hub-run-large", now: tick() });
    const feed = new LiveFeed(state, { coalesceMs: 0, keepaliveMs: 10_000 });
    const allowed = new Set(collectors.slice(0, 51).map((entry) => entry.machineId));
    const fake = fakeResponse();
    try {
      feed.attach(feedRequest() as never as import("node:http").IncomingMessage,
        fake as never as import("node:http").ServerResponse,
        { sessionId: "scoped-viewer", scope: allowed });
      const frames = parseFrames(fake.chunks);
      assert.equal(frames.length, 1);
      const snapshot = JSON.parse(frames[0]!.data!) as { machines: Array<{ machineId: string }> };
      assert.equal(snapshot.machines.length, 51);
      assert.equal(snapshot.machines.every((machine) => allowed.has(machine.machineId)), true);
      assert.equal(
        snapshot.machines.some((machine) => machine.machineId === collectors[51]!.machineId),
        false,
      );
      assert.equal(frames[0]!.data!.includes(collectors[51]!.collectorId), false);
    } finally {
      feed.stop();
    }
  });

  it("rejects /api/live connections beyond the client cap with 503", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const feed = new LiveFeed(state, { coalesceMs: 0, maxClients: 1, keepaliveMs: 10_000 });
    const first = fakeResponse() as never as import("node:http").ServerResponse;
    feed.attach(feedRequest() as never as import("node:http").IncomingMessage, first, { sessionId: "test-viewer", scope: "*" });
    const second = fakeResponse() as never as import("node:http").ServerResponse;
    feed.attach(feedRequest() as never as import("node:http").IncomingMessage, second, { sessionId: "test-viewer", scope: "*" });
    assert.equal((second as unknown as FakeResponse).status, 503);
    assert.equal(feed.clientCount(), 1);
  });

  it("cleans up on client close and stops the feed on shutdown", () => {
    const state = new HubState({ collectors: CREDENTIALS, hubRunId: "hub-run-1", now: tick() });
    const feed = new LiveFeed(state, { coalesceMs: 0, keepaliveMs: 10_000 });
    const fake = fakeResponse();
    feed.attach(feedRequest() as never as import("node:http").IncomingMessage, fake as never as import("node:http").ServerResponse, { sessionId: "test-viewer", scope: "*" });
    fake.destroy();
    assert.equal(feed.clientCount(), 0);
    const connected = fakeResponse();
    feed.attach(feedRequest() as never as import("node:http").IncomingMessage, connected as never as import("node:http").ServerResponse, { sessionId: "test-viewer", scope: "*" });
    feed.stop();
    assert.equal(connected.writableEnded, true);
    assert.equal(feed.clientCount(), 0);
  });

// Production coalescing leaves the initial snapshot synchronous and bounds
// full-snapshot work even when collectors publish many samples per tick.
it("coalesces state bursts into one latest snapshot for live browsers", async () => {
  const state = new HubState({ collectors: CREDENTIALS, hubRunId: "coalesced", now: tick() });
  const feed = new LiveFeed(state, { coalesceMs: 10 });
  const fake = fakeResponse();
  try {
    feed.attach(feedRequest() as never, fake as never, { sessionId: "viewer", scope: "*" });
    const id = helloSession(state);
    state.applyMetrics(id, makeRecord("collector-a", 1), new Date(1000).toISOString());
    state.applyMetrics(id, makeRecord("collector-a", 2), new Date(2000).toISOString());
    assert.equal(parseFrames(fake.chunks).length, 1);
    await new Promise(resolve => setTimeout(resolve, 30));
    const frames = parseFrames(fake.chunks);
    assert.equal(frames.length, 2);
    const final = JSON.parse(frames[1].data!);
    assert.equal(final.machines[0].metrics.sequence, 2);
  } finally { feed.stop(); }
});

});
