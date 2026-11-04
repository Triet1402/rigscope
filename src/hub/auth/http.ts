/**
 * Browser authentication endpoints (milestone 3): /api/auth/login,
 * /api/auth/session, /api/auth/logout.
 *
 * Security properties:
 * - Login/logout mutations require an Origin header that exactly matches the
 *   hub identity. Loopback mode accepts scheme http on an allowlisted loopback
 *   hostname with the bound port. Remote mode accepts only the configured
 *   canonical HTTPS origin and exact Host. Forwarded headers are ignored
 *   entirely; they never establish scheme or host.
 * - Failure messages are generic ("invalid username or password"); an
 *   unknown username performs the same scrypt work as a known one.
 * - Requests are bounded: 5s deadline, 4 KiB JSON body, 4 concurrent
 *   verifications, and a global token-bucket login rate.
 * - Sessions are opaque random tokens. Local plaintext mode keeps the
 *   `rigscope_session` HttpOnly, SameSite=Strict, Path=/ cookie without Secure.
 *   Remote mode uses the separate `__Host-rigscope_session` cookie with Secure,
 *   HttpOnly, SameSite=Strict, Path=/, and no Domain — deletion carries the
 *   same attributes.
 * - Password hashes and passwords never appear in responses or logs.
 */

import type { IncomingMessage, ServerResponse } from "node:http";
import { performance } from "node:perf_hooks";
import { type BrowserUserConfig, MAX_BROWSER_USERS } from "./config";
import { MAX_BROWSER_PASSWORD_CHARS, verifyPasswordAsync } from "./password";
import {
  BrowserSessionStore,
  REMOTE_SESSION_COOKIE_NAME,
  SESSION_COOKIE_NAME,
  type BrowserSession,
} from "./sessions";
import { singleRequestHeader } from "../proxy/boundary";

export const AUTH_LOGIN_PATH = "/api/auth/login";
export const AUTH_SESSION_PATH = "/api/auth/session";
export const AUTH_LOGOUT_PATH = "/api/auth/logout";
export const AUTH_PATH_PREFIX = "/api/auth/";
export const MAX_AUTH_BODY_BYTES = 4096;
export const AUTH_REQUEST_TIMEOUT_MS = 5_000;
export const MAX_CONCURRENT_LOGINS = 4;
/** Token bucket: 10-attempt burst, one recovered attempt per 500ms. */
const LOGIN_BURST = 10;
const LOGIN_REFILL_MS = 500;
const MAX_USERNAME_CHARS = 128;

const LOOPBACK_HOSTNAME_PATTERN = /^localhost$|^127(?:\.\d{1,3}){3}$|^\[::1\]$/;

/**
 * Same-origin policy for browser mutations, upgrades, and SSE.
 *
 * - `loopback`: plaintext local mode; current behavior is unchanged.
 * - `remote`: the exact canonical HTTPS origin and exact Host header that the
 *   private Unix socket serves. There is no inferred scheme/host and no
 *   forwarded-header trust.
 */
export type AuthOriginRules =
  | {
      mode?: "loopback";
      /** Exact validated origin — browser-visible port for a same-origin POST. */
      originPort: number;
      /** Loopback hostnames (bind host, localhost, loopback literals) a same-origin POST may use. */
      originHostnames: readonly string[];
    }
  | {
      mode: "remote";
      publicOrigin: string;
      hostHeader: string;
    };

/**
 * Login/logout rate limiter and handlers. `sessions` and `users` are provided
 * by the composition root (src/hub/main.ts); unit tests may drive the class
 * directly against a local http.Server.
 */
export class BrowserHttpAuth {
  private users = new Map<string, BrowserUserConfig>();
  private readonly sessions: BrowserSessionStore;
  private readonly origin: AuthOriginRules;
  private loginTokens = LOGIN_BURST;
  private lastRefill = performance.now();
  private activeLogins = 0;

  constructor(input: {
    users: readonly BrowserUserConfig[];
    sessions: BrowserSessionStore;
    origin: AuthOriginRules;
  }) {
    this.sessions = input.sessions;
    this.origin = input.origin;
    this.replaceUsers(input.users);
  }

  /**
   * Atomically replaces the in-memory directory after a durable admin change.
   * Existing sessions are not mutated here: the composition root decides when
   * to revoke them (BrowserSessionStore.endUser) from its change callback.
   */
  replaceUsers(users: readonly BrowserUserConfig[]): void {
    if (!Array.isArray(users) || users.length > MAX_BROWSER_USERS) {
      throw new Error(`browser user directory must contain at most ${MAX_BROWSER_USERS} entries`);
    }
    const next = new Map<string, BrowserUserConfig>();
    for (const user of users) {
      if (user === null || typeof user !== "object") throw new Error("invalid browser user entry");
      if (typeof user.username !== "string" || user.username.length === 0) {
        throw new Error("browser user requires a username");
      }
      if (user.machineIds !== "*" && !Array.isArray(user.machineIds)) {
        throw new Error("browser user requires a machine scope");
      }
      if (next.has(user.username)) throw new Error("duplicate browser username");
      next.set(user.username, {
        username: user.username,
        passwordHash: user.passwordHash,
        machineIds: user.machineIds === "*" ? "*" : [...user.machineIds],
        ...(user.admin === true ? { admin: true } : {}),
      });
    }
    this.users = next;
  }

  /** True when browser users are configured; controls fail-closed behavior. */
  get configured(): boolean {
    return this.users.size > 0;
  }

  /** Handles every /api/auth/* request; returns false for other paths. */
  async handleApiAuth(req: IncomingMessage, res: ServerResponse, pathname: string): Promise<boolean> {
    if (!pathname.startsWith(AUTH_PATH_PREFIX)) return false;
    if (pathname === AUTH_LOGIN_PATH) {
      await this.handleLogin(req, res);
      return true;
    }
    if (pathname === AUTH_SESSION_PATH) {
      this.handleSession(req, res);
      return true;
    }
    if (pathname === AUTH_LOGOUT_PATH) {
      await this.handleLogout(req, res);
      return true;
    }
    sendJson(res, 404, { error: "unknown auth endpoint" });
    return true;
  }

  /**
   * Resolves the current session from the cookie header only (no CSRF).
   * Used by the composition root to gate /api/live and delegated UI routes
   * before any headers or Next.js processing happen.
   */
  requestSession(req: IncomingMessage): BrowserSession | null {
    const cookies = parseCookieHeader(req.headers.cookie);
    const token = cookies[this.sessionCookieName];
    if (typeof token !== "string") return null;
    return this.sessions.find(token);
  }

  /** Remote mode uses the host-only `__Host-` cookie; local keeps `rigscope_session`. */
  private get sessionCookieName(): string {
    return this.origin.mode === "remote" ? REMOTE_SESSION_COOKIE_NAME : SESSION_COOKIE_NAME;
  }

  private get secureCookies(): boolean {
    return this.origin.mode === "remote";
  }

  // -- login ------------------------------------------------------------

  private async handleLogin(req: IncomingMessage, res: ServerResponse): Promise<void> {
    if (req.method !== "POST") {
      sendJson(res, 405, { error: "POST required" }, { Allow: "POST" });
      return;
    }
    if (!this.configured) {
      sendJson(res, 503, { error: "browser access is not configured on this hub" });
      return;
    }
    if (!isTrustedOrigin(req, this.origin)) {
      sendJson(res, 403, { error: "origin not allowed" });
      return;
    }
    const contentType = req.headers["content-type"] ?? "";
    if (!contentType.startsWith("application/json")) {
      sendJson(res, 415, { error: "JSON required" });
      return;
    }
    if (!this.consumeLoginBudget()) {
      sendJson(res, 429, { error: "too many login attempts; wait briefly and retry" });
      return;
    }
    if (this.activeLogins >= MAX_CONCURRENT_LOGINS) {
      sendJson(res, 429, { error: "too many login attempts; wait briefly and retry" });
      return;
    }
    this.activeLogins += 1;
    try {
      const body = await readJsonBody(req);
      if (!body.ok) {
        sendJson(res, body.status, { error: body.error }, { Connection: "close" });
        return;
      }
      let username: unknown;
      let password: unknown;
      let shaped = true;
      if (body.value === null || typeof body.value !== "object" || Array.isArray(body.value)) {
        shaped = false;
      } else {
        const record = body.value as Record<string, unknown>;
        username = record.username;
        password = record.password;
        shaped = objectKeys(record) === "password,username";
      }
      if (!shaped || typeof username !== "string" || username.length > MAX_USERNAME_CHARS
        || typeof password !== "string" || password.length === 0
        || password.length > MAX_BROWSER_PASSWORD_CHARS) {
        sendJson(res, 400, { error: "invalid login request" });
        return;
      }
      const candidate = this.users.get(username);
      // All users use identical work parameters. Reusing a configured hash
      // for unknown names costs one verification without a first-use spike.
      const fallback = this.users.values().next().value as BrowserUserConfig | undefined;
      const hash = candidate?.passwordHash ?? fallback?.passwordHash;
      if (hash === undefined) {
        // The directory was emptied while this request was in flight.
        sendJson(res, 401, { error: "invalid username or password" });
        return;
      }
      const valid = await verifyPasswordAsync(password, hash);

      // Re-read AFTER the async scrypt work. A concurrent disable, delete,
      // password, scope or admin change must invalidate this attempt instead
      // of minting a session from the stale snapshot.
      const current = this.users.get(username);
      if (!candidate || !valid || !current || !sameUserVersion(candidate, current)) {
        sendJson(res, 401, { error: "invalid username or password" });
        return;
      }
      const session = this.sessions.create(username, current.machineIds, current.admin === true);
      const maxAgeSeconds = Math.floor(this.sessions.ttlMs() / 1000);
      res.setHeader("Set-Cookie", sessionCookie(session.id, maxAgeSeconds, this.secureCookies));
      sendJson(res, 200, sessionPayload(session));
    } finally {
      this.activeLogins -= 1;
    }
  }

  // -- session ----------------------------------------------------------

  private handleSession(req: IncomingMessage, res: ServerResponse): void {
    if (req.method !== "GET" && req.method !== "HEAD") {
      sendJson(res, 405, { error: "GET required" }, { Allow: "GET, HEAD" });
      return;
    }
    if (!this.configured) {
      sendJson(res, 503, { error: "browser access is not configured on this hub" });
      return;
    }
    const session = this.requestSession(req);
    if (!session) {
      sendJson(res, 401, { error: "authentication required" });
      return;
    }
    sendJson(res, 200, sessionPayload(session));
  }

  // -- logout -----------------------------------------------------------

  private async handleLogout(req: IncomingMessage, res: ServerResponse): Promise<void> {
    if (req.method !== "POST") {
      sendJson(res, 405, { error: "POST required" }, { Allow: "POST" });
      return;
    }
    if (!this.configured) {
      sendJson(res, 503, { error: "browser access is not configured on this hub" });
      return;
    }
    if (!isTrustedOrigin(req, this.origin)) {
      sendJson(res, 403, { error: "origin not allowed" });
      return;
    }
    const cookies = parseCookieHeader(req.headers.cookie);
    const token = cookies[this.sessionCookieName];
    const session = this.sessions.find(typeof token === "string" ? token : "");
    if (typeof token !== "string" || !session) {
      res.setHeader("Set-Cookie", clearedSessionCookie(this.secureCookies));
      sendJson(res, 401, { error: "authentication required" });
      return;
    }
    const csrfHeader = req.headers["x-csrf-token"];
    if (typeof csrfHeader !== "string" || csrfHeader.length !== session.csrfToken.length
      || !constantTimeEquals(csrfHeader, session.csrfToken)) {
      sendJson(res, 403, { error: "missing or invalid csrf token" });
      return;
    }
    this.sessions.end(token, "logout");
    res.setHeader("Set-Cookie", clearedSessionCookie(this.secureCookies));
    sendJson(res, 200, { ok: true });
  }

  /** Global rate budget for login attempts on this hub process. */
  private consumeLoginBudget(): boolean {
    const now = performance.now();
    this.loginTokens = Math.min(
      LOGIN_BURST,
      this.loginTokens + Math.max(0, now - this.lastRefill) / LOGIN_REFILL_MS,
    );
    this.lastRefill = now;
    if (this.loginTokens < 1) return false;
    this.loginTokens -= 1;
    return true;
  }

}

// -- shared cookie/header helpers -----------------------------------------

/** Parses a Cookie header into name/value pairs (first value wins). */
export function parseCookieHeader(header: string | undefined): Record<string, string> {
  const cookies: Record<string, string> = {};
  if (typeof header !== "string" || header.length === 0) return cookies;
  for (const part of header.split(";")) {
    const index = part.indexOf("=");
    if (index < 0) continue;
    const name = part.slice(0, index).trim();
    const value = part.slice(index + 1).trim();
    if (name.length === 0) continue;
    if (!(name in cookies)) cookies[name] = value;
  }
  return cookies;
}

export function sessionCookie(
  token: string,
  maxAgeSeconds: number,
  secure = false,
): string {
  const name = secure ? REMOTE_SESSION_COOKIE_NAME : SESSION_COOKIE_NAME;
  const secureAttribute = secure ? "Secure; " : "";
  return `${name}=${token}; ${secureAttribute}HttpOnly; SameSite=Strict; Path=/; Max-Age=${maxAgeSeconds}`;
}

export function clearedSessionCookie(secure = false): string {
  const name = secure ? REMOTE_SESSION_COOKIE_NAME : SESSION_COOKIE_NAME;
  const secureAttribute = secure ? "Secure; " : "";
  return `${name}=; ${secureAttribute}HttpOnly; SameSite=Strict; Path=/; Max-Age=0`;
}

/**
 * Exact same-origin validation for mutations and upgrades. `origin` is the
 * ONLY trusted origin signal and must parse into the configured identity:
 *
 * - loopback: http, an allowlisted loopback hostname, the hub's actual bound
 *   port, and equality with the request's validated Host header (defends
 *   DNS-rebinding);
 * - remote: byte-for-byte equality with the configured canonical HTTPS origin
 *   and exact Host header.
 *
 * X-Forwarded-* / Forwarded are never consulted; the hub establishes scheme
 * and host from static configuration and its private transport, not headers.
 */
export function isTrustedOrigin(req: IncomingMessage, rules: AuthOriginRules): boolean {
  const originHeader = singleRequestHeader(req, "origin");
  const hostHeader = singleRequestHeader(req, "host");
  if (originHeader.kind !== "single" || hostHeader.kind !== "single") return false;
  const origin = originHeader.value;
  const host = hostHeader.value;
  if (origin.length === 0 || origin.length > 256) return false;
  if (host.length === 0 || host.length > 255) return false;

  if (rules.mode === "remote") {
    return origin === rules.publicOrigin && host === rules.hostHeader;
  }

  let parsed: URL;
  try {
    parsed = new URL(origin);
  } catch {
    return false;
  }
  if (parsed.protocol !== "http:") return false;
  // Origins are scheme://host:port only; anything with a path/query is not an origin.
  if (parsed.pathname !== "/" || parsed.search !== "" || parsed.hash !== "" || parsed.username !== "" || parsed.password !== "") {
    return false;
  }
  if (!rules.originHostnames.includes(parsed.hostname)) return false;
  if (parsed.port !== "") {
    if (parsed.port !== String(rules.originPort)) return false;
  } else if (rules.originPort !== 80) {
    return false;
  }
  // The validated Host header must name exactly the same host and port.
  return parsed.host.toLowerCase() === host.toLowerCase();
}

function constantTimeEquals(a: string, b: string): boolean {
  let diff = 0;
  for (let i = 0; i < a.length; i += 1) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

function objectKeys(record: Record<string, unknown>): string {
  return Object.keys(record).sort().join(",");
}

/** Legacy response shape plus an optional admin flag for explicit admins. */
function sessionPayload(session: BrowserSession): {
  username: string;
  csrfToken: string;
  admin?: boolean;
} {
  return {
    username: session.username,
    csrfToken: session.csrfToken,
    ...(session.admin === true ? { admin: true } : {}),
  };
}

function sameUserVersion(a: BrowserUserConfig, b: BrowserUserConfig): boolean {
  return (
    a.passwordHash === b.passwordHash &&
    (a.admin === true) === (b.admin === true) &&
    sameScope(a.machineIds, b.machineIds)
  );
}

function sameScope(a: BrowserUserConfig["machineIds"], b: BrowserUserConfig["machineIds"]): boolean {
  if (a === "*" || b === "*") return a === b;
  if (a.length !== b.length) return false;
  for (let index = 0; index < a.length; index += 1) {
    if (a[index] !== b[index]) return false;
  }
  return true;
}

export type JsonBodyResult =
  | { ok: true; value: unknown }
  | { ok: false; status: 400 | 413; error: string };

/** Bounded JSON body read: size cap plus an absolute request deadline. */
export function readJsonBody(req: IncomingMessage, maxBytes = MAX_AUTH_BODY_BYTES): Promise<JsonBodyResult> {
  return new Promise((resolve) => {
    const chunks: Buffer[] = [];
    let bytes = 0;
    let settled = false;
    const finish = (result: JsonBodyResult) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      resolve(result);
    };
    const timer = setTimeout(() => {
      req.destroy();
      finish({ ok: false, status: 400, error: "request timeout" });
    }, AUTH_REQUEST_TIMEOUT_MS);
    req.on("data", (chunk: Buffer) => {
      bytes += chunk.length;
      if (bytes > maxBytes) {
        // Stop reading (TCP backpressure holds the client) and resolve;
        // the response 413 still needs this socket writable.
        req.pause();
        finish({ ok: false, status: 413, error: "body too large" });
        clearTimeout(timer);
        return;
      }
      chunks.push(chunk);
    });
    req.on("end", () => {
      if (!settled) {
        try {
          finish({ ok: true, value: JSON.parse(Buffer.concat(chunks).toString("utf8")) });
        } catch {
          finish({ ok: false, status: 400, error: "invalid JSON body" });
        }
      }
    });
    req.on("error", () => finish({ ok: false, status: 400, error: "request failed" }));
    req.on("close", () => {
      if (!settled && !req.readableEnded) {
        finish({ ok: false, status: 400, error: "request closed" });
      }
    });
  });
}

export function sendJson(
  res: ServerResponse,
  status: number,
  body: object,
  extraHeaders: Record<string, string> = {},
): void {
  res.writeHead(status, {
    "Content-Type": "application/json; charset=utf-8",
    "Cache-Control": "no-store",
    ...extraHeaders,
  });
  res.end(JSON.stringify(body));
}

/** Loopback hostname allowlist for same-origin checks (bind host incl.). */
export function loopbackOriginHostnames(bindHost: string): string[] {
  const hostnames = new Set<string>(["localhost"]);
  // Bind host must itself be loopback (validated in parseHubConfig).
  hostnames.add(bindHost);
  hostnames.add("127.0.0.1");
  hostnames.add("::1");
  hostnames.add("[::1]");
  if (LOOPBACK_HOSTNAME_PATTERN.test(bindHost)) hostnames.add(bindHost);
  return [...hostnames];
}
