/**
 * Browser-safe administrative API client.
 *
 * The UI never invents authorization: every call returns 401/403/409/503 from
 * the hub and those failures are surfaced without retry or local elevation.
 * Mutations carry the session CSRF token and rely on the browser's same-origin
 * Origin header; no secret ever enters a URL, history entry or storage.
 */

export type AdminScope = "*" | string[];

export type AdminUserRecord = {
  username: string;
  machineIds: AdminScope;
  admin: boolean;
  disabled: boolean;
};

export type CollectorRecord = {
  collectorId: string;
  machineId: string;
  revoked: boolean;
  updatedAt: string;
};

export type AuditEvent = {
  actor: string;
  action: string;
  target: string;
  at: string;
};

export type EnrollmentGrant = {
  token: string;
  expiresAt: string;
};

export type SaveUserInput = {
  username: string;
  machineIds: AdminScope;
  admin: boolean;
  disabled?: boolean;
  password?: string;
};

export type MachineMetadataInput = {
  displayName: string | null;
  tags: string[];
};

export class AdminApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "AdminApiError";
    this.status = status;
  }
}

export function isAbortError(error: unknown): boolean {
  return (
    typeof error === "object" &&
    error !== null &&
    (error as { name?: unknown }).name === "AbortError"
  );
}

export function createAdminApi(options: { fetchImpl?: typeof fetch } = {}) {
  const request = options.fetchImpl ?? fetch;

  async function call<T>(
    path: string,
    init: {
      method?: "GET" | "POST" | "DELETE";
      body?: unknown;
      csrfToken?: string;
      signal?: AbortSignal;
    } = {},
  ): Promise<T> {
    const headers: Record<string, string> = { Accept: "application/json" };
    if (init.body !== undefined) headers["Content-Type"] = "application/json";
    if (init.csrfToken !== undefined) headers["X-CSRF-Token"] = init.csrfToken;
    const response = await request(path, {
      method: init.method ?? "GET",
      headers,
      body: init.body === undefined ? undefined : JSON.stringify(init.body),
      signal: init.signal,
      cache: "no-store",
      credentials: "same-origin",
      redirect: "error",
    });
    const text = await response.text();
    let data: unknown = null;
    if (text.length > 0) {
      try {
        data = JSON.parse(text) as unknown;
      } catch {
        data = null;
      }
    }
    if (!response.ok) {
      throw new AdminApiError(response.status, errorMessage(response.status, data));
    }
    return data as T;
  }

  return {
    async listUsers(signal?: AbortSignal): Promise<AdminUserRecord[]> {
      const data = await call<{ users?: unknown }>("/api/admin/users", { signal });
      if (!Array.isArray(data?.users)) throw unexpected();
      return data.users as AdminUserRecord[];
    },

    async saveUser(input: SaveUserInput, csrfToken: string, signal?: AbortSignal): Promise<void> {
      await call("/api/admin/users", { method: "POST", body: input, csrfToken, signal });
    },

    async deleteUser(username: string, csrfToken: string, signal?: AbortSignal): Promise<void> {
      await call(`/api/admin/users?username=${encodeURIComponent(username)}`, {
        method: "DELETE",
        csrfToken,
        signal,
      });
    },

    async listAudit(limit = 50, signal?: AbortSignal): Promise<AuditEvent[]> {
      const bounded = Number.isSafeInteger(limit) ? Math.min(Math.max(limit, 1), 1000) : 50;
      const data = await call<{ events?: unknown }>(`/api/admin/audit?limit=${bounded}`, {
        signal,
      });
      if (!Array.isArray(data?.events)) throw unexpected();
      return data.events as AuditEvent[];
    },

    async listCollectors(signal?: AbortSignal): Promise<CollectorRecord[]> {
      const data = await call<{ collectors?: unknown }>("/api/admin/collectors", { signal });
      if (!Array.isArray(data?.collectors)) throw unexpected();
      return data.collectors as CollectorRecord[];
    },

    async revokeCollector(
      collectorId: string,
      csrfToken: string,
      signal?: AbortSignal,
    ): Promise<void> {
      await call("/api/admin/collectors", {
        method: "POST",
        body: { action: "revoke", collectorId },
        csrfToken,
        signal,
      });
    },

    async createGrant(
      input: { replaceMachineId?: string; ttlMs?: number },
      csrfToken: string,
      signal?: AbortSignal,
    ): Promise<EnrollmentGrant> {
      const data = await call<{ token?: unknown; expiresAt?: unknown }>(
        "/api/admin/collector-grants",
        { method: "POST", body: input, csrfToken, signal },
      );
      if (typeof data?.token !== "string" || typeof data.expiresAt !== "string") throw unexpected();
      return { token: data.token, expiresAt: data.expiresAt };
    },

    async saveMachineMeta(
      machineId: string,
      input: MachineMetadataInput,
      csrfToken: string,
      signal?: AbortSignal,
    ): Promise<void> {
      await call(`/api/machines/meta?machineId=${encodeURIComponent(machineId)}`, {
        method: "POST",
        body: input,
        csrfToken,
        signal,
      });
    },
  };
}

export type AdminApi = ReturnType<typeof createAdminApi>;

function unexpected(): AdminApiError {
  return new AdminApiError(502, "The hub returned an unexpected response.");
}

function errorMessage(status: number, data: unknown): string {
  if (data !== null && typeof data === "object") {
    const message = (data as { error?: unknown }).error;
    if (typeof message === "string" && message.length > 0 && message.length <= 256) return message;
  }
  if (status === 400) return "The request was not valid.";
  if (status === 401) return "Your session ended. Sign in again.";
  if (status === 403) return "Administrative access is required.";
  if (status === 404) return "That record no longer exists.";
  if (status === 409) return "That change conflicts with the current directory.";
  if (status === 413) return "The request was too large.";
  if (status === 503) return "Administration is temporarily unavailable.";
  return "The hub could not complete that request.";
}
