# Release notes

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
