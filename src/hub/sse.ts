/**
 * Server-sent events feed for browser clients (GET /api/live).
 *
 * Per docs/runtime-spike.md (browser auth superseding anonymous access):
 * - cache disabled, X-Accel-Buffering: no,
 * - the first event is `snapshot` (or `reset` when the request carries any
 *   Last-Event-ID; this spike always resynchronizes, it never replays),
 * - later `update` events each carry a full HubSnapshot,
 * - event IDs are `<hubRunId>:<cursor>`,
 * - subscription is synchronous (the initial frame is written before the
 *   handler returns), client count and socket buffers are bounded, periodic
 *   comments keep intermediaries from timing out, and all work is cleaned up
 *   on client close.
 *
 * Authorization: every frame is filtered against the caller-provided access
 * scope BEFORE serialization (initial snapshot, updates, and reset alike),
 * so a scoped session never sees another machine's row, hostname, collector
 * identity, or metrics. Session logout/expiry ends that session's streams
 * through endSessionStreams().
 */

import type { IncomingMessage, ServerResponse } from "node:http";
import { randomBytes } from "node:crypto";
import type { HubSnapshot } from "../contracts";
import { HubState } from "./state";

export const MAX_LIVE_CLIENTS = 64;
export const LIVE_CLIENT_BUFFER_LIMIT_BYTES = 1_048_576;
export const LIVE_KEEPALIVE_INTERVAL_MS = 15_000;

/**
 * What one connected browser may see. Computed by the composition root from
 * a validated session; the feed itself performs no cookie/session checks.
 */
export type LiveAccess = {
  /** Owning authenticated browser session. */
  sessionId: string;
  /** "*" for all machines, otherwise the exact machine-ID allowlist. */
  scope: "*" | ReadonlySet<string>;
};

export type LiveEvent = {
  id: string;
  event: "snapshot" | "reset" | "update";
  data: string;
};

type Client = {
  res: ServerResponse;
  keepalive: NodeJS.Timeout;
  /** Preset at attach; "*" or a resolved ReadonlySet. Never recomputed. */
  scope: "*" | ReadonlySet<string>;
  sessionId: string;
  scopeKey: string;
  cursor: bigint;
  lastData: string;
};

export class LiveFeed {
  private readonly state: HubState;
  private readonly maxClients: number;
  private readonly keepaliveMs: number;
  private readonly clients = new Set<Client>();
  private unsubscribe: (() => void) | null = null;
  private stopped = false;
  private readonly coalesceMs: number;
  private pending: NodeJS.Timeout | null = null;

  constructor(
    state: HubState,
    options?: { maxClients?: number; keepaliveMs?: number; coalesceMs?: number },
  ) {
    this.state = state;
    this.maxClients = options?.maxClients ?? MAX_LIVE_CLIENTS;
    this.keepaliveMs = options?.keepaliveMs ?? LIVE_KEEPALIVE_INTERVAL_MS;
    this.coalesceMs = options?.coalesceMs ?? 500;
    this.unsubscribe = state.subscribeChanges(() => {
      if (this.clients.size === 0 || this.pending !== null) return;
      if (this.coalesceMs === 0) { this.broadcast("update", state.snapshot()); return; }
      this.pending = setTimeout(() => {
        this.pending = null;
        if (this.clients.size > 0) this.broadcast("update", state.snapshot());
      }, this.coalesceMs);
      this.pending.unref();
    });
  }

  clientCount(): number {
    return this.clients.size;
  }

  /** Serializes an already-filtered frame for tests and reuse across clients. */
  private frame(
    event: LiveEvent["event"],
    snapshot: HubSnapshot,
    cursor: bigint,
  ): string {
    const id = `${snapshot.hubRunId}:${cursor}`;
    // JSON.stringify never emits raw newlines, so each `data:` line is safe.
    return `id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(snapshot)}\n\n`;
  }

  private broadcast(event: LiveEvent["event"], snapshot: HubSnapshot): void {
    if (this.stopped || this.clients.size === 0) return;
    const serialized = new Map<string, string>();
    for (const client of [...this.clients]) {
      let data = serialized.get(client.scopeKey);
      if (data === undefined) {
        data = JSON.stringify(filterSnapshot(snapshot, client.scope));
        serialized.set(client.scopeKey, data);
      }
      if (data === client.lastData) continue;
      client.lastData = data;
      client.cursor += 1n;
      this.write(client, `id: ${snapshot.hubRunId}:${client.cursor}\nevent: ${event}\ndata: ${data}\n\n`);
    }
  }

  private write(client: Client, chunk: string): void {
    if (client.res.writableEnded || client.res.destroyed) return;
    try {
      client.res.write(chunk);
    } catch {
      client.res.destroy();
      return;
    }
    if (client.res.writableLength > LIVE_CLIENT_BUFFER_LIMIT_BYTES) {
      // The client is too slow to keep up with full snapshots; dropping the
      // connection makes it resynchronize on reconnect.
      client.res.destroy();
    }
  }

  /**
   * Handles GET /api/live after the composition root authenticated the
   * session and rejected unauthorized requests. Missing authorization fails
   * closed; callers must explicitly provide the authenticated scope.
   *
   * Snapshot registration is synchronous: the initial frame is written
   * before attach returns, so subscribers never race a state change between
   * registration and first delivery.
   */
  attach(
    req: IncomingMessage,
    res: ServerResponse,
    authorization: LiveAccess | null,
  ): void {
    if (this.stopped) {
      res.writeHead(503, { "Content-Type": "application/json; charset=utf-8" });
      res.end(JSON.stringify({ error: "hub is shutting down" }));
      return;
    }
    if (this.clients.size >= this.maxClients) {
      res.writeHead(503, { "Content-Type": "application/json; charset=utf-8" });
      res.end(JSON.stringify({ error: "too many live clients" }));
      return;
    }
    if (!authorization) {
      res.writeHead(401, { "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store" });
      res.end(JSON.stringify({ error: "authentication required" }));
      return;
    }
    const access = authorization;
    const lastEventId = req.headers["last-event-id"];
    const initialEvent: LiveEvent["event"] =
      typeof lastEventId === "string" && lastEventId.length > 0 ? "reset" : "snapshot";
    res.writeHead(200, {
      "Content-Type": "text/event-stream; charset=utf-8",
      "Cache-Control": "no-cache, no-store, no-transform",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    });
    const client: Client = {
      res,
      scope: access.scope,
      sessionId: access.sessionId,
      scopeKey: access.scope === "*" ? "*" : JSON.stringify([...access.scope].sort()),
      // Opaque decimal cursor, incremented only for this viewer's changes.
      // Random starting points prevent exposing other viewers/activity and
      // distinguish reconnects without promising cross-stream ordering.
      cursor: BigInt(`0x${randomBytes(16).toString("hex")}`),
      lastData: "",
      keepalive: setInterval(() => {
        this.write(client, `: keepalive ${Date.now()}\n\n`);
      }, this.keepaliveMs),
    };
    // Never let idle SSE clients keep the process alive on shutdown.
    client.keepalive.unref();
    const snapshot = this.state.snapshot();
    const scoped = filterSnapshot(snapshot, client.scope);
    client.lastData = JSON.stringify(scoped);
    const initial = this.frame(initialEvent, scoped, client.cursor);
    res.write(initial);
    this.clients.add(client);
    res.on("close", () => this.remove(client));
    res.on("error", () => this.remove(client));
  }

  /**
   * Ends every stream owned by a browser session (logout or expiry).
   * Returns the number of connections ended.
   */
  endSessionStreams(sessionId: string): number {
    if (sessionId.length === 0) return 0;
    let ended = 0;
    for (const client of [...this.clients]) {
      if (client.sessionId !== sessionId) continue;
      this.remove(client);
      try {
        client.res.end();
      } catch {
        // Response already gone.
      }
      ended += 1;
    }
    return ended;
  }

  private remove(client: Client): void {
    if (!this.clients.has(client)) return;
    this.clients.delete(client);
    clearInterval(client.keepalive);
  }

  /** Stops the feed and ends every client (used on shutdown). */
  stop(): void {
    if (this.stopped) return;
    this.stopped = true;
    if (this.pending !== null) { clearTimeout(this.pending); this.pending = null; }
    for (const client of [...this.clients]) {
      this.remove(client);
      try {
        client.res.end();
      } catch {
        // Response already gone; nothing to clean up.
      }
    }
    this.unsubscribe?.();
    this.unsubscribe = null;
  }
}

/**
 * Server-side scope filter applied to the authoritative snapshot. Filtering
 * happens before serialization: excluded machines (including collector IDs
 * and hostnames) never enter the serialized bytes.
 */
function filterSnapshot(
  snapshot: HubSnapshot,
  scope: "*" | ReadonlySet<string>,
): HubSnapshot {
  if (scope === "*") return snapshot;
  const machines = snapshot.machines.filter((machine) => scope.has(machine.machineId));
  if (machines.length === snapshot.machines.length) return snapshot;
  return { hubRunId: snapshot.hubRunId, machines };
}
