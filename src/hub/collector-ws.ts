/**
 * Collector-side WebSocket endpoint of the hub
 * (upgrades on /api/collector/connect only).
 *
 * - bearer credentials are authenticated *before* the upgrade completes,
 * - browser Origin headers are rejected,
 * - the first message must be hello within 5 seconds, and conflicting
 *   simultaneous sessions for an identity lose to the current session (a new
 *   run supersedes only via authenticated hello),
 * - an authenticated hello advertises this destination's delivery cadence
 *   (bounded expectedIntervalMs),
 * - the hello/welcome exchange is unversioned: the hub always sends the single
 *   canonical welcome shape and accepts the canonical full metrics payload,
 * - frames, batch size, message rate, socket output queues and idle
 *   connections (ping/pong) are bounded,
 * - Sample/inventory acceptance, in-flight batch bounds, commit-to-live
 *   sequencing and Incident eligibility live in the Sample ingestion module
 *   (./sample-ingestion); this transport only formats acknowledgments,
 * - HTTP access to the endpoint answers 426.
 */

import type { Duplex } from "node:stream";
import type { IncomingMessage } from "node:http";
import { randomUUID } from "node:crypto";
import { WebSocket, WebSocketServer, type RawData } from "ws";
import {
  MAX_MESSAGE_BYTES,
  MAX_DETAIL_BYTES,
  parseCollectorMessage,
  type AckMessage,
  type CollectorMessage,
  type HubMessage,
  type DetailResult,
  type DetailEnded,
  type WelcomeMessage,
} from "../contracts";
import { HubState, HubStateError } from "./state";
import type { HubIdentityConfig } from "./config";
import { SampleIngestion, MAX_IN_FLIGHT_SAMPLE_BATCHES } from "./sample-ingestion";

export const COLLECTOR_CONNECT_PATH = "/api/collector/connect";
export const HELLO_TIMEOUT_MS = 5_000;
export const COLLECTOR_PING_INTERVAL_MS = 15_000;
export const COLLECTOR_IDLE_TIMEOUT_MS = 45_000;
export const COLLECTOR_SEND_BUFFER_LIMIT_BYTES = 1_048_576;
/** Token bucket: sustained rate covers the collector's 100 ms minimum cadence. */
export const COLLECTOR_MESSAGES_PER_SECOND = 10;
export const COLLECTOR_MESSAGE_BURST = 30;
export const MAX_COLLECTOR_CONNECTIONS = 100;

/** WebSocket close codes chosen within the 4000-4999 private range. */
const CLOSE_SHUTDOWN = 1001;
const CLOSE_POLICY = 1008; // auth/identity problems
const CLOSE_HELLO_TIMEOUT = 4001;
const CLOSE_IDENTITY_MISMATCH = 4002;
const CLOSE_INVALID_MESSAGE = 4003;
const CLOSE_SESSION_CONFLICT = 4005;
const CLOSE_PROTOCOL_STEP = 4006;
const CLOSE_RATE = 4007;

type Phase = "await-hello" | "active";

type CollectorConnection = {
  id: string;
  ws: WebSocket;
  phase: Phase;
  credentials: HubIdentityConfig | null;
  tokens: number;
  lastRefillAt: number;
  lastActivity: number;
  helloTimer: NodeJS.Timeout | null;
  pingTimer: NodeJS.Timeout | null;
};

/** True when the request is a WebSocket upgrade aimed at the collector path. */
export function isCollectorUpgradeRequest(req: IncomingMessage): boolean {
  try {
    return new URL(req.url ?? "/", "http://hub.invalid").pathname === COLLECTOR_CONNECT_PATH;
  } catch {
    return false;
  }
}

export class CollectorGateway {
  private readonly state: HubState;
  private readonly wss = new WebSocketServer({
    noServer: true,
    maxPayload: MAX_DETAIL_BYTES,
    perMessageDeflate: false,
    autoPong: true,
  });
  private readonly connections = new Map<string, CollectorConnection>();
  private stopped = false;
  private readonly ingestion: SampleIngestion;
  private readonly maxConnections: number;
  private readonly onDetail: (machineId: string, message: DetailResult | DetailEnded) => void;
  private readonly onDisconnect: (machineId: string) => void;

  constructor(input: { state: HubState; ingestion: SampleIngestion;
    maxConnections?: number;
    onDetail?: (machineId: string, message: DetailResult | DetailEnded) => void;
    onDisconnect?: (machineId: string) => void }) {
    this.state = input.state;
    this.ingestion = input.ingestion;
    this.maxConnections = input.maxConnections ?? MAX_COLLECTOR_CONNECTIONS;
    if (!Number.isSafeInteger(this.maxConnections) || this.maxConnections < 1) {
      throw new Error("collector connection budget must be a positive safe integer");
    }
    this.onDetail = input.onDetail ?? (() => {});
    this.onDisconnect = input.onDisconnect ?? (() => {});
  }

  /** HTTP handler for the collector endpoint: everything non-upgrade is 426. */
  handleHttpRequest(req: IncomingMessage, res: import("node:http").ServerResponse): void {
    const method = req.method ?? "GET";
    res.writeHead(426, {
      "Content-Type": "text/plain; charset=utf-8",
      Upgrade: "websocket",
      Connection: "close",
    });
    res.end(
      `426 Upgrade Required: ${method} ${COLLECTOR_CONNECT_PATH} expects a WebSocket upgrade (wss://... or loopback ws://...)\n`,
    );
  }

  /**
   * Authenticated upgrade. Authenticates bearer credentials before calling
   * into ws's upgrade handler so rejected clients never reach the protocol.
   */
  handleUpgrade(req: IncomingMessage, socket: Duplex, head: Buffer): void {
    if (this.stopped) {
      this.reject(socket, 503, "hub is shutting down");
      return;
    }
    if (req.headers.origin !== undefined) {
      // Browser pages may not reach the collector endpoint; collectors do
      // not send Origin.
      this.reject(socket, 403, "origin not allowed");
      return;
    }
    const credentials = this.authenticate(req);
    if (!credentials) {
      this.reject(socket, 401, "invalid bearer credentials");
      return;
    }
    if (this.connections.size >= this.maxConnections) {
      this.reject(socket, 503, "too many collector connections");
      return;
    }
    this.wss.handleUpgrade(req, socket, head, (ws) => {
      this.setupConnection(ws, credentials);
    });
  }

  private authenticate(req: IncomingMessage): HubIdentityConfig | null {
    const header = req.headers.authorization;
    if (typeof header !== "string" || header.length > 4096) return null;
    const match = /^Bearer\s+(\S+)$/i.exec(header.trim());
    if (!match) return null;
    return this.state.identityByToken(match[1]);
  }

  /** Writes a minimal HTTP rejection onto the still-plain socket. */
  private reject(socket: Duplex, status: number, reason: string): void {
    const text = status === 401 ? "unauthorized" : status === 403 ? "forbidden" : status === 503 ? "unavailable" : "rejected";
    try {
      socket.end(
        `HTTP/1.1 ${status} ${text}\r\nConnection: close\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: ${reason.length}\r\n\r\n${reason}`,
      );
    } catch {
      socket.destroy();
    }
    // Requests rejected before upgrade must not linger.
    socket.once("close", () => socket.destroy());
  }

  private setupConnection(ws: WebSocket, credentials: HubIdentityConfig): void {
    if (this.stopped) {
      ws.close(CLOSE_SHUTDOWN, "hub shutting down");
      return;
    }
    const connection: CollectorConnection = {
      id: randomUUID(),
      ws,
      phase: "await-hello",
      credentials,
      tokens: COLLECTOR_MESSAGE_BURST,
      lastRefillAt: Date.now(),
      lastActivity: Date.now(),
      helloTimer: null,
      pingTimer: null,
    };
    this.connections.set(connection.id, connection);
    connection.helloTimer = setTimeout(() => {
      if (connection.phase === "await-hello") {
        ws.close(CLOSE_HELLO_TIMEOUT, "hello deadline exceeded");
        setTimeout(() => {
          if (connection.phase === "await-hello") ws.terminate();
        }, 1_000).unref();
      }
    }, HELLO_TIMEOUT_MS);
    ws.on("message", (data, isBinary) => {
      if (ws.readyState !== WebSocket.OPEN) return;
      if (isBinary) {
        this.fail(connection, CLOSE_INVALID_MESSAGE, "expected a JSON text message");
        return;
      }
      this.onMessage(connection, data);
    });
    ws.on("pong", () => {
      connection.lastActivity = Date.now();
    });
    ws.on("close", () => this.closeConnection(connection));
    ws.on("error", () => this.closeConnection(connection));
    connection.pingTimer = setInterval(() => {
      const idleFor = Date.now() - connection.lastActivity;
      if (idleFor > COLLECTOR_IDLE_TIMEOUT_MS) {
        ws.terminate();
        return;
      }
      if (ws.readyState === WebSocket.OPEN) ws.ping();
    }, COLLECTOR_PING_INTERVAL_MS);
    // Keep the ping timer from holding the process open if shutdown skipped it.
    connection.pingTimer.unref?.();
  }

  private closeConnection(connection: CollectorConnection): void {
    this.connections.delete(connection.id);
    if (connection.helloTimer) {
      clearTimeout(connection.helloTimer);
      connection.helloTimer = null;
    }
    if (connection.pingTimer) {
      clearInterval(connection.pingTimer);
      connection.pingTimer = null;
    }
    if (connection.phase === "active") {
      connection.phase = "await-hello";
      const current = this.state.currentSession(connection.id);
      if (this.state.endSession(connection.id) && current) this.onDisconnect(current.machineId);
    }
  }

  private refill(connection: CollectorConnection): boolean {
    const now = Date.now();
    const elapsedSeconds = Math.max(0, now - connection.lastRefillAt) / 1_000;
    connection.lastRefillAt = now;
    connection.tokens = Math.min(
      COLLECTOR_MESSAGE_BURST,
      connection.tokens + elapsedSeconds * COLLECTOR_MESSAGES_PER_SECOND,
    );
    if (connection.tokens < 1) return false;
    connection.tokens -= 1;
    return true;
  }

  private onMessage(connection: CollectorConnection, data: RawData): void {
    connection.lastActivity = Date.now();
    if (this.stopped) {
      connection.ws.close(CLOSE_SHUTDOWN, "hub shutting down");
      return;
    }
    if (!this.refill(connection)) {
      this.fail(connection, CLOSE_RATE, "message rate limit exceeded");
      return;
    }
    let text: string;
    try {
      text = (Array.isArray(data) ? Buffer.concat(data) : Buffer.from(data as ArrayBuffer)).toString("utf8");
      // Size checks must precede parsing untrusted text. The outer bound covers
      // the largest allowed message (detail results); the per-type bound is
      // enforced after the type is known.
      if (Buffer.byteLength(text, "utf8") > MAX_DETAIL_BYTES) {
        this.fail(connection, 1009, "message too large");
        return;
      }
      const message = parseCollectorMessage(JSON.parse(text));
      const maxBytes = message.type === "detail.result" ? MAX_DETAIL_BYTES : MAX_MESSAGE_BYTES;
      if (Buffer.byteLength(text, "utf8") > maxBytes) {
        this.fail(connection, 1009, "message too large");
        return;
      }
      if (connection.phase === "await-hello") this.onHello(connection, message);
      else this.onSessionMessage(connection, message);
    } catch (error) {
      if (error instanceof SyntaxError) {
        this.fail(connection, CLOSE_INVALID_MESSAGE, "message is not valid JSON");
        return;
      }
      this.fail(connection, CLOSE_INVALID_MESSAGE, errorDescription(error));
    }
  }

  private onHello(connection: CollectorConnection, message: CollectorMessage): void {
    if (message.type !== "hello") {
      this.fail(connection, CLOSE_PROTOCOL_STEP, "first message must be hello");
      return;
    }
    const hello = message;
    if (connection.credentials === null || hello.collectorId !== connection.credentials.collectorId) {
      this.fail(
        connection,
        CLOSE_IDENTITY_MISMATCH,
        `hello collectorId does not match authenticated identity`,
      );
      return;
    }
    try {
      this.state.startSession({
        sessionId: connection.id,
        collectorId: hello.collectorId,
        runId: hello.runId,
        bootId: hello.bootId,
        hostname: hello.hostname,
        expectedIntervalMs: hello.expectedIntervalMs,
      });
    } catch (error) {
      if (error instanceof HubStateError) {
        const code =
          error.code === "session-conflict" ? CLOSE_SESSION_CONFLICT : CLOSE_POLICY;
        this.fail(connection, code, error.message);
        return;
      }
      throw error;
    }
    connection.phase = "active";
    // A new authenticated run replaces the previous run's actual socket as
    // well as its state, so superseded connections cannot accumulate.
    for (const other of this.connections.values()) {
      if (other.id !== connection.id && other.phase === "active" &&
          other.credentials?.collectorId === hello.collectorId) {
        other.ws.terminate();
      }
    }
    if (connection.helloTimer) {
      clearTimeout(connection.helloTimer);
      connection.helloTimer = null;
    }
    const machineId = connection.credentials.machineId;
    const welcome: WelcomeMessage = {
      type: "welcome",
      hubRunId: this.state.hubRunId,
      machineId,
      delivery: "durable",
      maxInFlightBatches: MAX_IN_FLIGHT_SAMPLE_BATCHES,
    };
    this.send(connection, JSON.stringify(welcome));
  }

  private onSessionMessage(connection: CollectorConnection, message: CollectorMessage): void {
    if (message.type === "hello") {
      this.fail(connection, CLOSE_PROTOCOL_STEP, "duplicate hello");
      return;
    }
    const current = this.state.currentSession(connection.id);
    if (!current) throw new Error("session is no longer current");
    if (message.type === "inventory") {
      this.ingestion.acceptInventory(connection.id, message.capturedAt, message.inventory);
    } else if (message.type === "status") {
      this.state.setDelivery(connection.id, message);
    } else if (message.type === "detail.result" || message.type === "detail.ended") {
      this.onDetail(current.machineId, message);
    } else if (message.type === "samples") {
      const submission = this.ingestion.submitSamples(connection.id, message.mode, message.records);
      if (submission.status === "retry") {
        this.send(connection, this.formatAck([], [], submission.retryAfterMs));
        return;
      }
      void submission.committed.then((outcome) => {
        const acknowledgement: string = outcome.status === "committed"
          ? this.formatAck(outcome.accepted, outcome.rejected)
          : this.formatAck([], [], outcome.retryAfterMs);
        this.send(connection, acknowledgement);
      }).catch(() => {
        this.fail(connection, CLOSE_POLICY, "storage temporarily unavailable");
      });
    }
  }

  /** Acknowledgments describe exactly the durably accepted/rejected records. */
  private formatAck(
    accepted: AckMessage["accepted"],
    rejected: AckMessage["rejected"],
    retryAfterMs?: number,
  ): string {
    return JSON.stringify({
      type: "ack", accepted, rejected,
      ...(retryAfterMs === undefined ? {} : { retryAfterMs }),
    });
  }

  /** Process interests reuse the authenticated outbound collector connection. */
  sendToMachine(machineId: string, message: HubMessage): boolean {
    for (const connection of this.connections.values()) {
      if (connection.phase === "active" &&
        connection.credentials?.machineId === machineId && this.state.currentSession(connection.id)) {
        if (connection.ws.readyState !== WebSocket.OPEN || connection.ws.bufferedAmount > COLLECTOR_SEND_BUFFER_LIMIT_BYTES) return false;
        this.send(connection, JSON.stringify(message));
        return true;
      }
    }
    return false;
  }

  revokeInvalidConnections(): void {
    for (const connection of this.connections.values()) {
      const credential = connection.credentials;
      if (credential && this.state.identityByToken(credential.token)?.collectorId !== credential.collectorId) {
        // Registry synchronization may already have ended this session. Only
        // notify an active socket, and never clear a healthy replacement run.
        const orphanedActive = connection.phase === 'active' && !this.state.currentSession(connection.id) &&
          !this.state.snapshot().machines.some(m => m.machineId === credential.machineId && m.connected);
        this.closeConnection(connection);
        if (orphanedActive) this.onDisconnect(credential.machineId);
        this.fail(connection, CLOSE_POLICY, 'collector credential revoked');
      }
    }
  }

  private send(connection: CollectorConnection, text: string): void {
    const ws = connection.ws;
    if (ws.readyState !== WebSocket.OPEN) return;
    ws.send(text);
    // Bound the socket output queue; a collector too slow to drain gets cut.
    if (ws.bufferedAmount > COLLECTOR_SEND_BUFFER_LIMIT_BYTES) {
      ws.terminate();
    }
  }

  private fail(connection: CollectorConnection, code: number, reason: string): void {
    try {
      connection.ws.close(code, reason.slice(0, 120));
    } catch {
      // ignore
    }
    // Do not wait indefinitely for a close frame from a broken client.
    setTimeout(() => {
      try {
        connection.ws.terminate();
      } catch {
        // already gone
      }
    }, 1_000).unref();
  }

  /** Closes every collector socket within a bounded time. */
  stop(): void {
    this.stopped = true;
    for (const connection of [...this.connections.values()]) {
      try {
        connection.ws.close(CLOSE_SHUTDOWN, "hub shutting down");
      } catch {
        // already closed
      }
    }
    setTimeout(() => {
      for (const connection of [...this.connections.values()]) {
        try {
          connection.ws.terminate();
        } catch {
          // already gone
        }
      }
    }, 1_000).unref();
  }
}

function errorDescription(error: unknown): string {
  if (error instanceof Error && error.message) return error.message;
  return String(error);
}
