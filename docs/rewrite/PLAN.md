# Rigscope 1.0: Go rewrite plan

Status: code complete on branch `go-rewrite`; local 1.0.0 release preparation
is in progress. The TypeScript code up to `v0.2.6`
is a prototype. It is the reference for behaviour and hard-won platform
knowledge, not code to preserve. No 0.2 → 1.0 data importer is planned: 0.2
has a single user, who reinstalls fresh. From 1.0 on, other people use
Rigscope, so 1.x releases must upgrade in place without data loss.

The 1.0.0 candidate also includes the subsequent RAM-default telemetry mode,
five-second persistent commit batching and coalesced rollup writes. These
changes require fresh release validation; the earlier progress-log results
below describe their original revisions. Local candidate logs, checksums and
provenance are collected under `build/release-evidence/`. Publication follows
[releasing-v1.md](releasing-v1.md); draft notes are in
[release-notes-1.0.0.md](release-notes-1.0.0.md).

## Goals

1. **One static Go binary** (`rigscope`) that contains the CLI/installer, the
   hub and the collector. No Node.js, npm or python3 at runtime.
2. **Light footprint.** Today one host runs four processes at about 440 MB
   RSS, plus 620 MB on disk (391 MB runtime + 230 MB Node). Targets: collector
   15–25 MB RSS, hub 30–60 MB RSS, under 40 MB on disk.
3. **Easy installation:** `curl -fsSL <site>/install.sh | sh`, plus an
   "Add machine" one-liner from the hub that installs and enrolls a collector.
4. **Stable for other users:** versioned protocol, versioned config, database
   migrations, and tested upgrade/rollback.

## Architecture

```
rigscope  (CGO_ENABLED=0; linux/amd64, linux/arm64, linux/arm/v7)
├── rigscope install|upgrade|rollback|uninstall|status|doctor|logs|...
├── rigscope hub        HTTP(S), SSE, collector WSS, SQLite, auth, alerts,
│                       React UI served from embed.FS (static export)
└── rigscope collector  sampling, process detail, delivery to one hub
```

- Hub and collector stay **separate processes and separate systemd units**.
  The collector needs hardware groups; the hub faces the network.
- systemd supervises the binary directly; no wrapper processes.
- The UI remains React/TypeScript and is built as a static export
  (`next build` with `output: 'export'`, Vite later if Next adds nothing).
  The browser ↔ hub JSON API becomes the cross-language boundary; TS types
  are generated from Go types.
- Storage is SQLite through `modernc.org/sqlite` (pure Go).
- WebSocket: `github.com/coder/websocket`.

## Decisions carried over (keep)

| Area | Decision |
| --- | --- |
| Topology | Collectors connect outbound over WSS to a hub. One-time enrollment grant → per-collector credential in an `Authorization` header. TLS for anything not on loopback. |
| Wire messages | `hello/welcome`, `samples/ack`, `inventory`, `collector.status`, `detail.subscribe/cancel/result/ended`, `gap`. Records keyed by `(collectorId, runId, sequence)`; `live` vs `replay` batches. Strict parsing: unknown fields are rejected. |
| Data honesty | Unsupported or failed metrics carry explicit `status: "unavailable"` with a reason and absent values. **Never a fabricated zero.** In Go this means pointers / explicit optional types, never bare zero values. |
| Identity | Stable resource IDs (GPU UUID, not display order). Persistent collector installation ID; cloned installs regenerate it. |
| Sampling | procfs/sysfs parsing, hwmon, PSI pressure, nvidia-smi parsing, AMD sysfs GPU, RAPL, runtime-PM gating for hybrid laptops, GPU register temperatures (opt-in). Rates come from local monotonic time. |
| Process detail | On demand only, with expiring subscriptions. Arguments and environment are omitted by default. |
| Bounds | Message size/count limits, bounded queues, gap records for lost intervals. |
| Product | Live fleet, machine view, history, processes, connections, alerts + webhooks, accounts (admin flag, last-admin protection, small audit log), CSV exports, synthetic collector mode for the website demo and tests. |

**Runtime PM guarantees.** The collector never deliberately wakes a suspended
discrete GPU. The gate reads `power/runtime_status` before deciding, every
hardware operation runs under a per-device access lease, and a command's leases
are released only after its child (`nvidia-smi`, the `pci-read` helper) has
actually been reaped, so a revocation cannot drain and a quiet window cannot
start underneath a command that may still be running. The quiet window is
measured from the moment the drain finishes, not from when it was requested.
Immediately before each access — nvidia-smi launch, helper dispatch, DRM sysfs
counter read — the status is re-read under the lease, and a device the kernel no
longer reports usable is refused. An `unsupported` status only means runtime PM
is disabled, which can mask a suspended device, so it authorizes access only
when the cached `power_state` attribute reads exactly `D0`; a `D3*` reading is
treated as suspended (even for an otherwise exempt device) and a missing or
unrecognised one fails closed as unknown. The window between that last re-check
and the access cannot be closed from user space (nvidia-smi and sysfs offer no
non-waking power reference), so losing that race can wake a suspended device
once; the reading is then reported unavailable with a power-state reason and
sampling resumes. This residual race is accepted as a documented best effort.

## Decisions changed

| Prototype (0.2) | 1.0 |
| --- | --- |
| No protocol-version markers; hub and collectors upgrade in lockstep | Versioned protocol with a real compatibility window; see "Protocol compatibility". |
| No config schema version; pre-canonical installs reset fresh | Versioned config file with forward migrations; 1.x never discards user data on upgrade. |
| Two hub history modes (RAM rings / persistent) | RAM summaries by default; opt-in persistent SQLite summaries. Both use bounded retention and a recent RAM ring. |
| Two collector queue modes (memory / durable SQLite outbox) | One mode: bounded RAM queue plus gap records for *known* drops, a bounded graceful drain on shutdown, and an explicit "unknown loss" marker after a restart (a crash loses pending samples; this is documented, not hidden). |
| Collector fans out to several hubs with per-destination policy and budgets | **One hub per collector in 1.0.** Config keeps `destinations` as a list (exactly one accepted) so multi-hub can return later. **One collector-local sharing policy stays** (metric families, network addresses, process visibility, command arguments), applied to metrics, inventory and detail before transmission; narrowing it purges queued records. |
| npm package, Node 26 required, `rigscope run` wrappers, `sg` group nesting, python3 mmap helper | curl install of a static binary; systemd runs it directly; system units use `SupplementaryGroups=`; PCI-register mmap runs in a private self-exec helper (`rigscope internal pci-read`) with a hard timeout, so a device fault or hang cannot stall sampling or kill the collector. |
| Install/upgrade ceremony (`src/cli` 10.5k lines, `src/operations` 3.8k) | Idempotent `install`; `upgrade` is an activation transaction (see "Upgrade activation"); `backup` = SQLite online backup + config + collector identity. |

## Cut or deferred

- Recordings are deferred. To keep a fine-resolution diagnostic path, the hub
  keeps a bounded 1-second RAM ring per machine (default 15 minutes) next to
  the summary store, viewable and exportable as CSV. Recording controls
  and history-mode copy are removed from the UI in Phase 2.
- Multi-hub fan-out, per-destination sharing policy, disk budgets (deferred).
- Unix-socket proxy boundary and dev HTTPS validation scripts, replaced by
  built-in HTTP(S) plus one documented reverse-proxy setup with explicit
  trusted-proxy configuration. The protections they gave are kept; see
  "Security gate". The relevant HTTPS/auth tests are ported, not deleted.
- Scale/stress artifacts in `docs/stress-*` (re-run a harness against Go).
- The TypeScript unit tests: read them as specifications while porting, then
  delete them with the code they test.

## Protocol compatibility

- `hello.protocol` carries the collector's highest version; `welcome.protocol`
  carries the version the hub selected. A missing marker means version 1
  (the 0.2-compatible contract the Node hub speaks during Phase 1).
- The hub supports the current and previous version, and encodes and
  validates each session with that session's version. Strict unknown-field
  rejection stays, per version.
- An unsupported version is refused with a typed `welcome` error telling the
  collector to upgrade; the hub shows such collectors as "needs upgrade".
- Upgrade order is hub first, then collectors. Compatibility is tested in
  both directions (old collector → new hub, new collector → old hub refused
  cleanly).

## Upgrade activation

`upgrade` is a transaction, not a binary swap:

1. Take the installation lock; download and verify the release (SHA-256).
2. Quiesce: stop the collector, drain and stop the hub.
3. Back up the database, config and collector identity together.
4. Run config and database migrations with the new binary (`rigscope migrate`).
5. Activate the new binary (keep `rigscope.prev`), start components, verify
   health.
6. On failure before the new version accepts writes: restore the backup and
   previous binary. After it has accepted writes, rollback is allowed only
   when the previous version declares the schema compatible; otherwise the
   user is told and nothing is discarded.
7. An interrupted upgrade is detected and resumed or rolled back on the next
   run.

Tests cover a migration-bearing upgrade, failed activation, interrupted
upgrade and delayed rollback in disposable installations.

Schema compatibility contract (`compatibleDown`, "the floor"): the lowest
schema version whose binary can safely open and write the data. Each
migration is either additive (keeps the floor) or breaking (a breaking
migration to version N raises the floor to N); with no breaking migration
the floor is the first schema version. Rollback to a binary with schema P is
allowed iff P ≥ floor, and an older binary may open a newer ledger iff its
own schema ≥ floor. When both configuration and database migrate, the
reported floor is the larger of the two.

## Security gate (must pass before 1.0)

- Canonical public origin; exact Host and Origin checks before every handler.
- Forwarded headers trusted only from configured proxy addresses.
- Direct HTTPS and the documented reverse-proxy setup both tested.
- Secure, HttpOnly, SameSite cookies; CSRF protection on mutations.
- Session expiry, logout and account changes immediately end SSE streams and
  process-detail interests; collector revocation closes its session.
- Login rate limiting and bounded password-hash verification cost.
- Initial admin bootstrap (password printed once) and offline recovery
  (`rigscope reset-password`).
- Collector TLS trust: system CAs by default, optional pinned CA file in
  collector config; plaintext only on loopback.
- Enrollment survives a lost enrollment response (retry with the same grant
  returns the same credential), as the prototype does.

## Storage and diagnostics policy

- RAM telemetry by default, with a separate durable control database; opt-in
  persistent telemetry. Both have summary retention and a 1-second RAM ring.
  Persistent receipts outlive the replay horizon; database budgets, bounded
  cleanup and WAL checkpointing control disk use.
- Bounded cleanup and query work; explicit busy responses under pressure;
  disk-full puts the hub into a visible degraded state, never silent loss.
- Live publication stays independent of commit latency.
- Diagnostics visible in the UI and `rigscope status`: collector queue depth,
  drops and gaps, hub ingestion lag, storage pressure. Prometheus can come later.

## Added for other users

- Clear support matrix (distros, architectures, systemd requirement).
- Login rate limiting, origin/CSRF checks, secure defaults for an exposed hub.
- Documented 1.x stability promises (config keys, protocol window, migrations).
- Later: optional Prometheus `/metrics` on the hub; optional Docker image.

## Repository layout

The Go module lives at the repository root next to the TypeScript prototype.
The prototype is deleted piece by piece as Go replaces it.

```
go.mod                         module github.com/Triet1402/rigscope
cmd/rigscope/                  main: subcommand dispatch only
internal/version/              build version (ldflags)
internal/protocol/             wire types, strict decode/encode, limits, protocol version
internal/collector/
  sampling/procfs/             pure parsers: /proc and /sys text → typed structs
  sampling/gpu/                nvidia-smi poller, AMD sysfs, runtime-PM gate, temperatures, PCI registers
  processes/                   process table scan
  metrics/                     assembles protocol MetricsPayload/Inventory from samplers (wave 2)
  delivery/                    session, bounded queue, acks, gaps, reconnect (wave 2)
  enroll/, identity/, config/  (wave 2)
internal/hub/                  http, sse, ws ingestion, storage, auth, enrollment, alerts, admin, exports (wave 2+)
internal/cli/                  install, upgrade, rollback, uninstall, status, doctor, logs
internal/platform/             systemd units, process leases, private file writes
testdata/protocol/             golden wire fixtures shared by Go and TS tests
testdata/proc/<host>/          captured /proc and /sys trees
scripts/install.sh             tiny POSIX installer: detect arch, download, verify, exec `rigscope install`
docs/rewrite/                  this plan, protocol-v1.md, layout-v1.md
```

## Engineering conventions

- Go 1.27, `CGO_ENABLED=0`, Linux only (`//go:build linux` where syscalls are used).
- Standard library first. Approved dependencies: `modernc.org/sqlite`,
  `github.com/coder/websocket`. Anything else needs a written reason.
- `gofmt`, `go vet ./...` and `go test ./...` pass on every commit.
- Parsers are pure functions over bytes or an `fs.FS` root so they are tested
  against `testdata/` fixtures without root or real hardware.
- No package-level mutable state; dependencies are passed in. Errors, not panics.
- Each package starts with a `doc.go` that says what it owns and what it does not.
- Port behaviour, not structure: read the TS module and its tests, then write
  idiomatic Go with tests covering the same cases.

## Phases

### Phase 1: Go collector against the existing Node hub

The Go collector must work with the current TypeScript hub so it can replace
the Node collector on real machines before the hub is ported. Protocol v1 is
therefore the current contract plus an optional `protocol` field, which the TS
parser is updated to accept.

- **Wave 1 (parallel):** protocol package + fixtures; procfs/sysfs parsers;
  GPU sampling; process scan; UI static export; installer/CLI foundation.
- **Wave 1 integration checkpoint:** explicit adapters from the sampler,
  GPU and process types into `internal/protocol` (mirrored structs are never
  serialized directly). Assembled metrics, inventory and process detail
  round-trip through the Go and TS validators, including unavailable values,
  empty collections, limits and oversized process tables. CI gains Go
  checks and cross-builds (amd64, arm64, arm with `GOARM=7`, `CGO_ENABLED=0`).
- **Test harness adapter:** `tests/runtime/support/process.ts` can launch a
  selectable collector executable (Node or Go) and, later, hub executable.
  The real-collector assertions that survive the scope cuts are listed;
  multi-hub fan-out assertions are marked out of scope.
- **Wave 2:** metrics assembler, sharing policy, delivery session/queue,
  enrollment (with lost-response recovery), identity, collector config and
  runtime. Exit check: Go collector → Node hub tests pass for both Node
  history modes, delayed and lost acks, retry responses, reconnect,
  inventory, detail leases and TLS.

### Phase 2: Go hub

- First task: the browser API contract. Requests, responses, errors, SSE
  envelopes and nullability are defined as Go types; TS types and validators
  are generated from them with a regeneration check in CI, and real Go
  responses are tested against the browser validators. Hashed UI assets keep
  working across hub upgrades (old pages reload cleanly).
- HTTP server with embedded static UI, SQLite + migrations, auth and sessions,
  enrollment, collector WS ingestion, SSE live stream.
- History summaries and retention, alerts + webhooks, admin + audit,
  exports, process-detail subscriptions.
- Exit check: the browser and runtime integration suites pass against the Go
  hub; the Node hub and Next server are deleted.

### Phase 3: Release 1.0

- GoReleaser tarballs + SHA-256 checksums on GitHub Releases; `install.sh`
  hosted on the website; `npm deprecate` pointing to the new installer.
- Support matrix, stability promises and rewritten docs.
- Disposable-install tests for install, upgrade, rollback and uninstall;
  artifact smoke tests on each supported architecture.
- Footprint measured with representative history, an offline-hub queue and
  active process detail, not only at idle.

## Progress log

- Wave 0: plan, Go module skeleton, Go 1.27.4 toolchain.
- Wave 1 started. Each task runs in its own worktree branched from the Wave 0
  commit, is reviewed independently, and is merged into `go-rewrite` after its
  review findings are fixed:
  - W1a `internal/protocol`, `testdata/protocol/`, `docs/rewrite/protocol-v1.md`,
    optional `protocol` field accepted by the TS parsers.
  - W1b static export of the Next UI, served by the Node hub.
  - W1c `internal/collector/sampling/procfs` + `testdata/proc/`.
  - W1d `internal/collector/sampling/gpu` + `testdata/gpu/`.
  - W1e `internal/collector/processes` + `testdata/procs/`.
  - W1f `internal/config`, `internal/platform`, `internal/cli`,
    `scripts/install.sh`, `docs/rewrite/layout-v1.md`.
  - Plan review (Sol 6.1) done; its 12 findings are folded into this plan
    (protocol compatibility, upgrade activation, security gate, sharing
    policy, storage/diagnostics policy, integration checkpoint, harness
    adapter, API contract task, PCI helper isolation, early cross-builds).
- Wave 1 status (Sol 6.1 review after every task; findings fixed before or
  right after merge):
  - Merged: W1a protocol (+ fixes: one strict ISO-8601 grammar in Go and TS,
    lone surrogates rejected in both, discriminator checks, validated
    `Encode*Message`), W1c procfs (+ fixes: honest swap, sparse cores,
    contract bounds, UTF-16 caps, bounded ids), W1d GPU, W1e processes,
    W1b static UI export (Node hub RSS 105 MiB → 63 MiB).
  - Fixing: W1d (runtime-PM gate for register reads, self-exec PCI helper),
    W1e (per-send policy refresh, lease expiry during scans, cheaper fitting,
    4-worker default), W1b (symlink containment, `/machines?machineId=`
    route with redirects, Accept-Encoding q-values).
  - Rework: W1f installer — review found that `uninstall` could delete the
    live 0.2 state. Split into W1f-A (ownership manifest, legacy detection,
    safe uninstall, collector-private secrets, config v1 additions, lock) and
    W1f-B (hardened release fetch, journaled upgrade activation).
  - Decision: `go-rewrite` never ships a 0.2.x release. The 0.2 release and
    install tooling (`src/operations`, `src/cli`) is not updated for the
    static export and is deleted when the Go installer lands.
- Wave 2 started: W2b delivery/enrollment/identity, W2c runtime test harness
  adapter. CI runs Go vet, race tests, fixture drift and cross-builds.
- Wave 2 status:
  - Merged: W2b delivery/enrollment/identity (works against the real Node
    hub: handshake, enrollment, durable acks), W2a metrics assembler +
    sharing policy + synthetic mode (assembled messages accepted by the TS
    parsers), GPU fixes round 2 (fail-closed gate, full PCI addresses,
    owned reapers, `/proc/self/exe` helper).
  - Fixing: W2b (redirect downgrade, one sequence allocator, atomic
    reservation, enrollment file format owned by the collector, run-boundary
    gaps in the Node hub history), W2a (pointer-form sanitization, status
    failure filtering, panic isolation per sampling boundary).
  - Installer: two review rounds found data-loss paths. Integration branch
    `rb/installer` combines both halves; rework A2 (FD-anchored
    install/uninstall, planned vs owned manifest entries, lock in the
    runtime dir) and B2 (write barrier before first start, hashed publish,
    rollback substates, WAL-aware backups, bounded migrate exec) in progress.
  - Harness rework (single-hub assertions kept for Go, fault-injecting relay)
    in progress.
- Phase 2 started early: P2a browser API contract (`internal/api`,
  `docs/rewrite/api-v1.md`, generated TS types, Node hub conformance
  fixtures) and P2b hub storage (`internal/hub/storage` on modernc SQLite).
- Commit policy: no Co-Authored-By or other attribution trailers.
- Later merges: W2a fixes (pointer-safe sanitization, failure filtering,
  panic isolation), W2b fixes (no WebSocket redirects, single sequence
  allocator, atomic queue reservations, collector-owned enrollment file,
  run-boundary gaps in Node hub history), GPU round 3 (per-device access
  arbiter: every hardware access holds a lease, quiet windows start only
  after a drained revocation, unknown runtime-PM state denied).
- Pending review: `rb/installer-v3` (installer round 3: FD-anchored
  uninstall, manifest v2, journal v2 with rollback substates), P2a API
  contract, GPU round 3 re-check. Sol reviews paused at 08:25 UTC on a
  provider usage limit (resets 09:15 UTC).
- **Phase 1 exit check met** on `go-rewrite` after merging the installer
  round 3 and W2d (`rigscope collector`): with
  `RIGSCOPE_TEST_COLLECTOR=go` the TS runtime suite against the Node hub
  passes 43/48, 0 failures, 5 documented skips (multi-hub fan-out, durable
  outbox). Go collector RSS with process detail active: 24 MiB vs 168 MiB
  for the Node collector; static binary 13 MB.
- Installer round 3 is merged but its review is still pending: do not run
  `rigscope install`/`uninstall`/`upgrade` from this branch on a real machine.
- Merged since: P2a browser API contract and P2b hub storage (both "merge
  after fixes"; fixes in progress), installer round 3 reviewed as "fix
  before real use" (round 4 in progress), GPU round 3 re-check: 6 of 8
  findings closed; the remaining kernel autosuspend race is accepted as a
  documented best-effort risk (round 4 narrows it and moves nvidia-smi
  leases to the reaper). W2d runtime review: rework in progress (bounded
  sampling worker, policy-filtered logs) plus harness follow-ups.
- P2c hub HTTP foundation (security middleware, embedded UI) in progress.
- Later merges: P2c hub HTTP foundation (Go hub serving the UI at 16.8 MiB
  RSS vs 147 MiB for the Node hub), P2a fixes (generated TS validators,
  cross-language conformance, 41 captured fixtures, 1.0 API decisions), P2b
  fixes (safe backups, replay horizon, coverage-aware ring, bounded queries,
  hashed collector credentials with a sealed recovery copy), W2d runtime
  fixes, harness follow-ups (each tightened test proven to catch a broken
  collector), installer round 4.
- GPU sampling closed after five review rounds: per-device access arbiter,
  reaper-owned leases for every child, fail-closed runtime-PM states
  (`unsupported` only with cached `D0`), panic-safe launch.
- Schema floor contract aligned across storage, `migrate` and the activation
  coordinator.
- In progress: installer round 5 (Unicode-safe mountinfo, hardened staging
  cleanup, publish-intent vs outcome, running-image identity, DB-aware
  `migrate`), P2c security fixes (proxy chain, plaintext ingress, CSP),
  P2d auth/sessions/admin, P2e collector ingestion + live SSE + history.
- **All-Go deployment runs the real UI.** After P2d (auth/admin), P2e
  (ingestion, live SSE, process interests, history) and the hub integration
  (one composition root, one database handle, durable sessions, recordings UI
  removed), the TS runtime suite passes in every combination with no
  failures: Node+Node 48/0/0, Node hub+Go collector 43/0/5, Go hub+Node
  collector 38/0/10, Go+Go 34/0/14 (skips documented in
  `docs/rewrite/test-matrix.md`). Go hub + Go collector use about 55 MiB RSS
  together with the UI open, against about 440 MB for 0.2.
- Reviews of P2d and P2e found race conditions (privilege re-checks inside
  transactions, atomic login/session creation, revocation fencing for
  collector sessions, atomic admission). Reworks are in progress, as are
  P2f alerts + webhooks, hub security round 3 and installer round 5.
- Merged since: installer round 5 (Unicode-safe mountinfo, mount-safe
  staging cleanup, observed publication outcome, running-image identity,
  DB-aware `migrate`), hub security round 3 (strict RFC 7239 parsing fuzzed,
  loopback admission, real `http.Flusher`, progress write deadlines), P2f
  alerts engine + webhooks (SSRF-pinned delivery; not yet wired into the
  hub). All-Go runtime suite still 34/0/14 after these merges.
- Sol reviews paused at 11:38 UTC on a provider usage limit (resets 14:15
  UTC). Queued: installer round 5 confirmation, P2f review, then the auth
  and ingest/live reworks and the release pipeline (P3a), all in progress
  on DeepSeek.
- Merged since: ingest/live rework (fenced collector sessions, atomic
  admission, eligibility before live publication, subscribe-before-snapshot
  SSE), auth/admin rework (in-transaction privilege checks, versioned atomic
  login, transactional revocation, additive migration 6), alerts wired into
  the hub (`/api/alerts`, live observations, 1 s tick), P3a reproducible
  release pipeline (6.3–6.9 MB tarballs per arch, byte-identical rebuilds,
  manual workflow only), P3b 1.0 user docs with a docs/CLI drift test, and
  hub URL normalization for `install --hub`. **Phase 2 is feature complete in
  Go.** Runtime suites: Go+Go 34/0/14, Go hub + Node collector 38/0/10, Node
  hub + Go collector 43/0/5.
- Remaining before 1.0: Sol reviews of the reworks, alerts, release pipeline
  and docs (queued for 14:15 UTC); then delete the TypeScript prototype
  (Node hub, collector, CLI, operations) and make the runtime harness
  Go-only; website copy (P3c, in progress, no deploy); manual steps in
  `docs/rewrite/releasing-v1.md` (real-machine smoke test after uninstalling
  0.2, GitHub Release, website deploy, npm deprecation).
- 2026-11-12, code complete for 1.0. Merged since the Phase 2 entry:
  - Local auto-pairing for default installs, then two review rounds:
    - the grant is bound to purpose and collector id and redeemable only
      from this host (loopback or a self-connection), never through a proxy
      or forwarded request (migration 7, additive, floor 5);
    - lazy import into a running hub, reinstall repair and doctor states;
    - origin plus TLS pairing and exact-host certificate checks.
  - Prototype removal: the TypeScript/Node runtime is deleted, and the
    runtime harness and CI are Go-only (35/0/0).
  - Installer round 6:
    - mounts at the removal root are refused;
    - migration holds the hub lease;
    - one shared hub URL policy, where `wss://` keeps its hostname and
      collector state matches credentials by canonical URL;
    - `.prev` publication is journaled;
    - readiness requires `active/running`.
  - Release readiness:
    - UI notices ship as a release asset;
    - allowlisted environments for e2e, release-smoke and the runtime
      harness;
    - the tag is pinned to the verified SHA;
    - actions and toolchains are pinned;
    - "Add machine" emits `install --hub/--token` or `collector enroll`;
    - TLS transition docs;
    - `install.sh` refuses BusyBox wget.
  - Alerts, three rounds:
    - durations come from receipt evidence;
    - ticks read only evaluated observations;
    - the committed horizon advances only after commit;
    - durable continuity breaks with bounded barriers;
    - shutdown admission accounting;
    - admin-scoped `GET /api/admin/diagnostics/alerts`.
  - Hub auth and ingest, four rounds:
    - account incarnation, with repair of empty incarnations at open;
    - session and scope re-checked inside every admin and alert write;
    - revocation tombstones, capped, with a revocation counter and a durable
      re-read;
    - credential-fenced telemetry commits;
    - exact credential invalidation;
    - live state replacement that keeps the successor row and reports the
      superseded session's disconnect once.
- Every area's last review returned "ship":
  - auto-pairing round 2: Opus;
  - hub round 4 follow-up: Opus;
  - release readiness: Sol confirmation, with its three P2s fixed directly
    after.
- The Sol reviews moved to Opus 5.5 (medium) once the Codex usage limit
  was reached.
- On `go-rewrite`, `go vet`, `go test -race ./...` and the UI tests
  (453/453) pass. The lifecycle e2e passes in about 3 minutes, covering
  install, auto-pairing, the hub-only to both flow, upgrade, rollback and
  uninstall. Two release builds are byte-identical, and `verify.sh` passes.
- Remaining are only the manual steps in `docs/rewrite/releasing-v1.md`:
  - a real-machine smoke test after uninstalling 0.2;
  - an ARM smoke test on hardware (no qemu here; `verify.sh` prints that ARM
    execution was not checked);
  - the GitHub Release with `--target` set to the verified SHA;
  - the website deploy;
  - npm deprecation.
- Integration note for Wave 2: W1c, W1d and W1e define local types that
  mirror the wire contract because `internal/protocol` was written in
  parallel. Wave 2's metrics assembler maps them to `internal/protocol` types
  and replaces the mirrored limit constants with the protocol ones.
