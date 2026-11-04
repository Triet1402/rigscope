/**
 * Collector configuration parsing and validation for the runtime spike.
 *
 * This is a loopback-only spike: plaintext `ws://` destinations are restricted
 * to literal loopback addresses (`127.0.0.0/8`, `::1`). Remote
 * plaintext is rejected; remote TLS (`wss://` with ordinary certificate
 * validation) is the supported remote transport in principle, but this spike
 * makes no remote deployment or enrollment claims.
 *
 * Tokens travel only in the Authorization header, never in the URL, so hub
 * URLs must not contain userinfo, query strings, or fragments.
 *
 * The composition root resolves persistent real identity and enrolled destinations
 * before calling this parser. Defaults here preserve explicit mock fixtures.
 */

import { SHARING_KEYS, type SharingInput } from './destinations/policy';

export class ConfigError extends Error {
  readonly issues: readonly string[];

  constructor(issues: readonly string[]) {
    super(`invalid collector configuration:\n- ${issues.join('\n- ')}`);
    this.name = 'ConfigError';
    this.issues = [...issues];
  }
}

export const DEFAULT_COLLECTOR_ID = 'mock-collector';
export const MOCK_HOSTNAME = 'mock-machine';
export const CONNECT_PATH = '/api/collector/connect';
/** Persisted installations from before the protocol consolidation use this path. */
export const PREVIOUS_CONNECT_PATH = '/api/collector/v1/connect';
/** Hard destination budget for the spike; a larger fleet needs a redesign. */
export const MAX_DESTINATIONS = 8;

const DEFAULT_INTERVAL_MS = 1_000;
const MIN_INTERVAL_MS = 100;
const MAX_INTERVAL_MS = 3_600_000;
// Matches the shared contract's identifier bound (MAX_ID_LENGTH = 128).
const MAX_ID_LENGTH = 128;
// Matches the shared contract's hostname bound (MAX_HOSTNAME_LENGTH = 255).
const MAX_HOSTNAME_LENGTH = 253;
/** Matches the hub's credential bound. */
const MAX_TOKEN_LENGTH = 256;

const PRINTABLE_ASCII = /^[\x21-\x7e]+$/;
const IPV4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;
const DIGITS = /^\d+$/;

export type EnvLike = Readonly<Record<string, string | undefined>>;

export type DestinationEndpoint = {
  /** Validated gateway URL (normalized href). */
  url: string;
  /** Bearer token; sent only in the Authorization header. */
  token: string;
  /**
   * Local sharing policy. Optional; every omitted family gets its default in
   * policy.ts.
   */
  sharing?: SharingInput;
  intervalMs?: number;
};

export type CollectorConfig = {
  collectorId: string;
  hostname: string;
  /**
   * True when COLLECTOR_HOSTNAME was absent and the mock default
   * "mock-machine" is in effect; used to log mock mode explicitly.
   */
  hostnameIsMockDefault: boolean;
  intervalMs: number;
  destinations: readonly DestinationEndpoint[];
};

function requirePrintable(value: unknown, label: string, issues: string[], maxLength: number): void {
  if (typeof value !== 'string' || !PRINTABLE_ASCII.test(value)) {
    issues.push(
      `${label} must be a non-empty printable-ASCII string without spaces/control characters`,
    );
    return;
  }
  if (value.length > maxLength) {
    issues.push(`${label} must be at most ${maxLength} characters`);
  }
}

/**
 * Literal loopback check for plaintext destinations. Hostnames are limited to
 * numeric loopback literals; the URL parser never resolves DNS.
 */
function isLoopbackHostname(hostname: string): boolean {
  let lower = hostname.toLowerCase();
  if (lower.startsWith('[') && lower.endsWith(']')) {
    // WHATWG URL keeps brackets in the hostname for non-special ws URLs.
    lower = lower.slice(1, -1);
  }
  if (lower === '::1') {
    return true;
  }
  const match = IPV4.exec(lower);
  if (match === null) {
    return false;
  }
  if (Number(match[1]) !== 127) {
    return false;
  }
  return [match[2], match[3], match[4]].every((part) => Number(part) <= 255);
}

/**
 * Validates and parses a single hub URL. Issue messages deliberately never
 * echo the full URL so credentials accidentally embedded there are not logged.
 */
export function validateHubUrl(raw: unknown, label: string, issues: string[]): URL | null {
  if (typeof raw !== 'string' || raw.trim() === '') {
    issues.push(`${label}: url must be a non-empty string`);
    return null;
  }
  if (raw.length > 2048) {
    issues.push(`${label}: url must be at most 2048 characters`);
    return null;
  }
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    issues.push(`${label}: not a valid URL`);
    return null;
  }
  if (parsed.protocol !== 'ws:' && parsed.protocol !== 'wss:') {
    issues.push(`${label}: scheme must be ws:// or wss://`);
    return null;
  }
  if (parsed.username !== '' || parsed.password !== '') {
    issues.push(
      `${label}: URL must not embed credentials (userinfo); send the token in the Authorization header`,
    );
    return null;
  }
  if (parsed.search !== '') {
    issues.push(`${label}: URL must not include a query string`);
    return null;
  }
  if (parsed.hash !== '') {
    issues.push(`${label}: URL must not include a fragment`);
    return null;
  }
  if (parsed.hostname === '') {
    issues.push(`${label}: URL must include a host`);
    return null;
  }
  // Normalize local configuration only; the hub still exposes one wire endpoint.
  if (parsed.pathname === PREVIOUS_CONNECT_PATH) parsed.pathname = CONNECT_PATH;
  if (parsed.pathname !== CONNECT_PATH) {
    issues.push(`${label}: path must be exactly ${CONNECT_PATH}`);
    return null;
  }
  if (parsed.protocol === 'ws:' && !isLoopbackHostname(parsed.hostname)) {
    issues.push(
      `${label}: plaintext ws:// is restricted to literal loopback addresses; use wss:// otherwise`,
    );
    return null;
  }
  return parsed;
}

type ParseOutcome<T> = {value: T} | {value: null};

const SHARING_KEY_SET = new Set<string>(SHARING_KEYS);

/**
 * Validates a destination sharing object. `cpu` and `memory` are required
 * booleans; the remaining families are optional. Unknown keys are rejected
 * rather than ignored. The input's key shape is preserved so stored objects
 * round-trip unchanged.
 */
function parseSharing(raw: unknown, label: string, issues: string[]): SharingInput | undefined {
  if (raw === undefined) return undefined;
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
    issues.push(`${label}.sharing must be an object`);
    return undefined;
  }
  const record = raw as Record<string, unknown>;
  let valid = true;
  for (const key of Object.keys(record)) {
    if (!SHARING_KEY_SET.has(key)) {
      issues.push(`${label}.sharing has unknown field "${key}"`);
      valid = false;
      continue;
    }
    if (typeof record[key] !== 'boolean') {
      issues.push(`${label}.sharing.${key} must be a boolean`);
      valid = false;
    }
  }
  if (typeof record['cpu'] !== 'boolean' || typeof record['memory'] !== 'boolean') {
    issues.push(`${label}.sharing must contain boolean cpu and memory fields`);
    valid = false;
  }
  if (!valid) return undefined;
  const out: SharingInput = {};
  for (const key of SHARING_KEYS) {
    const value = record[key];
    if (typeof value === 'boolean') out[key] = value;
  }
  return out;
}

function parseDestinations(env: EnvLike, issues: string[], destinations: DestinationEndpoint[]): void {
  const raw = env['COLLECTOR_HUBS']?.trim();
  if (raw === undefined || raw === '') {
    issues.push(
      'COLLECTOR_HUBS is required: JSON array of {url, token} objects (1..8 destinations)',
    );
    return;
  }
  let value: unknown;
  try {
    value = JSON.parse(raw) as unknown;
  } catch {
    issues.push('COLLECTOR_HUBS is not valid JSON');
    return;
  }
  if (!Array.isArray(value)) {
    issues.push('COLLECTOR_HUBS must be a JSON array');
    return;
  }
  if (value.length === 0) {
    issues.push('COLLECTOR_HUBS must contain at least one destination');
    return;
  }
  if (value.length > MAX_DESTINATIONS) {
    issues.push(
      `COLLECTOR_HUBS allows at most ${MAX_DESTINATIONS} destinations (got ${value.length})`,
    );
  }

  const seenUrls = new Set<string>();
  const limit = Math.min(value.length, MAX_DESTINATIONS);
  for (let index = 0; index < limit; index += 1) {
    const label = `COLLECTOR_HUBS[${index}]`;
    const entry: unknown = value[index];
    if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) {
      issues.push(`${label} must be an object`);
      continue;
    }
    const keys = Object.keys(entry as Record<string, unknown>).sort();
    if (
      !keys.includes('url') || !keys.includes('token') ||
      keys.some(k => !['url', 'token', 'sharing', 'intervalMs'].includes(k))
    ) {
      issues.push(`${label} must have exactly the fields {url, token}, plus optional sharing and intervalMs`);
      continue;
    }
    const candidate = entry as {url?: unknown; token?: unknown; sharing?: unknown; intervalMs?: unknown};
    const sharing = parseSharing(candidate.sharing, label, issues);
    if (candidate.intervalMs !== undefined && (
      !Number.isSafeInteger(candidate.intervalMs) ||
      Number(candidate.intervalMs) < MIN_INTERVAL_MS || Number(candidate.intervalMs) > MAX_INTERVAL_MS
    )) {
      issues.push(`${label}.intervalMs is outside cadence bounds`);
    }
    const token = candidate.token;
    if (typeof token !== 'string' || token === '') {
      issues.push(`${label}.token must be a non-empty string`);
    } else {
      requirePrintable(token, `${label}.token`, issues, MAX_TOKEN_LENGTH);
    }
    const url = validateHubUrl(candidate.url, `${label}.url`, issues);
    if (url === null) {
      continue;
    }
    if (seenUrls.has(url.href)) {
      issues.push(`${label}: duplicate destination URL`);
      continue;
    }
    seenUrls.add(url.href);
    if (typeof token === 'string' && token !== '') {
      destinations.push({
        url: url.href,
        token,
        ...(sharing === undefined ? {} : {sharing}),
        ...(candidate.intervalMs === undefined ? {} : {intervalMs: Number(candidate.intervalMs)}),
      });
    }
  }
}

function parseIntervalMs(env: EnvLike, issues: string[]): number {
  const raw = env['COLLECTOR_INTERVAL_MS']?.trim();
  if (raw === undefined || raw === '') {
    return DEFAULT_INTERVAL_MS;
  }
  const parsed = DIGITS.test(raw) ? Number(raw) : Number.NaN;
  if (!Number.isSafeInteger(parsed) || parsed < MIN_INTERVAL_MS || parsed > MAX_INTERVAL_MS) {
    issues.push(
      `COLLECTOR_INTERVAL_MS must be an integer between ${MIN_INTERVAL_MS} and ${MAX_INTERVAL_MS}`,
    );
    return DEFAULT_INTERVAL_MS;
  }
  return parsed;
}

/**
 * Parses collector configuration from an environment-like object.
 * Throws {@link ConfigError} collecting every issue at once instead of
 * stopping at the first problem.
 */
export function parseCollectorConfig(env: EnvLike): CollectorConfig {
  const issues: string[] = [];

  const rawCollectorId = env['COLLECTOR_ID']?.trim() ?? '';
  let collectorId = DEFAULT_COLLECTOR_ID;
  if (rawCollectorId !== '') {
    requirePrintable(rawCollectorId, 'COLLECTOR_ID', issues, MAX_ID_LENGTH);
    collectorId = rawCollectorId;
  }

  const rawHostname = env['COLLECTOR_HOSTNAME']?.trim() ?? '';
  let hostname = MOCK_HOSTNAME;
  if (rawHostname !== '') {
    requirePrintable(rawHostname, 'COLLECTOR_HOSTNAME', issues, MAX_HOSTNAME_LENGTH);
    hostname = rawHostname;
  }

  const destinations: DestinationEndpoint[] = [];
  const intervalMs = parseIntervalMs(env, issues);
  parseDestinations(env, issues, destinations);

  if (issues.length > 0) {
    throw new ConfigError(issues);
  }

  return {
    collectorId,
    hostname,
    hostnameIsMockDefault: rawHostname === '',
    intervalMs,
    destinations,
  };
}
