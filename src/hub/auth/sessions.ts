/**
 * In-memory browser session store (milestone 3 browser access).
 *
 * Sessions are opaque, cryptographically random in-memory tokens. Nothing
 * here is persisted: a hub restart signs every browser out, and scope
 * changes (HUB_BROWSER_USERS) only apply after a restart that also cleared
 * every session. Expiration is fixed at creation (no sliding renewal);
 * a short sweep timer removes expired sessions so open SSE streams for an
 * expired sign-in are ended promptly even when the stream is idle.
 *
 * Tokens and CSRF values travel only in cookies/JSON bodies; they are
 * never logged or serialized into telemetry.
 */

import { randomBytes } from "node:crypto";
import { performance } from "node:perf_hooks";
import { type BrowserMachineScope } from "./config";

export const SESSION_COOKIE_NAME = "rigscope_session";
/**
 * Remote HTTPS sessions use the `__Host-` prefix so the browser enforces a
 * Secure, host-only, Path=/ cookie. It is deliberately separate from the
 * local plaintext cookie: forwarding or copying the local cookie to a remote
 * origin cannot authenticate, and deleting one never touches the other.
 */
export const REMOTE_SESSION_COOKIE_NAME = "__Host-rigscope_session";
/** Fixed lifetime; not extended by activity. Deliberately a same-day value. */
export const DEFAULT_SESSION_TTL_MS = 12 * 60 * 60 * 1000;
/** Bounded store size; oldest abandoned sessions are evicted beyond this. */
export const DEFAULT_MAX_SESSIONS = 256;
/** Sweep cadence for fixed expiration; bounds how long dead sessions linger. */
export const SESSION_SWEEP_MS = 1_000;

export type BrowserSession = {
  /** Opaque random cookie token; also the store key. */
  id: string;
  username: string;
  /** Random per-session value the browser holds in memory for mutations. */
  csrfToken: string;
  /** Snapshot of the user's machine scope taken at sign-in. */
  scope: BrowserMachineScope;
  /** Snapshot for presentation only; authorization always rechecks the store. */
  admin?: boolean;
  createdAt: number;
  expiresAt: number;
};

export type SessionEndReason = "logout" | "expired" | "revoked";

export class BrowserSessionStore {
  private readonly sessionTtl: number;
  private readonly maxSessions: number;
  private readonly now: () => number;
  private readonly sessions = new Map<string, BrowserSession>();
  private readonly onEnd: (session: BrowserSession, reason: SessionEndReason) => void;
  private readonly sweepTimer: NodeJS.Timeout | null;

  constructor(input: {
    ttlMs?: number;
    maxSessions?: number;
    sweepMs?: number;
    now?: () => number;
    onEnd?: (session: BrowserSession, reason: SessionEndReason) => void;
  } = {}) {
    this.sessionTtl = input.ttlMs ?? DEFAULT_SESSION_TTL_MS;
    this.maxSessions = input.maxSessions ?? DEFAULT_MAX_SESSIONS;
    this.now = input.now ?? (() => performance.now());
    this.onEnd = input.onEnd ?? (() => {});
    if (!Number.isFinite(this.sessionTtl) || this.sessionTtl <= 0) {
      throw new Error("session TTL must be a positive number of milliseconds");
    }
    if (!Number.isInteger(this.maxSessions) || this.maxSessions < 1) {
      throw new Error("session store capacity must be a positive integer");
    }
    const sweepMs = input.sweepMs ?? SESSION_SWEEP_MS;
    this.sweepTimer =
      sweepMs > 0 && Number.isFinite(sweepMs)
        ? setInterval(() => this.sweepExpired(), sweepMs)
        : null;
    // Never keep the process alive just for cleanup; shutdown disposes this.
    this.sweepTimer?.unref();
  }

  /**
   * Creates a session for an authenticated user. Bounded: when the store is
   * full the oldest session (not the newest sign-in) is evicted first, and
   * eviction ends that browser's streams exactly like an expiry.
   */
  create(username: string, scope: BrowserMachineScope, admin = false): BrowserSession {
    const at = this.now();
    this.sweepExpired();
    if (this.sessions.size >= this.maxSessions) {
      const oldest = this.sessions.keys().next();
      if (oldest.done !== true) {
        const evicted = this.sessions.get(oldest.value);
        if (evicted) {
          this.sessions.delete(oldest.value);
          this.notify(evicted, "expired");
        }
      }
    }
    const session: BrowserSession = {
      id: randomToken(),
      username,
      csrfToken: randomToken(),
      scope: scope === "*" ? "*" : [...scope],
      createdAt: at,
      expiresAt: at + this.sessionTtl,
    };
    if (admin) session.admin = true;
    this.sessions.set(session.id, session);
    return session;
  }

  /** Looks a cookie token up; expired tokens are removed and never returned. */
  find(token: string): BrowserSession | null {
    if (typeof token !== "string" || token.length === 0) return null;
    const session = this.sessions.get(token);
    if (!session) return null;
    if (session.expiresAt <= this.now()) {
      this.sessions.delete(token);
      this.notify(session, "expired");
      return null;
    }
    return session;
  }

  /** Ends a session (logout); returns whether it existed and was removed. */
  end(token: string, reason: SessionEndReason = "logout"): boolean {
    if (typeof token !== "string" || token.length === 0) return false;
    const session = this.sessions.get(token);
    if (!session) return false;
    this.sessions.delete(token);
    this.notify(session, reason);
    return true;
  }

  /**
   * Revokes every session for one username. Each end goes through the same
   * onEnd path as logout/expiry, so the composition root closes that user's
   * SSE streams, HMR sockets and process interests. Returns the count.
   */
  endUser(username: string): number {
    if (typeof username !== "string" || username.length === 0) return 0;
    const tokens: string[] = [];
    for (const [token, session] of this.sessions) {
      if (session.username === username) tokens.push(token);
    }
    let ended = 0;
    for (const token of tokens) {
      if (this.end(token, "revoked")) ended += 1;
    }
    return ended;
  }

  /** Removes every fixed-expiration session that is now past its deadline. */
  sweepExpired(): number {
    if (this.sessions.size === 0) return 0;
    const at = this.now();
    let removed = 0;
    for (const [token, session] of this.sessions) {
      if (session.expiresAt > at) continue;
      this.sessions.delete(token);
      this.notify(session, "expired");
      removed += 1;
    }
    return removed;
  }

  size(): number {
    return this.sessions.size;
  }

  /** Fixed session lifetime in milliseconds (for cookie Max-Age windows). */
  ttlMs(): number {
    return this.sessionTtl;
  }

  dispose(): void {
    if (!this.sweepTimer) return;
    clearInterval(this.sweepTimer);
  }

  private notify(session: BrowserSession, reason: SessionEndReason): void {
    try {
      this.onEnd(session, reason);
    } catch (error) {
      // Listener failures must never leak into the request/session path.
      console.error("browser session end listener failed:", error);
    }
  }
}

/** 256 bits of cryptographic randomness, URL/base64-safe, no padding. */
function randomToken(): string {
  return randomBytes(32).toString("base64url");
}
