/**
 * Webhook URL/header validation and the real async sender.
 *
 * The URL and credentials never leave the server: this module returns them
 * only to the engine/sender, while HTTP responses use the secret-free
 * `AlertWebhookStatus`. The sender has a hard deadline, refuses redirects and
 * reports bounded error text without echoing the URL or headers.
 */
import { createHmac } from "node:crypto";

import { ALERT_LIMITS, type AlertWebhookTransport } from "../../alerts/types";
import { AlertValidationError } from "./errors";

export type ParsedWebhookUrl = {url: string; transport: AlertWebhookTransport};

export function isLoopbackHostname(hostname: string): boolean {
  const host = hostname.toLowerCase();
  return host === "localhost" || host === "::1" || host === "[::1]" || /^127(?:\.\d{1,3}){3}$/.test(host);
}

/**
 * Accepts exact HTTPS endpoints and HTTP only for loopback-controlled sinks.
 * Userinfo and fragments are rejected. Query strings are allowed because some
 * receivers require a channel token in the path; they remain private.
 */
export function parseWebhookUrl(raw: unknown): ParsedWebhookUrl {
  if (typeof raw !== "string") throw new AlertValidationError("webhook url must be a string");
  const value = raw.trim();
  if (value.length === 0 || value.length > ALERT_LIMITS.maxWebhookUrl) {
    throw new AlertValidationError("webhook url length is invalid");
  }
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw new AlertValidationError("webhook url is not a valid URL");
  }
  if (url.username !== "" || url.password !== "") {
    throw new AlertValidationError("webhook url must not embed credentials");
  }
  if (url.hash !== "") throw new AlertValidationError("webhook url must not include a fragment");
  if (url.protocol === "https:") {
    if (url.hostname === "") throw new AlertValidationError("webhook url must include a host");
    return {url: url.href, transport: "https"};
  }
  if (url.protocol === "http:" && isLoopbackHostname(url.hostname)) {
    return {url: url.href, transport: "loopback-http"};
  }
  throw new AlertValidationError(
    "webhook url must be HTTPS, or loopback HTTP for a controlled local sink",
  );
}

const HEADER_NAME = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;
const FORBIDDEN_HEADERS = new Set(["host", "content-length", "content-type", "idempotency-key"]);

export function parseWebhookHeaders(raw: unknown): Record<string, string> {
  if (raw === undefined || raw === null) return {};
  if (typeof raw !== "object" || Array.isArray(raw)) {
    throw new AlertValidationError("webhook headers must be an object of strings");
  }
  const entries = Object.entries(raw as Record<string, unknown>);
  if (entries.length > ALERT_LIMITS.maxWebhookHeaders) {
    throw new AlertValidationError("too many webhook headers");
  }
  const headers: Record<string, string> = {};
  for (const [rawKey, rawValue] of entries) {
    const key = rawKey.trim();
    if (
      key.length === 0 ||
      key.length > ALERT_LIMITS.maxWebhookHeaderKey ||
      !HEADER_NAME.test(key) ||
      FORBIDDEN_HEADERS.has(key.toLowerCase())
    ) {
      throw new AlertValidationError("webhook header name is invalid or reserved");
    }
    if (typeof rawValue !== "string" || rawValue.length > ALERT_LIMITS.maxWebhookHeaderValue) {
      throw new AlertValidationError("webhook header value is invalid");
    }
    if (/[\u0000-\u001f\u007f]/.test(rawValue)) {
      throw new AlertValidationError("webhook header value must not contain control characters");
    }
    headers[key] = rawValue;
  }
  return headers;
}

export function parseWebhookSecret(raw: unknown): string | null {
  if (raw === undefined || raw === null) return null;
  if (typeof raw !== "string") throw new AlertValidationError("webhook secret must be a string");
  const value = raw.trim();
  if (value.length === 0) return null;
  if (value.length > ALERT_LIMITS.maxWebhookSecret || /[\u0000-\u001f\u007f]/.test(value)) {
    throw new AlertValidationError("webhook secret is invalid");
  }
  return value;
}

export type WebhookDeliveryRequest = {
  url: string;
  headers: Record<string, string>;
  body: string;
  timeoutMs: number;
  jobId: string;
  secret: string | null;
  /** Aborted by AlertEngine.stop()/drain(); injected senders should honor it. */
  signal?: AbortSignal;
};

export type WebhookDeliveryResult = {
  ok: boolean;
  status: number | null;
  error: string | null;
};

export type WebhookSender = (request: WebhookDeliveryRequest) => Promise<WebhookDeliveryResult>;

export function sanitizeDeliveryError(error: unknown): string {
  if (error instanceof Error) {
    if (error.name === "AbortError" || error.name === "TimeoutError") return "delivery timed out";
  }
  return "delivery failed";
}

/**
 * Real HTTP delivery. `redirect: "error"` prevents a configured endpoint from
 * silently redirecting a secret-bearing request to another host.
 */
export const defaultWebhookSender: WebhookSender = async (request) => {
  const headers: Record<string, string> = {
    ...request.headers,
    "content-type": "application/json",
    "idempotency-key": request.jobId,
  };
  if (request.secret !== null) {
    headers["x-rigscope-signature"] = `sha256=${createHmac("sha256", request.secret).update(request.body).digest("hex")}`;
  }
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), request.timeoutMs);
  const onAbort = () => controller.abort();
  request.signal?.addEventListener("abort", onAbort, {once: true});
  try {
    const response = await fetch(request.url, {
      method: "POST",
      headers,
      body: request.body,
      redirect: "error",
      signal: controller.signal,
    });
    if (!response.ok) {
      await response.body?.cancel().catch(() => {});
      return {ok: false, status: response.status, error: `HTTP ${response.status}`};
    }
    await response.body?.cancel().catch(() => {});
    return {ok: true, status: response.status, error: null};
  } catch (error) {
    return {ok: false, status: null, error: sanitizeDeliveryError(error)};
  } finally {
    clearTimeout(timer);
    request.signal?.removeEventListener("abort", onAbort);
  }
};
