/**
 * Controllable WebSocket relay between a collector executable and the hub.
 *
 * The runtime suites otherwise cannot observe or steer the delivery protocol:
 * the collector talks straight to the hub and acks, retries and connection
 * drops happen inside one TLS/WS session. This relay is a transparent proxy
 * that also lets a test:
 *
 * - hold every hub→collector `ack` (records stay pending) and release them,
 * - drop `ack` frames (a lost ack that only the collector's timeout or a
 *   reconnect can recover from),
 * - inject an `ack` carrying `retryAfterMs` (the hub's backpressure answer),
 * - close both sides abruptly while staying listening, so the collector
 *   reconnects (a dropped link) or survives a hub restart (a new upstream),
 *
 * and it records every frame in both directions for assertions. Only `ack`
 * frames are ever held or dropped; samples, inventory, status and detail
 * frames always flow, so a test controls acknowledgement without stalling
 * everything else.
 *
 * The relay forwards the collector's end-to-end handshake headers upstream
 * unchanged (in particular `Origin` and `Authorization`, which the hub
 * validates) and never substitutes a token: the hub still authenticates the
 * real credential. The `Host` header is the one exception: it names the relay,
 * so the upstream client generates the hub's authority instead.
 * Payload bytes and the text/binary flag are preserved in both directions,
 * including frames buffered while the upstream connects and acks held back.
 */
import { createServer, type IncomingMessage, type Server } from "node:http";
import type { Duplex } from "node:stream";
import { WebSocket, WebSocketServer, type RawData } from "ws";
import { registerCleanup, unregisterCleanup } from "./cleanup";

export type RelayDirection = "collector" | "hub";

/** collector = collector→hub; hub = hub→collector. */
export type RelayDelivery = "forwarded" | "held" | "dropped" | "injected";

export type RelayFrame = {
  direction: RelayDirection;
  delivery: RelayDelivery;
  /** Parsed JSON, or the raw text when a frame is not JSON. */
  data: unknown;
  /** UTF-8 decoding of the payload, for readable logs and JSON parsing. */
  text: string;
  /** Exact payload bytes, unchanged from the sender. */
  bytes: Buffer;
  /** WebSocket text/binary flag, unchanged from the sender. */
  isBinary: boolean;
  at: number;
};

export type AcknowledgmentMode = "forward" | "hold" | "drop";

export interface RelayController {
  /** ws:// URL the collector should use as its destination. */
  readonly url: string;
  readonly port: number;
  readonly upstreamUrl: string;
  /** Collector connections that completed the WebSocket upgrade. */
  connections(): number;
  frames(): readonly RelayFrame[];
  /** Wait for a frame (existing or future) that satisfies the predicate. */
  waitForFrame(
    predicate: (frame: RelayFrame) => boolean,
    deadlineMs: number,
    what: string,
  ): Promise<RelayFrame>;
  waitForConnection(count: number, deadlineMs: number, what: string): Promise<void>;
  /** Hold hub→collector acks; they are not delivered until `releaseAcks`. */
  holdAcks(): void;
  /** Deliver held acks in order and stop holding. */
  releaseAcks(): void;
  /** Discard hub→collector acks instead of delivering them. */
  dropAcks(): void;
  /** Deliver acks normally (also ends `holdAcks`/`dropAcks`). */
  forwardAcks(): void;
  /**
   * Install a filter that discards hub→collector frames of the given message
   * types, for example to hide later `detail.subscribe` renewals and
   * `detail.cancel` from the collector.
   */
  dropHubTypes(types: readonly string[]): void;
  /** Deliver every hub→collector frame type again. */
  forwardHubTypes(): void;
  /** Cumulative hub→collector frames discarded by `dropHubTypes`. */
  suppressedHubFrames(): number;
  /** Acks currently held back from the collector. */
  pendingHeldAcks(): number;
  /** Cumulative acks held back (including already released ones). */
  heldAcks(): number;
  /** Cumulative acks discarded. */
  droppedAcks(): number;
  /** Send `{type:"ack", accepted:[], rejected:[], retryAfterMs}` to the collector. */
  injectRetry(retryAfterMs: number): boolean;
  /** Close the current collector and upstream sockets abruptly; keep listening. */
  disconnect(): void;
  close(): Promise<void>;
}

const MAX_FRAMES = 20_000;
const CONNECT_PATH = "/api/collector/connect";

function parseText(text: string): unknown {
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return text;
  }
}

export function relayMessageType(frame: RelayFrame): string | undefined {
  const value = frame.data as { type?: unknown } | null;
  return typeof value === "object" && value !== null && typeof value.type === "string"
    ? value.type
    : undefined;
}

export function isAckFrame(frame: RelayFrame): boolean {
  return relayMessageType(frame) === "ack";
}

export type RelaySampleRecord = { runId: string; sequence: number; mode: string };

/** Records carried by a `samples` frame in either direction, or an empty list. */
export function relaySampleRecords(frame: RelayFrame): RelaySampleRecord[] {
  const value = frame.data as { type?: unknown; mode?: unknown; records?: unknown } | null;
  if (typeof value !== "object" || value === null || value.type !== "samples" || !Array.isArray(value.records)) {
    return [];
  }
  const mode = value.mode === "replay" ? "replay" : "live";
  const records: RelaySampleRecord[] = [];
  for (const entry of value.records) {
    const record = entry as { runId?: unknown; sequence?: unknown; kind?: unknown };
    if (typeof record.runId !== "string" || typeof record.sequence !== "number") continue;
    records.push({ runId: record.runId, sequence: record.sequence, mode });
  }
  return records;
}

class Relay implements RelayController {
  readonly url: string;
  private readonly server: Server;
  private readonly wss: WebSocketServer;
  private readonly options: { upstreamUrl: string };
  private readonly frameLog: RelayFrame[] = [];
  private readonly frameWaiters: Array<{
    predicate: (frame: RelayFrame) => boolean;
    resolve: (frame: RelayFrame) => void;
  }> = [];
  private client: WebSocket | null = null;
  private upstream: WebSocket | null = null;
  /** Payloads read from the client before the upstream socket opened. */
  private readonly pendingUpstream: RelayPayload[] = [];
  private readonly held: RelayPayload[] = [];
  private readonly suppressedHubTypes = new Set<string>();
  private mode: AcknowledgmentMode = "forward";
  private connectionCount = 0;
  private heldTotal = 0;
  private droppedTotal = 0;
  private suppressedTotal = 0;
  private closed = false;

  constructor(options: { upstreamUrl: string; port: number; server: Server; wss: WebSocketServer }) {
    this.options = options;
    this.server = options.server;
    this.wss = options.wss;
    this.url = `ws://127.0.0.1:${options.port}${CONNECT_PATH}`;
    this.port = options.port;
  }

  readonly port: number;

  get upstreamUrl(): string {
    return this.options.upstreamUrl;
  }

  connections(): number {
    return this.connectionCount;
  }

  frames(): readonly RelayFrame[] {
    return this.frameLog;
  }

  async waitForFrame(
    predicate: (frame: RelayFrame) => boolean,
    deadlineMs: number,
    what: string,
  ): Promise<RelayFrame> {
    const existing = this.frameLog.find(predicate);
    if (existing !== undefined) return existing;
    return await new Promise<RelayFrame>((resolve, reject) => {
      const waiter = {
        predicate,
        resolve: (frame: RelayFrame) => {
          clearTimeout(timer);
          resolve(frame);
        },
      };
      const timer = setTimeout(() => {
        const index = this.frameWaiters.indexOf(waiter);
        if (index >= 0) this.frameWaiters.splice(index, 1);
        reject(new Error(`relay timeout after ${deadlineMs}ms waiting for ${what}`));
      }, deadlineMs);
      this.frameWaiters.push(waiter);
    });
  }

  async waitForConnection(count: number, deadlineMs: number, what: string): Promise<void> {
    const end = Date.now() + deadlineMs;
    while (this.connectionCount < count) {
      if (Date.now() >= end) {
        throw new Error(`relay timeout after ${deadlineMs}ms waiting for ${what} (connections ${this.connectionCount}/${count})`);
      }
      await new Promise((resolve) => setTimeout(resolve, 25));
    }
  }

  holdAcks(): void {
    this.mode = "hold";
  }

  releaseAcks(): void {
    this.mode = "forward";
    const client = this.client;
    if (client === null || client.readyState !== WebSocket.OPEN) return;
    while (this.held.length > 0) {
      const payload = this.held.shift() as RelayPayload;
      sendPayload(client, payload);
    }
  }

  dropAcks(): void {
    this.mode = "drop";
  }

  forwardAcks(): void {
    this.mode = "forward";
  }

  dropHubTypes(types: readonly string[]): void {
    for (const type of types) this.suppressedHubTypes.add(type);
  }

  forwardHubTypes(): void {
    this.suppressedHubTypes.clear();
  }

  suppressedHubFrames(): number {
    return this.suppressedTotal;
  }

  pendingHeldAcks(): number {
    return this.held.length;
  }

  heldAcks(): number {
    return this.heldTotal;
  }

  droppedAcks(): number {
    return this.droppedTotal;
  }

  injectRetry(retryAfterMs: number): boolean {
    const client = this.client;
    if (client === null || client.readyState !== WebSocket.OPEN) return false;
    const text = JSON.stringify({ type: "ack", accepted: [], rejected: [], retryAfterMs });
    client.send(text);
    this.record("hub", "injected", textPayload(text));
    return true;
  }

  disconnect(): void {
    const client = this.client;
    const upstream = this.upstream;
    this.client = null;
    this.upstream = null;
    if (upstream !== null && upstream.readyState !== WebSocket.CLOSED) {
      try {
        upstream.terminate();
      } catch {
        // already gone
      }
    }
    if (client !== null && client.readyState !== WebSocket.CLOSED) {
      try {
        client.terminate();
      } catch {
        // already gone
      }
    }
  }

  accept(req: IncomingMessage, socket: Duplex, head: Buffer): void {
    if (this.closed) {
      socket.destroy();
      return;
    }
    this.wss.handleUpgrade(req, socket, head, (ws) => this.attach(ws, req));
  }

  private attach(client: WebSocket, req: IncomingMessage): void {
    this.connectionCount += 1;
    // A collector has one socket; a leftover previous one belongs to a later
    // generation and must not observe this connection's traffic.
    const previous = this.client;
    this.client = client;
    if (previous !== null && previous.readyState !== WebSocket.CLOSED) {
      try {
        previous.terminate();
      } catch {
        // already gone
      }
    }

    const upstream = new WebSocket(this.options.upstreamUrl, {
      // A transparent proxy: every end-to-end handshake header the collector
      // sent (Origin, Authorization, cookies, custom headers) reaches the hub
      // unchanged. Only the WebSocket handshake headers this new client must
      // generate itself are omitted.
      headers: upstreamHandshakeHeaders(req),
      perMessageDeflate: false,
    });
    this.upstream = upstream;
    this.pendingUpstream.length = 0;

    client.on("message", (data, isBinary) => {
      if (this.client !== client) return;
      const payload = toPayload(data, isBinary);
      this.record("collector", "forwarded", payload);
      if (upstream.readyState === WebSocket.OPEN) sendPayload(upstream, payload);
      else if (upstream.readyState === WebSocket.CONNECTING) this.pendingUpstream.push(payload);
    });

    upstream.on("open", () => {
      if (this.upstream !== upstream) return;
      for (const payload of this.pendingUpstream.splice(0)) sendPayload(upstream, payload);
    });

    upstream.on("message", (data, isBinary) => {
      if (this.upstream !== upstream) return;
      const payload = toPayload(data, isBinary);
      const type = payloadMessageType(payload);
      if (type !== undefined && this.suppressedHubTypes.has(type)) {
        this.suppressedTotal += 1;
        this.record("hub", "dropped", payload);
        return;
      }
      if (isAckPayload(payload) && this.mode === "hold") {
        this.held.push(payload);
        this.heldTotal += 1;
        this.record("hub", "held", payload);
        return;
      }
      if (isAckPayload(payload) && this.mode === "drop") {
        this.droppedTotal += 1;
        this.record("hub", "dropped", payload);
        return;
      }
      this.record("hub", "forwarded", payload);
      if (this.client === client && client.readyState === WebSocket.OPEN) sendPayload(client, payload);
    });

    const upstreamFailed = (): void => {
      if (this.upstream !== upstream) return;
      this.upstream = null;
      // The collector must observe the link going away and reconnect later.
      if (this.client === client && client.readyState !== WebSocket.CLOSED) {
        try {
          client.terminate();
        } catch {
          // already gone
        }
      }
    };
    upstream.on("close", upstreamFailed);
    upstream.on("error", upstreamFailed);

    client.on("close", () => {
      if (this.client === client) this.client = null;
      if (this.upstream === upstream) {
        this.upstream = null;
        if (upstream.readyState !== WebSocket.CLOSED) {
          try {
            upstream.terminate();
          } catch {
            // already gone
          }
        }
      }
    });
    client.on("error", () => {
      // The close handler performs the cleanup.
    });
  }

  private record(direction: RelayDirection, delivery: RelayDelivery, payload: RelayPayload): void {
    const text = payload.bytes.toString("utf8");
    const frame: RelayFrame = {
      direction,
      delivery,
      data: parseText(text),
      text,
      bytes: payload.bytes,
      isBinary: payload.isBinary,
      at: Date.now(),
    };
    this.frameLog.push(frame);
    if (this.frameLog.length > MAX_FRAMES) this.frameLog.shift();
    for (let index = this.frameWaiters.length - 1; index >= 0; index -= 1) {
      const waiter = this.frameWaiters[index];
      if (!waiter.predicate(frame)) continue;
      this.frameWaiters.splice(index, 1);
      waiter.resolve(frame);
    }
  }

  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    this.disconnect();
    await new Promise<void>((resolve) => this.wss.close(() => resolve()));
    await new Promise<void>((resolve) => {
      if (!this.server.listening) {
        resolve();
        return;
      }
      this.server.close(() => resolve());
    });
  }
}

type RelayPayload = { bytes: Buffer; isBinary: boolean };

const HANDSHAKE_HEADERS = new Set([
  // Generated fresh by the ws client for this upstream handshake. Host is the
  // relay's authority, not the hub's: a forward proxy must let the upstream
  // client name the origin server, or the hub cannot validate its own Host.
  "host",
  "connection",
  "upgrade",
  "sec-websocket-key",
  "sec-websocket-version",
  "sec-websocket-extensions",
  "sec-websocket-protocol",
]);

/**
 * End-to-end request headers, unchanged. `ws` overwrites the handshake headers
 * above when it builds the upstream request, so they cannot be forwarded.
 */
function upstreamHandshakeHeaders(req: IncomingMessage): Record<string, string | string[]> {
  const headers: Record<string, string | string[]> = {};
  for (const [name, value] of Object.entries(req.headers)) {
    if (value === undefined || HANDSHAKE_HEADERS.has(name)) continue;
    headers[name] = value;
  }
  return headers;
}

function toPayload(data: RawData, isBinary: boolean): RelayPayload {
  const bytes = typeof data === "string"
    ? Buffer.from(data, "utf8")
    : Array.isArray(data)
      ? Buffer.concat(data)
      : Buffer.isBuffer(data)
        ? data
        : Buffer.from(data as ArrayBuffer);
  return { bytes, isBinary };
}

function textPayload(text: string): RelayPayload {
  return { bytes: Buffer.from(text, "utf8"), isBinary: false };
}

function sendPayload(socket: WebSocket, payload: RelayPayload): void {
  // A Buffer sent with binary=false is a text frame; the exact bytes and the
  // text/binary flag both survive the relay.
  socket.send(payload.bytes, { binary: payload.isBinary });
}

function payloadText(payload: RelayPayload): string {
  return payload.bytes.toString("utf8");
}

function payloadMessageType(payload: RelayPayload): string | undefined {
  const value = parseText(payloadText(payload)) as { type?: unknown } | null;
  return typeof value === "object" && value !== null && typeof value.type === "string"
    ? value.type
    : undefined;
}

function isAckPayload(payload: RelayPayload): boolean {
  return payloadMessageType(payload) === "ack";
}

export async function startRelay(options: { upstreamUrl: string; label?: string }): Promise<RelayController> {
  const wss = new WebSocketServer({ noServer: true, perMessageDeflate: false });
  const server = createServer((request, response) => {
    response.writeHead(426, { "Content-Type": "text/plain; charset=utf-8", Upgrade: "websocket" });
    response.end(`426 Upgrade Required: ${request.url ?? CONNECT_PATH} expects a WebSocket upgrade\n`);
  });
  let relay: Relay | null = null;
  server.on("upgrade", (request, socket, head) => {
    let pathname = "";
    try {
      pathname = new URL(request.url ?? "/", "http://relay.invalid").pathname;
    } catch {
      socket.destroy();
      return;
    }
    if (pathname !== CONNECT_PATH || relay === null) {
      socket.destroy();
      return;
    }
    relay.accept(request, socket, head);
  });
  await new Promise<void>((resolve) => server.listen({ host: "127.0.0.1", port: 0 }, () => resolve()));
  const address = server.address();
  if (address === null || typeof address === "string") throw new Error("relay did not bind a loopback port");
  relay = new Relay({ upstreamUrl: options.upstreamUrl, port: address.port, server, wss });
  const handle = relay;
  const cleanup = async (): Promise<void> => {
    await handle.close();
  };
  registerCleanup(cleanup);
  const close = handle.close.bind(handle);
  handle.close = async () => {
    unregisterCleanup(cleanup);
    await close();
  };
  return handle;
}
