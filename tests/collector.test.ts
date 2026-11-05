/**
 * Unit tests for the collector runtime surface (owned by the collector slice).
 *
 * Covers configuration parsing/bounds, boot identity and run IDs, the
 * reconnect schedule, and per-destination session behavior via fake sockets:
 * hello wire shape (including advertised cadence cadence bounds), welcome
 * validation, gridlock bounds and destination independence. Hub-side behavior
 * is covered by hub tests and tests/runtime*.integration.ts.
 */
import test from 'node:test';
import assert from 'node:assert/strict';

import {
  CONNECT_PATH,
  ConfigError,
  MAX_DESTINATIONS,
  MOCK_HOSTNAME,
  parseCollectorConfig,
} from '../collector/config';
import {
  BOOT_ID_UNAVAILABLE,
  generateRunId,
  readBootIdentity,
  type BootIdentity,
  type BootIdFileSystem,
} from '../collector/identity';
import { nextReconnectDelayMs } from '../collector/destinations/backoff';
import type { DestinationOutbox } from '../collector/destinations/outbox';
import {
  DEFAULT_WELCOME_TIMEOUT_MS,
  DestinationSession,
  MAX_SOCKET_INBOUND_BYTES,
  type CollectorSocket,
  type SessionSocketOptions,
} from '../collector/destinations/session';
import type { WelcomeMessage } from '../src/contracts';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const GOOD_URL = `ws://127.0.0.1:8081${CONNECT_PATH}`;

function envWithHubs(hubs: unknown = [{url: GOOD_URL, token: 'token-a'}]): {
  [key: string]: string;
} {
  return {COLLECTOR_HUBS: JSON.stringify(hubs)};
}

function parseIssues(env: {readonly [key: string]: string | undefined}): string[] {
  try {
    parseCollectorConfig(env);
    assert.fail('expected ConfigError');
  } catch (error) {
    assert.ok(error instanceof ConfigError, `expected ConfigError, got ${String(error)}`);
    return [...error.issues];
  }
}

const BOOT_ID = '10000000-2000-3000-4000-500000000000';
const HELLO_IDENTITY = {
  collectorId: 'mock-collector',
  hostname: 'mock-machine',
  runId: 'run-1',
  bootId: BOOT_ID,
};

const WELCOME: WelcomeMessage = {
  type: 'welcome',
  hubRunId: 'hub-run-1',
  machineId: 'machine-1',
  delivery: 'durable',
  maxInFlightBatches: 8,
};

/** Minimal in-memory outbox stub; the session only exercises a narrow subset. */
function stubOutbox(): DestinationOutbox {
  return {
    status: () => ({
      pendingRecords: 0, pendingBytes: 0, oldestPendingCapturedAt: null,
      inFlightRecords: 0, replaying: false, lastAckAt: null,
    }),
    takeLive: () => null,
    takeReplay: () => [],
    markInFlight: () => {},
    acknowledge: () => {},
    release: () => {},
    releaseAll: () => {},
    enforceLimits: () => {},
    close: () => {},
  } as unknown as DestinationOutbox;
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
    for (const listener of [...(this.listeners.get(event) ?? [])]) {
      listener(...args);
    }
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

  lastMessage(): unknown {
    const last = this.sent.at(-1);
    assert.ok(typeof last === 'string', 'expected a sent message');
    return JSON.parse(last) as unknown;
  }

  helloBytes(): string {
    assert.equal(this.sent.length, 1, 'expected exactly one message (hello)');
    return this.sent[0]!;
  }
}

type SessionHarness = {
  session: DestinationSession;
  sockets: FakeSocket[];
  events: string[];
  currentSocket: () => FakeSocket;
};

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function makeSession(
  options: Partial<ConstructorParameters<typeof DestinationSession>[0]> = {},
): SessionHarness {
  const sockets: FakeSocket[] = [];
  const events: string[] = [];
  const session = new DestinationSession({
    name: 't0',
    url: GOOD_URL,
    token: 't0-secret-token',
    identity: {...HELLO_IDENTITY},
    outbox: stubOutbox(),
    expectedIntervalMs: 1_000,
    capabilities: ['metrics'],
    socketFactory: () => {
      const socket = new FakeSocket();
      sockets.push(socket);
      return socket;
    },
    welcomeTimeoutMs: options.welcomeTimeoutMs,
    idleTimeoutMs: options.idleTimeoutMs,
    // Prevent any real reconnect attempt inside unit tests.
    backoff: options.backoff ?? {baseMs: 900_000, maxMs: 900_000},
    rand: options.rand ?? (() => 0.5),
    onEvent: (line) => events.push(line),
  });
  session.start();
  return {
    session,
    sockets,
    events,
    currentSocket: () => {
      assert.ok(sockets.length > 0, 'expected at least one socket');
      return sockets[sockets.length - 1]!;
    },
  };
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

test('config: applies documented defaults with a valid hub', () => {
  const config = parseCollectorConfig(envWithHubs());
  assert.equal(config.collectorId, 'mock-collector');
  assert.equal(config.hostname, MOCK_HOSTNAME);
  assert.equal(config.hostnameIsMockDefault, true);
  assert.equal(config.intervalMs, 1000);
  assert.equal(config.destinations.length, 1);
  assert.equal(config.destinations[0]?.url, GOOD_URL);
  assert.equal(config.destinations[0]?.token, 'token-a');
});

test('config: honors explicit overrides', () => {
  const config = parseCollectorConfig({
    COLLECTOR_ID: 'rig-collector',
    COLLECTOR_HOSTNAME: 'rig-a',
    COLLECTOR_INTERVAL_MS: '250',
    COLLECTOR_HUBS: JSON.stringify([
      {url: `wss://hub.example.com${CONNECT_PATH}`, token: 't1'},
      {url: `ws://[::1]:9100${CONNECT_PATH}`, token: 't2'},
    ]),
  });
  assert.equal(config.collectorId, 'rig-collector');
  assert.equal(config.hostname, 'rig-a');
  assert.equal(config.hostnameIsMockDefault, false);
  assert.equal(config.intervalMs, 250);
  assert.equal(config.destinations.length, 2);
  assert.equal(config.destinations[0]?.url, `wss://hub.example.com${CONNECT_PATH}`);
  assert.equal(config.destinations[1]?.url, `ws://[::1]:9100${CONNECT_PATH}`);
});

test('config: COLLECTOR_HUBS is required', () => {
  const issues = parseIssues({});
  assert.equal(issues.filter((issue) => issue.includes('COLLECTOR_HUBS is required')).length, 1);
});

test('config: rejects invalid JSON, non-arrays, empty arrays and oversized destination lists', () => {
  assert.ok(parseIssues({COLLECTOR_HUBS: '{oops'}).some((issue) => issue.includes('not valid JSON')));
  assert.ok(parseIssues({COLLECTOR_HUBS: '{"url":"x"}'}).some((issue) => issue.includes('must be a JSON array')));
  assert.ok(parseIssues({COLLECTOR_HUBS: '[]'}).some((issue) => issue.includes('at least one destination')));
  const tooMany = Array.from({length: MAX_DESTINATIONS + 1}, () => ({url: GOOD_URL, token: 't'}));
  assert.ok(
    parseIssues({COLLECTOR_HUBS: JSON.stringify(tooMany)}).some((issue) =>
      issue.includes(`at most ${MAX_DESTINATIONS} destinations`),
    ),
  );
});

test('config: rejects destination entries without exact {url, token} shape', () => {
  assert.ok(
    parseIssues(envWithHubs([{url: GOOD_URL}])).some((issue) =>
      issue.includes('exactly the fields'),
    ),
    'missing token key must be reported',
  );
  assert.ok(
    parseIssues(envWithHubs([{url: GOOD_URL, token: ''}])).some((issue) =>
      issue.includes('.token must be a non-empty string'),
    ),
    'empty token must be reported',
  );
  assert.ok(
    parseIssues(envWithHubs([GOOD_URL])).some((issue) => issue.includes('must be an object')),
  );
  assert.ok(
    parseIssues(envWithHubs([{url: GOOD_URL, token: 't', extra: 1}])).some((issue) =>
      issue.includes('exactly the fields'),
    ),
  );
  assert.ok(
    parseIssues(envWithHubs([{url: GOOD_URL, token: 'a b'}])).some((issue) =>
      issue.includes('.token must be a non-empty printable-ASCII string'),
    ),
  );
});

test('config: rejects bad schemes and every non-canonical path', () => {
  assert.ok(
    parseIssues(envWithHubs([{url: `http://127.0.0.1:8081${CONNECT_PATH}`, token: 't'}])).some(
      (issue) => issue.includes('scheme must be ws:// or wss://'),
    ),
  );
  for (const path of ['/connect', '/api/collector/v1/connect']) {
    assert.ok(
      parseIssues(envWithHubs([{url: `ws://127.0.0.1:8081${path}`, token: 't'}])).some((issue) =>
        issue.includes(`path must be exactly ${CONNECT_PATH}`),
      ),
      `obsolete path ${path} must be rejected, never normalized`,
    );
  }
});

test('config: rejects credentials embedded in the URL and never echoes the URL', () => {
  const issues = parseIssues(
    envWithHubs([{url: `ws://user:secret-pw@127.0.0.1:8081${CONNECT_PATH}`, token: 't'}]),
  );
  assert.ok(issues.some((issue) => issue.includes('URL must not embed credentials')));
  for (const issue of issues) {
    assert.equal(issue.includes('user:secret-pw'), false, `URL must not be echoed: ${issue}`);
  }
});

test('config: rejects query strings and fragments', () => {
  assert.ok(
    parseIssues(envWithHubs([{url: `ws://127.0.0.1:8081${CONNECT_PATH}?token=x`, token: 't'}])).some(
      (issue) => issue.includes('query string'),
    ),
  );
  assert.ok(
    parseIssues(envWithHubs([{url: `ws://127.0.0.1:8081${CONNECT_PATH}#frag`, token: 't'}])).some(
      (issue) => issue.includes('fragment'),
    ),
  );
});

test('config: plaintext destinations are restricted to literal loopback addresses', () => {
  assert.ok(
    parseIssues(envWithHubs([{url: `ws://192.168.1.10:8081${CONNECT_PATH}`, token: 't'}])).some(
      (issue) => issue.includes('restricted to literal loopback'),
    ),
  );
  assert.ok(
    parseIssues(envWithHubs([{url: `ws://hub.example.com:8081${CONNECT_PATH}`, token: 't'}])).some(
      (issue) => issue.includes('restricted to literal loopback'),
    ),
  );
  assert.doesNotThrow(() =>
    parseCollectorConfig(envWithHubs([{url: `ws://127.0.0.7:8081${CONNECT_PATH}`, token: 't'}])),
  );
  assert.throws(() =>
    parseCollectorConfig(envWithHubs([{url: `ws://localhost:8081${CONNECT_PATH}`, token: 't'}])),
  );
  assert.doesNotThrow(() =>
    parseCollectorConfig(envWithHubs([{url: `ws://[::1]:8081${CONNECT_PATH}`, token: 't'}])),
  );
  assert.doesNotThrow(() =>
    parseCollectorConfig(envWithHubs([{url: `wss://hub.example.com${CONNECT_PATH}`, token: 't'}])),
  );
});

test('config: rejects duplicate destinations', () => {
  const issues = parseIssues(
    envWithHubs([
      {url: GOOD_URL, token: 't1'},
      {url: GOOD_URL, token: 't2'},
    ]),
  );
  assert.ok(issues.some((issue) => issue.includes('duplicate destination URL')));
});

test('config: bounds the interval', () => {
  const hubs = envWithHubs()['COLLECTOR_HUBS']!;
  assert.ok(parseIssues({COLLECTOR_INTERVAL_MS: '50', COLLECTOR_HUBS: hubs}).some((issue) => issue.includes('COLLECTOR_INTERVAL_MS')));
  assert.ok(parseIssues({COLLECTOR_INTERVAL_MS: 'abc', COLLECTOR_HUBS: hubs}).some((issue) => issue.includes('COLLECTOR_INTERVAL_MS')));
  assert.ok(parseIssues({COLLECTOR_INTERVAL_MS: '70000000', COLLECTOR_HUBS: hubs}).some((issue) => issue.includes('COLLECTOR_INTERVAL_MS')));
  assert.equal(parseCollectorConfig({COLLECTOR_INTERVAL_MS: ' ', COLLECTOR_HUBS: hubs}).intervalMs, 1000);
  assert.equal(parseCollectorConfig({COLLECTOR_INTERVAL_MS: '100', COLLECTOR_HUBS: hubs}).intervalMs, 100);
});

test('config: identifiers and hostname are bounded printable strings; blank strings fall back', () => {
  const hubs = envWithHubs()['COLLECTOR_HUBS']!;
  assert.ok(
    parseIssues({COLLECTOR_ID: 'a b', COLLECTOR_HUBS: hubs}).some((issue) => issue.includes('COLLECTOR_ID')),
  );
  assert.ok(
    parseIssues({COLLECTOR_HOSTNAME: 'a\nb', COLLECTOR_HUBS: hubs}).some((issue) =>
      issue.includes('COLLECTOR_HOSTNAME'),
    ),
  );
  assert.ok(
    parseIssues({COLLECTOR_HOSTNAME: 'h'.repeat(300), COLLECTOR_HUBS: hubs}).some((issue) =>
      issue.includes('at most 253 characters'),
    ),
  );
  assert.equal(parseCollectorConfig({COLLECTOR_ID: '  ', COLLECTOR_HUBS: hubs}).collectorId, 'mock-collector');
});

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

test('identity: reads the Linux boot id when present and normalizes it', () => {
  const fsStub: BootIdFileSystem = {
    readFileSync: () => `  ${BOOT_ID.toUpperCase()}\n`,
  };
  const identity: BootIdentity = readBootIdentity(fsStub);
  assert.equal(identity.available, true);
  assert.equal(identity.bootId, BOOT_ID);
});

test('identity: falls back to the unavailable sentinel for junk and unreadable boot ids', () => {
  const junk: BootIdFileSystem = {readFileSync: () => 'not-a-uuid'};
  assert.deepEqual(readBootIdentity(junk), {bootId: BOOT_ID_UNAVAILABLE, available: false});

  const throwing: BootIdFileSystem = {
    readFileSync: () => {
      throw new Error('missing /proc');
    },
  };
  assert.deepEqual(readBootIdentity(throwing), {bootId: BOOT_ID_UNAVAILABLE, available: false});
});

test('identity: run ids are unique per launch', () => {
  const first = generateRunId();
  const second = generateRunId();
  assert.ok(first.length > 0);
  assert.notEqual(first, second);
});

// ---------------------------------------------------------------------------
// Reconnect schedule
// ---------------------------------------------------------------------------

test('backoff: grows exponentially, caps, and applies bounded jitter', () => {
  const mid = nextReconnectDelayMs(4, () => 0.5, {baseMs: 1000, maxMs: 30_000});
  assert.ok(mid >= 14_000 && mid <= 16_000, `expected ~15s (jittered 16s), got ${mid}`);

  const high = nextReconnectDelayMs(40, () => 0.5, {baseMs: 1000, maxMs: 30_000});
  assert.equal(high, 30_000, 'jitter must not exceed the cap');

  const lowJitter = nextReconnectDelayMs(0, () => 0, {baseMs: 1000, maxMs: 30_000});
  assert.equal(lowJitter, 700, 'attempt 0 with low jitter');
  const highJitter = nextReconnectDelayMs(0, () => 1, {baseMs: 1000, maxMs: 30_000});
  assert.equal(highJitter, 1300, 'attempt 0 with high jitter');
  for (let attempts = 0; attempts < 8; attempts += 1) {
    assert.ok(nextReconnectDelayMs(attempts) >= 1);
  }
});

test('session: repeated short welcomes do not reset reconnect backoff', async () => {
  const h = makeSession({backoff: {baseMs: 1, maxMs: 2}});
  try {
    h.currentSocket().emit('error', new Error('first failure'));
    await new Promise(resolve => setTimeout(resolve, 15));
    assert.equal(h.sockets.length, 2);
    h.currentSocket().emit('open');
    h.currentSocket().emit('message', JSON.stringify({
      type: 'welcome', hubRunId: 'hub', machineId: 'machine', delivery: 'durable', maxInFlightBatches: 8,
    }));
    h.currentSocket().emit('close');
    assert.equal(h.session.describeState().attempts, 2);
  } finally { h.session.stop(); }
});

test('session: silent welcomed hubs expire and schedule a reconnect', async () => {
  const h = makeSession({idleTimeoutMs: 15});
  try {
    h.currentSocket().emit('open');
    h.currentSocket().emit('message', JSON.stringify({
      type: 'welcome', hubRunId: 'hub', machineId: 'machine', delivery: 'durable', maxInFlightBatches: 8,
    }));
    await new Promise(resolve => setTimeout(resolve, 35));
    assert.equal(h.currentSocket().terminated, true);
    assert.equal(h.session.describeState().reconnectScheduled, true);
  } finally { h.session.stop(); }
});

// ---------------------------------------------------------------------------
// Session: hello and welcome validation
// ---------------------------------------------------------------------------

test('session: sends a bounded hello with token only in the Authorization header', () => {
  const seenOptions: SessionSocketOptions[] = [];
  const seenUrls: string[] = [];
  const sockets: FakeSocket[] = [];
  const session = new DestinationSession({
    name: 't1',
    url: GOOD_URL,
    token: 'secret-bearer-token-xyz',
    identity: {...HELLO_IDENTITY},
    outbox: stubOutbox(),
    expectedIntervalMs: 600,
    capabilities: ['metrics'],
    socketFactory: (url, factoryOptions) => {
      seenUrls.push(url);
      seenOptions.push(factoryOptions);
      const socket = new FakeSocket();
      sockets.push(socket);
      return socket;
    },
    backoff: {baseMs: 900_000, maxMs: 900_000},
  });
  session.start();
  assert.deepEqual(seenUrls, [GOOD_URL]);
  assert.equal(seenOptions.length, 1);
  assert.deepEqual(seenOptions[0]?.headers, {Authorization: 'Bearer secret-bearer-token-xyz'});
  // Both timeouts bounded by the welcome deadline.
  assert.equal(seenOptions[0]?.handshakeTimeoutMs, DEFAULT_WELCOME_TIMEOUT_MS);
  assert.equal(seenOptions[0]?.maxPayloadBytes, MAX_SOCKET_INBOUND_BYTES);

  const socket = sockets[0]!;
  socket.emit('open');
  assert.deepEqual(JSON.parse(socket.helloBytes()!), {
    type: 'hello',
    collectorId: 'mock-collector',
    runId: 'run-1',
    bootId: BOOT_ID,
    hostname: 'mock-machine',
    capabilities: ['metrics'],
    expectedIntervalMs: 600,
  });
});

test('session: accepts a valid welcome (also delivered as Buffer) and reports it', () => {
  const harness = makeSession();
  const socket = harness.currentSocket();
  socket.emit('open');
  socket.emit('message', Buffer.from(JSON.stringify(WELCOME), 'utf8'));
  assert.equal(harness.session.describeState().status, 'welcomed');
  assert.ok(
    harness.events.some((line) => line.includes('machineId=machine-1')),
    `welcome should be logged, got ${JSON.stringify(harness.events)}`,
  );
});

test('session: a malformed welcome is rejected and rescheduled', async () => {
  for (const bad of [
    {...WELCOME, delivery: 'ephemeral'},
    {...WELCOME, spool: true}, // unexpected field
    'not json at all',
  ]) {
    const harness = makeSession();
    const socket = harness.currentSocket();
    socket.emit('open');
    socket.emit('message', typeof bad === 'string' ? bad : JSON.stringify(bad));
    assert.equal(harness.session.describeState().status, 'connecting');
    assert.equal(socket.terminated, true, 'invalid welcome must terminate the socket');
    assert.equal(harness.session.describeState().reconnectScheduled, true);
    assert.equal(harness.session.describeState().attempts, 1);
    await harness.session.stop();
  }
});

test('session: welcome timeout terminates and schedules a reconnect', async () => {
  const harness = makeSession({welcomeTimeoutMs: 15});
  const socket = harness.currentSocket();
  socket.emit('open');
  assert.equal(harness.session.describeState().welcomeTimerActive, true);
  await sleep(70);
  assert.equal(socket.terminated, true);
  assert.equal(harness.session.describeState().status, 'connecting');
  assert.equal(harness.session.describeState().reconnectScheduled, true);
  await harness.session.stop();
});

// ---------------------------------------------------------------------------
// Hello cadence advertisement
// ---------------------------------------------------------------------------

test('session: hello advertises the per-destination cadence supplied as an option', async () => {
  const {parseCollectorMessage} = await import('../src/contracts');
  const sockets: FakeSocket[] = [];
  const session = new DestinationSession({
    name: 't-adv',
    url: GOOD_URL,
    token: 't-adv-token',
    identity: {...HELLO_IDENTITY},
    outbox: stubOutbox(),
    expectedIntervalMs: 600,
    capabilities: ['metrics'],
    socketFactory: () => {
      const socket = new FakeSocket();
      sockets.push(socket);
      return socket;
    },
    backoff: {baseMs: 900_000, maxMs: 900_000},
  });
  try {
    session.start();
    const socket = sockets[0]!;
    socket.emit('open');
    const hello = JSON.parse(socket.helloBytes()!) as Record<string, unknown>;
    assert.deepEqual(hello, {
      type: 'hello',
      collectorId: HELLO_IDENTITY.collectorId,
      runId: HELLO_IDENTITY.runId,
      bootId: BOOT_ID,
      hostname: HELLO_IDENTITY.hostname,
      capabilities: ['metrics'],
      expectedIntervalMs: 600,
    });
    // What leaves the wire must parse under the strict contract.
    const parsed = parseCollectorMessage(hello);
    assert.equal((parsed as {expectedIntervalMs?: number}).expectedIntervalMs, 600);
    session.stop();
  } finally {
    session.stop();
  }
});

test('session: rejects an unbounded advertised cadence at construction time', () => {
  for (const bad of [99, 7_200_001, Number.NaN, 600.5]) {
    assert.throws(
      () =>
        new DestinationSession({
          name: 't-bad',
          url: GOOD_URL,
          token: 't-bad-token',
          identity: {...HELLO_IDENTITY},
          outbox: stubOutbox(),
          expectedIntervalMs: bad,
          capabilities: ['metrics'],
          socketFactory: () => new FakeSocket(),
        }),
      Error,
    );
  }
});

test('session: rejects empty or duplicate capabilities at construction time', () => {
  for (const capabilities of [[], ['metrics', 'metrics'], ['summary'] as never[]]) {
    assert.throws(
      () =>
        new DestinationSession({
          name: 't-caps',
          url: GOOD_URL,
          token: 't-caps-token',
          identity: {...HELLO_IDENTITY},
          outbox: stubOutbox(),
          expectedIntervalMs: 500,
          capabilities: capabilities as never,
          socketFactory: () => new FakeSocket(),
        }),
      Error,
    );
  }
});
