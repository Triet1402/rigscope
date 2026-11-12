# Release notes

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
