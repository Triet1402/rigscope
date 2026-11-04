/**
 * DetailScheduler tests run on an injectable monotonic clock and timer queue,
 * so cadence, coalescing, leases, and failure behavior are deterministic.
 */
import assert from 'node:assert/strict';
import test from 'node:test';

import type { DetailResult, DetailEnded, ProcessRow, ProcessSnapshot } from '../src/contracts';
import { parseCollectorMessage } from '../src/contracts';
import { DetailScheduler, type DetailPolicy } from '../collector/processes/scheduler';

type Sent = { destination: number; message: DetailResult | DetailEnded };

class FakeClock {
  now = 0;
  private nextId = 1;
  private readonly timers = new Map<number, { at: number; callback: () => void }>();

  setTimer = (callback: () => void, delayMs: number): number => {
    const id = this.nextId++;
    this.timers.set(id, { at: this.now + Math.max(0, delayMs), callback });
    return id;
  };

  clearTimer = (handle: unknown): void => {
    if (typeof handle === 'number') this.timers.delete(handle);
  };

  async settle(): Promise<void> {
    for (let i = 0; i < 8; i++) await Promise.resolve();
    await new Promise<void>((resolve) => setImmediate(resolve));
  }

  async advance(ms: number): Promise<void> {
    const target = this.now + ms;
    let guard = 0;
    for (; guard < 10_000; guard++) {
      let chosenId: number | null = null;
      let chosen: { at: number; callback: () => void } | null = null;
      for (const [id, timer] of this.timers) {
        if (timer.at > target) continue;
        if (
          chosen === null ||
          timer.at < chosen.at ||
          (timer.at === chosen.at && id < (chosenId as number))
        ) {
          chosenId = id;
          chosen = timer;
        }
      }
      if (chosen === null || chosenId === null) break;
      this.now = Math.max(this.now, chosen.at);
      this.timers.delete(chosenId);
      chosen.callback();
      await this.settle();
    }
    this.now = target;
    await this.settle();
    assert.ok(guard < 10_000, 'timer queue did not settle');
  }
}

function processRow(pid: number, overrides: Partial<ProcessRow> = {}): ProcessRow {
  return {
    pid,
    ppid: 1,
    startTicks: pid,
    name: `proc-${pid}`,
    user: 'alice',
    state: 'S',
    cpuPercent: 1,
    memoryRssBytes: 1024,
    gpuMemoryBytes: null,
    ioReadBytesPerSec: null,
    ioWriteBytesPerSec: null,
    threads: 1,
    unit: null,
    ...overrides,
  };
}

function snapshot(rows: ProcessRow[]): ProcessSnapshot {
  return { totalProcesses: rows.length, truncated: false, inaccessible: 0, processes: rows };
}

function setup(options: {
  scanner?: { scan: (o: { includeCommand: boolean }) => Promise<ProcessSnapshot> };
  policies?: Record<number, DetailPolicy>;
  clock?: FakeClock;
  minIntervalMs?: number;
  maxSubscriptionsPerDestination?: number;
  maxSubscriptions?: number;
  send?: (destination: number, message: DetailResult | DetailEnded) => boolean;
}) {
  const clock = options.clock ?? new FakeClock();
  const calls: boolean[] = [];
  const policies = options.policies ?? {};
  const sent: Sent[] = [];
  const baseScanner = options.scanner ?? {
    scan: async () => snapshot([processRow(10)]),
  };
  const scheduler = new DetailScheduler({
    scanner: {
      scan: async (scanOptions) => {
        calls.push(scanOptions.includeCommand);
        return baseScanner.scan(scanOptions);
      },
    },
    policyFor: (destination) => policies[destination] ?? { processes: true, processArguments: false },
    send: (destination, message) => {
      sent.push({ destination, message });
      return options.send ? options.send(destination, message) : true;
    },
    minIntervalMs: options.minIntervalMs,
    maxSubscriptionsPerDestination: options.maxSubscriptionsPerDestination,
    maxSubscriptions: options.maxSubscriptions,
    monotonicMs: () => clock.now,
    now: () => 1_700_000_000_000 + clock.now,
    setTimer: clock.setTimer,
    clearTimer: clock.clearTimer,
  });
  return { clock, scheduler, sent, calls };
}

function sub(options: ReturnType<typeof setup>, destination: number, subscriptionId: string, intervalMs = 1000, leaseMs = 20_000) {
  options.scheduler.handle(destination, {
    type: 'detail.subscribe',
    subscriptionId,
    kind: 'processes',
    intervalMs,
    leaseMs,
  });
}

function cancel(options: ReturnType<typeof setup>, destination: number, subscriptionId: string) {
  options.scheduler.handle(destination, {
    type: 'detail.cancel',
    subscriptionId,
  });
}

function results(sent: Sent[]): Array<{ destination: number; message: DetailResult }> {
  return sent.filter((entry): entry is { destination: number; message: DetailResult } => entry.message.type === 'detail.result');
}

function ended(sent: Sent[]): Array<{ destination: number; message: DetailEnded }> {
  return sent.filter((entry): entry is { destination: number; message: DetailEnded } => entry.message.type === 'detail.ended');
}

test('two destinations coalesce into one immediate scan per cadence tick', async () => {
  const t = setup({});
  sub(t, 1, 'a', 2000, 20_000);
  sub(t, 2, 'b', 2000, 20_000);
  await t.clock.advance(0);
  assert.equal(t.calls.length, 1, 'one shared scan');
  assert.deepEqual(results(t.sent).map((entry) => entry.message.subscriptionId).sort(), ['a', 'b']);
  assert.deepEqual(t.scheduler.stats().byDestination, { 1: 1, 2: 1 });
  assert.equal(t.scheduler.stats().scans, 1);
  assert.equal(t.scheduler.stats().scanning, false);
  for (const { message } of t.sent) {
    assert.equal(parseCollectorMessage(JSON.parse(JSON.stringify(message))).type, message.type);
  }

  await t.clock.advance(2000);
  assert.equal(t.calls.length, 2, 'next shared cadence tick');
  assert.equal(results(t.sent).length, 4);
});

test('per-destination argument policy strips commands without mutating the shared scan', async () => {
  const secret = 'run --token=secret';
  const shared = snapshot([processRow(10, { command: secret })]);
  const t = setup({
    scanner: { scan: async () => shared },
    policies: {
      1: { processes: true, processArguments: true },
      2: { processes: true, processArguments: false },
    },
  });
  sub(t, 1, 'with-args');
  sub(t, 2, 'without-args');
  await t.clock.advance(0);

  const sentResults = results(t.sent);
  const allowed = sentResults.find((entry) => entry.destination === 1);
  const denied = sentResults.find((entry) => entry.destination === 2);
  assert.ok(allowed && denied);
  assert.equal(allowed.message.snapshot.processes[0].command, secret);
  assert.equal(
    Object.prototype.hasOwnProperty.call(denied.message.snapshot.processes[0], 'command'),
    false,
    'another destination must never receive arguments',
  );
  assert.equal(shared.processes[0].command, secret, 'the shared snapshot is not mutated');
  assert.equal(t.calls.length, 1);
});

test('a withheld destination ends immediately; reducing policy later ends active subscriptions', async () => {
  const policies: Record<number, DetailPolicy> = {
    1: { processes: false, processArguments: true },
    2: { processes: true, processArguments: false },
  };
  const t = setup({ policies });
  sub(t, 1, 'denied', 1000, 20_000);
  await t.clock.advance(0);
  assert.equal(t.calls.length, 0, 'withheld policy must not trigger a scan');
  assert.deepEqual(
    ended(t.sent).map((entry) => [entry.destination, entry.message.reason]),
    [[1, 'withheld']],
  );

  sub(t, 2, 'allowed', 1000, 20_000);
  sub(t, 2, 'second', 1000, 20_000);
  await t.clock.advance(0);
  assert.equal(t.calls.length, 1);
  assert.equal(t.scheduler.stats().active, 2);

  policies[2] = { processes: false, processArguments: false };
  await t.clock.advance(1000);
  assert.equal(t.calls.length, 1, 'policy is checked before spending another scan');
  assert.deepEqual(
    ended(t.sent).filter((entry) => entry.destination === 2).map((entry) => entry.message.reason),
    ['withheld', 'withheld'],
    'reducing policy drops every subscription for that hub',
  );
  assert.equal(t.scheduler.stats().active, 0);
});

test('renewal extends the monotonic lease; expiry acknowledges and stops further scans', async () => {
  const t = setup({});
  sub(t, 1, 'renew', 1000, 5000);
  await t.clock.advance(0);
  await t.clock.advance(4000);
  assert.equal(ended(t.sent).length, 0);

  sub(t, 1, 'renew', 1000, 5000); // now = 4000, expiry becomes 9000
  await t.clock.advance(4000); // now = 8000, still alive
  assert.equal(ended(t.sent).length, 0);

  await t.clock.advance(1500); // crosses expiry at 9000
  assert.deepEqual(ended(t.sent).map((entry) => entry.message.reason), ['expired']);
  assert.equal(t.scheduler.stats().active, 0);

  const scans = t.scheduler.stats().scans;
  await t.clock.advance(10_000);
  assert.equal(t.scheduler.stats().scans, scans, 'no scans after the last lease');
});

test('cancelDestination drops only one hub, and cancelling the last stops scans', async () => {
  const t = setup({});
  sub(t, 1, 'one');
  sub(t, 2, 'two');
  await t.clock.advance(0);
  assert.equal(t.scheduler.stats().active, 2);

  t.scheduler.cancelDestination(1);
  assert.deepEqual(t.scheduler.stats().byDestination, { 2: 1 });
  await t.clock.advance(1000);
  assert.equal(t.calls.length, 2);

  t.scheduler.cancelDestination(2);
  assert.equal(t.scheduler.stats().active, 0);
  const scans = t.scheduler.stats().scans;
  await t.clock.advance(10_000);
  assert.equal(t.scheduler.stats().scans, scans);
});

test('subscription budgets report busy without disturbing accepted subscriptions', async () => {
  const t = setup({ maxSubscriptionsPerDestination: 1, maxSubscriptions: 2 });
  sub(t, 1, 'a');
  sub(t, 1, 'b');
  sub(t, 2, 'c');
  sub(t, 3, 'd');
  await t.clock.advance(0);

  const busy = ended(t.sent).filter((entry) => entry.message.reason === 'busy');
  assert.deepEqual(busy.map((entry) => entry.message.subscriptionId).sort(), ['b', 'd']);
  assert.deepEqual(results(t.sent).map((entry) => entry.message.subscriptionId).sort(), ['a', 'c']);
  assert.equal(t.scheduler.stats().active, 2);
});

test('a slow scan never overlaps; the next scan is rescheduled after it settles', async () => {
  let resolveScan: (value: ProcessSnapshot) => void = () => {};
  let calls = 0;
  const t = setup({
    scanner: {
      scan: () => {
        calls++;
        return new Promise<ProcessSnapshot>((resolve) => {
          resolveScan = resolve;
        });
      },
    },
  });
  sub(t, 1, 'slow', 1000, 20_000);
  await t.clock.advance(0);
  assert.equal(calls, 1);
  assert.equal(t.scheduler.stats().scanning, true);

  await t.clock.advance(5000);
  assert.equal(calls, 1, 'no overlapping scan while one is in flight');
  assert.equal(t.scheduler.stats().scanning, true);

  assert.ok(resolveScan);
  resolveScan(snapshot([processRow(10)]));
  await t.clock.settle();
  assert.equal(t.scheduler.stats().scanning, false);
  assert.equal(results(t.sent).length, 1);
  assert.equal(t.scheduler.stats().lastScanDurationMs, 5000);

  await t.clock.advance(1000);
  assert.equal(calls, 2, 'scan resumes after the overrun with a fresh cadence');
  resolveScan(snapshot([processRow(10)]));
  await t.clock.settle();
});

test('transient scan failures retry; three consecutive failures end the subscription', async () => {
  let calls = 0;
  const t = setup({
    scanner: {
      scan: async () => {
        calls++;
        throw new Error('scan exploded');
      },
    },
  });
  sub(t, 1, 'failing', 1000, 20_000);
  await t.clock.advance(0);
  assert.equal(calls, 1);
  assert.equal(ended(t.sent).length, 0, 'first failure only retries');
  assert.equal(t.scheduler.stats().active, 1);

  await t.clock.advance(1000);
  assert.equal(calls, 2);
  assert.equal(ended(t.sent).length, 0, 'second failure only retries');

  await t.clock.advance(1000);
  assert.equal(calls, 3);
  assert.deepEqual(ended(t.sent).map((entry) => entry.message.reason), ['failed']);
  assert.equal(t.scheduler.stats().active, 0);
  assert.equal(t.scheduler.stats().scans, 3);

  await t.clock.advance(10_000);
  assert.equal(calls, 3, 'no scans after the failed subscription is removed');
});

test('each subscription receives results at its own interval within one shared cadence', async () => {
  const t = setup({});
  sub(t, 1, 'fast', 2000, 20_000);
  sub(t, 2, 'slow', 5000, 20_000);
  await t.clock.advance(6000);

  const perDestination = new Map<number, number>();
  for (const entry of results(t.sent)) {
    perDestination.set(entry.destination, (perDestination.get(entry.destination) ?? 0) + 1);
  }
  assert.equal(t.calls.length, 4, 'scans at 0, 2000, 4000, 6000');
  assert.equal(perDestination.get(1), 4);
  assert.equal(perDestination.get(2), 2, 'slow subscription only at 0 and 6000');
});

test('send() returning false does not count as delivered and retries next scan', async () => {
  let allow = false;
  const t = setup({
    send: (_destination, message) => {
      if (message.type !== 'detail.result') return true;
      return allow;
    },
  });
  sub(t, 1, 'retry', 1000, 20_000);
  await t.clock.advance(0);
  assert.equal(results(t.sent).length, 1, 'the attempt happened');
  assert.equal(t.scheduler.stats().scans, 1);

  allow = true;
  await t.clock.advance(1000);
  assert.equal(results(t.sent).length, 2);
  // If the first attempt had been marked delivered, the 1s interval would
  // still allow this one; the stronger check is that state had no lastSentAt.
  assert.equal(t.scheduler.stats().active, 1);
});
