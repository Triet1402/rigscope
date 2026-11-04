/** Ephemeral, scoped viewer interests. Equivalent viewers share one collector lease. */
import { randomUUID } from 'node:crypto';
import { performance } from 'node:perf_hooks';
import type { DetailEnded, DetailResult, HubMessage } from '../contracts';

const VIEWER_LEASE_MS = 15_000;
const COLLECTOR_LEASE_MS = 20_000;
const INTERVAL_MS = 1_000;

type Interest = { sessionId: string; machineId: string; expiresAt: number };
type MachineInterest = { subscriptionId: string; lastRenewedAt: number; result: DetailResult | null; ended: DetailEnded['reason'] | 'disconnected' | null };

export class ProcessViewerBudgetError extends Error {
  constructor() { super('Process viewer budget exhausted. Close another Processes view and retry.'); }
}

export class ProcessInterests {
  private readonly interests = new Map<string, Interest>();
  private readonly machines = new Map<string, MachineInterest>();
  private readonly timer: NodeJS.Timeout;
  constructor(private readonly send: (machineId: string, msg: HubMessage) => boolean,
    private readonly clock = () => performance.now()) {
    this.timer = setInterval(() => this.sweep(), 1000);
    this.timer.unref();
  }

  renew(sessionId: string, machineId: string, viewerId: string): { leaseMs: number; intervalMs: number } {
    const key = this.key(sessionId, viewerId);
    if (!this.interests.has(key) && (this.interests.size >= 256 ||
      [...this.interests.values()].filter(i => i.sessionId === sessionId).length >= 8)) {
      throw new ProcessViewerBudgetError();
    }
    const previous = this.interests.get(key);
    if (previous && previous.machineId !== machineId) {
      this.interests.delete(key);
      this.releaseUnused(previous.machineId);
    }
    this.interests.set(key, { sessionId, machineId, expiresAt: this.clock() + VIEWER_LEASE_MS });
    if (!this.machines.has(machineId)) this.machines.set(machineId, { subscriptionId: randomUUID(), lastRenewedAt: -Infinity, result: null, ended: null });
    this.renewCollector(machineId);
    return { leaseMs: VIEWER_LEASE_MS, intervalMs: INTERVAL_MS };
  }

  cancel(sessionId: string, viewerId: string): void {
    const key = this.key(sessionId, viewerId);
    const interest = this.interests.get(key);
    this.interests.delete(key);
    if (interest) this.releaseUnused(interest.machineId);
  }

  endSession(sessionId: string): void {
    for (const [key, interest] of this.interests) if (interest.sessionId === sessionId) {
      this.interests.delete(key); this.releaseUnused(interest.machineId);
    }
  }

  read(sessionId: string, machineId: string, viewerId: string): { result: DetailResult | null; state: string } | null {
    this.sweep();
    const interest = this.interests.get(this.key(sessionId, viewerId));
    if (!interest || interest.machineId !== machineId) return null;
    const machine = this.machines.get(machineId)!;
    return { result: machine.result, state: machine.ended ?? (machine.result ? 'ready' : 'loading') };
  }

  receive(machineId: string, msg: DetailResult | DetailEnded): void {
    const machine = this.machines.get(machineId);
    if (!machine || machine.subscriptionId !== msg.subscriptionId) return;
    if (msg.type === 'detail.result') { machine.result = msg; machine.ended = null; }
    else { machine.ended = msg.reason; machine.result = null; }
  }

  disconnect(machineId: string): void {
    const machine = this.machines.get(machineId);
    if (machine) { machine.result = null; machine.ended = 'disconnected'; machine.lastRenewedAt = -Infinity; }
  }

  stop(): void {
    clearInterval(this.timer);
    this.interests.clear();
    for (const id of this.machines.keys()) this.releaseUnused(id);
  }

  private key(sessionId: string, viewerId: string): string { return `${sessionId}\0${viewerId}`; }
  private releaseUnused(machineId: string): void {
    if ([...this.interests.values()].some(i => i.machineId === machineId)) return;
    const machine = this.machines.get(machineId);
    if (machine) this.send(machineId, { type: 'detail.cancel', subscriptionId: machine.subscriptionId });
    this.machines.delete(machineId); // Process snapshots have no persistence or idle cache.
  }
  private renewCollector(machineId: string): void {
    const machine = this.machines.get(machineId)!;
    const now = this.clock();
    if (now - machine.lastRenewedAt < 5000) return;
    if (this.send(machineId, { type: 'detail.subscribe', subscriptionId: machine.subscriptionId,
      kind: 'processes', intervalMs: INTERVAL_MS, leaseMs: COLLECTOR_LEASE_MS })) {
      machine.lastRenewedAt = now;
      if (machine.ended === 'disconnected') machine.ended = null;
    } else machine.ended = 'disconnected';
  }
  private sweep(): void {
    const now = this.clock();
    for (const [key, interest] of this.interests) if (interest.expiresAt <= now) {
      this.interests.delete(key); this.releaseUnused(interest.machineId);
    }
    for (const machineId of this.machines.keys()) this.renewCollector(machineId);
  }
}
