/** Alert engine: durations, recovery, unavailable/stale, maintenance, restart dedup. */
import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { openHubDatabase } from "../src/hub/storage/db";
import { AlertEngine } from "../src/hub/alerts/engine";
import { basePayload, machineState, metricsRecord } from "./alerts-fixtures";
import type { MetricsPayload } from "../src/contracts";
import type { WebhookDeliveryRequest } from "../src/hub/alerts/webhook";

function sender() {
  const calls: WebhookDeliveryRequest[] = [];
  return {
    calls,
    send: async (request: WebhookDeliveryRequest) => {
      calls.push(request);
      return {ok: true, status: 200, error: null};
    },
  };
}

function start() {
  const database = openHubDatabase(":memory:");
  let clock = 1_000_000;
  const webhook = sender();
  const engine = new AlertEngine({
    database,
    now: () => clock,
    sendWebhook: webhook.send,
  });
  engine.configureWebhook({url: "http://127.0.0.1:9/alerts-sink"});
  let sequence = 0;
  return {
    database,
    engine,
    webhook,
    advance(ms: number) {
      clock += ms;
    },
    record(payload: MetricsPayload = basePayload()) {
      sequence += 1;
      return metricsRecord(payload, {sequence});
    },
    clock: () => clock,
  };
}

test("threshold duration opens one incident, recovers, and never duplicates", async () => {
  const fixture = start();
  const rule = fixture.engine.createRule({
    name: "CPU high",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "above",
    threshold: 80,
    durationMs: 5_000,
  });
  const high = basePayload({cpu: {status: "ok", usagePercent: 90, perCore: [90, 90]}});
  const low = basePayload({cpu: {status: "ok", usagePercent: 20, perCore: [20, 20]}});

  fixture.engine.observe("m1", fixture.record(high), fixture.clock());
  assert.equal(fixture.engine.listIncidents()[0], undefined);
  assert.equal(fixture.engine.getRule(rule.id).state, "pending");

  fixture.advance(4_000);
  fixture.engine.observe("m1", fixture.record(high), fixture.clock());
  assert.equal(fixture.engine.listIncidents().length, 0, "duration has not elapsed");

  fixture.advance(1_000);
  fixture.engine.observe("m1", fixture.record(high), fixture.clock());
  await fixture.engine.processNotifications();
  let incidents = fixture.engine.listIncidents();
  assert.equal(incidents.length, 1);
  assert.equal(incidents[0]!.status, "open");
  assert.equal(fixture.engine.getRule(rule.id).state, "firing");
  assert.equal(fixture.webhook.calls.length, 1);
  assert.match(fixture.webhook.calls[0]!.body, /"event":"fired"/);
  assert.equal(fixture.engine.notificationSummary().delivered, 1);

  fixture.advance(1_000);
  fixture.engine.observe("m1", fixture.record(high), fixture.clock());
  await fixture.engine.processNotifications();
  assert.equal(fixture.engine.listIncidents().length, 1, "still one open incident");
  assert.equal(fixture.webhook.calls.length, 1, "no duplicate fired job");

  fixture.advance(1_000);
  fixture.engine.observe("m1", fixture.record(low), fixture.clock());
  await fixture.engine.processNotifications();
  incidents = fixture.engine.listIncidents();
  assert.equal(incidents.length, 1);
  assert.equal(incidents[0]!.status, "recovered");
  assert.equal(fixture.engine.getRule(rule.id).state, "ok");
  assert.equal(fixture.webhook.calls.length, 2);
  assert.match(fixture.webhook.calls[1]!.body, /"event":"recovered"/);
  assert.equal(fixture.engine.notificationSummary().delivered, 2);

  fixture.advance(1_000);
  fixture.engine.observe("m1", fixture.record(low), fixture.clock());
  await fixture.engine.processNotifications();
  assert.equal(fixture.engine.listIncidents().length, 1, "recovery is not duplicated");
  fixture.engine.stop();
  fixture.database.close();
});

test("unavailable conditions fire and recover; threshold rules never use an unavailable value", async () => {
  const fixture = start();
  const unavailable = fixture.engine.createRule({
    name: "GPU unavailable",
    machineId: "m1",
    metric: "gpu.utilization",
    resourceId: "gpu:GPU-1",
    condition: "unavailable",
    durationMs: 0,
  });
  const threshold = fixture.engine.createRule({
    name: "CPU high",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "above",
    threshold: 80,
    durationMs: 0,
  });
  const gpuDown = basePayload({
    cpu: {status: "unavailable", reason: "cpu counters unavailable"},
    gpus: {status: "unavailable", reason: "no supported GPU telemetry"},
  });
  fixture.engine.observe("m1", fixture.record(gpuDown), fixture.clock());
  await fixture.engine.processNotifications();

  const incidents = fixture.engine.listIncidents();
  assert.equal(incidents.length, 1);
  assert.equal(incidents[0]!.ruleId, unavailable.id);
  assert.equal(fixture.engine.getRule(unavailable.id).state, "firing");
  assert.equal(fixture.engine.getRule(threshold.id).state, "ok");

  fixture.advance(1_000);
  fixture.engine.observe("m1", fixture.record(basePayload()), fixture.clock());
  await fixture.engine.processNotifications();
  assert.equal(fixture.engine.listIncidents()[0]!.status, "recovered");
  assert.equal(fixture.engine.getRule(unavailable.id).state, "ok");
  fixture.engine.stop();
  fixture.database.close();
});

test("stale uses max(15 s, 3 × cadence), receipt time, and recovers on a fresh receipt", async () => {
  const fixture = start();
  const rule = fixture.engine.createRule({
    name: "Stale",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "stale",
    durationMs: 0,
  });
  const record = fixture.record();
  const received = new Date(fixture.clock()).toISOString();

  // 20 s old with a 10 s cadence is still below 3 × cadence = 30 s.
  fixture.engine.tick([machineState({
    lastReceivedAt: new Date(fixture.clock() - 20_000).toISOString(),
    expectedIntervalMs: 10_000,
    metrics: record,
  })]);
  assert.equal(fixture.engine.listIncidents().length, 0);

  // 5 s cadence lowers the threshold to the 15 s floor; 20 s is stale.
  fixture.engine.tick([machineState({
    lastReceivedAt: new Date(fixture.clock() - 20_000).toISOString(),
    expectedIntervalMs: 5_000,
    metrics: record,
  })]);
  await fixture.engine.processNotifications();
  assert.equal(fixture.engine.listIncidents().length, 1);
  assert.equal(fixture.engine.listIncidents()[0]!.status, "open");

  fixture.engine.tick([machineState({
    lastReceivedAt: received,
    expectedIntervalMs: 5_000,
    connected: false,
    metrics: record,
  })]);
  await fixture.engine.processNotifications();
  assert.equal(fixture.engine.listIncidents()[0]!.status, "recovered");
  assert.equal(fixture.engine.getRule(rule.id).state, "ok");
  fixture.engine.stop();
  fixture.database.close();
});

test("maintenance suppresses pending and incident creation until it expires", () => {
  const fixture = start();
  const rule = fixture.engine.createRule({
    name: "Maintenance CPU",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "above",
    threshold: 80,
    durationMs: 0,
    maintenanceUntil: new Date(fixture.clock() + 60_000).toISOString(),
  });
  const high = basePayload({cpu: {status: "ok", usagePercent: 99, perCore: [99, 99]}});
  fixture.engine.observe("m1", fixture.record(high), fixture.clock());
  assert.equal(fixture.engine.listIncidents().length, 0);
  assert.equal(fixture.engine.getRule(rule.id).state, "ok");

  fixture.advance(61_000);
  fixture.engine.observe("m1", fixture.record(high), fixture.clock());
  assert.equal(fixture.engine.listIncidents().length, 1);
  assert.equal(fixture.engine.getRule(rule.id).state, "firing");
  fixture.engine.stop();
  fixture.database.close();
});

test("restart preserves open incidents/pending durations and ignores the same record again", async () => {
  const dir = mkdtempSync(join(tmpdir(), "rigscope-alerts-"));
  const path = join(dir, "hub.db");
  try {
    let clock = 2_000_000;
    const first = openHubDatabase(path);
    const firstEngine = new AlertEngine({
      database: first,
      now: () => clock,
      sendWebhook: async () => ({ok: true, status: 200, error: null}),
    });
    firstEngine.configureWebhook({url: "http://127.0.0.1:9/sink"});
    const rule = firstEngine.createRule({
      name: "Restart",
      machineId: "m1",
      metric: "cpu.usage",
      condition: "above",
      threshold: 80,
      durationMs: 0,
    });
    const high = basePayload({cpu: {status: "ok", usagePercent: 95, perCore: [95, 95]}});
    const record = metricsRecord(high, {sequence: 1});
    firstEngine.observe("m1", record, clock);
    await firstEngine.processNotifications();
    assert.equal(firstEngine.listIncidents().length, 1);
    firstEngine.stop();
    first.close();

    clock += 1_000;
    const second = openHubDatabase(path);
    const secondEngine = new AlertEngine({
      database: second,
      now: () => clock,
      sendWebhook: async () => ({ok: true, status: 200, error: null}),
    });
    // Same persisted record after a lost event/restart: progress dedup ignores it.
    secondEngine.observe("m1", record, clock);
    await secondEngine.processNotifications();
    assert.equal(secondEngine.listIncidents().length, 1, "restart did not duplicate the incident");
    assert.equal(secondEngine.notificationSummary().delivered, 1);

    clock += 1_000;
    const next = metricsRecord(high, {sequence: 2});
    secondEngine.observe("m1", next, clock);
    await secondEngine.processNotifications();
    assert.equal(secondEngine.listIncidents().length, 1, "firing state survives restart");
    assert.equal(secondEngine.getRule(rule.id).state, "firing");

    // Pending duration also survives: 4 s elapsed before restart, 6 s after.
    const pendingRule = secondEngine.createRule({
      name: "Pending restart",
      machineId: "m2",
      metric: "cpu.usage",
      condition: "above",
      threshold: 80,
      durationMs: 10_000,
    });
    secondEngine.observe("m2", metricsRecord(high, {sequence: 1}), clock);
    assert.equal(secondEngine.getRule(pendingRule.id).state, "pending");
    secondEngine.stop();
    second.close();

    clock += 6_000;
    const third = openHubDatabase(path);
    const thirdEngine = new AlertEngine({
      database: third,
      now: () => clock,
      sendWebhook: async () => ({ok: true, status: 200, error: null}),
    });
    thirdEngine.observe("m2", metricsRecord(high, {sequence: 2}), clock);
    assert.equal(thirdEngine.getRule(pendingRule.id).state, "pending", "only 6 of 10 s elapsed");
    clock += 4_000;
    thirdEngine.observe("m2", metricsRecord(high, {sequence: 3}), clock);
    assert.equal(thirdEngine.getRule(pendingRule.id).state, "firing");
    thirdEngine.stop();
    third.close();
  } finally {
    rmSync(dir, {recursive: true, force: true});
  }
});

test("a retry state survives restart and delivers later without creating a second job", async () => {
  const dir = mkdtempSync(join(tmpdir(), "rigscope-alerts-retry-"));
  const path = join(dir, "hub.db");
  try {
    let clock = 7_000_000;
    const first = openHubDatabase(path);
    const firstEngine = new AlertEngine({
      database: first,
      now: () => clock,
      maxAttempts: 4,
      sendWebhook: async () => ({ok: false, status: 503, error: "HTTP 503"}),
    });
    firstEngine.configureWebhook({url: "http://127.0.0.1:9/sink"});
    firstEngine.createRule({
      name: "Retry restart",
      machineId: "m1",
      metric: "cpu.usage",
      condition: "above",
      threshold: 1,
      durationMs: 0,
    });
    const high = basePayload({cpu: {status: "ok", usagePercent: 99, perCore: [99, 99]}});
    firstEngine.observe("m1", metricsRecord(high, {sequence: 1}), clock);
    await firstEngine.processNotifications();
    const before = firstEngine.notificationSummary();
    assert.equal(before.pending, 1);
    assert.equal(before.recent[0]!.attempts, 1);
    const jobId = before.recent[0]!.id;
    firstEngine.stop();
    first.close();

    clock += 10_000;
    const second = openHubDatabase(path);
    let deliveredCalls = 0;
    const secondEngine = new AlertEngine({
      database: second,
      now: () => clock,
      maxAttempts: 4,
      sendWebhook: async () => {
        deliveredCalls += 1;
        return {ok: true, status: 200, error: null};
      },
    });
    await secondEngine.processNotifications();
    await secondEngine.processNotifications();
    const after = secondEngine.notificationSummary();
    assert.equal(deliveredCalls, 1);
    assert.equal(after.delivered, 1);
    assert.equal(after.pending, 0);
    assert.equal(after.recent.length, 1);
    assert.equal(after.recent[0]!.id, jobId);
    assert.equal(after.recent[0]!.attempts, 2);
    secondEngine.stop();
    second.close();
  } finally {
    rmSync(dir, {recursive: true, force: true});
  }
});

test("remote error text is redacted before it reaches APIs or the database", async () => {
  const database = openHubDatabase(":memory:");
  let clock = 8_000_000;
  const secrets = {
    url: "http://127.0.0.1:9/private-sink?token=url-secret",
    header: "header-secret",
    secret: "hmac-secret",
  };
  const engine = new AlertEngine({
    database,
    now: () => clock,
    maxAttempts: 1,
    sendWebhook: async () => ({
      ok: false,
      status: 500,
      error:
        `POST ${secrets.url} failed; Authorization: Bearer bearer-secret; ` +
        `x-channel: ${secrets.header}; signature=${secrets.secret}`,
    }),
  });
  engine.configureWebhook({
    url: secrets.url,
    headers: {"x-channel": secrets.header},
    secret: secrets.secret,
  });
  engine.createRule({
    name: "Redaction",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "above",
    threshold: 1,
    durationMs: 0,
  });
  const high = basePayload({cpu: {status: "ok", usagePercent: 99, perCore: [99, 99]}});
  engine.observe("m1", metricsRecord(high, {sequence: 1}), clock);
  await engine.processNotifications();
  const summary = engine.notificationSummary();
  const stored = JSON.stringify(summary);
  for (const secret of [secrets.url, secrets.header, secrets.secret, "bearer-secret", "url-secret"]) {
    assert.equal(stored.includes(secret), false, `must redact ${secret}`);
  }
  assert.match(summary.lastError ?? "", /\[redacted/);
  assert.equal(JSON.stringify(engine.getWebhookStatus()).includes(secrets.url), false);
  engine.stop();
  await engine.drain();
  database.close();
});

test("historical unavailable state after restart is ignored until a new live record arrives", () => {
  const database = openHubDatabase(":memory:");
  let clock = 9_000_000;
  const engine = new AlertEngine({database, now: () => clock});
  engine.createRule({
    name: "Historical unavailable",
    machineId: "m1",
    metric: "gpu.utilization",
    resourceId: "gpu:GPU-1",
    condition: "unavailable",
    durationMs: 0,
  });
  const record = metricsRecord(basePayload({gpus: {status: "unavailable", reason: "old"}}), {sequence: 1});
  engine.tick([machineState({
    lastReceivedAt: new Date(clock - 20_000).toISOString(),
    expectedIntervalMs: 5_000,
    metrics: record,
  })]);
  assert.equal(engine.listIncidents().length, 0);
  engine.stop();
  database.close();
});

test("drain bounds shutdown and late remote completions cannot write after the database closes", async () => {
  const database = openHubDatabase(":memory:");
  let clock = 10_000_000;
  let release: ((result: {ok: boolean; status: number | null; error: string | null}) => void) | null = null;
  const engine = new AlertEngine({
    database,
    now: () => clock,
    deliveryTimeoutMs: 100,
    sendWebhook: () => new Promise((resolve) => {
      release = resolve;
    }),
  });
  engine.configureWebhook({url: "http://127.0.0.1:9/sink"});
  engine.createRule({
    name: "Drain",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "above",
    threshold: 1,
    durationMs: 0,
  });
  const high = basePayload({cpu: {status: "ok", usagePercent: 99, perCore: [99, 99]}});
  engine.observe("m1", metricsRecord(high, {sequence: 1}), clock);
  await new Promise((resolve) => setTimeout(resolve, 10));
  await engine.drain(50);
  database.close();
  // A remote sender that ignored the abort signal completes later; the engine
  // must observe `closed` and return without touching SQLite.
  release!({ok: true, status: 200, error: null});
  await new Promise((resolve) => setTimeout(resolve, 30));
  await engine.processNotifications();
});

test("rule creation no longer depends on the machine's receipt protocol", () => {
  const database = openHubDatabase(":memory:");
  const engine = new AlertEngine({database, now: () => 11_000_000});
  // A machine that has not reported any metrics yet may still be configured;
  // value/unavailable rules evaluate the unreported families as absent.
  engine.tick([machineState({lastReceivedAt: new Date(11_000_000).toISOString()})]);
  const value = engine.createRule({
    name: "cpu threshold",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "above",
    threshold: 80,
    durationMs: 0,
  });
  assert.equal(value.condition, "above");
  const unavailable = engine.createRule({
    name: "memory unavailable",
    machineId: "m1",
    metric: "memory.used",
    condition: "unavailable",
    durationMs: 0,
  });
  assert.equal(unavailable.condition, "unavailable");
  const stale = engine.createRule({
    name: "cpu stale",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "stale",
    durationMs: 0,
  });
  assert.equal(stale.condition, "stale");
  engine.stop();
  database.close();
});

test("bottleneck metrics keep resource rules and I/O pressure follows the disk family", async () => {
  const fixture = start();
  const memory = fixture.engine.createRule({
    name: "Memory percent",
    machineId: "m1",
    metric: "memory.usage",
    condition: "above",
    threshold: 90,
    durationMs: 0,
  });
  assert.equal(memory.resourceId, "");
  const filesystem = fixture.engine.createRule({
    name: "Filesystem percent",
    machineId: "m1",
    metric: "fs.usage",
    resourceId: "fs:/",
    condition: "above",
    threshold: 90,
    durationMs: 0,
  });
  assert.equal(filesystem.resourceId, "fs:/");
  assert.throws(
    () =>
      fixture.engine.createRule({
        name: "Missing resource",
        machineId: "m1",
        metric: "fs.usage",
        condition: "above",
        threshold: 90,
        durationMs: 0,
      }),
    /requires a resource id/,
  );
  assert.throws(
    () =>
      fixture.engine.createRule({
        name: "Unexpected resource",
        machineId: "m1",
        metric: "memory.usage",
        resourceId: "fs:/",
        condition: "above",
        threshold: 90,
        durationMs: 0,
      }),
    /does not accept a resource id/,
  );

  const io = fixture.engine.createRule({
    name: "I/O pressure unavailable",
    machineId: "m1",
    metric: "io.pressure.some",
    condition: "unavailable",
    durationMs: 0,
  });
  fixture.engine.observe(
    "m1",
    fixture.record(basePayload({disks: {status: "unavailable", reason: "disk counters unavailable"}})),
    fixture.clock(),
  );
  await fixture.engine.processNotifications();
  assert.equal(fixture.engine.getRule(io.id).state, "firing");
  fixture.engine.stop();
  fixture.database.close();
});

test("a missing previously observed resource is unavailable, not a silent recovery", async () => {
  const fixture = start();
  const rule = fixture.engine.createRule({
    name: "GPU missing",
    machineId: "m1",
    metric: "gpu.utilization",
    resourceId: "gpu:GPU-1",
    condition: "unavailable",
    durationMs: 0,
  });
  const down = basePayload({gpus: {status: "unavailable", reason: "gpu reading stale"}});
  fixture.engine.observe("m1", fixture.record(down), fixture.clock());
  await fixture.engine.processNotifications();
  assert.equal(fixture.engine.getRule(rule.id).state, "firing");

  // Family is healthy again but the configured card is missing from the
  // device list. It must remain unavailable rather than recover silently.
  fixture.advance(1_000);
  const missing = basePayload({
    gpus: {
      status: "ok",
      devices: [
        {
          id: "gpu:GPU-2",
          status: "ok",
          utilizationPercent: 20,
          memoryUsedBytes: 10,
          memoryTotalBytes: 20,
          temperatureC: 40,
          powerWatts: 50,
        },
      ],
    },
  });
  fixture.engine.observe("m1", fixture.record(missing), fixture.clock());
  await fixture.engine.processNotifications();
  assert.equal(fixture.engine.getRule(rule.id).state, "firing");
  assert.equal(fixture.engine.listIncidents()[0]!.status, "open");

  // A real reading of that card recovers the incident.
  fixture.advance(1_000);
  fixture.engine.observe("m1", fixture.record(basePayload()), fixture.clock());
  await fixture.engine.processNotifications();
  assert.equal(fixture.engine.listIncidents()[0]!.status, "recovered");
  fixture.engine.stop();
  fixture.database.close();
});

test("failed deliveries persist, back off, expose errors, and stop at max attempts", async () => {
  const database = openHubDatabase(":memory:");
  let clock = 3_000_000;
  let fail = true;
  const calls: WebhookDeliveryRequest[] = [];
  const engine = new AlertEngine({
    database,
    now: () => clock,
    maxAttempts: 2,
    sendWebhook: async (request) => {
      calls.push(request);
      return fail
        ? {ok: false, status: 500, error: "HTTP 500"}
        : {ok: true, status: 200, error: null};
    },
  });
  engine.configureWebhook({url: "http://127.0.0.1:9/sink"});
  engine.createRule({
    name: "Failure path",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "above",
    threshold: 1,
    durationMs: 0,
  });
  const high = basePayload({cpu: {status: "ok", usagePercent: 99, perCore: [99, 99]}});
  engine.observe("m1", metricsRecord(high, {sequence: 1}), clock);
  await engine.processNotifications();
  let summary = engine.notificationSummary();
  assert.equal(summary.pending, 1);
  assert.equal(summary.failed, 0);
  assert.equal(summary.lastError, "HTTP 500");
  assert.equal(summary.recent[0]!.attempts, 1);

  clock += 4_000;
  await engine.processNotifications();
  assert.equal(calls.length, 1, "retry waits for the persisted backoff");
  clock += 2_000;
  await engine.processNotifications();
  summary = engine.notificationSummary();
  assert.equal(summary.failed, 1);
  assert.equal(summary.recent[0]!.status, "failed");
  assert.equal(summary.recent[0]!.attempts, 2);
  assert.equal(summary.recent[0]!.lastError, "HTTP 500");

  // A restart must not retry a permanently failed job as if it were new.
  engine.stop();
  database.close();
});
