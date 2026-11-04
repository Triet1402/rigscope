/**
 * Focused tests for scripts/validate-https.cjs: canonical origin parsing,
 * private password-file refusal, exact cookie validation, fail-closed HTTPS
 * behavior, and the full probe sequence against a local HTTPS fixture whose
 * self-signed CA is explicitly injected as a trust anchor.
 *
 * The fixture CA is test-only. The production CLI passes no CA and therefore
 * uses Node's system trust store with normal certificate verification; one
 * test below proves the fixture certificate is rejected without the CA.
 */

import { strict as assert } from "node:assert";
import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import { createServer, type Server } from "node:https";
import { createRequire } from "node:module";
import { after, describe, it } from "node:test";
import { tmpdir } from "node:os";
import { join } from "node:path";

const requireScript = createRequire(import.meta.url);

type OriginInfo = { origin: string; host: string; hostname: string; port: number };
type ValidationResult = {
  origin: string;
  username: string;
  machineId: string;
  machineSelectedByOperator: boolean;
  cpuStatus: string;
  memoryStatus: string;
  keepaliveMs: number;
  summaryObservations: number;
  checks: string[];
};
type ValidationOptions = Record<string, unknown>;
type CliApi = {
  ValidationError: new (message: string) => Error;
  parseCanonicalHttpsOrigin(value: string): OriginInfo;
  resolveValidationConfig(env: Record<string, string | undefined>): {
    origin: string;
    originInfo: OriginInfo;
    username: string;
    passwordFile: string;
    machineId?: string;
  };
  readPasswordFile(path: string): Promise<string>;
  randomPasswordDifferentFrom(value: string): string;
  assertSecureSessionCookie(
    headers: Record<string, string | string[] | undefined>,
    label: string,
  ): string;
  assertClearedSessionCookie(
    headers: Record<string, string | string[] | undefined>,
    label: string,
  ): void;
  validateHttpsDeployment(
    input: {
      origin: string;
      username?: string;
      password: string;
      machineId?: string;
    },
    options?: ValidationOptions,
  ): Promise<ValidationResult>;
  formatSuccess(result: ValidationResult): string;
  main(argv?: string[], env?: Record<string, string | undefined>): Promise<ValidationResult>;
};

const cli = requireScript("../scripts/validate-https.cjs") as CliApi;

// -- local TLS fixture certificate -----------------------------------------

type TlsFixture = {
  key: Buffer;
  cert: string;
  ca: string;
  dispose(): void;
};

let cachedTls: TlsFixture | null | undefined;

function getTlsFixture(): TlsFixture | null {
  if (cachedTls !== undefined) return cachedTls;
  const probe = spawnSync("openssl", ["version"], { encoding: "utf8" });
  if (probe.error !== undefined || probe.status !== 0) {
    cachedTls = null;
    return null;
  }
  const dir = mkdtempSync(join(tmpdir(), "rigscope-https-validation-cert-"));
  const keyPath = join(dir, "key.pem");
  const certPath = join(dir, "cert.pem");
  const generated = spawnSync("openssl", [
    "req", "-x509", "-newkey", "rsa:2048", "-sha256", "-days", "1", "-nodes",
    "-subj", "/CN=127.0.0.1",
    "-addext", "subjectAltName=IP:127.0.0.1,DNS:localhost",
    "-keyout", keyPath,
    "-out", certPath,
  ], { encoding: "utf8" });
  if (generated.status !== 0) {
    rmSync(dir, { recursive: true, force: true });
    cachedTls = null;
    return null;
  }
  cachedTls = {
    key: readFileSync(keyPath),
    cert: readFileSync(certPath, "utf8"),
    ca: readFileSync(certPath, "utf8"),
    dispose: () => rmSync(dir, { recursive: true, force: true }),
  };
  return cachedTls;
}

after(() => {
  cachedTls?.dispose();
});

// -- fixture HTTPS server ---------------------------------------------------

const SESSION_COOKIE = "__Host-rigscope_session";
const FIXTURE_HUB_RUN_ID = "fixture-hub-run";

type FixtureStats = {
  invalidLogins: number;
  originRejected: number;
  validLogins: number;
  csrfRejected: number;
  logouts: number;
  liveUnauthorized: number;
  keepalives: number;
  updates: number;
  streamsClosedByLogout: number;
};

type FixtureOptions = {
  password: string;
  machineId?: string;
  keepaliveMs?: number;
  updateMs?: number;
  updates?: boolean;
  initialCpuUnavailable?: boolean;
  redirectHealth?: boolean;
  healthStatus?: number;
  healthBody?: string;
  initialEventName?: string;
  oversizedSseEvent?: boolean;
  relaxedLoginCookie?: boolean;
  cacheableSession?: boolean;
  closeResetEarly?: boolean;
  floodReset?: boolean;
  redirectLive?: boolean;
};

type StreamRecord = { response: ServerResponse; timers: NodeJS.Timeout[] };
type SessionRecord = {
  username: string;
  csrfToken: string;
  streams: Set<StreamRecord>;
};

type Fixture = {
  origin: string;
  ca: string;
  password: string;
  stats: FixtureStats;
  close(): Promise<void>;
};

function sendJson(
  response: ServerResponse,
  status: number,
  body: unknown,
  headers: Record<string, string> = {},
): void {
  response.writeHead(status, { "content-type": "application/json", "cache-control": "no-store", ...headers });
  response.end(JSON.stringify(body));
}

function readBody(request: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let bytes = 0;
    request.on("data", (chunk: Buffer) => {
      bytes += chunk.length;
      if (bytes > 4096) {
        request.destroy();
        reject(new Error("fixture body too large"));
        return;
      }
      chunks.push(chunk);
    });
    request.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    request.on("error", reject);
  });
}

function cookieToken(request: IncomingMessage): string | null {
  const header = request.headers.cookie;
  if (typeof header !== "string") return null;
  for (const part of header.split(";")) {
    const segment = part.trim();
    const equals = segment.indexOf("=");
    if (equals < 0) continue;
    if (segment.slice(0, equals) === SESSION_COOKIE) {
      return segment.slice(equals + 1);
    }
  }
  return null;
}

async function startFixture(tls: TlsFixture, options: FixtureOptions): Promise<Fixture> {
  const machineId = options.machineId ?? "fixture-machine";
  const collectorId = "fixture-collector";
  const password = options.password;
  const stats: FixtureStats = {
    invalidLogins: 0,
    originRejected: 0,
    validLogins: 0,
    csrfRejected: 0,
    logouts: 0,
    liveUnauthorized: 0,
    keepalives: 0,
    updates: 0,
    streamsClosedByLogout: 0,
  };
  const sessions = new Map<string, SessionRecord>();
  const baseTime = Date.now();
  let sequence = 1;
  let cursor = 0;
  let origin = "";

  function snapshot(seq: number): unknown {
    const capturedAt = new Date(baseTime + seq * 1_000).toISOString();
    const metrics = {
      collectorId,
      runId: "fixture-run",
      bootId: "fixture-boot",
      sequence: seq,
      capturedAt,
      kind: "metrics",
      payload: {
        cpu: options.initialCpuUnavailable === true && seq === 1
          ? { status: "unavailable", reason: "fixture warmup" }
          : { status: "ok", usagePercent: 10 + seq, perCore: [10 + seq] },
        memory: { status: "ok", usedBytes: 1_000 + seq, totalBytes: 2_000 },
        swap: { status: "unavailable", reason: "fixture has no swap" },
        gpus: { status: "unavailable", reason: "no GPUs detected" },
        disks: { status: "ok", filesystems: [], devices: [] },
        network: { status: "ok", interfaces: [] },
      },
      receivedAt: capturedAt,
    };
    return {
      hubRunId: FIXTURE_HUB_RUN_ID,
      machines: [{
        machineId,
        collectorId,
        hostname: "fixture-host",
        connected: true,
        lastReceivedAt: capturedAt,
        metrics,
        expectedIntervalMs: null,
      }],
    };
  }

  function nextCursor(): number {
    cursor += 1;
    return cursor;
  }

  function writeEvent(response: ServerResponse, seq: number, event: "snapshot" | "reset" | "update"): void {
    if (response.writableEnded) return;
    response.write(
      `id: ${FIXTURE_HUB_RUN_ID}:${nextCursor()}\nevent: ${event}\ndata: ${JSON.stringify(snapshot(seq))}\n\n`,
    );
  }

  function cleanupStream(stream: StreamRecord, session: SessionRecord): void {
    for (const timer of stream.timers) clearInterval(timer);
    stream.timers.length = 0;
    session.streams.delete(stream);
  }

  function endStream(stream: StreamRecord, session: SessionRecord): void {
    cleanupStream(stream, session);
    if (!stream.response.writableEnded) stream.response.end();
  }

  async function handle(request: IncomingMessage, response: ServerResponse): Promise<void> {
    const url = new URL(request.url ?? "/", "http://fixture.invalid");

    // This fixture applies the same broad Origin rule as the remote boundary:
    // any foreign Origin is rejected before auth or routing.
    if (typeof request.headers.origin === "string" && request.headers.origin !== origin) {
      stats.originRejected += 1;
      sendJson(response, 403, { error: "origin not allowed" });
      return;
    }

    if (url.pathname === "/api/health") {
      if (options.redirectHealth === true) {
        response.writeHead(302, { location: "https://invalid.example.test/" });
        response.end("health-redirect-canary");
        return;
      }
      response.writeHead(options.healthStatus ?? 200, { "content-type": "application/json" });
      response.end(options.healthBody ?? JSON.stringify({
        status: "ok",
        mode: "fixture",
        hubRunId: FIXTURE_HUB_RUN_ID,
      }));
      return;
    }

    if (url.pathname === "/api/auth/login") {
      if (request.method !== "POST") {
        sendJson(response, 405, { error: "POST required" });
        return;
      }
      if (request.headers.origin !== origin) {
        stats.originRejected += 1;
        sendJson(response, 403, { error: "origin not allowed" });
        return;
      }
      let body: unknown;
      try {
        body = JSON.parse(await readBody(request));
      } catch {
        sendJson(response, 400, { error: "invalid login request" });
        return;
      }
      if (body === null || typeof body !== "object" || Array.isArray(body)) {
        sendJson(response, 400, { error: "invalid login request" });
        return;
      }
      const record = body as Record<string, unknown>;
      if (record.username !== "owner" || record.password !== password) {
        stats.invalidLogins += 1;
        sendJson(response, 401, { error: "invalid username or password" });
        return;
      }
      const token = randomBytes(32).toString("base64url");
      const csrfToken = randomBytes(32).toString("base64url");
      sessions.set(token, { username: "owner", csrfToken, streams: new Set() });
      stats.validLogins += 1;
      sendJson(response, 200, { username: "owner", csrfToken }, {
        "set-cookie": `${SESSION_COOKIE}=${token}; ${options.relaxedLoginCookie ? '' : 'Secure; '}HttpOnly; SameSite=Strict; Path=/; Max-Age=43200`,
      });
      return;
    }

    if (url.pathname === "/api/auth/session") {
      const token = cookieToken(request);
      const session = token === null ? undefined : sessions.get(token);
      if (session === undefined) {
        sendJson(response, 401, { error: "authentication required" });
        return;
      }
      sendJson(response, 200, { username: session.username, csrfToken: session.csrfToken },
        options.cacheableSession ? { "cache-control": "public, max-age=60" } : {});
      return;
    }

    if (url.pathname === "/api/auth/logout") {
      if (request.method !== "POST") {
        sendJson(response, 405, { error: "POST required" });
        return;
      }
      if (request.headers.origin !== origin) {
        stats.originRejected += 1;
        sendJson(response, 403, { error: "origin not allowed" });
        return;
      }
      const token = cookieToken(request);
      const session = token === null ? undefined : sessions.get(token);
      if (token === null || session === undefined) {
        sendJson(response, 401, { error: "authentication required" }, {
          "set-cookie": `${SESSION_COOKIE}=; Secure; HttpOnly; SameSite=Strict; Path=/; Max-Age=0`,
        });
        return;
      }
      if (typeof request.headers["x-csrf-token"] !== "string"
        || request.headers["x-csrf-token"] !== session.csrfToken) {
        stats.csrfRejected += 1;
        sendJson(response, 403, { error: "missing or invalid csrf token" });
        return;
      }
      sessions.delete(token);
      stats.logouts += 1;
      for (const stream of [...session.streams]) {
        stats.streamsClosedByLogout += 1;
        endStream(stream, session);
      }
      sendJson(response, 200, { ok: true }, {
        "set-cookie": `${SESSION_COOKIE}=; Secure; HttpOnly; SameSite=Strict; Path=/; Max-Age=0`,
      });
      return;
    }

    if (url.pathname === "/api/live") {
      if (request.method !== "GET") {
        sendJson(response, 405, { error: "GET required" });
        return;
      }
      const token = cookieToken(request);
      const session = token === null ? undefined : sessions.get(token);
      if (session === undefined) {
        stats.liveUnauthorized += 1;
        sendJson(response, 401, { error: "authentication required" });
        return;
      }
      if (options.redirectLive) {
        response.writeHead(302, { location: "https://invalid.example.test/" });
        response.end("sse-redirect-canary");
        return;
      }
      response.writeHead(200, {
        "content-type": "text/event-stream; charset=utf-8",
        "cache-control": "no-cache, no-store, no-transform",
        connection: "keep-alive",
        "x-accel-buffering": "no",
      });
      const stream: StreamRecord = { response, timers: [] };
      session.streams.add(stream);
      const cleanup = (): void => cleanupStream(stream, session);
      response.on("close", cleanup);
      response.on("error", cleanup);

      const reset = typeof request.headers["last-event-id"] === "string";
      if (options.initialEventName !== undefined) {
        response.write(`event: ${options.initialEventName}\ndata: ${JSON.stringify(snapshot(sequence))}\n\n`);
        return;
      }
      if (options.oversizedSseEvent === true) {
        response.write("event: snapshot\n" + "data: xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n".repeat(30_000));
        return;
      }
      writeEvent(response, sequence, reset ? "reset" : "snapshot");
      if (reset && options.closeResetEarly) {
        endStream(stream, session);
        return;
      }
      if (reset && options.floodReset) {
        const data = JSON.stringify({ hubRunId: FIXTURE_HUB_RUN_ID, machines: [], padding: "x".repeat(140_000) });
        for (let index = 0; index < 35; index++) response.write("event: update\ndata: " + data + "\n\n");
        return;
      }
      if (options.updates !== false) {
        let sent = 0;
        const timer = setInterval(() => {
          if (response.writableEnded) return;
          sent += 1;
          sequence += 1;
          stats.updates += 1;
          writeEvent(response, sequence, "update");
          if (sent >= 3) clearInterval(timer);
        }, options.updateMs ?? 60);
        stream.timers.push(timer);
      }
      const keepalive = setInterval(() => {
        if (response.writableEnded) return;
        stats.keepalives += 1;
        response.write(`: keepalive ${Date.now()}\n\n`);
      }, options.keepaliveMs ?? 100);
      stream.timers.push(keepalive);
      return;
    }

    sendJson(response, 404, { error: "not found" });
  }

  const server: Server = createServer({ key: tls.key, cert: tls.cert }, (request, response) => {
    void handle(request, response).catch(() => {
      if (!response.headersSent) response.writeHead(500);
      response.end();
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("fixture did not bind a TCP port");
  }
  origin = `https://127.0.0.1:${address.port}`;

  return {
    origin,
    ca: tls.ca,
    password,
    stats,
    close: async () => {
      for (const session of sessions.values()) {
        for (const stream of [...session.streams]) endStream(stream, session);
      }
      const closable = server as unknown as { closeAllConnections?: () => void };
      closable.closeAllConnections?.();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}

const FAST_OPTIONS: ValidationOptions = {
  totalTimeoutMs: 8_000,
  requestTimeoutMs: 2_000,
  initialEventMs: 2_000,
  sampleWaitMs: 3_000,
  logoutCloseMs: 2_000,
  keepaliveWaitMs: 2_000,
  sseKeepalive: { expectedMs: 100, minMs: 50, maxMs: 1_000 },
};

function fixtureOptions(fixture: Fixture, overrides: ValidationOptions = {}): ValidationOptions {
  return { ...FAST_OPTIONS, trustedCa: fixture.ca, ...overrides };
}

async function expectFailure(
  work: Promise<unknown>,
  pattern: RegExp,
  forbidden?: string,
): Promise<Error> {
  let failure: Error | null = null;
  try {
    await work;
  } catch (error) {
    failure = error instanceof Error ? error : new Error(String(error));
  }
  assert.ok(failure !== null, "expected the validation to fail");
  assert.match(failure.message, pattern);
  if (forbidden !== undefined) {
    assert.equal(failure.message.includes(forbidden), false, "failure message must not include the secret");
  }
  return failure;
}

// -- tests ------------------------------------------------------------------

describe("HTTPS validation CLI configuration", () => {
  it("accepts only a canonical HTTPS origin", () => {
    for (const value of [
      "https://127.0.0.1:8443",
      "https://rig.tailnet.example.test",
      "https://[::1]:8443",
    ]) {
      const parsed = cli.parseCanonicalHttpsOrigin(value);
      assert.equal(parsed.origin, value);
    }
    for (const value of [
      "",
      "http://rig.tailnet.example.test",
      "ws://rig.tailnet.example.test",
      "https://rig.tailnet.example.test/",
      "https://rig.tailnet.example.test/path",
      "https://rig.tailnet.example.test?query=1",
      "https://rig.tailnet.example.test#fragment",
      "https://user:pass@rig.tailnet.example.test",
      "https://RIG.TAILNET.EXAMPLE.TEST",
      "https://rig.tailnet.example.test:443",
      "https://rig.tailnet.example.test:0",
      "https://rig.tailnet.example.test.",
      "https://rig tailnet.example.test",
      "not a url",
    ]) {
      assert.throws(() => cli.parseCanonicalHttpsOrigin(value), `${JSON.stringify(value)} must be rejected`);
    }
  });

  it("defaults the username and rejects plaintext password environment values and arguments", async () => {
    const env = {
      RIGSCOPE_VALIDATE_ORIGIN: "https://rig.tailnet.example.test:8443",
      RIGSCOPE_VALIDATE_PASSWORD_FILE: "/private/rigscope/browser-password",
    };
    const config = cli.resolveValidationConfig(env);
    assert.equal(config.username, "owner");
    assert.equal(config.origin, env.RIGSCOPE_VALIDATE_ORIGIN);

    await expectFailure(
      cli.main([], { ...env, RIGSCOPE_VALIDATE_PASSWORD: "plaintext-secret" }),
      /plaintext passwords are not accepted in environment variables/,
      "plaintext-secret",
    );
    await expectFailure(
      cli.main(["--password", "plaintext-secret"], env),
      /takes no arguments/,
      "plaintext-secret",
    );
  });

  it("refuses relative password-file paths", () => {
    assert.throws(
      () => cli.resolveValidationConfig({
        RIGSCOPE_VALIDATE_ORIGIN: "https://rig.tailnet.example.test:8443",
        RIGSCOPE_VALIDATE_PASSWORD_FILE: "relative/password",
      }),
      /normalized absolute path/,
    );
  });
});

describe("private password file", () => {
  it("reads only a private owned regular file and never leaks its contents", async (t) => {
    const root = mkdtempSync(join(tmpdir(), "rigscope-https-password-"));
    t.after(() => rmSync(root, { recursive: true, force: true }));
    const secret = "fixture-password-value-not-real";
    const file = join(root, "password");
    writeFileSync(file, `${secret}\n`, { mode: 0o600 });
    chmodSync(file, 0o600);
    assert.equal(await cli.readPasswordFile(file), secret);

    chmodSync(file, 0o644);
    await expectFailure(cli.readPasswordFile(file), /mode must be 0600 or 0400/, secret);

    chmodSync(file, 0o400);
    assert.equal(await cli.readPasswordFile(file), secret);

    const target = join(root, "target");
    writeFileSync(target, `${secret}\n`, { mode: 0o600 });
    chmodSync(target, 0o600);
    const link = join(root, "link");
    symlinkSync(target, link);
    await expectFailure(cli.readPasswordFile(link), /symbolic link/, secret);

    const directory = join(root, "directory");
    mkdirSync(directory, { mode: 0o700 });
    await expectFailure(cli.readPasswordFile(directory), /regular file/, secret);

    const empty = join(root, "empty");
    writeFileSync(empty, "", { mode: 0o600 });
    chmodSync(empty, 0o600);
    await expectFailure(cli.readPasswordFile(empty), /must not be empty/, secret);
  });
});

describe("exact remote session cookie validation", () => {
  const token = "A".repeat(43);

  it("accepts only the exact Secure host-only fixed-lifetime cookie", () => {
    const valid = {
      "set-cookie": [`${SESSION_COOKIE}=${token}; Secure; HttpOnly; SameSite=Strict; Path=/; Max-Age=43200`],
    };
    assert.equal(cli.assertSecureSessionCookie(valid, "login"), token);

    const invalid = [
      `${SESSION_COOKIE}=${token}; HttpOnly; SameSite=Strict; Path=/; Max-Age=43200`,
      `${SESSION_COOKIE}=${token}; Secure; SameSite=Strict; Path=/; Max-Age=43200`,
      `${SESSION_COOKIE}=${token}; Secure; HttpOnly; SameSite=Lax; Path=/; Max-Age=43200`,
      `${SESSION_COOKIE}=${token}; Secure; HttpOnly; SameSite=Strict; Path=/wrong; Max-Age=43200`,
      `${SESSION_COOKIE}=${token}; Secure; HttpOnly; SameSite=Strict; Path=/; Max-Age=3600`,
      `${SESSION_COOKIE}=${token}; Secure; HttpOnly; SameSite=Strict; Path=/; Max-Age=43200; Domain=example.test`,
      `rigscope_session=${token}; Secure; HttpOnly; SameSite=Strict; Path=/; Max-Age=43200`,
    ];
    for (const header of invalid) {
      let rejected = false;
      let message = "";
      try {
        cli.assertSecureSessionCookie({ "set-cookie": [header] }, "login");
      } catch (error) {
        rejected = true;
        message = error instanceof Error ? error.message : String(error);
      }
      assert.equal(rejected, true, "relaxed cookie must be rejected");
      assert.equal(message.includes(token), false, "cookie value must never appear in an error");
    }

    assert.throws(
      () => cli.assertSecureSessionCookie({ "set-cookie": [valid["set-cookie"][0], valid["set-cookie"][0]] }, "login"),
      /exactly one session cookie/,
    );
  });

  it("accepts only the matching logout deletion cookie", () => {
    cli.assertClearedSessionCookie({
      "set-cookie": [`${SESSION_COOKIE}=; Secure; HttpOnly; SameSite=Strict; Path=/; Max-Age=0`],
    }, "logout");
    assert.throws(
      () => cli.assertClearedSessionCookie({
        "set-cookie": [`${SESSION_COOKIE}=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0`],
      }, "logout"),
      /must carry secure/,
    );
  });

  it("generates a cryptographically random invalid-password probe distinct from the real value", () => {
    const actual = "fixture-password-value-not-real";
    const probe = cli.randomPasswordDifferentFrom(actual);
    assert.notEqual(probe, actual);
    assert.match(probe, /^[A-Za-z0-9_-]{43}$/);
  });

  it("compares the invalid-password probe against the hub's normalized password", (t) => {
    const crypto = requireScript("node:crypto") as typeof import("node:crypto");
    t.mock.method(crypto, "randomBytes", () => Buffer.alloc(32));
    assert.throws(() => cli.randomPasswordDifferentFrom("Ａ".repeat(43)), /independent invalid-password probe/);
  });
});

describe("actual HTTPS probe sequence", () => {
  it("cleans up failed sessions and rejects dead streams, cacheable auth and SSE redirects", async (t) => {
    const tls = getTlsFixture();
    if (tls === null) { t.skip("openssl is unavailable"); return; }
    const cases: { options: Partial<FixtureOptions>; error: RegExp }[] = [
      { options: { relaxedLoginCookie: true }, error: /must carry secure/ },
      { options: { cacheableSession: true }, error: /Cache-Control: no-store/ },
      { options: { closeResetEarly: true }, error: /ended before logout/ },
      { options: { floodReset: true }, error: /bounded frame queue/ },
      { options: { redirectLive: true }, error: /redirects are not followed/ },
    ];
    for (const check of cases) {
      const fixture = await startFixture(tls, { password: "fixture-password", ...check.options });
      try {
        await expectFailure(cli.validateHttpsDeployment({
          origin: fixture.origin, password: fixture.password,
        }, fixtureOptions(fixture)), check.error, "sse-redirect-canary");
        assert.equal(fixture.stats.logouts, 1, "a failed probe must attempt to end its authenticated session");
      } finally { await fixture.close(); }
    }
  });
  it("bounds multiline SSE events and keeps unexpected event names out of errors", async (t) => {
    const tls = getTlsFixture();
    if (tls === null) { t.skip("openssl is unavailable"); return; }
    const canary = "response-body-secret-canary";
    for (const options of [{ initialEventName: canary }, { oversizedSseEvent: true }]) {
      const fixture = await startFixture(tls, { password: "fixture-password", ...options });
      try {
        await expectFailure(cli.validateHttpsDeployment({
          origin: fixture.origin, password: fixture.password,
        }, fixtureOptions(fixture)), /must be snapshot|bounded parser buffer/, canary);
      } finally { await fixture.close(); }
    }
  });
  it("rejects the fixture certificate without an injected CA and ignores a disable-verification option", async (t) => {
    const tls = getTlsFixture();
    if (tls === null) {
      t.skip("openssl is unavailable");
      return;
    }
    const fixture = await startFixture(tls, { password: "fixture-password" });
    t.after(async () => fixture.close());

    await expectFailure(
      cli.validateHttpsDeployment({
        origin: fixture.origin,
        username: "owner",
        password: fixture.password,
      }, {
        totalTimeoutMs: 3_000,
        requestTimeoutMs: 2_000,
      }),
      /certificate/i,
      fixture.password,
    );

    // The helper accepts only an additional trust anchor. An unknown option
    // named rejectUnauthorized must not disable verification.
    await expectFailure(
      cli.validateHttpsDeployment({
        origin: fixture.origin,
        username: "owner",
        password: fixture.password,
      }, {
        totalTimeoutMs: 3_000,
        requestTimeoutMs: 2_000,
        rejectUnauthorized: false,
      }),
      /certificate/i,
      fixture.password,
    );
  });

  it("passes the complete probe sequence against a trusted local HTTPS fixture", async (t) => {
    const tls = getTlsFixture();
    if (tls === null) {
      t.skip("openssl is unavailable");
      return;
    }
    const password = "fixture-browser-password-not-real";
    const fixture = await startFixture(tls, {
      password,
      machineId: "fixture-machine",
      keepaliveMs: 100,
      updateMs: 60,
    });
    t.after(async () => fixture.close());

    const result = await cli.validateHttpsDeployment({
      origin: fixture.origin,
      username: "owner",
      password,
      machineId: "fixture-machine",
    }, fixtureOptions(fixture));

    assert.equal(result.origin, fixture.origin);
    assert.equal(result.machineId, "fixture-machine");
    assert.equal(result.machineSelectedByOperator, true);
    assert.equal(result.cpuStatus, "ok");
    assert.equal(result.memoryStatus, "ok");
    assert.ok(result.keepaliveMs >= 50 && result.keepaliveMs <= 1_000);
    assert.ok(result.summaryObservations >= 2);
    for (const check of [
      "TLS certificate verification and /api/health",
      "anonymous /api/live rejection and Origin enforcement",
      "invalid password rejection",
      "exact Origin and forwarded-spoof rejection",
      "secure __Host session cookie",
      "session restore",
      "authenticated SSE snapshot for a connected machine",
      "reconnect reset with event ID",
      "15-second SSE keepalive",
      "advancing CPU/memory summaries",
      "logout CSRF rejection",
      "session survives rejected logout",
      "logout cookie deletion attributes",
      "logout closes active SSE streams",
      "post-logout rejection",
    ]) {
      assert.ok(result.checks.includes(check), `missing check: ${check}`);
    }
    assert.equal(JSON.stringify(result).includes(password), false);

    assert.ok(fixture.stats.invalidLogins >= 1);
    assert.ok(fixture.stats.originRejected >= 4);
    assert.equal(fixture.stats.validLogins, 1);
    assert.equal(fixture.stats.csrfRejected, 1);
    assert.equal(fixture.stats.logouts, 1);
    assert.ok(fixture.stats.liveUnauthorized >= 2);
    assert.ok(fixture.stats.keepalives >= 1);
    assert.ok(fixture.stats.updates >= 2);
    assert.equal(fixture.stats.streamsClosedByLogout, 2);

    const output = cli.formatSuccess(result);
    for (const phrase of [
      "only covers automated HTTPS checks",
      "Browser cookie acceptance",
      "actual fixed session expiry",
      "real WSS collector",
      "HMR edits",
      "shutdown",
      "does not enable or deploy",
      "does not by itself validate",
    ]) {
      assert.ok(output.includes(phrase), `missing scope phrase: ${phrase}`);
    }
    assert.equal(output.includes(password), false);
  });

  it("waits past a collector's first unavailable CPU sample for advancing usable readings", async (t) => {
    const tls = getTlsFixture();
    if (tls === null) {
      t.skip("openssl is unavailable");
      return;
    }
    const fixture = await startFixture(tls, {
      password: "fixture-password",
      initialCpuUnavailable: true,
      updateMs: 60,
    });
    t.after(async () => fixture.close());

    const result = await cli.validateHttpsDeployment({
      origin: fixture.origin,
      username: "owner",
      password: fixture.password,
    }, fixtureOptions(fixture));

    assert.equal(result.cpuStatus, "ok");
    assert.equal(result.memoryStatus, "ok");
    assert.ok(result.summaryObservations >= 2);
  });

  it("fails with a bounded error when the configured machine is missing", async (t) => {
    const tls = getTlsFixture();
    if (tls === null) {
      t.skip("openssl is unavailable");
      return;
    }
    const fixture = await startFixture(tls, { password: "fixture-password" });
    t.after(async () => fixture.close());

    await expectFailure(
      cli.validateHttpsDeployment({
        origin: fixture.origin,
        username: "owner",
        password: fixture.password,
        machineId: "another-machine",
      }, fixtureOptions(fixture)),
      /RIGSCOPE_VALIDATE_MACHINE_ID did not match/,
      fixture.password,
    );
  });

  it("fails within the sample window when summaries never advance", async (t) => {
    const tls = getTlsFixture();
    if (tls === null) {
      t.skip("openssl is unavailable");
      return;
    }
    const fixture = await startFixture(tls, {
      password: "fixture-password",
      updates: false,
    });
    t.after(async () => fixture.close());

    const started = Date.now();
    await expectFailure(
      cli.validateHttpsDeployment({
        origin: fixture.origin,
        username: "owner",
        password: fixture.password,
      }, fixtureOptions(fixture, { sampleWaitMs: 250 })),
      /no advancing CPU\/memory summary/,
      fixture.password,
    );
    assert.ok(Date.now() - started < 3_000, "the sample wait must be bounded");
  });

  it("enforces keepalive timing rather than accepting any comment", async (t) => {
    const tls = getTlsFixture();
    if (tls === null) {
      t.skip("openssl is unavailable");
      return;
    }
    const fixture = await startFixture(tls, {
      password: "fixture-password",
      keepaliveMs: 300,
    });
    t.after(async () => fixture.close());

    await expectFailure(
      cli.validateHttpsDeployment({
        origin: fixture.origin,
        username: "owner",
        password: fixture.password,
      }, fixtureOptions(fixture, {
        keepaliveWaitMs: 1_000,
        sseKeepalive: { expectedMs: 100, minMs: 50, maxMs: 150 },
      })),
      /SSE keepalive arrived after/,
      fixture.password,
    );
  });

  it("fails closed on redirects and non-OK health responses without exposing the body", async (t) => {
    const tls = getTlsFixture();
    if (tls === null) {
      t.skip("openssl is unavailable");
      return;
    }
    const redirecting = await startFixture(tls, {
      password: "fixture-password",
      redirectHealth: true,
    });
    t.after(async () => redirecting.close());
    await expectFailure(
      cli.validateHttpsDeployment({
        origin: redirecting.origin,
        username: "owner",
        password: redirecting.password,
      }, fixtureOptions(redirecting)),
      /redirects are not followed/,
      "health-redirect-canary",
    );

    const unhealthy = await startFixture(tls, {
      password: "fixture-password",
      healthStatus: 500,
      healthBody: "health-body-canary",
    });
    t.after(async () => unhealthy.close());
    await expectFailure(
      cli.validateHttpsDeployment({
        origin: unhealthy.origin,
        username: "owner",
        password: unhealthy.password,
      }, fixtureOptions(unhealthy)),
      /returned status 500/,
      "health-body-canary",
    );
  });

  it("bounds a stalled request and cleans it up", async (t) => {
    const tls = getTlsFixture();
    if (tls === null) {
      t.skip("openssl is unavailable");
      return;
    }
    const server: Server = createServer({ key: tls.key, cert: tls.cert }, () => {
      // Intentionally leave the request unanswered.
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    t.after(async () => {
      const closable = server as unknown as { closeAllConnections?: () => void };
      closable.closeAllConnections?.();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    });
    const address = server.address();
    if (address === null || typeof address === "string") {
      throw new Error("fixture did not bind a TCP port");
    }
    const origin = `https://127.0.0.1:${address.port}`;
    const started = Date.now();
    await expectFailure(
      cli.validateHttpsDeployment({
        origin,
        username: "owner",
        password: "fixture-password",
      }, {
        trustedCa: tls.ca,
        totalTimeoutMs: 1_000,
        requestTimeoutMs: 120,
      }),
      /timed out/,
      "fixture-password",
    );
    assert.ok(Date.now() - started < 2_000, "the request deadline must be bounded");
  });
});
