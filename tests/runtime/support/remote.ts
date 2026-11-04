/**
 * Fixtures for the remote Unix-socket hub boundary. These are isolated local
 * processes: no tailnet, Tailscale, Funnel, or public TCP listener is used.
 * Demo credentials are the same public local-only strings as the other
 * runtime fixtures.
 */

import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { TestContext } from "node:test";
import { HUB_BOOT_MS, type CredentialConfig, requireArtifacts } from "./config";
import { baseEnv, FIXTURE_BROWSER_USERS } from "./fixtures";
import { ChildHandle, ExitInfo, startChild } from "./process";
import { unixPathStats, unixRequest, type UnixRequestOptions, type UnixResponse } from "./unix";
import { BROWSER_PASSWORD, BROWSER_USERNAME } from "./browser";

export const REMOTE_LISTENING_LINE =
  /^hub listening on unix:(.+) \(public origin (https:\/\/[^()\s]+)\)$/;

export function remoteListenMatch(line: string): boolean {
  return REMOTE_LISTENING_LINE.test(line);
}

export type RemoteHubController = {
  child: ChildHandle;
  /** Temporary private root removed after the test. */
  directory: string;
  socketPath: string;
  publicOrigin: string;
  hostHeader: string;
  /** Canonical origin used as Origin for browser mutations. */
  baseUrl: string;
  browserHeaders: { cookie: string };
  csrfToken: string;
  request(options?: UnixRequestOptions): Promise<UnixResponse>;
  stop(signal?: NodeJS.Signals, graceMs?: number): Promise<ExitInfo>;
};

export type RemoteHubOptions = {
  collectors?: CredentialConfig[];
  /** `false` starts a fail-closed hub with no browser users. */
  browserUsers?: false | { username: string; machineIds: "*" | string[] }[];
  publicOrigin?: string;
  /** Username for the automatic fixture login (defaults to the demo viewer). */
  loginUsername?: string;
  /** Extra HUB_* variables (must not include HUB_HOST/HUB_PORT). */
  env?: Record<string, string | undefined>;
};

export async function startRemoteHub(
  t: TestContext,
  options: RemoteHubOptions = {},
): Promise<RemoteHubController> {
  requireArtifacts();
  const directory = mkdtempSync(join(tmpdir(), "rigscope-remote-hub-"));
  const socketPath = join(directory, "run", "hub.sock");
  const publicOrigin = options.publicOrigin ?? "https://hub.example.test";
  const hostHeader = new URL(publicOrigin).host;

  const overrides: Record<string, string | undefined> = {
    HUB_PUBLIC_ORIGIN: publicOrigin,
    HUB_SOCKET_PATH: socketPath,
    ...options.env,
  };
  if (options.collectors !== undefined) {
    overrides.HUB_COLLECTORS = JSON.stringify(options.collectors);
  }
  if (options.browserUsers !== false) {
    overrides.HUB_BROWSER_USERS = JSON.stringify(
      options.browserUsers
        ? options.browserUsers.map((user) => ({ ...user, passwordHash: FIXTURE_BROWSER_USERS[0].passwordHash }))
        : FIXTURE_BROWSER_USERS,
    );
  }

  const child = startChild("hub-remote", ["dist/hub.cjs"], baseEnv("hub", overrides));
  t.after(async () => {
    await child.stop();
    rmSync(directory, { recursive: true, force: true });
  });

  const line = await child.waitForLine(
    remoteListenMatch,
    HUB_BOOT_MS,
    "the 'hub listening on unix:<path> (public origin <https origin>)' line",
  );
  const match = REMOTE_LISTENING_LINE.exec(line);
  assert.ok(match, `remote listening line malformed: ${JSON.stringify(line)}`);
  assert.equal(match[1], socketPath, "the hub prints its exact Unix socket path");
  assert.equal(match[2], publicOrigin, "the hub prints its exact public origin");

  const socket = unixPathStats(socketPath);
  assert.ok(socket, "the hub created its Unix socket");
  assert.equal(socket.isSocket, true);
  assert.equal(socket.mode, 0o600, "the hub socket is mode 0600");

  const request = (requestOptions: UnixRequestOptions = {}): Promise<UnixResponse> =>
    unixRequest(socketPath, {
      ...requestOptions,
      headers: { host: hostHeader, ...(requestOptions.headers ?? {}) },
    });

  let browserHeaders: { cookie: string } = { cookie: "" };
  let csrfToken = "";
  if (options.browserUsers !== false) {
    const login = await loginRemote(
      { socketPath, publicOrigin, hostHeader },
      { username: options.loginUsername },
    );
    browserHeaders = login.headers;
    csrfToken = login.csrfToken;
  }

  return {
    child,
    directory,
    socketPath,
    publicOrigin,
    hostHeader,
    baseUrl: publicOrigin,
    browserHeaders,
    csrfToken,
    request,
    stop: (async (signal?: NodeJS.Signals, graceMs?: number) => {
      return await child.stop(signal, graceMs);
    }) as ChildHandle["stop"],
  };
}

export type RemoteLogin = {
  headers: { cookie: string };
  csrfToken: string;
  rawSetCookie: string | null;
};

export async function loginRemote(
  hub: { socketPath: string; publicOrigin: string; hostHeader: string },
  options: { username?: string; password?: string; origin?: string } = {},
): Promise<RemoteLogin> {
  const response = await unixRequest(hub.socketPath, {
    method: "POST",
    path: "/api/auth/login",
    headers: {
      host: hub.hostHeader,
      origin: options.origin ?? hub.publicOrigin,
      "content-type": "application/json",
    },
    body: JSON.stringify({
      username: options.username ?? BROWSER_USERNAME,
      password: options.password ?? BROWSER_PASSWORD,
    }),
  });
  const body = response.json as { username?: string; csrfToken?: string; error?: string };
  assert.equal(response.status, 200, `fixture remote login failed: ${JSON.stringify(body)}`);
  assert.equal(body.username, options.username ?? BROWSER_USERNAME);
  assert.equal(typeof body.csrfToken, "string");
  const rawSetCookie = firstSetCookie(response.headers["set-cookie"]);
  assert.ok(rawSetCookie, "remote login must issue a session cookie");
  assert.match(rawSetCookie, /^__Host-rigscope_session=/);
  assert.match(rawSetCookie, /;\s*Secure/i);
  assert.match(rawSetCookie, /HttpOnly/i);
  assert.match(rawSetCookie, /SameSite=Strict/i);
  assert.match(rawSetCookie, /Path=\//i);
  assert.equal(/Domain=/i.test(rawSetCookie), false);
  return {
    headers: { cookie: rawSetCookie.split(";", 1)[0] },
    csrfToken: body.csrfToken!,
    rawSetCookie,
  };
}

export function firstSetCookie(value: string | string[] | undefined): string | null {
  if (Array.isArray(value)) return value[0] ?? null;
  return value ?? null;
}

export function spawnRemoteCandidate(
  t: TestContext,
  overrides: Record<string, string | undefined>,
): ChildHandle {
  requireArtifacts();
  const child = startChild("hub-remote(misconfigured)", ["dist/hub.cjs"], baseEnv("hub", overrides));
  t.after(() => child.stop());
  return child;
}
