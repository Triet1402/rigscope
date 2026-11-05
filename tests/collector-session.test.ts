/**
 * Unit tests for the durable destination session.
 *
 * Fake sockets and real timers (with generous bounds) exercise the handshake,
 * live-before-replay priority and window reservation, replay throttling,
 * retryAfter/ack-timeout handling, reconnect behavior, detail routing,
 * inventory re-announcement and destination independence.
 */
import test from 'node:test';
import type { TestContext } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';

import { DestinationOutbox } from '../collector/destinations/outbox';
import { OutboxBudget } from '../collector/destinations/budget';
import {
  DestinationSession,
  type DestinationSessionOptions,
} from '../collector/destinations/session';
import type { CollectorSocket, SessionSocketOptions } from '../collector/destinations/session';
import type {
  CapabilityName,
  DetailCancel,
  DetailSubscribe,
  HelloMessage,
  Inventory,
  InventoryMessage,
  MetricsPayload,
  MetricsRecord,
  RecordRef,
  SamplesMessage,
  TelemetryRecord,
  WelcomeMessage,
} from '../src/contracts';

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const ROOT = mkdtempSync(join(tmpdir(), 'rigscope-session-test-'));
process.on('exit', () => rmSync(ROOT, { recursive: true, force: true }));

const URL = 'ws://127.0.0.1:8081/collect';
const TOKEN = 'destination-secret-token';
const IDENTITY = {
  collectorId: 'collector-1',
  hostname: 'host-1',
  runId: 'run-a',
  bootId: 'boot-a',
};
const CAPABILITIES: CapabilityName[] = ['metrics', 'inventory'];
const INTERVAL_MS = 1_000;

const WELCOME: WelcomeMessage = {
  type: 'welcome',
  hubRunId: 'hub-run-1',
  machineId: 'machine-1',
  delivery: 'durable',
  maxInFlightBatches: 8,
};

const METRICS_PAYLOAD: MetricsPayload = {
  cpu: { status: 'ok', usagePercent: 12.5, perCore: [10] },
  memory: { status: 'ok', usedBytes: 1_000, totalBytes: 8_000 },
  swap: { status: 'unavailable', reason: 'no swap' },
  gpus: { status: 'ok', devices: [] },
  disks: { status: 'ok', filesystems: [], devices: [] },
  network: { status: 'ok', interfaces: [] },
};

const INVENTORY: Inventory = {
  os: { platform: 'linux', release: '6.1', distro: null, arch: 'x64' },
  cpu: { model: null, logicalCores: 4 },
  memory: { totalBytes: 8_000, swapTotalBytes: null },
  gpus: [],
  filesystems: [],
  disks: [],
  network: [],
  capabilities: {
    cpu: { status: 'supported' },
    memory: { status: 'supported' },
    swap: { status: 'supported' },
    gpus: { status: 'unsupported' },
    disks: { status: 'supported' },
    network: { status: 'supported' },
    processes: { status: 'supported' },
  },
};

const INVENTORY_MESSAGE: InventoryMessage = {
  type: 'inventory',
  capturedAt: new Date().toISOString(),
  inventory: INVENTORY,
};

let nextSequence = 1;

function metrics(sequence = nextSequence++): MetricsRecord {
  return {
    collectorId: IDENTITY.collectorId,
    runId: IDENTITY.runId,
    bootId: IDENTITY.bootId,
    sequence,
    capturedAt: new Date().toISOString(),
    kind: 'metrics',
    payload: METRICS_PAYLOAD,
  };
}

/** A durable record carrying every optional current metric field. */
function canonicalMetrics(sequence = nextSequence++): MetricsRecord {
  const base = metrics(sequence);
  return {
    ...base,
    payload: {
      ...base.payload,
      cpu: {
        status: 'ok',
        usagePercent: 12.5,
        perCore: [10],
        temperatureC: 61.4,
        powerWatts: 88.2,
        iowaitPercent: 1.25,
        pressureSomePercent: 3.5,
      },
      memory: {
        status: 'ok',
        usedBytes: 1_000,
        totalBytes: 8_000,
        pressureSomePercent: 1.5,
        pressureFullPercent: 0.5,
      },
      disks: {
        status: 'ok',
        filesystems: [
          { id: 'fs:/', status: 'ok', usedBytes: 100, totalBytes: 200, inodesUsed: 10, inodesTotal: 100 },
        ],
        devices: [
          {
            id: 'disk:sda',
            status: 'ok',
            readBytesPerSec: 1,
            writeBytesPerSec: 2,
            readLatencyMs: 1.5,
            writeLatencyMs: 2.5,
            ioUtilizationPercent: 12.5,
          },
        ],
        pressureSomePercent: 2.5,
        pressureFullPercent: 1.5,
      },
    },
  };
}

type Listener = (...args: unknown[]) => void;

class FakeSocket implements CollectorSocket {
  readyState = 0;
  bufferedAmount = 0;
  sent: string[] = [];
  terminated = false;
  closedByStop = false;
  private readonly listeners = new Map<string, Listener[]>();

  on(event: string, listener: Listener): unknown {
    const bucket = this.listeners.get(event) ?? [];
    bucket.push(listener);
    this.listeners.set(event, bucket);
    return this;
  }

  emit(event: string, ...args: unknown[]): void {
    for (const listener of [...(this.listeners.get(event) ?? [])]) listener(...args);
  }

  send(data: string): void {
    this.sent.push(data);
  }

  close(): void {
    this.closedByStop = true;
  }

  terminate(): void {
    this.terminated = true;
  }

  messages(): Array<Record<string, unknown>> {
    return this.sent.map((line) => JSON.parse(line) as Record<string, unknown>);
  }

  samples(): SamplesMessage[] {
    return this.messages().filter((message) => message.type === 'samples') as unknown as SamplesMessage[];
  }

  ofType(type: string): Array<Record<string, unknown>> {
    return this.messages().filter((message) => message.type === type);
  }

  lastMessage(): Record<string, unknown> {
    const last = this.messages().at(-1);
    assert.ok(last !== undefined, 'expected at least one sent message');
    return last;
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

type HarnessOptions = {
  ackTimeoutMs?: number;
  replayBatchesPerSecond?: number;
  maxLocalInFlightBatches?: number;
  maxBatchBytes?: number;
  backoff?: { baseMs?: number; maxMs?: number };
  rand?: () => number;
  budget?: OutboxBudget;
};

type Harness = {
  session: DestinationSession;
  outbox: DestinationOutbox;
  dir: string;
  sockets: FakeSocket[];
  events: string[];
  details: Array<DetailSubscribe | DetailCancel>;
  welcomes: WelcomeMessage[];
  factoryOptions: SessionSocketOptions[];
  socket: () => FakeSocket;
  welcome: (socket: FakeSocket, overrides?: Partial<WelcomeMessage>) => void;
  ack: (socket: FakeSocket, accepted: RecordRef[], rejected?: Array<RecordRef & { reason: string }>, retryAfterMs?: number) => void;
  disconnects: () => number;
};

function makeHarness(t: TestContext, options: HarnessOptions = {}): Harness {
  const dir = mkdtempSync(join(ROOT, 'session-'));
  let gapSequence = 10_000;
  const outbox = DestinationOutbox.open({
    dir,
    budget: options.budget,
    identity: IDENTITY,
    allocateSequence: () => ++gapSequence,
  });
  const sockets: FakeSocket[] = [];
  const events: string[] = [];
  const details: Array<DetailSubscribe | DetailCancel> = [];
  const welcomes: WelcomeMessage[] = [];
  const factoryOptions: SessionSocketOptions[] = [];
  let disconnectCount = 0;

  const sessionOptions: DestinationSessionOptions = {
    name: 'hub0',
    url: URL,
    token: TOKEN,
    identity: { ...IDENTITY },
    outbox,
    expectedIntervalMs: INTERVAL_MS,
    capabilities: CAPABILITIES,
    socketFactory: (_url, socketOptions) => {
      factoryOptions.push(socketOptions);
      const socket = new FakeSocket();
      sockets.push(socket);
      return socket;
    },
    welcomeTimeoutMs: 1_000,
    idleTimeoutMs: 60_000,
    ackTimeoutMs: options.ackTimeoutMs ?? 30_000,
    replayBatchesPerSecond: options.replayBatchesPerSecond ?? 1_000,
    maxLocalInFlightBatches: options.maxLocalInFlightBatches ?? 8,
    maxBatchBytes: options.maxBatchBytes,
    // Unit tests never want an accidental reconnect unless they opt in.
    backoff: options.backoff ?? { baseMs: 900_000, maxMs: 900_000 },
    rand: options.rand ?? (() => 0.5),
    onEvent: (line) => events.push(line),
    onDetailRequest: (message) => details.push(message),
    onWelcome: (welcome) => welcomes.push(welcome),
    onDisconnect: () => {
      disconnectCount += 1;
    },
  };
  const session = new DestinationSession(sessionOptions);
  session.start();

  t.after(() => {
    session.stop();
    outbox.close();
    rmSync(dir, { recursive: true, force: true });
  });

  const current = (): FakeSocket => {
    assert.ok(sockets.length > 0, 'expected at least one socket');
    return sockets.at(-1)!;
  };

  return {
    session,
    outbox,
    dir,
    sockets,
    events,
    details,
    welcomes,
    factoryOptions,
    socket: current,
    welcome: (socket, overrides = {}) => {
      socket.readyState = 1;
      socket.emit('open');
      socket.emit('message', JSON.stringify({ ...WELCOME, ...overrides }));
      assert.equal(session.describeState().status, 'welcomed');
    },
    ack: (socket, accepted, rejected = [], retryAfterMs) => {
      socket.emit(
        'message',
        JSON.stringify({
          type: 'ack',
          accepted,
          rejected,
          ...(retryAfterMs === undefined ? {} : { retryAfterMs }),
        }),
      );
    },
    disconnects: () => disconnectCount,
  };
}

function refs(records: TelemetryRecord[]): RecordRef[] {
  return records.map(({ runId, sequence }) => ({ runId, sequence }));
}

// ---------------------------------------------------------------------------
// Handshake
// ---------------------------------------------------------------------------

test('opens with a hello and the bearer token only in the header', (t) => {
  const h = makeHarness(t);
  const socket = h.socket();
  socket.readyState = 1;
  socket.emit('open');
  assert.equal(socket.sent.length, 1, 'exactly the hello before the welcome');

  const hello = socket.lastMessage() as unknown as HelloMessage;
  assert.equal(hello.type, 'hello');
  assert.equal(hello.collectorId, IDENTITY.collectorId);
  assert.equal(hello.runId, IDENTITY.runId);
  assert.equal(hello.bootId, IDENTITY.bootId);
  assert.equal(hello.hostname, IDENTITY.hostname);
  assert.deepEqual(hello.capabilities, CAPABILITIES);
  assert.equal(hello.expectedIntervalMs, INTERVAL_MS);
  assert.equal(Object.hasOwn(hello, 'collectorVersion'), false, 'hello carries no compatibility version');
  assert.equal(socket.sent[0]!.includes(TOKEN), false, 'token must never be in a message');

  const factory = h.factoryOptions[0]!;
  assert.deepEqual(factory.headers, { Authorization: `Bearer ${TOKEN}` });
  assert.ok(factory.handshakeTimeoutMs > 0);
  assert.ok(factory.maxPayloadBytes > 0);
});

test('an unrecognized hub is detected once, logged clearly, and data stays in the outbox', (t) => {
  const h = makeHarness(t);
  h.outbox.append(metrics(1));
  const socket = h.socket();
  socket.readyState = 1;
  socket.emit('open');
  socket.emit(
    'message',
    JSON.stringify({
      type: 'welcome',
      hubRunId: 'hub-run-1',
      machineId: 'machine-1',
      delivery: 'ephemeral',
    }),
  );

  assert.ok(
    h.events.some((line) =>
      line.includes('hub sent an unrecognized welcome; the hub and collector builds must match'),
    ),
    'clear incompatible-hub log line',
  );
  assert.equal(socket.terminated, true);
  assert.equal(h.socket().samples().length, 0, 'no telemetry was sent to the unrecognized hub');
  assert.equal(h.outbox.status().pendingRecords, 1, 'pending records survive the failed handshake');
  const state = h.session.describeState();
  assert.equal(state.status, 'connecting');
  assert.equal(state.reconnectScheduled, true);
  assert.equal(state.welcomed, false);
});

test('a protocol-error close before welcome logs the incompatible-hub hint', (t) => {
  const h = makeHarness(t);
  const socket = h.socket();
  socket.readyState = 1;
  socket.emit('open');
  socket.emit('close', 1002, 'protocol error');
  assert.ok(
    h.events.some((line) =>
      line.includes('hub sent an unrecognized welcome; the hub and collector builds must match'),
    ),
  );
  assert.equal(h.session.describeState().status, 'connecting');
});

// ---------------------------------------------------------------------------
// Canonical serialization
// ---------------------------------------------------------------------------

test('live and replay serialize every current metric field exactly as stored', async (t) => {
  const h = makeHarness(t, { replayBatchesPerSecond: 1_000 });
  const first = canonicalMetrics(1);
  const second = canonicalMetrics(2);
  h.outbox.append(first);
  h.outbox.append(second);

  const socket = h.socket();
  h.welcome(socket);
  await sleep(30);

  const samples = socket.samples();
  assert.ok(samples.some((message) => message.mode === 'live'), 'live path was exercised');
  assert.ok(samples.some((message) => message.mode === 'replay'), 'replay path was exercised');
  const sent = samples.flatMap((message) => message.records as MetricsRecord[]);
  const bySequence = new Map(sent.map((record) => [record.sequence, record]));
  assert.deepEqual(bySequence.get(1), first);
  assert.deepEqual(bySequence.get(2), second);

  // There is no projection even at rest: the outbox keeps the exact record.
  const db = new DatabaseSync(join(h.dir, 'outbox.sqlite'), { readOnly: true });
  try {
    const rows = db.prepare('SELECT sequence, json FROM outbox_records ORDER BY sequence').all() as Array<{
      sequence: number;
      json: string;
    }>;
    assert.deepEqual(rows.map((row) => row.sequence), [1, 2]);
    const stored = rows.map((row) => JSON.parse(row.json) as MetricsRecord);
    assert.deepEqual(stored[0], first);
    assert.deepEqual(stored[1], second);
  } finally {
    db.close();
  }

  h.ack(socket, sent.map(({ runId, sequence }) => ({ runId, sequence })));
  assert.equal(h.outbox.status().pendingRecords, 0);
  assert.equal(h.outbox.status().inFlightRecords, 0);
});

test('reconnect resends the same canonical record without renegotiation', async (t) => {
  const h = makeHarness(t, {
    backoff: { baseMs: 1, maxMs: 1 },
    replayBatchesPerSecond: 1_000,
  });
  const record = canonicalMetrics(1);
  h.outbox.append(record);

  const first = h.socket();
  h.welcome(first);
  await sleep(20);
  const firstSent = first.samples().flatMap((message) => message.records as MetricsRecord[]);
  assert.deepEqual(firstSent.find((candidate) => candidate.sequence === record.sequence), record);

  first.emit('close');
  await sleep(40);
  assert.ok(h.sockets.length >= 2, 'reconnected');

  const second = h.socket();
  h.welcome(second);
  await sleep(20);
  const secondSent = second.samples().flatMap((message) => message.records as MetricsRecord[]);
  assert.deepEqual(
    secondSent.find((candidate) => candidate.sequence === record.sequence),
    record,
    'the same canonical durable record is resent',
  );
});

// ---------------------------------------------------------------------------
// Priority delivery / window
// ---------------------------------------------------------------------------

test('live is sent before replay and one window slot stays reserved for live', async (t) => {
  const h = makeHarness(t, { replayBatchesPerSecond: 1_000 });
  for (let i = 1; i <= 100; i += 1) h.outbox.append(metrics(i));
  const socket = h.socket();
  h.welcome(socket, { maxInFlightBatches: 4 });
  await sleep(100);

  let samples = socket.samples();
  assert.equal(samples[0]!.mode, 'live');
  assert.deepEqual(refs(samples[0]!.records), [{ runId: 'run-a', sequence: 100 }]);
  const replayMessages = samples.filter((message) => message.mode === 'replay');
  assert.equal(replayMessages.length, 2, 'replay fills window-1 slots');
  assert.ok(replayMessages.every((message) => message.records.length <= 32));
  assert.equal(h.session.describeState().inFlightBatches, 3, 'live + two replay batches');
  assert.equal(h.session.describeState().inFlightWindow, 4);

  // The reserved slot is still free for a fresh record even though replay has
  // a large backlog and the window is otherwise full.
  h.outbox.append(metrics(101));
  assert.equal(h.session.deliverLive(), true);
  const live = socket.lastMessage() as unknown as SamplesMessage;
  assert.equal(live.mode, 'live');
  assert.deepEqual(refs(live.records), [{ runId: 'run-a', sequence: 101 }]);
  assert.equal(h.session.deliverLive(), false, 'window is now full');

  await sleep(20);
  assert.equal(
    socket.samples().filter((message) => message.mode === 'replay').length,
    2,
    'replay cannot consume the live reservation',
  );
});

test('replay is throttled by replayBatchesPerSecond', async (t) => {
  const h = makeHarness(t, { replayBatchesPerSecond: 2 });
  for (let i = 1; i <= 60; i += 1) h.outbox.append(metrics(i));
  const socket = h.socket();
  h.welcome(socket);

  const replayCount = () => socket.samples().filter((message) => message.mode === 'replay').length;
  assert.equal(replayCount(), 1, 'one batch is allowed immediately');
  await sleep(100);
  assert.equal(replayCount(), 1, 'no second batch within the 500ms token interval');
  await sleep(700);
  const after = replayCount();
  assert.ok(after >= 2 && after <= 3, `expected 2..3 replay batches after ~800ms, got ${after}`);
});

// ---------------------------------------------------------------------------
// Ack handling
// ---------------------------------------------------------------------------

test('retryAfterMs releases the batch, pauses replay, and leaves live unaffected', async (t) => {
  const h = makeHarness(t, { replayBatchesPerSecond: 1_000 });
  for (let i = 1; i <= 60; i += 1) h.outbox.append(metrics(i));
  const socket = h.socket();
  h.welcome(socket);
  await sleep(20);

  const firstReplay = socket.samples().find((message) => message.mode === 'replay');
  assert.ok(firstReplay !== undefined);
  const rejected = { ...refs([firstReplay.records[0]!])[0]!, reason: 'busy' };
  const inFlightBefore = h.outbox.status().inFlightRecords;
  h.ack(socket, [], [rejected], 300);
  assert.equal(
    h.outbox.status().inFlightRecords,
    inFlightBefore - firstReplay.records.length,
    'the affected batch was released (one rejected record was removed)',
  );

  const replayBefore = socket.samples().filter((message) => message.mode === 'replay').length;
  await sleep(100);
  assert.equal(
    socket.samples().filter((message) => message.mode === 'replay').length,
    replayBefore,
    'replay stays paused',
  );

  // Live delivery is not paused by retryAfterMs.
  h.outbox.append(metrics(61));
  assert.equal(h.session.deliverLive(), true);
  const live = socket.lastMessage() as unknown as SamplesMessage;
  assert.equal(live.mode, 'live');
  assert.deepEqual(refs(live.records), [{ runId: 'run-a', sequence: 61 }]);

  await sleep(300);
  assert.ok(
    socket.samples().filter((message) => message.mode === 'replay').length > replayBefore,
    'replay resumes after the retry delay',
  );
});

test('a lost ack times out, resends the same refs, and duplicate acks are harmless', async (t) => {
  const h = makeHarness(t, { ackTimeoutMs: 50 });
  h.outbox.append(metrics(1));
  const socket = h.socket();
  h.welcome(socket);

  const first = socket.samples()[0]!;
  assert.equal(first.mode, 'live');
  await sleep(150);
  const samples = socket.samples();
  assert.ok(samples.length >= 2, 'the unacknowledged record was resent');
  const resent = samples.at(-1)!;
  assert.deepEqual(refs(resent.records), refs(first.records));

  const ref = refs(first.records)[0]!;
  h.ack(socket, [ref]);
  assert.equal(h.outbox.status().pendingRecords, 0);
  assert.equal(h.outbox.status().inFlightRecords, 0);
  assert.ok(h.outbox.status().lastAckAt !== null);

  assert.doesNotThrow(() => h.ack(socket, [ref]));
  assert.equal(h.outbox.status().pendingRecords, 0);
});

test('disconnect releases in-flight records and notifies the caller', (t) => {
  const h = makeHarness(t);
  h.outbox.append(metrics(1));
  const socket = h.socket();
  h.welcome(socket);
  assert.equal(h.session.describeState().inFlightBatches, 1);

  socket.emit('close');
  assert.equal(h.disconnects(), 1);
  assert.equal(h.session.describeState().inFlightBatches, 0);
  assert.equal(h.outbox.status().inFlightRecords, 0, 'released for the next connection');
  assert.equal(h.outbox.status().pendingRecords, 1);
  assert.equal(h.session.describeState().status, 'connecting');
});

// ---------------------------------------------------------------------------
// Detail and inventory
// ---------------------------------------------------------------------------

test('detail subscribe/cancel requests are routed to the caller', (t) => {
  const h = makeHarness(t);
  const socket = h.socket();
  h.welcome(socket);

  const subscribe: DetailSubscribe = {
    type: 'detail.subscribe',
    subscriptionId: 'sub-1',
    kind: 'processes',
    intervalMs: 1_000,
    leaseMs: 10_000,
  };
  socket.emit('message', JSON.stringify(subscribe));
  const cancel: DetailCancel = { type: 'detail.cancel', subscriptionId: 'sub-1' };
  socket.emit('message', JSON.stringify(cancel));

  assert.deepEqual(h.details, [subscribe, cancel]);
  assert.equal(h.events.some((line) => line.includes('invalid hub message')), false);
});

test('inventory is remembered and re-announced after every welcome', async (t) => {
  const h = makeHarness(t, { backoff: { baseMs: 1, maxMs: 1 } });
  const first = h.socket();
  assert.equal(h.session.sendInventory(INVENTORY_MESSAGE), false, 'not welcomed yet, but remembered');
  h.welcome(first);
  assert.equal(first.ofType('inventory').length, 1, 'sent automatically on welcome');
  assert.equal(h.session.sendInventory(INVENTORY_MESSAGE), true);

  first.emit('close');
  await sleep(40);
  assert.ok(h.sockets.length >= 2, 'reconnected with backoff');
  const second = h.socket();
  assert.notEqual(second, first);
  h.welcome(second);
  const resent = second.ofType('inventory');
  assert.equal(resent.length, 1, 'inventory is re-sent after reconnect');
  assert.deepEqual(resent[0]!.inventory, INVENTORY);
});

// ---------------------------------------------------------------------------
// Independence
// ---------------------------------------------------------------------------

test('a never-acking destination does not block a healthy destination sharing the budget', async (t) => {
  const budget = new OutboxBudget(10 * 1024 * 1024);
  const slow = makeHarness(t, { budget, replayBatchesPerSecond: 1_000 });
  const healthy = makeHarness(t, { budget, replayBatchesPerSecond: 1_000 });
  for (let i = 1; i <= 400; i += 1) slow.outbox.append(metrics(i));
  for (let i = 1; i <= 20; i += 1) healthy.outbox.append(metrics(i));

  slow.welcome(slow.socket());
  healthy.welcome(healthy.socket());
  await sleep(30);

  const healthySocket = healthy.socket();
  const accepted = healthySocket.samples().flatMap((message) => refs(message.records));
  healthy.ack(healthySocket, accepted);
  await sleep(30);

  assert.equal(healthy.outbox.status().pendingRecords, 0, 'healthy destination drains completely');
  assert.ok(healthy.outbox.status().lastAckAt !== null);
  assert.equal(healthy.session.describeState().status, 'welcomed');

  const slowState = slow.session.describeState();
  assert.equal(slowState.status, 'welcomed');
  assert.ok(slowState.inFlightBatches <= slowState.inFlightWindow, 'slow destination stays bounded');
  assert.ok(slow.outbox.status().pendingRecords > 0, 'slow destination still has a backlog');
  assert.ok(
    slow.outbox.status().inFlightRecords <= 32 * slowState.inFlightWindow,
    'the slow destination holds only its bounded in-flight records',
  );

  // The shared budget was never forced to evict the healthy destination.
  assert.equal(budget.overBy(), 0);
});
