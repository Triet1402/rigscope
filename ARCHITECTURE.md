# Rigscope architecture

Rigscope has one hub application process and one collector per monitored machine. This is a custom-server deployment with a production Next.js UI, not `next start` or a serverless application.

## Topology and ownership

- Install one collector per monitored machine.
- Each collector opens one outbound connection to each configured hub.
- A hub receives many collector connections and serves many authenticated browsers.
- Browsers connect to a hub; they need no direct access to collectors.
- Monitor the hub's machine by installing a collector there using the same protocol.
- Each hub owns enrollment, permissions, retention, alerts, and users independently.
  Two hubs do not imply replicated databases or an automatic failover cluster.
- Multiple destinations are supported from the first collector implementation. Hub
  federation, automatic discovery, and clustered hub deployments are separate requirements.

Only collector-to-hub and browser-to-hub reachability is necessary. LAN, Tailscale,
and ordinary routed networks use the same protocol. No collector-to-collector links
or inbound collector ports are required.

## Transport choices

| Link / operation | Transport | Reason |
| --- | --- | --- |
| Collector ↔ hub | WebSocket over TLS (`wss://`) | One outbound session carries telemetry, acknowledgments, capabilities, and detail requests in both directions. |
| Hub → browser, live updates | SSE over HTTPS | Predominantly one-way updates; EventSource provides reconnects and event IDs. |
| Browser → hub, queries/actions | HTTPS JSON requests | Normal request/response semantics for history, settings, authentication, and explicit actions. |
| Collector enrollment | HTTPS JSON request | Exchange a short-lived enrollment token for a credential specific to that hub connection. |

WebSocket keeps acknowledgments and on-demand detail requests on the collector's existing outbound connection. Browser SSE and JSON requests keep live updates separate from explicit queries. Bounded JSON frames have no automatic transport fallback.

## Runtime and module boundaries

Keep TypeScript/Node and Next.js, with two executable roles: `collector` and `hub`.
The hub has one explicit, long-running Node entry point that owns its HTTP server,
WebSocket upgrades, application services, and background jobs. It delegates UI requests to
Next.js. This preserves one hub application process/listener; a TLS proxy is optional
infrastructure. Collection belongs to the collector and continues without a browser.

Transport handlers receive services from the composition root. Do not assume a `globalThis`
object is shared between Next.js bundles or let route requests start monitoring loops.
This is a custom-server deployment, not unchanged `next start` or a serverless deployment.
The entry point has its own runtime bundle. Production ships `.next` with `dist/*.cjs`; development uses `.next-dev`. Collector upgrades go to `/api/collector/connect`; framework upgrades reach Next.js.

Module layout (responsibilities; consult the source tree for exact paths):

```text
collector/
  main.ts                  # lifecycle and local configuration
  sampling/                # OS metrics, capabilities, monotonic rate calculation
  destinations/            # independent delivery session/outbox per hub
src/
  contracts/               # wire schemas, units, identifiers
  hub/
    main.ts                # composition root; HTTP + upgrades + Next.js
    enrollment/            # credentials and hub-local machine mapping
    telemetry/             # validation, persistence, latest state, freshness
    subscriptions/         # browser interests -> expiring detail requests
    history/               # SQLite batches, queries, retention, rollups
    alerts/                # incident lifecycle and notifications
    transport/             # WebSocket, SSE, and HTTP handlers
  app/                     # fleet and machine-scoped UI
  components/              # existing visual system
```

Keep substantial behavior behind small module interfaces. Telemetry accepts batches,
returns state/history, and provides subscriptions; callers do not manage its deduplication
or freshness. Destination delivery hides reconnects/acks/outboxes from sampling. Sampling
hides OS/tool differences. Do not add generic interfaces for hypothetical implementations;
wire contracts are a real process boundary already.

### Installation and component lifecycle

One release contains the hub, collector, UI and administration tools. The install
role is an initial preset; version 2 configuration records independent hub and
collector enablement. Version 1 roles are mapped in memory on read and migrated
on an explicit mutation. Read-only status never rewrites configuration or units.

User installations have separate hashed hub and collector systemd units, each
running `rigscope run hub|collector` with its own supervisor lease. System scope
uses separate dedicated accounts and hardened units. Neither service depends on
the other. Each retains its settings, credentials and data when disabled; a
collector can continue sending to remote hubs while its local hub is disabled.
An unconfigured collector reports Awaiting connection and stays dormant.

The configuration module owns presets, migration and component setup. The service
module owns systemd control and component service registration. The lifecycle
module starts and verifies selected components. Upgrade and rollback coordinate
one shared runtime, hold both component leases, back up hub storage and restore
the exact previous activity and startup settings. Uninstall removes both units,
launcher and runtime copies while retaining application data.

Automatic colocated pairing uses bootstrap configuration only before the first
hub start. Once SQLite owns the registry, pairing uses that registry and the
normal enrollment endpoint to update a live hub without restarting it. Existing
collector destinations are preserved. Explicit `connect` also supports a
one-time grant from another machine's hub. The [installation guide](docs/installation.md) describes component commands and hub-first setup.

The implemented coordination boundaries are:

- `src/live/machine-selection.ts` resolves route, authorized Fleet and remembered
  selection with view-specific fallbacks, and coordinates navigation and persistence.
  Views consume the selection; explicit missing Machines retain their access outcome.
- `src/hub/sample-ingestion.ts` authenticates Sample identity against the current
  Collector session, commits through `TelemetryStore`, then publishes eligible live
  Samples and Incident observations only while that session remains current. It also
  restores persisted metrics, cadence, inventory and metadata at startup without
  marking a Collector connected. Transport formats acknowledgments from its outcomes.
- `collector/destinations/connection.ts` owns each Hub connection's sharing policy,
  cadence, recovered queue sanitation, inventory and durable append-before-send path.
  Collector runtime fans shared Samples out to these connections. Sampling, GPU
  polling, aggregate disk budgets and cross-Hub Detail scheduling remain shared.
  Recovery applies privacy filtering without reapplying capture-time cadence.

## Identity, enrollment, and messages

Generate a persistent collector installation ID locally. Enroll it separately with each hub,
which maps the authenticated collector to a hub-local machine ID. Addresses/hostnames are
metadata. Reinstall/replacement needs explicit enrollment or administrator reassociation;
cloned installations must generate new identities.

Each hub connection has an independent credential and local sharing policy. Verify the
hub's TLS certificate using ordinary certificate trust. Send credentials in an authorization
header, not a URL. Remote connections require TLS; loopback-only development may explicitly
use plaintext. Authenticate first and reject conflicting claimed collector identities.

JSON message families:

- `hello` / `welcome`: capabilities, inventory, hub/session identity.
- `samples` / `ack`: bounded batches; identify exactly which sample IDs were durably accepted
  or permanently rejected.
- `detail.subscribe` / `detail.cancel` / `detail.result`: scoped, temporary monitoring detail.
- `heartbeat`, `collector.status`, `gap`: connection liveness, collection errors, and
  monitoring intervals lost to delivery limits.

Sample fields: `collectorId`, `runId`, `sequence`, `bootId`, `capturedAt`,
`kind`, and validated `payload`. `runId` changes on collector restart; `bootId` identifies
an OS boot. `(collectorId, runId, sequence)` is unique, with sequence increasing across
sample kinds within a run. Each hub assigns `receivedAt` and its own browser-stream cursor.
Use stable resource IDs where possible, e.g. GPU UUID, not display order. Define units/CPU
normalization and calculate rates from local monotonic elapsed time.

Separate inventory, periodic metrics, and process detail. Unsupported/failed metrics have
explicit status and absent values, never successful zeroes. Process detail is live/on
demand and is not persisted by default.

## Multiple hubs and subscription policy

Collect each metric once at the fastest locally permitted requested cadence, then fan it
out to authorized connections. Apply per-destination field filters and cadence before
queueing/transmission. Omit process arguments and environment variables by default. Hubs
cannot install destinations, change another destination's policy, or exceed local budgets.
If a destination's sharing policy is reduced, cancel disallowed subscriptions and purge
now-forbidden fields from its pending queue before further transmission.

Each destination owns its session, bounded queue, acknowledgment state, and reconnect
schedule. An offline hub must not block a healthy hub or sampling. Set a configurable
destination count and global resource budget as well as per-destination limits.

Example: A needs GPU summaries; B needs summaries and has someone viewing Processes. Share
one GPU collection pass, deliver process detail only to B, and stop extra scans when its
subscription expires. A's subscription must not authorize sensitive delivery to B.

## Failure, delivery, and overload

- WebSocket ordering applies within a connection, not across reconnects. Use heartbeats,
  idle deadlines, and exponential reconnect backoff with jitter.
- Periodic metrics use at-least-once delivery within the retention budget. Retry retained samples until acked;
  deduplicate in the hub transaction. Ack after durable commit, not socket receipt. Reject
  invalid records explicitly so bad samples cannot retry forever. Process results are
  transient and request-scoped.
- Use disk-backed per-hub outboxes for periodic metrics to recover across collector restarts.
  Bound age/bytes per destination and total disk usage. Default caps: 10 minutes / 64 MiB
  per destination, whichever comes first; measure before finalizing. Evict oldest pending
  telemetry at the cap and retain a bounded, coalesced gap notice. Bound the in-flight
  window too. Do not spool sensitive process detail by default.
- Prioritize fresh metrics over rate-limited backfill. Backfill fills history but cannot
  replace newer live state or trigger an old incident as if it just happened.
- Bound frames, ingestion rates, outstanding requests, and browser buffers. WebSocket does
  not supply application delivery guarantees. Coalesce metric updates for a slow browser
  or disconnect/resynchronize it; refetch incident state after gaps.
- Track connection health separately from sample freshness: a connected collector can
  have broken GPU collection. Use expected cadence and receipt time for liveness; retain
  capture time and flag clock skew rather than trusting remote wall clocks.
- Removing a destination stops delivery and purges its credential/outbox. Hub revocation
  terminates existing sessions as well as rejecting new ones.

## Browser flow

Use one authorized, multiplexed SSE stream per open app tab for its machines/event types,
not one per chart or machine. Prefer HTTP/2 at the TLS proxy because HTTP/1.1 browsers limit
concurrent SSE connections per origin. Disable proxy buffering for streams and configure
WebSocket upgrades/idle timeouts end to end.

Authenticate with same-origin secure, HttpOnly session cookies. Authorize scope on connect,
replay, and permission changes; protect mutations against CSRF. Filter before transmission.
Establish a consistent initial subscription/snapshot boundary, then emit updates. Provide
event IDs and bounded replay. Expired cursors or a previous hub run cause a reset/fresh
snapshot. EventSource reconnects do not themselves provide durable replay. History queries
use HTTPS with a point/result budget and suitable rollups.

Opening an authorized detail view creates/renews a bounded lease over the collector's
existing WebSocket. The hub combines equivalent viewer interests; the collector combines
compatible interests across hubs while preserving delivery permissions. Expire leases
using local monotonic TTLs. Browser disconnect, hub disconnect, or renewal failure releases
extra work promptly. Requests must fit local policy and reported capabilities.

## Storage and scale

Each hub owns a SQLite database, one bounded write queue, batched writes, retention, and
rollups that preserve min/max. Never scan the OS or write telemetry once per browser.
Bound query/transaction work. Measure event loop delay during ingestion, history queries,
retention and rendering; move synchronous database work to a worker thread if needed.

One hub application instance owns connection registry, leases, latest state, and bounded
browser replay. Many connections do not require multiple backend replicas. Replicas later
need explicit session routing, pub/sub, shared storage, and background-job ownership.

Historical planning envelope: 1–50 machines with multiple viewers, not a measured capacity
claim. There is no fixed machine-count limit. The concurrent collector socket budget
defaults to 100 and is configurable with `HUB_MAX_COLLECTOR_CONNECTIONS`; it is a
deployment resource budget, not enrollment capacity. Benchmark representative payload
sizes, process counts, subscriptions, viewers, history queries and host resources.
Observe queue depth, stream count, active leases,
ingestion lag, dropped samples, memory, and event loop delay.

## Future actions

The release is read-only. Future structured actions travel browser HTTPS →
hub authorization/audit → collector WebSocket → result → hub → browser SSE. Each destination
has an explicit action policy, default denied. Read-only hubs may request allowed monitoring
detail. Initially allow at most one mutating hub per collector to avoid conflicting control.

Actions need request IDs, deadlines, handling of ambiguous outcomes after connection loss,
and execution-time target validation (machine/boot/PID/start identity for processes).
Do not apply telemetry retry behavior to destructive actions. Specify action semantics
separately before implementing kill/renice or service restarts.

## Terminology

**Machine**:
A physical server or virtual machine whose operating system and workloads are monitored.
_Avoid_: Node (when it means the monitored machine)

**Collector**:
The background service installed on a machine to gather its monitoring information and report it to configured hubs.
_Avoid_: Agent, AI agent, sensor, exporter (for this service)

**Hub**:
An independently operated monitoring service that receives information from collectors and makes it available to its users.
_Avoid_: Primary (unless an explicit primary relationship exists)

**Hub connection**:
A collector's independently configured relationship with one hub, including what information that hub may receive and what requests it may make.
_Avoid_: Replica, mirror (independent hubs do not imply replicated state)

**Fleet**:
The collection of machines a user can view through a particular hub.

**Browser session**:
A user's authenticated access to one independently operated hub.

**Machine scope**:
The set of machines a user is permitted to view through a hub. An empty scope grants no machine access.

**Sample**:
A measurement of a machine or resource at a particular time.

**Capability**:
A category of monitoring information or permitted operation that a collector can provide for a particular hub connection.

**Detail subscription**:
A temporary request for more detailed monitoring information about selected resources.

**Incident**:
A tracked occurrence of an alert condition, including its active and resolved states.


See [operations](docs/operations.md) for persistence, upgrade and recovery guarantees.
