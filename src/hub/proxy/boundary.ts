/**
 * Remote request boundary for the hub's Unix-socket listener.
 *
 * The outer TLS terminator (Tailscale Serve and similar) is the only thing
 * that can reach the private Unix socket. We therefore trust two static
 * inputs — the configured canonical HTTPS origin and the path-private
 * transport — and nothing else:
 *
 * - The exact canonical Host (including a non-default public port) is
 *   required on EVERY request and upgrade, before enrollment, authentication,
 *   health, SSE, collector, or Next.js handling.
 * - Any incoming Origin header must equal the configured canonical HTTPS
 *   origin before the same checks. Collector upgrades still reject every
 *   Origin in the gateway; this check only narrows what reaches it.
 * - `Forwarded` and every `X-Forwarded-*` header are removed before Next.js
 *   or any handler sees the request. They are never evidence of transport,
 *   client identity, or the public host. After the incoming boundary passes,
 *   the hub sets internally known `X-Forwarded-Proto: https` and
 *   `X-Forwarded-Host: <canonical host>` so framework URL construction has the
 *   transport facts the hub itself established.
 *
 * Loopback TCP mode does not use this module; its behavior is unchanged.
 */

import type { Duplex } from "node:stream";
import type { IncomingMessage } from "node:http";

export type RemoteBoundary = {
  /** Exact canonical HTTPS origin, for example `https://rig.tailnet.ts.net`. */
  publicOrigin: string;
  /** Exact Host header value, including a non-default port. */
  hostHeader: string;
};

export type BoundaryFailure = {
  status: 400 | 403 | 421;
  error: string;
};

type HeaderRead =
  | { kind: "missing" }
  | { kind: "single"; value: string }
  | { kind: "multiple" };

/**
 * Reads exactly one raw header value. Node's combined `req.headers` object
 * merges duplicate fields (notably Host and Origin), which would otherwise
 * let a request smuggle an extra value; `rawHeaders` preserves every field.
 */
export function singleRequestHeader(
  req: IncomingMessage,
  name: string,
): HeaderRead {
  const wanted = name.toLowerCase();
  const values: string[] = [];
  const raw = req.rawHeaders;
  for (let index = 0; index + 1 < raw.length; index += 2) {
    if (raw[index].toLowerCase() !== wanted) continue;
    values.push(raw[index + 1]);
  }
  if (values.length === 0) return { kind: "missing" };
  if (values.length > 1) return { kind: "multiple" };
  return { kind: "single", value: values[0] };
}

/**
 * Validates the static remote boundary before any application handler runs.
 * Returns null when the request may proceed.
 */
export function checkRemoteRequest(
  req: IncomingMessage,
  boundary: RemoteBoundary,
): BoundaryFailure | null {
  const host = singleRequestHeader(req, "host");
  if (host.kind === "missing") {
    return { status: 400, error: "host header required" };
  }
  if (host.kind === "multiple") {
    return { status: 400, error: "duplicate host header" };
  }
  if (host.value !== boundary.hostHeader) {
    return { status: 421, error: "misdirected request" };
  }

  const origin = singleRequestHeader(req, "origin");
  if (origin.kind === "multiple") {
    return { status: 400, error: "duplicate origin header" };
  }
  if (origin.kind === "single" && origin.value !== boundary.publicOrigin) {
    return { status: 403, error: "origin not allowed" };
  }

  // Next.js redirects protocol-relative/backslash targets before routing.
  // Admit only origin-form paths or an HTTP(S) absolute-form target that
  // normalization below can safely reduce to a path and query.
  const target = req.url ?? "/";
  if (target.includes("\\") || /[\u0000-\u0020\u007f]/.test(target)) {
    return { status: 400, error: "invalid request target" };
  }
  if (/^https?:\/\//i.test(target)) {
    try {
      const parsed = new URL(target);
      if (parsed.username || parsed.password || parsed.hash) {
        return { status: 400, error: "invalid request target" };
      }
      if (parsed.pathname.startsWith("//") || /%5c/i.test(parsed.pathname)) {
        return { status: 400, error: "invalid request target" };
      }
    } catch {
      return { status: 400, error: "invalid request target" };
    }
  } else if (!target.startsWith("/") || target.startsWith("//") || target.includes("#") || /%5c/i.test(target.split("?", 1)[0])) {
    return { status: 400, error: "invalid request target" };
  }
  return null;
}

/**
 * Replaces attacker-controlled proxy headers with the values this hub knows
 * from its own validated boundary. Call only after `checkRemoteRequest`
 * succeeds and before Next.js or any handler consumes the request.
 */
export function normalizeForwardedHeaders(
  req: IncomingMessage,
  boundary: RemoteBoundary,
): void {
  for (const key of Object.keys(req.headers)) {
    const lower = key.toLowerCase();
    if (lower === "forwarded" || lower.startsWith("x-forwarded-")) {
      delete req.headers[key];
    }
  }
  // Keep the raw view consistent: some framework code iterates rawHeaders.
  const filteredRaw: string[] = [];
  for (let index = 0; index + 1 < req.rawHeaders.length; index += 2) {
    const lower = req.rawHeaders[index].toLowerCase();
    if (lower === "forwarded" || lower.startsWith("x-forwarded-")) continue;
    filteredRaw.push(req.rawHeaders[index], req.rawHeaders[index + 1]);
  }
  req.rawHeaders = filteredRaw;
  req.headers["x-forwarded-proto"] = "https";
  req.headers["x-forwarded-host"] = boundary.hostHeader;
  req.rawHeaders.push(
    "X-Forwarded-Proto", "https",
    "X-Forwarded-Host", boundary.hostHeader,
  );
  // A proxy normally forwards an origin-form request target. If a client sent
  // absolute-form anyway, keep only its path/query so a foreign scheme/host in
  // the request line cannot compete with the validated boundary.
  if (typeof req.url === "string" && /^https?:\/\//i.test(req.url)) {
    try {
      const parsed = new URL(req.url);
      req.url = `${parsed.pathname}${parsed.search}`;
    } catch {
      // Malformed targets fail later in normal request parsing.
    }
  }
}

/** Writes a minimal HTTP rejection onto a still-plain upgrade socket. */
export function rejectUpgradeSocket(
  socket: Duplex,
  status: BoundaryFailure["status"] | 401 | 429,
  message: string,
): void {
  const reason =
    status === 421
      ? "Misdirected Request"
      : status === 403
        ? "Forbidden"
        : status === 401
          ? "Unauthorized"
          : status === 429
            ? "Too Many Requests"
          : "Bad Request";
  const body = message;
  try {
    socket.end(
      `HTTP/1.1 ${status} ${reason}\r\n` +
        `Connection: close\r\n` +
        `Content-Type: text/plain; charset=utf-8\r\n` +
        `Content-Length: ${Buffer.byteLength(body, "utf8")}\r\n\r\n${body}`,
    );
  } catch {
    socket.destroy();
  }
  socket.once("close", () => socket.destroy());
}
