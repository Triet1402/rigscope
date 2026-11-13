import assert from "node:assert/strict";
import { type TestContext } from "node:test";
import { CONNECT_PATH, type CredentialConfig, HUB_BOOT_MS } from "./config";
import { ChildHandle, ExitInfo } from "./process";
import { startCollectorProcess, startHubProcess, type HubLaunchSpec } from "./launcher";
import { BROWSER_USERNAME, loginBrowser } from "./browser";

export { baseEnv } from "./env";

export const FIXTURE_BROWSER_USERS = [{ username: BROWSER_USERNAME, machineIds: "*" as const }];

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
    host?: string;
    port?: number;
    collectors?: CredentialConfig[];
    stateDir?: string;
    databasePath?: string;
    grants?: { token: string; expiresAt: string }[];
    browserUsers?: false | { username: string; machineIds: "*" | string[]; admin?: boolean }[];
    telemetryFlushMs?: number | null;
    telemetryMode?: "memory" | "persistent";
  } = {},
): Promise<HubController> {
  const host = options.host ?? "127.0.0.1";
  const spec: HubLaunchSpec = {
    label: "hub",
    host,
    port: options.port ?? 0,
    stateDir: options.stateDir,
    databasePath: options.databasePath,
    collectors: options.collectors,
    grants: options.grants,
    flushMs: options.telemetryFlushMs,
    telemetryMode: options.telemetryMode,
    browserUsers:
      options.browserUsers === false
        ? false
        : options.browserUsers ?? FIXTURE_BROWSER_USERS,
  };
  const child = startHubProcess(t, spec);
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
    baseUrl,
    healthUrl: `http://127.0.0.1:${port}/api/health`,
    liveUrl: `http://127.0.0.1:${port}/api/live`,
    collectorWsUrl: `ws://127.0.0.1:${port}${CONNECT_PATH}`,
    collectorHttpUrl: `http://127.0.0.1:${port}${CONNECT_PATH}`,
    child,
    stop: (async (signal?: NodeJS.Signals, graceMs?: number) => child.stop(signal, graceMs)) as ChildHandle["stop"],
  };
}

/** Launch a hub that is expected to reject its configuration before listening. */
export function spawnStartupCandidate(t: TestContext, overrides: Partial<HubLaunchSpec>): ChildHandle {
  return startHubProcess(t, { label: "hub(misconfigured)", port: 0, browserUsers: false, ...overrides });
}

export function startCollector(
  t: TestContext,
  options: {
    collectorId: string;
    hostname: string;
    hubs: { url: string; token: string }[];
    intervalMs?: number;
    stateDir?: string;
    mode?: "real" | "mock";
  },
): ChildHandle {
  return startCollectorProcess(t, {
    mode: options.mode ?? "mock",
    stateDir: options.stateDir,
    collectorId: options.collectorId,
    hostname: options.hostname,
    hubs: options.hubs,
    intervalMs: options.intervalMs ?? 250,
  });
}
