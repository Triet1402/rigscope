/**
 * Unit tests for the per-Hub connection interface (owned by the collector slice).
 *
 * Each HubConnection owns sharing policy, delivery cadence, recovered-queue
 * sanitation, inventory refresh and durable append-before-send submission.
 * These tests exercise that whole interface through the existing socket seam
 * (FakeSocket) with real temporary outboxes on disk. The hub side, the
 * outbox's internal behavior and the session's reconnect/ack mechanics keep
 * their dedicated suites.
 */
import test from 'node:test';
import type { TestContext } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import type { DestinationEndpoint } from '../collector/config';
import { DestinationOutbox, outboxDirFor } from '../collector/destinations/outbox';
import { OutboxBudget } from '../collector/destinations/budget';
import { HubConnection, type SharedSampler } from '../collector/destinations/connection';
import type { QueueMode } from '../collector/destinations/queue';
import type { SharingInput } from '../collector/destinations/policy';
import type {
  CollectionFailure,
  DetailCancel,
  DetailEnded,
  DetailSubscribe,
  Inventory,
  InventoryMessage,
  MetricsPayload,
  MetricsRecord,
  SamplesMessage,
  StatusMessage,
  TelemetryRecord,
  WelcomeMessage,
} from '../src/contracts';

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const ROOT = mkdtempSync(join(tmpdir(), 'rigscope-connection-test-'));
process.on('exit', () => rmSync(ROOT, { recursive: true, force: true }));

const HUB_URL = 'ws://127.0.0.1:8081/api/collector/connect';
const TOKEN = 'destination-secret-token';
const IDENTITY = {
  collectorId: 'collector-1',
  hostname: 'host-1',
  runId: 'run-a',
  bootId: 'boot-a',
};

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
  network: { status: 'ok', interfaces: [{ id: 'eth0', status: 'ok', rxBytesPerSec: 5, txBytesPerSec: 6 }] },
};

const INVENTORY: Inventory = {
  os: { platform: 'linux', release: '6.1', distro: null, arch: 'x64' },
  cpu: { model: 'cpu-model', logicalCores: 4 },
  memory: { totalBytes: 8_000, swapTotalBytes: null },
  gpus: [],
  filesystems: [],
  disks: [],
  network: [{ id: 'eth0', name: 'eth0', mac: 'aa:00', addresses: [{ family: 'ipv4', address: '10.0.0.2', prefix: 24 }], virtual: false, state: 'up', speedMbps: null }],
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

const WITHHELD = { status: 'unavailable', reason: 'withheld by local sharing policy' };

function iso(sequence: number): string {
  return new Date(Date.parse('2026-03-01T00:00:00.000Z') + sequence).toISOString();
}

function metrics(sequence: number): MetricsRecord {
  return {
    collectorId: IDENTITY.collectorId,
    runId: IDENTITY.runId,
    bootId: IDENTITY.bootId,
    sequence,
    capturedAt: iso(sequence),
    kind: 'metrics',
    payload: METRICS_PAYLOAD,
  };
}

type Listener = (...args: unknown[]) => void;

class FakeSocket {
  readyState = 0;
  bufferedAmount = 0;
  sent: string[] = [];
  terminated = false;
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

  close(): void {}

  terminate(): void {
    this.terminated = true;
  }

  messages(): Array<Record<string, unknown>> {
    return this.sent.map((line) => JSON.parse(line) as Record<string, unknown>);
  }
}

function seedOutbox(stateDir: string, url: string, records: MetricsRecord[]): void {
  let filler = 9_000;
  const outbox = DestinationOutbox.open({
    dir: outboxDirFor(stateDir, url),
    identity: { ...IDENTITY },
    allocateSequence: () => ++filler,
  });
  for (const record of records) outbox.append(record);
  outbox.close();
}

type HarnessOptions = {
  sharing?: SharingInput;
  destinationIntervalMs?: number;
  baseIntervalMs?: number;
  budget?: OutboxBudget;
  backoff?: { baseMs?: number; maxMs?: number };
  stateDir?: string;
  /** The durable suite is the default so existing assertions stay durable-specific. */
  queueMode?: QueueMode;
};

type Harness = {
  connection: HubConnection;
  stateDir: string;
  sockets: FakeSocket[];
  events: string[];
  details: {
    handled: Array<DetailSubscribe | DetailCancel>;
    disconnections: number;
  };
  samplerState: {
    inventory: Inventory;
    inventoryVersion: number;
    failures: CollectionFailure[];
  };
  socket: () => FakeSocket;
  welcome: (socket: FakeSocket) => void;
  samplesOf: (socket: FakeSocket) => SamplesMessage[];
  messagesOfType: (socket: FakeSocket, type: string) => Array<Record<string, unknown>>;
};

function makeHarness(t: TestContext, options: HarnessOptions = {}): Harness {
  const stateDir = options.stateDir ?? mkdtempSync(join(ROOT, 'state-'));
  let gapSequence = 10_000;
  const sockets: FakeSocket[] = [];
  const events: string[] = [];
  const details = {
    handled: [] as Array<DetailSubscribe | DetailCancel>,
    disconnections: 0,
  };
  const samplerState = {
    inventory: INVENTORY,
    inventoryVersion: 1,
    failures: [] as CollectionFailure[],
  };
  const sampler: SharedSampler = {
    inventory: () => samplerState.inventory,
    inventoryVersion: () => samplerState.inventoryVersion,
    failures: () => samplerState.failures,
  };

  const endpoint: DestinationEndpoint = {
    url: HUB_URL,
    token: TOKEN,
    ...(options.sharing === undefined ? {} : { sharing: options.sharing }),
    ...(options.destinationIntervalMs === undefined ? {} : { intervalMs: options.destinationIntervalMs }),
  };
  const connection = HubConnection.open({
    index: 0,
    endpoint,
    baseIntervalMs: options.baseIntervalMs ?? 1_000,
    stateDir,
    queueMode: options.queueMode ?? 'durable',
    identity: { ...IDENTITY },
    budget: options.budget ?? new OutboxBudget(),
    allocateSequence: () => ++gapSequence,
    sampler,
    socketFactory: () => {
      const socket = new FakeSocket();
      sockets.push(socket);
      return socket;
    },
    // Unit tests never want an accidental reconnect unless they opt in.
    backoff: options.backoff ?? { baseMs: 900_000, maxMs: 900_000 },
    rand: () => 0.5,
    onEvent: (line) => events.push(line),
  });
  connection.attachDetails({
    handle: (message) => details.handled.push(message),
    connectionDisconnected: () => {
      details.disconnections += 1;
    },
  });
  connection.start();

  t.after(() => {
    connection.stop();
    rmSync(stateDir, { recursive: true, force: true });
  });

  return {
    connection,
    stateDir,
    sockets,
    events,
    details,
    samplerState,
    socket: () => {
      assert.ok(sockets.length > 0, 'expected at least one socket');
      return sockets.at(-1)!;
    },
    welcome: (socket) => {
      socket.readyState = 1;
      socket.emit('open');
      socket.emit('message', JSON.stringify(WELCOME));
    },
    samplesOf: (socket) =>
      socket.messages().filter((message) => message.type === 'samples') as unknown as SamplesMessage[],
    messagesOfType: (socket, type) => socket.messages().filter((message) => message.type === type),
  };
}

function records(samples: SamplesMessage[]): TelemetryRecord[] {
  return samples.flatMap((message) => message.records as TelemetryRecord[]);
}

function metricRecords(samples: SamplesMessage[]): MetricsRecord[] {
  return records(samples).filter((record): record is MetricsRecord => record.kind === 'metrics');
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// ---------------------------------------------------------------------------
// Sharing policy and durable submission
// ---------------------------------------------------------------------------

test('a fresh sample is policy-filtered, durably queued, then delivered as live', (t) => {
  const h = makeHarness(t, { sharing: { cpu: true, memory: false } });
  h.welcome(h.socket());

  h.connection.acceptSample(metrics(1), 0);

  const samples = h.samplesOf(h.socket());
  assert.equal(samples.length, 1, 'exactly one live delivery');
  const record = samples[0]!.records[0] as MetricsRecord;
  assert.equal(record.payload.cpu.status, 'ok');
  assert.deepEqual(record.payload.memory, WITHHELD, 'forbidden family never leaves the collector');
});

test('the delivery cadence defers under the destination interval but sends at the cutoff', (t) => {
  const h = makeHarness(t, { destinationIntervalMs: 15_000 });
  h.welcome(h.socket());

  h.connection.acceptSample(metrics(1), 0);
  assert.equal(h.samplesOf(h.socket()).length, 1, 'the first tick always sends');

  h.connection.acceptSample(metrics(2), 9_999);
  assert.equal(h.samplesOf(h.socket()).length, 1, 'inside the configured cutoff nothing is queued or sent');

  h.connection.acceptSample(metrics(3), 15_000);
  assert.deepEqual(
    metricRecords(h.samplesOf(h.socket())).map((record) => record.sequence),
    [1, 3],
    'the cutoff tick sends again and deferred samples never entered the durable queue',
  );
});

test('hello advertises the quantized cadence and the capabilities its policy permits', (t) => {
  const h = makeHarness(t, { sharing: { processes: false }, destinationIntervalMs: 450, baseIntervalMs: 200 });
  h.welcome(h.socket());
  const hello = h.socket().messages()[0] as Record<string, unknown>;
  assert.equal(hello.type, 'hello');
  assert.equal(hello.expectedIntervalMs, 600, 'cadence is quantized onto the shared tick grid');
  assert.deepEqual(hello.capabilities, ['metrics', 'inventory'], 'withheld processes are not advertised');
  assert.deepEqual(h.connection.detailPolicy(), { processes: false, processArguments: false });

  const permitted = makeHarness(t);
  permitted.welcome(permitted.socket());
  const otherHello = permitted.socket().messages()[0] as Record<string, unknown>;
  assert.deepEqual(otherHello.capabilities, ['metrics', 'inventory', 'processes']);
  assert.equal(otherHello.expectedIntervalMs, 1_000);
  assert.deepEqual(permitted.connection.detailPolicy(), { processes: true, processArguments: false });
});

// ---------------------------------------------------------------------------
// Recovered queue sanitation and upgrades
// ---------------------------------------------------------------------------

test('recovered pending samples are sanitized by policy and never cadence-filtered away', (t) => {
  const stateDir = mkdtempSync(join(ROOT, 'sanitized-'));
  seedOutbox(stateDir, HUB_URL, [metrics(1), metrics(2), metrics(3)]);
  const h = makeHarness(t, {
    stateDir,
    sharing: { memory: false, network: false },
    destinationIntervalMs: 15_000,
  });

  assert.equal(
    h.connection.describeState().pendingRecords,
    3,
    'sanitation cannot cadence-filter retained samples away',
  );
  h.welcome(h.socket());
  const replayed = metricRecords(h.samplesOf(h.socket()));
  assert.deepEqual(replayed.map((record) => record.sequence), [1, 2, 3]);
  for (const record of replayed) {
    assert.deepEqual(record.payload.memory, WITHHELD);
    assert.deepEqual(record.payload.network, WITHHELD);
    assert.equal(record.payload.cpu.status, 'ok', 'permitted families are untouched');
  }
  assert.equal(h.connection.describeState().pendingRecords, 0, 'everything replays once welcomed');
});

test('a current-endpoint outbox replays retained canonical records with privacy applied', (t) => {
  const stateDir = mkdtempSync(join(ROOT, 'retained-'));
  const retained: MetricsRecord = {
    ...metrics(7),
    payload: {
      ...METRICS_PAYLOAD,
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
    },
  };
  seedOutbox(stateDir, HUB_URL, [retained]);
  const h = makeHarness(t, { stateDir, sharing: { memory: false } });

  h.welcome(h.socket());
  const replayed = metricRecords(h.samplesOf(h.socket()));
  assert.deepEqual(replayed.map((record) => record.sequence), [7]);
  assert.deepEqual(
    replayed[0]!.payload.cpu,
    retained.payload.cpu,
    'every canonical metric field is serialized exactly as stored',
  );
  assert.deepEqual(replayed[0]!.payload.memory, WITHHELD, 'privacy applies to retained records');
});

test('stop preserves pending authorized delivery for the next run', (t) => {
  const h = makeHarness(t, { sharing: { memory: false } });
  h.connection.acceptSample(metrics(1), 0);
  h.connection.acceptSample(metrics(2), 0);
  h.connection.stop();

  const reopened = DestinationOutbox.open({
    dir: outboxDirFor(h.stateDir, HUB_URL),
    identity: { ...IDENTITY },
    allocateSequence: () => 9_000,
  });
  try {
    assert.equal(reopened.status().pendingRecords, 2, 'stopped delivery stays durable');
    const surviving = reopened
      .takeReplay(10, Number.MAX_SAFE_INTEGER)
      .filter((record): record is MetricsRecord => record.kind === 'metrics');
    assert.deepEqual(surviving.map((record) => record.sequence), [1, 2]);
    assert.deepEqual(surviving[0]!.payload.memory, WITHHELD);
  } finally {
    reopened.close();
  }
});

// ---------------------------------------------------------------------------
// Independence
// ---------------------------------------------------------------------------

test('an offline connection stores its share durably without blocking a healthy connection', (t) => {
  const budget = new OutboxBudget();
  const offline = makeHarness(t, { budget });
  const healthy = makeHarness(t, { budget });
  healthy.welcome(healthy.socket());

  offline.connection.acceptSample(metrics(1), 0);
  healthy.connection.acceptSample(metrics(11), 0);

  assert.equal(healthy.samplesOf(healthy.socket()).length, 1, 'the healthy hub received its share');
  assert.equal(healthy.connection.describeState().pendingRecords, 0);
  assert.equal(
    offline.connection.describeState().pendingRecords,
    1,
    'the offline share rests durable without affecting the healthy hub',
  );
});

// ---------------------------------------------------------------------------
// Inventory
// ---------------------------------------------------------------------------

test('inventory announces on every welcome, refreshes on version change, and reconnects fresh', async (t) => {
  const h = makeHarness(t, { sharing: { networkAddresses: false }, backoff: { baseMs: 1, maxMs: 1 } });
  const first = h.socket();
  h.welcome(first);

  const announced = h.messagesOfType(first, 'inventory') as unknown as InventoryMessage[];
  assert.equal(announced.length, 1, 'the first welcome announces the current inventory');
  assert.deepEqual(announced[0]!.inventory.network[0]!.addresses, [], 'addresses are withheld independent of samples');
  assert.equal(announced[0]!.inventory.cpu.logicalCores, 4);

  h.samplerState.inventoryVersion = 2;
  h.samplerState.inventory = { ...INVENTORY, cpu: { ...INVENTORY.cpu, logicalCores: 8 } };
  h.connection.acceptSample(metrics(20), 5_000);
  const refreshed = h.messagesOfType(first, 'inventory') as unknown as InventoryMessage[];
  assert.equal(refreshed.length, 2, 'a version change refreshes the inventory');
  assert.equal(refreshed[1]!.inventory.cpu.logicalCores, 8);

  first.emit('close');
  await sleep(40);
  const second = h.socket();
  assert.notEqual(second, first, 'the connection reconnected');
  h.welcome(second);
  const resent = h.messagesOfType(second, 'inventory') as unknown as InventoryMessage[];
  assert.equal(resent.length, 1, 'reconnect re-announces without a version change');
  assert.equal(resent[0]!.inventory.cpu.logicalCores, 8, 'the re-announced snapshot is current');
});

// ---------------------------------------------------------------------------
// Detail subscriptions and status
// ---------------------------------------------------------------------------

test('detail subscriptions route per connection and cancel on disconnect', (t) => {
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
  assert.deepEqual(h.details.handled, [subscribe], 'inbound subscriptions route cross-hub scheduling');

  const ended: DetailEnded = { type: 'detail.ended', subscriptionId: 'sub-1', reason: 'cancelled' };
  assert.equal(h.connection.sendDetail(ended), true);
  assert.ok(socket.messages().some((message) => message.type === 'detail.ended'));

  socket.emit('close');
  assert.equal(h.details.disconnections, 1, "this connection's subscriptions are cancelled");
  assert.equal(h.connection.sendDetail(ended), false, 'detail stays request-scoped after disconnect');
});

test('the status snapshot reports this connection delivery state and sampler failures', (t) => {
  const h = makeHarness(t);
  h.welcome(h.socket());
  h.samplerState.failures.push({ metric: 'gpu', resourceId: 'gpu:uuid-1', reason: 'driver flapped' });
  h.connection.acceptSample(metrics(40), 0);

  assert.equal(h.connection.refreshStatus(), true);
  const statuses = h.messagesOfType(h.socket(), 'status') as unknown as StatusMessage[];
  assert.equal(statuses.length, 1);
  assert.equal(statuses[0]!.delivery.inFlightRecords, 1, 'the delivered sample is in flight');
  assert.deepEqual(statuses[0]!.failures, h.samplerState.failures);
});
