/**
 * Jittered exponential reconnect schedule for collector destinations.
 *
 * Kept as a pure function so unit tests can assert bounds deterministically:
 * delay(attempt) grows exponentially from `baseMs`, is capped at `maxMs`, and
 * is then multiplied by a jitter factor in [0.7, 1.3] driven by `rand()`.
 */

export type BackoffOptions = {
  /** Delay for the first retry attempt. */
  baseMs?: number;
  /** Upper bound for any computed delay. */
  maxMs?: number;
};

const DEFAULT_BASE_MS = 1_000;
const DEFAULT_MAX_MS = 30_000;
const JITTER_LOW = 0.7;
const JITTER_HIGH = 1.3;

export function nextReconnectDelayMs(
  attempts: number,
  rand: () => number = Math.random,
  options: BackoffOptions = {},
): number {
  const baseMs = options.baseMs ?? DEFAULT_BASE_MS;
  const maxMs = options.maxMs ?? DEFAULT_MAX_MS;
  const clampedAttempts = Math.max(0, Math.floor(attempts));
  const exponential = Math.min(baseMs * Math.pow(2, clampedAttempts), maxMs);
  const jitter = JITTER_LOW + (JITTER_HIGH - JITTER_LOW) * rand();
  return Math.max(1, Math.min(Math.round(exponential * jitter), maxMs));
}
