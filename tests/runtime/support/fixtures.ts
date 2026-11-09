import assert from "node:assert/strict";
import { type TestContext } from "node:test";
import { CONNECT_PATH, CredentialConfig, HUB_BOOT_MS, requireArtifacts } from "./config";
import { ChildHandle, ExitInfo, startChild } from "./process";
import { BROWSER_PASSWORD, BROWSER_USERNAME, loginBrowser } from "./browser";
import { hashPassword } from "../../../src/hub/auth/password";
import { dirname, join } from 'node:path';

export const FIXTURE_PASSWORD_HASH = hashPassword(BROWSER_PASSWORD);
export const FIXTURE_BROWSER_USERS = [{ username: BROWSER_USERNAME, passwordHash: FIXTURE_PASSWORD_HASH, machineIds: "*" }];

export function baseEnv(role: "hub" | "collector", overrides: Record<string, string | undefined>): NodeJS.ProcessEnv {
  // Strip NODE_ENV and every HUB_/COLLECTOR_ variable from the inherited shell
  // environment so a developer's leftovers cannot flip the packaged defaults.
  const env: Record<string, string | undefined> = {};
  for (const [key, value] of Object.entries(process.env)) {
    if (key === "NODE_ENV" || key.startsWith("HUB_") || key.startsWith("COLLECTOR_")) continue;
    if (value !== undefined) env[key] = value;
  }
  // Isolated state; every suite provides explicit config via overrides.
  if (role === 'hub') env.HUB_DATABASE_PATH = ':memory:';
  for (const [key, value] of Object.entries(overrides)) {
    if (value !== undefined) env[key] = value;
    else delete env[key];
  }
  if (role === "hub") env.NODE_ENV = "production"; // packaged entry-point default, made explicit
  return env as NodeJS.ProcessEnv;
}

export interface HubController {
  browserHeaders: { cookie: string };
  port: number;
  baseUrl: string;
  healthUrl: string;
  liveUrl: string;
  collectorWsUrl: string;
  /** Same path as the collector WS endpoint, for the plain-HTTP 426 check. */
  collectorHttpUrl: string;
  child: ChildHandle;
  stop(signal?: NodeJS.Signals, graceMs?: number): Promise<ExitInfo>;
}

export const LISTENING_LINE = /^hub listening on http:\/\/([^\s]+):(\d+)$/;

export function listenMatch(line: string): boolean {
  return LISTENING_LINE.test(line);
}

export async function startHub(
  t: TestContext,
  options: {
    host?: string; port?: number; collectors?: CredentialConfig[]; stateFile?: string; databasePath?: string;
    grants?: {token:string; expiresAt:string}[];
    browserUsers?: false | { username: string; machineIds: "*" | string[]; admin?: boolean }[];
    telemetryMode?: "memory" | "persistent";
    telemetryFlushMs?: number;
  } = {},
): Promise<HubController> {
  requireArtifacts();
  const host = options.host ?? "127.0.0.1";
  const overrides: Record<string, string | undefined> = { HUB_HOST: host, HUB_PORT: String(options.port ?? 0) };
  if (options.telemetryMode !== undefined) overrides.HUB_TELEMETRY_MODE = options.telemetryMode;
  if (options.telemetryFlushMs !== undefined) overrides.HUB_TELEMETRY_FLUSH_MS = String(options.telemetryFlushMs);
  if (options.databasePath || options.stateFile) overrides.HUB_DATABASE_PATH = options.databasePath ?? join(dirname(options.stateFile!), 'telemetry.sqlite');
  if (options.collectors !== undefined) overrides.HUB_COLLECTORS = JSON.stringify(options.collectors);
  if (options.stateFile) overrides.HUB_STATE_FILE = options.stateFile;
  if (options.grants) overrides.HUB_ENROLLMENT_GRANTS = JSON.stringify(options.grants);
  if (options.browserUsers !== false) {
    overrides.HUB_BROWSER_USERS = JSON.stringify(options.browserUsers
      ? options.browserUsers.map(user => ({ ...user, passwordHash: FIXTURE_PASSWORD_HASH }))
      : FIXTURE_BROWSER_USERS);
  }
  const child = startChild("hub", ["dist/hub.cjs"], baseEnv("hub", overrides));
  t.after(() => child.stop());
  const line = await child.waitForLine(listenMatch, HUB_BOOT_MS, "the 'hub listening on http://<host>:<port>' line");
  const match = LISTENING_LINE.exec(line);
  assert.ok(match, `listening line malformed: ${JSON.stringify(line)}`);
  assert.equal(match[1], host, `listening line host must equal the configured loopback host`);
  const port = Number(match[2]);
  assert.ok(Number.isInteger(port) && port > 0, `not an actual bound port: ${port}`);
  const baseUrl = `http://127.0.0.1:${port}`;
  const browser = options.browserUsers === false || options.browserUsers
    ? { headers: { cookie: "" } }
    : await loginBrowser(baseUrl);
  return {
    browserHeaders: browser.headers,
    port,
    baseUrl: `http://127.0.0.1:${port}`,
    healthUrl: `http://127.0.0.1:${port}/api/health`,
    liveUrl: `http://127.0.0.1:${port}/api/live`,
    collectorWsUrl: `ws://127.0.0.1:${port}${CONNECT_PATH}`,
    collectorHttpUrl: `http://127.0.0.1:${port}${CONNECT_PATH}`,
    child,
    stop: (async (signal?: NodeJS.Signals, graceMs?: number) => child.stop(signal, graceMs)) as ChildHandle["stop"],
  };
}

export function spawnStartupCandidate(t: TestContext, overrides: Record<string, string | undefined>): ChildHandle {
  requireArtifacts();
  const child = startChild("hub(misconfigured)", ["dist/hub.cjs"], baseEnv("hub", { HUB_PORT: "0", ...overrides }));
  t.after(() => child.stop());
  return child;
}

export function startCollector(
  t: TestContext,
  options: {
    collectorId: string;
    hostname: string;
    hubs: { url: string; token: string }[];
    intervalMs?: number;
  },
): ChildHandle {
  requireArtifacts();
  const env = baseEnv("collector", {
    COLLECTOR_MODE: "mock",
    COLLECTOR_ID: options.collectorId,
    COLLECTOR_HUBS: JSON.stringify(options.hubs),
    COLLECTOR_HOSTNAME: options.hostname,
    COLLECTOR_INTERVAL_MS: String(options.intervalMs ?? 250),
  });
  const child = startChild("collector", ["dist/collector.cjs"], env);
  t.after(() => child.stop());
  return child;
}
