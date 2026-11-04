/**
 * Multi-hub process-detail scheduling.
 *
 * A hub's browser viewer creates a bounded, expiring detail subscription.
 * Every destination (hub connection) has its own local sharing policy. This
 * scheduler is the single place that combines compatible subscriptions:
 *
 * - one scan serves all destinations at the fastest locally permitted cadence
 *   (`max(minIntervalMs, min(active intervals))`),
 * - each subscription receives results no more often than its own interval,
 * - the scan reads command lines when *any* active destination may see them,
 *   and each destination receives rows rebuilt for its own policy, so one
 *   hub's argument permission can never leak into another hub's result,
 * - a policy change is checked again immediately before every send,
 * - leases run on a monotonic clock; expiry, cancellation, hub disconnect,
 *   and policy withdrawal stop work promptly, and the timer is cleared when
 *   the last subscription disappears,
 * - scans never overlap. A scan that fails retries on the next cadence and
 *   only ends subscriptions after three consecutive failures.
 *
 * The hub is responsible for combining viewer interests into subscription
 * ids and for renewing leases; this module intentionally does not know about
 * browsers.
 */
import {
  MIN_DETAIL_INTERVAL_MS,
  MIN_DETAIL_LEASE_MS,
  type DetailCancel,
  type DetailEnded,
  type DetailResult,
  type DetailSubscribe,
  type ProcessSnapshot,
} from '../../src/contracts';

export type DetailPolicy = { processes: boolean; processArguments: boolean };

export type DetailScanner = {
  scan(options: { includeCommand: boolean }): Promise<ProcessSnapshot>;
};

export type DetailSchedulerOptions = {
  scanner: DetailScanner;
  /** Local, per-hub policy. Called at subscribe time and again before every send. */
  policyFor: (destination: number) => DetailPolicy;
  /** Deliver one message to a destination; return false when the socket cannot take it now. */
  send: (destination: number, message: DetailResult | DetailEnded) => boolean;
  /** Local scan floor; default 1000 ms. */
  minIntervalMs?: number;
  /** Per-hub subscription budget; default 8. */
  maxSubscriptionsPerDestination?: number;
  /** Global subscription budget; default 32. */
  maxSubscriptions?: number;
  /** Monotonic clock for leases and cadence; default `performance.now`. */
  monotonicMs?: () => number;
  /** Wall clock for `capturedAt`; default `Date.now`. */
  now?: () => number;
  /** Injectable timers for deterministic tests. */
  setTimer?: (callback: () => void, delayMs: number) => unknown;
  clearTimer?: (handle: unknown) => void;
  /** Optional observability hook; errors thrown by it are ignored. */
  onEvent?: (event: DetailSchedulerEvent) => void;
};

export type DetailSchedulerEvent =
  | { type: 'subscribed'; destination: number; subscriptionId: string; renewed: boolean }
  | { type: 'ended'; destination: number; subscriptionId: string; reason: DetailEnded['reason'] }
  | { type: 'scan'; scans: number; subscriptions: number; durationMs: number; error: string | null }
  | { type: 'send-skipped'; destination: number; subscriptionId: string };

export type DetailSchedulerStats = {
  active: number;
  byDestination: Record<number, number>;
  scanning: boolean;
  /** Scan attempts started (including failed attempts). */
  scans: number;
  /** Monotonic timestamp of the last scan start; null before the first scan. */
  lastScanMs: number | null;
  /** Duration of the last scan; null before the first completed attempt. */
  lastScanDurationMs: number | null;
};

type Subscription = {
  destination: number;
  subscriptionId: string;
  /** Internal map key; ids are only unique within one destination. */
  key: string;
  intervalMs: number;
  leaseMs: number;
  expiresAt: number;
  lastSentAt: number | null;
  failures: number;
};

const DEFAULT_MIN_INTERVAL_MS = 1000;
const DEFAULT_MAX_PER_DESTINATION = 8;
const DEFAULT_MAX_SUBSCRIPTIONS = 32;
const FAILURE_THRESHOLD = 3;

const defaultSetTimer = (callback: () => void, delayMs: number): unknown => setTimeout(callback, delayMs);
const defaultClearTimer = (handle: unknown): void => clearTimeout(handle as ReturnType<typeof setTimeout>);

export class DetailScheduler {
  private readonly scanner: DetailScanner;
  private readonly policyFor: (destination: number) => DetailPolicy;
  private readonly send: (destination: number, message: DetailResult | DetailEnded) => boolean;
  private readonly minIntervalMs: number;
  private readonly maxSubscriptionsPerDestination: number;
  private readonly maxSubscriptions: number;
  private readonly monotonicMs: () => number;
  private readonly now: () => number;
  private readonly setTimer: (callback: () => void, delayMs: number) => unknown;
  private readonly clearTimer: (handle: unknown) => void;
  private readonly onEvent: ((event: DetailSchedulerEvent) => void) | null;

  private readonly subscriptions = new Map<string, Subscription>();
  private timer: { handle: unknown; at: number } | null = null;
  private stopped = false;
  private scanning = false;
  private scans = 0;
  private lastScanMs: number | null = null;
  private lastScanDurationMs: number | null = null;
  /** Start time used only for cadence; reset when the scheduler goes idle. */
  private scanBaselineMs: number | null = null;
  private nextScanAt: number | null = null;

  constructor(options: DetailSchedulerOptions) {
    this.scanner = options.scanner;
    this.policyFor = options.policyFor;
    this.send = options.send;
    this.minIntervalMs = nonNegativeFinite(options.minIntervalMs, DEFAULT_MIN_INTERVAL_MS);
    this.maxSubscriptionsPerDestination = budget(options.maxSubscriptionsPerDestination, DEFAULT_MAX_PER_DESTINATION);
    this.maxSubscriptions = budget(options.maxSubscriptions, DEFAULT_MAX_SUBSCRIPTIONS);
    this.monotonicMs = options.monotonicMs ?? (() => performance.now());
    this.now = options.now ?? (() => Date.now());
    this.setTimer = options.setTimer ?? defaultSetTimer;
    this.clearTimer = options.clearTimer ?? defaultClearTimer;
    this.onEvent = options.onEvent ?? null;
  }

  handle(destination: number, message: DetailSubscribe | DetailCancel): void {
    if (this.stopped) return;
    if (message.type === 'detail.subscribe') {
      this.subscribe(destination, message);
    } else {
      this.cancel(destination, message.subscriptionId);
    }
  }

  /** Hub connection closed: drop its subscriptions immediately, without sending. */
  cancelDestination(destination: number): void {
    if (this.stopped) return;
    let changed = false;
    for (const subscription of [...this.subscriptions.values()]) {
      if (subscription.destination !== destination) continue;
      changed = this.remove(subscription) || changed;
      this.emit({ type: 'ended', destination, subscriptionId: subscription.subscriptionId, reason: 'cancelled' });
    }
    if (changed) this.schedule();
  }

  stop(): void {
    this.stopped = true;
    this.clearScheduledTimer();
    for (const subscription of this.subscriptions.values()) {
      this.emit({ type: 'ended', destination: subscription.destination, subscriptionId: subscription.subscriptionId, reason: 'cancelled' });
    }
    this.subscriptions.clear();
    this.scanBaselineMs = null;
    this.nextScanAt = null;
  }

  stats(): DetailSchedulerStats {
    const byDestination: Record<number, number> = {};
    for (const subscription of this.subscriptions.values()) {
      byDestination[subscription.destination] = (byDestination[subscription.destination] ?? 0) + 1;
    }
    return {
      active: this.subscriptions.size,
      byDestination,
      scanning: this.scanning,
      scans: this.scans,
      lastScanMs: this.lastScanMs,
      lastScanDurationMs: this.lastScanDurationMs,
    };
  }

  private subscribe(destination: number, message: DetailSubscribe): void {
    const policy = this.policy(destination);
    const key = subscriptionKey(destination, message.subscriptionId);
    const existing = this.subscriptions.get(key);

    if (!policy.processes) {
      // A hub that may not see processes must not keep any previously
      // granted subscription alive after its local sharing policy was
      // reduced. Drop the whole destination, not just the new id.
      let dropped = false;
      for (const subscription of [...this.subscriptions.values()]) {
        if (subscription.destination !== destination) continue;
        if (this.remove(subscription)) {
          dropped = true;
          this.end(subscription, 'withheld', true);
        }
      }
      if (!existing) this.sendEnded(destination, message.subscriptionId, 'withheld');
      if (dropped) this.schedule();
      return;
    }

    if (!existing) {
      let forDestination = 0;
      for (const subscription of this.subscriptions.values()) {
        if (subscription.destination === destination) forDestination++;
      }
      if (this.subscriptions.size >= this.maxSubscriptions || forDestination >= this.maxSubscriptionsPerDestination) {
        this.sendEnded(destination, message.subscriptionId, 'busy');
        return;
      }
    }

    const intervalMs = positiveFinite(message.intervalMs, MIN_DETAIL_INTERVAL_MS);
    const leaseMs = positiveFinite(message.leaseMs, MIN_DETAIL_LEASE_MS);
    const expiresAt = this.monotonicMs() + leaseMs;

    if (existing) {
      // Create-or-renew: renewing extends the lease and may adjust cadence,
      // but it must not bypass the destination's own delivery interval.
      existing.intervalMs = intervalMs;
      existing.leaseMs = leaseMs;
      existing.expiresAt = expiresAt;
      this.emit({ type: 'subscribed', destination, subscriptionId: message.subscriptionId, renewed: true });
    } else {
      this.subscriptions.set(key, {
        destination,
        subscriptionId: message.subscriptionId,
        key,
        intervalMs,
        leaseMs,
        expiresAt,
        lastSentAt: null,
        failures: 0,
      });
      this.emit({ type: 'subscribed', destination, subscriptionId: message.subscriptionId, renewed: false });
    }
    this.schedule();
  }

  private cancel(destination: number, subscriptionId: string): void {
    const subscription = this.subscriptions.get(subscriptionKey(destination, subscriptionId));
    if (!subscription) return;
    this.remove(subscription);
    this.end(subscription, 'cancelled', true);
    this.schedule();
  }

  /**
   * Remove a subscription and send the terminal acknowledgement. `remove`
   * must have happened first so a send callback cannot re-enter into a stale
   * active subscription.
   */
  private end(subscription: Subscription, reason: DetailEnded['reason'], notify: boolean): void {
    if (notify) this.sendEnded(subscription.destination, subscription.subscriptionId, reason);
    this.emit({ type: 'ended', destination: subscription.destination, subscriptionId: subscription.subscriptionId, reason });
  }

  private sendEnded(destination: number, subscriptionId: string, reason: DetailEnded['reason']): void {
    this.safeSend(destination, { type: 'detail.ended', subscriptionId, reason });
  }

  private safeSend(destination: number, message: DetailResult | DetailEnded): boolean {
    try {
      return this.send(destination, message) === true;
    } catch {
      return false;
    }
  }

  private remove(subscription: Subscription): boolean {
    if (this.subscriptions.get(subscription.key) !== subscription) return false;
    this.subscriptions.delete(subscription.key);
    return true;
  }

  private policy(destination: number): DetailPolicy {
    try {
      const policy = this.policyFor(destination);
      return { processes: policy.processes === true, processArguments: policy.processArguments === true };
    } catch {
      // A broken policy source fails closed.
      return { processes: false, processArguments: false };
    }
  }

  private cadence(): number | null {
    if (this.subscriptions.size === 0) return null;
    let fastest = Number.POSITIVE_INFINITY;
    for (const subscription of this.subscriptions.values()) {
      if (Number.isFinite(subscription.intervalMs) && subscription.intervalMs > 0) {
        fastest = Math.min(fastest, subscription.intervalMs);
      }
    }
    if (!Number.isFinite(fastest)) return null;
    return Math.max(this.minIntervalMs, fastest);
  }

  private schedule(): void {
    if (this.stopped) return;
    if (this.subscriptions.size === 0) {
      this.clearScheduledTimer();
      this.scanBaselineMs = null;
      this.nextScanAt = null;
      return;
    }
    const cadence = this.cadence();
    if (cadence === null) {
      this.clearScheduledTimer();
      this.nextScanAt = null;
      return;
    }

    const now = this.monotonicMs();
    let nextScan: number;
    if (this.scanBaselineMs === null) {
      // A fresh subscription gets its first result immediately; subsequent
      // ones are paced by the shared cadence.
      nextScan = now;
    } else {
      nextScan = this.scanBaselineMs + cadence;
      // A scan that took longer than the cadence must not queue back-to-back
      // work. Wait one fresh cadence after the overrun was noticed.
      if (nextScan <= now) nextScan = now + cadence;
    }

    let nextExpiry = Number.POSITIVE_INFINITY;
    for (const subscription of this.subscriptions.values()) {
      nextExpiry = Math.min(nextExpiry, subscription.expiresAt);
    }

    const at = Math.min(nextScan, nextExpiry);
    this.nextScanAt = nextScan;
    const delay = Math.max(0, at - now);
    if (this.timer && this.timer.at === at) return;
    this.clearScheduledTimer();
    this.timer = {
      at,
      handle: this.setTimer(() => {
        this.timer = null;
        this.tick();
      }, delay),
    };
  }

  private clearScheduledTimer(): void {
    if (!this.timer) return;
    try {
      this.clearTimer(this.timer.handle);
    } catch {
      // A broken injected timer must not take the scheduler down.
    }
    this.timer = null;
  }

  private tick(): void {
    if (this.stopped) return;
    const now = this.monotonicMs();
    this.expireDue(now);
    if (this.subscriptions.size === 0) {
      this.schedule();
      return;
    }
    if (this.scanning) {
      // The in-flight scan schedules the next tick when it settles.
      return;
    }
    if (this.nextScanAt !== null && now >= this.nextScanAt) {
      this.runScan();
    } else {
      this.schedule();
    }
  }

  private expireDue(now: number): void {
    let changed = false;
    for (const subscription of [...this.subscriptions.values()]) {
      if (now < subscription.expiresAt) continue;
      if (this.remove(subscription)) {
        changed = true;
        this.end(subscription, 'expired', true);
      }
    }
    if (changed && this.subscriptions.size === 0) this.schedule();
  }

  private runScan(): void {
    if (this.scanning || this.stopped) return;
    const started = this.monotonicMs();
    this.scanning = true;
    this.nextScanAt = null;

    // Re-check policy before spending a scan. A destination whose processes
    // became withheld is dropped and acknowledged immediately.
    for (const destination of this.destinations()) {
      if (this.policy(destination).processes) continue;
      for (const subscription of [...this.subscriptions.values()]) {
        if (subscription.destination !== destination) continue;
        if (this.remove(subscription)) this.end(subscription, 'withheld', true);
      }
    }
    if (this.subscriptions.size === 0) {
      this.scanning = false;
      this.schedule();
      return;
    }

    this.scans++;
    this.lastScanMs = started;
    this.scanBaselineMs = started;
    const policies = new Map<number, DetailPolicy>();
    for (const destination of this.destinations()) policies.set(destination, this.policy(destination));
    const includeCommand = [...policies.values()].some((policy) => policy.processes && policy.processArguments);

    void this.performScan(started, policies, includeCommand);
  }

  private async performScan(
    started: number,
    policies: Map<number, DetailPolicy>,
    includeCommand: boolean,
  ): Promise<void> {
    try {
      const snapshot = await this.scanner.scan({ includeCommand });
      const durationMs = Math.max(0, this.monotonicMs() - started);
      this.lastScanDurationMs = durationMs;
      if (this.stopped) return;
      for (const subscription of this.subscriptions.values()) subscription.failures = 0;
      this.deliver(snapshot, includeCommand);
      this.emit({ type: 'scan', scans: this.scans, subscriptions: this.subscriptions.size, durationMs, error: null });
    } catch (error) {
      const durationMs = Math.max(0, this.monotonicMs() - started);
      this.lastScanDurationMs = durationMs;
      if (this.stopped) return;
      const message = error instanceof Error ? error.message : String(error);
      for (const subscription of [...this.subscriptions.values()]) {
        subscription.failures++;
        if (subscription.failures < FAILURE_THRESHOLD) continue;
        if (this.remove(subscription)) this.end(subscription, 'failed', true);
      }
      this.emit({ type: 'scan', scans: this.scans, subscriptions: this.subscriptions.size, durationMs, error: message });
    } finally {
      this.scanning = false;
      this.schedule();
    }
  }

  private deliver(snapshot: ProcessSnapshot, includeCommand: boolean): void {
    // Re-read policy now: sharing policy may have been reduced while the scan
    // was in flight, and the contract says to purge forbidden fields before
    // transmitting.
    for (const destination of this.destinations()) {
      const policy = this.policy(destination);
      const subscriptions = [...this.subscriptions.values()].filter(
        (subscription) => subscription.destination === destination,
      );
      if (subscriptions.length === 0) continue;

      if (!policy.processes) {
        for (const subscription of subscriptions) {
          if (this.remove(subscription)) this.end(subscription, 'withheld', true);
        }
        continue;
      }

      // The scan only has commands when at least one destination allowed
      // them. If this destination just gained permission mid-scan it waits
      // for the next scan instead of receiving invented/absent data as if it
      // had been requested.
      const canIncludeCommand = policy.processArguments && includeCommand;
      const snapshotForDestination: ProcessSnapshot = canIncludeCommand
        ? {
            totalProcesses: snapshot.totalProcesses,
            truncated: snapshot.truncated,
            inaccessible: snapshot.inaccessible,
            processes: snapshot.processes,
          }
        : {
            totalProcesses: snapshot.totalProcesses,
            truncated: snapshot.truncated,
            inaccessible: snapshot.inaccessible,
            // New row objects make stripping a structural guarantee; the
            // shared snapshot is never mutated.
            processes: snapshot.processes.map(withoutCommand),
          };

      for (const subscription of subscriptions) {
        // Policy may have changed between scan start and this send; policies
        // were refreshed above for the whole destination.
        if (this.subscriptions.get(subscription.key) !== subscription) continue;
        const now = this.monotonicMs();
        if (now >= subscription.expiresAt) {
          if (this.remove(subscription)) this.end(subscription, 'expired', true);
          continue;
        }
        if (subscription.lastSentAt !== null && now - subscription.lastSentAt < subscription.intervalMs) {
          continue;
        }
        const result: DetailResult = {
          type: 'detail.result',
          subscriptionId: subscription.subscriptionId,
          capturedAt: new Date(this.now()).toISOString(),
          snapshot: snapshotForDestination,
        };
        if (this.safeSend(subscription.destination, result)) {
          subscription.lastSentAt = now;
        } else {
          // Never mark an unsent result as delivered; the next scan retries.
          this.emit({ type: 'send-skipped', destination: subscription.destination, subscriptionId: subscription.subscriptionId });
        }
      }
    }
  }

  private destinations(): number[] {
    const seen = new Set<number>();
    const out: number[] = [];
    for (const subscription of this.subscriptions.values()) {
      if (seen.has(subscription.destination)) continue;
      seen.add(subscription.destination);
      out.push(subscription.destination);
    }
    return out;
  }

  private emit(event: DetailSchedulerEvent): void {
    if (!this.onEvent) return;
    try {
      this.onEvent(event);
    } catch {
      // Observability must never affect scheduling or delivery.
    }
  }
}

function withoutCommand(row: ProcessSnapshot['processes'][number]): ProcessSnapshot['processes'][number] {
  const { command: _command, ...rest } = row;
  return rest;
}

function subscriptionKey(destination: number, subscriptionId: string): string {
  return `${destination}\u0000${subscriptionId}`;
}

function positiveFinite(value: number | undefined, fallback: number): number {
  return value !== undefined && Number.isFinite(value) && value > 0 ? value : fallback;
}

function nonNegativeFinite(value: number | undefined, fallback: number): number {
  return value !== undefined && Number.isFinite(value) && value >= 0 ? value : fallback;
}

function budget(value: number | undefined, fallback: number): number {
  if (value === undefined || !Number.isFinite(value)) return fallback;
  return Math.max(0, Math.floor(value));
}
