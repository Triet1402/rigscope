#!/usr/bin/env node
/**
 * Isolated sustained scale harness for Rigscope.
 *
 * This is deliberately a standalone script, not an `ops` subcommand: it must
 * never touch a live hub, service socket, or production database. The parent
 * process creates a private temporary directory, writes a throwaway hub
 * configuration, spawns this same file in `hub` child mode, and drives real
 * authenticated HTTP/SSE/WebSocket traffic against that child.
 *
 * What is real:
 * - the hub composition modules (HubState, TelemetryStore, SampleIngestion,
 *   LiveFeed, CollectorGateway, BrowserHttpAuth, MonitorHttp),
 * - the collector WebSocket protocol, including hello/ack/backpressure,
 * - browser login with the normal scrypt verification and login budget,
 * - authenticated SSE streams, including reset-on-reconnect,
 * - authenticated concurrent /api/history requests with the real per-session
 *   token bucket and 25k-row/100 ms query budget,
 * - SQLite WAL, commits, retention preflight and store counters.
 *
 * What is synthetic: only the collector payload values and the sample cadence
 * driver. The report always says so and never implies a capacity guarantee.
 *
 * Usage:
 *   npx tsx scripts/scale-harness.ts --suite --json-out docs/scale-validation.json
 *   npx tsx scripts/scale-harness.ts --case fleet-75m --duration-ms 60000 --keep
 *
 * No package metadata or operational CLI is changed by this file.
 */

import { spawn, type ChildProcess } from "node:child_process";
import { randomUUID } from "node:crypto";
import {
  chmodSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { createServer, get as httpGet, type IncomingMessage, type ServerResponse } from "node:http";
import { cpus, arch, loadavg, platform, release, tmpdir, totalmem } from "node:os";
import { join, resolve } from "node:path";
import { monitorEventLoopDelay, performance } from "node:perf_hooks";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";
import { WebSocket } from "ws";

import {
  COLLECTOR_PROTOCOL_VERSION,
  MAX_BATCH_RECORDS,
  parseHubMessage,
  type HubSnapshot,
  type MetricsPayload,
  type MetricsRecord,
} from "../src/contracts";
import { hashPassword } from "../src/hub/auth/password";
import { BrowserHttpAuth, type AuthOriginRules } from "../src/hub/auth/http";
import { BrowserSessionStore } from "../src/hub/auth/sessions";
import { COLLECTOR_CONNECT_PATH, CollectorGateway, isCollectorUpgradeRequest } from "../src/hub/collector-ws";
import type { HubIdentityConfig } from "../src/hub/config";
import { LiveFeed, type LiveAccess } from "../src/hub/sse";
import { HubState } from "../src/hub/state";
import { SampleIngestion } from "../src/hub/sample-ingestion";
import { MonitorHttp } from "../src/hub/monitor-http";
import { ProcessInterests } from "../src/hub/process-interests";
import { openHubDatabase, type HubDatabase } from "../src/hub/storage/db";
import { TelemetryStore, type TelemetryStoreStats } from "../src/hub/storage/telemetry-store";

const SCRIPT_PATH = fileURLToPath(import.meta.url);
const ROOT = fileURLToPath(new URL("..", import.meta.url));
const TOOL = "rigscope-scale-harness";
const DISCLAIMER =
  "Synthetic bounded workload on an isolated temporary hub/database, not a capacity, latency, or hardware guarantee. Values depend on this host, disk, Node version, payload, cadence, viewer count and concurrent activity.";

// ---------------------------------------------------------------------------
// Shared types
// ---------------------------------------------------------------------------

type Identity = HubIdentityConfig;

type ChildConfig = {
  databasePath: string;
  identities: Identity[];
  users: Array<{ username: string; passwordHash: string; machineIds: "*" | string[] }>;
  maxConnections: number;
  coalesceMs: number;
  maxQueuedRecords: number;
  maxRecordsPerTransaction: number;
  maxPhysicalBytes: number | null;
  feedMaxClients: number;
};

type HubStats = {
  pid: number;
  uptimeMs: number;
  hubRunId: string;
  eventLoopDelayMs: { p50: number; p95: number; p99: number; max: number };
  eventLoopUtilization: number;
  cpuPercent: number;
  cpuMs: { user: number; system: number };
  rssBytes: number;
  rssPeakBytes: number;
  heapUsedBytes: number;
  database: { bytes: number; walBytes: number; mainBytes: number };
  store: TelemetryStoreStats;
  liveClients: number;
  sessions: number;
  connectedMachines: number;
  configuredMachines: number;
  snapshotBytes: number;
};

type PercentileSummary = {
  count: number;
  min: number;
  avg: number;
  p50: number;
  p95: number;
  p99: number;
  max: number;
};

type CaseSpec = {
  name: string;
  description: string;
  machines: number;
  durationMs: number;
  sampleIntervalMs: number;
  recordsPerBatch: number;
  viewers: number;
  sseClients: number;
  slowSseClients: number;
  slowSsePauseAfterMs: number;
  slowSsePauseMs: number;
  historyIntervalMs: number;
  historyMetricSet: "standard" | "broad";
  churnEveryMs: number | null;
  churnMachines: number;
  replayRecords: number;
};

type CaseReport = {
  name: string;
  description: string;
  config: CaseSpec & { warmupMs: number; clientWarmupMs: number; sseWorkers: number; maxCollectorConnections: number; databaseBudgetBytes: number | null };
  host: ReturnType<typeof hostInfo>;
  authorization: {anonymousDenied: boolean; scopedHistory: boolean; scopedSnapshot: boolean; scopedReset: boolean};
  ingestion: {
    recordsSent: number;
    recordsAccepted: number;
    recordsRejected: number;
    replayRecordsSent: number;
    replayRecordsAccepted: number;
    retryAcks: number;
    unackedLiveRecords: number;
    sendErrors: number;
    ackLatencyMs: PercentileSummary;
    sampleToCommitLagMs: PercentileSummary;
    messagesSent: number;
    backpressureSkips: number;
    connects: number;
    reconnects: number;
    reconnectFailures: number;
    collectorRecoveryMs: PercentileSummary;
  };
  history: {
    queries: number;
    ok: number;
    rateLimited: number;
    errors: number;
    latencyMs: PercentileSummary;
    series: PercentileSummary;
    points: PercentileSummary;
    truncated: number;
    tiers: Record<string, number>;
  };
  stream: {
    clients: number;
    httpStatuses: Record<string, number>;
    slowClients: number;
    events: number;
    snapshots: number;
    updates: number;
    resets: number;
    bytes: number;
    eventLagMs: PercentileSummary;
    normalEventLagMs: PercentileSummary;
    slowEventLagMs: PercentileSummary;
    firstEventMs: PercentileSummary;
    reconnects: number;
    resetsAfterReconnect: number;
    slowPauses: number;
    slowDrops: number;
    sseRecoveryMs: PercentileSummary;
  };
  hub: {
    baseline: HubStats | null;
    window: HubStats | null;
    final: HubStats | null;
    databaseGrowth: { bytes: number; walBytes: number; mainBytes: number } | null;
  };
  elapsedMs: number;
  artifacts: { directory: string; kept: boolean; configPath: string; databasePath: string };
};

type SuiteReport = {
  tool: typeof TOOL;
  synthetic: true;
  disclaimer: string;
  generatedAt: string;
  node: string;
  host: ReturnType<typeof hostInfo>;
  command: string;
  suite: CaseSpec[];
  cases: CaseReport[];
  limitations: string[];
};

// ---------------------------------------------------------------------------
// Small utilities
// ---------------------------------------------------------------------------

function sleep(ms: number): Promise<void> {
  return new Promise((resolvePromise) => setTimeout(resolvePromise, ms));
}

function finite(value: unknown, fallback = 0): number {
  return typeof value === "number" && Number.isFinite(value) ? value : fallback;
}

function round(value: number, places = 2): number {
  const factor = 10 ** places;
  return Math.round(value * factor) / factor;
}

function summarize(values: number[]): PercentileSummary {
  if (values.length === 0) return { count: 0, min: 0, avg: 0, p50: 0, p95: 0, p99: 0, max: 0 };
  const sorted = [...values].sort((a, b) => a - b);
  const at = (fraction: number): number =>
    sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * fraction))]!;
  return {
    count: sorted.length,
    min: round(sorted[0]!),
    avg: round(sorted.reduce((sum, value) => sum + value, 0) / sorted.length),
    p50: round(at(0.5)),
    p95: round(at(0.95)),
    p99: round(at(0.99)),
    max: round(sorted[sorted.length - 1]!),
  };
}

function hostInfo() {
  const processors = cpus();
  return {
    platform: platform(),
    release: release(),
    arch: arch(),
    cpuModel: processors[0]?.model ?? "unknown",
    logicalCores: processors.length,
    totalMemoryBytes: totalmem(),
    loadAverage: loadavg().map((value) => round(value)),
    node: process.version,
  };
}

function optionNumber(values: Record<string, unknown>, key: string, fallback: number): number {
  const raw = values[key];
  if (raw === undefined) return fallback;
  const value = typeof raw === "number" ? raw : Number(raw);
  if (!Number.isSafeInteger(value) || value < 0) throw new Error(`--${key} must be a nonnegative safe integer`);
  return value;
}

function optionBoolean(values: Record<string, unknown>, key: string): boolean {
  return values[key] === true;
}

function optionString(values: Record<string, unknown>, key: string): string | undefined {
  const value = values[key];
  if (value === undefined) return undefined;
  if (typeof value !== "string" || value.length === 0) throw new Error(`--${key} requires a value`);
  return value;
}

// ---------------------------------------------------------------------------
// Child mode: isolated hub host
// ---------------------------------------------------------------------------

class HubStatsWindow {
  private readonly delay = monitorEventLoopDelay({ resolution: 10 });
  private lastCpu = process.cpuUsage();
  private lastAt = performance.now();
  private lastElu = performance.eventLoopUtilization();
  private rssPeak = process.memoryUsage().rss;
  private readonly sampler: NodeJS.Timeout;

  constructor() {
    this.delay.enable();
    this.sampler = setInterval(() => {
      this.rssPeak = Math.max(this.rssPeak, process.memoryUsage().rss);
    }, 250);
    this.sampler.unref();
  }

  snapshot(input: {
    database: HubDatabase;
    store: TelemetryStore;
    state: HubState;
    feed: LiveFeed;
    sessions: BrowserSessionStore;
    hubRunId: string;
    reset: boolean;
  }): HubStats {
    const now = performance.now();
    const cpu = process.cpuUsage(this.lastCpu);
    const elapsedMs = Math.max(1, now - this.lastAt);
    const elu = performance.eventLoopUtilization(this.lastElu);
    const memory = process.memoryUsage();
    this.rssPeak = Math.max(this.rssPeak, memory.rss);
    const delay = this.delay;
    const percentile = (fraction: number): number => {
      try {
        return delay.percentile(fraction) / 1e6;
      } catch {
        return 0;
      }
    };
    const storeStats = input.store.stats();
    const result: HubStats = {
      pid: process.pid,
      uptimeMs: round(process.uptime() * 1000),
      hubRunId: input.hubRunId,
      eventLoopDelayMs: {
        p50: round(percentile(50)),
        p95: round(percentile(95)),
        p99: round(percentile(99)),
        max: round(delay.max / 1e6),
      },
      eventLoopUtilization: round(elu.utilization, 4),
      cpuPercent: round(((cpu.user + cpu.system) / 1000 / elapsedMs) * 100),
      cpuMs: { user: round(cpu.user / 1000), system: round(cpu.system / 1000) },
      rssBytes: memory.rss,
      rssPeakBytes: this.rssPeak,
      heapUsedBytes: memory.heapUsed,
      database: {
        bytes: input.database.sizeBytes(),
        walBytes: input.database.walBytes(),
        mainBytes: Math.max(0, input.database.sizeBytes() - input.database.walBytes()),
      },
      store: storeStats,
      liveClients: input.feed.clientCount(),
      sessions: input.sessions.size(),
      connectedMachines: input.state.connectedCount(),
      configuredMachines: input.state.configuredCount(),
      snapshotBytes: Buffer.byteLength(JSON.stringify(input.state.snapshot())),
    };
    if (input.reset) {
      delay.reset();
      this.lastCpu = process.cpuUsage();
      this.lastAt = performance.now();
      this.lastElu = performance.eventLoopUtilization();
      this.rssPeak = memory.rss;
    }
    return result;
  }

  dispose(): void {
    clearInterval(this.sampler);
    this.delay.disable();
  }
}

async function runHubChild(configPath: string): Promise<void> {
  const config = JSON.parse(readFileSync(configPath, "utf8")) as ChildConfig;
  const database = openHubDatabase(config.databasePath);
  const store = new TelemetryStore({
    database,
    maxQueuedRecords: config.maxQueuedRecords,
    maxRecordsPerTransaction: config.maxRecordsPerTransaction,
    flushIntervalMs: 20,
    maxDatabaseBytes: null,
    maxPhysicalBytes: config.maxPhysicalBytes,
  });
  const hubRunId = randomUUID();
  const state = new HubState({ collectors: config.identities, hubRunId });
  const ingestion = new SampleIngestion({ state, storage: store });
  const feed = new LiveFeed(state, { coalesceMs: config.coalesceMs, maxClients: config.feedMaxClients });
  const sessions = new BrowserSessionStore({ ttlMs: 12 * 60 * 60 * 1000 });
  const origin: Extract<AuthOriginRules, { mode?: "loopback" }> = {
    mode: "loopback",
    originPort: 0,
    originHostnames: ["127.0.0.1", "localhost"],
  };
  const auth = new BrowserHttpAuth({ users: config.users, sessions, origin });
  const processes = new ProcessInterests(() => false);
  const monitorHttp = new MonitorHttp({
    auth,
    origin,
    state,
    database,
    storage: store,
    processes,
  });
  const gateway = new CollectorGateway({
    state,
    ingestion,
    maxConnections: config.maxConnections,
  });
  const stats = new HubStatsWindow();

  const server = createServer((req, res) => {
    void dispatch(req, res).catch((error: unknown) => {
      console.error("scale hub request failed:", error);
      if (!res.headersSent) res.writeHead(500, { "Content-Type": "text/plain; charset=utf-8" });
      res.end("scale hub error");
    });
  });

  async function dispatch(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const url = new URL(req.url ?? "/", "http://scale.invalid");
    const pathname = url.pathname;
    if (pathname === "/__scale/stats") {
      const body = stats.snapshot({
        database,
        store,
        state,
        feed,
        sessions,
        hubRunId,
        reset: url.searchParams.get("reset") === "1",
      });
      res.writeHead(200, { "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store" });
      res.end(JSON.stringify(body));
      return;
    }
    if (pathname === "/api/health") {
      res.writeHead(200, { "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store" });
      res.end(JSON.stringify({ status: "ok", mode: "scale-harness", hubRunId }));
      return;
    }
    if (pathname.startsWith("/api/auth/")) {
      const handled = await auth.handleApiAuth(req, res, pathname);
      if (handled) return;
    }
    if (pathname === "/api/live") {
      if (req.headers.origin !== undefined) {
        res.writeHead(403, { "Content-Type": "application/json; charset=utf-8" });
        res.end(JSON.stringify({ error: "origin not allowed" }));
        return;
      }
      if (!auth.configured) {
        res.writeHead(503, { "Content-Type": "application/json; charset=utf-8" });
        res.end(JSON.stringify({ error: "browser access is not configured" }));
        return;
      }
      const session = auth.requestSession(req);
      if (!session) {
        res.writeHead(401, { "Content-Type": "application/json; charset=utf-8" });
        res.end(JSON.stringify({ error: "authentication required" }));
        return;
      }
      if (session.scope !== "*" && session.scope.length === 0) {
        res.writeHead(403, { "Content-Type": "application/json; charset=utf-8" });
        res.end(JSON.stringify({ error: "no machine access is granted to this account" }));
        return;
      }
      if ((req.method ?? "GET") !== "GET") {
        res.writeHead(405, { "Content-Type": "text/plain; charset=utf-8", Allow: "GET" });
        res.end("method not allowed");
        return;
      }
      const access: LiveAccess = {
        sessionId: session.id,
        scope: session.scope === "*" ? "*" : new Set(session.scope),
      };
      feed.attach(req, res, access);
      return;
    }
    if (pathname === COLLECTOR_CONNECT_PATH) {
      gateway.handleHttpRequest(req, res);
      return;
    }
    if (await monitorHttp.handle(req, res, url)) return;
    res.writeHead(404, { "Content-Type": "application/json; charset=utf-8" });
    res.end(JSON.stringify({ error: "not found" }));
  }

  server.on("upgrade", (req, socket, head) => {
    if (isCollectorUpgradeRequest(req)) {
      gateway.handleUpgrade(req, socket, head);
      return;
    }
    socket.destroy();
  });

  let stopping = false;
  const shutdown = async (signal: string): Promise<void> => {
    if (stopping) return;
    stopping = true;
    console.error(`scale hub received ${signal}; shutting down`);
    gateway.stop();
    feed.stop();
    processes.stop();
    const closed = new Promise<void>((resolvePromise) => server.close(() => resolvePromise()));
    server.closeIdleConnections?.();
    server.closeAllConnections?.();
    await Promise.allSettled([closed, store.close()]);
    database.close();
    stats.dispose();
    sessions.dispose();
    process.exit(0);
  };
  process.on("SIGTERM", () => void shutdown("SIGTERM"));
  process.on("SIGINT", () => void shutdown("SIGINT"));

  server.listen(0, "127.0.0.1", () => {
    const address = server.address();
    const port = typeof address === "object" && address !== null ? address.port : 0;
    origin.originPort = port;
    console.log(`SCALE_HUB_READY ${JSON.stringify({ port, pid: process.pid })}`);
  });
}

// ---------------------------------------------------------------------------
// Parent: hub lifecycle
// ---------------------------------------------------------------------------

type HubHandle = {
  child: ChildProcess;
  port: number;
  baseUrl: string;
  stats(reset: boolean): Promise<HubStats>;
  stop(): Promise<void>;
  logs(): string;
};

async function startHubChild(config: ChildConfig, directory: string): Promise<HubHandle> {
  const configPath = join(directory, "hub-config.json");
  writeFileSync(configPath, JSON.stringify(config), { mode: 0o600 });
  chmodSync(configPath, 0o600);
  const env: Record<string, string | undefined> = {};
  for (const [key, value] of Object.entries(process.env)) {
    if (key.startsWith("HUB_") || key.startsWith("COLLECTOR_")) continue;
    env[key] = value;
  }
  env.NODE_ENV = "production";
  const child = spawn(process.execPath, ["--import", "tsx", SCRIPT_PATH, "hub", configPath], {
    cwd: ROOT,
    env: env as NodeJS.ProcessEnv,
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stdoutBuffer = "";
  let stderr = "";
  let port = 0;
  const ready = new Promise<number>((resolvePromise, rejectPromise) => {
    const timer = setTimeout(() => {
      rejectPromise(new Error(`hub child did not become ready within 30s\n${stderr.slice(-4000)}`));
    }, 30_000);
    child.stdout?.setEncoding("utf8");
    child.stdout?.on("data", (chunk: string) => {
      stdoutBuffer += chunk;
      let index = stdoutBuffer.indexOf("\n");
      while (index >= 0) {
        const line = stdoutBuffer.slice(0, index).trim();
        stdoutBuffer = stdoutBuffer.slice(index + 1);
        if (line.startsWith("SCALE_HUB_READY ")) {
          const parsed = JSON.parse(line.slice("SCALE_HUB_READY ".length)) as { port?: number };
          if (typeof parsed.port === "number" && parsed.port > 0) {
            port = parsed.port;
            clearTimeout(timer);
            resolvePromise(port);
          }
        }
        index = stdoutBuffer.indexOf("\n");
      }
    });
    child.stderr?.setEncoding("utf8");
    child.stderr?.on("data", (chunk: string) => {
      stderr += chunk;
      if (stderr.length > 20_000) stderr = stderr.slice(-20_000);
    });
    child.once("exit", (code, signal) => {
      clearTimeout(timer);
      if (port === 0) rejectPromise(new Error(`hub child exited before ready (code=${code}, signal=${signal})\n${stderr.slice(-4000)}`));
    });
  });
  try {
    await ready;
  } catch (error) {
    child.kill("SIGKILL");
    throw error;
  }
  const baseUrl = `http://127.0.0.1:${port}`;
  let stopped = false;
  return {
    child,
    port,
    baseUrl,
    stats: async (reset: boolean): Promise<HubStats> => {
      const response = await fetch(`${baseUrl}/__scale/stats${reset ? "?reset=1" : ""}`, {
        signal: AbortSignal.timeout(5_000),
      });
      if (response.status !== 200) throw new Error(`stats request returned ${response.status}`);
      return (await response.json()) as HubStats;
    },
    stop: async (): Promise<void> => {
      if (stopped) return;
      stopped = true;
      if (child.exitCode !== null) return;
      child.kill("SIGTERM");
      await Promise.race([
        new Promise<void>((resolvePromise) => child.once("exit", () => resolvePromise())),
        sleep(8_000),
      ]);
      if (child.exitCode === null) child.kill("SIGKILL");
      await Promise.race([
        new Promise<void>((resolvePromise) => child.once("exit", () => resolvePromise())),
        sleep(2_000),
      ]);
    },
    logs: () => stderr,
  };
}

// ---------------------------------------------------------------------------
// Parent: collector WebSocket simulator
// ---------------------------------------------------------------------------

class CollectorSim {
  readonly index: number;
  readonly identity: Identity;
  readonly intervalMs: number;
  readonly recordsPerBatch: number;
  readonly maxInFlight = 8;
  readonly payload: MetricsPayload;

  private ws: WebSocket | null = null;
  private runId = "";
  private bootId = "";
  private sequence = 0;
  private inFlight = 0;
  private sending = false;
  private stopping = false;
  private reconnecting = false;
  private loop: NodeJS.Timeout | null = null;
  private welcomeResolve: ((value: void) => void) | null = null;
  private welcomeReject: ((error: Error) => void) | null = null;
  private readonly liveBatches = new Map<
    string,
    { firstKey: string; remaining: number; sentAt: number; capturedAtMs: number }
  >();
  private readonly refToBatch = new Map<string, string>();
  private readonly replayWaiters = new Set<string>();
  private reconnectStartedAt = 0;
  private recovering = false;
  private replayCounter = 0;

  recordsSent = 0;
  recordsAccepted = 0;
  recordsRejected = 0;
  replayRecordsSent = 0;
  replayRecordsAccepted = 0;
  retryAcks = 0;
  sendErrors = 0;
  messagesSent = 0;
  backpressureSkips = 0;
  connects = 0;
  reconnects = 0;
  reconnectFailures = 0;
  readonly ackLatencyMs: number[] = [];
  readonly sampleToCommitLagMs: number[] = [];
  readonly collectorRecoveryMs: number[] = [];

  constructor(index: number, identity: Identity, options: { intervalMs: number; recordsPerBatch: number }) {
    this.index = index;
    this.identity = identity;
    this.intervalMs = options.intervalMs;
    this.recordsPerBatch = options.recordsPerBatch;
    this.payload = buildMetricsPayload(index);
    this.bootId = `boot-${index}-${randomUUID().slice(0, 8)}`;
  }

  async connect(): Promise<void> {
    if (this.stopping) throw new Error("collector is stopping");
    this.runId = `run-${this.index}-${Date.now()}-${randomUUID().slice(0, 6)}`;
    this.sequence = 0;
    const ws = new WebSocket(`ws://127.0.0.1:${this.port ?? 0}${COLLECTOR_CONNECT_PATH}`, {
      headers: { Authorization: `Bearer ${this.identity.token}` },
      perMessageDeflate: false,
      handshakeTimeout: 10_000,
    });
    this.ws = ws;
    const welcome = new Promise<void>((resolvePromise, rejectPromise) => {
      this.welcomeResolve = resolvePromise;
      this.welcomeReject = rejectPromise;
    });
    ws.on("open", () => {
      this.connects += 1;
      this.sendRaw({
        type: "hello",
        collectorId: this.identity.collectorId,
        runId: this.runId,
        bootId: this.bootId,
        hostname: `scale-host-${this.index}`,
        capabilities: ["metrics"],
        expectedIntervalMs: this.intervalMs,
        collectorVersion: COLLECTOR_PROTOCOL_VERSION,
      });
    });
    ws.on("message", (data) => {
      try {
        const message = parseHubMessage(JSON.parse(String(data)));
        if (message.type === "welcome") {
          this.welcomeResolve?.();
          this.welcomeResolve = null;
          this.welcomeReject = null;
          this.startLoop();
        } else if (message.type === "ack") {
          this.handleAck(message);
        }
      } catch (error) {
        this.sendErrors += 1;
        this.welcomeReject?.(error instanceof Error ? error : new Error(String(error)));
      }
    });
    ws.on("error", (error) => {
      this.sendErrors += 1;
      this.welcomeReject?.(error instanceof Error ? error : new Error(String(error)));
    });
    ws.on("close", () => {
      this.stopLoop();
      this.welcomeReject?.(new Error("collector socket closed before welcome"));
      this.welcomeReject = null;
      this.welcomeResolve = null;
      if (!this.stopping && !this.reconnecting) {
        // Unexpected drop: recover automatically so one bad socket does not
        // silently reduce the measured fleet.
        setTimeout(() => {
          void this.reconnect(false).catch(() => {
            // reconnect() already recorded the failure.
          });
        }, 500);
      }
    });
    await Promise.race([
      welcome,
      sleep(10_000).then(() => {
        throw new Error(`collector ${this.index} welcome timeout`);
      }),
    ]);
  }

  port: number | null = null;

  private startLoop(): void {
    if (this.loop !== null || this.stopping) return;
    this.sending = true;
    this.sendLive();
    this.loop = setInterval(() => this.sendLive(), this.intervalMs);
  }

  private stopLoop(): void {
    this.sending = false;
    if (this.loop !== null) {
      clearInterval(this.loop);
      this.loop = null;
    }
  }

  /** Stop the live cadence while keeping the socket open for final acks. */
  stopSending(): void {
    this.stopLoop();
  }

  private sendRaw(value: unknown): void {
    if (this.ws === null || this.ws.readyState !== WebSocket.OPEN) return;
    try {
      this.ws.send(JSON.stringify(value));
      this.messagesSent += 1;
    } catch {
      this.sendErrors += 1;
    }
  }

  private sendLive(): void {
    if (!this.sending || this.stopping) return;
    if (this.inFlight >= this.maxInFlight) {
      this.backpressureSkips += 1;
      return;
    }
    const batch = Math.max(1, Math.min(MAX_BATCH_RECORDS, this.recordsPerBatch));
    const records: MetricsRecord[] = [];
    for (let index = 0; index < batch; index += 1) {
      this.sequence += 1;
      records.push({
        collectorId: this.identity.collectorId,
        runId: this.runId,
        bootId: this.bootId,
        sequence: this.sequence,
        capturedAt: new Date().toISOString(),
        kind: "metrics",
        payload: this.payload,
      });
    }
    const first = records[0]!;
    const batchKey = `${first.runId}\u0000${first.sequence}`;
    this.liveBatches.set(batchKey, {
      firstKey: batchKey,
      remaining: records.length,
      sentAt: performance.now(),
      capturedAtMs: Date.parse(first.capturedAt),
    });
    for (const record of records) {
      this.refToBatch.set(`${record.runId}\u0000${record.sequence}`, batchKey);
    }
    this.inFlight += 1;
    this.recordsSent += records.length;
    this.sendRaw({ type: "samples", mode: "live", records });
  }

  sendReplay(count: number): void {
    if (this.stopping || count <= 0) return;
    this.replayCounter += 1;
    const replayRunId = `replay-${this.index}-${this.replayCounter}-${Date.now()}`;
    const records: MetricsRecord[] = [];
    const now = Date.now();
    const total = Math.min(count, MAX_BATCH_RECORDS * 4);
    for (let index = 0; index < total; index += 1) {
      const capturedAtMs = now - (total - index) * 1_000;
      records.push({
        collectorId: this.identity.collectorId,
        runId: replayRunId,
        bootId: this.bootId,
        sequence: index + 1,
        capturedAt: new Date(capturedAtMs).toISOString(),
        kind: "metrics",
        payload: this.payload,
      });
      this.replayWaiters.add(`${replayRunId}\u0000${index + 1}`);
    }
    for (let offset = 0; offset < records.length; offset += MAX_BATCH_RECORDS) {
      const chunk = records.slice(offset, offset + MAX_BATCH_RECORDS);
      this.replayRecordsSent += chunk.length;
      this.sendRaw({ type: "samples", mode: "replay", records: chunk });
    }
  }

  private handleAck(message: {
    type: "ack";
    accepted: Array<{ runId: string; sequence: number }>;
    rejected: Array<{ runId: string; sequence: number; reason: string }>;
    retryAfterMs?: number;
  }): void {
    if (message.retryAfterMs !== undefined) {
      this.retryAcks += 1;
      // The hub rejected the whole batch before commit. Drop outstanding live
      // state; the next cadence tick starts a fresh sequence.
      this.liveBatches.clear();
      this.refToBatch.clear();
      this.inFlight = 0;
      return;
    }
    for (const ref of message.accepted) {
      const key = `${ref.runId}\u0000${ref.sequence}`;
      if (this.replayWaiters.delete(key)) {
        this.replayRecordsAccepted += 1;
        continue;
      }
      const batchKey = this.refToBatch.get(key);
      if (batchKey === undefined) continue;
      this.refToBatch.delete(key);
      const batch = this.liveBatches.get(batchKey);
      if (batch === undefined) continue;
      this.recordsAccepted += 1;
      batch.remaining -= 1;
      if (batch.remaining <= 0) {
        this.liveBatches.delete(batchKey);
        this.inFlight = Math.max(0, this.inFlight - 1);
        const now = performance.now();
        this.ackLatencyMs.push(now - batch.sentAt);
        this.sampleToCommitLagMs.push(Date.now() - batch.capturedAtMs);
        if (this.recovering) {
          this.recovering = false;
          this.collectorRecoveryMs.push(now - this.reconnectStartedAt);
        }
      }
    }
    for (const ref of message.rejected) {
      const key = `${ref.runId}\u0000${ref.sequence}`;
      if (this.replayWaiters.delete(key)) {
        this.recordsRejected += 1;
        continue;
      }
      const batchKey = this.refToBatch.get(key);
      if (batchKey === undefined) continue;
      this.refToBatch.delete(key);
      const batch = this.liveBatches.get(batchKey);
      if (batch === undefined) continue;
      this.recordsRejected += 1;
      batch.remaining -= 1;
      if (batch.remaining <= 0) {
        this.liveBatches.delete(batchKey);
        this.inFlight = Math.max(0, this.inFlight - 1);
      }
    }
  }

  async reconnect(replay: boolean): Promise<void> {
    if (this.stopping) return;
    this.reconnecting = true;
    this.stopLoop();
    const started = performance.now();
    await this.closeSocket();
    try {
      await this.connect();
      this.reconnects += 1;
      this.recovering = true;
      this.reconnectStartedAt = started;
      if (replay) this.sendReplay(this.replayRecords ?? 0);
      this.sendLive();
    } catch (error) {
      this.reconnectFailures += 1;
      throw error;
    } finally {
      this.reconnecting = false;
    }
  }

  replayRecords = 0;

  private async closeSocket(): Promise<void> {
    const ws = this.ws;
    this.ws = null;
    if (ws === null) return;
    if (ws.readyState === WebSocket.CLOSED) return;
    await new Promise<void>((resolvePromise) => {
      const timer = setTimeout(() => resolvePromise(), 1_000);
      ws.once("close", () => {
        clearTimeout(timer);
        resolvePromise();
      });
      try {
        ws.close();
      } catch {
        clearTimeout(timer);
        resolvePromise();
      }
    });
    try {
      ws.terminate();
    } catch {
      // already closed
    }
  }

  async stop(): Promise<void> {
    this.stopping = true;
    this.stopLoop();
    await this.closeSocket();
  }

  unackedLiveRecords(): number {
    let total = 0;
    for (const batch of this.liveBatches.values()) total += batch.remaining;
    return total;
  }
}

// ---------------------------------------------------------------------------
// Parent: authenticated SSE viewer
// ---------------------------------------------------------------------------

class SseClient {
  readonly id: number;
  readonly cookie: string;
  readonly baseUrl: string;
  readonly slow: boolean;
  readonly churn: boolean;
  readonly pauseAfterMs: number;
  readonly pauseMs: number;

  private request: ReturnType<typeof httpGet> | null = null;
  private response: IncomingMessage | null = null;
  private buffer = "";
  private stopped = false;
  private reconnectTimer: NodeJS.Timeout | null = null;
  private churnTimer: NodeJS.Timeout | null = null;
  private pauseTimer: NodeJS.Timeout | null = null;
  private resumeTimer: NodeJS.Timeout | null = null;
  private connectedAt = 0;
  private lastEventId: string | null = null;
  private reconnecting = false;
  private intentionalClose = false;
  private connectionEvents = 0;
  private recoveryStartedAt: number | null = null;

  events = 0;
  readonly httpStatuses: Record<string, number> = {};
  snapshots = 0;
  updates = 0;
  resets = 0;
  bytes = 0;
  reconnects = 0;
  resetsAfterReconnect = 0;
  slowPauses = 0;
  slowDrops = 0;
  readonly eventLagMs: number[] = [];
  readonly firstEventMs: number[] = [];
  readonly sseRecoveryMs: number[] = [];

  constructor(
    id: number,
    options: {
      cookie: string;
      baseUrl: string;
      slow: boolean;
      churn: boolean;
      pauseAfterMs: number;
      pauseMs: number;
    },
  ) {
    this.id = id;
    this.cookie = options.cookie;
    this.baseUrl = options.baseUrl;
    this.slow = options.slow;
    this.churn = options.churn;
    this.pauseAfterMs = options.pauseAfterMs;
    this.pauseMs = options.pauseMs;
  }

  connect(lastEventId: string | null = null): void {
    if (this.stopped) return;
    this.buffer = "";
    this.connectionEvents = 0;
    this.connectedAt = performance.now();
    const headers: Record<string, string> = {
      cookie: this.cookie,
      Accept: "text/event-stream",
      "Cache-Control": "no-cache",
    };
    if (lastEventId !== null) headers["Last-Event-ID"] = lastEventId;
    const request = httpGet(`${this.baseUrl}/api/live`, { headers }, (response) => {
      const status = String(response.statusCode ?? 0);
      this.httpStatuses[status] = (this.httpStatuses[status] ?? 0) + 1;
      this.response = response;
      response.setEncoding("utf8");
      if (response.statusCode !== 200) {
        this.slowDrops += 1;
        response.resume();
        this.scheduleReconnect();
        return;
      }
      response.on("data", (chunk: string) => this.onData(chunk));
      response.on("end", () => this.onDisconnect());
      response.on("error", () => this.onDisconnect());
      if (this.slow) this.scheduleSlowPause();
    });
    request.on("error", () => this.onDisconnect());
    this.request = request;
    if (this.churn && this.churnTimer === null) {
      this.churnTimer = setInterval(() => this.disconnect(), 5_000);
      this.churnTimer.unref();
    }
  }

  private scheduleSlowPause(): void {
    if (this.pauseTimer !== null) clearTimeout(this.pauseTimer);
    this.pauseTimer = setTimeout(() => {
      if (this.stopped || this.response === null) return;
      this.slowPauses += 1;
      this.response.pause();
      this.resumeTimer = setTimeout(() => {
        if (this.stopped) return;
        try {
          this.response?.resume();
        } catch {
          // The hub may already have dropped this slow consumer.
        }
      }, this.pauseMs);
    }, this.pauseAfterMs);
    this.pauseTimer.unref?.();
    this.resumeTimer?.unref?.();
  }

  private onData(chunk: string): void {
    this.bytes += Buffer.byteLength(chunk);
    this.buffer += chunk;
    let index = this.buffer.indexOf("\n\n");
    while (index >= 0) {
      const block = this.buffer.slice(0, index);
      this.buffer = this.buffer.slice(index + 2);
      this.handleBlock(block);
      index = this.buffer.indexOf("\n\n");
    }
  }

  private handleBlock(block: string): void {
    if (block.startsWith(":")) return;
    let event = "message";
    let data = "";
    let id: string | null = null;
    for (const line of block.split("\n")) {
      if (line.startsWith("event:")) event = line.slice(6).trim();
      else if (line.startsWith("data:")) data += line.slice(5).trim();
      else if (line.startsWith("id:")) id = line.slice(3).trim();
    }
    if (data.length === 0) return;
    const wasReconnect = this.recoveryStartedAt !== null;
    this.events += 1;
    this.connectionEvents += 1;
    if (id !== null) this.lastEventId = id;
    if (this.connectionEvents === 1) {
      this.firstEventMs.push(performance.now() - this.connectedAt);
      if (this.recoveryStartedAt !== null) {
        this.sseRecoveryMs.push(performance.now() - this.recoveryStartedAt);
        this.recoveryStartedAt = null;
      }
      this.reconnecting = false;
    }
    if (event === "snapshot") this.snapshots += 1;
    else if (event === "update") this.updates += 1;
    else if (event === "reset") {
      this.resets += 1;
      if (wasReconnect) this.resetsAfterReconnect += 1;
    }
    try {
      const snapshot = JSON.parse(data) as HubSnapshot;
      let newest = 0;
      for (const machine of snapshot.machines) {
        const captured = machine.metrics?.capturedAt;
        if (typeof captured === "string") newest = Math.max(newest, Date.parse(captured));
      }
      if (newest > 0) this.eventLagMs.push(Math.max(0, Date.now() - newest));
    } catch {
      // A malformed frame is counted as an event but cannot be parsed.
    }
  }

  private onDisconnect(): void {
    this.response = null;
    if (this.stopped) return;
    if (this.intentionalClose) {
      this.intentionalClose = false;
      return;
    }
    if (this.slow) this.slowDrops += 1;
    this.scheduleReconnect();
  }

  private scheduleReconnect(): void {
    if (this.stopped || this.reconnectTimer !== null) return;
    if (!this.churn && !this.slow) return;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      if (this.stopped) return;
      this.intentionalClose = false;
      this.reconnecting = true;
      this.reconnects += 1;
      this.recoveryStartedAt = performance.now();
      this.connect(this.lastEventId);
    }, 250);
    this.reconnectTimer.unref();
  }

  /** Deliberately drop the current stream and resynchronize with Last-Event-ID. */
  disconnect(): void {
    if (this.stopped) return;
    this.intentionalClose = true;
    try {
      this.response?.destroy();
    } catch {
      // already gone
    }
    try {
      this.request?.destroy();
    } catch {
      // already gone
    }
    this.response = null;
    this.request = null;
    this.scheduleReconnect();
  }

  stop(): void {
    this.stopped = true;
    if (this.reconnectTimer !== null) clearTimeout(this.reconnectTimer);
    if (this.churnTimer !== null) clearInterval(this.churnTimer);
    if (this.pauseTimer !== null) clearTimeout(this.pauseTimer);
    if (this.resumeTimer !== null) clearTimeout(this.resumeTimer);
    try {
      this.response?.destroy();
    } catch {
      // already gone
    }
    try {
      this.request?.destroy();
    } catch {
      // already gone
    }
  }
}

// Keep collector cadence independent of JSON parsing by simulated browsers.
// Workers only consume the same isolated loopback hub and use throwaway cookies
// sent over IPC. They never log or persist the cookies.
const SSE_STAT_KEYS = [
  "events", "snapshots", "updates", "resets", "bytes", "reconnects",
  "resetsAfterReconnect", "slowPauses", "slowDrops", "eventLagMs",
  "firstEventMs", "sseRecoveryMs", "httpStatuses",
] as const;

function sseStats(client: SseClient): Record<string, unknown> {
  return Object.fromEntries(SSE_STAT_KEYS.map((key) => [key, client[key]]));
}

async function runSseWorker(): Promise<void> {
  const clients: SseClient[] = [];
  let timer: NodeJS.Timeout | undefined;
  const report = (): void => {
    if (process.connected) process.send?.({ type: "stats", clients: clients.map(sseStats) });
  };
  process.on("message", (message: any) => {
    if (message.type === "start") {
      for (const item of message.clients) {
        const client = new SseClient(item.id, item.options);
        clients.push(client);
        client.connect();
      }
      timer = setInterval(report, 1000);
      process.send?.({ type: "ready" });
    } else if (message.type === "stop") {
      clearInterval(timer);
      for (const client of clients) client.stop();
      report();
      process.disconnect?.();
    }
  });
  process.on("disconnect", () => {
    clearInterval(timer);
    for (const client of clients) client.stop();
    process.exit(0);
  });
  await new Promise<void>(() => {});
}

async function startSseWorkers(clients: SseClient[], count: number): Promise<() => Promise<void>> {
  const children: ChildProcess[] = [];
  const stop = async (): Promise<void> => {
    await Promise.all(children.map(async (child) => {
      if (child.exitCode !== null || child.signalCode !== null) return;
      const exited = new Promise<void>((done) => child.once("exit", () => done()));
      if (child.connected) child.send({ type: "stop" });
      const timeout = setTimeout(() => child.kill("SIGKILL"), 3000);
      await exited;
      clearTimeout(timeout);
    }));
  };
  try {
    await Promise.all(Array.from({ length: Math.min(count, clients.length) }, async (_, worker) => {
      const group = clients.filter((_, index) => index % count === worker);
      const child = spawn(process.execPath, ["--import", "tsx", SCRIPT_PATH, "sse-worker"], {
        cwd: ROOT, stdio: ["ignore", "ignore", "ignore", "ipc"],
      });
      children.push(child);
      await new Promise<void>((ready, fail) => {
        const timeout = setTimeout(() => fail(new Error("SSE worker startup timeout")), 10000);
        child.on("error", fail);
        child.once("exit", (code) => {
          clearTimeout(timeout);
          fail(new Error(`SSE worker exited: ${code}`));
        });
        child.on("message", (message: any) => {
          if (message.type === "ready") {
            clearTimeout(timeout);
            ready();
          } else if (message.type === "stats") {
            message.clients.forEach((stats: Record<string, unknown>, index: number) => {
              Object.assign(group[index]!, stats);
            });
          }
        });
        child.send({ type: "start", clients: group.map((client) => ({
          id: client.id, options: {
            cookie: client.cookie, baseUrl: client.baseUrl, slow: client.slow,
            churn: client.churn, pauseAfterMs: client.pauseAfterMs, pauseMs: client.pauseMs,
          },
        })) });
      });
    }));
    return stop;
  } catch (error) {
    await stop();
    throw error;
  }
}

// ---------------------------------------------------------------------------
// Parent: authenticated History requests
// ---------------------------------------------------------------------------

type HistoryCounters = {
  queries: number;
  ok: number;
  rateLimited: number;
  errors: number;
  latencyMs: number[];
  series: number[];
  points: number[];
  truncated: number;
  tiers: Record<string, number>;
};

function emptyHistoryCounters(): HistoryCounters {
  return {
    queries: 0,
    ok: 0,
    rateLimited: 0,
    errors: 0,
    latencyMs: [],
    series: [],
    points: [],
    truncated: 0,
    tiers: {},
  };
}

async function historyLoop(options: {
  cookie: string;
  baseUrl: string;
  machineIds: string[];
  metrics: string[];
  intervalMs: number;
  stop: () => boolean;
  counters: HistoryCounters;
}): Promise<void> {
  while (!options.stop()) {
    const machineId = options.machineIds[Math.floor(Math.random() * options.machineIds.length)]!;
    const to = Date.now();
    const from = to - 5 * 60 * 1000;
    const url =
      `${options.baseUrl}/api/history?machineId=${encodeURIComponent(machineId)}` +
      `&metrics=${encodeURIComponent(options.metrics.join(","))}` +
      `&from=${from}&to=${to}&maxPoints=600`;
    const started = performance.now();
    options.counters.queries += 1;
    try {
      const response = await fetch(url, {
        headers: { cookie: options.cookie },
        signal: AbortSignal.timeout(10_000),
      });
      const text = await response.text();
      options.counters.latencyMs.push(performance.now() - started);
      if (response.status === 429) {
        options.counters.rateLimited += 1;
        await sleep(400);
        continue;
      }
      if (response.status !== 200) {
        options.counters.errors += 1;
        await sleep(options.intervalMs);
        continue;
      }
      const body = JSON.parse(text) as {
        series?: Array<{ points?: unknown[] }>;
        truncated?: boolean;
        tier?: string;
      };
      options.counters.ok += 1;
      options.counters.series.push(body.series?.length ?? 0);
      options.counters.points.push(
        (body.series ?? []).reduce((sum, series) => sum + (series.points?.length ?? 0), 0),
      );
      if (body.truncated === true) options.counters.truncated += 1;
      const tier = body.tier ?? "unknown";
      options.counters.tiers[tier] = (options.counters.tiers[tier] ?? 0) + 1;
    } catch {
      options.counters.errors += 1;
    }
    const elapsed = performance.now() - started;
    const wait = Math.max(0, options.intervalMs - elapsed);
    if (wait > 0) await sleep(wait);
  }
}

// ---------------------------------------------------------------------------
// Parent: one sustained case
// ---------------------------------------------------------------------------

async function login(baseUrl: string, username: string, password: string): Promise<string> {
  for (let attempt = 0; attempt < 30; attempt += 1) {
    const response = await fetch(`${baseUrl}/api/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Origin: baseUrl },
      body: JSON.stringify({ username, password }),
      signal: AbortSignal.timeout(5_000),
    });
    if (response.status === 429) {
      await sleep(600);
      continue;
    }
    if (response.status !== 200) {
      throw new Error(`viewer login failed with status ${response.status}: ${await response.text()}`);
    }
    const headers = response.headers as unknown as { getSetCookie?: () => string[] };
    const setCookie = headers.getSetCookie?.()[0] ?? response.headers.get("set-cookie");
    if (typeof setCookie !== "string" || setCookie.length === 0) throw new Error("viewer login did not set a cookie");
    return setCookie.split(";", 1)[0]!;
  }
  throw new Error("viewer login remained rate limited");
}

function historyMetrics(set: "standard" | "broad"): string[] {
  return set === "broad"
    ? [
        "cpu.usage",
        "cpu.core",
        "cpu.temperature",
        "cpu.power",
        "memory.used",
        "memory.usage",
        "gpu.utilization",
        "gpu.memory.used",
        "disk.read",
        "disk.write",
        "net.rx",
        "net.tx",
      ]
    : ["cpu.usage", "memory.used", "gpu.utilization"];
}

async function checkAuthorization(baseUrl: string, password: string, machines: string[]): Promise<CaseReport['authorization']> {
  const anonymous=await fetch(baseUrl+'/api/live',{signal:AbortSignal.timeout(5000)});
  if (anonymous.status !== 401) throw new Error('anonymous SSE was not denied');
  await anonymous.body?.cancel();
  const cookie=await login(baseUrl,'scale-scoped',password);
  const history=(machineId:string)=>fetch(`${baseUrl}/api/history?machineId=${encodeURIComponent(machineId)}&metrics=cpu.usage&from=${Date.now()-60000}&to=${Date.now()}&maxPoints=10`,
    {headers:{cookie},signal:AbortSignal.timeout(5000)});
  const allowed=await history(machines[0]!);
  if (allowed.status !== 200) throw new Error('scoped History could not read its allowed machine');
  await allowed.body?.cancel();
  const denied=await history(machines[1] ?? 'scale-forbidden-machine');
  if (denied.status !== 403) throw new Error('scoped History leaked another machine');
  await denied.body?.cancel();
  async function stream(reset:boolean):Promise<void> {
    const response=await fetch(baseUrl+'/api/live',{headers:{cookie,...(reset?{'last-event-id':'previous-run:1'}:{})},signal:AbortSignal.timeout(5000)});
    if (!response.ok || !response.body) throw new Error('scoped SSE could not open');
    const reader=response.body.getReader();const decoder=new TextDecoder();let text='';
    try {
      while (!text.includes('\n\n')) {
        const part=await reader.read();if(part.done) throw new Error('scoped SSE ended before a snapshot');
        text+=decoder.decode(part.value,{stream:true});
        if(text.length>1024*1024) throw new Error('scoped SSE exceeded its validation budget');
      }
      const frame=text.slice(0,text.indexOf('\n\n'));
      if(reset&&!frame.includes('event: reset')) throw new Error('scoped reconnect did not reset');
      const data=frame.split('\n').filter(line=>line.startsWith('data:')).map(line=>line.slice(5).trim()).join('');
      const snapshot=JSON.parse(data) as HubSnapshot;
      if(snapshot.machines.length!==1||snapshot.machines[0]!.machineId!==machines[0]) throw new Error('SSE crossed the limited machine scope');
    } finally {await reader.cancel();}
  }
  await stream(false);await stream(true);
  return {anonymousDenied:true,scopedHistory:true,scopedSnapshot:true,scopedReset:true};
}

async function runCase(spec: CaseSpec, options: { keep: boolean; maxPhysicalBytes: number | null; sseWorkers: number }): Promise<CaseReport> {
  const directory = mkdtempSync(join(tmpdir(), "rigscope-scale-"));
  chmodSync(directory, 0o700);
  const databasePath = join(directory, "scale.sqlite");
  const identities: Identity[] = Array.from({ length: spec.machines }, (_, index) => ({
    collectorId: `scale-collector-${index}`,
    machineId: `scale-machine-${index}`,
    token: `scale-token-${index}-${randomUUID()}`,
  }));
  const username = "scale-viewer";
  const password = `scale-password-${randomUUID()}`;
  const passwordHash = hashPassword(password);
  const childConfig: ChildConfig = {
    databasePath,
    identities,
    users: [{ username, passwordHash, machineIds: "*" },
      {username:'scale-scoped',passwordHash,machineIds:[identities[0]!.machineId]}],
    maxConnections: Math.max(100, spec.machines + 10),
    coalesceMs: 500,
    maxQueuedRecords: 4096,
    maxRecordsPerTransaction: 32,
    maxPhysicalBytes: options.maxPhysicalBytes,
    feedMaxClients: 64,
  };
  const hub = await startHubChild(childConfig, directory);
  const warmupMs = 1_500;
  const collectors: CollectorSim[] = [];
  const sseClients: SseClient[] = [];
  const historyCounters: HistoryCounters[] = [];
  const stopFlags: boolean[] = [];
  let churnTimer: NodeJS.Timeout | null = null;
  let baselineStats: HubStats | null = null;
  let windowStats: HubStats | null = null;
  let finalStats: HubStats | null = null;
  let startedAt = 0;
  let stopSseWorkers: (() => Promise<void>) | undefined;

  try {
    const cookies: string[] = [];
    for (let viewer = 0; viewer < spec.viewers; viewer += 1) {
      if (viewer === 10) await sleep(600); // preserve the real 10-attempt login burst
      cookies.push(await login(hub.baseUrl, username, password));
    }
    const machines = identities.map((identity) => identity.machineId);

    for (let index = 0; index < identities.length; index += 20) {
      const batch = identities.slice(index, index + 20);
      const created = batch.map((identity, offset) => {
        const collector = new CollectorSim(index + offset, identity, {
          intervalMs: spec.sampleIntervalMs,
          recordsPerBatch: spec.recordsPerBatch,
        });
        collector.port = hub.port;
        collector.replayRecords = spec.replayRecords;
        return collector;
      });
      collectors.push(...created);
      await Promise.all(created.map((collector) => collector.connect()));
    }

    await sleep(warmupMs);
    const authorization = await checkAuthorization(hub.baseUrl,password,identities.map(identity=>identity.machineId));
    baselineStats = await hub.stats(true);
    startedAt = performance.now();

    // A churn case deliberately disconnects and reconnects every viewer with
    // Last-Event-ID, which the hub answers with a fresh `reset` snapshot.
    const sseChurn = spec.churnEveryMs !== null;
    for (let index = 0; index < spec.sseClients; index += 1) {
      const client = new SseClient(index, {
        cookie: cookies[index % cookies.length]!,
        baseUrl: hub.baseUrl,
        slow: index < spec.slowSseClients,
        churn: sseChurn,
        pauseAfterMs: spec.slowSsePauseAfterMs,
        pauseMs: spec.slowSsePauseMs,
      });
      sseClients.push(client);
      if (options.sseWorkers === 0) client.connect();
    }
    const clientWarmupMs = options.sseWorkers > 0 && sseClients.length > 0 ? 1000 : 0;
    if (clientWarmupMs > 0) {
      stopSseWorkers = await startSseWorkers(sseClients, options.sseWorkers);
      await sleep(clientWarmupMs);
      baselineStats = await hub.stats(true);
      startedAt = performance.now();
    }

    for (let viewer = 0; viewer < spec.viewers; viewer += 1) {
      const counters = emptyHistoryCounters();
      historyCounters.push(counters);
      stopFlags.push(false);
      void historyLoop({
        cookie: cookies[viewer]!,
        baseUrl: hub.baseUrl,
        machineIds: machines,
        metrics: historyMetrics(spec.historyMetricSet),
        intervalMs: spec.historyIntervalMs,
        stop: () => stopFlags[viewer] === true,
        counters,
      });
    }

    if (spec.churnEveryMs !== null && spec.churnMachines > 0) {
      const churnTargets = collectors
        .filter((collector) => collector.index % Math.max(1, Math.floor(spec.machines / spec.churnMachines)) === 0)
        .slice(0, spec.churnMachines);
      churnTimer = setInterval(() => {
        for (const collector of churnTargets) {
          void collector.reconnect(true).catch(() => {
            // reconnect() already recorded the failure.
          });
        }
      }, spec.churnEveryMs);
      churnTimer.unref();
    }

    await sleep(spec.durationMs);
    for (let index = 0; index < stopFlags.length; index += 1) stopFlags[index] = true;
    if (churnTimer !== null) clearInterval(churnTimer);
    const elapsedMs = performance.now() - startedAt;
    windowStats = await hub.stats(true);
    // Give the durable queue a bounded drain before closing sockets.
    await sleep(1_500);
    for (const collector of collectors) collector.stopSending();
    await sleep(1_500);
    finalStats = await hub.stats(false);
    await stopSseWorkers?.();
    for (const client of sseClients) client.stop();
    await Promise.all(collectors.map((collector) => collector.stop()));

    const allHistory = mergeHistoryCounters(historyCounters);
    const ingestion = summarizeIngestion(collectors);
    const stream = summarizeStream(sseClients, spec);
    const report: CaseReport = {
      name: spec.name,
      description: spec.description,
      config: {
        ...spec,
        warmupMs,
        clientWarmupMs,
        sseWorkers: options.sseWorkers,
        maxCollectorConnections: childConfig.maxConnections,
        databaseBudgetBytes: options.maxPhysicalBytes,
      },
      host: hostInfo(),
      authorization,
      ingestion,
      history: {
        queries: allHistory.queries,
        ok: allHistory.ok,
        rateLimited: allHistory.rateLimited,
        errors: allHistory.errors,
        latencyMs: summarize(allHistory.latencyMs),
        series: summarize(allHistory.series),
        points: summarize(allHistory.points),
        truncated: allHistory.truncated,
        tiers: allHistory.tiers,
      },
      stream,
      hub: {
        baseline: baselineStats,
        window: windowStats,
        final: finalStats,
        databaseGrowth:
          baselineStats === null || finalStats === null
            ? null
            : {
                bytes: finalStats.database.bytes - baselineStats.database.bytes,
                walBytes: finalStats.database.walBytes - baselineStats.database.walBytes,
                mainBytes: finalStats.database.mainBytes - baselineStats.database.mainBytes,
              },
      },
      elapsedMs: round(elapsedMs),
      artifacts: {
        directory,
        kept: options.keep,
        configPath: join(directory, "hub-config.json"),
        databasePath,
      },
    };
    return report;
  } finally {
    for (let index = 0; index < stopFlags.length; index += 1) stopFlags[index] = true;
    if (churnTimer !== null) clearInterval(churnTimer);
    await stopSseWorkers?.();
    for (const client of sseClients) client.stop();
    await Promise.all(collectors.map((collector) => collector.stop()));
    await hub.stop();
    if (!options.keep) rmSync(directory, { recursive: true, force: true });
  }
}

function mergeHistoryCounters(counters: HistoryCounters[]): HistoryCounters {
  const merged = emptyHistoryCounters();
  for (const counter of counters) {
    merged.queries += counter.queries;
    merged.ok += counter.ok;
    merged.rateLimited += counter.rateLimited;
    merged.errors += counter.errors;
    merged.latencyMs.push(...counter.latencyMs);
    merged.series.push(...counter.series);
    merged.points.push(...counter.points);
    merged.truncated += counter.truncated;
    for (const [tier, count] of Object.entries(counter.tiers)) {
      merged.tiers[tier] = (merged.tiers[tier] ?? 0) + count;
    }
  }
  return merged;
}

function summarizeIngestion(collectors: CollectorSim[]): CaseReport["ingestion"] {
  const ackLatency: number[] = [];
  const commitLag: number[] = [];
  const recovery: number[] = [];
  let recordsSent = 0;
  let recordsAccepted = 0;
  let recordsRejected = 0;
  let replayRecordsSent = 0;
  let replayRecordsAccepted = 0;
  let retryAcks = 0;
  let unackedLiveRecords = 0;
  let sendErrors = 0;
  let messagesSent = 0;
  let backpressureSkips = 0;
  let connects = 0;
  let reconnects = 0;
  let reconnectFailures = 0;
  for (const collector of collectors) {
    ackLatency.push(...collector.ackLatencyMs);
    commitLag.push(...collector.sampleToCommitLagMs);
    recovery.push(...collector.collectorRecoveryMs);
    recordsSent += collector.recordsSent;
    recordsAccepted += collector.recordsAccepted;
    recordsRejected += collector.recordsRejected;
    replayRecordsSent += collector.replayRecordsSent;
    replayRecordsAccepted += collector.replayRecordsAccepted;
    retryAcks += collector.retryAcks;
    unackedLiveRecords += collector.unackedLiveRecords();
    sendErrors += collector.sendErrors;
    messagesSent += collector.messagesSent;
    backpressureSkips += collector.backpressureSkips;
    connects += collector.connects;
    reconnects += collector.reconnects;
    reconnectFailures += collector.reconnectFailures;
  }
  return {
    recordsSent,
    recordsAccepted,
    recordsRejected,
    replayRecordsSent,
    replayRecordsAccepted,
    retryAcks,
    unackedLiveRecords,
    sendErrors,
    ackLatencyMs: summarize(ackLatency),
    sampleToCommitLagMs: summarize(commitLag),
    messagesSent,
    backpressureSkips,
    connects,
    reconnects,
    reconnectFailures,
    collectorRecoveryMs: summarize(recovery),
  };
}

function summarizeStream(clients: SseClient[], spec: CaseSpec): CaseReport["stream"] {
  const httpStatuses: Record<string, number> = {};
  const lag: number[] = [];
  const normalLag: number[] = [];
  const slowLag: number[] = [];
  const first: number[] = [];
  const recovery: number[] = [];
  let events = 0;
  let snapshots = 0;
  let updates = 0;
  let resets = 0;
  let bytes = 0;
  let reconnects = 0;
  let resetsAfterReconnect = 0;
  let slowPauses = 0;
  let slowDrops = 0;
  for (const client of clients) {
    for (const [status, count] of Object.entries(client.httpStatuses)) {
      httpStatuses[status] = (httpStatuses[status] ?? 0) + count;
    }
    lag.push(...client.eventLagMs);
    if (client.slow) slowLag.push(...client.eventLagMs);
    else normalLag.push(...client.eventLagMs);
    first.push(...client.firstEventMs);
    recovery.push(...client.sseRecoveryMs);
    events += client.events;
    snapshots += client.snapshots;
    updates += client.updates;
    resets += client.resets;
    bytes += client.bytes;
    reconnects += client.reconnects;
    resetsAfterReconnect += client.resetsAfterReconnect;
    slowPauses += client.slowPauses;
    slowDrops += client.slowDrops;
  }
  return {
    clients: spec.sseClients,
    httpStatuses,
    slowClients: spec.slowSseClients,
    events,
    snapshots,
    updates,
    resets,
    bytes,
    eventLagMs: summarize(lag),
    normalEventLagMs: summarize(normalLag),
    slowEventLagMs: summarize(slowLag),
    firstEventMs: summarize(first),
    reconnects,
    resetsAfterReconnect,
    slowPauses,
    slowDrops,
    sseRecoveryMs: summarize(recovery),
  };
}

// ---------------------------------------------------------------------------
// Payload and case definitions
// ---------------------------------------------------------------------------

function buildMetricsPayload(index: number): MetricsPayload {
  const jitter = index % 17;
  const used = 6 * 1024 ** 3 + jitter * 1024 ** 2;
  const total = 32 * 1024 ** 3;
  return {
    cpu: {
      status: "ok",
      usagePercent: 20 + jitter,
      perCore: Array.from({ length: 8 }, (_, core) => (core * 11 + jitter) % 100),
      temperatureC: 45 + jitter,
      powerWatts: 80 + jitter * 2,
      iowaitPercent: jitter % 5,
      pressureSomePercent: jitter % 10,
    },
    memory: {
      status: "ok",
      usedBytes: used,
      totalBytes: total,
      pressureSomePercent: jitter % 8,
      pressureFullPercent: jitter % 3,
    },
    swap: { status: "ok", usedBytes: 512 * 1024 ** 2, totalBytes: 8 * 1024 ** 3 },
    gpus: {
      status: "ok",
      devices: [0, 1].map((gpu) => ({
        id: `gpu:scale-${index}-${gpu}`,
        status: "ok" as const,
        utilizationPercent: (index * 7 + gpu * 23) % 100,
        memoryUsedBytes: (gpu + 1) * 2 * 1024 ** 3,
        memoryTotalBytes: 24 * 1024 ** 3,
        temperatureC: 50 + jitter + gpu,
        powerWatts: 110 + jitter + gpu * 10,
      })),
    },
    disks: {
      status: "ok",
      filesystems: [
        {
          id: "fs:/",
          status: "ok" as const,
          usedBytes: 100 * 1024 ** 3,
          totalBytes: 500 * 1024 ** 3,
          inodesUsed: 2_000_000 + jitter * 1000,
          inodesTotal: 30_000_000,
        },
        {
          id: "fs:/data",
          status: "ok" as const,
          usedBytes: 300 * 1024 ** 3,
          totalBytes: 1024 ** 4,
          inodesUsed: 10_000_000 + jitter * 1000,
          inodesTotal: 200_000_000,
        },
      ],
      devices: [
        {
          id: "disk:nvme0",
          status: "ok" as const,
          readBytesPerSec: 12 * 1024 ** 2 + jitter * 1024,
          writeBytesPerSec: 4 * 1024 ** 2 + jitter * 512,
          readLatencyMs: 0.4 + (jitter % 5) / 10,
          writeLatencyMs: 0.8 + (jitter % 7) / 10,
          ioUtilizationPercent: jitter % 40,
        },
        {
          id: "disk:nvme1",
          status: "ok" as const,
          readBytesPerSec: 3 * 1024 ** 2 + jitter * 256,
          writeBytesPerSec: 9 * 1024 ** 2 + jitter * 1024,
          readLatencyMs: 0.3 + (jitter % 3) / 10,
          writeLatencyMs: 1.1 + (jitter % 9) / 10,
          ioUtilizationPercent: (jitter * 2) % 50,
        },
      ],
      pressureSomePercent: jitter % 6,
      pressureFullPercent: jitter % 2,
    },
    network: {
      status: "ok",
      interfaces: [
        {
          id: "net:eth0",
          status: "ok",
          rxBytesPerSec: 8 * 1024 ** 2 + jitter * 1024,
          txBytesPerSec: 2 * 1024 ** 2 + jitter * 512,
        },
        { id: "net:lo", status: "ok", rxBytesPerSec: 1024, txBytesPerSec: 1024 },
      ],
    },
  };
}

function suiteSpecs(): CaseSpec[] {
  return [
    {
      name: "small-5m",
      description: "Small fleet, four authenticated History viewers and two SSE viewers.",
      machines: 5,
      durationMs: 60_000,
      sampleIntervalMs: 1_000,
      recordsPerBatch: 1,
      viewers: 4,
      sseClients: 2,
      slowSseClients: 0,
      slowSsePauseAfterMs: 0,
      slowSsePauseMs: 0,
      historyIntervalMs: 1_000,
      historyMetricSet: "standard",
      churnEveryMs: null,
      churnMachines: 0,
      replayRecords: 0,
    },
    {
      name: "fleet-75m",
      description: "75 machines with eight concurrent History viewers and four SSE viewers.",
      machines: 75,
      durationMs: 60_000,
      sampleIntervalMs: 1_000,
      recordsPerBatch: 1,
      viewers: 8,
      sseClients: 4,
      slowSseClients: 0,
      slowSsePauseAfterMs: 0,
      slowSsePauseMs: 0,
      historyIntervalMs: 1_000,
      historyMetricSet: "standard",
      churnEveryMs: null,
      churnMachines: 0,
      replayRecords: 0,
    },
    {
      name: "fleet-75m-churn",
      description: "75 machines with collector reconnect/replay churn and SSE reconnect/reset churn.",
      machines: 75,
      durationMs: 60_000,
      sampleIntervalMs: 1_000,
      recordsPerBatch: 1,
      viewers: 8,
      sseClients: 4,
      slowSseClients: 0,
      slowSsePauseAfterMs: 0,
      slowSsePauseMs: 0,
      historyIntervalMs: 1_000,
      historyMetricSet: "standard",
      churnEveryMs: 15_000,
      churnMachines: 15,
      replayRecords: 20,
    },
    {
      name: "fleet-75m-slow-sse",
      description: "75 machines with six SSE viewers, two deliberately paused to force buffering/reconnect.",
      machines: 75,
      durationMs: 60_000,
      sampleIntervalMs: 1_000,
      recordsPerBatch: 1,
      viewers: 8,
      sseClients: 6,
      slowSseClients: 2,
      slowSsePauseAfterMs: 10_000,
      slowSsePauseMs: 15_000,
      historyIntervalMs: 1_000,
      historyMetricSet: "standard",
      churnEveryMs: null,
      churnMachines: 0,
      replayRecords: 0,
    },
    {
      name: "fleet-75m-history",
      description: "75 machines with eight History viewers polling a broad metric set near the session budget.",
      machines: 75,
      durationMs: 60_000,
      sampleIntervalMs: 1_000,
      recordsPerBatch: 1,
      viewers: 8,
      sseClients: 2,
      slowSseClients: 0,
      slowSsePauseAfterMs: 0,
      slowSsePauseMs: 0,
      historyIntervalMs: 300,
      historyMetricSet: "broad",
      churnEveryMs: null,
      churnMachines: 0,
      replayRecords: 0,
    },
  ];
}

function caseFromOptions(values: Record<string, unknown>): CaseSpec {
  const name = optionString(values, "case") ?? "fleet-75m";
  const base = suiteSpecs().find((spec) => spec.name === name);
  if (base === undefined) {
    throw new Error(`unknown --case ${JSON.stringify(name)}; choices: ${suiteSpecs().map((spec) => spec.name).join(", ")}`);
  }
  return {
    ...base,
    name,
    durationMs: optionNumber(values, "duration-ms", base.durationMs),
    machines: optionNumber(values, "machines", base.machines),
    sampleIntervalMs: optionNumber(values, "sample-interval-ms", base.sampleIntervalMs),
    recordsPerBatch: optionNumber(values, "records-per-batch", base.recordsPerBatch),
    viewers: optionNumber(values, "viewers", base.viewers),
    sseClients: optionNumber(values, "sse-clients", base.sseClients),
    slowSseClients: optionNumber(values, "slow-sse", base.slowSseClients),
    historyIntervalMs: optionNumber(values, "history-interval-ms", base.historyIntervalMs),
    churnEveryMs:
      values["churn-every-ms"] === undefined
        ? base.churnEveryMs
        : optionNumber(values, "churn-every-ms", base.churnEveryMs ?? 15_000) || null,
    churnMachines: optionNumber(values, "churn-machines", base.churnMachines),
  };
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

async function main(): Promise<number> {
  const argv = process.argv.slice(2);
  if (argv[0] === "hub") {
    if (argv[1] === undefined) throw new Error("hub child mode requires a config path");
    await runHubChild(argv[1]);
    return 0;
  }
  if (argv[0] === "sse-worker") {
    await runSseWorker();
    return 0;
  }

  const parsed = parseArgs({
    args: argv,
    options: {
      suite: { type: "boolean" },
      case: { type: "string" },
      "duration-ms": { type: "string" },
      machines: { type: "string" },
      "sample-interval-ms": { type: "string" },
      "records-per-batch": { type: "string" },
      viewers: { type: "string" },
      "sse-clients": { type: "string" },
      "sse-workers": { type: "string" },
      "slow-sse": { type: "string" },
      "history-interval-ms": { type: "string" },
      "churn-every-ms": { type: "string" },
      "churn-machines": { type: "string" },
      keep: { type: "boolean" },
      "json-out": { type: "string" },
      "database-budget-mb": { type: "string" },
      help: { type: "boolean", short: "h" },
    },
    allowPositionals: false,
  });
  if (optionBoolean(parsed.values, "help")) {
    console.log(HELP);
    return 0;
  }
  if (!optionBoolean(parsed.values, "suite") && parsed.values.case === undefined) {
    console.error(HELP);
    return 2;
  }
  const budgetMb = optionNumber(parsed.values, "database-budget-mb", 512);
  const maxPhysicalBytes = budgetMb === 0 ? null : budgetMb * 1024 * 1024;
  const specs = optionBoolean(parsed.values, "suite") ? suiteSpecs() : [caseFromOptions(parsed.values)];
  const sseWorkers = optionNumber(parsed.values, "sse-workers", 0);
  if (!Number.isSafeInteger(sseWorkers) || sseWorkers < 0 || sseWorkers > 8) {
    throw new Error("sse-workers must be an integer from 0 to 8");
  }
  for (const spec of specs) validateSpec(spec);
  const cases: CaseReport[] = [];
  for (const spec of specs) {
    console.error(`scale case ${spec.name}: ${spec.machines} machines, ${spec.viewers} viewers, ${spec.durationMs} ms`);
    const report = await runCase(spec, { keep: optionBoolean(parsed.values, "keep"), maxPhysicalBytes, sseWorkers });
    cases.push(report);
    console.error(
      `scale case ${spec.name}: accepted ${report.ingestion.recordsAccepted}/${report.ingestion.recordsSent}, ` +
        `history ${report.history.ok}/${report.history.queries}, SSE events ${report.stream.events}, ` +
        `event-loop p99 ${report.hub.window?.eventLoopDelayMs.p99 ?? "n/a"} ms`,
    );
  }
  const suite: SuiteReport = {
    tool: TOOL,
    synthetic: true,
    disclaimer: DISCLAIMER,
    generatedAt: new Date().toISOString(),
    node: process.version,
    host: hostInfo(),
    command: `npx tsx scripts/scale-harness.ts ${argv.join(" ")}`,
    suite: specs,
    cases,
    limitations: [
      "Collector payload values and cadence are synthetic; the hub, database, auth, SSE and History paths are real modules.",
      "The hub child and all load generators share one host; the hub event loop is single-threaded, while load processes can use other cores. This is not distributed load.",
      "History queries are synchronous on the hub event loop, so 'concurrent' viewers model overlapping HTTP requests, not parallel SQLite readers.",
      "The measurement never contacts the live service; it uses a fresh temporary database and loopback ephemeral port.",
      "A passing case is evidence for this host and workload, not an unlimited-capacity or latency guarantee.",
    ],
  };
  const jsonOut = optionString(parsed.values, "json-out");
  if (jsonOut !== undefined) {
    const path = resolve(ROOT, jsonOut);
    writeFileSync(path, `${JSON.stringify(suite, null, 2)}\n`, { mode: 0o644 });
    console.error(`scale report written: ${path}`);
  }
  console.log(JSON.stringify(suite, null, 2));
  return 0;
}

function validateSpec(spec: CaseSpec): void {
  const positive = (value: number, name: string, max: number): void => {
    if (!Number.isSafeInteger(value) || value < 1 || value > max) {
      throw new Error(`${name} must be an integer from 1 to ${max}`);
    }
  };
  positive(spec.machines, "machines", 8_192);
  positive(spec.durationMs, "durationMs", 3_600_000);
  positive(spec.sampleIntervalMs, "sampleIntervalMs", 60_000);
  if (spec.sampleIntervalMs < 100) throw new Error("sampleIntervalMs must be at least 100 ms (protocol rate limit)");
  positive(spec.recordsPerBatch, "recordsPerBatch", MAX_BATCH_RECORDS);
  positive(spec.viewers, "viewers", 200);
  // Permit one over the production limit to measure admission rejection.
  // The child's LiveFeed limit remains 64.
  if (spec.sseClients !== 0) positive(spec.sseClients, "sseClients", 65);
  if (spec.slowSseClients > spec.sseClients) throw new Error("slowSseClients cannot exceed sseClients");
  positive(spec.historyIntervalMs, "historyIntervalMs", 60_000);
  if (spec.churnMachines > spec.machines) throw new Error("churnMachines cannot exceed machines");
  if (spec.replayRecords > MAX_BATCH_RECORDS * 4) throw new Error(`replayRecords is bounded to ${MAX_BATCH_RECORDS * 4}`);
  const liveRecords =
    spec.machines * Math.ceil(spec.durationMs / spec.sampleIntervalMs) * spec.recordsPerBatch;
  const churnCycles = spec.churnEveryMs === null ? 0 : Math.ceil(spec.durationMs / spec.churnEveryMs);
  const replayRecords = churnCycles * spec.churnMachines * spec.replayRecords;
  if (liveRecords + replayRecords > 500_000) {
    throw new Error(
      `workload is bounded to 500000 records; this case would send about ${liveRecords + replayRecords}`,
    );
  }
}

const HELP = `Rigscope isolated scale harness

Usage:
  npx tsx scripts/scale-harness.ts --suite [--json-out docs/scale-validation.json]
  npx tsx scripts/scale-harness.ts --case fleet-75m [options]

Options:
  --suite                 Run the five sustained cases (about five minutes).
  --case NAME             small-5m | fleet-75m | fleet-75m-churn |
                          fleet-75m-slow-sse | fleet-75m-history
  --duration-ms N         Measured phase per case (default from case, 60000).
  --machines N            Collector identities (1..8192; total record bound applies).
  --sample-interval-ms N  Live cadence per collector (>=100).
  --records-per-batch N   Records per live protocol batch (1..32).
  --viewers N             Authenticated History sessions (1..200).
  --sse-clients N         SSE attempts (0..65; hub admits at most 64).
  --sse-workers N         Separate stream-reader processes (0..8; default 0).
  --slow-sse N            How many SSE viewers pause reading (<=sse-clients).
  --history-interval-ms N Per-viewer History poll interval.
  --churn-every-ms N      Collector/SSE reconnect interval (0 disables).
  --churn-machines N      Collectors that reconnect per churn interval.
  --database-budget-mb N  Physical main+WAL budget; 0 disables it (default 512).
  --keep                  Keep the temporary database/config directory.
  --json-out PATH         Write the full report JSON.

The harness never contacts a live hub. It creates a private temporary database,
spawns a loopback-only hub child with ephemeral port, logs in real browser
sessions, connects real collector WebSockets and reads real SSE/History APIs.`;

main()
  .then((code) => {
    process.exitCode = code;
  })
  .catch((error: unknown) => {
    console.error(`scale harness failed: ${error instanceof Error ? (error.stack ?? error.message) : String(error)}`);
    process.exitCode = 1;
  });
