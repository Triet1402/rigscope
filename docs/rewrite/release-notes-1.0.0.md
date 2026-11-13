# Rigscope 1.0.0

Local release draft. Publication is a separate operator action after the exact
candidate artifacts pass the checks in [releasing-v1.md](releasing-v1.md).

Rigscope 1.0 replaces the Node.js prototype with one static Go binary containing
the installer, hub and collector. Linux `amd64`, `arm64` and `armv7` are supported;
managed installations require systemd. Node.js, npm, Python and an external
database are not runtime dependencies.

## Highlights

- Project-owned sources are AGPL-3.0-only. Releases include the license and
  corresponding source; the hub provides a prominent source link. See [Licensing](../../LICENSING.md).

- Live machine and process monitoring, summary history, CSV/JSON exports,
  accounts, threshold alerts and HTTPS webhooks.
- Local collector pairing on a default installation, plus enrollment grants for
  additional machines. Each collector connects outbound to one hub.
- Bounded RAM telemetry by default. A hub restart loses telemetry history and
  inventory; accounts, sessions, credentials, machine names/tags and alert state
  remain durable. Run `rigscope configure --telemetry-mode persistent` for
  durable SQLite summary history; the command previews the change and handles
  the restart.
- Persistent commits batch for up to five seconds, with coalesced minute/hour
  rollup updates. Live readings publish immediately; durable acknowledgments
  wait for commit. RAM acceptance batches for up to one second.
- Versioned configuration and protocol, journalled upgrades, backups and
  schema-aware rollback for future 1.x releases.
- Uninstall removes owned CPU-power and GPU-temperature access rules and
  system-scope sysusers configuration, even when keeping application data.
  Failed privileged GPU cleanup preserves the installation for a retry.
- `cleanup-legacy-hardware` previews leftover default 0.2 udev/sysusers setup;
  root-only `--apply` removes recognized leftovers after the old installation
  has been uninstalled. Modified files and remaining installations are refused.
- Commands never refuse because a service is running: they name what must
  stop, ask `[y/N]`, and start again only what was running (`uninstall` never
  restarts). `--yes`/`-y` approves for scripts. Re-running `install` with a new
  port now restarts running services instead of leaving them on old settings.
- `rigscope configure` changes the hub address (rebinding a locally paired
  collector), listen address and TLS, proxy trust, history storage, collector
  sharing and sampling, GPU register sampling, and which components run. It
  previews and validates each change and asks once before restarting.
  `configure --show` lists the settings; Administration → Hub settings shows
  them in the dashboard. Terminal output is colored unless `NO_COLOR` is set.
- Hardened authentication, origin and CSRF checks, collector revocation, and
  SSRF-checked webhook delivery.
- GPU runtime power-management gates and optional NVIDIA junction/VRAM register
  temperatures, including user-scope access setup.

## Moving from 0.2

There is no 0.2 data importer. Archive anything needed outside the old
installation, uninstall 0.2 with its own uninstaller, then install 1.0 fresh.
Retained 0.2 state blocks the new installer. npm is no longer a delivery channel.

Recordings, a durable collector outbox and multiple hub destinations are not
included. The collector uses a bounded RAM queue; a crash can lose pending
samples, with restart gaps reported explicitly. Process detail is live only.

See [installation](../installation.md), [configuration](../configuration.md),
[telemetry storage](../telemetry-storage.md) and the full
[changelog](../../CHANGELOG.md) for setup, limits and compatibility details.

## Release assets

The release contains one tarball per supported architecture, `checksums.txt`,
the canonical `install.sh`, `LICENSE`, corresponding source, and both
third-party notices files. Verify checksums
before installing. The version compiled into every binary is `1.0.0`.

After publication, the installer command is:

```sh
curl -fsSL https://rigscope.dev/install.sh | sh
```

This draft does not indicate that the release or website installer has been
published.
