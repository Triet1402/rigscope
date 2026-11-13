# Architecture

Rigscope 1.0 is one static Go binary (`rigscope`). The same executable is the
installer/CLI, the hub and the collector. The browser UI is a React/TypeScript
static export embedded in the binary. There is no Node.js, npm or Python at
runtime, no downloaded runtime tree, and no wrapper process.

## Topology

```
browser ── HTTPS/SSE ──┐
                       ├── rigscope hub ── SQLite (WAL) ── state directory
collector ── WSS ──────┘      │
  (one per machine,           └── embedded static UI (out/ → embed.FS)
   outbound only)
```

- A hub stores history and serves the browser UI, the JSON API, the SSE live
  stream and the collector WebSocket endpoint.
- A collector samples its machine and connects outbound to exactly one hub. It
  opens no inbound port.
- The default installation runs both components on one computer. They remain
  separate processes and separate systemd units: the collector needs hardware
  groups, while the hub faces the network.
- TLS is built in for anything outside loopback; a documented reverse-proxy
  setup is the alternative. Plaintext is loopback only.

## Process model

| Command | Responsibility |
| --- | --- |
| `rigscope install`, `upgrade`, `rollback`, `uninstall` | Idempotent installation and the journaled activation transaction. |
| `rigscope status`, `doctor`, `logs`, `reset-password` | Operations against the installed state. |
| `rigscope migrate` | Versioned config and database migrations, run by upgrade. |
| `rigscope hub` | HTTP(S), SSE, collector WSS ingestion, SQLite, auth, alerts, UI. |
| `rigscope collector` | Sampling, inventory, delivery to one hub, process detail, enrollment. |
| `rigscope collector enroll` | One-time grant exchange for a per-collector credential. |
| `rigscope internal pci-read` | Private self-exec helper for opt-in GPU register reads. |
| `rigscope version` | Build version (ldflags). |

systemd supervises the binary directly. Hub and collector hold separate
installation locks; a second process on one state directory fails instead of
double-writing. The collector keeps a bounded RAM queue, drains it within a
bounded window on SIGTERM, and reports gaps rather than inventing samples.

## Repository map

```
go.mod                         module github.com/Triet1402/rigscope
cmd/rigscope/                  main: subcommand dispatch only
internal/version/              build version (ldflags)
internal/protocol/             collector wire types, strict codecs, limits, versions
internal/api/                  browser API contract, generated TS types/validators
internal/config/               versioned config file, forward migrations
internal/activation/           upgrade/rollback journal, backups, schema floor
internal/cli/                  install, upgrade, rollback, uninstall, status, doctor, logs
internal/platform/             systemd units, file ownership, leases, private writes
internal/release/              release fetch, checksum and archive verification
internal/collector/
  sampling/procfs/             /proc and /sys text → typed structs
  sampling/gpu/                nvidia-smi, AMD sysfs, runtime-PM gate, temperatures
  processes/                   process table scan and detail scheduler
  metrics/                     assembles protocol payloads and inventory
  policy/                      collector-local sharing policy
  delivery/                    session, queue, acks, gaps, reconnect
  enroll/, state/, runtime/    enrollment, on-disk identity, process lifecycle
  synthetic/                   explicit mock machine
internal/hub/
  server/                      HTTP(S), Host/Origin, TLS, trusted proxies, static UI
  auth/                        passwords, sessions, CSRF, login rate limiting
  ingest/, live/               collector gateway, admission, SSE snapshots
  history/                     query policy and downsampling
  storage/                     SQLite schema, migrations, telemetry, receipts, alerts
  alerts/                      rules, evaluation and webhook delivery
  admin/                       user directory, collector administration, audit log
  webui/                       embed.FS export, CSP hashes
src/app, src/components/       Next.js app router and React components (static export)
src/contracts/                 generated API types + validators, wire parsers
src/live, src/history/         browser stream, history and presentation logic
src/processes, src/exports/    process views and CSV exports
testdata/protocol/             golden wire fixtures shared by Go and TS tests
testdata/api/                  captured browser-API fixtures
scripts/                       install.sh, release build/verify, UI test runners
docs/rewrite/                  plan, protocol/API contracts, test matrix
```

## Data flow

1. The collector samples procfs/sysfs and hardware, applies its local sharing
   policy, then sends `hello`, `samples`, `inventory` and `collector.status`
   frames over WSS.
2. The hub authenticates the collector credential, validates and admits the
   batch, commits it to SQLite, publishes eligible live state to SSE clients,
   and feeds the alert engine. A `live` batch may update latest state; a
   `replay` batch only fills history.
3. The browser reads snapshots/updates from `/api/live` and history/processes
   from the JSON API; mutations are origin- and CSRF-checked.
4. Process detail is request-scoped: the browser subscribes, the hub forwards an
   expiring `detail.subscribe` lease, and results stop when the interest or
   lease ends.

Records are keyed by `(collectorId, runId, sequence)`. A collector restart keeps
the installation identity and starts a new run. Lost intervals are reported as
gap records; an unsupported or failed reading is explicitly unavailable with a
reason, never a fabricated zero.

## Contracts

- Wire protocol: [`docs/rewrite/protocol-v1.md`](docs/rewrite/protocol-v1.md),
  implemented by `internal/protocol`, with `testdata/protocol/` shared by Go and
  TypeScript tests.
- Browser API: [`docs/rewrite/api-v1.md`](docs/rewrite/api-v1.md), implemented
  by `internal/api`. TypeScript types and validators are generated from the Go
  declarations; `tests/api-contract.test.ts` validates real Go responses.
- Installation layout and config: [`docs/rewrite/layout-v1.md`](docs/rewrite/layout-v1.md)
  and [`docs/configuration.md`](docs/configuration.md).

## Storage

- SQLite through `modernc.org/sqlite` (pure Go): a durable WAL database for
  control state and, by default, a separate bounded in-memory telemetry database.
  `components.hub.telemetryMode: "persistent"` stores telemetry in the durable
  database instead.
- Append-only migrations with a schema floor contract shared by storage,
  `migrate` and upgrade rollback.
- 10-second/1-minute/1-hour summary tiers, sample receipts, gaps and inventory
  live in the selected telemetry database. Alert state, accounts/sessions,
  enrollment grants and audit logs are always durable. Raw per-second samples
  are not stored; only summaries are.
- A bounded in-RAM 1-second ring per machine (default 15 minutes) sits next to
  the summary tiers.
- Retention, database size budgets, bounded cleanup and WAL checkpointing.
  Disk-full is a visible degraded state, never silent loss. Live publication
  stays independent of commit latency.
- Backup is a SQLite online backup plus config and collector identity.

Recordings are gone. Hub history supports RAM (default) and persistent modes;
the collector uses a bounded RAM queue. Welcome messages advertise memory or
durable acceptance to match the selected hub mode.

## Security boundary

The threat model and operator responsibilities are in
[`docs/security.md`](docs/security.md). The code-level gate:

- Canonical public origin; exact Host and Origin checks before every handler.
- Forwarded headers trusted only from configured proxy addresses.
- Secure, HttpOnly, SameSite cookies; CSRF on mutations; session expiry,
  logout, account changes and collector revocation immediately end SSE and
  process-detail interests.
- Login rate limiting and bounded password-hash verification cost; initial
  admin bootstrap and offline `reset-password`.
- Collector TLS trust from system CAs or a pinned CA file; plaintext only on
  loopback. Collector credentials are stored hashed; enrollment grants are
  single-use and survive a lost response by returning the same credential.
- The opt-in PCI register helper is a private self-exec with a hard timeout, so
  a device fault cannot stall sampling or kill the collector.

## Deferred

Multi-hub fan-out with per-destination policy, recordings, Prometheus metrics
and an optional Docker image are not part of 1.0. The config keeps
`destinations` as a list (exactly one accepted) so multi-hub can return later.
