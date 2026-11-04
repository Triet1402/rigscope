# Configuration reference

For an installation made with `rigscope install`, use `rigscope config` to find
its private settings. See [Hub configuration](installation.md#hub-configuration)
and [Remote access](installation.md#access-from-another-computer) for managed setup.

The environment variables below apply to source development and services that
run the hub or collector directly. Setting them in your shell does not change
an installed background service. Keep private directories at mode `0700` and
configuration files at `0600`; do not commit them.

## Hub variables

| Variable | Purpose and default |
| --- | --- |
| `HUB_HOST`, `HUB_PORT` | Local listener: `127.0.0.1:8081`. Only literal loopback addresses are accepted. Port `0` selects an unused port for tests. |
| `HUB_PUBLIC_ORIGIN`, `HUB_SOCKET_PATH` | Remote access: set both the exact HTTPS origin and an absolute Unix socket path in a private directory. The hub then has no TCP listener. |
| `HUB_DATABASE_PATH` | SQLite database for telemetry, users, alerts and the collector registry. Defaults beside `HUB_STATE_FILE`, or under `$XDG_STATE_HOME/rigscope/hub/` (falling back to `~/.local/state`). |
| `HUB_BROWSER_USERS` | First-run users as JSON: `{username,passwordHash,machineIds,admin?}`. `machineIds` is `"*"` or a list; `[]` allows no machines. |
| `HUB_ADMIN_USERS` | Comma-separated usernames to make administrators during first import. If omitted, an existing bootstrap account named `owner` becomes an administrator. Other users need an explicit `admin:true`. |
| `HUB_COLLECTORS` | First-run collector credentials as JSON: `{collectorId,machineId,token}`. The limit applies to JSON bytes, not a fixed machine count. |
| `HUB_MAX_COLLECTOR_CONNECTIONS` | Maximum concurrent collector connections. Positive integer; defaults to `100`. Choose a value based on measured capacity. |
| `HUB_STATE_FILE` | Private enrollment file to import from an existing installation; also enables enrollment. Keep it with the deployment configuration for recovery. |
| `HUB_ENROLLMENT_GRANTS` | Initial grants as JSON: `{token,expiresAt}`. Expiry must be within 15 minutes. Used grants stay consumed after import and restart. |

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
