# Operations

For an npm installation, use the managed commands below. The
[manifest archive workflow](#low-level-manifest-archive-workflow) is for operators
building and deploying from source.

| Task | Section |
| --- | --- |
| Check, upgrade or roll back an npm installation | [Managed CLI operations](#managed-cli-operations) |
| Reset a lost password | [Password recovery](#password-recovery) |
| Change localhost/HTTPS access | [Access changes](#access-changes) |
| Back up or restore a database | [Database commands](#database-commands) |
| Plan what to back up | [Consistent backup set](#consistent-backup-set) |
| Handle a full database or queue | [Storage pressure recovery](#storage-budgets-and-pressure-recovery) |

## Managed CLI operations

These commands apply to an installation created by `rigscope install`. Its
state directory defaults to `~/.local/state/rigscope`.

### Check health

```sh
rigscope status
rigscope doctor
```

`status` shows services and the dashboard URL. `doctor` checks configuration,
runtimes, database integrity, listeners, TLS destinations, disk space and sensor
access. It follows enabled components: a hub-only installation does not need a
collector. Checks are read-only and omit credentials. Use `doctor --json` for
structured output.

### Upgrade

```sh
rigscope upgrade
```

Use `--from PACKAGE` for an exact npm version or local archive. The command
stages the package, stops running components, takes a consistent hub database
backup and activates the new runtime. It restarts only the components that were
running, then checks:

- the installed and running versions match the new package;
- restarted services are active;
- hub health is OK and its database passes integrity/schema checks;
- a restarted collector produces a fresh sampling marker.

The health timeout is 25 seconds; change it with `--timeout SECONDS`. A failed
start, migration or health check restores the previous runtime. It also restores
the paired database backup if the old runtime cannot read the new schema,
preserving the replaced database first.

Credentials, collector identities and pending samples are retained.
`release-state.json` records which backup belongs to each runtime. Upgrading a
stopped installation leaves it stopped and reports live checks as skipped.

### Roll back

```sh
rigscope rollback
```

Rollback uses `runtime-previous` and runs the same health checks. If that runtime
can read the current database, post-upgrade data stays intact. Otherwise it
restores the paired pre-upgrade backup and saves the replaced database.

A missing compatible backup stops rollback. Older installations without a
recorded backup can supply a compatible copy with `--backup PATH`. Never point
old code at a newer database schema.

## Password recovery

Stop the installation, enter a new password at the private prompt, then restart:

```sh
rigscope stop
rigscope recover-password --user owner
rigscope start
```

Recovery requires the hub to be stopped and refuses a live or uncertain database
lock. For unattended recovery, use `--password-file` with an owned `0600` file.
It updates the existing user's stored hash. `--enable` also clears that user's
disabled flag; recovery never creates a user or grants administrator access.

The operation records an audit event and invalidates sessions. It keeps
configuration, history and collector credentials. Sign in again after restart;
editing bootstrap hashes in `config.json` or `HUB_BROWSER_USERS` does not reset a
managed account.

## Access changes

Follow the commands in [Remote access](installation.md#access-from-another-computer).
`configure` requires a stopped hub and, when changing its local collector's
destination, a stopped collector.

It validates the HTTPS origin and private socket, backs up the old settings, and
updates the matching local collector destination. It moves that destination's
whole queue directory, including SQLite WAL files. Credentials and other hubs
stay unchanged. An existing target queue stops the conversion to avoid merging
or discarding pending records. Accounts and the hub history database are retained.
Rollback targets must support the saved configuration and database schema.

## Low-level manifest archive workflow

The remaining archive deployment examples assume a source checkout with built
artifacts. They use a manifest `.tar.gz`, which is different from the npm `.tgz`
used by `rigscope install` and `rigscope upgrade`. For ordinary installations,
follow [installation](installation.md) and the managed commands above. The npm
package excludes source helper scripts and does not require users to build it.

The built deployment runs `dist/hub.cjs` for the dashboard and collector
connections, and `dist/collector.cjs` for collection. `dist/ops.cjs` provides the
database and archive tools below. The hub serves the production Next.js output
from `.next`; it does not use `next start` or standalone output.

Choose a prefix and state directory owned by the service user. All paths below
are examples; generated units support paths containing spaces.

### Build and package

```sh
npm ci
npm run build:ui          # production .next
npm run build:runtime     # dist/hub.cjs, dist/collector.cjs, dist/ops.cjs
node scripts/package-release.cjs --output /var/tmp/rigscope-release.tar.gz
```

The archive contains the built runtime, `.next` without its cache, package and
lock files, the repository's `next.config.js`, `tsconfig.json`,
operational docs and the `packaging/` examples. It deliberately excludes source, tests, git data,
`node_modules`, private environment files, SQLite databases, keys and logs.
`manifest.json` lists every packaged file with size and SHA-256; the installer
re-verifies it before activation.

Production dependencies are installed inside each staged release:

```sh
cd /home/operator/.local/opt/rigscope/releases/<version>
npm ci --omit=dev
```

`install --install-deps` can run that command explicitly, but the installer
never runs npm implicitly for activation. If a package mirror is unavailable,
stage without activation and install from an approved mirror; do not weaken npm
security policy just to make installation pass. Isolated tests use a synthetic
dependency tree and do not perform remote installs.

## Stage and activate

```sh
node scripts/install-release.cjs \
  --source /var/tmp/rigscope-release.tar.gz \
  --prefix /home/operator/.local/opt/rigscope \
  --state-dir /home/operator/.local/state/rigscope \
  --units-dir /home/operator/.config/systemd/user
```

The installer creates:

```text
<prefix>/
  releases/<version>/          immutable built release + manifest
  releases/<version>/node_modules/
  current -> releases/<version>
  previous -> releases/<previous>   (retained for rollback)
<state>/
  hub/telemetry.sqlite          hub database (0700 dir, 0600 file)
  hub.env                       private EnvironmentFile (0600, no secrets printed)
  collector.env                 private EnvironmentFile (0600)
  collector/collector.json      collector identity/enrollment (created by collector)
  outbox/<sha256(url)[0:16]>/   per-destination collector outboxes
  backups/                      private pre-upgrade/pre-activation database copies
  install-state.json            versions, schema backups, no credentials
  run/                          private runtime directory for a Unix socket
<units-dir>/
  rigscope-hub.service
  rigscope-collector.service
```

Staging does not switch `current` or start anything. After production
dependencies are installed and the old hub is stopped, activate explicitly:

```sh
node scripts/install-release.cjs activate \
  --prefix /home/operator/.local/opt/rigscope \
  --state-dir /home/operator/.local/state/rigscope \
  --version <version> --confirm-stopped
```

Activation order is fixed:

1. Validate the release manifest, hashes and production dependencies.
2. Verify no hub lock exists and the operator confirmed the hub is stopped.
3. Inspect the current database schema.
4. If migration is needed: write a consistent online backup, then migrate, then
   verify integrity and the target schema. A failed migration restores the
   pre-migration backup and leaves `current` unchanged.
5. Atomically switch `current`, retaining `previous` and a schema-backup record.
6. Print (never execute) systemd commands.

If validation or migration fails, a good database is never silently replaced.
The pre-migration backup path is printed. First installation with no database
creates and migrates a new private database.

## systemd user units

The generated units use an absolute Node 26 executable, absolute paths,
`WorkingDirectory=<prefix>/current`, a private `EnvironmentFile`, journald
logging, `Restart=on-failure`, `NoNewPrivileges=yes`, `PrivateTmp=yes`,
`UMask=0077`, `ProtectSystem=strict`, and explicit writable state/database/cache paths.

Startup is explicit and is not performed by the installer:

```sh
systemctl --user daemon-reload
systemctl --user enable --now rigscope-hub.service rigscope-collector.service
```

Automatic startup after logout/host reboot requires an explicit administrator
choice to enable user lingering for that account:

```sh
sudo loginctl enable-linger operator
```

Review the generated units and private env files first. Health and observation:

```sh
curl --fail --silent http://127.0.0.1:8081/api/health
systemctl --user status rigscope-hub.service rigscope-collector.service
journalctl --user -u rigscope-hub.service -f
journalctl --user -u rigscope-collector.service -f
```

Shutdown is bounded: the hub targets five seconds and the collector three
seconds; the generated `TimeoutStopSec` leaves a ten- and seven-second slack
respectively. Keep those times at or above the process deadlines.

## Configuration

`RIGSCOPE_OPS_DEBUG` enables operation diagnostics; keep diagnostic output private. `RIGSCOPE_HUB_LOCK_FILE` overrides the hub lease path when needed; use the same path for the hub and operations commands.

The generated `hub.env` and `collector.env` contain only comments. Empty
configuration fails closed: no `HUB_BROWSER_USERS` means browser data is
unavailable, and no collector credentials/destinations means no fleet data.
Never copy the public demo credentials from the examples to a real host.

Loopback development:

```sh
HUB_HOST=127.0.0.1
HUB_PORT=8081
HUB_DATABASE_PATH=/home/operator/.local/state/rigscope/hub/telemetry.sqlite
```

Browser users and collectors belong in the private environment file. Generate
browser password hashes locally with `scripts/hash-browser-password.cjs`; never
store plaintext passwords or hashes in shared logs.

Remote mode uses a private Unix socket behind an external TLS terminator:

```sh
HUB_PUBLIC_ORIGIN=https://monitor.example.invalid
HUB_SOCKET_PATH=/home/operator/.local/state/rigscope/run/hub.sock
```

The parent directory must be private and owned by the service user. The
installer's state `run/` directory is already writable by the unit. Remote
health checks go through the socket with the exact Host:

```sh
curl --fail --silent --unix-socket <socket> \
  -H 'Host: monitor.example.invalid' http://localhost/api/health
```

This example hostname and every `demo-token-*` value are public fixtures, not a
deployment. See `development.md` for the exact origin/Host/cookie
boundary. Never expose the loopback HTTP listener remotely without the private
socket/TLS boundary and authentication.

Collector enrollment remains separate from browser sessions:

```sh
COLLECTOR_ENROLL_URL=ws://127.0.0.1:8081/api/collector/connect
COLLECTOR_ENROLL_TOKEN=<one-use grant>
COLLECTOR_MODE=real
COLLECTOR_STATE_FILE=/home/operator/.local/state/rigscope/collector/collector.json
```

Remove the one-use grant after enrollment and start or restart the collector. The
hub does not need a restart. Enrollment state
and outboxes live outside the repository.

For an isolated local check without touching the real deployment, the public demo
grant from the packaging examples can drive a loopback enrollment and collection
run. These are deliberate demo strings only, never a real deployment:

```sh
HUB_STATE_FILE="$PWD/.rigscope-local/hub/enrollment.json" \
HUB_ENROLLMENT_GRANTS="$(node -e 'console.log(JSON.stringify([{token:"demo-token-enrollment-local",expiresAt:new Date(Date.now()+300000).toISOString()}]))')" \
npm start

# In another terminal:
COLLECTOR_STATE_FILE="$PWD/.rigscope-local/collector/state.json" \
COLLECTOR_ENROLL_URL='ws://127.0.0.1:8081/api/collector/connect' \
COLLECTOR_ENROLL_TOKEN='demo-token-enrollment-local' \
npm run collector:enroll

# Start real local collection with the saved credential:
COLLECTOR_STATE_FILE="$PWD/.rigscope-local/collector/state.json" npm run collector:start
```

Enrollment posts `{collectorId}` to `/api/collector/enroll`; the bearer grant travels
only in the Authorization header. Grants must expire within fifteen minutes and are
consumed across restarts. Plaintext is allowed only for literal loopback addresses;
WSS enrollment uses HTTPS with ordinary certificate validation.

## Database commands

All commands take explicit paths. They print no credentials, user hashes,
webhook URLs, tokens or record payloads.

```sh
# Read-only schema/integrity/size/table summary.
node dist/ops.cjs inspect --database /path/telemetry.sqlite

# Consistent single-file online backup. Does NOT migrate the source.
node dist/ops.cjs backup \
  --database /path/telemetry.sqlite \
  --output /path/private-backups/telemetry-$(date +%Y%m%dT%H%M%S).sqlite

# Offline guarded migration. Existing databases require --confirm-stopped.
node dist/ops.cjs migrate \
  --database /path/telemetry.sqlite \
  --backup-dir /path/private-backups --confirm-stopped

# Guarded restore over an existing database: both flags are required.
node dist/ops.cjs restore \
  --input /path/private-backups/telemetry-....sqlite \
  --database /path/telemetry.sqlite \
  --replace --confirm-stopped

# Offline managed-user recovery; password comes from a private 0600 file.
node dist/ops.cjs recover-admin \
  --database /path/telemetry.sqlite \
  --user owner --password-file /path/private-recovery-password --enable
```

Safety rules enforced by `backup`/`restore`:

- The hub database is one SQLite file. The online backup copies all tables in
  that file: telemetry, history rollups, receipts/gaps, machine latest and
  inventory, the collector registry/grants/audit, admin users/audit, and alert
  rules/incidents/jobs/webhook state. There is no partial table selection.
- Backup opens the source read-only, runs `integrity_check`, stages the copy,
  converts it to a self-contained DELETE-journal file, verifies the staged copy,
  sets mode 0600 and atomically renames it. The source schema is never migrated.
- Restore validates the input integrity and migration ledger, refuses a schema
  newer than this operations build, refuses to overwrite the target without
  `--replace --confirm-stopped`, and refuses if a hub lock is present.
- It stages and verifies a copy first. For an existing target it checkpoints the
  old database, takes a pre-restore backup, then removes only the old WAL/SHM
  sidecars after proof that the hub is stopped, and atomically replaces the
  main file. A failed validation never overwrites the target.
- The hub process lease is `<databasePath>.hub.lock` (JSON `{pid,instance}`).
  Operations refuses while it exists, even if the PID looks stale, because only
  the hub's own startup proof may recover a proven-dead lease. The hub removes
  it on clean shutdown/exit; operators must inspect a lock and never delete it
  blindly.
- Restoring an older-schema backup is allowed; the matching release migrates it
  on the next hub start. Never start an old release against a newer schema.

Schema versions are derived from the build's migration ledger. The current
ledger includes telemetry, latest-session identity, admin, alerts and collector
registry migrations. A newer database must be opened by newer code; an old
release must never be pointed at a newer schema. `inspect` reports this state.

## Consistent backup set

The database backup is not the whole installation. A restorable set is:

1. Hub SQLite database: `telemetry.sqlite` (includes registry/admin/alerts).
   Use `ops backup`; do not copy the main file alone while the hub is running.
2. Enrollment/config state when configured: `HUB_STATE_FILE` (for
   example `hub/enrollment.json`, the import source for previously enrolled
   collectors) and the private hub environment file. These
   contain credentials: protect and copy them only between private locations.
3. Collector identity/enrollment: `collector/collector.json`.
4. Collector outboxes: `outbox/<destination-hash>/outbox.sqlite` per hub URL,
   which may contain unacknowledged telemetry. Replay is at-least-once; the hub
   deduplicates by collector/run/sequence.
5. Optional operational metadata: `install-state.json`, units and the built
   release. Keep the release matching the database schema for rollback.

For a consistent set, prefer stopping both services cleanly, then run the online
database backup and copy the other private files. For an online backup, the SQLite
copy is internally consistent, but JSON identity/outbox files should be copied in
a quiescent moment; outbox copies may lag and replay duplicates are harmless.

Restore the database with `ops restore`; copy non-database state back separately
with the same ownership and modes. Never silently copy another installation's private keys, browser credentials or unrelated files.

## Upgrade and rollback

For an upgrade:

1. Package and stage the new release; install `npm ci --omit=dev` there.
2. Stop the old hub/collector and verify the lock is gone.
3. Run `install ... --activate --confirm-stopped` (or `activate`).
4. The installer backs up, migrates and verifies before switching `current`.
5. Start/enable services explicitly and check health. If the services were
   already installed:
   `systemctl --user restart rigscope-hub.service rigscope-collector.service`.

Rollback:

```sh
node dist/ops.cjs rollback \
  --prefix /home/operator/.local/opt/rigscope \
  --state-dir /home/operator/.local/state/rigscope \
  --confirm-stopped
```

Rollback is refused while the hub lock exists. If the database schema is newer
than the previous release's manifest, rollback restores the recorded matching
pre-upgrade backup (verified against that release's schema) before switching
`current`. If no matching backup exists, it refuses rather than claiming an old
binary can read a newer schema. The replaced database is itself backed up first.

### Upgrade ordering

Within a user installation or this manifest archive layout, hub and collector
share one release: upgrade the installation together. Stop the hub and
collector, activate the new release (which migrates and verifies the database),
then start both services from the same release. Avoid running an old collector
against a newer hub or vice versa for longer than the restart itself. Do not roll
a hub back below the schema its database already uses; `rollback` enforces that
boundary, and collector rollback keeps its private state and outboxes. Process
detail remains request-scoped and is never persisted or included in the backup
set. Separate machines and hubs are upgraded independently. System installations
use the component controls in [System installation](system-installation.md#system-component-lifecycle).

On upgrade from the previous collector endpoint, saved `/api/collector/v1/connect`
URLs normalize to `/api/collector/connect` without changing identity, credentials,
sharing policy or pending enrollment recovery proofs. The collector recovers the
old URL's outbox before connecting. If both queue directories exist, it imports
missing record identities in a transaction before removing the old directory.
Stored metrics and gaps with the previous `protocolVersion: 2` envelope are
decoded into the current wire format at the outbox boundary; record identities
and previously offered gap payloads are preserved. Wire parsers remain strict.

## Measurement

`ops measure` is a bounded synthetic runner, not a capacity guarantee:

```sh
node dist/ops.cjs measure --machines 2 --batches 30 --records-per-batch 5 \
  --gpus 2 --viewers 2 --json
```

It writes synthetic records through the real `TelemetryStore` into a
temporary private database and reports accepted/rejected/busy counts, ingestion
latency, event-loop delay/utilization, RSS, database/WAL size, store queue and
flush counters, and one history-query latency. The output is labelled synthetic.
Defaults are intentionally small; upper bounds are 1,000 machines, 2,000 batches,
32 records per batch and 100,000 records total. It never opens a service socket
or contacts a hub. `--keep` retains the temporary directory for inspection. The
library accepts `historyConcurrency` for overlapping History request rounds;
the CLI exposes it as `--history-concurrency N` (default 1).

Current integration notes: storage commits batches of at most 32 records and the
history query has a 25,000-row/100 ms budget; a 512-record synthetic benchmark
showed roughly 407 ms transactions. Measure on the actual deployment before
choosing cadence, retention or viewer counts. No latency or capacity guarantee
is implied by these development numbers.

For sustained concurrent viewers, collector reconnects/replay, slow SSE
consumers and a fleet beyond 50 machines, use the isolated harness described in
[scale-validation.md](scale-validation.md). It spawns a temporary loopback hub
and never contacts the live service.

## Verification limits

Default repository tests exercise backup/restore, newer-schema refusal, corrupted input,
lock refusal, installer staging/activation/rollback and failed-upgrade recovery
in isolated temporary directories. CLI fixture tests additionally cover doctor
redaction and role separation, inactive and live upgrade validation, paired
rollback restore/refusal, compatible rollback without data replacement, and
offline password recovery with lock refusal and audit. They do not start
services, enable units, touch Tailscale, use real credentials, modify developer
state, or perform npm network installs; fixture dependency trees stand in for
production `npm ci`.
Production visual/render verification and integrated runtime tests are separate.
The opt-in real-service checks in [releasing](releasing.md#verify-the-exact-archive)
do install disposable systemd user services and production npm dependencies;
they remove their test units afterward and use separate state and launchers.

Pass `--node /absolute/path/to/node` only when the unit must use a Node 26
executable different from the one running the installer. The path must be an
absolute regular file; use the release's `node_modules` installed with
`npm ci --omit=dev`, never a development-only tree.

## Storage budgets and pressure recovery

Each Collector Hub connection owns policy filtering, cadence, inventory and its
durable outbox submission. On restart, recovered metrics are sanitized with the
current sharing policy before the connection starts; capture-time cadence is not
reapplied to retained records. Endpoint upgrade recovery and aggregate budgets
continue to use the existing outboxes. Hub startup restores stored telemetry,
cadence, inventory and metadata through Sample ingestion while retaining a
disconnected state until an authenticated Collector reconnects.

The default hub main-file plus WAL budget is 2 GiB, shared by every SQLite table.
The main file has a page-count limit; WAL reserves up to 128 MiB inside that budget.
Telemetry writes return busy on pressure or SQLITE_FULL, with no receipt/ack for a
failed transaction. Retention reclaims old raw/one-minute points in bounded chunks
while preserving the newest hour; hourly rollups retain their configured age window.
A full newest-hour working set must fit the deployment's budget. Outboxes retry
while the operator frees space or provisions a larger supported configuration.

Collector defaults are 64 MiB logical JSON and 80 MiB physical main+WAL per hub,
256 MiB logical and 640 MiB physical across eight destinations. Physical pressure
evicts only pending metrics, records dropped intervals, and protects in-flight
records. Failed gap writes are held in bounded memory and retried after recovery;
an abrupt power loss can still lose that in-memory notice.

New databases use incremental page reclamation. Existing databases created without
auto-vacuum may keep allocated pages after retention deletes rows. Startup reports
pressure and limits growth; it never runs an unbounded VACUUM. Back up the database,
stop the hub and inspect file/WAL sizes before offline maintenance. These are
application/SQLite budgets rather than an operating-system filesystem quota. An
atomic transaction can briefly increase WAL before its post-commit checkpoint;
blocked checkpoints leave pressure visible and prevent continued ingestion. Keep
free disk space for backups, SQLite sidecars and release artifacts.

Collector state and enrollment file locks now contain an owner PID and instance.
A proven-dead owner is recovered with inode checks. Legacy empty `.lock` files have
no owner proof and intentionally require one manual inspection: verify no collector
or enrollment writer is running before removing that specific abandoned file.


Generated user units omit `ProtectKernelModules=yes`: on the target user manager,
that directive fails before Node starts with `218/CAPABILITIES`. Normal user
privileges and `NoNewPrivileges=yes` already prevent gaining module-loading
privileges. PrivateTmp, ProtectSystem=strict, explicit writable paths and the other
validated user-unit hardening remain enabled. Do not copy root-only hardening into
user units without starting and stopping an isolated unit on that host.
