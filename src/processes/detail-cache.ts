import type { DetailResult } from "../contracts";

export type CachedProcessDetail = {
  detail: DetailResult;
  intervalMs: number;
  leaseMs: number;
  lastReceivedMs: number;
};

/** Last-known snapshots belong to one browser login and never go to storage. */
export class ProcessDetailCache {
  private session: string | null = null;
  private entries = new Map<string, CachedProcessDetail>();

  constructor(private readonly maxAgeMs = 5 * 60_000, private readonly limit = 32) {}

  private scope(session: string | null): void {
    if (this.session !== session || session === null) this.entries.clear();
    this.session = session;
  }

  get(machineId: string, session: string | null, now = Date.now()): CachedProcessDetail | undefined {
    this.scope(session);
    const cached = this.entries.get(machineId);
    if (cached && now - cached.lastReceivedMs >= this.maxAgeMs) {
      this.entries.delete(machineId);
      return undefined;
    }
    return cached;
  }

  set(machineId: string, session: string, snapshot: CachedProcessDetail): void {
    this.scope(session);
    this.entries.delete(machineId);
    this.entries.set(machineId, snapshot);
    if (this.entries.size > this.limit) this.entries.delete(this.entries.keys().next().value!);
  }

  delete(machineId: string, session: string): void {
    this.scope(session);
    this.entries.delete(machineId);
  }
}
