# Rigscope browser API v1

Status: 1.0 contract. `internal/hub` implements it. This document, the Go
types in `internal/api`, the generated TypeScript in
`src/contracts/api.generated.ts`, the captured responses in `testdata/api` and
the Go-encoded fixtures in `testdata/api/reencoded` describe the same contract.

Conventions used below:

- **Session** means a valid `rigscope_session` (plaintext loopback) or
  `__Host-rigscope_session` (remote HTTPS) cookie.
- **Admin** means a session whose username is an active administrator row in
  the persistent user directory (`AdminStore.isAdmin`), looked up on every
  request. A wildcard machine scope is *not* administrative access.
- **CSRF** means an `X-CSRF-Token` header whose value is the session's
  `csrfToken`, compared in constant time.
- Every JSON error body is `{"error": string}` with an optional `"code"`
  field. The only code in use is `"process-viewer-budget"` on a 429 from
  `POST /api/processes`. 1.0 standardizes the text of the common 403 bodies
  (`authentication required`, `administrative access required`,
  `origin not allowed`, `missing or invalid csrf token`); collector
  administration keeps its specific `origin or csrf token not allowed`
  message. Response headers are `Content-Type: application/json; charset=utf-8`
  and `Cache-Control: no-store` unless stated otherwise.
- Body limits are byte limits on the raw request body, checked before parsing.
  All request bodies must be JSON objects.
- Type names refer to `internal/api` / `api.generated.ts` types.

## Route inventory

28 method+path routes are in the 1.0 contract (25 JSON/SSE operations, two
`HEAD` variants of `GET` routes, and the collector WebSocket upgrade).
Recording routes exist in the Node hub today but are **removed in 1.0** and are
not modelled; see "Removed: recordings".

| Method | Path | Access |
| --- | --- | --- |
| GET, HEAD | `/api/health` | none |
| GET | `/api/live` | session, non-empty machine scope |
| POST | `/api/auth/login` | none (Origin required) |
| GET, HEAD | `/api/auth/session` | none (cookie read only) |
| POST | `/api/auth/logout` | session + CSRF + Origin |
| POST | `/api/collector/enroll` | enrollment grant bearer token |
| GET | `/api/collector/connect` | collector bearer token, WebSocket upgrade only |
| GET | `/api/admin/users` | admin |
| POST | `/api/admin/users` | admin + CSRF + Origin |
| DELETE | `/api/admin/users?username=` | admin + CSRF + Origin |
| GET | `/api/admin/audit?limit=` | admin |
| GET | `/api/admin/collectors` | admin |
| POST | `/api/admin/collectors` | admin + CSRF + Origin |
| POST | `/api/admin/collector-grants` | admin + CSRF + Origin |
| GET | `/api/admin/diagnostics/alerts` | admin |
| GET | `/api/admin/settings` | admin |
| GET | `/api/alerts?limit=` | session |
| POST | `/api/alerts/rules` | admin + CSRF + Origin |
| DELETE | `/api/alerts/rules?id=` | admin + CSRF + Origin |
| POST | `/api/alerts/webhook` | admin + CSRF + Origin |
| GET | `/api/history/status` | session |
| GET | `/api/history/metrics?machineId=` | session + machine scope |
| GET | `/api/history?machineId=...` | session + machine scope |
| POST | `/api/processes?machineId=&viewerId=` | session + CSRF + Origin + machine scope |
| GET | `/api/processes?machineId=&viewerId=` | session + machine scope |
| DELETE | `/api/processes?machineId=&viewerId=` | session + CSRF + Origin + machine scope |
| POST | `/api/machines/meta?machineId=` | admin + CSRF + Origin + machine scope |

Not used by the UI: `/api/health` (CLI, installer, reverse proxy),
`/api/collector/enroll` (collector/CLI), `/api/collector/connect` (collector).
`HEAD` variants are not used by the UI. Every other route is called by the
React client.

## Cookies, sessions and CSRF

### Cookie

| Mode | Cookie name | Attributes |
| --- | --- | --- |
| Loopback plaintext (`HUB_HOST`/`HUB_PORT`) | `rigscope_session` | `HttpOnly; SameSite=Strict; Path=/; Max-Age=<ttl>` |
| Remote Unix socket + HTTPS origin | `__Host-rigscope_session` | `Secure; HttpOnly; SameSite=Strict; Path=/; Max-Age=<ttl>` |

The cookie value is an opaque random session token. `Max-Age` is the fixed
remaining lifetime at sign-in (default 43 200 s = 12 h). Logout and rejected
logout clear it with `Max-Age=0` and the same attributes.

### Session semantics

- The 0.2 Node reference hub kept sessions in process memory only, so a hub
  restart signed every browser out. The 1.0 Go hub stores sessions in SQLite
  keyed by token hash, with the same fixed lifetime and the same per-failure
  cookie rules; this is the one intended session difference. See "1.0
  decisions".
- Lifetime is fixed at creation (12 h default); activity does not extend it.
- At most 256 sessions; creating one beyond the cap evicts the oldest session.
- A session snapshots the user's machine scope and admin flag at sign-in.
  Authorization-sensitive routes re-check the admin directory on every request.
- Logout, expiry, administrator change and user deletion revoke sessions. That
  immediately ends the session's SSE streams, development framework upgrades
  and process-detail interests, and releases collector detail leases.
- `POST /api/auth/logout` is session-authenticated (`AuthSession`): a missing
  session is 401 and clears the cookie; an Origin/CSRF rejection is 403 and
  leaves the cookie untouched.
- Sign-in performs the same scrypt work for unknown usernames. Failures are
  generic (`invalid username or password`).

### Origin rules

`isTrustedOrigin` is the only trusted origin check. `X-Forwarded-*` and
`Forwarded` are never trusted.

- **Loopback mode**: the `Origin` header must parse to `http://<allowlisted
  loopback hostname>:<bound port>` (port 80 may be implicit), with no path,
  query, fragment or credentials, and its host:port must equal the `Host`
  header exactly. Allowlisted hostnames are the bind host, `localhost`,
  `127.0.0.1` and IPv6 loopback literals.
- **Remote mode**: every request (before auth, health, enrollment, SSE or the
  collector endpoint) must carry the exact canonical `Host` header; a missing
  or duplicate Host is 400, a mismatched Host is 421. Any `Origin` present must
  be byte-identical to the configured canonical HTTPS origin, otherwise 403.
  Mutating methods additionally require that exact Origin (`/api/collector/enroll`
  and `/api/collector/connect` are exempt; both reject Origin themselves).

### Mutation proof

- `POST /api/auth/login` and `POST /api/auth/logout` require a trusted Origin.
  Login has no CSRF token yet; logout additionally requires the session CSRF
  token.
- Authenticated browser mutations (`history` GETs are exempt, every POST/
  DELETE below) require a trusted Origin **and** `X-CSRF-Token`.
- Failure shapes differ today (see "Inconsistencies"): most handlers return
  403 `{"error":"missing or invalid csrf token"}`, collector administration
  returns 403 `{"error":"origin or csrf token not allowed"}`, and
  `/api/admin/users`, `/api/admin/collector-grants` and `/api/admin/collectors`
  return 403 `{"error":"administrative access required"}` for either an origin
  or CSRF failure.
- The UI always sends the Origin implicitly (same-origin `fetch`) and the
  header explicitly.

## Authentication endpoints

### POST /api/auth/login

Access: none; requires a trusted Origin. Body limit 4 KiB. Exactly
`{"username","password"}` (both strings; extra or missing keys are 400).

| Status | Body | Meaning |
| --- | --- | --- |
| 200 | `SessionResponse` | signed in; sets the session cookie |
| 400 | error | invalid JSON, wrong shape, or invalid credential field types/lengths |
| 401 | error | invalid username or password |
| 403 | error | `origin not allowed` |
| 405 | error | `POST required` (`Allow: POST`) |
| 413 | error | body above 4 KiB |
| 415 | error | content type is not `application/json...` |
| 429 | error | hub-wide login budget (burst 10, one recovered per 500 ms) or 4 concurrent logins |
| 503 | error | no browser users configured |

Success: `SessionResponse {username, csrfToken, admin?}`. `admin` is present
and `true` only for administrators; it is omitted for ordinary users (never
`false`). Side effects: creates a session, sets the cookie.

The request reader has a 5 s deadline. Unknown usernames cost one password
verification. Field bounds: username ≤ 128 characters, password 1..1024
characters (UTF-16 code units).

### GET /api/auth/session

Access: none (reads the cookie only). Returns 200 `SessionResponse`, 401
`{"error":"authentication required"}`, 503 when browser access is not
configured, or 405 for other methods. `HEAD` returns headers only.

### POST /api/auth/logout

Access: session cookie + CSRF + trusted Origin.

| Status | Body | Meaning |
| --- | --- | --- |
| 200 | `{"ok":true}` | session ended; cookie cleared |
| 401 | error | no valid session; cookie is still cleared |
| 403 | error | origin or CSRF failure |
| 405 | error | `POST required` |
| 503 | error | no browser users configured |

Side effects: removes the session, ends its SSE streams/process interests, and
releases collector detail work. Other sessions of the same user are untouched.

Cookie clearing is per failure: a 401 (no valid session) clears the cookie so a
stale credential cannot loop, while an Origin/CSRF 403 does not clear it
because the request might be a cross-site forgery rather than a real logout.

### Other `/api/auth/*`

Any other path under `/api/auth/` returns 404 `{"error":"unknown auth endpoint"}`.

## Health

### GET|HEAD /api/health

Access: none. This route is not used by the UI.

- 200 `HealthResponse`: `{"status":"ok","version":"1.0.0","mode":"go","hubRunId":"<uuid>"}`.
  The 0.2 Node hub reported `"runtime-spike"`; the 1.0 Go hub reports `"go"`
  and the captured corpus is normalized to that value.
- Non-GET/HEAD: 405 with `Content-Type: text/plain; charset=utf-8`,
  `Allow: GET, HEAD`, body `method not allowed`. This is the one JSON route
  whose method error is not JSON.
- `HEAD` sends headers and no body.

This endpoint is deliberately unauthenticated for local health checks and
proxies. It exposes only process-level facts.

## Live stream (SSE)

### GET /api/live

Access: session; machine scope must be non-empty.

Pre-checks, in order:

| Status | Body | Meaning |
| --- | --- | --- |
| 403 | error | an `Origin` header is present and not trusted (`origin not allowed`) |
| 503 | error | browser access is not configured |
| 401 | error | no valid session |
| 403 | error | session scope is empty (`no machine access is granted to this account`) |
| 405 | text | method is not `GET` (`Allow: GET`) |
| 503 | error | more than 64 live clients (`too many live clients`) |
| 503 | error | hub shutting down |

Success headers:

```
Content-Type: text/event-stream; charset=utf-8
Cache-Control: no-cache, no-store, no-transform
Connection: keep-alive
X-Accel-Buffering: no
```

Frames (one blank-line-terminated block each):

```
id: <hubRunId>:<cursor>
event: snapshot|reset|update
data: <HubSnapshot JSON>
```

- The first frame is `snapshot` when the request has no `Last-Event-ID` (or an
  empty one); it is `reset` when any non-empty `Last-Event-ID` is present. The
  hub **never replays**: a reset always carries a full current snapshot.
- `update` carries a complete replacement `HubSnapshot`, not a delta.
- `id` is `<hubRunId>:<opaque decimal cursor>`. The cursor is a random 128-bit
  starting value incremented only for that viewer's delivered changes. It is
  not comparable across viewers or hub runs.
- The scope filter runs before serialization: a scoped session never receives
  another machine's row, hostname, collector identity or metrics.
- Identical consecutive snapshots are skipped for a viewer. State changes are
  coalesced for 500 ms; updates are full snapshots, so coalescing loses no
  information.
- Keepalive comments (`: keepalive <epoch-ms>`) are written every 15 s.
- A client that falls more than 1 MiB behind is disconnected; it must
  resynchronize by reconnecting.
- Logout, session expiry, user revocation and hub shutdown end the response.
  The browser treats end-of-stream as a reason to reconnect or re-probe.
- The UI's `EventSource` controller closes and reopens connections itself; the
  browser's automatic retry is not relied upon, and reopening always produces
  `snapshot` (no `Last-Event-ID` is sent by the UI).

`HubSnapshot`:

```json
{
  "hubRunId": "uuid",
  "machines": [
    {
      "machineId": "id",
      "collectorId": "id",
      "hostname": "string",
      "connected": true,
      "lastReceivedAt": "ISO-8601 or null",
      "expectedIntervalMs": 1000,
      "metrics": { "...MetricsRecord": true, "kind": "metrics", "receivedAt": "ISO-8601" },
      "inventory": { "...protocol Inventory": true },
      "delivery": { "...protocol StatusMessage": true },
      "displayName": null,
      "tags": ["lab"]
    }
  ]
}
```

- `machines` is sorted by `machineId`; IDs are unique.
- `expectedIntervalMs`, `metrics`, `inventory`, `delivery`, `displayName` and
  `tags` are optional keys (absent means "no value yet"). `displayName` and
  `expectedIntervalMs` may additionally be present as `null`.
  `expectedIntervalMs` is bounded 100..7 200 000 ms and is never invented.
- `metrics` is the collector's last metrics record plus the hub's
  `receivedAt`; its `collectorId` must match the row and `receivedAt` must
  equal `lastReceivedAt`.
- `inventory`, `delivery`, metrics payloads and embedded process snapshots are
  the protocol v1 shapes documented in `docs/rewrite/protocol-v1.md`.

## Collector endpoints

These are not browser endpoints. They are listed here because they share the
hub's HTTP listener and the enrollment response is part of the same JSON
contract. Collector WebSocket messages are out of scope for this document.

### POST /api/collector/enroll

Access: `Authorization: Bearer <enrollment-grant>`; no browser session.
Rejects any `Origin` header with 403. Body limit 1 KiB; `Content-Type` must be
exactly `application/json` (parameters are rejected, unlike the auth routes).

Request: `{"collectorId":"id","requestId?":"id","recoverySecret?":"base64url"}`
(`collectorId` and `requestId` are 1..128 printable ASCII; `recoverySecret` is
32..128 base64url characters; the recovery pair is both-or-neither).

Success 201 `EnrollmentIdentity {collectorId, machineId, token}`. A retry with
the same grant and the same `requestId`/`recoverySecret` returns the same
credential for up to 24 h; this is how a lost enrollment response is recovered.
`token` is the collector's long-lived hub credential and must be treated as a
secret.

Errors: 429 (budget: burst 30, one per 200 ms, 16 concurrent),
405 `POST required`, 404 `{"error":"enrollment disabled"}` when the hub has no
enrollment store, 403 `origin not allowed`, 401 `{"error":"invalid enrollment
grant"}`, 415 `JSON required`, 413 `body too large`, 400 `invalid enrollment
request`, 403 `enrollment denied` (unknown/expired/reused grant, duplicate
collector without a replacement grant), 503 `enrollment storage unavailable`.
All enrollment responses include `Connection: close`.

### GET /api/collector/connect

WebSocket upgrade endpoint. A plain HTTP request gets 426 with
`Content-Type: text/plain`, `Upgrade: websocket`, `Connection: close`, and a
human-readable body. Collector credentials are sent in the `Authorization`
header; every `Origin` header is rejected. Not part of the browser contract.

## Administration

All admin routes require a session and an active administrator. If browser
access is not configured: 503. Missing session: 401 `authentication required`.
Non-admin: 403 `administrative access required`.

### GET /api/admin/users

Returns 200 `AdminUserList {users:[AdminUser]}` where
`AdminUser {username, machineIds, admin, disabled}` and `machineIds` is `"*"`
or an array of machine IDs. Password hashes are never returned.

Errors: 401, 403, 503.

### POST /api/admin/users

Admin + CSRF + Origin. Body limit 8 KiB. Required
`{username, machineIds, admin}`; optional `{disabled?, password?}`. `username`
is 1..128 printable ASCII; `machineIds` is `"*"` or a duplicate-free array of
IDs; `password` is 1..1024 characters when present.

- 200 `{"ok":true}`. Side effects: creates or updates the user, hashes a new
  password, writes an audit row, reloads the auth directory, and revokes every
  session of the affected username. An administrator who changes their own
  account is signed out by this.
- 400 invalid body or identifier, 409 would remove/demote/disable the last
  active administrator, 401/403 as above, 413 oversized body, 415 non-JSON,
  503 storage unavailable.

### DELETE /api/admin/users?username=<username>

Admin + CSRF + Origin. Exactly one `username` query parameter (1..128
printable ASCII).

- 200 `{"ok":true}`. Side effects: deletes the user, writes an audit row,
  revokes that user's sessions.
- 400 missing/duplicate/invalid username, 404 unknown user, 409 last active
  administrator, 401/403/503.

### GET /api/admin/audit?limit=<n>

Admin. `limit` is a positive integer, at most one value, default 100, clamped
to 1000. Returns 200 `AuditEventList {events:[AuditEvent]}` newest first, where
`AuditEvent {actor, action, target, at}`. Rows contain no passwords, hashes or
request bodies. Collector registry events are merged in.

Errors: 400 (`at most one limit is allowed`, `limit must be a positive
integer`), 405 `GET required`, 401/403/503.

### GET /api/admin/collectors

Admin. Returns 200 `CollectorList {collectors:[CollectorRecord]}` where
`CollectorRecord {collectorId, machineId, revoked, updatedAt}`. Rows outside
the session's machine scope are filtered out. Collector bearer credentials are
never returned.

Errors: 401, 403, 503.

### POST /api/admin/collectors

Admin + CSRF + Origin. Body limit 2 KiB. The body must be exactly
`{"action":"revoke","collectorId":"id"}`; no other action exists.

- 200 `{"ok":true}`. Side effects: marks the collector revoked, writes an
  audit row, closes its live WebSocket session, and removes it from live state.
- 400 malformed body, 403 collector outside the session scope, 405 any other
  method, 401/403/503.

### POST /api/admin/collector-grants

Admin + CSRF + Origin. Body limit 2 KiB. Optional
`{"replaceMachineId?": id, "ttlMs?": integer}`; no other keys.

- 201 `GrantResponse {token, expiresAt}`. `token` is a short-lived, single-use
  bootstrap credential; it is the only credential ever shown to a browser.
  Default TTL 900 000 ms; explicit TTL is bounded 60 000..900 000 ms. At most
  256 unexpired grants. With `replaceMachineId`, the browser must have access to
  that machine; without it, the session scope must be `"*"`.
- 400 invalid body/TTL, 403 machine access denied, 409 grant budget exhausted
  (`cap-reached`), 401/403/503.

### GET /api/admin/diagnostics/alerts

Admin. Returns 200 `AlertDiagnostics` describing alert evaluation losses that
can change rule semantics:

```json
{
  "generatedAt": "ISO-8601",
  "droppedObservations": 0,
  "observationsAbandoned": 0,
  "continuityResets": 0,
  "continuityResetFailures": 0,
  "unresolvedBreaks": [],
  "queueDepth": 0,
  "queueCapacity": 256,
  "lastTickAt": "ISO-8601" | null
}
```

`droppedObservations` counts LIVE records the bounded observation queue
refused; `continuityResets` counts machines whose sustained-duration state was
reset after such a gap; `continuityResetFailures` counts reset writes that
failed and are still being retried; `unresolvedBreaks` lists the machines whose
reset has not committed. `unresolvedBreaks` is filtered through the validated
administrator session's machine scope: a scoped administrator sees only its own
machines, while a wildcard scope sees every id. The global counters are not
per-machine and remain visible. `queueDepth`/`queueCapacity` describe the
current queue, and `lastTickAt` is the evaluation clock of the most recent
committed liveness tick. `observationsAbandoned` counts LIVE record evaluations
not performed because shutdown had begun, including records still queued when
the worker stopped. The payload never contains webhook private state.

`rigscope doctor` has no authenticated hub HTTP channel, so it does not query
this endpoint; operators can read it with an administrator session.

### GET /api/admin/settings

Admin, read-only. Returns 200 `HubSettings`: the settings the running hub
started with.

```json
{
  "generatedAt": "ISO-8601",
  "scope": "user" | "system",
  "storage": {
    "mode": "memory" | "persistent",
    "diskBytes": 0,
    "diskLimitBytes": 0 | null,
    "memoryBytes": 0,
    "memoryBudgetBytes": 0,
    "oldestHistoryMs": 0 | null
  },
  "network": {
    "url": "http://127.0.0.1:8081",
    "listen": "127.0.0.1",
    "port": 8081,
    "origin": "https://hub.example.com" | null,
    "tls": false,
    "trustedProxies": [],
    "forwardedHeaders": "x-forwarded" | "forwarded"
  },
  "localCollector": {
    "samplingIntervalMs": 2000,
    "destination": "ws://127.0.0.1:8081/api/collector/connect" | null,
    "shared": ["cpu"],
    "withheld": ["process-arguments"],
    "gpuRegisters": false
  } | null
}
```

`diskBytes` is the hub database plus WAL; `memoryBytes` is RAM history (all of
it in memory mode, the one-second ring in persistent mode). `shared` and
`withheld` use the effective policy from the collector's saved configuration,
reloaded on each request. Collector settings take effect when it starts; they
do not prove that the collector is currently running. Hub network settings
describe the running hub. `localCollector` is null when the saved configuration
disables local collection or the administrator is scoped to specific machines. The payload never contains TLS file paths, keys, grants or
credentials. There is no write method (405): the unprivileged hub cannot
rewrite its configuration or restart itself, so changes are made with
`rigscope configure` on the hub machine.

Errors: 405 `GET required`, 401/403/503.

## Alerts

Alert rules are machine-scoped. `resourceId` is `""` for metrics without a
resource and a resource identifier (for example `gpu:GPU-…`, `core:3`, `fs:/`,
`disk:sda`, `net:eno1`) for resource metrics. Metric names and units follow the
storage catalog (`KnownMetrics`, 33 names). Thresholds are on the metric's
native scale (percent is 0–100).

### GET /api/alerts?limit=<n>

Session. `limit` is an integer 1..500, default 200. Returns 200
`AlertListResponse`:

```json
{
  "generatedAt": "ISO-8601",
  "canAdmin": true,
  "rules": [AlertRule],
  "incidents": [AlertIncident],
  "notifications": AlertNotificationSummary,
  "webhook": AlertWebhookStatus,
  "limits": AlertLimits,
  "deliveryDisclosure": "Webhook delivery is at-least-once. ..."
}
```

Rules and incidents are filtered to the session's machine scope. The webhook
status never contains the URL, headers or secret.

Errors: 400 (`limit must be an integer from 1 to 500`), 401, 405, 503.

### POST /api/alerts/rules

Admin + CSRF + Origin. Body limit 16 KiB. Required
`{name, machineId, metric, condition, durationMs}`; optional
`{id?, resourceId?, threshold?, enabled?, maintenanceUntil?}`; no other keys.
`id` present updates, absent creates. `name` 1..128; `machineId` 1..128;
`metric` one of the 33 storage metrics; `condition` is `above|below|stale|
unavailable`; `durationMs` 0..86 400 000; `threshold` is a finite number or
`null` and is required for `above`/`below`; `maintenanceUntil` is an ISO
timestamp or `null`.

- 201 `AlertRuleResponse {rule}` on create, 200 `AlertRuleResponse {rule}` on
  update. Side effects: persists the rule and its evaluation state.
- 400 invalid body, 403 non-admin / machine outside scope / origin / CSRF,
  404 rule not found on update, 415 non-JSON, 503 storage unavailable,
  405 other methods (`Allow: POST, DELETE`).

`AlertRule` fields: `id, name, machineId, metric, resourceId, condition,
threshold (number|null), durationMs, enabled, maintenanceUntil (string|null),
state ("ok"|"pending"|"firing"), pendingSince (string|null), activeIncidentId
(string|null), lastValue (number|null), lastObservedAt (string|null),
createdAt, updatedAt`.

### DELETE /api/alerts/rules?id=<rule-id>

Admin + CSRF + Origin. Exactly one `id`, 1..128 characters.

- 200 `{"ok":true}`.
- 400 missing/duplicate/oversized id, 403 rule outside scope, 404 unknown rule,
  401/403/503.

### POST /api/alerts/webhook

Admin + CSRF + Origin. Body limit 8 KiB. Discriminated by `action`:

| Body | Response |
| --- | --- |
| `{"action":"configure","url":"...", "headers?": {...}, "secret?": string|null, "enabled?": bool}` | 200 `AlertWebhookResponse {status}` |
| `{"action":"clear"}` | 200 `AlertWebhookResponse {status}` |
| `{"action":"test"}` | 200 `AlertWebhookTestResponse {result, status}` |

`configure` requires `url` (1..2048 characters; `https://` or loopback
`http://`, no embedded credentials or fragment), at most 16 headers with names
1..128 characters using the HTTP token grammar and values 0..1024 characters,
and a secret of at most 256 characters. `host`, `content-length`,
`content-type` and `idempotency-key` header names are reserved. `clear` and
`test` reject every other field. `test` delivers to a configured loopback HTTP
sink only; an HTTPS webhook is tested by the background delivery job instead.

Errors: 400 invalid body/unknown action, 415 non-JSON, 401/403/503.

### Other `/api/alerts/*`

After the session check and the admin+CSRF mutation check, an unknown path
returns 404 `{"error":"unknown alerts endpoint"}`. This means a non-admin
receives 403 before learning that the path does not exist.

## History

History data is bounded. Queries never zero-fill: a bucket that only saw
explicit unavailable readings carries `null` min/max/avg, and empty buckets are
omitted.

### GET /api/history/status

Session. Returns 200 `HistoryStatus`:

```json
{
  "mode": "memory" | "persistent",
  "persistentHistory": false,
  "availableFromMs": 1794470876011,
  "availableToMs": 1794470876011,
  "memory": MemoryHistoryStatus | null,
  "persistence": PersistentHistoryStatus | null,
  "gaps": 0,
  "acceptedRecords": 1,
  "generatedAt": "ISO-8601"
}
```

Exactly one of `memory` and `persistence` is non-null. Tier entries are
`{tier: "1s"|"raw"|"10s"|"1m"|"1h", stepMs, retentionMs, bytes}`.

Errors: 401, 403 (bad Origin when one is present), 405, 503.

### GET /api/history/metrics?machineId=<id>

Session + machine scope. `machineId` must exist in the live state.

Returns 200 `MetricCatalogResponse {metrics: { "<metric>": {unit} }}` with all
33 storage metrics and their units (`percent`, `bytes`, `bytes_per_second`,
`celsius`, `watts`, `milliseconds`).

Errors: 403 (`machine access denied` for an out-of-scope or empty/oversized
machineId), 404 `machine not found`, 405, 503.

This endpoint confirms which metrics the hub records; the UI also has its own
presentation catalog.

### GET /api/history?machineId=...&metrics=...&from=...&to=...

Query parameters:

| Parameter | Required | Rules |
| --- | --- | --- |
| `machineId` | yes | 1..128 characters, in session scope, must exist |
| `metrics` | yes | comma-separated, 1..33 known names, no duplicates; default if omitted is `cpu.usage,memory.used` |
| `resourceIds` | no | comma-separated, at most 64, each 1..256, no duplicates |
| `coreIds` | no | comma-separated, at most 1024, each `core:<0..1023>`, no duplicates; restricts only `cpu.core` |
| `from` | yes | epoch milliseconds, safe integer |
| `to` | yes | epoch milliseconds, safe integer; `from < to`; range ≤ 400 days |
| `maxPoints` | no | positive integer, default 600; larger values are clamped to 2000, not rejected |

Returns 200 `HistoryResult`:

```json
{
  "machineId": "id",
  "fromMs": 0,
  "toMs": 0,
  "stepMs": 1000,
  "tier": "1s",
  "series": [
    {"metric": "cpu.usage", "resourceId": "", "unit": "percent",
     "points": [{"t": 0, "min": 0, "max": 1.19, "avg": 0.2975}]}
  ],
  "gaps": [{"fromMs": 0, "toMs": 0, "kind": "missing",
            "reason?": "string", "droppedRecords?": 1}],
  "truncated": false,
  "elapsedMs": 3.2
}
```

Scan budgets: at most 25 000 rows and 100 ms per query; at most 64 series
(1024 when `cpu.core` is requested). Exceeding a budget sets `truncated`.
Queries are rate-limited per session with a token bucket of 8 requests and one
refill per 250 ms, shared by all of a session's tabs; the budget map is bounded
to 256 sessions.

`internal/api.ParseHistoryQuery` mirrors these parsing rules and returns a
`HistoryQuery`; the hub should call it instead of re-deriving the bounds. The
HTTP layer applies the same 128-character `machineId` bound to every machine
route before this parser runs.

Errors: 429 `Too many history requests. Pause another History tab, then
refresh.`, 400 invalid query (`HistoryQueryError`), 403 machine access denied,
404 machine not found, 405, 503 (`monitoring request temporarily unavailable`).
Only `GET` is accepted for the three history routes.

## Machine metadata

### POST /api/machines/meta?machineId=<id>

Admin + CSRF + Origin; the session must include the machine, and the machine
must exist. Body limit 4 KiB; at least one field is required; no other keys.

`{"displayName": string|null, "tags": [string]}`. `displayName` is ≤ 128
characters and must not contain control characters. A request may send up to
32 tags, each 1..64 characters after trimming and free of control characters;
the hub trims each tag and drops duplicates in first-seen order, exactly like
the Node handler (`[" lab ", "lab"]` stores `["lab"]`). The stored response is
strict: tags are trimmed, non-empty and unique. Absent keys keep the stored
value; an explicit `null` clears `displayName`; `tags` must be an array (not
null).

- 200 `MachineMeta {displayName, tags}` with the stored result (tags
  deduplicated).
- 400 invalid body, 403 non-admin / origin / CSRF, 404 unknown machine,
  405 (`POST required`), 503.

Side effect: persists metadata immediately and publishes a live-state change
(SSE `update`). `GET /api/machines/meta` is not supported (405).

## Process detail

Process detail is ephemeral and scope-checked. Every request takes
`machineId` and `viewerId` query parameters; `viewerId` must match
`^[a-zA-Z0-9-]{1,128}$`. A viewer lease is 15 s; the browser renews every ~5 s
and polls at the returned interval (~1 s). One session may hold at most 8
viewers and the hub at most 256.

Common pre-checks for all three methods: session 401, bad Origin when present
403, method outside GET/POST/DELETE 405, invalid `viewerId` 400, machine
outside scope or missing `machineId` 403, machine not in live state 404,
storage/interests failure 503.

### POST /api/processes

Session + CSRF + Origin. Acquires or renews a viewer lease.

- 200 `ProcessAcquireResponse {leaseMs: 15000, intervalMs: 1000}`.
- 409 `collector not connected for Processes` when the machine is offline.
- 403 with the capability reason when the collector reports processes as
  unsupported or withheld.
- 429 `{"error":"...","code":"process-viewer-budget"}` when the per-session or
  hub viewer budget is exhausted.

Side effect: subscribes the collector to a process-detail lease (collectors
receive `detail.subscribe`); equivalent viewers on one machine share it.

### GET /api/processes

Session only (no CSRF). Reads the latest snapshot for the viewer.

- 200 `ProcessReadResponse {state, result}`; `result` is `null` while loading
  or after the interest ended. `state` is one of `loading`, `ready`, `expired`,
  `cancelled`, `withheld`, `unsupported`, `busy`, `failed`, `disconnected`.
- 410 `{"error":"process interest expired; renew to continue"}` when the
  viewer has no live interest (the UI re-acquires).

`result` is the protocol `detail.result` object
(`{type:"detail.result", subscriptionId, capturedAt, snapshot:{totalProcesses,
truncated, inaccessible, processes:[...]}}`).

### DELETE /api/processes

Session + CSRF + Origin. Releases the viewer.

- 200 `{"ok":true}` (even when the interest already expired).
- 405 for other methods.

Side effect: when no viewer remains for a machine, sends `detail.cancel` to
the collector and drops the snapshot (no persistence or idle cache).

## Removed: recordings

Recordings are deferred and their endpoints are **removed in 1.0**. The Node
hub still serves them and `src/components/history/recording-panel.tsx` still
calls them today; the Phase 2 UI removes the controls. They are intentionally
absent from `internal/api`, the generated TypeScript and the fixtures.

| Method | Path | Node behaviour |
| --- | --- | --- |
| GET | `/api/recordings` | list the caller's recordings (admins see all), scope-filtered |
| POST | `/api/recordings` | start a bounded capture, 201 |
| GET | `/api/recordings/:id` | metadata |
| POST | `/api/recordings/:id/stop` | stop and keep the file |
| DELETE | `/api/recordings/:id` | remove metadata and file |
| GET | `/api/recordings/:id/download` | private NDJSON file download |

A Go hub must not implement these routes; the browser must not call them.

## Rows that are not part of the JSON API

- Requests to unknown `/api/*` paths do not get a JSON 404. Unauthenticated
  requests get 401 `{"error":"authentication required"}`; authenticated
  requests fall through to the static UI, which typically serves its 404
  document. Do not build clients that depend on a JSON 404 for an unknown API
  path.
- Non-API UI paths redirect unauthenticated `GET`/`HEAD` requests to `/login`
  (302) except a small public allowlist (`/login`, favicon, robots, manifest,
  framework static chunks). RSC/data requests get 401 JSON instead. This is UI
  behaviour, not part of `internal/api`.
- Development framework upgrades (`/_next/hmr`, etc.) require a session and an
  exact same-origin `Origin`; at most 8 per session and 64 total. Production
  serves no framework upgrades.

## Inconsistencies and ambiguities in the current API

These were real differences in the 0.2 Node reference hub. The "1.0 decisions"
table below records, for each one, whether the Go hub fixes it, preserves the
observable behaviour, changes the session backend, or removes the feature.

1. **CSRF failure messages.** Most routes say `missing or invalid csrf token`;
   collector admin says `origin or csrf token not allowed`; user admin and
   collector admin failures are reported as `administrative access required`.
   Status is 403 in all cases.
2. **Method-not-allowed bodies.** JSON endpoints use `{"error": ...}` with
   varied `Allow` values; `/api/health` and `/api/collector/connect` use
   `text/plain`.
3. **Content-Type strictness.** Login/logout/admin use `startsWith(
   "application/json")`; collector enrollment requires the exact string
   `application/json` and rejects parameters.
4. **Alert unknown-path ordering.** `/api/alerts/<unknown>` first requires a
   session, then admin+CSRF, and only then returns 404.
5. **`POST /api/processes` concurrency and leases.** The hub does not document
   which viewer wins when two viewers renew the same machine; equivalent
   viewers share a collector lease, and `state` strings are not formally
   enumerated in the response type.
6. **`POST /api/admin/users` self-revocation.** Changing your own account
   ends your session but still returns 200 with the committed change; the UI
   must tolerate being signed out on success.
7. **Machine-ID bounds differ internally.** The HTTP layer bounds every
   `machineId` query parameter to 128 characters; the storage query module
   alone accepts 256. The observable contract is 128.
8. **`elapsedMs` precision.** It is a rounded `performance.now()` delta and
   has no documented upper bound; the Go type uses a float.
9. **`maxPoints`.** Values above 2000 are silently clamped rather than
   rejected, while a non-positive value is a 400.
10. **`HistoryResult.series` may omit a series** when a metric has no points,
    and `resourceId` is `""` for the machine-level series. Consumers must not
    assume one series per requested metric.
11. **`/api/history/metrics` uses the full metric catalog** regardless of
    which machines exist; only the `machineId` existence/scope check applies.
12. **Alert webhook URL validation** is delegated to the alert engine, not the
    HTTP body validator; an invalid URL can be a 400 with an engine-specific
    message.
13. **`storageTier`** is a free string (`raw` or `summary`) in the response;
    only those two are currently emitted.
14. **Timestamps** are ISO-8601 strings produced by `Date.toISOString()`.
    The API contract accepts the protocol's broader ISO grammar; consumers
    should parse any RFC 3339 value.
15. **`expectedIntervalMs` is null before a hello** but always present as a
    key on snapshots from current hubs; hand-built fixtures may omit it.
16. **Health `mode` is `"runtime-spike"`**, a leftover implementation label,
    not a meaningful product mode.
17. **Enrollment with a consumed grant returns 403 `enrollment denied`** for
    every failure (wrong recovery proof, expired recovery window, unknown
    collector). There is no distinct `recoverable` signal.
18. **Sessions are in-memory and fixed-lifetime**, so a hub restart invalidates
    every browser session and every open SSE stream. There is no refresh or
    sliding expiry.
19. **`/api/live` accepts Origin-less requests**, unlike mutations. A
    non-browser client with a stolen cookie can open a stream without an
    Origin; the session scope still applies.
20. **Recording routes still exist.** They are the only endpoints in the
    current hub that the 1.0 target removes; the UI still uses them in this
    branch.

## 1.0 decisions

Each row resolves one of the inconsistencies above. "FIX" changes the Go hub's
observable contract intentionally; "PRESERVE" keeps Node behaviour exactly;
"SESSIONS" and "RECORDINGS" record deliberate product decisions.

| Area | Decision | Notes |
| --- | --- | --- |
| 403 error bodies (1) | FIX | Standardize on `authentication required`, `administrative access required`, `origin not allowed` and `missing or invalid csrf token`. Collector administration keeps its specific `origin or csrf token not allowed` message. `internal/api` exposes the standard message constants. |
| Method-not-allowed bodies (2) | FIX | Every JSON route returns a JSON 405 with an `Allow` header. `/api/health` keeps the plain-text 405 body, and `/api/collector/connect` keeps 426 for a non-upgrade request. |
| Content-Type parsing (3) | FIX | All JSON routes parse the media type the same way: `application/json` with optional parameters and case-insensitive type/subtype. Collector enrollment no longer demands the byte-exact string. |
| Alert unknown-path ordering (4) | FIX | Authenticate first, then return the JSON 404 `unknown alerts endpoint`; a non-admin still receives 403 before path discovery. |
| Process leases (5) | PRESERVE | Equivalent viewers share one collector lease; the response `state` is the explicit `ProcessState` union. |
| Self-revocation (6) | PRESERVE | An administrator changing their own account is signed out by the successful 200 response. |
| Machine-ID bounds (7) | PRESERVE | Every HTTP machine identifier is 1..128 characters; the storage layer's wider internal limit is not observable. |
| `elapsedMs` (8) | PRESERVE | Float milliseconds with no documented upper bound; non-negative and finite. |
| `maxPoints` (9) | PRESERVE | Values above 2000 are clamped, not rejected; a non-positive value is a 400. |
| Sparse series (10) | PRESERVE | A requested metric may produce no series; consumers never assume one series per metric. |
| Full metric catalog (11) | PRESERVE | `/api/history/metrics` returns all 33 metrics after only the machine scope/existence check. |
| Webhook URL validation (12) | PRESERVE | The alert engine validates the URL before persistence and reports its own bounded 400 message. |
| `storageTier` (13) | FIX | Typed enum `StorageTier = "raw" | "summary"` in the generated TypeScript; no other value decodes. |
| Timestamp grammar (14) | FIX | One explicit output grammar: RFC 3339 with a `Z` or numeric offset inside the protocol's 2000..2200 window. The Go decoders accept the same grammar for inputs. |
| `expectedIntervalMs` (15) | PRESERVE | Optional key; present `null` means the cadence is unknown before a hello, and a present number is bounded 100..7,200,000 ms. |
| Health `mode` (16) | FIX | The 1.0 Go hub reports `"go"`; the captured corpus is normalized from Node's `"runtime-spike"`. |
| Enrollment denial (17) | PRESERVE | Every consumed/unknown/expired grant failure is the generic 403 `enrollment denied`. |
| Sessions (18) | SESSIONS | The Go hub stores sessions durably in SQLite keyed by token hash with a fixed expiry. A hub restart no longer signs browsers out; this is a deliberate change from Node. |
| Origin-less SSE (19) | PRESERVE | A missing `Origin` is accepted for same-origin SSE reads, while a present foreign `Origin` is rejected. Mutations still require the trusted `Origin`. |
| Recordings (20) | RECORDINGS | Recordings are removed in 1.0. The endpoints are absent from `internal/api` and the generated TypeScript, and the UI controls are removed before switching to the Go hub. |

## Validation and CI

- `internal/api` strictly decodes every captured fixture in `testdata/api`
  (unknown fields, missing fields, required keys, nullability, enum values,
  bounds), re-encodes it, and compares the JSON value. Fixtures are also
  mutated one required key or non-nullable value at a time; each mutation must
  be rejected.
- `internal/api/gen` regenerates `src/contracts/api.generated.ts` and
  `src/contracts/api.validators.generated.ts` twice and fails when either
  committed file is stale. Types marked `//api:skip` are Go-only helpers
  (query and error values) and are deliberately not generated.
- `go generate ./internal/api` refreshes the TypeScript and
  `testdata/api/reencoded`, the Go-encoded fixture corpus. A drift test fails
  when either output is stale.
- `tests/api-contract.test.ts` validates every captured fixture and every
  Go-encoded fixture with the generated validators; SSE payloads are also
  parsed with the browser's `parseHubSnapshot`.
- The captured corpus in `testdata/api` was recorded from the 0.2 Node hub
  during the port and is kept as a frozen compatibility check. Intentional 1.0
  changes are represented by the Go-encoded fixtures under
  `testdata/api/reencoded`, which `go generate ./internal/api` refreshes. The
  Node capture script is deleted with the Node hub; a contract change is made
  in `internal/api` and regenerated, not recaptured from Node.
