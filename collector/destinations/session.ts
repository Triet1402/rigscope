/**
 * Per-destination WebSocket session for the durable telemetry protocol.
 *
 * One instance owns exactly one socket and one durable outbox. The session:
 * - sends a hello immediately on open and requires a welcome as the first
 *   inbound message (a hub that answers with something unrecognized is
 *   reported once and then retried with backoff while the outbox keeps
 *   everything),
 * - gives fresh metrics priority through the outbox's dedicated live slot,
 *   reserving one in-flight window slot so a long replay can never starve it,
 * - pumps replay batches through a token bucket, bounded by the hub's
 *   advertised window, the local window, record count and serialized bytes,
 * - serializes durable records exactly as stored, with every current metric
 *   field; there is no wire projection or downgrade,
 * - tracks acknowledgements per batch with a per-batch timeout: accepted and
 *   rejected records leave the outbox, timed-out/disconnected batches are
 *   released for a later retry (the hub deduplicates),
 * - honours `retryAfterMs` by releasing the affected batch and pausing only
 *   replay, never live delivery,
 * - routes detail subscription requests to the composition root (detail data
 *   is request-scoped and never spooled), and
 * - reports disconnects so the caller can cancel that destination's detail
 *   subscriptions.
 *
 * A destination's failures never affect another: every session owns its own
 * socket, timers, reconnect schedule and outbox.
 */
import {
  CAPABILITY_NAMES,
  MAX_BATCH_RECORDS,
  MAX_DETAIL_BYTES,
  MAX_MESSAGE_BYTES,
  parseHubMessage,
} from '../../src/contracts';
import type {
  AckMessage,
  CapabilityName,
  DetailCancel,
  DetailEnded,
  DetailResult,
  DetailSubscribe,
  HelloMessage,
  InventoryMessage,
  RecordRef,
  StatusMessage,
  TelemetryRecord,
  WelcomeMessage,
} from '../../src/contracts';
import type { CollectorIdentity } from '../identity';
import { nextReconnectDelayMs } from './backoff';
import type { BackoffOptions } from './backoff';
import type { DestinationOutbox } from './outbox';

/** Welcome (and handshake) deadline for one destination. */
export const DEFAULT_WELCOME_TIMEOUT_MS = 10_000;
export const DEFAULT_IDLE_TIMEOUT_MS = 45_000;

/**
 * Inbound socket cap (welcomes, acks and detail requests are small). Matches
 * the shared contract's message bound rather than trusting the peer to behave.
 */
export const MAX_SOCKET_INBOUND_BYTES = MAX_MESSAGE_BYTES;

/**
 * Outbound gridlock cap: if the socket has more than this many unsent bytes
 * the hub is not consuming, so the connection is terminated and rescheduled.
 */
export const MAX_UNACKED_SEND_BYTES = 256 * 1024;

/** Structural socket subset of `ws.WebSocket` used by sessions; fakes in tests implement it. */
export type CollectorSocket = {
  readyState: number;
  readonly bufferedAmount: number;
  send(data: string, callback?: (error?: Error) => void): void;
  close(code?: number, reason?: string): void;
  terminate(): void;
  on(event: string, listener: (...args: unknown[]) => void): unknown;
};

export type SessionSocketOptions = {
  headers: Record<string, string>;
  handshakeTimeoutMs: number;
  maxPayloadBytes: number;
};

export type SocketFactory = (
  url: string,
  options: SessionSocketOptions,
) => CollectorSocket;

export const DEFAULT_ACK_TIMEOUT_MS = 30_000;
export const DEFAULT_REPLAY_BATCHES_PER_SECOND = 4;
export const DEFAULT_MAX_LOCAL_IN_FLIGHT_BATCHES = 8;
export const DEFAULT_MAX_BATCH_BYTES = 200_000;
/** How often retention limits are applied while the session is running. */
export const OUTBOX_ENFORCE_INTERVAL_MS = 10_000;

/** Outbound gridlock cap. Control messages are dropped above it; samples fail the socket. */
const MAX_OUTBOUND_BUFFERED_BYTES = MAX_UNACKED_SEND_BYTES;
/** Slack for the batch envelope/commas when asking the outbox for bytes. */
const BATCH_ENVELOPE_BYTES = 4_096;

const MIN_HELLO_INTERVAL_MS = 100;
const MAX_HELLO_INTERVAL_MS = 7_200_000;

const SOCKET_OPEN = 1;
/** A connection must stay healthy this long before backoff attempts reset. */
const HEALTHY_CONNECTION_RESET_MS = 30_000;

const INCOMPATIBLE_HUB_LOG = 'hub sent an unrecognized welcome; the hub and collector builds must match';

export type DestinationSessionOptions = {
  /** Human label for logs, e.g. "hub0". */
  name: string;
  url: string;
  /** Bearer token; only ever placed into the Authorization header. */
  token: string;
  identity: CollectorIdentity;
  outbox: DestinationOutbox;
  /** Nominal delivery cadence advertised in hello (milliseconds). */
  expectedIntervalMs: number;
  capabilities: CapabilityName[];
  socketFactory: SocketFactory;
  welcomeTimeoutMs?: number;
  idleTimeoutMs?: number;
  ackTimeoutMs?: number;
  replayBatchesPerSecond?: number;
  maxLocalInFlightBatches?: number;
  /** Must stay below MAX_MESSAGE_BYTES. */
  maxBatchBytes?: number;
  backoff?: BackoffOptions;
  now?: () => number;
  rand?: () => number;
  onEvent?: (line: string) => void;
  onDetailRequest?: (message: DetailSubscribe | DetailCancel) => void;
  onWelcome?: (welcome: WelcomeMessage) => void;
  onDisconnect?: () => void;
  /**
   * Where the current inventory snapshot comes from. Called once after every
   * welcome so the hub always receives an inventory filtered for this
   * connection by its owner; when absent, the last remembered
   * `sendInventory` snapshot is re-announced instead.
   */
  inventoryProvider?: () => InventoryMessage;
};

export type SessionStatus = 'idle' | 'connecting' | 'welcomed' | 'stopped';

export type SessionState = {
  status: SessionStatus;
  attempts: number;
  reconnectScheduled: boolean;
  welcomeTimerActive: boolean;
  welcomed: boolean;
  inFlightBatches: number;
  inFlightWindow: number;
  replayPausedMs: number;
  pendingRecords: number;
  lastError: string | null;
};

type InFlightBatch = {
  refs: RecordRef[];
  keys: Set<string>;
  mode: 'live' | 'replay';
  timer: ReturnType<typeof setTimeout>;
};

function inboundText(data: unknown): string {
  if (typeof data === 'string') return data;
  if (data instanceof ArrayBuffer) return Buffer.from(data).toString('utf8');
  if (data instanceof Uint8Array) return Buffer.from(data).toString('utf8');
  if (Array.isArray(data)) {
    return Buffer.concat(data.map((part) => Buffer.from(part as Uint8Array))).toString('utf8');
  }
  return '';
}

function refKey(ref: RecordRef): string {
  return `${ref.runId}\u0000${ref.sequence}`;
}

function toRef(record: TelemetryRecord): RecordRef {
  return { runId: record.runId, sequence: record.sequence };
}

/** Structurally welcome-shaped inbound message, regardless of fields. */
function looksLikeWelcome(value: unknown): boolean {
  return (
    typeof value === 'object' && value !== null && !Array.isArray(value) &&
    (value as { type?: unknown }).type === 'welcome'
  );
}

function assertExpectedIntervalMs(value: number): void {
  if (!Number.isSafeInteger(value) || value < MIN_HELLO_INTERVAL_MS || value > MAX_HELLO_INTERVAL_MS) {
    throw new Error(
      `expectedIntervalMs must be a safe integer between ${MIN_HELLO_INTERVAL_MS} and ${MAX_HELLO_INTERVAL_MS}, got ${String(value)}`,
    );
  }
}

function assertCapabilities(capabilities: readonly CapabilityName[]): void {
  if (capabilities.length === 0 || !capabilities.includes('metrics')) {
    throw new Error('capabilities must include metrics');
  }
  if (new Set(capabilities).size !== capabilities.length) {
    throw new Error('capabilities must be unique');
  }
  for (const capability of capabilities) {
    if (!(CAPABILITY_NAMES as readonly string[]).includes(capability)) {
      throw new Error(`unsupported capability ${String(capability)}`);
    }
  }
}

export class DestinationSession {
  private readonly name: string;
  private readonly url: string;
  private readonly token: string;
  private readonly identity: CollectorIdentity;
  private readonly outbox: DestinationOutbox;
  private readonly expectedIntervalMs: number;
  private readonly capabilities: CapabilityName[];
  private readonly socketFactory: SocketFactory;
  private readonly welcomeTimeoutMs: number;
  private readonly idleTimeoutMs: number;
  private readonly ackTimeoutMs: number;
  private readonly replayBatchesPerSecond: number;
  private readonly maxLocalInFlightBatches: number;
  private readonly maxBatchBytes: number;
  private readonly backoff: BackoffOptions;
  private readonly now: () => number;
  private readonly rand: () => number;
  private readonly onEvent: (line: string) => void;
  private readonly onDetailRequest: ((message: DetailSubscribe | DetailCancel) => void) | undefined;
  private readonly onWelcome: ((welcome: WelcomeMessage) => void) | undefined;
  private readonly onDisconnect: (() => void) | undefined;
  private readonly inventoryProvider: (() => InventoryMessage) | undefined;

  private status: SessionStatus = 'idle';
  private socket: CollectorSocket | null = null;
  /** Increments per socket generation so stale listeners are ignored. */
  private epoch = 0;
  private failedEpoch = -1;
  private welcomeTimer: ReturnType<typeof setTimeout> | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private idleTimer: ReturnType<typeof setTimeout> | null = null;
  private replayTimer: ReturnType<typeof setTimeout> | null = null;
  private enforceTimer: ReturnType<typeof setInterval> | null = null;
  private attempts = 0;
  private welcomedAtMs = 0;
  private stopping = false;
  private lastError: string | null = null;
  private reportedIncompatibleHub = false;

  /** min(hub window, local window); recomputed on each welcome. */
  private inFlightWindow = 1;
  private readonly inFlight = new Map<number, InFlightBatch>();
  private nextBatchId = 1;

  /** One token per batch; capacity 1 keeps the rate itself strictly bounded. */
  private replayTokens = 1;
  private lastTokenRefillMs = 0;
  private replayPausedUntilMs = 0;

  private lastInventory: InventoryMessage | null = null;

  constructor(options: DestinationSessionOptions) {
    assertExpectedIntervalMs(options.expectedIntervalMs);
    assertCapabilities(options.capabilities);
    const maxBatchBytes = options.maxBatchBytes ?? DEFAULT_MAX_BATCH_BYTES;
    if (!Number.isSafeInteger(maxBatchBytes) || maxBatchBytes < 1 || maxBatchBytes >= MAX_MESSAGE_BYTES) {
      throw new Error(`maxBatchBytes must be a safe integer below ${MAX_MESSAGE_BYTES}, got ${String(maxBatchBytes)}`);
    }
    const ackTimeoutMs = options.ackTimeoutMs ?? DEFAULT_ACK_TIMEOUT_MS;
    if (!Number.isFinite(ackTimeoutMs) || ackTimeoutMs <= 0) {
      throw new Error('ackTimeoutMs must be positive');
    }
    const replayBatchesPerSecond = options.replayBatchesPerSecond ?? DEFAULT_REPLAY_BATCHES_PER_SECOND;
    if (!Number.isFinite(replayBatchesPerSecond) || replayBatchesPerSecond <= 0) {
      throw new Error('replayBatchesPerSecond must be positive');
    }
    const maxLocalInFlightBatches = options.maxLocalInFlightBatches ?? DEFAULT_MAX_LOCAL_IN_FLIGHT_BATCHES;
    if (!Number.isSafeInteger(maxLocalInFlightBatches) || maxLocalInFlightBatches < 1) {
      throw new Error('maxLocalInFlightBatches must be a positive safe integer');
    }

    this.name = options.name;
    this.url = options.url;
    this.token = options.token;
    this.identity = options.identity;
    this.outbox = options.outbox;
    this.expectedIntervalMs = options.expectedIntervalMs;
    this.capabilities = [...options.capabilities];
    this.socketFactory = options.socketFactory;
    this.welcomeTimeoutMs = options.welcomeTimeoutMs ?? DEFAULT_WELCOME_TIMEOUT_MS;
    this.idleTimeoutMs = options.idleTimeoutMs ?? DEFAULT_IDLE_TIMEOUT_MS;
    this.ackTimeoutMs = ackTimeoutMs;
    this.replayBatchesPerSecond = replayBatchesPerSecond;
    this.maxLocalInFlightBatches = maxLocalInFlightBatches;
    this.maxBatchBytes = maxBatchBytes;
    this.backoff = options.backoff ?? {};
    this.now = options.now ?? Date.now;
    this.rand = options.rand ?? Math.random;
    this.onEvent = options.onEvent ?? (() => {});
    this.onDetailRequest = options.onDetailRequest;
    this.onWelcome = options.onWelcome;
    this.onDisconnect = options.onDisconnect;
    this.inventoryProvider = options.inventoryProvider;
    this.inFlightWindow = maxLocalInFlightBatches;
    this.lastTokenRefillMs = this.now();
  }

  start(): void {
    if (this.stopping || this.status !== 'idle') return;
    this.enforceTimer = setInterval(() => {
      try {
        this.outbox.enforceLimits();
      } catch {
        // Retention is best effort; never let it kill the session.
      }
    }, OUTBOX_ENFORCE_INTERVAL_MS);
    this.enforceTimer.unref?.();
    this.openSocket();
  }

  /** Observable state for tests and operational logging. */
  describeState(): SessionState {
    return {
      status: this.status,
      attempts: this.attempts,
      reconnectScheduled: this.reconnectTimer !== null,
      welcomeTimerActive: this.welcomeTimer !== null,
      welcomed: this.status === 'welcomed',
      inFlightBatches: this.inFlight.size,
      inFlightWindow: this.inFlightWindow,
      replayPausedMs: Math.max(0, this.replayPausedUntilMs - this.now()),
      pendingRecords: this.outbox.status().pendingRecords,
      lastError: this.lastError,
    };
  }

  /**
   * Called by the composition root right after appending a fresh record.
   * Sends it as a single-record live batch when the window has room; returns
   * false (record stays pending) otherwise.
   */
  deliverLive(): boolean {
    return this.sendLiveBatch();
  }

  /** Remember and (when welcomed) send an inventory snapshot. Not queued. */
  sendInventory(message: InventoryMessage): boolean {
    this.lastInventory = message;
    return this.sendControlMessage(message);
  }

  /** Send a status snapshot. Not queued. */
  sendStatus(message: StatusMessage): boolean {
    return this.sendControlMessage(message);
  }

  /** Send request-scoped process detail. Never queued or spooled. */
  sendDetail(message: DetailResult | DetailEnded): boolean {
    if (!this.canSend()) return false;
    const text = JSON.stringify(message);
    const bytes = Buffer.byteLength(text, 'utf8');
    if (bytes > MAX_DETAIL_BYTES) {
      this.log(`dropping oversized detail message (${bytes} bytes > ${MAX_DETAIL_BYTES})`);
      return false;
    }
    return this.sendOnSocket(text);
  }

  stop(): void {
    if (this.stopping) return;
    this.stopping = true;
    this.status = 'stopped';
    this.clearTimers();
    for (const batch of this.inFlight.values()) clearTimeout(batch.timer);
    this.inFlight.clear();
    this.outbox.releaseAll();
    const socket = this.socket;
    this.socket = null;
    if (socket !== null) {
      try {
        socket.close(1000, 'collector stopping');
      } catch {
        // Already gone; nothing to do during shutdown.
      }
    }
  }

  // -------------------------------------------------------------------------
  // Connection lifecycle
  // -------------------------------------------------------------------------

  private clearTimers(): void {
    if (this.idleTimer !== null) {
      clearTimeout(this.idleTimer);
      this.idleTimer = null;
    }
    if (this.welcomeTimer !== null) {
      clearTimeout(this.welcomeTimer);
      this.welcomeTimer = null;
    }
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.replayTimer !== null) {
      clearTimeout(this.replayTimer);
      this.replayTimer = null;
    }
    if (this.enforceTimer !== null) {
      clearInterval(this.enforceTimer);
      this.enforceTimer = null;
    }
  }

  private scheduleReconnect(delayMs: number): void {
    if (this.stopping) return;
    if (this.reconnectTimer !== null) clearTimeout(this.reconnectTimer);
    this.status = 'connecting';
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.openSocket();
    }, Math.max(0, Math.round(delayMs)));
    this.reconnectTimer.unref?.();
  }

  private openSocket(): void {
    if (this.stopping) return;
    const epoch = ++this.epoch;
    this.failedEpoch = -1;
    this.status = 'connecting';

    let socket: CollectorSocket;
    try {
      socket = this.socketFactory(this.url, {
        headers: { Authorization: `Bearer ${this.token}` },
        handshakeTimeoutMs: this.welcomeTimeoutMs,
        maxPayloadBytes: MAX_SOCKET_INBOUND_BYTES,
      } satisfies SessionSocketOptions);
    } catch (error) {
      this.fail(`socket error: ${error instanceof Error ? error.message : 'unknown'}`);
      return;
    }
    this.socket = socket;
    socket.on('open', () => this.handleOpen(epoch));
    socket.on('message', (data: unknown) => this.handleMessage(epoch, data));
    socket.on('ping', () => {
      if (epoch === this.epoch && this.status === 'welcomed') this.refreshIdleDeadline();
    });
    socket.on('error', (error: unknown) =>
      this.handleClose(epoch, error instanceof Error ? error.message : 'socket error'),
    );
    socket.on('close', (...args: unknown[]) => {
      const code = typeof args[0] === 'number' ? args[0] : undefined;
      if (code === 1002 && !this.describeWelcomed(epoch)) this.reportIncompatibleHub();
      this.handleClose(epoch, code === 1002 ? 'protocol error' : 'connection closed');
    });

    this.welcomeTimer = setTimeout(() => {
      this.welcomeTimer = null;
      if (epoch === this.epoch) this.fail('welcome timed out');
    }, this.welcomeTimeoutMs);
    this.welcomeTimer.unref?.();
  }

  private describeWelcomed(epoch: number): boolean {
    return epoch === this.epoch && this.status === 'welcomed';
  }

  private handleOpen(epoch: number): void {
    if (this.stopping || epoch !== this.epoch) return;
    const socket = this.socket;
    if (socket === null) return;
    try {
      socket.send(JSON.stringify(this.buildHello()));
    } catch {
      this.fail('failed to send hello');
    }
  }

  private buildHello(): HelloMessage {
    return {
      type: 'hello',
      collectorId: this.identity.collectorId,
      runId: this.identity.runId,
      bootId: this.identity.bootId,
      hostname: this.identity.hostname,
      capabilities: [...this.capabilities],
      expectedIntervalMs: this.expectedIntervalMs,
    };
  }

  private handleMessage(epoch: number, data: unknown): void {
    if (this.stopping || epoch !== this.epoch || this.socket === null) return;
    let parsed: unknown;
    try {
      parsed = JSON.parse(inboundText(data)) as unknown;
    } catch {
      if (this.status === 'welcomed') {
        this.log('ignoring invalid JSON from hub');
        return;
      }
      this.fail('first message was not valid JSON');
      return;
    }

    if (this.status !== 'welcomed') {
      let message: unknown;
      try {
        message = parseHubMessage(parsed);
      } catch (error) {
        const detail = error instanceof Error ? error.message : 'invalid welcome';
        // Strict welcome parsing rejects unknown shapes; a structurally
        // welcome-looking inbound message that fails means the hub build does
        // not match this collector.
        if (looksLikeWelcome(parsed)) {
          this.reportIncompatibleHub();
          this.fail('hub answered with an unrecognized welcome');
          return;
        }
        this.fail(`rejected welcome: ${detail}`);
        return;
      }
      if (typeof message !== 'object' || message === null || (message as { type?: unknown }).type !== 'welcome') {
        this.fail('first message is not a welcome');
        return;
      }
      this.handleWelcome(message as WelcomeMessage);
      return;
    }

    this.refreshIdleDeadline();
    let message: unknown;
    try {
      message = parseHubMessage(parsed);
    } catch (error) {
      this.log(`ignoring invalid hub message: ${error instanceof Error ? error.message : 'invalid'}`);
      return;
    }
    const typed = message as { type: string };
    switch (typed.type) {
      case 'ack':
        this.handleAck(message as AckMessage);
        return;
      case 'detail.subscribe':
      case 'detail.cancel': {
        const detail = message as DetailSubscribe | DetailCancel;
        if (this.onDetailRequest !== undefined) {
          try {
            this.onDetailRequest(detail);
          } catch (error) {
            this.log(`detail request handler failed: ${error instanceof Error ? error.message : 'unknown'}`);
          }
        }
        return;
      }
      case 'welcome':
        this.log('ignoring duplicate welcome');
        return;
      default:
        return;
    }
  }

  private handleWelcome(welcome: WelcomeMessage): void {
    if (this.welcomeTimer !== null) {
      clearTimeout(this.welcomeTimer);
      this.welcomeTimer = null;
    }
    this.status = 'welcomed';
    this.welcomedAtMs = this.now();
    this.inFlightWindow = Math.max(1, Math.min(welcome.maxInFlightBatches, this.maxLocalInFlightBatches));
    this.replayTokens = 1;
    this.lastTokenRefillMs = this.now();
    this.lastError = null;
    this.refreshIdleDeadline();
    this.log(
      `welcomed (hubRunId=${welcome.hubRunId} machineId=${welcome.machineId} maxInFlightBatches=${welcome.maxInFlightBatches})`,
    );
    if (this.onWelcome !== undefined) {
      try {
        this.onWelcome(welcome);
      } catch (error) {
        this.log(`welcome handler failed: ${error instanceof Error ? error.message : 'unknown'}`);
      }
    }
    // Inventory is connection-scoped state: re-announce after every welcome.
    this.announceInventory();
    // A live record may have been appended while disconnected.
    this.sendLiveBatch();
    this.pumpReplay();
  }

  /** Announce this connection's inventory: pulled fresh when owned, otherwise the remembered snapshot. */
  private announceInventory(): void {
    if (this.inventoryProvider !== undefined) {
      let message: InventoryMessage | null = null;
      try {
        message = this.inventoryProvider();
      } catch {
        // A failing provider falls back to the remembered snapshot below.
      }
      if (message !== null) {
        this.sendInventory(message);
        return;
      }
    }
    if (this.lastInventory !== null) this.sendInventory(this.lastInventory);
  }

  private handleClose(epoch: number, reason: string): void {
    if (this.stopping || epoch !== this.epoch) return;
    this.fail(reason);
  }

  private refreshIdleDeadline(): void {
    if (this.idleTimer !== null) clearTimeout(this.idleTimer);
    this.idleTimer = setTimeout(() => {
      this.idleTimer = null;
      this.fail('hub heartbeat timed out');
    }, this.idleTimeoutMs);
    this.idleTimer.unref?.();
  }

  private reportIncompatibleHub(): void {
    if (this.reportedIncompatibleHub) return;
    this.reportedIncompatibleHub = true;
    this.log(INCOMPATIBLE_HUB_LOG);
  }

  private fail(reason: string): void {
    if (this.stopping || this.failedEpoch === this.epoch) return;
    this.failedEpoch = this.epoch;
    this.lastError = reason;
    if (this.idleTimer !== null) {
      clearTimeout(this.idleTimer);
      this.idleTimer = null;
    }
    if (this.welcomeTimer !== null) {
      clearTimeout(this.welcomeTimer);
      this.welcomeTimer = null;
    }
    if (this.replayTimer !== null) {
      clearTimeout(this.replayTimer);
      this.replayTimer = null;
    }
    const wasWelcomed = this.status === 'welcomed';
    const socket = this.socket;
    this.socket = null;
    if (socket !== null) {
      try {
        socket.terminate();
      } catch {
        // The socket is already unusable.
      }
    }

    // Everything sent on this connection is unresolved: make it pending again
    // so the next connection resends it. The hub deduplicates by runId+sequence.
    for (const batch of this.inFlight.values()) clearTimeout(batch.timer);
    this.inFlight.clear();
    this.outbox.releaseAll();
    this.status = 'connecting';
    this.replayTokens = 1;
    this.lastTokenRefillMs = this.now();

    if (wasWelcomed && this.onDisconnect !== undefined) {
      try {
        this.onDisconnect();
      } catch (error) {
        this.log(`disconnect handler failed: ${error instanceof Error ? error.message : 'unknown'}`);
      }
    }

    if (wasWelcomed && this.now() - this.welcomedAtMs >= HEALTHY_CONNECTION_RESET_MS) {
      this.attempts = 0;
    } else {
      this.attempts += 1;
    }
    const delayMs = nextReconnectDelayMs(this.attempts, this.rand, this.backoff);
    this.log(`disconnected (${reason}); reconnecting in ${delayMs}ms (attempt ${this.attempts})`);
    this.scheduleReconnect(delayMs);
  }

  // -------------------------------------------------------------------------
  // Delivery
  // -------------------------------------------------------------------------

  /** Window slots reserved for live delivery (one when the window is > 1). */
  private liveReservation(): number {
    return this.inFlightWindow > 1 ? 1 : 0;
  }

  private replayCapacity(): number {
    return Math.max(0, this.inFlightWindow - this.liveReservation());
  }

  private sendLiveBatch(): boolean {
    if (!this.canSend()) return false;
    if (this.inFlight.size >= this.inFlightWindow) return false;
    const record = this.outbox.takeLive();
    if (record === null) return false;
    return this.sendBatch('live', [record]);
  }

  private pumpReplay(): void {
    if (this.stopping || this.status !== 'welcomed') return;
    const time = this.now();
    this.refillTokens(time);
    if (time < this.replayPausedUntilMs) {
      this.scheduleReplay(this.replayPausedUntilMs - time);
      return;
    }
    let guard = 0;
    while (this.inFlight.size < this.replayCapacity() && this.replayTokens >= 1 && guard < MAX_BATCH_RECORDS * 8) {
      guard += 1;
      const records = this.outbox.takeReplay(
        MAX_BATCH_RECORDS,
        Math.max(1, this.maxBatchBytes - BATCH_ENVELOPE_BYTES),
      );
      if (records.length === 0) return;
      if (!this.sendBatch('replay', records)) return;
      this.replayTokens -= 1;
    }
    if (this.replayTokens < 1 && this.inFlight.size < this.replayCapacity()) {
      const waitMs = Math.ceil(((1 - this.replayTokens) / this.replayBatchesPerSecond) * 1_000);
      this.scheduleReplay(waitMs);
    }
  }

  private refillTokens(time: number): void {
    if (time <= this.lastTokenRefillMs) return;
    const elapsedSeconds = (time - this.lastTokenRefillMs) / 1_000;
    this.replayTokens = Math.min(1, this.replayTokens + elapsedSeconds * this.replayBatchesPerSecond);
    this.lastTokenRefillMs = time;
  }

  private scheduleReplay(delayMs: number): void {
    if (this.stopping || this.status !== 'welcomed') return;
    if (this.replayTimer !== null) clearTimeout(this.replayTimer);
    this.replayTimer = setTimeout(() => {
      this.replayTimer = null;
      this.pumpReplay();
    }, Math.max(1, Math.ceil(delayMs)));
    this.replayTimer.unref?.();
  }

  private sendBatch(mode: 'live' | 'replay', records: readonly TelemetryRecord[]): boolean {
    if (!this.canSend()) return false;
    const socket = this.socket;
    if (socket === null) return false;
    if (socket.bufferedAmount > MAX_OUTBOUND_BUFFERED_BYTES) {
      this.fail('outbound buffer exceeded the gridlock cap');
      return false;
    }

    let batch = [...records];
    let message = this.buildSamplesMessage(mode, batch);
    // Trim from the tail (never the oldest record) if the envelope tips the
    // batch over the byte cap. Omitted records stay pending for the next pump.
    while (Buffer.byteLength(message, 'utf8') > this.maxBatchBytes && batch.length > 1) {
      batch = batch.slice(0, -1);
      message = this.buildSamplesMessage(mode, batch);
    }
    const bytes = Buffer.byteLength(message, 'utf8');
    const refs = batch.map(toRef);
    if (bytes > this.maxBatchBytes) {
      // A single record can never fit; it would otherwise block the queue
      // forever. Treat it as rejected locally (never sent, never retried).
      this.log(`dropping unsendable record ${refs[0]?.runId ?? '?'}#${String(refs[0]?.sequence ?? '?')} (${bytes} bytes > ${this.maxBatchBytes})`);
      this.outbox.acknowledge([], refs);
      return false;
    }

    this.outbox.markInFlight(refs);
    const id = this.nextBatchId++;
    const entry: InFlightBatch = {
      refs,
      keys: new Set(refs.map(refKey)),
      mode,
      timer: setTimeout(() => this.handleAckTimeout(id), this.ackTimeoutMs),
    };
    entry.timer.unref?.();
    this.inFlight.set(id, entry);
    try {
      socket.send(message);
    } catch {
      clearTimeout(entry.timer);
      this.inFlight.delete(id);
      this.outbox.release(refs);
      this.fail('outbound send failed');
      return false;
    }
    return true;
  }

  private buildSamplesMessage(mode: 'live' | 'replay', records: readonly TelemetryRecord[]): string {
    // One canonical wire shape: every current metric field is serialized
    // exactly as stored, with no negotiation, projection or downgrade.
    return JSON.stringify({ type: 'samples', mode, records });
  }

  private handleAckTimeout(batchId: number): void {
    const batch = this.inFlight.get(batchId);
    if (batch === undefined) return;
    this.inFlight.delete(batchId);
    this.outbox.release(batch.refs);
    this.log(`ack timed out for ${batch.refs.length} record(s); will resend`);
    this.pumpReplay();
  }

  private handleAck(message: AckMessage): void {
    const accepted: RecordRef[] = message.accepted.map((ref) => ({ runId: ref.runId, sequence: ref.sequence }));
    const rejected: RecordRef[] = message.rejected.map((ref) => ({ runId: ref.runId, sequence: ref.sequence }));
    this.outbox.acknowledge(accepted, rejected);
    if (message.rejected.length > 0) {
      this.log(
        `hub rejected ${message.rejected.length} record(s): ${message.rejected[0]?.reason ?? 'no reason given'}`,
      );
    }

    const ackedKeys = new Set([...accepted, ...rejected].map(refKey));
    const touched: InFlightBatch[] = [];
    for (const [id, batch] of [...this.inFlight.entries()]) {
      let intersects = false;
      for (const key of ackedKeys) {
        if (batch.keys.delete(key)) intersects = true;
      }
      if (intersects) touched.push(batch);
      if (batch.keys.size === 0) {
        clearTimeout(batch.timer);
        this.inFlight.delete(id);
      }
    }

    if (message.retryAfterMs !== undefined) {
      const delay = message.retryAfterMs;
      this.replayPausedUntilMs = Math.max(this.replayPausedUntilMs, this.now() + delay);
      // Release whatever the hub could not take now (its ack may cover part of
      // a batch). With no refs in the ack, pause and release replay batches.
      const toRelease =
        touched.length > 0
          ? touched
          : ackedKeys.size === 0
            ? [...this.inFlight.values()].filter((batch) => batch.mode === 'replay')
            : [];
      for (const batch of toRelease) {
        this.inFlight.delete(this.batchIdOf(batch));
        clearTimeout(batch.timer);
        this.outbox.release(batch.refs.filter((ref) => batch.keys.has(refKey(ref))));
      }
      this.log(`hub asked to retry after ${delay}ms; replay paused`);
    }

    this.pumpReplay();
  }

  private batchIdOf(target: InFlightBatch): number {
    for (const [id, batch] of this.inFlight) if (batch === target) return id;
    return -1;
  }

  // -------------------------------------------------------------------------
  // Control messages
  // -------------------------------------------------------------------------

  private canSend(): boolean {
    return !this.stopping && this.status === 'welcomed' && this.socket !== null && this.socket.readyState === SOCKET_OPEN;
  }

  private sendControlMessage(message: InventoryMessage | StatusMessage): boolean {
    if (!this.canSend()) return false;
    const text = JSON.stringify(message);
    if (Buffer.byteLength(text, 'utf8') > MAX_MESSAGE_BYTES) {
      this.log(`dropping oversized control message (${message.type})`);
      return false;
    }
    return this.sendOnSocket(text);
  }

  private sendOnSocket(text: string): boolean {
    const socket = this.socket;
    if (socket === null || socket.readyState !== SOCKET_OPEN) return false;
    if (socket.bufferedAmount > MAX_OUTBOUND_BUFFERED_BYTES) {
      // Control messages are connection-scoped and must never be queued; a
      // later refresh will be sent on the next tick/welcome.
      this.log('dropping control message (outbound buffer full)');
      return false;
    }
    try {
      socket.send(text);
      return true;
    } catch {
      this.fail('outbound send failed');
      return false;
    }
  }

  private log(line: string): void {
    try {
      this.onEvent(`${this.name}: ${line}`);
    } catch {
      // A misbehaving log sink must not break delivery.
    }
  }
}
