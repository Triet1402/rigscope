# Release notes

## 1.0.0 (unreleased)

Rigscope 1.0 is a ground-up Go rewrite. It replaces the 0.2 npm/Node prototype
with one static binary that contains the installer/CLI, the hub and the
collector. Nothing from 0.2 is imported.

### Highlights

- Rigscope 1.0.0 project sources are AGPL-3.0-only, with an embedded CLI
  license, hub source links and corresponding source alongside release binaries.

- System installation recognizes unchanged default 0.2 udev rules, previews
  replacement of required rules, and records ownership of the new 1.x files.
  Unused defaults do not block installation; modified rules remain protected.

- `rigscope configure --telemetry-mode memory|persistent` changes hub history
  storage after confirming the service restart; no config editing is needed.
- `rigscope configure` now covers everyday settings without editing JSON:
  `--share FAMILY=on|off` (sensitive families always ask first),
  `--sampling-interval`, `--gpu-registers off`, `--listen`,
  `--tls-cert/--tls-key` and `--no-tls` (checked for pair, expiry, names and
  service-account access before anything changes), `--forwarded-headers`, and
  `--hub on|off` / `--collector on|off`. Each change is previewed, validated
  whole, confirmed once and restarts only what it affects; collector-only
  machines are supported. `configure --show [--json]` prints the settings.
- Administration → Hub settings shows the running hub's address, HTTPS, proxy
  trust, storage use and the local collector's sharing, and its storage form
  gives the exact command to switch history storage on the hub machine.

- One static, `CGO_ENABLED=0` Linux binary for `amd64`, `arm64` and `armv7`.
  No Node.js, npm, Python, downloaded runtime tree or wrapper process. systemd
  supervises `rigscope hub` and `rigscope collector` directly.
- `curl -fsSL https://rigscope.dev/install.sh | sh` installs and starts a local
  hub and collector. A hub **Add machine** grant installs and enrolls a remote
  collector in one line.
- `install` creates the owner account itself: choose the password at the
  prompt (or `--password-stdin`), or get a generated one printed at the end of
  the install output. The hub no longer writes a password to the journal.
  `reset-password` also accepts a chosen password.
- Commands that need root ask `sudo` for your password themselves instead of
  telling you to run them again with `sudo`.
- `collector enroll` restarts a running collector so the new credential takes
  effect, and writes the credential owned by the collector account in a system
  install (it was left root-owned and unreadable by the service).
- Administration > Collectors can **Remove** a revoked collector, deleting its
  machine's name, tags, alert rules, incidents and all stored history. The
  registry also refreshes when a machine connects or drops out, so a
  re-enrolled collector no longer shows as Revoked until a manual refresh.
- The hub's grant panel shows one complete command with the grant in it, and
  `collector enroll` without `--grant` asks you to paste the grant.
- `start` says when a service is already running instead of claiming it
  started.
- Setting a public origin no longer cuts off the hub's own collector (every
  connection got `421 misdirected request`): the collector endpoints accept the
  loopback address from an unproxied local connection, so the local collector
  keeps its `127.0.0.1` destination and credential.
- `install --hub URL --token TOKEN` protects a collector that already works:
  it asks before moving it to another hub and redeems the grant first, keeping
  the old destination when the new hub refuses (before, the collector switched
  and the refusal only appeared in its log). With the hub it is already enrolled
  with, the grant is left unused; with this machine's own hub, the collector
  keeps its direct local address. `install` over an older version says
  `Upgraded Rigscope from OLD to NEW`.
- `configure --trusted-proxy LIST|none` sets the hub's trusted proxies without
  editing the configuration file. `install` and `configure` explain what an
  HTTPS origin still needs when the hub does not serve TLS itself, with the
  exact `tailscale serve` command for a `*.ts.net` origin, and no longer
  suggest a `--role collector` reinstall that would remove the hub. The hub log
  shows the address it actually listens on next to the public one.
- Re-running `install` without `--system` acts on an existing system
  installation instead of starting a second, per-user hub beside it, and keeps
  the installed components unless `--role` is given. The owner password is
  asked before anything is written.
- Commands find a system installation without `--system` when there is no
  per-user one, so `rigscope status` and `sudo rigscope reset-password` just
  work after `install --system`.
- Versioned configuration, versioned protocol and journalled upgrades with
  automatic rollback. 1.x upgrades preserve accounts, history, identities and
  settings in place.
- Bounded RAM summary history by default, with opt-in persistent SQLite
  history and a one-second RAM ring. RAM history is lost on hub restart;
  accounts, sessions, credentials and alert state remain durable.
- Persistent ingestion batches commits for up to five seconds and combines
  rollup updates per target bucket; live publication remains immediate.
- Uninstall removes owned hardware-access udev and sysusers files, including
  with `--keep-data`. Failed user-scope GPU rule removal or udev reload retains
  the installation and ownership record so cleanup can be retried.
- Add `cleanup-legacy-hardware` to preview leftover default 0.2 CPU-power and
  GPU-temperature access files. Root-only `--apply` removes recognized files
  and reloads udev after checking for remaining installations and workers.
- Commands never refuse because a service is running. `install` over a running
  installation, `configure`, `upgrade`, `rollback`, `reset-password` and
  `uninstall` name the services they must stop, ask `[y/N]` on the terminal
  (also under `curl | sh`), and start again only what was running; `uninstall`
  never restarts. `--yes`/`-y` approves for scripts; `--no-restart` leaves
  services stopped after `configure` and `reset-password`.
- Re-running `install` with a new `--port` or `--origin` now restarts the
  running hub and collector. Before, `systemctl start` left them on the old
  binary and settings while reporting them as started.
- `rigscope configure --port PORT | --origin URL | --local` changes the hub
  address of an existing installation. A locally paired collector is rebound to
  the new address with its existing credential.
- Colored terminal output for errors, warnings, status and doctor results,
  disabled by `NO_COLOR` or when output is not a terminal.
- Security gate: canonical Host and Origin checks, CSRF, secure cookies,
  CSP/HSTS/Referrer-Policy, login rate limits with argon2id, hashed collector
  credentials, replay-safe enrollment and SSRF-checked webhooks.
- Runtime-PM gating so the collector never deliberately wakes a suspended
  discrete GPU, with the documented residual race.
- NVIDIA junction and VRAM register temperatures work in user scope as well as
  system scope. A user install applies only the root-owned udev rule through
  `sudo`, grants the user's private primary group when it has one, and falls
  back to `rigscope-gpu` with a one-time full log-out otherwise. Sampling is
  enabled only after the installer can read BAR0 as the invoking user.

### Breaking changes from 0.2

- **No 0.2 data importer.** Accounts, history, recordings, settings, grants and
  collector credentials from 0.2 are not read. Remove the 0.2 installation with
  0.2's own uninstaller before installing 1.0. `rigscope install` refuses when it
  detects 0.2 units, state or `/opt/rigscope`.
- **npm is no longer a delivery channel.** `npx rigscope`, `npm install
  rigscope`, `rigscope try`, `rigscope connect` and the npm upgrade path are
  gone. 1.0 is not published to npm; the old package may be deprecated with a
  pointer to the installer.
- **No Node.js, npm or Python dependency.** Any 0.2 setup or automation that
  expected those runtimes must be removed.
- **Environment-variable configuration is gone.** `HUB_*`, `COLLECTOR_*` and
  `RIGSCOPE_*` settings are replaced by the versioned JSON configuration file.
  Unknown keys are errors. A small set of `RIGSCOPE_*` variables remains
  internal plumbing only (test/fixture overrides and the upgrade/lock
  handshakes); see [the configuration reference](docs/configuration.md).
- **One hub per collector.** 0.2's multi-hub fan-out and per-destination policy
  are removed in 1.0. The configuration keeps `destinations` as a list for a
  possible future return, but exactly one entry is accepted.
- **History configuration changed.** RAM remains the default; set
  `components.hub.telemetryMode` to `"persistent"` for durable history. Both
  modes use 10-second/1-minute/1-hour summaries plus a 15-minute, one-second
  RAM ring. The legacy raw mode, `HUB_TELEMETRY_MODE` and related environment
  variables no longer exist.
- **Recordings are removed.** The recording panel, routes, files, download links
  and recording limits are gone. Use a bounded History export for an incident
  snapshot.
- **One collector queue.** The durable SQLite outbox and
  `COLLECTOR_QUEUE_MODE=durable` are gone. The queue is bounded RAM (10 minutes
  or 64 MiB). Known drops are gaps; a crash or forced stop discards pending
  samples and is reported as unknown loss at the run boundary.
- **Remote access changed.** 0.2's Unix-socket remote mode and development
  HTTPS validation scripts are replaced by built-in HTTP(S) and one documented
  reverse-proxy setup with explicit `trustedProxies` and `forwardedHeaders`.
  Plaintext non-loopback listeners require a trusted proxy or TLS.
- **Sessions are durable.** Browser sessions are stored as token hashes in
  SQLite with a fixed 12-hour lifetime, so a hub restart no longer signs every
  browser out. Logout, expiry, account changes and collector revocation still
  end streams and detail leases immediately.
- **Install layout and CLI changed.** 0.2's per-install hashed units and multiple
  state directories are replaced by stable `rigscope-hub.service` and
  `rigscope-collector.service` units. New commands are `install`, `configure`,
  `uninstall`, `upgrade`, `rollback`, `status`, `start`, `stop`, `restart`,
  `logs`, `doctor`, `hub`, `collector`, `collector enroll` and
  `reset-password`. `enable`, `disable`, `try`, `connect`, `gpu-temps`,
  `cpu-power`, `system` and the `ops` database commands are removed.
- **Hardware access is an install decision.** NVIDIA junction/VRAM register
  access is enabled only by `install --gpu-registers`. A system install creates
  the `rigscope-gpu` group and the udev rule. A user install keeps running as
  the invoking user and applies only the root-owned rule through `sudo`: the
  rule grants the user's private primary group when it has one, otherwise the
  shared `rigscope-gpu` group and a full log-out is required before a reinstall
  can verify and enable sampling. `gpuRegisters` is recorded `true` only after a
  register read succeeds, and an existing working rule is adopted rather than
  rewritten. The `gpu-temps` and `cpu-power` commands are replaced.
- **System installs use dedicated accounts.** `rigscope-hub` and
  `rigscope-collector` run with no capabilities, hardened systemd units and
  private state subtrees. 0.2's system layout and environment files are not
  reused.
- **Owner password delivery changed.** The installer no longer prints the
  password. The first hub start prints it once to the hub journal; read it with
  `rigscope logs hub`. `rigscope reset-password` is the offline recovery path.
- **Default retention changed.** Persistent mode keeps 10-second summaries
  for 2 hours, one-minute summaries for 30 days and hourly summaries for
  400 days, with a 15-minute fine ring. RAM mode uses the same maximum ages,
  subject to its memory budget and the lifetime of the hub process. 0.2's
  tier durations and RAM budget variables do not apply.

### Upgrade and rollback

- `rigscope upgrade` downloads and SHA-256-verifies a release, backs up the
  database, configuration and collector identity, migrates, atomically replaces
  the binary, and verifies health. A failure before the new release accepts
  writes restores the backup and previous binary automatically.
- An interrupted transaction is detected and resumed or rolled back on the next
  run. Upgrades keep `rigscope.prev` and the newest three backup snapshots.
- `rigscope rollback` is a transaction with the same health checks. It refuses
  when the data is newer than the previous release can safely use (the schema
  floor) instead of risking corruption.
- Upgrade the hub first, then collectors. The hub accepts the current and
  previous protocol versions and marks older collectors as needing an upgrade.

### Security

- Exact canonical Host and Origin checks in remote mode; same-origin checks for
  loopback mutations. Absolute-form targets and `CONNECT` are rejected.
- Forwarded headers are trusted only from configured proxy addresses, only from
  the selected header family, and are parsed strictly; the other family is
  stripped.
- `HttpOnly`, `SameSite=Strict` cookies; `Secure` and `__Host-` over HTTPS;
  fixed 12-hour sessions; constant-time CSRF tokens.
- CSP with per-document script hashes, HSTS on HTTPS, `nosniff`,
  `Referrer-Policy: no-referrer` and `X-Frame-Options: DENY`.
- Argon2id password hashing with bounded verification, identical work for
  unknown usernames, and hub-wide, per-IP and per-username login limits.
- Collector credentials and enrollment grants are stored as SHA-256 hashes;
  grants are single-use, with a bounded retry window recovering a lost response.
- Webhook delivery validates the address actually dialed, refuses redirects and
  proxy environment variables, and blocks private, loopback, link-local and
  non-public targets.
- GPU access is read-only, lease-gated and isolated in a bounded helper process.

### Fixed in the rewrite

- Missing sensor values, unsupported hardware and failed reads are explicit
  unavailable values with reasons. The collector never sends a fabricated zero.
- Stable resource identities use GPU UUIDs and PCI addresses, not display order.
- The collector's installation ID is persistent; a cloned state directory
  regenerates it.
- Collector and hub logs are structured JSON on stderr for journald, with one
  plain readiness line on stdout.
- The static binary measures about 13 MB. Hub and collector together use about
  55 MiB RSS with the UI open, against roughly 440 MB for the 0.2 prototype.

## 0.2.6


- Let a revoked collector be re-enrolled. Administration listed only active
  machines as enrollment targets, and the hub refuses a new-machine grant for
  a collector it already knows, so a revoked machine could not come back.
  Revoked rows now offer **Re-enroll**, which creates a grant bound to that
  machine and shows the matching `rigscope connect … --replace` command.
- A collector whose credential the hub rejects (HTTP 401) now logs once how
  to re-enroll and retries every five minutes instead of every few seconds.
- `rigscope connect` restarts the collector it stopped when enrollment fails,
  instead of leaving it stopped. Its errors now name the cause: an already
  enrolled hub points to `--replace`, the eight-hub limit is reported on its
  own, and a 403 lists the usual reasons.
- The collector launcher enters every granted hardware group it is missing.
  `sg` adds one group per call, so a user granted both CPU power and GPU
  temperature access only received the first. Without `sg`, the warning now
  names every group and says a reboot applies them: startup that continues
  after logout keeps the user manager alive across logins.
- The GPU register-temperature access warning points to
  `rigscope gpu-temps status` instead of a source-tree file.

## 0.2.5

Released 2026-11-12.

- Start the collector even when `sg` is unavailable. 0.2.4 used the setuid
  `sg` helper to enter newly granted hardware groups without a re-login; on
  systems without that helper the collector failed to spawn and systemd
  restarted it forever. The collector now starts without the missing groups,
  logs one warning, and keeps telemetry running until the next login. A spawn
  failure also reports its underlying error instead of only "collector could
  not start", and a collector that never wrote a readiness marker is named as
  such rather than surfacing a stale lock path.
- Let laptop discrete GPUs power down. The collector polled `nvidia-smi`
  every two seconds, which kept an NVIDIA dGPU from ever entering runtime D3
  on battery (amdgpu DRM counter reads had the same effect). A GPU whose
  runtime-PM status is suspended is now left alone and reported as powered
  down, and an awake but idle offload GPU gets a short sampling pause so the
  driver can suspend it. Desktops, the boot display adapter and NVIDIA GPUs
  without Runtime D3 are unaffected.

## 0.2.4

Released 2026-11-09.

- User-scope install, collector enable and upgrade automatically reconcile CPU
  package-power and NVIDIA temperature permissions for the collector owner.
  A leftover service account no longer steals the user's RAPL grant. The
  launcher activates newly granted groups without a logout or reboot.
- Read NVIDIA memory BAR registers through read-only mappings instead of
  ordinary file reads, which Linux rejects with EIO. A bounded Python 3 helper
  isolates device faults; setup installs Python when missing on supported
  distributions. Add both RTX 5060 Laptop PCI IDs to the GDDR7 decoder.
- Use the latest CLI in the documented upgrade command so installations whose
  upgrader predates sensor setup receive the same automatic reconciliation.

## 0.2.3

- `rigscope gpu-temps enable|disable` and `rigscope cpu-power enable|disable`
  now request sudo themselves in an interactive terminal, so a new user runs
  the command once instead of reconstructing a sudo invocation. `sudo` replaces
  `PATH` with its own `secure_path`, which cannot find the installer's
  `~/.local/bin` launcher, so the old `sudo rigscope …` hint failed with
  `command not found`. Non-interactive runs change nothing and print
  `sudo env "PATH=$PATH" rigscope …` to run in scripts.

## 0.2.2

- Add `rigscope gpu-temps` and `rigscope cpu-power` commands
  (`status|enable|disable`): one sudo'd command installs the group, group
  membership and persistent udev rule that make NVIDIA junction/VRAM registers
  and CPU package-power (RAPL) counters readable by the unprivileged
  collector. `status` needs no root and answers by actually reading the
  sensors, printing the exact fix command when access is missing.
- Add NVIDIA **junction and VRAM temperature** monitoring. The collector reads
  the same undocumented thermal registers as
  [gddr6-core-junction-vram-temps](https://github.com/ThomasBaruzier/gddr6-core-junction-vram-temps)
  (GDDR6/GDDR6X therm+ADC, Blackwell thermal channels, GDDR7 DQR codes)
  through each GPU's sysfs BAR0 file. Pure TypeScript: no native helper, no
  NVML, no `/dev/mem` and no `iomem=relaxed`. Junction and VRAM join the GPU
  cards, History (`Junction`, `VRAM temp`), CSV/JSON exports and the new
  `GPU junction hot` / `GPU VRAM hot` alert presets. Reading BAR0 needs a
  one-time group grant; see
  [GPU monitoring](docs/monitoring.md#gpu-junction-and-vram-temperatures) and
  `packaging/system/90-rigscope-gpu-temps.rules`. Disable with
  `COLLECTOR_GPU_TEMPS=off`. Without the grant nothing changes.

## 0.2.1

- Make the collector's delivery queue RAM-only by default, mirroring the hub's
  RAM history. Routine delivery now writes nothing to disk: the previous design
  inserted, updated and deleted every sample in a per-destination SQLite file,
  which was a steady write stream even when the hub acknowledged promptly. The
  collector's readiness marker is also rewritten every five seconds instead of
  every second, and without a durability barrier.
- Add opt-in `COLLECTOR_QUEUE_MODE=durable` for operators who need unacknowledged
  samples to survive a collector restart. Both modes share the same
  10-minute/64 MiB per-destination bounds, live-slot priority, oldest-first
  eviction and coalesced gap records, so the hub still distinguishes "captured
  here and dropped" from "never captured". The mode is reported to the hub as
  `delivery.queueMode` and shown under a connection's delivery facts, because in
  the default mode a collector restart loses whatever was not yet acknowledged.
- Complete a one-time machine-data transition on the first `rigscope upgrade`
  after this release: the previous design's durable collector queue files plus
  the hub's telemetry rows, summaries, receipts, gaps, latest snapshots, series,
  inventory and machine-derived alert history are discarded once, even data an
  operator had previously opted into keeping. This is authorized specifically
  for the RAM-first transition: the components now in place keep history and
  the collector queue in RAM, so that on-disk copy is cold. It is recorded in a
  version-independent `machine-data-transition.json` marker written only after
  the activated release verifies healthy, so every subsequent upgrade preserves
  durable history and queues the operator opts back into. Accounts, passwords,
  sessions, collector enrollment and grants, settings, alert rules/jobs/webhook
  settings, machine display names/tags (`machine_meta`), and recordings are
  always preserved. The transition runs with all components stopped under the
  installation's leases; it is reversible until it completes — the queue tree is
  renamed aside rather than deleted, and a failed activation restores the
  cleared rows from the pre-upgrade database copy even when the schema was
  compatible — and a cleanup that cannot finish is reported as a visible warning
  and retried by the next upgrade instead of failing an otherwise healthy one.
  `rigscope system upgrade` performs the same transition.

## 0.2.0

- Make telemetry history RAM-only by default. Live readings update immediately,
  a global typed-array memory budget keeps 1s/10s/1m/1h summaries, and history
  deliberately disappears on restart. Users, credentials, enrollment, settings,
  alert state, metadata and inventory remain persistent.
- Add opt-in `HUB_TELEMETRY_MODE=persistent`: publish live state immediately,
  buffer roughly five seconds, commit one compact `points_10s` batch plus folded
  rollups, then acknowledge. Deduplication receipts now have an independent
  seven-day lifetime. Automatic checkpointing waits for a larger WAL reserve.
- Bound persistent delivery: use the protocol's 64-batch in-flight window,
  default the flush interval to five seconds and reject intervals above five
  seconds, which keeps a 100 ms collector cadence inside the window and the
  collector's 30-second ack deadline. A waiting live sample is sent as soon as
  an acknowledgement frees a slot, and the hub's collector message bucket covers
  live plus replay/status traffic instead of exactly the live rate.
- Add explicit recordings with bounded duration/size, optional recent RAM
  backlog, restart-surviving private output, authenticated download and a
  History panel that states whether history is temporary or saved.
- Add `ops reset-telemetry` to discard only telemetry history while preserving
  accounts, enrollment, settings, alert state, metadata, inventory and
  recordings.
- Add a repeatable RAM-vs-persistent benchmark measuring process-attributed disk
  writes, latency, RSS and row counts, plus tests for aggregation, nulls, gaps,
  spikes, late/duplicate samples, memory eviction, restart semantics, summary
  commits, replay deduplication, recording limits and history reset.

## 0.1.8

- Fix remaining hub stalls during retention by grouping per-series deletes into
  one durable transaction per bounded tier chunk. This avoids a separate disk
  sync for every series while preserving primary-key seeks and the row budget.
- Roll back the whole tier chunk if a delete fails, with regression coverage
  proving earlier series remain intact and the transaction is released.

## 0.1.7

- Fix hub stalls caused by age retention scanning entire point tables. Retention
  now deletes one series at a time through the clustered `(series, time)`
  primary key, so finding expired raw rows is milliseconds instead of seconds on
  a large history and no longer blocks ingestion or health requests.
- Add regression coverage that every retention tier uses its primary key instead
  of a full table scan.

## 0.1.6

- Hide navigation completely on narrow screens and open the shared sidebar in
  an overlay drawer. Include full machine and navigation labels, larger touch
  targets, and native dialog focus and Escape handling. Desktop navigation stays
  visible; the drawer closes after a selection or when resizing to desktop.
- Start History chart inspection on touch press and capture horizontal drags,
  while allowing vertical page scrolling. Preserve the cursor after touch release
  and place mobile tooltips above the graph. Apply touch inspection to per-core
  history rows as well as time-series charts.
- Refresh the rigscope.dev synthetic demo with the same shared mobile UI.

## 0.1.5

- Refresh the dark console palette and replace the sidebar machine selector with
  visible machine rows. Show collector status dots and CPU models, align machine
  names with navigation labels, and preserve the selected view when switching
  machines. Alerts, Connections and administrator-only Administration remain
  below the machine views; narrow screens retain initials and status indicators.
- Add a responsive showcase website with an installation command and copy button,
  documentation and contact links, and an interactive synthetic demo. The demo
  renders the actual Fleet, Overview, History and Processes components and product
  stylesheet through isolated routing and data adapters, without accessing a hub.
- Validate synthetic fixtures against product contracts and check the website
  build and types in the manual CI workflow to catch drift from the product UI.

## 0.1.4

- Coordinate stopping running user services and owned foreground processes for
  installation, configuration, enrollment and password recovery. Prompt before
  stopping; `--yes` approves without prompting and `--no-restart` leaves the
  installation stopped. Successful changes resume previously running components.
- **Uninstall now deletes saved data by default**, including accounts, history,
  collector credentials, pending samples and backups. Use `uninstall --keep-data`
  to retain data for reinstalling. `--purge` remains an explicit alias for default
  deletion. Uninstall never restarts processes.
- Add terminal colors for help, status, warnings and completed actions. Piped
  output and JSON remain plain; `NO_COLOR` disables terminal colors.
- Avoid extra History GPU cards created by empty historical PCI placeholder
  series. Preserve historical cards that contain readings. Use the system PCI
  name database when available, simplify fallback names and keep PCI addresses
  separate. Normalize NVIDIA PCI domain widths without merging distinct domains.
- Clarify unavailable GPU counters, including Intel integrated adapters, and
  avoid a last-known badge when no GPU reading is displayed.
- Update installation and operations documentation and add lifecycle, uninstall,
  terminal output and GPU regression coverage.

## 0.1.3

- Replace the unfinished versioned configuration formats with one canonical
  on-disk shape. `config.json` and `system-install.json` require a `components`
  object; collector state, `release-state.json`, `install-state.json`, release
  manifests, package metadata, export documents and hub enrollment state carry
  no format-version field. Adoption reads only a canonical `config.json`;
  `hub-config.json` and `collector.json` fallbacks are not read. There is no
  compatibility reader or in-place migration for the unfinished shapes.
- Reset pre-canonical installations instead of migrating them. Stop the owned
  hub and collector completely first: the latest CLI stages and validates the
  replacement release, then removes the old configuration and history and
  installs fresh. Run `npx --yes rigscope@latest upgrade` directly, because an
  older installed CLI cannot stage a package without the removed metadata.
  `rigscope install` and detected system installs reset the same way. Back up the
  state directory first if that history must be kept; there is no rollback to the
  old shape. Reset upgrades retain the installation preset, and interrupted
  resets cannot reuse the unfinished runtime on retry.
- Canonical upgrades still retain accounts, history, settings, collector
  identities and pending samples. Rollback still returns to the retained previous
  canonical runtime with its paired database backup. Database migration numbers
  in `schema_migrations` remain independent implementation details.
- Use one canonical collector endpoint and wire format. Remove collector-version
  markers, metric-extension negotiation, old-peer payload projection and the old
  endpoint/outbox migration. Hub and collector must use the canonical contract;
  reinstall existing collectors when updating an unfinished installation.
- Add `rigscope configure --port PORT` to change the local hub port while the
  hub and collector are stopped, updating the colocated collector destination and
  moving its outbox. `--origin` and `--port` cannot be combined; `--local
  --port` is allowed.

## 0.1.2

- Show History downtime shading as soon as the collector disconnects, and stop
  extending it on reconnect, including short outages.
- Detect missing-data intervals that cross the selected range's start. Align
  shading with plotted readings and preserve outage breaks during downsampling.
- Keep chart inspection tracking over the detail box, and hold the box in a
  stable position while retaining scrolling for long reading lists.
- Reduce inspection rendering work by caching chart geometry and summaries,
  sharing formatted core timestamps and coalescing pointer updates per frame.
- Preserve the per-core heatmap peak summary and keep inspection headers on one
  line so the heatmap does not jump when the cursor appears.

The full-fleet snapshot buffer limitation documented in 0.1.1 remains unchanged.

## 0.1.1

- Improve History chart behavior and add synchronized chart inspection.
- Add resource-guarded hub capacity testing with independent live-stream readers,
  two-minute confirmations and reproducible measurements.
- Include the capacity report and measurement JSON in the npm package so the
  scale-validation documentation links resolve after installation.

Known limitation: sufficiently large full-fleet snapshots can exceed the live
feed's 1 MiB buffer guard and disconnect viewers. The synthetic test crossed
that boundary at 530 machines; the exact count depends on payload size and scope.

## 0.1.0

Initial public release of Rigscope, a self-hosted Linux system and process monitor.

- Fleet and machine views with CPU, memory, swap, GPU, disk and network readings.
- Durable History, per-core charts, live process inspection and CSV/JSON exports.
- Threshold and availability alerts, HTTPS webhooks, users and machine scopes.
- Outbound collectors with independent sharing and durable retry queues for up to
  eight hubs per collector.
- Managed user installation, independent hub/collector services, enrollment,
  diagnostics, verified upgrades, rollback and offline password recovery.
- Optional system installation with dedicated unprivileged service accounts and
  narrowly scoped CPU package-energy access.

Requires Linux and Node.js 26+. Background user installation requires systemd.
NVIDIA and available AMD GPU counters are supported; Intel sensors are partial.
Missing or inaccessible readings remain unavailable. Remote process mutation,
hub federation and clustering are outside this release.

See [installation](docs/installation.md) for hardware limits and
[scale validation](docs/scale-validation.md) for measured synthetic workloads.
Database migration numbers are independent of the application version.
