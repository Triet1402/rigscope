/**
 * Authenticated recording API.
 *
 * `GET    /api/recordings`            list recordings owned by the caller (admins see all)
 * `POST   /api/recordings`            start a bounded capture
 * `GET    /api/recordings/:id`        status
 * `POST   /api/recordings/:id/stop`   stop and keep the file
 * `DELETE /api/recordings/:id`        remove metadata and file
 * `GET    /api/recordings/:id/download` stream the private JSON-lines file
 *
 * Mutation routes require the same CSRF/origin proof as other browser writes.
 * No request can name a filesystem path.
 */

import { createReadStream } from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import { timingSafeEqual } from "node:crypto";
import type { AuthOriginRules, BrowserHttpAuth } from "../auth/http";
import { isTrustedOrigin, sendJson } from "../auth/http";
import type { HubState } from "../state";
import { HttpInputError, readJsonBody } from "../http-input";
import {
  RecordingError,
  type RecordingInput,
  type RecordingManager,
  type RecordingResolution,
} from "./manager";
import { METRICS } from "../storage/series";

const RESOLUTIONS = new Set<RecordingResolution>(["1s", "10s", "1m", "5m", "1h"]);

export class RecordingHttp {
  constructor(
    private readonly options: {
      auth: BrowserHttpAuth;
      origin: AuthOriginRules;
      state: HubState;
      recordings: RecordingManager;
      isAdmin?: (username: string) => boolean;
    },
  ) {}

  async handle(req: IncomingMessage, res: ServerResponse, url: URL): Promise<boolean> {
    const path = url.pathname;
    if (path !== "/api/recordings" && !path.startsWith("/api/recordings/")) return false;
    const { auth, origin, state, recordings } = this.options;
    const session = auth.requestSession(req);
    if (!session) {
      sendJson(res, 401, { error: "authentication required" });
      return true;
    }
    const method = req.method ?? "GET";
    if (req.headers.origin !== undefined && !isTrustedOrigin(req, origin)) {
      sendJson(res, 403, { error: "origin not allowed" });
      return true;
    }
    if (method !== "GET") {
      const csrf = req.headers["x-csrf-token"];
      if (
        !isTrustedOrigin(req, origin) ||
        typeof csrf !== "string" ||
        Buffer.byteLength(csrf) !== Buffer.byteLength(session.csrfToken) ||
        !timingSafeEqual(Buffer.from(csrf), Buffer.from(session.csrfToken))
      ) {
        sendJson(res, 403, { error: "missing or invalid csrf token" });
        return true;
      }
    }
    const isAdmin = this.options.isAdmin?.(session.username) === true;
    const parts = path.slice("/api/recordings".length).split("/").filter(Boolean);
    const id = parts[0];
    const action = parts[1];

    try {
      if (id === undefined) {
        if (method === "GET") {
          const allowed = recordings
            .list(session.username, isAdmin)
            .filter((recording) => scopeAllows(recording.machines, session.scope));
          sendJson(res, 200, { recordings: allowed });
          return true;
        }
        if (method === "POST") {
          const body = await readJsonBody(req, 16 * 1024);
          const input = parseInput(body);
          this.authorizeMachines(input.machines, session.scope, state);
          sendJson(res, 201, recordings.start(session.username, input));
          return true;
        }
        sendJson(res, 405, { error: "method not allowed" });
        return true;
      }

      // An id route must not reveal or serve a recording that includes a
      // machine outside the caller's current scope, even if they created it.
      const visible = recordings.get(id, session.username, isAdmin);
      if (!scopeAllows(visible.machines, session.scope)) {
        sendJson(res, 403, { error: "machine access denied" });
        return true;
      }

      if (action === "download") {
        if (method !== "GET") {
          sendJson(res, 405, { error: "GET required" });
          return true;
        }
        const file = recordings.read(id, session.username, isAdmin);
        res.writeHead(200, {
          "Content-Type": "application/x-ndjson; charset=utf-8",
          // An active file can grow while it streams; use chunked framing then.
          ...(file.active ? {} : { "Content-Length": file.size }),
          "Content-Disposition": `attachment; filename="${file.name}"`,
          "Cache-Control": "no-store",
          "X-Content-Type-Options": "nosniff",
        });
        const stream = createReadStream(file.file);
        stream.on("error", () => res.destroy());
        stream.pipe(res);
        return true;
      }

      if (action === "stop") {
        if (method !== "POST") {
          sendJson(res, 405, { error: "POST required" });
          return true;
        }
        sendJson(res, 200, recordings.stop(id, session.username, isAdmin));
        return true;
      }

      if (action !== undefined) {
        sendJson(res, 404, { error: "recording action not found" });
        return true;
      }

      if (method === "GET") {
        sendJson(res, 200, recordings.get(id, session.username, isAdmin));
        return true;
      }
      if (method === "DELETE") {
        recordings.remove(id, session.username, isAdmin);
        sendJson(res, 200, { ok: true });
        return true;
      }
      sendJson(res, 405, { error: "method not allowed" });
      return true;
    } catch (error) {
      if (error instanceof HttpInputError) {
        sendJson(res, error.status, { error: error.message });
      } else if (error instanceof RecordingError) {
        sendJson(res, error.status, { error: error.message });
      } else {
        sendJson(res, 503, { error: "recording request temporarily unavailable" });
      }
      return true;
    }
  }

  private authorizeMachines(
    machines: readonly string[],
    scope: "*" | readonly string[],
    state: HubState,
  ): void {
    const known = new Set(state.snapshot().machines.map((machine) => machine.machineId));
    for (const machineId of machines) {
      if (scope !== "*" && !scope.includes(machineId)) {
        throw new RecordingError(403, "machine access denied");
      }
      if (!known.has(machineId)) {
        throw new RecordingError(404, `machine ${machineId} is not available to the hub`);
      }
    }
  }
}

function scopeAllows(machines: readonly string[], scope: "*" | readonly string[]): boolean {
  return scope === "*" || machines.every((machineId) => scope.includes(machineId));
}

function parseInput(value: unknown): RecordingInput {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new HttpInputError(400, "invalid recording request");
  }
  const body = value as Record<string, unknown>;
  const allowed = new Set([
    "machines",
    "metrics",
    "resolution",
    "durationMs",
    "maxBytes",
    "includeBacklog",
  ]);
  if (Object.keys(body).some((key) => !allowed.has(key)) || Object.keys(body).length === 0) {
    throw new HttpInputError(400, "invalid recording request");
  }
  if (!Array.isArray(body.machines) || body.machines.some((item) => typeof item !== "string")) {
    throw new HttpInputError(400, "machines must be an array of identifiers");
  }
  if (!Array.isArray(body.metrics) || body.metrics.some((item) => typeof item !== "string")) {
    throw new HttpInputError(400, "metrics must be an array of metric names");
  }
  for (const metric of body.metrics as string[]) {
    if (!Object.prototype.hasOwnProperty.call(METRICS, metric)) {
      throw new HttpInputError(400, `unknown metric ${JSON.stringify(metric)}`);
    }
  }
  if (typeof body.resolution !== "string" || !RESOLUTIONS.has(body.resolution as RecordingResolution)) {
    throw new HttpInputError(400, "resolution must be 1s, 10s, 1m, 5m or 1h");
  }
  return {
    machines: body.machines as string[],
    metrics: body.metrics as string[],
    resolution: body.resolution as RecordingResolution,
    ...(body.durationMs === undefined ? {} : { durationMs: numberField(body.durationMs, "durationMs") }),
    ...(body.maxBytes === undefined ? {} : { maxBytes: numberField(body.maxBytes, "maxBytes") }),
    ...(body.includeBacklog === undefined ? {} : { includeBacklog: body.includeBacklog === true }),
  };
}

function numberField(value: unknown, name: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value)) {
    throw new HttpInputError(400, `${name} must be a safe integer`);
  }
  return value;
}
