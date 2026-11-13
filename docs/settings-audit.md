# Settings access audit

Audited against the JSON schema in `internal/config/config.go`, CLI setup and
service commands, hub administration routes, and the Administration and Alerts UI.
All high- and medium-priority gaps from the 1.0 audit now have a supported
control. One later item remains.

## Implemented

| Setting / user task | Control |
| --- | --- |
| Hub history storage | `configure --telemetry-mode memory\|persistent`. Collector buffering is RAM-only by design; there is no collector storage mode. |
| Sharing: CPU, memory, swap, GPUs, disks, network, addresses, processes, arguments | `configure --share FAMILY=on\|off` (repeatable or comma-separated), applied by the collector before anything leaves the machine. Turning on `network-addresses` or `process-arguments` explains what becomes visible and always asks, even when no service restarts; opt-in is judged on the effective policy, so turning `processes` back on with arguments already on asks too. The hub cannot change this policy. |
| Remote hub: listen address, direct TLS | `configure --listen ADDRESS`, `--tls-cert FILE --tls-key FILE`, `--no-tls`. The result must pass the hub's own startup rule (non-loopback needs an origin and TLS or a trusted proxy; TLS needs an `https` origin). The pair must load, be unexpired and cover the origin name; in system scope the `rigscope-hub` account must be able to search every directory and read both files, and paths under `/home`, `/root`, `/run/user`, `/tmp` and `/var/tmp` are refused because the unit hides them. The preview explains restarting after certificate renewal. |
| Enable or disable local collection or dashboard | `configure --hub on\|off`, `--collector on\|off`. Off stops and disables the unit and keeps its data; on enables and starts it, pairs a local collector automatically and, for a hub that never ran, sets up the owner account (`--password-stdin` supported). Turning both off is refused. |
| Sampling frequency | `configure --sampling-interval DURATION` (100ms–1h), with a cost note below 1s. |
| Reverse proxy header family | `configure --forwarded-headers x-forwarded\|forwarded`. |
| Disable GPU register sampling | `configure --gpu-registers off`. The udev rule, group and unit group stay provisioned in both scopes; `install --gpu-registers` turns sampling back on and uninstall removes the access. Turning it on stays an installer workflow because it provisions permissions. `doctor` reports the kept rule as off, not as working. |
| Discover settings | `configure --show` (readable, with the supported changes) and `--show --json` for scripts. Both read only the configuration and service state, never grants, credentials or passwords. Administration → Hub settings (`GET /api/admin/settings`, admin-only, read-only) shows the running hub's address, HTTPS and proxy trust, storage, and the local collector's saved sampling and effective sharing policy (reloaded on refresh; applied when the collector starts); it omits TLS paths and withholds the local collector's policy from machine-scoped administrators. |
| Choose hub history storage from the dashboard | Administration → Hub settings → History storage shows the mode, disk and RAM use, oldest history and the loss warning for a switch, then hands off the exact scope-correct `configure --telemetry-mode` command. The hub gets no write route, no root privilege and no collector-state access: it cannot rewrite its configuration or restart itself, and the CLI is the supervisor that previews, confirms and restarts. |

Every setter shares one pipeline. It previews the effective change, validates
the complete result before writing or stopping anything, asks once before a
service interruption or sensitive opt-in, writes only the named keys, supports
`--yes` and `--no-restart`, and does nothing for identical values. Collector
settings work on collector-only installations. A collector-only change no
longer restarts the hub, and storage or header-family changes no longer
restart the collector. Configure starts only components it turns on; a
component that was already stopped stays stopped.

## Remaining

| Priority | Setting / user task | Recommended control | Current gap |
| --- | --- | --- | --- |
| Later | Change collector destination/private CA | Improve the existing enrollment flow to handle destination changes in one operation | `install --hub URL --token TOKEN [--hub-ca FILE]` already safely moves an installed collector. `collector enroll` stores credentials but expects matching config. Prefer one complete guided operation over asking users to synchronize the two manually. |

Known limits of the implemented controls: the service-account TLS check reads
classic file modes; when a POSIX ACL is present it reports that access could
not be confirmed instead of refusing. The dashboard storage form does not
apply the change itself, by design.

## Controls already available

- Port, public origin, local-origin reset and trusted proxies: `configure`.
- Password recovery: `reset-password`; user and access management: Administration.
- Enrollment, replacement grants, revocation, machine names/tags and audit history:
  Administration. Collector destination changes also have the install flow above.
- Alert rules, maintenance windows, webhook configuration and testing: Alerts UI.
- User/system scope, boot startup, roles and GPU permissions: installer options.
- Service status, diagnostics, logs, upgrades, rollback and removal: CLI commands.

## Settings that are not ordinary user configuration

Synthetic telemetry is a demo/test facility and should stay out of normal setup.
Schema version is managed by migrations. State paths and ownership permissions
are installer responsibilities. Storage retention, query/ring budgets and queue
limits are currently runtime implementation defaults, not editable fields in the
JSON schema; exposing them needs a separate supported configuration design.
The recent one-second ring stays in RAM even with persistent hub history, and
process detail is never persisted. Neither should appear as a misleading
persistent-storage toggle.
