/**
 * Memory-only latest state for the hub.
 *
 * Owns, independently of transport:
 * - the collector identity -> machine credential mapping (auth + welcome),
 * - one session per collector identity, with new-run supersession only after
 *   an authenticated hello and no overwrite from replaced (old) sessions,
 * - the per-session advertised delivery cadence (hello expectedIntervalMs):
 *   set from the hello, retained across disconnects, and untouched by
 *   rejected sessions,
 * - nondecreasing sequence deduplication within a collector run for the
 *   connected session (safe latest-state ordering),
 * - the authoritative HubSnapshot for SSE subscribers.
 *
 * No persistence here: durable telemetry lives in the telemetry store and
 * combined acceptance/publication/restoration coordination lives in
 * ./sample-ingestion; this state is scoped to one hub process run
 * (hubRunId).
 */

import {
  type HubSnapshot,
  type MachineState,
  type Inventory,
  type MetricsRecord,
  type StatusMessage,
} from "../contracts";
import { type HubIdentityConfig } from "./config";

export type HubStateErrorCode =
  | "unknown-collector"
  | "session-conflict"
  | "session-closed"
  | "identity-mismatch";

export class HubStateError extends Error {
  readonly code: HubStateErrorCode;
  constructor(code: HubStateErrorCode, message: string) {
    super(message);
    this.name = "HubStateError";
    this.code = code;
  }
}

export type StartSessionInput = {
  sessionId: string;
  collectorId: string;
  runId: string;
  bootId: string;
  hostname: string;
  /**
   * Advertised delivery cadence from the hello (bounded integer), or null
   * before a hello. Stored per machine; null is never replaced by an
   * invented default cadence.
   */
  expectedIntervalMs: number | null;
};

type MachineRow = {
  identity: HubIdentityConfig;
  collectorId: string;
  machineId: string;
  hostname: string;
  /** Session that currently represents this machine, if any. */
  sessionId: string | null;
  runId: string | null;
  bootId: string | null;
  /**
   * Hello-advertised delivery cadence in milliseconds, or null before any
   * hello. Retained while the machine is disconnected; a rejected hello can
   * never change it.
   */
  expectedIntervalMs: number | null;
  lastSequence: number | null;
  lastReceivedAt: string | null;
  metrics?: MetricsRecord & { receivedAt: string };
  inventory?: Inventory;
  delivery?: StatusMessage;
  displayName?: string | null;
  tags?: string[];
};

export class HubState {
  readonly hubRunId: string;
  private readonly now: () => Date;
  private readonly rows = new Map<string, MachineRow>();
  private readonly byMachineId = new Map<string, MachineRow>();
  private readonly bySession = new Map<string, MachineRow>();
  private readonly tokens = new Map<string, HubIdentityConfig>();
  private readonly changeListeners = new Set<() => void>();
  private readonly listenerSet = new Set<(snapshot: HubSnapshot) => void>();

  constructor(input: {
    collectors: readonly HubIdentityConfig[];
    hubRunId: string;
    now?: () => Date;
  }) {
    if (typeof input.hubRunId !== "string" || input.hubRunId.length === 0) {
      throw new Error("HubState requires a nonempty hubRunId");
    }
    this.hubRunId = input.hubRunId;
    this.now = input.now ?? (() => new Date());
    for (const entry of input.collectors) this.registerIdentity(entry);
  }

  /** Register an already persisted enrollment before acknowledging it. */
  registerIdentity(entry: HubIdentityConfig): void {
    if (this.rows.has(entry.collectorId)) throw new Error("duplicate collectorId");
    if (this.byMachineId.has(entry.machineId)) throw new Error("duplicate machineId");
    if (this.tokens.has(entry.token)) throw new Error("duplicate credential token");
    const row: MachineRow = {
      identity: entry, collectorId: entry.collectorId, machineId: entry.machineId,
      hostname: "", sessionId: null, runId: null, bootId: null,
      expectedIntervalMs: null,
      lastSequence: null, lastReceivedAt: null,
    };
    this.rows.set(entry.collectorId, row);
    this.byMachineId.set(entry.machineId, row);
    this.tokens.set(entry.token, entry);
    this.emit();
  }

  /** Maps a bearer token to its configured collector identity, or null. */
  identityByToken(token: string): HubIdentityConfig | null {
    const found = this.tokens.get(token);
    return found ?? null;
  }

  /** Publish a committed credential change; old tokens/sessions lose access immediately. */
  synchronizeIdentities(identities: readonly HubIdentityConfig[]): void {
    const active = new Map(identities.map(identity => [identity.collectorId, identity]));
    this.tokens.clear();
    for (const row of this.rows.values()) {
      const identity = active.get(row.collectorId);
      if (!identity || identity.machineId !== row.machineId || identity.token !== row.identity.token) {
        if (row.sessionId !== null) this.endSession(row.sessionId);
      }
      if (identity && identity.machineId === row.machineId) {
        row.identity = identity;
        this.tokens.set(identity.token, identity);
      }
    }
    for (const identity of identities) {
      if (this.rows.has(identity.collectorId)) continue;
      const previous = this.byMachineId.get(identity.machineId);
      if (previous) {
        if (previous.sessionId) this.endSession(previous.sessionId);
        this.rows.delete(previous.collectorId);
        this.byMachineId.delete(previous.machineId);
      }
      this.registerIdentity(identity);
    }
    this.emit();
  }

  /**
   * Opens the authenticated session for one collector identity. A different
   * run supersedes the previous session only here, after an authenticated
   * hello; a connection claiming the identity's current run while another
   * session holds it is a conflicting simultaneous session and is rejected.
   *
   * The advertised cadence is taken from this hello: null only before a
   * hello, never an invented default. Every mutation below happens after all
   * rejection paths, so a rejected session cannot change cadence.
   */
  startSession(input: StartSessionInput): HubIdentityConfig {
    const row = this.rows.get(input.collectorId);
    if (!row) {
      throw new HubStateError(
        "unknown-collector",
        `unconfigured collector "${input.collectorId}"`,
      );
    }
    const identity = row.identity;
    if (row.runId === input.runId && row.bootId !== input.bootId) {
      throw new HubStateError("identity-mismatch", "a collector run cannot change boot identity");
    }
    if (row.sessionId !== null) {
      if (row.runId === input.runId) {
        throw new HubStateError(
          "session-conflict",
          `collector "${input.collectorId}" already has an open session for run "${row.runId}"`,
        );
      }
      // A different run replaces the old session's state; its sockets and
      // later records can no longer match the replacement (applyMetrics
      // looks up state only through the still-registered session id).
      this.bySession.delete(row.sessionId);
      row.sessionId = null;
    }
    if (row.runId !== input.runId) {
      row.lastSequence = null;
      row.lastReceivedAt = null;
      delete row.metrics;
      delete row.delivery;
    }
    row.sessionId = input.sessionId;
    row.runId = input.runId;
    row.bootId = input.bootId;
    row.hostname = input.hostname;
    row.expectedIntervalMs = input.expectedIntervalMs;
    this.bySession.set(input.sessionId, row);
    this.emit();
    return identity;
  }

  /**
   * Marks the session closed; last known metrics, hostname and the
   * hello-advertised cadence are retained while the machine is offline.
   */
  endSession(sessionId: string): boolean {
    const row = this.bySession.get(sessionId);
    if (!row || row.sessionId !== sessionId) return false;
    row.sessionId = null;
    this.bySession.delete(sessionId);
    this.emit();
    return true;
  }

  sessionCount(): number {
    return this.bySession.size;
  }

  /**
   * Re-check this after async commits; replaced sessions cannot publish live
   * state. Sample ingestion resolves and re-checks the current session
   * through here, including the queue-time hostname.
   */
  currentSession(sessionId: string): { machineId: string; collectorId: string; runId: string; bootId: string; hostname: string; expectedIntervalMs: number | null } | null {
    const row = this.bySession.get(sessionId);
    return row && row.sessionId === sessionId && row.runId !== null && row.bootId !== null
      ? { machineId: row.machineId, collectorId: row.collectorId, runId: row.runId, bootId: row.bootId, hostname: row.hostname, expectedIntervalMs: row.expectedIntervalMs } : null;
  }

  applyMetrics(sessionId: string, record: MetricsRecord, receivedAt: string): void {
    const row = this.bySession.get(sessionId);
    if (!row || row.runId !== record.runId || row.bootId !== record.bootId || row.collectorId !== record.collectorId) return;
    if (row.lastSequence !== null && record.sequence <= row.lastSequence) return;
    this.setMetrics(row, record, receivedAt);
    this.emit();
  }

  /** Restore last-known readings without manufacturing a new receipt or connection. */
  restoreMetrics(machineId: string, record: MetricsRecord, receivedAt: string, metadata?: { hostname: string | null; expectedIntervalMs: number | null }): void {
    const row = this.byMachineId.get(machineId);
    if (!row || row.collectorId !== record.collectorId || row.sessionId !== null) return;
    row.runId = record.runId;
    row.bootId = record.bootId;
    if (metadata) { row.hostname = metadata.hostname ?? ''; row.expectedIntervalMs = metadata.expectedIntervalMs; }
    this.setMetrics(row, record, receivedAt);
  }

  private setMetrics(row: MachineRow, record: MetricsRecord, receivedAt: string): void {
    row.lastSequence = record.sequence;
    row.lastReceivedAt = receivedAt;
    row.metrics = { ...record, receivedAt };
  }

  setInventory(machineId: string, inventory: Inventory): void {
    const row = this.byMachineId.get(machineId);
    if (row) { row.inventory = inventory; this.emit(); }
  }

  setDelivery(sessionId: string, status: StatusMessage): void {
    const row = this.bySession.get(sessionId);
    if (row) { row.delivery = status; this.emit(); }
  }

  setMetadata(machineId: string, meta: { displayName: string | null; tags: string[] }): void {
    const row = this.byMachineId.get(machineId);
    if (row) { row.displayName = meta.displayName; row.tags = meta.tags; this.emit(); }
  }

  connectedCount(): number {
    let count = 0;
    for (const row of this.rows.values()) if (row.sessionId !== null) count += 1;
    return count;
  }

  configuredCount(): number {
    return this.rows.size;
  }

  /** Authoritative full snapshot; machines are ordered by machineId. */
  snapshot(): HubSnapshot {
    const machines: MachineState[] = [...this.rows.values()]
      .sort((a, b) => (a.machineId < b.machineId ? -1 : a.machineId > b.machineId ? 1 : 0))
      .map((row) => ({
        machineId: row.machineId,
        collectorId: row.collectorId,
        hostname: row.hostname,
        connected: row.sessionId !== null,
        lastReceivedAt: row.lastReceivedAt,
        // Current hubs declare the cadence on every row; null means no hello yet.
        expectedIntervalMs: row.expectedIntervalMs,
        ...(row.metrics === undefined ? {} : { metrics: row.metrics }),
        ...(row.inventory === undefined ? {} : { inventory: row.inventory }),
        ...(row.delivery === undefined ? {} : { delivery: row.delivery }),
        ...(row.displayName === undefined ? {} : { displayName: row.displayName }),
        ...(row.tags === undefined ? {} : { tags: row.tags }),
      }));
    return { hubRunId: this.hubRunId, machines };
  }

  /** Subscribes to every state change; returns an unsubscribe function. */
  subscribe(listener: (snapshot: HubSnapshot) => void): () => void {
    this.listenerSet.add(listener);
    return () => {
      this.listenerSet.delete(listener);
    };
  }

  /** Invalidation without building a full snapshot; consumers may coalesce reads. */
  subscribeChanges(listener: () => void): () => void {
    this.changeListeners.add(listener);
    return () => { this.changeListeners.delete(listener); };
  }

  private emit(): void {
    for (const listener of this.changeListeners) {
      try { listener(); } catch (error) { console.error("hub state listener failed:", error); }
    }
    if (this.listenerSet.size === 0) return;
    const snapshot = this.snapshot();
    for (const listener of [...this.listenerSet]) {
      try {
        listener(snapshot);
      } catch (error) {
        console.error("hub state listener failed:", error);
      }
    }
  }
}
