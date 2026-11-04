#!/usr/bin/env node
/**
 * Actual-HTTPS deployment validation CLI for the private Unix-socket hub.
 *
 * This command deliberately exercises the public HTTPS route with Node's
 * ordinary `https` client and normal certificate verification. It is meant to
 * be run only after an operator has configured a real browser user and a real
 * collector; it never starts, stops, or reconfigures the hub or Serve.
 *
 * Usage (all configuration is environment-based; there is no password argv or
 * environment-value path):
 *
 *   RIGSCOPE_VALIDATE_ORIGIN=https://host.tailnet.example:8443 \
 *   RIGSCOPE_VALIDATE_USERNAME=owner \
 *   RIGSCOPE_VALIDATE_PASSWORD_FILE=/private/path/browser-password \
 *   [RIGSCOPE_VALIDATE_MACHINE_ID=optional-machine-id] \
 *   node scripts/validate-https.cjs
 *
 * The password file must be a regular file owned by the current user, must not
 * be a symlink, and must have mode 0600 or 0400. Passwords, cookies, CSRF
 * tokens, response bodies, and hashes are never printed, including on failure.
 *
 * This is not a deployment switch. A pass only means the automated HTTPS probes
 * below worked. Browser cookie acceptance, actual fixed session expiry, a real
 * WSS collector, HMR edits, and shutdown still require separate checks.
 */

"use strict";

const fs = require("node:fs");
const fsp = require("node:fs/promises");
const https = require("node:https");
const path = require("node:path");
const crypto = require("node:crypto");

const DEFAULT_USERNAME = "owner";
const SESSION_COOKIE_NAME = "__Host-rigscope_session";
const SESSION_MAX_AGE_SECONDS = 12 * 60 * 60;
const MAX_ORIGIN_LENGTH = 512;
const MAX_USERNAME_CHARS = 128;
const MAX_PASSWORD_CHARS = 1024;
const MAX_PASSWORD_FILE_BYTES = 4096;
const MAX_RESPONSE_BODY_BYTES = 64 * 1024;
const MAX_SSE_QUEUE_ITEMS = 1_024;
const MAX_SSE_PARSER_BUFFER_CHARS = 1_048_576;
const MAX_SSE_QUEUE_CHARS = 4 * MAX_SSE_PARSER_BUFFER_CHARS;
const ID_PATTERN = /^[\x21-\x7e]{1,128}$/;

const DEFAULT_TOTAL_TIMEOUT_MS = 120_000;
const DEFAULT_REQUEST_TIMEOUT_MS = 10_000;
const DEFAULT_INITIAL_EVENT_MS = 10_000;
const DEFAULT_SAMPLE_WAIT_MS = 30_000;
const DEFAULT_LOGOUT_CLOSE_MS = 5_000;
const DEFAULT_KEEPALIVE = Object.freeze({
  expectedMs: 15_000,
  minMs: 13_000,
  maxMs: 20_000,
});
const SEPARATE_CHECKS = "Browser cookie acceptance, actual fixed session expiry, a real WSS collector, HMR edits, and shutdown require separate checks.";
const SUCCESS_SCOPE = `Scope: this command only covers automated HTTPS checks. ${SEPARATE_CHECKS} It does not enable or deploy anything, and a pass does not by itself validate the deployment. Collector real/mock mode was not determined, so the observed CPU/memory values are not proof of real host collection.`;
const FAILURE_SCOPE = `This command only covers automated HTTPS checks. ${SEPARATE_CHECKS} It does not enable or deploy anything.`;

class ValidationError extends Error {
  constructor(message) {
    super(message);
    this.name = "HttpsValidationError";
  }
}

/** Canonical HTTPS origin only: the same shape the hub accepts at startup. */
function parseCanonicalHttpsOrigin(value) {
  if (typeof value !== "string" || value.length === 0) {
    throw new ValidationError("RIGSCOPE_VALIDATE_ORIGIN must be a nonempty HTTPS origin");
  }
  if (value.length > MAX_ORIGIN_LENGTH) {
    throw new ValidationError(`HTTPS origin must be at most ${MAX_ORIGIN_LENGTH} characters`);
  }
  if (!/^[\x21-\x7e]+$/.test(value)) {
    throw new ValidationError("HTTPS origin must be printable ASCII without spaces or control characters");
  }
  let parsed;
  try {
    parsed = new URL(value);
  } catch {
    throw new ValidationError("HTTPS origin is not a valid absolute origin");
  }
  if (parsed.protocol !== "https:") {
    throw new ValidationError("HTTPS origin must use https://");
  }
  if (parsed.username !== "" || parsed.password !== "") {
    throw new ValidationError("HTTPS origin must not contain userinfo");
  }
  if (parsed.pathname !== "/" || parsed.search !== "" || parsed.hash !== "") {
    throw new ValidationError("HTTPS origin must be exactly scheme://host[:port] with no path, query, or fragment");
  }
  // URL.origin is canonical: lowercase host, no trailing slash, default port
  // elided. Requiring exact equality rejects aliases that are ambiguous against
  // the exact Host header the hub enforces.
  if (parsed.origin !== value) {
    throw new ValidationError(`HTTPS origin must be canonical; use "${parsed.origin}"`);
  }
  if (parsed.hostname.length === 0 || parsed.hostname.endsWith(".")) {
    throw new ValidationError("HTTPS origin must name a nonempty host without a trailing dot");
  }
  if (parsed.host.length > 255) {
    throw new ValidationError("HTTPS origin host is too long");
  }
  if (parsed.port === "0") {
    throw new ValidationError("HTTPS origin must use a nonzero public port");
  }
  const hostname = parsed.hostname.startsWith("[") && parsed.hostname.endsWith("]")
    ? parsed.hostname.slice(1, -1)
    : parsed.hostname;
  return {
    origin: value,
    host: parsed.host,
    hostname,
    port: parsed.port === "" ? 443 : Number(parsed.port),
  };
}

function parseValidationUsername(value) {
  const username = value === undefined || value === "" ? DEFAULT_USERNAME : value;
  if (typeof username !== "string" || !ID_PATTERN.test(username)) {
    throw new ValidationError("RIGSCOPE_VALIDATE_USERNAME must be 1..128 printable ASCII characters without spaces");
  }
  return username;
}

function parseMachineId(value) {
  if (typeof value !== "string" || !ID_PATTERN.test(value)) {
    throw new ValidationError("RIGSCOPE_VALIDATE_MACHINE_ID must be 1..128 printable ASCII characters without spaces");
  }
  return value;
}

/**
 * Parses environment-only CLI configuration. The password itself is never
 * accepted from the environment; only an absolute private file path is.
 */
function resolveValidationConfig(env) {
  if (env === null || typeof env !== "object") {
    throw new ValidationError("validation configuration is unavailable");
  }
  if (env.RIGSCOPE_VALIDATE_PASSWORD !== undefined) {
    throw new ValidationError(
      "plaintext passwords are not accepted in environment variables; use RIGSCOPE_VALIDATE_PASSWORD_FILE",
    );
  }
  const origin = parseCanonicalHttpsOrigin(env.RIGSCOPE_VALIDATE_ORIGIN);
  const username = parseValidationUsername(env.RIGSCOPE_VALIDATE_USERNAME);

  const passwordFile = env.RIGSCOPE_VALIDATE_PASSWORD_FILE;
  if (typeof passwordFile !== "string" || passwordFile.length === 0) {
    throw new ValidationError("RIGSCOPE_VALIDATE_PASSWORD_FILE must name an absolute private password file");
  }
  if (passwordFile.length > 4096 || /[\u0000-\u001f\u007f]/.test(passwordFile)) {
    throw new ValidationError("RIGSCOPE_VALIDATE_PASSWORD_FILE must be a bounded path without control characters");
  }
  if (!path.isAbsolute(passwordFile) || passwordFile !== path.resolve(passwordFile)) {
    throw new ValidationError("RIGSCOPE_VALIDATE_PASSWORD_FILE must be a normalized absolute path");
  }

  const machineValue = env.RIGSCOPE_VALIDATE_MACHINE_ID;
  const machineId = machineValue === undefined || machineValue === ""
    ? undefined
    : parseMachineId(machineValue);

  return {
    origin: origin.origin,
    originInfo: origin,
    username,
    passwordFile,
    machineId,
  };
}

/**
 * Reads a password from a private file. The file is opened with O_NOFOLLOW and
 * checked through the open descriptor, so a symlink swap cannot later redirect
 * the read. Errors never include file contents or the file path.
 */
async function readPasswordFile(filePath) {
  if (typeof filePath !== "string" || !path.isAbsolute(filePath)
    || filePath !== path.resolve(filePath)) {
    throw new ValidationError("password file must be a normalized absolute path");
  }

  // lstat + O_NOFOLLOW: reject an existing symlink even on platforms where the
  // open flag is unavailable, and reject a swap between lstat and open.
  try {
    const linkStats = await fsp.lstat(filePath);
    if (linkStats.isSymbolicLink()) {
      throw new ValidationError("password file must not be a symbolic link");
    }
    if (!linkStats.isFile()) {
      throw new ValidationError("password file must be a regular file");
    }
  } catch (error) {
    if (error instanceof ValidationError) throw error;
    throw new ValidationError("password file must be an existing regular file");
  }

  let handle;
  try {
    const flags = fs.constants.O_RDONLY | (fs.constants.O_NOFOLLOW ?? 0) | (fs.constants.O_NONBLOCK ?? 0);
    handle = await fsp.open(filePath, flags);
  } catch (error) {
    const code = error && typeof error.code === "string" ? error.code : "";
    if (code === "ELOOP") {
      throw new ValidationError("password file must not be a symbolic link");
    }
    throw new ValidationError("password file could not be opened as a regular file");
  }

  try {
    const stats = await handle.stat();
    if (!stats.isFile()) {
      throw new ValidationError("password file must be a regular file");
    }
    const euid = typeof process.geteuid === "function" ? process.geteuid() : undefined;
    if (euid === undefined || stats.uid !== euid) {
      throw new ValidationError("password file must be owned by the current user");
    }
    const mode = stats.mode & 0o7777;
    if (mode !== 0o600 && mode !== 0o400) {
      throw new ValidationError("password file mode must be 0600 or 0400");
    }
    if (stats.size <= 0) {
      throw new ValidationError("password file must not be empty");
    }
    if (stats.size > MAX_PASSWORD_FILE_BYTES) {
      throw new ValidationError(`password file must be at most ${MAX_PASSWORD_FILE_BYTES} bytes`);
    }

    const buffer = Buffer.alloc(MAX_PASSWORD_FILE_BYTES + 1);
    const { bytesRead } = await handle.read(buffer, 0, buffer.length, 0);
    const raw = buffer.subarray(0, bytesRead);
    if (raw.length === 0) {
      throw new ValidationError("password file must not be empty");
    }
    if (raw.length > MAX_PASSWORD_FILE_BYTES) {
      throw new ValidationError(`password file must be at most ${MAX_PASSWORD_FILE_BYTES} bytes`);
    }

    let text;
    try {
      text = new TextDecoder("utf-8", { fatal: true }).decode(raw);
    } catch {
      throw new ValidationError("password file must contain valid UTF-8 text");
    }
    // Trailing newlines are the common secret-file convention; do not silently
    // trim other whitespace or content.
    const password = text.replace(/(?:\r?\n)+$/, "");
    if (password.length === 0) {
      throw new ValidationError("password file must not contain an empty password");
    }
    if (password.length > MAX_PASSWORD_CHARS) {
      throw new ValidationError(`password must be at most ${MAX_PASSWORD_CHARS} characters`);
    }
    return password;
  } catch (error) {
    if (error instanceof ValidationError) throw error;
    throw new ValidationError("password file could not be read safely");
  } finally {
    await handle.close().catch(() => undefined);
  }
}

function constantTimeEqual(a, b) {
  const left = Buffer.from(a, "utf8");
  const right = Buffer.from(b, "utf8");
  if (left.length !== right.length) return false;
  return crypto.timingSafeEqual(left, right);
}

/**
 * An invalid-password probe must not accidentally equal the real password.
 * Cryptographic randomness makes this a formality, but the explicit comparison
 * keeps the security property independent of luck.
 */
function randomPasswordDifferentFrom(actualPassword) {
  for (let attempt = 0; attempt < 8; attempt += 1) {
    const candidate = crypto.randomBytes(32).toString("base64url");
    if (!constantTimeEqual(candidate, actualPassword.normalize("NFKC"))) return candidate;
  }
  throw new ValidationError("could not generate an independent invalid-password probe");
}

function parseJsonText(text, label) {
  let value;
  try {
    value = JSON.parse(text);
  } catch {
    throw new ValidationError(`${label} response was not valid JSON`);
  }
  return value;
}

function headerText(headers, name) {
  const value = headers[name.toLowerCase()];
  if (Array.isArray(value)) return value.join(", ");
  return typeof value === "string" ? value : "";
}

function hasSetCookie(headers) {
  const value = headers["set-cookie"];
  if (Array.isArray(value)) return value.length > 0;
  return typeof value === "string" && value.length > 0;
}

function singleSetCookie(headers, label) {
  const value = headers["set-cookie"];
  if (Array.isArray(value)) {
    if (value.length !== 1) {
      throw new ValidationError(`${label} must set exactly one session cookie`);
    }
    return value[0];
  }
  if (typeof value === "string" && value.length > 0) return value;
  throw new ValidationError(`${label} did not set a session cookie`);
}

function parseSetCookie(header, label) {
  if (typeof header !== "string" || header.length === 0) {
    throw new ValidationError(`${label} set-cookie header is malformed`);
  }
  const segments = header.split(";").map((segment) => segment.trim());
  const first = segments.shift();
  if (typeof first !== "string" || first.length === 0) {
    throw new ValidationError(`${label} set-cookie header is malformed`);
  }
  const equals = first.indexOf("=");
  if (equals <= 0) {
    throw new ValidationError(`${label} set-cookie header is malformed`);
  }
  const name = first.slice(0, equals);
  const value = first.slice(equals + 1);
  const attributes = new Map();
  for (const segment of segments) {
    if (segment.length === 0) continue;
    const index = segment.indexOf("=");
    const attribute = (index < 0 ? segment : segment.slice(0, index)).toLowerCase();
    const attributeValue = index < 0 ? "" : segment.slice(index + 1);
    if (attribute.length === 0 || attributes.has(attribute)) {
      throw new ValidationError(`${label} session cookie has duplicate or malformed attributes`);
    }
    attributes.set(attribute, attributeValue);
  }
  return { name, value, attributes };
}

function requireCookieFlag(attributes, name, label) {
  if (!attributes.has(name) || attributes.get(name) !== "") {
    throw new ValidationError(`${label} session cookie must carry ${name}`);
  }
}

function requireNoDomain(attributes, label) {
  if (attributes.has("domain")) {
    throw new ValidationError(`${label} session cookie must not carry Domain`);
  }
}

function requireCommonSecureAttributes(attributes, label) {
  requireCookieFlag(attributes, "secure", label);
  requireCookieFlag(attributes, "httponly", label);
  requireNoDomain(attributes, label);
  if (attributes.get("samesite") !== "Strict") {
    throw new ValidationError(`${label} session cookie must carry SameSite=Strict`);
  }
  if (attributes.get("path") !== "/") {
    throw new ValidationError(`${label} session cookie must carry Path=/`);
  }
}

/** Validates the exact remote-session cookie and returns its opaque value. */
function assertSecureSessionCookie(headers, label) {
  const cookie = parseSetCookie(singleSetCookie(headers, label), label);
  if (cookie.name !== SESSION_COOKIE_NAME) {
    throw new ValidationError(`${label} session cookie must be named ${SESSION_COOKIE_NAME}`);
  }
  if (!/^[A-Za-z0-9_-]{32,}$/.test(cookie.value)) {
    throw new ValidationError(`${label} session cookie value is not an opaque token`);
  }
  requireCommonSecureAttributes(cookie.attributes, label);
  if (cookie.attributes.get("max-age") !== String(SESSION_MAX_AGE_SECONDS)) {
    throw new ValidationError(
      `${label} session cookie must carry the fixed ${SESSION_MAX_AGE_SECONDS}-second Max-Age; remove HUB_DEV_SESSION_TTL_MS before normal validation`,
    );
  }
  return cookie.value;
}

/** Validates the logout deletion cookie's matching attributes. */
function assertClearedSessionCookie(headers, label) {
  const cookie = parseSetCookie(singleSetCookie(headers, label), label);
  if (cookie.name !== SESSION_COOKIE_NAME) {
    throw new ValidationError(`${label} session cookie must be named ${SESSION_COOKIE_NAME}`);
  }
  if (cookie.value !== "") {
    throw new ValidationError(`${label} logout cookie must have an empty value`);
  }
  requireCommonSecureAttributes(cookie.attributes, label);
  if (cookie.attributes.get("max-age") !== "0") {
    throw new ValidationError(`${label} logout cookie must carry Max-Age=0`);
  }
}

function mapRequestError(error, label) {
  if (error instanceof ValidationError) return error;
  const code = error && typeof error.code === "string" && /^[A-Z0-9_]+$/.test(error.code)
    ? error.code
    : null;
  if (code !== null && (
    code.startsWith("CERT_")
    || code.startsWith("ERR_TLS")
    || code === "DEPTH_ZERO_SELF_SIGNED_CERT"
    || code === "SELF_SIGNED_CERT_IN_CHAIN"
    || code === "CERT_UNTRUSTED"
    || code === "UNABLE_TO_GET_ISSUER_CERT"
    || code === "UNABLE_TO_GET_ISSUER_CERT_LOCALLY"
    || code === "UNABLE_TO_VERIFY_LEAF_SIGNATURE"
    || code === "ERR_TLS_CERT_ALTNAME_INVALID"
  )) {
    return new ValidationError(`${label} failed TLS certificate verification (${code})`);
  }
  if (code === "ENOTFOUND" || code === "EAI_AGAIN" || code === "ECONNREFUSED"
    || code === "ECONNRESET" || code === "ETIMEDOUT" || code === "EPROTO") {
    return new ValidationError(`${label} connection failed (${code})`);
  }
  return new ValidationError(`${label} request failed`);
}

function positiveNumber(value, fallback, label) {
  if (value === undefined) return fallback;
  if (!Number.isFinite(value) || value <= 0) {
    throw new ValidationError(`${label} must be a positive number of milliseconds`);
  }
  return value;
}

function normalizeKeepalive(value) {
  const input = value === null || typeof value !== "object" ? {} : value;
  const expectedMs = positiveNumber(input.expectedMs, DEFAULT_KEEPALIVE.expectedMs, "SSE keepalive expectation");
  const minMs = positiveNumber(input.minMs, DEFAULT_KEEPALIVE.minMs, "SSE keepalive minimum");
  const maxMs = positiveNumber(input.maxMs, DEFAULT_KEEPALIVE.maxMs, "SSE keepalive maximum");
  if (!(minMs <= expectedMs && expectedMs <= maxMs)) {
    throw new ValidationError("SSE keepalive bounds must satisfy min <= expected <= max");
  }
  return { expectedMs, minMs, maxMs };
}

function createContext(options) {
  const trustedCa = options.trustedCa === undefined || options.trustedCa === null
    ? undefined
    : options.trustedCa;
  // trustedCa is an additional local test trust anchor. It never disables
  // verification: every request below sets rejectUnauthorized: true explicitly
  // and accepts no caller-provided rejectUnauthorized option.
  const keepalive = normalizeKeepalive(options.sseKeepalive);
  return {
    origin: options.originInfo,
    trustedCa,
    deadline: Date.now() + positiveNumber(options.totalTimeoutMs, DEFAULT_TOTAL_TIMEOUT_MS, "total timeout"),
    requestTimeoutMs: positiveNumber(options.requestTimeoutMs, DEFAULT_REQUEST_TIMEOUT_MS, "request timeout"),
    initialEventMs: positiveNumber(options.initialEventMs, DEFAULT_INITIAL_EVENT_MS, "initial SSE event wait"),
    sampleWaitMs: positiveNumber(options.sampleWaitMs, DEFAULT_SAMPLE_WAIT_MS, "SSE sample wait"),
    logoutCloseMs: positiveNumber(options.logoutCloseMs, DEFAULT_LOGOUT_CLOSE_MS, "logout stream-close wait"),
    keepalive,
    keepaliveWaitMs: positiveNumber(
      options.keepaliveWaitMs,
      keepalive.maxMs + 2_000,
      "SSE keepalive wait",
    ),
    active: new Set(),
    streams: new Set(),
  };
}

function remaining(ctx, cap) {
  const left = ctx.deadline - Date.now();
  if (left <= 0) {
    throw new ValidationError("validation deadline exceeded");
  }
  return Math.max(1, Math.min(cap, left));
}

function requestOptions(ctx, input, headers) {
  return {
    hostname: ctx.origin.hostname,
    port: ctx.origin.port,
    path: input.path,
    method: input.method === undefined ? "GET" : input.method,
    headers,
    rejectUnauthorized: true,
    ...(ctx.trustedCa === undefined ? {} : { ca: ctx.trustedCa }),
    // A per-request agent makes cleanup deterministic; no keep-alive socket is
    // left behind to keep the CLI process alive after success or failure.
    agent: false,
  };
}

/**
 * Bounded HTTPS request. Redirects fail closed (they are never followed), the
 * response body is capped, and errors expose only the path label plus a status
 * or safe error code. Response bodies are never printed or included in errors.
 */
function requestHttps(ctx, input) {
  const label = input.label === undefined ? `request ${input.path}` : input.label;
  let timeoutMs;
  try {
    timeoutMs = remaining(ctx, input.timeoutMs === undefined ? ctx.requestTimeoutMs : input.timeoutMs);
  } catch (error) {
    return Promise.reject(error);
  }

  const body = input.body === undefined ? null : Buffer.from(input.body, "utf8");
  const headers = {
    accept: "application/json",
    ...(input.headers === undefined ? {} : input.headers),
    // Keep the exact canonical Host even if a caller supplied something else.
    host: ctx.origin.host,
  };
  if (body !== null) headers["content-length"] = String(body.length);

  return new Promise((resolve, reject) => {
    let settled = false;
    let absolute = null;
    let request;
    const finish = (settle, value) => {
      if (settled) return;
      settled = true;
      if (absolute !== null) clearTimeout(absolute);
      if (request !== undefined) ctx.active.delete(request);
      settle(value);
    };

    request = https.request(requestOptions(ctx, input, headers), (response) => {
      if (settled) {
        response.destroy();
        return;
      }
      const socket = response.socket;
      if (!socket || socket.authorized !== true) {
        response.destroy();
        finish(reject, new ValidationError("TLS certificate verification did not succeed"));
        return;
      }
      const status = response.statusCode === undefined ? 0 : response.statusCode;
      if (status >= 300 && status < 400) {
        response.destroy();
        finish(reject, new ValidationError(
          `${label} returned a redirect (status ${status}); redirects are not followed`,
        ));
        return;
      }
      if (input.rejectEventStream === true
        && headerText(response.headers, "content-type").toLowerCase().includes("text/event-stream")) {
        response.destroy();
        finish(reject, new ValidationError(`${label} returned an event stream`));
        return;
      }
      const chunks = [];
      let bytes = 0;
      response.on("data", (chunk) => {
        if (settled) return;
        bytes += chunk.length;
        if (bytes > MAX_RESPONSE_BODY_BYTES) {
          response.destroy();
          finish(reject, new ValidationError(`${label} response body exceeded the bounded size`));
          return;
        }
        chunks.push(chunk);
      });
      response.on("end", () => {
        finish(resolve, {
          status,
          headers: response.headers,
          body: Buffer.concat(chunks).toString("utf8"),
        });
      });
      response.on("aborted", () => {
        finish(reject, new ValidationError(`${label} response was aborted`));
      });
      response.on("error", () => {
        finish(reject, new ValidationError(`${label} response failed`));
      });
    });

    ctx.active.add(request);
    absolute = setTimeout(() => {
      request.destroy();
      finish(reject, new ValidationError(`${label} timed out`));
    }, timeoutMs);
    request.on("timeout", () => {
      request.destroy();
      finish(reject, new ValidationError(`${label} timed out`));
    });
    request.on("error", (error) => {
      finish(reject, mapRequestError(error, label));
    });
    request.setTimeout(timeoutMs);
    request.end(body === null ? undefined : body);
  });
}

class SseParser {
  constructor(onItem) {
    this.onItem = onItem;
    this.buffer = "";
    this.eventName = "message";
    this.dataLines = [];
    this.pendingId = undefined;
    this.eventChars = 0;
  }

  push(chunk) {
    if (this.buffer.length + this.eventChars + chunk.length > MAX_SSE_PARSER_BUFFER_CHARS) return false;
    this.buffer += chunk;
    for (;;) {
      const index = this.buffer.indexOf("\n");
      if (index < 0) return true;
      let line = this.buffer.slice(0, index);
      this.buffer = this.buffer.slice(index + 1);
      if (line.endsWith("\r")) line = line.slice(0, -1);

      if (line.startsWith(":")) {
        this.onItem({ kind: "comment", value: line.slice(1).trim() });
        continue;
      }
      if (line === "") {
        if (this.dataLines.length > 0) {
          this.onItem({
            kind: "event",
            event: this.eventName,
            data: this.dataLines.join("\n"),
            id: this.pendingId,
          });
        }
        this.eventName = "message";
        this.dataLines = [];
        this.pendingId = undefined;
        this.eventChars = 0;
        continue;
      }
      this.eventChars += line.length + 1;
      if (this.eventChars > MAX_SSE_PARSER_BUFFER_CHARS) return false;
      const colon = line.indexOf(":");
      const field = colon < 0 ? line : line.slice(0, colon);
      let value = colon < 0 ? "" : line.slice(colon + 1);
      if (value.startsWith(" ")) value = value.slice(1);
      if (field === "event") this.eventName = value;
      else if (field === "data") this.dataLines.push(value);
      else if (field === "id") this.pendingId = value;
    }
  }
}

class SseStream {
  constructor(request, response, openedAt, onClose) {
    this.request = request;
    this.response = response;
    this.openedAt = openedAt;
    this.onClose = onClose;
    this.items = [];
    this.queuedChars = 0;
    this.waiters = [];
    this.closeWaiters = [];
    this.closed = false;
    this.closedAt = null;
    this.failure = null;
    this.parser = new SseParser((item) => this.receive(item));

    response.setEncoding("utf8");
    response.on("data", (chunk) => {
      if (this.closed) return;
      if (!this.parser.push(chunk)) {
        this.finish(new ValidationError("SSE stream exceeded the bounded parser buffer"));
        try {
          response.destroy();
        } catch {
          // already gone
        }
      }
    });
    response.on("end", () => this.finish(null));
    response.on("close", () => this.finish(null));
    response.on("error", () => this.finish(new ValidationError("SSE response failed")));
    request.on("error", () => this.finish(new ValidationError("SSE request failed")));
  }

  receive(item) {
    if (this.closed) return;
    const chars = (item.data?.length ?? 0) + (item.value?.length ?? 0)
      + (item.event?.length ?? 0) + (item.id?.length ?? 0);
    if (this.items.length >= MAX_SSE_QUEUE_ITEMS || this.queuedChars + chars > MAX_SSE_QUEUE_CHARS) {
      this.finish(new ValidationError("SSE stream exceeded the bounded frame queue"));
      try {
        this.response.destroy();
      } catch {
        // already gone
      }
      return;
    }
    item.at = Date.now();
    item.chars = chars;
    this.queuedChars += chars;
    this.items.push(item);
    this.pump();
  }

  pump() {
    for (let index = 0; index < this.waiters.length;) {
      const waiter = this.waiters[index];
      const itemIndex = this.items.findIndex(waiter.predicate);
      if (itemIndex < 0) {
        index += 1;
        continue;
      }
      const [item] = this.items.splice(itemIndex, 1);
      this.queuedChars -= item.chars;
      this.waiters.splice(index, 1);
      clearTimeout(waiter.timer);
      waiter.resolve(item);
    }
  }

  finish(failure) {
    if (this.closed) return;
    this.closed = true;
    this.closedAt = Date.now();
    this.failure = failure;
    this.pump();
    for (const waiter of this.waiters.splice(0)) {
      clearTimeout(waiter.timer);
      waiter.reject(failure === null
        ? new ValidationError(`${waiter.label}: SSE stream ended before the expected frame`)
        : failure);
    }
    for (const waiter of this.closeWaiters.splice(0)) {
      clearTimeout(waiter.timer);
      waiter.resolve();
    }
    if (typeof this.onClose === "function") this.onClose();
  }

  waitFor(predicate, timeoutMs, label) {
    return new Promise((resolve, reject) => {
      const itemIndex = this.items.findIndex(predicate);
      if (itemIndex >= 0) {
        const [item] = this.items.splice(itemIndex, 1);
        this.queuedChars -= item.chars;
        resolve(item);
        return;
      }
      if (this.failure !== null) {
        reject(this.failure);
        return;
      }
      if (this.closed) {
        reject(new ValidationError(`${label}: SSE stream ended before the expected frame`));
        return;
      }
      const waiter = { predicate, label, resolve, reject, timer: null };
      waiter.timer = setTimeout(() => {
        const index = this.waiters.indexOf(waiter);
        if (index >= 0) this.waiters.splice(index, 1);
        reject(new ValidationError(`${label}: timed out after ${timeoutMs}ms`));
      }, timeoutMs);
      this.waiters.push(waiter);
      this.pump();
    });
  }

  waitClosed(timeoutMs, label) {
    return new Promise((resolve, reject) => {
      if (this.closed) {
        resolve();
        return;
      }
      const waiter = { resolve, reject, timer: null };
      waiter.timer = setTimeout(() => {
        const index = this.closeWaiters.indexOf(waiter);
        if (index >= 0) this.closeWaiters.splice(index, 1);
        reject(new ValidationError(`${label}: stream did not close within ${timeoutMs}ms`));
      }, timeoutMs);
      this.closeWaiters.push(waiter);
    });
  }

  close() {
    if (this.closed) return;
    try {
      this.request.destroy();
    } catch {
      // already gone
    }
    try {
      this.response.destroy();
    } catch {
      // already gone
    }
    this.finish(new ValidationError("SSE stream closed locally during cleanup"));
  }
}

function startSse(ctx, input) {
  const label = input.label === undefined ? "SSE endpoint" : input.label;
  let timeoutMs;
  try {
    timeoutMs = remaining(ctx, ctx.requestTimeoutMs);
  } catch (error) {
    return Promise.reject(error);
  }

  const headers = {
    accept: "text/event-stream",
    ...(input.headers === undefined ? {} : input.headers),
    host: ctx.origin.host,
  };
  const requestInput = {
    ...input,
    path: input.path === undefined ? "/api/live" : input.path,
  };

  return new Promise((resolve, reject) => {
    let settled = false;
    let absolute = null;
    let request;
    const finish = (settle, value) => {
      if (settled) return;
      settled = true;
      if (absolute !== null) clearTimeout(absolute);
      if (request !== undefined) ctx.active.delete(request);
      settle(value);
    };

    request = https.request(requestOptions(ctx, requestInput, headers), (response) => {
      if (settled) {
        response.destroy();
        return;
      }
      const socket = response.socket;
      if (!socket || socket.authorized !== true) {
        response.destroy();
        finish(reject, new ValidationError("TLS certificate verification did not succeed"));
        return;
      }
      const status = response.statusCode === undefined ? 0 : response.statusCode;
      if (status >= 300 && status < 400) {
        response.destroy();
        finish(reject, new ValidationError(
          `${label} returned a redirect (status ${status}); redirects are not followed`,
        ));
        return;
      }
      if (status !== 200) {
        response.destroy();
        finish(reject, new ValidationError(`${label} was rejected with status ${status}; expected 200`));
        return;
      }
      if (!headerText(response.headers, "content-type").toLowerCase().startsWith("text/event-stream")) {
        response.destroy();
        finish(reject, new ValidationError(`${label} did not return an event stream`));
        return;
      }
      if (headerText(response.headers, "x-accel-buffering").toLowerCase() !== "no") {
        response.destroy();
        finish(reject, new ValidationError(`${label} did not disable proxy buffering`));
        return;
      }
      const cacheControl = headerText(response.headers, "cache-control").toLowerCase();
      if (!cacheControl.includes("no-cache") && !cacheControl.includes("no-store")) {
        response.destroy();
        finish(reject, new ValidationError(`${label} did not disable caching`));
        return;
      }

      // Headers arrived; the SSE stream is now bounded by each waitFor()
      // deadline and by the overall validation deadline.
      if (absolute !== null) {
        clearTimeout(absolute);
        absolute = null;
      }
      request.setTimeout(0);
      ctx.active.delete(request);
      settled = true;
      let stream;
      try {
        stream = new SseStream(request, response, Date.now(), () => {
          ctx.streams.delete(stream);
        });
      } catch {
        try {
          request.destroy();
        } catch {
          // already gone
        }
        reject(new ValidationError(`${label} stream setup failed`));
        return;
      }
      ctx.streams.add(stream);
      resolve(stream);
    });

    ctx.active.add(request);
    absolute = setTimeout(() => {
      request.destroy();
      finish(reject, new ValidationError(`${label} timed out waiting for response headers`));
    }, timeoutMs);
    request.on("timeout", () => {
      request.destroy();
      finish(reject, new ValidationError(`${label} timed out waiting for response headers`));
    });
    request.on("error", (error) => {
      finish(reject, mapRequestError(error, label));
    });
    request.setTimeout(timeoutMs);
    request.end();
  });
}

function parseSnapshotData(text, label) {
  const value = parseJsonText(text, label);
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new ValidationError(`${label} payload is not a JSON object`);
  }
  if (typeof value.hubRunId !== "string" || !ID_PATTERN.test(value.hubRunId)) {
    throw new ValidationError(`${label} payload has an invalid hubRunId`);
  }
  if (!Array.isArray(value.machines) || value.machines.length > 50) {
    throw new ValidationError(`${label} payload has an invalid machine list`);
  }
  for (const machine of value.machines) {
    if (machine === null || typeof machine !== "object" || Array.isArray(machine)
      || typeof machine.machineId !== "string" || !ID_PATTERN.test(machine.machineId)
      || typeof machine.connected !== "boolean") {
      throw new ValidationError(`${label} payload has an invalid machine row`);
    }
  }
  return value;
}

function findMachine(snapshot, machineId) {
  return snapshot.machines.find((machine) => machine.machineId === machineId) ?? null;
}

function chooseTargetMachine(snapshot, requestedMachineId) {
  if (requestedMachineId !== undefined && requestedMachineId !== null) {
    const machine = findMachine(snapshot, requestedMachineId);
    if (machine === null) {
      throw new ValidationError(
        "RIGSCOPE_VALIDATE_MACHINE_ID did not match a machine in the authenticated snapshot",
      );
    }
    if (machine.connected !== true) {
      throw new ValidationError("the configured RIGSCOPE_VALIDATE_MACHINE_ID machine is not connected");
    }
    return machine.machineId;
  }
  const connected = snapshot.machines.filter((machine) => machine.connected === true);
  if (connected.length === 0) {
    throw new ValidationError(
      "no connected machine appears in the authenticated snapshot; configure a collector and optionally RIGSCOPE_VALIDATE_MACHINE_ID",
    );
  }
  // Prefer a machine that already carries metrics; either way the sample
  // window below still requires an advancing pair of usable readings.
  const withMetrics = connected.find((machine) => machine.metrics !== undefined);
  return (withMetrics ?? connected[0]).machineId;
}

function assertEventId(id, hubRunId, label) {
  if (typeof id !== "string" || id.length === 0 || id.length > 256) {
    throw new ValidationError(`${label} did not carry an event id`);
  }
  const prefix = `${hubRunId}:`;
  if (!id.startsWith(prefix) || !/^\d+$/.test(id.slice(prefix.length))) {
    throw new ValidationError(`${label} event id does not match the hubRunId:cursor contract`);
  }
}

function validateMetrics(machine, targetMachineId, label) {
  if (machine === null || typeof machine !== "object") {
    throw new ValidationError(`${label} did not include the target machine`);
  }
  if (machine.machineId !== targetMachineId) {
    throw new ValidationError(`${label} target machine identity changed`);
  }
  if (machine.connected !== true) {
    throw new ValidationError(`${label} target machine is not connected`);
  }
  if (typeof machine.collectorId !== "string" || machine.collectorId.length === 0) {
    throw new ValidationError(`${label} target machine has no collector identity`);
  }
  const metrics = machine.metrics;
  if (metrics === undefined) return null;
  if (metrics === null || typeof metrics !== "object" || Array.isArray(metrics)) {
    throw new ValidationError(`${label} target machine metrics is malformed`);
  }
  if (metrics.protocolVersion !== undefined || metrics.kind !== "metrics") {
    throw new ValidationError(`${label} target machine metrics has an unsupported shape`);
  }
  if (metrics.collectorId !== machine.collectorId) {
    throw new ValidationError(`${label} target machine metrics identity is inconsistent`);
  }
  if (typeof metrics.runId !== "string" || metrics.runId.length === 0
    || typeof metrics.bootId !== "string" || metrics.bootId.length === 0
    || !Number.isInteger(metrics.sequence) || metrics.sequence < 0) {
    throw new ValidationError(`${label} target machine metrics metadata is malformed`);
  }
  if (typeof metrics.capturedAt !== "string" || !Number.isFinite(Date.parse(metrics.capturedAt))) {
    throw new ValidationError(`${label} target machine metrics has an invalid capture timestamp`);
  }
  if (typeof metrics.receivedAt !== "string" || !Number.isFinite(Date.parse(metrics.receivedAt))) {
    throw new ValidationError(`${label} target machine metrics has an invalid receipt timestamp`);
  }
  if (metrics.payload === null || typeof metrics.payload !== "object" || Array.isArray(metrics.payload)) {
    throw new ValidationError(`${label} target machine metrics has no metrics payload`);
  }
  validateCpuMetric(metrics.payload.cpu, label);
  validateMemoryMetric(metrics.payload.memory, label);
  return metrics;
}

function validateCpuMetric(cpu, label) {
  if (cpu === null || typeof cpu !== "object" || Array.isArray(cpu)) {
    throw new ValidationError(`${label} target machine metrics has a malformed CPU metric`);
  }
  if (cpu.status === "ok") {
    if (!Number.isFinite(cpu.usagePercent) || cpu.usagePercent < 0 || cpu.usagePercent > 100) {
      throw new ValidationError(`${label} target machine metrics has an invalid CPU reading`);
    }
    if (!Array.isArray(cpu.perCore) || cpu.perCore.length > 1024
      || !cpu.perCore.every((value) => value === null || (Number.isFinite(value) && value >= 0 && value <= 100))) {
      throw new ValidationError(`${label} target machine metrics has an invalid per-core CPU reading`);
    }
    return;
  }
  if (cpu.status === "unavailable" && typeof cpu.reason === "string" && cpu.reason.length > 0) {
    return;
  }
  throw new ValidationError(`${label} target machine metrics has an unsupported CPU metric`);
}

function validateMemoryMetric(memory, label) {
  if (memory === null || typeof memory !== "object" || Array.isArray(memory)) {
    throw new ValidationError(`${label} target machine metrics has a malformed memory metric`);
  }
  if (memory.status === "ok") {
    if (!Number.isInteger(memory.usedBytes) || !Number.isInteger(memory.totalBytes)
      || memory.totalBytes <= 0 || memory.usedBytes < 0 || memory.usedBytes > memory.totalBytes) {
      throw new ValidationError(`${label} target machine metrics has an invalid memory reading`);
    }
    return;
  }
  if (memory.status === "unavailable" && typeof memory.reason === "string" && memory.reason.length > 0) {
    return;
  }
  throw new ValidationError(`${label} target machine metrics has an unsupported memory metric`);
}

/** True only when both readings are present, not merely unavailable. */
function summaryHasUsableMetrics(metrics) {
  return metrics.payload.cpu.status === "ok" && metrics.payload.memory.status === "ok";
}

function summaryAdvances(previous, next) {
  if (next.sequence > previous.sequence) return true;
  return Date.parse(next.capturedAt) > Date.parse(previous.capturedAt);
}

async function observeAdvancingSummaries(ctx, stream, targetMachineId, initialMetrics, sampleWaitMs) {
  const sampleDeadline = Date.now() + remaining(ctx, sampleWaitMs);
  let previous = initialMetrics;
  let observations = previous === null ? 0 : 1;

  for (;;) {
    const left = Math.min(sampleDeadline - Date.now(), ctx.deadline - Date.now());
    if (left <= 0) {
      throw new ValidationError(
        `no advancing CPU/memory summary with usable readings was observed for the target machine within ${sampleWaitMs}ms`,
      );
    }
    let item;
    try {
      item = await stream.waitFor(
        (candidate) => candidate.kind === "event",
        Math.max(1, left),
        "advancing CPU/memory summary",
      );
    } catch (error) {
      if (error instanceof ValidationError) {
        throw new ValidationError(
          `no advancing CPU/memory summary with usable readings was observed for the target machine within ${sampleWaitMs}ms (${error.message})`,
        );
      }
      throw error;
    }
    const snapshot = parseSnapshotData(item.data, "SSE update");
    const machine = findMachine(snapshot, targetMachineId);
    if (machine === null) continue;
    if (machine.connected !== true) {
      throw new ValidationError("the target machine disconnected during SSE validation");
    }
    const metrics = validateMetrics(machine, targetMachineId, "SSE update");
    if (metrics === null) continue;
    if (previous === null) {
      previous = metrics;
      observations = 1;
      continue;
    }
    if (summaryAdvances(previous, metrics)) {
      observations += 1;
      if (summaryHasUsableMetrics(metrics)) {
        return { summary: metrics, observations };
      }
      // A real collector's first sample can legitimately report CPU as
      // unavailable while it establishes a baseline. Keep the sequence moving
      // and wait for the next advancing readings.
      previous = metrics;
      continue;
    }
    // A non-advancing frame for the target machine (for example another row's
    // change) is not evidence; keep waiting inside the bounded sample window.
  }
}

/**
 * Runs the actual-HTTPS probe sequence. `options.trustedCa` exists only so the
 * test suite can add a local fixture certificate as a trust anchor; the CLI
 * never sets it and there is no option that disables verification.
 */
async function validateHttpsDeployment(input, options = {}) {
  if (input === null || typeof input !== "object") {
    throw new ValidationError("validation input is missing");
  }
  const originInfo = parseCanonicalHttpsOrigin(input.origin);
  const username = parseValidationUsername(input.username);
  if (typeof input.password !== "string" || input.password.length === 0
    || input.password.length > MAX_PASSWORD_CHARS) {
    throw new ValidationError("browser password is missing or outside the supported size");
  }
  const requestedMachineId = input.machineId === undefined || input.machineId === ""
    ? null
    : parseMachineId(input.machineId);

  const ctx = createContext({ ...options, originInfo });
  const checks = [];
  let initialStream = null;
  let resetStream = null;
  let cleanupSession = null;
  let loggedOut = false;

  try {
    // 1. Certificate verification + unauthenticated health. Node only resolves
    // this request after normal certificate and hostname verification.
    const health = await requestHttps(ctx, { path: "/api/health", label: "/api/health" });
    if (health.status !== 200) {
      throw new ValidationError(`/api/health returned status ${health.status}; expected 200`);
    }
    const healthBody = parseJsonText(health.body, "/api/health");
    if (healthBody === null || typeof healthBody !== "object" || healthBody.status !== "ok") {
      throw new ValidationError("/api/health did not report an ok status");
    }
    checks.push("TLS certificate verification and /api/health");

    // 2. Anonymous live data must fail closed with JSON 401, not an SSE stream.
    const anonymousLive = await requestHttps(ctx, {
      path: "/api/live",
      label: "anonymous /api/live",
      headers: { accept: "text/event-stream" },
      rejectEventStream: true,
    });
    if (anonymousLive.status !== 401) {
      throw new ValidationError(`anonymous /api/live returned status ${anonymousLive.status}; expected 401`);
    }
    if (headerText(anonymousLive.headers, "content-type").toLowerCase().includes("text/event-stream")) {
      throw new ValidationError("anonymous /api/live returned an event stream");
    }
    const foreignLive = await requestHttps(ctx, {
      path: "/api/live",
      label: "foreign-Origin /api/live",
      headers: { accept: "text/event-stream", origin: "https://invalid.example.test" },
      rejectEventStream: true,
    });
    if (foreignLive.status !== 403) {
      throw new ValidationError(`foreign-Origin /api/live returned status ${foreignLive.status}; expected 403`);
    }
    checks.push("anonymous /api/live rejection and Origin enforcement");

    // 3. An invalid random password, never the real one, must get a generic 401
    // and no session cookie.
    const invalidPassword = randomPasswordDifferentFrom(input.password);
    const invalidLogin = await requestHttps(ctx, {
      method: "POST",
      path: "/api/auth/login",
      label: "invalid-password login",
      headers: { origin: originInfo.origin, "content-type": "application/json" },
      body: JSON.stringify({ username, password: invalidPassword }),
    });
    if (invalidLogin.status !== 401) {
      throw new ValidationError(`invalid-password login returned status ${invalidLogin.status}; expected 401`);
    }
    if (hasSetCookie(invalidLogin.headers)) {
      throw new ValidationError("invalid-password login set a session cookie");
    }
    checks.push("invalid password rejection");

    // 4. Missing/foreign/plaintext Origin must fail even when forwarding headers
    // claim the canonical HTTPS host and protocol.
    const spoofedForwarding = {
      "x-forwarded-host": originInfo.host,
      "x-forwarded-proto": "https",
      forwarded: `host=${originInfo.host};proto=https`,
    };
    const originCases = [
      ["a missing Origin with spoofed forwarding", spoofedForwarding],
      ["a foreign Origin with spoofed forwarding", {
        origin: "https://invalid.example.test",
        ...spoofedForwarding,
      }],
      ["a plaintext Origin with spoofed forwarding", {
        origin: `http://${originInfo.host}`,
        ...spoofedForwarding,
      }],
    ];
    for (const [label, extraHeaders] of originCases) {
      const response = await requestHttps(ctx, {
        method: "POST",
        path: "/api/auth/login",
        label: `login with ${label}`,
        headers: { "content-type": "application/json", ...extraHeaders },
        body: JSON.stringify({ username, password: invalidPassword }),
      });
      if (response.status !== 403) {
        throw new ValidationError(`login with ${label} returned status ${response.status}; expected 403`);
      }
      if (hasSetCookie(response.headers)) {
        throw new ValidationError(`login with ${label} set a session cookie`);
      }
    }
    checks.push("exact Origin and forwarded-spoof rejection");

    // 5. Valid login: exact canonical Origin, JSON, fixed secure host-only
    // cookie, and no credentials in the JSON response.
    const login = await requestHttps(ctx, {
      method: "POST",
      path: "/api/auth/login",
      label: "valid login",
      headers: { origin: originInfo.origin, "content-type": "application/json" },
      body: JSON.stringify({ username, password: input.password }),
    });
    if (login.status !== 200) {
      throw new ValidationError(`valid login returned status ${login.status}; expected 200`);
    }
    // Capture only the expected opaque credentials before validating cookie
    // attributes so even a failed cookie/security assertion can end its session.
    try {
      const cookies = login.headers["set-cookie"];
      const all = Array.isArray(cookies) ? cookies : [cookies];
      const raw = all.find(
        (value) => typeof value === "string" && value.startsWith(SESSION_COOKIE_NAME + "="),
      ) ?? (all.length === 1 ? all[0] : undefined);
      const pair = typeof raw === "string" ? raw.split(";", 1)[0] : "";
      const token = pair.includes("=") ? pair.slice(pair.indexOf("=") + 1) : "";
      const body = JSON.parse(login.body);
      if (/^[A-Za-z0-9_-]{32,128}$/.test(token ?? "")
        && typeof body?.csrfToken === "string" && /^[A-Za-z0-9_-]{16,128}$/.test(body.csrfToken)) {
        cleanupSession = { cookie: SESSION_COOKIE_NAME + "=" + token, csrfToken: body.csrfToken };
      }
    } catch { /* Malformed responses may not provide usable cleanup credentials. */ }
    assertNoStore(login.headers, "login");
    const sessionToken = assertSecureSessionCookie(login.headers, "login");
    const loginBody = parseJsonText(login.body, "login");
    if (loginBody === null || typeof loginBody !== "object" || loginBody.username !== username
      || typeof loginBody.csrfToken !== "string"
      || !/^[A-Za-z0-9_-]{16,}$/.test(loginBody.csrfToken)) {
      throw new ValidationError("login response did not contain the expected username and CSRF token");
    }
    if (Object.prototype.hasOwnProperty.call(loginBody, "password")
      || Object.prototype.hasOwnProperty.call(loginBody, "passwordHash")) {
      throw new ValidationError("login response exposed credential material");
    }
    const cookieHeader = `${SESSION_COOKIE_NAME}=${sessionToken}`;
    checks.push("secure __Host session cookie");

    // 6. The cookie restores the same opaque session and CSRF value.
    const restored = await requestHttps(ctx, {
      path: "/api/auth/session",
      label: "session restore",
      headers: { cookie: cookieHeader },
    });
    if (restored.status !== 200) {
      throw new ValidationError(`session restore returned status ${restored.status}; expected 200`);
    }
    assertNoStore(restored.headers, "session restore");
    const restoredBody = parseJsonText(restored.body, "session restore");
    if (restoredBody === null || typeof restoredBody !== "object"
      || restoredBody.username !== username
      || restoredBody.csrfToken !== loginBody.csrfToken) {
      throw new ValidationError("session restore did not preserve the authenticated identity");
    }
    checks.push("session restore");

    // 7. Authenticated SSE: snapshot, event id, target machine, keepalive,
    // reconnect reset, and advancing CPU/memory summaries.
    const sseHeaders = {
      accept: "text/event-stream",
      cookie: cookieHeader,
      origin: originInfo.origin,
    };
    initialStream = await startSse(ctx, {
      label: "authenticated SSE snapshot",
      headers: sseHeaders,
    });
    const initialEvent = await initialStream.waitFor(
      (item) => item.kind === "event",
      remaining(ctx, ctx.initialEventMs),
      "authenticated SSE snapshot",
    );
    if (initialEvent.event !== "snapshot") {
      throw new ValidationError("initial authenticated SSE event must be snapshot");
    }
    const initialSnapshot = parseSnapshotData(initialEvent.data, "initial SSE snapshot");
    assertEventId(initialEvent.id, initialSnapshot.hubRunId, "initial SSE snapshot");
    const targetMachineId = chooseTargetMachine(initialSnapshot, requestedMachineId);
    const initialMachine = findMachine(initialSnapshot, targetMachineId);
    const initialMetrics = validateMetrics(initialMachine, targetMachineId, "initial SSE snapshot");
    checks.push("authenticated SSE snapshot for a connected machine");

    resetStream = await startSse(ctx, {
      label: "SSE reconnect",
      headers: { ...sseHeaders, "last-event-id": initialEvent.id },
    });
    const resetEvent = await resetStream.waitFor(
      (item) => item.kind === "event",
      remaining(ctx, ctx.initialEventMs),
      "SSE reconnect reset",
    );
    if (resetEvent.event !== "reset") {
      throw new ValidationError("reconnect SSE event must be reset");
    }
    const resetSnapshot = parseSnapshotData(resetEvent.data, "reconnect SSE reset");
    assertEventId(resetEvent.id, resetSnapshot.hubRunId, "reconnect SSE reset");
    const resetMachine = findMachine(resetSnapshot, targetMachineId);
    if (resetMachine === null) {
      throw new ValidationError("the target machine is missing from the reconnect reset snapshot");
    }
    validateMetrics(resetMachine, targetMachineId, "reconnect SSE reset");
    checks.push("reconnect reset with event ID");

    const keepaliveItem = await initialStream.waitFor(
      (item) => item.kind === "comment" && /^keepalive \d+$/.test(item.value),
      remaining(ctx, ctx.keepaliveWaitMs),
      "15-second SSE keepalive",
    );
    const keepaliveMs = keepaliveItem.at - initialStream.openedAt;
    if (keepaliveMs < ctx.keepalive.minMs || keepaliveMs > ctx.keepalive.maxMs) {
      throw new ValidationError(
        `SSE keepalive arrived after ${keepaliveMs}ms; expected approximately ${ctx.keepalive.expectedMs}ms`,
      );
    }
    checks.push("15-second SSE keepalive");

    const advancing = await observeAdvancingSummaries(
      ctx,
      initialStream,
      targetMachineId,
      initialMetrics,
      ctx.sampleWaitMs,
    );
    const finalMetrics = advancing.summary;
    checks.push("advancing CPU/memory summaries");

    // 8. Logout: CSRF is mandatory, the session survives a rejected attempt,
    // a valid logout deletes the cookie with matching attributes, closes both
    // active SSE streams, and the old cookie is rejected afterward.
    const wrongCsrf = randomPasswordDifferentFrom(loginBody.csrfToken);
    const rejectedLogout = await requestHttps(ctx, {
      method: "POST",
      path: "/api/auth/logout",
      label: "logout with an invalid CSRF token",
      headers: {
        cookie: cookieHeader,
        origin: originInfo.origin,
        "x-csrf-token": wrongCsrf,
      },
    });
    if (rejectedLogout.status !== 403) {
      throw new ValidationError(`logout with an invalid CSRF token returned status ${rejectedLogout.status}; expected 403`);
    }
    if (hasSetCookie(rejectedLogout.headers)) {
      throw new ValidationError("logout with an invalid CSRF token emitted a cookie");
    }
    checks.push("logout CSRF rejection");

    const stillAuthenticated = await requestHttps(ctx, {
      path: "/api/auth/session",
      label: "session after rejected logout",
      headers: { cookie: cookieHeader },
    });
    if (stillAuthenticated.status !== 200) {
      throw new ValidationError("a rejected CSRF logout ended the session");
    }
    checks.push("session survives rejected logout");

    for (const stream of [initialStream, resetStream]) {
      if (stream.failure !== null) throw stream.failure;
      if (stream.closed) throw new ValidationError("authenticated SSE stream ended before logout");
    }
    const logoutStartedAt = Date.now();
    const logout = await requestHttps(ctx, {
      method: "POST",
      path: "/api/auth/logout",
      label: "valid logout",
      headers: {
        cookie: cookieHeader,
        origin: originInfo.origin,
        "x-csrf-token": loginBody.csrfToken,
      },
    });
    if (logout.status !== 200) {
      throw new ValidationError(`valid logout returned status ${logout.status}; expected 200`);
    }
    assertClearedSessionCookie(logout.headers, "logout");
    checks.push("logout cookie deletion attributes");

    const closeResults = await Promise.allSettled([
      initialStream.waitClosed(remaining(ctx, ctx.logoutCloseMs), "valid logout closing the initial SSE stream"),
      resetStream.waitClosed(remaining(ctx, ctx.logoutCloseMs), "valid logout closing the reconnect SSE stream"),
    ]);
    const closeFailure = closeResults.find((result) => result.status === "rejected");
    if (closeFailure !== undefined && closeFailure.status === "rejected") {
      throw closeFailure.reason instanceof ValidationError
        ? closeFailure.reason
        : new ValidationError("valid logout did not close the active SSE streams");
    }
    for (const stream of [initialStream, resetStream]) {
      if (stream.failure !== null) throw stream.failure;
      if (stream.closedAt === null || stream.closedAt < logoutStartedAt) {
        throw new ValidationError("SSE stream did not close after logout started");
      }
    }
    checks.push("logout closes active SSE streams");

    const postLogoutSession = await requestHttps(ctx, {
      path: "/api/auth/session",
      label: "session after logout",
      headers: { cookie: cookieHeader },
    });
    if (postLogoutSession.status !== 401) {
      throw new ValidationError(`post-logout session returned status ${postLogoutSession.status}; expected 401`);
    }
    const postLogoutLive = await requestHttps(ctx, {
      path: "/api/live",
      label: "live data after logout",
      headers: { accept: "text/event-stream", cookie: cookieHeader },
      rejectEventStream: true,
    });
    if (postLogoutLive.status !== 401) {
      throw new ValidationError(`post-logout /api/live returned status ${postLogoutLive.status}; expected 401`);
    }
    if (headerText(postLogoutLive.headers, "content-type").toLowerCase().includes("text/event-stream")) {
      throw new ValidationError("post-logout /api/live returned an event stream");
    }
    checks.push("post-logout rejection");
    loggedOut = true;

    return {
      origin: originInfo.origin,
      username,
      machineId: targetMachineId,
      machineSelectedByOperator: requestedMachineId !== null,
      cpuStatus: finalMetrics.payload.cpu.status,
      memoryStatus: finalMetrics.payload.memory.status,
      keepaliveMs,
      summaryObservations: advancing.observations,
      checks,
    };
  } finally {
    if (cleanupSession !== null && !loggedOut) {
      // A failed main deadline must not prevent a short cleanup attempt. This
      // separate context still requires TLS verification and refuses redirects.
      const cleanup = createContext({
        originInfo, trustedCa: ctx.trustedCa, totalTimeoutMs: 2_000, requestTimeoutMs: 1_500,
      });
      try {
        await requestHttps(cleanup, {
          method: "POST", path: "/api/auth/logout", label: "validation session cleanup",
          headers: {
            origin: originInfo.origin,
            cookie: cleanupSession.cookie,
            "x-csrf-token": cleanupSession.csrfToken,
          },
        });
      } catch { /* Best effort: preserve the original safe validation error. */ }
      finally { for (const request of cleanup.active) request.destroy(); }
    }
    for (const stream of [...ctx.streams]) {
      try {
        stream.close();
      } catch {
        // cleanup is best-effort
      }
    }
    ctx.streams.clear();
    for (const request of [...ctx.active]) {
      try {
        request.destroy();
      } catch {
        // cleanup is best-effort
      }
    }
    ctx.active.clear();
  }
}

function assertNoStore(headers, label) {
  if (!/(?:^|,)\s*no-store\s*(?:,|$)/i.test(headerText(headers, "cache-control"))) {
    throw new ValidationError(label + " response must carry Cache-Control: no-store");
  }
}

function formatSuccess(result) {
  const selection = result.machineSelectedByOperator
    ? "operator-selected"
    : "auto-selected; set RIGSCOPE_VALIDATE_MACHINE_ID to pin a machine";
  return [
    `Rigscope HTTPS automated checks passed for ${result.origin}.`,
    ...result.checks.map((check) => `  - ${check}`),
    `  - target machine ${JSON.stringify(result.machineId)} (${selection}); CPU ${result.cpuStatus}, memory ${result.memoryStatus}; advancing observations: ${result.summaryObservations}`,
    "",
    SUCCESS_SCOPE,
  ].join("\n");
}

function safeErrorMessage(error) {
  if (error instanceof ValidationError) return error.message;
  if (error !== null && typeof error === "object"
    && typeof error.code === "string" && /^[A-Z0-9_]+$/.test(error.code)) {
    return `unexpected ${error.code}`;
  }
  return "unexpected validation error";
}

async function main(argv = process.argv.slice(2), env = process.env) {
  if (argv.length > 0) {
    throw new ValidationError(
      "this command takes no arguments; use RIGSCOPE_VALIDATE_ORIGIN, RIGSCOPE_VALIDATE_USERNAME, and RIGSCOPE_VALIDATE_PASSWORD_FILE",
    );
  }
  const config = resolveValidationConfig(env);
  const password = await readPasswordFile(config.passwordFile);
  const result = await validateHttpsDeployment({
    origin: config.origin,
    username: config.username,
    password,
    machineId: config.machineId,
  });
  console.log(formatSuccess(result));
  return result;
}

if (require.main === module) {
  main().catch((error) => {
    console.error(`Rigscope HTTPS validation failed: ${safeErrorMessage(error)}`);
    console.error(FAILURE_SCOPE);
    process.exitCode = 1;
  });
}

module.exports = {
  ValidationError,
  parseCanonicalHttpsOrigin,
  parseValidationUsername,
  parseMachineId,
  resolveValidationConfig,
  readPasswordFile,
  randomPasswordDifferentFrom,
  assertSecureSessionCookie,
  assertClearedSessionCookie,
  validateHttpsDeployment,
  formatSuccess,
  main,
};
