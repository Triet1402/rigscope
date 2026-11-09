# Configuration reference

For an installation made with `rigscope install`, use `rigscope config` to find
its private settings. See [Hub configuration](installation.md#hub-configuration)
and [Remote access](installation.md#access-from-another-computer) for managed setup.
The private `config.json` has one canonical shape: it has no version field, and
its required `components` object records hub and collector enablement.

The environment variables below apply to source development and services that
run the hub or collector directly. Setting them in your shell does not change
an installed background service. Keep private directories at mode `0700` and
configuration files at `0600`; do not commit them.

## Hub variables

| Variable | Purpose and default |
| --- | --- |
| `HUB_HOST`, `HUB_PORT` | Local listener: `127.0.0.1:8081`. Only literal loopback addresses are accepted. Port `0` selects an unused port for tests. |
| `HUB_PUBLIC_ORIGIN`, `HUB_SOCKET_PATH` | Remote access: set both the exact HTTPS origin and an absolute Unix socket path in a private directory. The hub then has no TCP listener. |
| `HUB_DATABASE_PATH` | SQLite database for control state (users, alerts and the collector registry) and, only when enabled, telemetry history. Defaults beside `HUB_STATE_FILE`, or under `$XDG_STATE_HOME/rigscope/hub/` (falling back to `~/.local/state`). |
| `HUB_TELEMETRY_MODE` | `memory` (default) keeps bounded history in RAM and writes no routine telemetry rows. `persistent` opts into compact SQLite summaries. See [telemetry storage](telemetry-storage.md). |
| `HUB_HISTORY_MEMORY_BYTES` | Global RAM budget for memory history. Default `134217728` (128 MiB). |
| `HUB_TELEMETRY_FLUSH_MS` | Persistent-mode write interval. Default and maximum `5000` ms (5 seconds); allowed 1000–5000. The bound keeps one commit inside the 64-batch collector window at the fastest supported 100 ms cadence and well below the collector's 30 s ack timeout. |
| `HUB_RETENTION_RAW_MS` | Persistent raw-mode retention. Default 2 days. Persistent summary mode does not write raw rows. |
| `HUB_RETENTION_10S_MS` | Ten-second summary retention in persistent mode. Default 2 hours. |
| `HUB_RETENTION_1M_MS` | One-minute rollup retention. Default 30 days. |
| `HUB_RETENTION_1H_MS` | One-hour rollup retention. Default 400 days. |
| `HUB_RECEIPT_RETENTION_MS` | Deduplication receipt lifetime, independent of chart retention. Default 7 days. |
| `HUB_GAP_RETENTION_MS` | Explicit collector gap retention. Default 400 days. |
| `HUB_RECORDINGS_DIR` | Private directory for explicit recordings. Default `recordings/` beside the hub database (0700 directory, 0600 files). |
| `HUB_RECORDING_MAX_DURATION_MS` | Maximum recording duration. Default 1 hour; maximum 24 hours. |
| `HUB_RECORDING_MAX_BYTES` | Maximum recording file size. Default 64 MiB; maximum 1 GiB. |
| `HUB_RECORDING_BACKLOG_MS` | Recent RAM history a new recording may include. Default 10 minutes. |
| `HUB_RECORDING_MAX_ACTIVE` | Concurrent active recordings. Default 4; maximum 32. |
| `HUB_BROWSER_USERS` | First-run users as JSON: `{username,passwordHash,machineIds,admin?}`. `machineIds` is `"*"` or a list; `[]` allows no machines. |
| `HUB_ADMIN_USERS` | Comma-separated usernames to make administrators during first import. If omitted, an existing bootstrap account named `owner` becomes an administrator. Other users need an explicit `admin:true`. |
| `HUB_COLLECTORS` | First-run collector credentials as JSON: `{collectorId,machineId,token}`. The limit applies to JSON bytes, not a fixed machine count. |
| `HUB_MAX_COLLECTOR_CONNECTIONS` | Maximum concurrent collector connections. Positive integer; defaults to `100`. Choose a value based on measured capacity. |
| `HUB_STATE_FILE` | Private enrollment file to import from an existing installation; also enables enrollment. Keep it with the deployment configuration for recovery. |
| `HUB_ENROLLMENT_GRANTS` | Initial grants as JSON: `{token,expiresAt}`. Expiry must be within 15 minutes. Used grants stay consumed after import and restart. |

### Telemetry history mode

The default `HUB_TELEMETRY_MODE=memory` keeps recent charts in a bounded RAM
budget and deliberately loses telemetry history when the hub restarts. Users,
credentials, enrollment, settings and alert records remain in SQLite.

Set `HUB_TELEMETRY_MODE=persistent` to keep compact ten-second summaries, with
one transaction roughly every `HUB_TELEMETRY_FLUSH_MS` (five seconds by
default). Collectors still acknowledge only after that transaction commits, so
the flush interval cannot exceed the collector's 64-batch in-flight window at
its advertised cadence; the hub rejects values above five seconds. See
[telemetry storage](telemetry-storage.md) for aggregation, deduplication,
recording and reset details.

Switching an existing installation to memory mode does not automatically delete
old telemetry rows. They are ignored, but can be reclaimed explicitly with
`ops reset-telemetry` after stopping the hub (see [operations](operations.md)).

### First-run users and credentials

The database imports bootstrap users and collector credentials once. Later
restarts do not overwrite managed users or restore revoked credentials. Use
**Administration** to change users after the first start.

The managed installer creates the initial account for you. From a source
checkout, use `scripts/hash-browser-password.cjs` to generate salted scrypt
hashes; it reads passwords from stdin. Keep hashes private.

## Collector variables

| Variable | Purpose and default |
| --- | --- |
| `COLLECTOR_MODE` | `real` by default. `mock` generates synthetic readings and never scans real processes. |
| `COLLECTOR_STATE_FILE` | Private saved identity, destinations and enrollment recovery proof. Defaults to `$XDG_STATE_HOME/rigscope/collector.json` (falling back to `~/.local/state`). |
| `COLLECTOR_INTERVAL_MS` | Sampling interval: `1000` ms by default, minimum `100` ms. GPU polling runs at most once per second. |
| `COLLECTOR_HUBS` | Explicit destinations as JSON: `{url,token,sharing?,intervalMs?}`, up to eight. Otherwise the collector uses saved enrollment. |
| `COLLECTOR_HOSTNAME` | Display metadata; defaults to the OS hostname in real mode. |
| `COLLECTOR_QUEUE_MODE` | Where unacknowledged samples wait: `memory` by default (RAM, no queue written to disk, lost on collector restart) or `durable` (a SQLite outbox per destination that survives a collector restart, at one database write per sample). Bounds are identical: 10 minutes and 64 MiB per destination. |

### Sharing settings

Each destination's `sharing` object accepts `cpu`, `memory`, `swap`, `gpus`,
`disks`, `network`, `networkAddresses`, `processes` and `processArguments`.
When supplying an object, CPU and memory must remain enabled.

Omitted fields default to true, except `swap` follows `memory` and
`processArguments` defaults to false. Managed enrollment starts with network
addresses and full process arguments disabled. Review a destination's saved
policy before expanding what it shares. Environment variables are never collected.

The collector applies sharing rules before queueing samples, again before
sending restored samples, and to each hub's process results. Restricting a policy
also restricts pending records.

## Source enrollment, rotation and replacement

For a managed installation, follow [Add machines and hubs](installation.md#add-machines-and-hubs)
using `rigscope connect`. The steps here require a source checkout with
development dependencies installed.

1. Create a short-lived grant through the hub's administration interface.
2. Set `COLLECTOR_ENROLL_URL` to the full `ws[s]://…/api/collector/connect` URL.
   Set `COLLECTOR_ENROLL_TOKEN` privately to the grant.
3. Run:

   ```sh
   npm run collector:enroll
   ```

4. Remove the grant from the environment and restart the collector.

The collector saves a random recovery proof before enrolling. If a successful
response is lost, retrying with that proof recovers the same credential. A
hostname or collector ID alone cannot recover it.

Rotation or replacement needs a grant bound to the existing machine on that
hub. Use `npm run collector:enroll -- --replace-enrollment` to replace a saved
destination. Revocation closes existing connections and invalidates old recovery
proofs. Replacement never identifies a machine by its hostname or address.
