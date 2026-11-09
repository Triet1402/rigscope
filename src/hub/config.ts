/**
 * Hub configuration for the runtime spike (HUB_* environment variables).
 *
 * Startup validation happens before anything else (including Next.js
 * `prepare`), so a bad configuration exits nonzero before listening.
 *
 * Two explicit listener modes:
 *
 * - **Loopback TCP (default).** `HUB_HOST` must be a literal loopback
 *   address; `HUB_PORT` is a TCP port (`0` for an ephemeral test port). This
 *   preserves the local-only behavior exactly.
 * - **Remote Unix socket.** `HUB_PUBLIC_ORIGIN` and `HUB_SOCKET_PATH` are
 *   required together. The hub then listens ONLY on the private Unix socket
 *   and serves the exact canonical HTTPS origin that an outer TLS terminator
 *   (for example Tailscale Serve) forwards. `HUB_HOST`/`HUB_PORT` are rejected
 *   in this mode so no TCP backend can be started by accident.
 *
 * The remote public origin is a static trust input, never derived from
 * `Forwarded`/`X-Forwarded-*` request headers.
 */

import { isIP } from "node:net";
import { basename, isAbsolute, resolve } from "node:path";
import { MAX_PERSISTENT_FLUSH_MS } from "../contracts/telemetry";

/**
 * Upper bound on the HUB_COLLECTORS JSON document in bytes.
 *
 * This bounds parse memory and work rather than the number of machines: a
 * valid identity entry is much smaller than this budget, so deployments are
 * not capped at an arbitrary machine count. A single Linux environment
 * variable is already limited to 128 KiB, and the larger bound keeps direct
 * parse callers (tests, future configuration sources) comfortably inside one
 * guarded allocation.
 */
export const MAX_HUB_COLLECTORS_BYTES = 1024 * 1024;
const MAX_COLLECTOR_ID_LENGTH = 128;
const MAX_MACHINE_ID_LENGTH = 128;
const MAX_TOKEN_LENGTH = 256;
/** Linux `sun_path` is 108 bytes including the terminating NUL. */
export const MAX_UNIX_SOCKET_PATH_BYTES = 100;
const MAX_PUBLIC_ORIGIN_LENGTH = 512;

export type HubIdentityConfig = {
  collectorId: string;
  machineId: string;
  token: string;
};

/**
 * Explicit remote boundary. `publicOrigin` is the exact canonical origin
 * (`scheme://host[:port]`, no trailing slash); `hostHeader` is that origin's
 * exact HTTP Host value (including a non-default port).
 */
export type HubPublicOrigin = {
  publicOrigin: string;
  hostHeader: string;
  hostname: string;
  port: number;
};

export type HubRemoteConfig = HubPublicOrigin & {
  socketPath: string;
};

export type TelemetryHistoryMode = "memory" | "persistent";

export type HubHistoryConfig = {
  mode: TelemetryHistoryMode;
  /** Global RAM budget for memory history. */
  memoryBudgetBytes: number;
  /** Persistent summary flush cadence. */
  flushIntervalMs: number;
  retention: {
    rawMs: number;
    tenSecondMs: number;
    oneMinuteMs: number;
    oneHourMs: number;
    receiptsMs: number;
    gapsMs: number;
  };
  recordings: {
    directory: string | null;
    maxDurationMs: number;
    maxBytes: number;
    backlogMs: number;
    maxActive: number;
  };
};

export type HubConfig = {
  /** Loopback bind host. Unused in remote mode (kept for local callers). */
  host: string;
  /** Loopback TCP port. Unused in remote mode (kept for local callers). */
  port: number;
  collectors: HubIdentityConfig[];
  development: boolean;
  /** Non-null only when HUB_PUBLIC_ORIGIN/HUB_SOCKET_PATH select remote mode. */
  remote: HubRemoteConfig | null;
  /** Optional deployment resource budget; defaults in the collector gateway. */
  maxCollectorConnections?: number;
  history: HubHistoryConfig;
};

export class HubConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "HubConfigError";
  }
}

export function parseHubConfig(
  env: Record<string, string | undefined>,
): HubConfig {
  const development = env.NODE_ENV === "development";
  const collectors = parseCollectors(env.HUB_COLLECTORS);
  const publicOrigin = nonEmpty(env.HUB_PUBLIC_ORIGIN);
  const socketPath = nonEmpty(env.HUB_SOCKET_PATH);
  let maxCollectorConnections: number | undefined;
  if (env.HUB_MAX_COLLECTOR_CONNECTIONS !== undefined) {
    const value = env.HUB_MAX_COLLECTOR_CONNECTIONS;
    const parsed = Number(value);
    if (!/^\d+$/.test(value) || !Number.isSafeInteger(parsed) || parsed < 1) {
      throw new HubConfigError("HUB_MAX_COLLECTOR_CONNECTIONS must be a positive safe integer");
    }
    maxCollectorConnections = parsed;
  }
  const connectionBudget = maxCollectorConnections === undefined ? {} : {maxCollectorConnections};
  const history = parseHistoryConfig(env);

  if ((publicOrigin === undefined) !== (socketPath === undefined)) {
    throw new HubConfigError(
      "HUB_PUBLIC_ORIGIN and HUB_SOCKET_PATH must be set together: remote mode needs both the exact HTTPS origin and the Unix socket path",
    );
  }

  if (publicOrigin !== undefined && socketPath !== undefined) {
    if (nonEmpty(env.HUB_HOST) !== undefined || nonEmpty(env.HUB_PORT) !== undefined) {
      throw new HubConfigError(
        "HUB_HOST/HUB_PORT are not valid in remote mode; the hub listens only on HUB_SOCKET_PATH",
      );
    }
    return {
      // Keep stable local defaults for callers that inspect these fields;
      // main.ts never creates a TCP listener when `remote` is set.
      host: "127.0.0.1",
      port: 8081,
      collectors,
      development,
      remote: parseRemoteConfig(publicOrigin, socketPath),
      history,
      ...connectionBudget,
    };
  }

  return {
    host: parseHost(env.HUB_HOST),
    port: parsePort(env.HUB_PORT),
    collectors,
    development,
    remote: null,
    history,
    ...connectionBudget,
  };
}

const DEFAULT_MEMORY_BUDGET_BYTES = 128 * 1024 * 1024;
/**
 * A collector can keep at most MAX_IN_FLIGHT_BATCHES unacknowledged messages,
 * and sends at most one live message per MIN_EXPECTED_INTERVAL_MS. The flush
 * interval must therefore stay below that window coverage; the headroom covers
 * the COMMIT, ack write and scheduling jitter. This is also far below the
 * collector's 30 s ack timeout, so a timeout never races a legitimate commit.
 */
const DEFAULT_PERSISTENT_FLUSH_MS = MAX_PERSISTENT_FLUSH_MS;
const DEFAULT_RECORDING_MAX_DURATION_MS = 60 * 60_000;
const DEFAULT_RECORDING_MAX_BYTES = 64 * 1024 * 1024;
const DEFAULT_RECORDING_BACKLOG_MS = 10 * 60_000;
const DEFAULT_MAX_ACTIVE_RECORDINGS = 4;
const HOUR_MS = 3_600_000;
const DAY_MS = 86_400_000;

function parseHistoryConfig(env: Record<string, string | undefined>): HubHistoryConfig {
  const modeValue = env.HUB_TELEMETRY_MODE ?? "memory";
  if (modeValue !== "memory" && modeValue !== "persistent") {
    throw new HubConfigError("HUB_TELEMETRY_MODE must be memory or persistent");
  }
  const memoryBudgetBytes = envInteger(
    env.HUB_HISTORY_MEMORY_BYTES,
    DEFAULT_MEMORY_BUDGET_BYTES,
    1 * 1024 * 1024,
    16 * 1024 * 1024 * 1024,
    "HUB_HISTORY_MEMORY_BYTES",
  );
  const flushIntervalMs = envInteger(
    env.HUB_TELEMETRY_FLUSH_MS,
    DEFAULT_PERSISTENT_FLUSH_MS,
    1_000,
    MAX_PERSISTENT_FLUSH_MS,
    "HUB_TELEMETRY_FLUSH_MS",
  );
  const retention = {
    rawMs: envInteger(env.HUB_RETENTION_RAW_MS, 2 * DAY_MS, 0, 400 * DAY_MS, "HUB_RETENTION_RAW_MS"),
    tenSecondMs: envInteger(env.HUB_RETENTION_10S_MS, 2 * HOUR_MS, 0, 30 * DAY_MS, "HUB_RETENTION_10S_MS"),
    oneMinuteMs: envInteger(env.HUB_RETENTION_1M_MS, 30 * DAY_MS, 0, 400 * DAY_MS, "HUB_RETENTION_1M_MS"),
    oneHourMs: envInteger(env.HUB_RETENTION_1H_MS, 400 * DAY_MS, 0, 800 * DAY_MS, "HUB_RETENTION_1H_MS"),
    receiptsMs: envInteger(env.HUB_RECEIPT_RETENTION_MS, 7 * DAY_MS, HOUR_MS, 400 * DAY_MS, "HUB_RECEIPT_RETENTION_MS"),
    gapsMs: envInteger(env.HUB_GAP_RETENTION_MS, 400 * DAY_MS, 0, 800 * DAY_MS, "HUB_GAP_RETENTION_MS"),
  };
  const recordings = {
    directory: nonEmpty(env.HUB_RECORDINGS_DIR) ?? null,
    maxDurationMs: envInteger(env.HUB_RECORDING_MAX_DURATION_MS, DEFAULT_RECORDING_MAX_DURATION_MS, 1_000, 24 * 60 * 60_000, "HUB_RECORDING_MAX_DURATION_MS"),
    maxBytes: envInteger(env.HUB_RECORDING_MAX_BYTES, DEFAULT_RECORDING_MAX_BYTES, 64 * 1024, 1024 * 1024 * 1024, "HUB_RECORDING_MAX_BYTES"),
    backlogMs: envInteger(env.HUB_RECORDING_BACKLOG_MS, DEFAULT_RECORDING_BACKLOG_MS, 0, 24 * 60 * 60_000, "HUB_RECORDING_BACKLOG_MS"),
    maxActive: envInteger(env.HUB_RECORDING_MAX_ACTIVE, DEFAULT_MAX_ACTIVE_RECORDINGS, 1, 32, "HUB_RECORDING_MAX_ACTIVE"),
  };
  // Retention variables still describe disk history. Memory mode ignores them
  // by design rather than rejecting a shared environment file.
  return { mode: modeValue, memoryBudgetBytes, flushIntervalMs, retention, recordings };
}

function envInteger(
  raw: string | undefined,
  fallback: number,
  min: number,
  max: number,
  name: string,
): number {
  if (raw === undefined || raw === "") return fallback;
  if (!/^\d+$/.test(raw)) {
    throw new HubConfigError(`${name} must be a nonnegative integer`);
  }
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value < min || value > max) {
    throw new HubConfigError(`${name} must be between ${min} and ${max}`);
  }
  return value;
}

function nonEmpty(value: string | undefined): string | undefined {
  return value === undefined || value === "" ? undefined : value;
}

/**
 * Exact canonical HTTPS origin. This deliberately rejects:
 * - any non-HTTPS scheme,
 * - paths, queries, fragments, userinfo, whitespace and control characters,
 * - non-canonical serializations such as a trailing slash, uppercase host, or
 *   an explicit default port (`https://host:443`),
 * - a trailing-dot hostname (ambiguous relative to the Host header).
 */
export function parsePublicOrigin(value: string): HubPublicOrigin {
  if (value.length > MAX_PUBLIC_ORIGIN_LENGTH) {
    throw new HubConfigError(
      `HUB_PUBLIC_ORIGIN must be at most ${MAX_PUBLIC_ORIGIN_LENGTH} characters`,
    );
  }
  if (!/^[\x21-\x7e]+$/.test(value)) {
    throw new HubConfigError(
      "HUB_PUBLIC_ORIGIN must be printable ASCII without spaces or control characters",
    );
  }
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    throw new HubConfigError(
      `HUB_PUBLIC_ORIGIN "${value}" is not a valid absolute origin`,
    );
  }
  if (parsed.protocol !== "https:") {
    throw new HubConfigError("HUB_PUBLIC_ORIGIN must use https://");
  }
  if (parsed.username !== "" || parsed.password !== "") {
    throw new HubConfigError("HUB_PUBLIC_ORIGIN must not contain userinfo");
  }
  if (parsed.pathname !== "/" || parsed.search !== "" || parsed.hash !== "") {
    throw new HubConfigError(
      "HUB_PUBLIC_ORIGIN must be exactly scheme://host[:port] with no path, query, or fragment",
    );
  }
  // `URL.origin` is the canonical serialization: lowercase host, no trailing
  // slash, default port elided. Requiring exact equality rejects aliases that
  // would otherwise be ambiguous against the Host header.
  if (parsed.origin !== value) {
    throw new HubConfigError(
      `HUB_PUBLIC_ORIGIN must be canonical; use "${parsed.origin}"`,
    );
  }
  if (parsed.hostname.length === 0 || parsed.hostname.endsWith(".")) {
    throw new HubConfigError("HUB_PUBLIC_ORIGIN must name a nonempty host");
  }
  if (parsed.host.length > 255) {
    throw new HubConfigError("HUB_PUBLIC_ORIGIN host is too long");
  }
  if (parsed.port === "0") {
    throw new HubConfigError("HUB_PUBLIC_ORIGIN must use a nonzero public port");
  }
  return {
    publicOrigin: value,
    hostHeader: parsed.host,
    hostname: parsed.hostname,
    port: parsed.port === "" ? 443 : Number(parsed.port),
  };
}

function parseRemoteConfig(publicOrigin: string, socketPath: string): HubRemoteConfig {
  const origin = parsePublicOrigin(publicOrigin);
  validateUnixSocketPathSyntax(socketPath);
  return { ...origin, socketPath };
}

/**
 * Structural validation for HUB_SOCKET_PATH. Filesystem checks (private
 * parent directory, symlinks, occupied socket) happen in the listener module
 * before Next.js is prepared.
 */
export function validateUnixSocketPathSyntax(value: string): void {
  if (typeof value !== "string" || value.length === 0) {
    throw new HubConfigError("HUB_SOCKET_PATH must be a nonempty absolute path");
  }
  if (Buffer.byteLength(value, "utf8") > MAX_UNIX_SOCKET_PATH_BYTES) {
    throw new HubConfigError(
      `HUB_SOCKET_PATH must be at most ${MAX_UNIX_SOCKET_PATH_BYTES} bytes for a Unix socket`,
    );
  }
  if (/[\u0000-\u001f\u007f]/.test(value)) {
    throw new HubConfigError(
      "HUB_SOCKET_PATH must not contain NUL or control characters",
    );
  }
  if (!isAbsolute(value)) {
    throw new HubConfigError("HUB_SOCKET_PATH must be an absolute path");
  }
  // Normalized absolute path: rejects `..`, repeated separators, and a
  // trailing separator. `/` itself has no basename and is rejected below.
  if (value !== resolve(value)) {
    throw new HubConfigError("HUB_SOCKET_PATH must be a normalized absolute path");
  }
  const name = basename(value);
  if (name === "" || name === "." || name === ".." || value === "/") {
    throw new HubConfigError("HUB_SOCKET_PATH must name a socket file");
  }
}

function parseHost(value: string | undefined): string {
  const host = value === undefined || value === "" ? "127.0.0.1" : value;
  if (typeof host !== "string" || host.length === 0 || host.length > 45) {
    throw new HubConfigError(`HUB_HOST must be a literal loopback address`);
  }
  if (isIPv4Loopback(host)) return host;
  if (host === "::1") return host;
  throw new HubConfigError(
    `HUB_HOST "${host}" is not a literal loopback address; only 127.0.0.0/8 and ::1 are allowed`,
  );
}

function isIPv4Loopback(host: string): boolean {
  if (isIP(host) !== 4) return false;
  const parts = host.split(".");
  if (parts[0] !== "127" || parts.length !== 4) return false;
  return parts.every((part) => /^\d+$/.test(part) && Number(part) <= 255);
}

function parsePort(value: string | undefined): number {
  const text = value === undefined || value === "" ? "8081" : value.trim();
  if (!/^\d+$/.test(text)) {
    throw new HubConfigError(`HUB_PORT must be a nonnegative integer, got "${text}"`);
  }
  const port = Number(text);
  if (port > 65535) {
    throw new HubConfigError(`HUB_PORT must be at most 65535, got ${text}`);
  }
  return port;
}

/**
 * Missing HUB_COLLECTORS yields an empty registry: UI/health/SSE still start,
 * but every collector connection is rejected. Wrong types, bounds, or
 * duplicate identities/tokens are configuration errors and exit nonzero.
 */
function parseCollectors(value: string | undefined): HubIdentityConfig[] {
  if (value === undefined || value === "") return [];
  if (Buffer.byteLength(value, "utf8") > MAX_HUB_COLLECTORS_BYTES) {
    throw new HubConfigError(
      `HUB_COLLECTORS must be at most ${MAX_HUB_COLLECTORS_BYTES} bytes`,
    );
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    throw new HubConfigError(`HUB_COLLECTORS is not valid JSON`);
  }
  if (!Array.isArray(parsed)) {
    throw new HubConfigError(`HUB_COLLECTORS must be a JSON array`);
  }
  const collectors: HubIdentityConfig[] = [];
  const ids = new Set<string>();
  const machineIds = new Set<string>();
  const tokens = new Set<string>();
  for (const [index, item] of parsed.entries()) {
    const message = `HUB_COLLECTORS[${index}]`;
    if (item === null || typeof item !== "object" || Array.isArray(item)) {
      throw new HubConfigError(`${message} must be an object`);
    }
    const record = item as Record<string, unknown>;
    const keys = Object.keys(record).sort();
    const expected = ["collectorId", "machineId", "token"].sort();
    if (
      keys.length !== expected.length ||
      keys.some((key, i) => key !== expected[i])
    ) {
      throw new HubConfigError(
        `${message} must contain exactly collectorId, machineId, and token`,
      );
    }
    const collectorId = boundedString(record.collectorId, "collectorId", message, MAX_COLLECTOR_ID_LENGTH);
    const machineId = boundedString(record.machineId, "machineId", message, MAX_MACHINE_ID_LENGTH);
    const token = boundedString(record.token, "token", message, MAX_TOKEN_LENGTH);
    if (ids.has(collectorId)) {
      throw new HubConfigError(`${message}: duplicate collectorId "${collectorId}"`);
    }
    if (machineIds.has(machineId)) {
      throw new HubConfigError(`${message}: duplicate machineId "${machineId}"`);
    }
    if (tokens.has(token)) {
      throw new HubConfigError(`${message}: duplicate token`);
    }
    ids.add(collectorId);
    machineIds.add(machineId);
    tokens.add(token);
    collectors.push({ collectorId, machineId, token });
  }
  return collectors;
}

function boundedString(
  value: unknown,
  field: string,
  message: string,
  maxLength: number,
): string {
  if (typeof value !== "string" || value.length === 0 || value.length > maxLength) {
    throw new HubConfigError(
      `${message}: ${field} must be a nonempty string of at most ${maxLength} characters`,
    );
  }
  return value;
}
