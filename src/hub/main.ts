/**
 * Hub entry point: one long-running Node process that owns a listener,
 * collector WebSocket upgrades, SSE subscriptions, browser authentication,
 * and the Next.js request path (development mode includes framework/HMR
 * upgrades).
 *
 * Executed via `tsx src/hub/main.ts` (dev) or `node dist/hub.cjs` (prod,
 * esbuild CJS bundle with external packages). Deliberately avoids
 * import.meta-based entry detection, which is incompatible with CJS output;
 * it expects to run from the project directory.
 *
 * Two listener modes, selected entirely by startup configuration:
 *
 * - Loopback TCP (default): literal loopback addresses only, unchanged local
 *   plaintext behavior.
 * - Remote Unix socket: HUB_PUBLIC_ORIGIN + HUB_SOCKET_PATH. The hub listens
 *   only on a private 0600 Unix socket for an outer TLS terminator. Every
 *   request must carry the exact canonical Host (and any Origin must match
 *   the canonical HTTPS origin) BEFORE enrollment, auth, health, SSE,
 *   collector, or Next.js handling. Forwarded headers are erased and replaced
 *   with hub-established values; they are never trusted as proof.
 *
 * Browser access fails closed in both modes: without HUB_BROWSER_USERS no
 * browser data is served, and there is never an anonymous fallback.
 */

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { Duplex } from "node:stream";
import { randomUUID } from "node:crypto";
import { dirname, join } from "node:path";
import { homedir } from "node:os";
import { openHubDatabase } from "./storage/db";
import { APPLICATION_VERSION } from "../runtime/version";
import { TelemetryStore } from "./storage/telemetry-store";
import { ProcessInterests } from "./process-interests";
import { MonitorHttp } from "./monitor-http";
import { CollectorRegistry } from "./enrollment/registry";
import { CollectorAdminHttp } from './enrollment/admin-http';
import { AdminStore } from './admin/store';
import { AdminHttp } from './admin/http';
import { AlertEngine } from './alerts/engine';
import { AlertsHttp } from './alerts/http';
import { acquireProcessLease } from '../runtime/process-lease';
import { CollectorGateway, isCollectorUpgradeRequest, COLLECTOR_CONNECT_PATH } from "./collector-ws";
import { SampleIngestion } from "./sample-ingestion";
import { parseHubConfig, type HubRemoteConfig } from "./config";
import { LiveFeed, type LiveAccess } from "./sse";
import { HubState } from "./state";
import { EnrollmentStore, type EnrollmentGrant } from "./enrollment/store";
import { ENROLL_PATH, handleEnrollment } from "./enrollment/http";
import { loopbackOriginHostnames, sendJson, isTrustedOrigin, type AuthOriginRules } from "./auth/http";
import { parseBrowserUsers, parseDevelopmentSessionTtl } from "./auth/config";
import { BrowserHttpAuth } from "./auth/http";
import { BrowserSessionStore } from "./auth/sessions";
import {
  checkRemoteRequest,
  normalizeForwardedHeaders,
  rejectUpgradeSocket,
  type BoundaryFailure,
  type RemoteBoundary,
} from "./proxy/boundary";
import {
  prepareUnixSocketPath,
  takeUnixSocketOwnership,
  removeOwnedUnixSocket,
  abandonUnixSocket,
  type UnixSocketIdentity,
} from "./proxy/unix-socket";

const SHUTDOWN_DEADLINE_MS = 5_000;
const HEALTH_PATH = "/api/health";
const LIVE_PATH = "/api/live";
const MAX_FRAMEWORK_SOCKETS_PER_SESSION = 8;
const MAX_FRAMEWORK_SOCKETS = 64;
/** Byte budget for the bootstrap grant JSON document (not a machine count). */
const MAX_ENROLLMENT_GRANTS_BYTES = 64 * 1024;

/**
 * UI paths served to unauthenticated browsers without redirecting: the
 * login screen itself and static assets (framework chunks, icons, favicon).
 * Everything else a browser may fetch is authenticated; protected data
 * APIs and RSC payloads get 401 instead of a redirect.
 */
const PUBLIC_UNAUTH_PATHS = new Set([
  "/login",
  "/favicon.ico",
  "/robots.txt",
  "/manifest.json",
  "/site.webmanifest",
]);
const PUBLIC_UNAUTH_PREFIXES = ["/_next/static/", "/icon", "/apple-icon", "/manifest"];

/**
 * Validators for same-origin browser mutations in local loopback mode. The
 * hub must be addressed exactly on this loopback host/port; a Host header
 * belongs to an allowlisted loopback hostname or the request is not trusted.
 *
 * With HUB_PORT=0 (isolated runtime fixtures) the actual bound port is only
 * known after listen; the rules object is corrected in the listen callback
 * before any browser request is served.
 */
function sameOriginRules(host: string, port: number): AuthOriginRules {
  return { mode: "loopback", originPort: port, originHostnames: loopbackOriginHostnames(host) };
}

function remoteOriginRules(remote: HubRemoteConfig): AuthOriginRules {
  return { mode: "remote", publicOrigin: remote.publicOrigin, hostHeader: remote.hostHeader };
}

function remoteBoundaryOf(remote: HubRemoteConfig): RemoteBoundary {
  return { publicOrigin: remote.publicOrigin, hostHeader: remote.hostHeader };
}

function isPublicUnauthPath(pathname: string): boolean {
  if (PUBLIC_UNAUTH_PATHS.has(pathname)) return true;
  return PUBLIC_UNAUTH_PREFIXES.some((prefix) => pathname.startsWith(prefix));
}

/** Methods that can change state and therefore need exact browser Origin in remote mode. */
function isMutationMethod(method: string): boolean {
  return method !== "GET" && method !== "HEAD" && method !== "OPTIONS";
}

/** RSC/data-ish requests must never be redirected; they get 401 JSON. */
function isProtectedDataRequest(req: IncomingMessage): boolean {
  const headers = req.headers;
  if (headers.rsc === "1") return true;
  if (headers["next-router-state-tree"] !== undefined) return true;
  const accept = headers.accept;
  return typeof accept === "string" && accept.includes("text/x-component");
}

function sendBoundaryFailure(res: ServerResponse, failure: BoundaryFailure): void {
  res.writeHead(failure.status, {
    "Content-Type": "application/json; charset=utf-8",
    "Cache-Control": "no-store",
    Connection: "close",
  });
  res.end(JSON.stringify({ error: failure.error }));
}

main().catch((error: unknown) => {
  console.error(
    `hub fatal on startup: ${error instanceof Error ? (error.stack ?? error.message) : error}`,
  );
  process.exit(1);
});

async function main(): Promise<void> {
  let stopping = false;
  // Framework (HMR/dev) upgraded sockets are tracked by browser session so
  // logout, expiry, eviction, and shutdown terminate them immediately. An
  // upgraded socket must not outlive the authorization that admitted it.
  const frameworkUpgradeSockets = new Map<string, Set<Duplex>>();
  let remoteSocketIdentity: UnixSocketIdentity | null = null;

  const closeFrameworkSession = (sessionId: string): void => {
    const sockets = frameworkUpgradeSockets.get(sessionId);
    if (sockets === undefined) return;
    frameworkUpgradeSockets.delete(sessionId);
    for (const socket of sockets) {
      try {
        socket.destroy();
      } catch {
        // already gone
      }
    }
  };
  const closeAllFrameworkSockets = (): void => {
    for (const sessionId of [...frameworkUpgradeSockets.keys()]) {
      closeFrameworkSession(sessionId);
    }
  };
  const trackFrameworkSocket = (sessionId: string, socket: Duplex): void => {
    let sockets = frameworkUpgradeSockets.get(sessionId);
    if (sockets === undefined) {
      sockets = new Set<Duplex>();
      frameworkUpgradeSockets.set(sessionId, sockets);
    }
    sockets.add(socket);
    const release = (): void => {
      const current = frameworkUpgradeSockets.get(sessionId);
      if (current === undefined) return;
      current.delete(socket);
      if (current.size === 0) frameworkUpgradeSockets.delete(sessionId);
    };
    socket.once("close", release);
    socket.once("error", release);
  };

  const shutdown = async (signal: string): Promise<void> => {
    if (stopping) return;
    stopping = true;
    console.log(`hub received ${signal}: shutting down`);
    const deadline = setTimeout(() => {
      console.error(`hub shutdown exceeded ${SHUTDOWN_DEADLINE_MS}ms; forcing exit`);
      if (config.remote !== null) {
        try {
          removeOwnedUnixSocket(config.remote.socketPath, remoteSocketIdentity);
        } catch {
          // A stale socket still fails closed on the next startup.
        }
      }
      process.exit(1);
    }, SHUTDOWN_DEADLINE_MS);
    gateway.stop();
    clearInterval(alertTimer);
    alerts.stop();
    processInterests.stop();
    feed.stop();
    closeAllFrameworkSockets();
    const httpClosed = new Promise<void>((resolve) => server.close(() => resolve()));
    // Drop remaining keep-alive/idle HTTP and SSE-adjacent connections; SSE
    // responses were ended above and sockets close with the server.
    server.closeIdleConnections?.();
    server.closeAllConnections?.();
    const results = await Promise.allSettled([httpClosed, nextApp.close(), storage.close(), alerts.drain(2000)]);
    database.close();
    releaseDatabaseLease();
    if (config.remote !== null) {
      try {
        removeOwnedUnixSocket(config.remote.socketPath, remoteSocketIdentity);
      } catch (error) {
        console.error(
          `hub could not remove its Unix socket: ${error instanceof Error ? error.message : error}`,
        );
      }
    }
    clearTimeout(deadline);
    sessions.dispose();
    process.exit(results.some((result) => result.status === "rejected") ? 1 : 0);
  };

  // Validate configuration before preparing Next.js or opening any socket,
  // so a bad configuration exits nonzero without ever binding.
  const config = parseHubConfig(process.env);
  // Synchronous best-effort cleanup also covers fatal JS errors/process.exit.
  // SIGKILL cannot run handlers; stale sockets still require operator recovery.
  if (config.remote !== null) {
    const socketPath = config.remote.socketPath;
    process.once("exit", () => {
      try {
        removeOwnedUnixSocket(socketPath, remoteSocketIdentity);
      } catch {
        // Never obscure the original exit reason.
      }
    });
  }
  const authOrigin = config.remote === null
    ? sameOriginRules(config.host, config.port)
    : remoteOriginRules(config.remote);
  const remoteBoundary = config.remote === null ? null : remoteBoundaryOf(config.remote);
  // Fail closed: no HUB_BROWSER_USERS means no browser sessions at all —
  // login/session report 503 and /api/live is unavailable.
  const browserUsers = parseBrowserUsers(process.env);
  const sessionTtlMs = parseDevelopmentSessionTtl(process.env);
  const isDevelopment = config.development;
  // Production is the default for the built entry; keep NODE_ENV in sync
  // before the (dynamic) next import loads it.
  Object.assign(process.env, { NODE_ENV: isDevelopment ? "development" : "production" });

  // Remote mode never opens a TCP listener. Verify its private directory and
  // unchosen socket path before the more expensive Next.js preparation.
  if (config.remote !== null) {
    prepareUnixSocketPath(config.remote.socketPath);
  }

  const hubRunId = randomUUID();
  const stateFile = process.env.HUB_STATE_FILE;
  if (process.env.HUB_ENROLLMENT_GRANTS && !stateFile) throw new Error("HUB_STATE_FILE is required for enrollment");
  let grants: EnrollmentGrant[] = [];
  try {
    const raw = process.env.HUB_ENROLLMENT_GRANTS || '[]';
    if (Buffer.byteLength(raw, 'utf8') > MAX_ENROLLMENT_GRANTS_BYTES) throw new Error();
    const value: unknown = JSON.parse(raw);
    if (!Array.isArray(value)) throw new Error();
    grants = value;
  } catch { throw new Error("HUB_ENROLLMENT_GRANTS must be an array of grants within its size budget"); }
  const legacyEnrollment = stateFile ? new EnrollmentStore({path:stateFile, identities:config.collectors, grants}) : undefined;
  const databasePath = process.env.HUB_DATABASE_PATH || join(stateFile ? dirname(stateFile) :
    join(process.env.XDG_STATE_HOME || join(homedir(), '.local', 'state'), 'rigscope', 'hub'), 'telemetry.sqlite');
  const releaseDatabaseLease = databasePath === ':memory:' ? () => {} : acquireProcessLease(`${databasePath}.hub.lock`);
  const database = openHubDatabase(databasePath);
  const registry = new CollectorRegistry({ database, legacyIdentities: legacyEnrollment?.identities() ?? config.collectors,
    consumedGrantHashes: legacyEnrollment?.consumedGrantHashes(), grants });
  const state = new HubState({ collectors: registry.identities(true), hubRunId });
  state.synchronizeIdentities(registry.identities());
  const enrollment = stateFile ? registry : undefined;
  const storage = new TelemetryStore({ database, maxRecordsPerTransaction: 32,
    maxDatabaseBytes: 2 * 1024 * 1024 * 1024 });
  const ingestion = new SampleIngestion({ state, storage,
    onLiveSample: (machineId, record, receivedAtMs) => alerts.observe(machineId, record, receivedAtMs) });
  ingestion.restorePersisted();
  const feed = new LiveFeed(state);
  // Session logout and fixed expiration both end that session's open SSE
  // streams and framework upgrades immediately, even while a stream sits idle.
  const sessions = new BrowserSessionStore({
    ttlMs: sessionTtlMs,
    onEnd: (session) => {
      feed.endSessionStreams(session.id);
      closeFrameworkSession(session.id);
      processInterests.endSession(session.id);
    },
  });
  let previousUsers = browserUsers;
  const auth = new BrowserHttpAuth({ users: browserUsers, sessions, origin: authOrigin });
  const admin = new AdminStore({ database, bootstrapUsers: browserUsers,
    // One-time migration of the existing explicitly named owner account.
    // Other accounts require an explicit admin flag or HUB_ADMIN_USERS.
    bootstrapAdminUsernames: process.env.HUB_ADMIN_USERS === undefined
      ? browserUsers.filter(user => user.username === 'owner').map(user => user.username)
      : process.env.HUB_ADMIN_USERS.split(',').map(name => name.trim()).filter(Boolean),
    onUsersChanged: users => {
      auth.replaceUsers(users);
      for (const user of previousUsers) if (JSON.stringify(user) !== JSON.stringify(users.find(next => next.username === user.username))) sessions.endUser(user.username);
      previousUsers = users;
    } });
  auth.replaceUsers(admin.users());
  previousUsers = admin.users();
  const adminHttp = new AdminHttp({ store: admin, auth, origin: authOrigin,
    extraAudit: () => registry.auditEvents(),
    onChanged: usernames => { for (const username of usernames) sessions.endUser(username); } });
  const collectorAdminHttp = new CollectorAdminHttp({ registry, auth, origin: authOrigin,
    isAdmin: username => admin.isAdmin(username), changed: () => {
      state.synchronizeIdentities(registry.identities()); gateway.revokeInvalidConnections();
    } });
  const alerts = new AlertEngine({ database });
  const alertsHttp = new AlertsHttp({ engine: alerts, auth, origin: authOrigin, isAdmin: username => admin.isAdmin(username) });
  const alertTimer = setInterval(() => alerts.tick(state.snapshot().machines), 1000); alertTimer.unref();
  const processInterests = new ProcessInterests((machineId, message) => gateway.sendToMachine(machineId, message));
  const gateway = new CollectorGateway({ state, ingestion, maxConnections: config.maxCollectorConnections,
    onDetail: (machineId, message) => processInterests.receive(machineId, message),
    onDisconnect: machineId => processInterests.disconnect(machineId) });
  const monitorHttp = new MonitorHttp({ auth, origin: authOrigin, state, database, storage, processes: processInterests,
    isAdmin: username => admin.isAdmin(username) });

  const { default: createNextApp } = await import("next");
  const nextApp = createNextApp({
    dev: isDevelopment,
    // Next 16 defaults its custom dev server to Turbopack. Keep the webpack
    // backend in development so the authenticated /_next/hmr upgrade
    // path and the rest of the pre-16 dev behavior stay unchanged.
    webpack: isDevelopment,
    hostname: config.remote === null ? config.host : config.remote.hostname,
    port: config.remote === null ? config.port : config.remote.port,
    dir: process.cwd(),
  });
  await nextApp.prepare();
  const nextRequestHandler = nextApp.getRequestHandler();
  const nextUpgradeHandler = nextApp.getUpgradeHandler();

  const server: Server = createServer((req, res) => {
    // Remote boundary first: before auth, enrollment, health, SSE, the
    // collector endpoint, or Next.js. Loopback mode skips this entirely.
    if (remoteBoundary !== null) {
      const failure = checkRemoteRequest(req, remoteBoundary);
      if (failure !== null) {
        sendBoundaryFailure(res, failure);
        return;
      }
      normalizeForwardedHeaders(req, remoteBoundary);
    }
    dispatch(req, res).catch((error: unknown) => {
      console.error("hub request handler failed:", error);
      if (!res.headersSent) {
        res.writeHead(500, { "Content-Type": "text/plain; charset=utf-8" });
      }
      res.end("hub error");
    });
  });

  // This is the only "upgrade" listener on the server, so upgrades are never
  // consumed or double-handled: the collector path is handled here and every
  // framework upgrade (including Next.js dev HMR) is delegated to Next.js.
  server.on("upgrade", (req, socket, head) => {
    if (remoteBoundary !== null) {
      const failure = checkRemoteRequest(req, remoteBoundary);
      if (failure !== null) {
        rejectUpgradeSocket(socket, failure.status, failure.error);
        return;
      }
      normalizeForwardedHeaders(req, remoteBoundary);
    }
    if (isCollectorUpgradeRequest(req)) {
      // The collector endpoint rejects every Origin itself, including the
      // hub's own canonical browser origin.
      gateway.handleUpgrade(req, socket, head);
      return;
    }
    // Production serves no framework upgrades. The packaged hub only needs
    // this path in development (Next.js HMR/websockets). Remote mode makes
    // that explicit; local production preserves its existing delegation.
    if (remoteBoundary !== null && !isDevelopment) {
      rejectUpgradeSocket(socket, 403, "framework upgrades are disabled");
      return;
    }
    // Delegated framework upgrades are browser sessions too, not anonymous
    // infrastructure: require a validated session AND an exactly same-origin
    // Origin header (cookie-less probes, missing origins, and foreign origins
    // are destroyed before Next.js sees the socket).
    const session = auth.requestSession(req);
    if (session === null || !isTrustedOrigin(req, authOrigin)) {
      if (remoteBoundary !== null) {
        rejectUpgradeSocket(socket, 403, "upgrade not authorized");
      } else {
        console.error("hub rejected unauthorized or non-loopback upgrade");
        socket.destroy();
      }
      return;
    }
    // Track before the authorization may change; the mapping is torn down on
    // logout/expiry/shutdown so an HMR socket cannot live indefinitely.
    const totalFrameworkSockets = [...frameworkUpgradeSockets.values()]
      .reduce((count, sockets) => count + sockets.size, 0);
    if ((frameworkUpgradeSockets.get(session.id)?.size ?? 0) >= MAX_FRAMEWORK_SOCKETS_PER_SESSION
      || totalFrameworkSockets >= MAX_FRAMEWORK_SOCKETS) {
      rejectUpgradeSocket(socket, 429, "too many framework upgrades");
      return;
    }
    trackFrameworkSocket(session.id, socket);
    if (sessions.find(session.id) === null) {
      socket.destroy();
      return;
    }
    Promise.resolve(nextUpgradeHandler(req, socket, head)).catch((error: unknown) => {
      console.error("hub upgrade delegation failed:", error);
      socket.destroy();
    });
  });

  const dispatch = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    let pathname = "/";
    try {
      pathname = new URL(req.url ?? "/", "http://hub.invalid").pathname;
    } catch {
      res.writeHead(400, { "Content-Type": "text/plain; charset=utf-8" });
      res.end("bad request");
      return;
    }
    const method = req.method ?? "GET";

    // Remote browser mutations must prove the exact configured HTTPS origin.
    // Collector enrollment carries a grant instead of a browser Origin (and
    // rejects any Origin itself), and the collector endpoint is upgrade-only,
    // so both stay outside this browser rule. The boundary already rejected
    // foreign/duplicate Origin values for every request.
    if (
      remoteBoundary !== null &&
      isMutationMethod(method) &&
      pathname !== ENROLL_PATH &&
      pathname !== COLLECTOR_CONNECT_PATH &&
      !isTrustedOrigin(req, authOrigin)
    ) {
      sendJson(res, 403, { error: "origin not allowed" });
      return;
    }

    if (pathname === ENROLL_PATH) {
      await handleEnrollment(req, res, enrollment, () => {
        state.synchronizeIdentities(registry.identities());
        gateway.revokeInvalidConnections();
      });
      return;
    }

    if (pathname.startsWith("/api/auth/")) {
      const handled = await auth.handleApiAuth(req, res, pathname);
      if (handled) return;
    }

    if (pathname === HEALTH_PATH) {
      if (method !== "GET" && method !== "HEAD") {
        res.writeHead(405, { "Content-Type": "text/plain; charset=utf-8", Allow: "GET, HEAD" });
        res.end("method not allowed");
        return;
      }
      res.writeHead(200, {
        "Content-Type": "application/json; charset=utf-8",
        "Cache-Control": "no-store",
      });
      res.end(method === "HEAD" ? undefined : JSON.stringify({ status: "ok", version: APPLICATION_VERSION, mode: "runtime-spike", hubRunId }));
      return;
    }

    if (pathname === LIVE_PATH) {
      if (req.headers.origin !== undefined && !isTrustedOrigin(req, authOrigin)) {
        sendJson(res, 403, { error: "origin not allowed" });
        return;
      }
      // Authentication strictly BEFORE any headers: unauthenticated browsers
      // get fail-closed JSON status codes, never an event stream.
      if (!auth.configured) {
        sendJson(res, 503, { error: "browser access is not configured on this hub" });
        return;
      }
      const session = auth.requestSession(req);
      if (!session) {
        sendJson(res, 401, { error: "authentication required" });
        return;
      }
      if (session.scope !== "*" && session.scope.length === 0) {
        sendJson(res, 403, { error: "no machine access is granted to this account" });
        return;
      }
      if (method !== "GET") {
        res.writeHead(405, { "Content-Type": "text/plain; charset=utf-8", Allow: "GET" });
        res.end("method not allowed");
        return;
      }
      const access: LiveAccess = {
        sessionId: session.id,
        scope: session.scope === "*" ? "*" : new Set(session.scope),
      };
      feed.attach(req, res, access);
      return;
    }

    if (pathname === COLLECTOR_CONNECT_PATH) {
      // HTTP access to the collector endpoint gets 426.
      gateway.handleHttpRequest(req, res);
      return;
    }

    const apiUrl = new URL(req.url ?? '/', 'http://hub.invalid');
    if (await collectorAdminHttp.handle(req, res, apiUrl)) return;
    if (await adminHttp.handle(req, res, apiUrl)) return;
    if (await alertsHttp.handle(req, res, apiUrl)) return;
    if (await monitorHttp.handle(req, res, apiUrl)) return;

    // Remaining requests belong to Next.js (UI + future routes): they are
    // delegated only to browsers with a validated session, to an allowlist
    // of public paths (login page/static assets), or 401/redirect otherwise.
    if (!auth.requestSession(req)) {
      if (method === "GET" || method === "HEAD") {
        if (isPublicUnauthPath(pathname)) {
          await nextRequestHandler(req, res);
          return;
        }
        if (!pathname.startsWith("/api/") && !isProtectedDataRequest(req)) {
          res.writeHead(302, {
            Location: "/login",
            "Content-Type": "text/plain; charset=utf-8",
            "Cache-Control": "no-store",
          });
          res.end("sign in to continue");
          return;
        }
      }
      sendJson(res, 401, { error: "authentication required" });
      return;
    }

    // Unknown HTTP requests delegate to Next.js (existing UI + future routes).
    await nextRequestHandler(req, res);
  };

  server.on("error", (error) => {
    console.error(`hub fatal: ${error instanceof Error ? error.message : error}`);
    process.exit(1);
  });

  // Startup errors (including Next prepare failures) exit nonzero above.
  if (config.remote !== null) {
    const remote = config.remote;
    server.listen(remote.socketPath, () => {
      try {
        remoteSocketIdentity = takeUnixSocketOwnership(remote.socketPath);
      } catch (error) {
        console.error(
          `hub fatal: ${error instanceof Error ? error.message : error}`,
        );
        // We only get here after a preflight that required the path to be
        // absent, so this exact socket file was created by this process.
        abandonUnixSocket(remote.socketPath);
        server.close(() => process.exit(1));
        return;
      }
      // Never print an http:// TCP URL in remote mode; the only reachable
      // listener is the private Unix socket, published through HTTPS.
      console.log(
        `hub listening on unix:${remote.socketPath} (public origin ${remote.publicOrigin})`,
      );
    });
  } else {
    server.listen(config.port, config.host, () => {
      const address = server.address();
      const port = typeof address === "object" && address !== null ? address.port : config.port;
      // Same-origin validation must use the actually bound port: HUB_PORT=0
      // binds an ephemeral port, and login/logout require an exact match.
      if (authOrigin.mode !== "remote") authOrigin.originPort = port;
      // Bracket IPv6 literals so the printed URL is directly usable.
      const displayHost = config.host.includes(":") ? `[${config.host}]` : config.host;
      console.log(`hub listening on http://${displayHost}:${port}`);
    });
  }

  process.on("SIGINT", () => void shutdown("SIGINT"));
  process.on("SIGTERM", () => void shutdown("SIGTERM"));
  process.on("uncaughtException", (error) => {
    console.error("hub uncaught exception:", error);
    process.exit(1);
  });
  process.on("unhandledRejection", (reason) => {
    console.error("hub unhandled rejection:", reason);
    process.exit(1);
  });
}
