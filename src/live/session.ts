/**
 * Browser-side client for the hub's authentication endpoints, per the
 * milestone 3 backend agreement:
 *
 * - POST /api/auth/login {username,password} — the browser sends Origin
 *   automatically; success returns {username, csrfToken} and sets an
 *   HttpOnly SameSite session cookie.
 * - GET /api/auth/session — same success response, 401 when signed out,
 *   503 when the hub has no browser users configured.
 * - POST /api/auth/logout with the X-CSRF-Token header.
 * - Login configuration is hub-admin-only (HUB_BROWSER_USERS); collector
 *   credentials never appear in the browser.
 *
 * Pure fetch wrappers with no UI coupling and no React dependency.
 */

export type SessionPayload = { username: string; csrfToken: string; admin?: boolean };

export type SessionProbe =
  | { kind: "signed-in"; user: SessionPayload }
  | { kind: "signed-out" }
  | { kind: "auth-unavailable"; detail: string | null }
  | { kind: "unreachable" }
  | { kind: "error"; detail: string | null };

export type LoginOutcome =
  | { kind: "signed-in"; user: SessionPayload }
  | { kind: "invalid-credentials"; detail: string | null }
  | { kind: "auth-unavailable"; detail: string | null }
  | { kind: "unreachable" }
  | { kind: "error"; detail: string | null };

export type SignOutOutcome =
  | "ok"
  | "unreachable"
  | { kind: "error"; detail: string | null };

/** Reads a human-readable error detail from a JSON error body, if any. */
async function errorDetail(res: Response): Promise<string | null> {
  try {
    const body: unknown = await res.json();
    if (body && typeof body === "object" && "error" in body) {
      const message = (body as { error: unknown }).error;
      if (typeof message === "string") return message;
    }
  } catch {
    // Non-JSON body; fall back to the status text.
  }
  return res.statusText || null;
}

function parseSessionPayload(value: unknown): SessionPayload | null {
  if (!value || typeof value !== "object") return null;
  const username = (value as { username?: unknown }).username;
  const csrfToken = (value as { csrfToken?: unknown }).csrfToken;
  if (typeof username !== "string" || username.length === 0) return null;
  if (typeof csrfToken !== "string" || csrfToken.length === 0) return null;
  const admin = (value as { admin?: unknown }).admin;
  if (admin !== undefined && typeof admin !== 'boolean') return null;
  return { username, csrfToken, ...(admin === true ? { admin: true } : {}) };
}

/** GET /api/auth/session. 401 maps to signed-out; 503 to auth-unavailable. */
export async function fetchSession(): Promise<SessionProbe> {
  try {
    const res = await fetch("/api/auth/session", {
      cache: "no-store",
      signal: AbortSignal.timeout(5_000),
      headers: { Accept: "application/json" },
    });
    if (res.ok) {
      try {
        const user = parseSessionPayload(await res.json());
        if (user) return { kind: "signed-in", user };
      } catch {
        return { kind: "error", detail: "unexpected session response" };
      }
      return { kind: "error", detail: "unexpected session response" };
    }
    if (res.status === 401) return { kind: "signed-out" };
    if (res.status === 503) {
      return { kind: "auth-unavailable", detail: await errorDetail(res) };
    }
    return { kind: "error", detail: `unexpected status ${res.status}` };
  } catch {
    return { kind: "unreachable" };
  }
}

/** POST /api/auth/login. */
export async function login(username: string, password: string): Promise<LoginOutcome> {
  try {
    const res = await fetch("/api/auth/login", {
      method: "POST",
      cache: "no-store",
      signal: AbortSignal.timeout(5_000),
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify({ username, password }),
    });
    if (res.ok) {
      try {
        const user = parseSessionPayload(await res.json());
        if (user) return { kind: "signed-in", user: user as SessionPayload };
      } catch {
        return { kind: "error", detail: "unexpected sign-in response" };
      }
      return { kind: "error", detail: "unexpected sign-in response" };
    }
    if (res.status === 401) {
      return { kind: "invalid-credentials", detail: await errorDetail(res) };
    }
    if (res.status === 429) {
      return { kind: "error", detail: "too many attempts — wait a moment and try again" };
    }
    if (res.status === 503) {
      return { kind: "auth-unavailable", detail: await errorDetail(res) };
    }
    return { kind: "error", detail: `unexpected status ${res.status}` };
  } catch {
    return { kind: "unreachable" };
  }
}

/** POST /api/auth/logout with the CSRF header the agreement requires. */
export async function signOut(csrfToken: string): Promise<SignOutOutcome> {
  try {
    const res = await fetch("/api/auth/logout", {
      method: "POST",
      cache: "no-store",
      signal: AbortSignal.timeout(5_000),
      headers: { "X-CSRF-Token": csrfToken, Accept: "application/json" },
    });
    if (res.ok) return "ok";
    if (res.status === 401) return "ok"; // already signed out server-side
    if (res.status === 403) return { kind: "error", detail: `unexpected status ${res.status}` };
    return { kind: "error", detail: `unexpected status ${res.status}` };
  } catch {
    return "unreachable";
  }
}

// ---------------------------------------------------------------------------
// Live endpoint access probe
// ---------------------------------------------------------------------------

export type LiveAccess =
  | { kind: "ok" }
  | { kind: "unauthorized" }
  | { kind: "forbidden" }
  | { kind: "unreachable" }
  | { kind: "error"; detail: string | null };

/**
 * Gets just the status of GET /api/live without opening an EventSource:
 * a fetch resolves as soon as the status headers arrive, at which point
 * the body (an SSE stream) is cancelled. Used after connection errors to
 * distinguish "the account may not see this hub" (403) from a transient
 * network problem, and to avoid opening repeated streams during recovery.
 */
export async function probeLiveEndpoint(timeoutMs = 5_000): Promise<LiveAccess> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const res = await fetch("/api/live", {
      cache: "no-store",
      signal: controller.signal,
      headers: { Accept: "text/event-stream" },
    });
    if (res.status === 200) return { kind: "ok" };
    if (res.status === 401) return { kind: "unauthorized" };
    if (res.status === 403) return { kind: "forbidden" };
    return { kind: "error", detail: `unexpected status ${res.status}` };
  } catch {
    return { kind: "unreachable" };
  } finally {
    clearTimeout(timer);
    try {
      controller.abort(); // hanging body cleanup, never downs an open stream
    } catch {
      // ignore
    }
  }
}
