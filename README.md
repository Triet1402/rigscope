# Rigscope

Your machines, in view. A self-hosted Linux system and process monitor for one machine or a small fleet. A collector samples each machine and connects outbound to one or more independently operated hubs. Each hub owns its users, permissions, SQLite history and alerts.

Requires Linux, Node.js 26+ with npm, and a working systemd user manager for background services. Install a published release with:

```sh
npx --yes rigscope@latest install --boot
~/.local/bin/rigscope
```

If the npm release is not available yet, use the [release archive instructions](docs/installation.md#install-from-an-archive). No global npm installation or source checkout is required.

Save the generated `owner` password and open the printed URL. The default installation starts separate hub and collector services. `--boot` enables user lingering; without it, startup is at login. The installer reports any host authorization needed. Add `~/.local/bin` to PATH to use the commands below. Bare `rigscope` shows status.

```sh
rigscope status
rigscope logs hub
rigscope stop collector
rigscope start collector
rigscope upgrade
rigscope rollback
```

For a foreground trial before installing services, run `npx --yes rigscope@latest try`. It uses separate trial state and stops with Ctrl+C. For a collector reporting to an existing hub, install with `--role collector`, then use `rigscope connect https://your-hub.example.com` and paste an Add machine grant at the private prompt. Each collector supports up to eight hubs.

Rigscope includes real resource telemetry, fleet and machine views, durable History, on-demand Processes, alerts/webhooks and user/collector administration. Process inspection is read-only; kill, renice, service control, hub federation and clustering are outside this release. Missing readings remain unavailable, and stale data retains its freshness status.

[Installation and hardware](docs/installation.md) · [Operations and recovery](docs/operations.md) · [Release notes](CHANGELOG.md) · [Development](docs/development.md) · [Releasing and verification](docs/releasing.md) · [Architecture](ARCHITECTURE.md) · [Design](DESIGN.md)

[Bottleneck readings](docs/bottlenecks.md) explains optional sensors and their limits. [Exports](docs/exports.md) describes visible-data CSV/JSON downloads. [Scale validation](docs/scale-validation.md) records synthetic workloads on one host; it is not a capacity guarantee.

Maintained by [Triet (@Triet1402)](https://github.com/Triet1402). Licensed under [MIT](LICENSE); dependency attribution is in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). See [SECURITY.md](SECURITY.md) to report vulnerabilities privately.

## Advanced configuration

Managed installations use `rigscope config` and private settings described in
[installation](docs/installation.md#hub-configuration). The environment variables
below apply to source development and raw hub/collector services; setting them in
your shell does not reconfigure a managed service.

Keep state/configuration directories 0700 and files 0600. Do not commit them.

| Hub variable | Behavior |
| --- | --- |
| `HUB_HOST`, `HUB_PORT` | Loopback-only listener; defaults `127.0.0.1:8081`. Port `0` is useful for isolated tests. |
| `HUB_PUBLIC_ORIGIN`, `HUB_SOCKET_PATH` | Required together for remote mode. Exact HTTPS origin and private absolute Unix socket. No TCP listener in this mode. |
| `HUB_DATABASE_PATH` | Authoritative SQLite telemetry/users/alerts/collector registry. Defaults beside `HUB_STATE_FILE`, otherwise `$XDG_STATE_HOME/rigscope/hub/telemetry.sqlite` (falling back to `~/.local/state`). |
| `HUB_BROWSER_USERS` | First-run bootstrap JSON: `{username,passwordHash,machineIds,admin?}`. Scope is `"*"` or a machine-ID array; `[]` grants no machine access. |
| `HUB_ADMIN_USERS` | Comma-separated usernames explicitly elevated during first import. When absent, an existing bootstrap account named `owner` is migrated as administrator. Other users need an explicit `admin:true`. |
| `HUB_COLLECTORS` | First-run bootstrap JSON: `{collectorId,machineId,token}`; bounded by JSON bytes rather than machine count. Credentials travel only in bearer headers. |
| `HUB_MAX_COLLECTOR_CONNECTIONS` | Concurrent collector socket budget; defaults to 100. Positive integer; tune to measured deployment capacity. |
| `HUB_STATE_FILE` | Private enrollment-file import source for existing installations; enables the enrollment endpoint. Preserve it with deployment configuration for recovery. |
| `HUB_ENROLLMENT_GRANTS` | Bootstrap grants `{token,expiresAt}`; at most 15 minutes. Consumed grants remain consumed across migration/restart. |

From a source checkout, generate salted scrypt hashes with
`scripts/hash-browser-password.cjs`, which reads passwords from stdin. Managed
installation generates the initial account automatically. Keep hashes private. The database imports existing
users/credentials once; later configuration restarts do not overwrite managed users
or resurrect revoked credentials. Administration changes apply to active sessions
and collector connections. Existing owner passwords and scopes are preserved.

| Collector variable | Behavior |
| --- | --- |
| `COLLECTOR_MODE` | `real` by default; `mock` is explicit synthetic telemetry and never scans real processes. |
| `COLLECTOR_STATE_FILE` | Private persistent identity, enrolled destinations and pending enrollment recovery proof. Defaults under `$XDG_STATE_HOME/rigscope/collector.json`. |
| `COLLECTOR_INTERVAL_MS` | Shared sampling cadence; defaults 1000 ms, minimum 100 ms. GPU polling has a 1-second local floor. |
| `COLLECTOR_HUBS` | Optional explicit destinations `{url,token,sharing?,intervalMs?}`; at most eight. Otherwise use saved enrollment. |
| `COLLECTOR_HOSTNAME` | Metadata only; real mode defaults to OS hostname. |

Destination `sharing` accepts `cpu`, `memory`, `swap`, `gpus`, `disks`, `network`,
`networkAddresses`, `processes`, `processArguments`. CPU/memory remain required when
an object is supplied. Missing fields default to true, except swap follows memory
and process arguments default false. Review local sharing before upgrading a
restricted destination to broader metrics. Environment variables are never collected.
Policy is applied before queueing, to restored pending metrics before sending, and
to every process result separately for each hub.

## Source enrollment, rotation and replacement

For a managed installation, use `rigscope connect` as described in
[Add machines and hubs](docs/installation.md#add-machines-and-hubs). The following
commands require a source checkout with development dependencies installed.

Create a short-lived grant through an authorized administration flow. On the machine,
set `COLLECTOR_ENROLL_URL` to its full `ws[s]://…/api/collector/connect` URL and
`COLLECTOR_ENROLL_TOKEN` privately, then run:

```sh
npm run collector:enroll
```

The collector saves a random recovery proof before requesting a credential. A retry
after a lost successful response recovers the same credential using that proof;
collector ID or hostname alone is insufficient. Restart the collector to apply changes.

Rotation/replacement requires a grant explicitly bound to the existing hub-local
machine mapping. Use `npm run collector:enroll -- --replace-enrollment` to replace a
saved destination. Revocation invalidates open connections and old recovery proofs.
Replacement never matches a machine by hostname or address.

## Monitoring behavior and bounds

- Samples are acknowledged only after a durable SQLite commit. Retries deduplicate
  by collector/run/sequence. Replay fills History without replacing live state or
  triggering historical threshold incidents.
- Per-destination outboxes retain pending metrics across collector restart, prioritize
  live telemetry, and bound replay/in-flight work. Defaults: ten minutes or 64 MiB
  of queued JSON per destination, 256 MiB aggregate queued JSON. Separate physical
  budgets count main database files plus WAL: 80 MiB per destination and 640 MiB
  across eight destinations. SQLite page caps, bounded writes, checkpoints and
  pressure rejection enforce these budgets; they are not OS filesystem quotas.
- History defaults: raw 48 hours, one-minute rollups 30 days, one-hour rollups 400 days.
  Rollups preserve minima/maxima. Queries have point, series, scanned-row and elapsed
  budgets; partial results are disclosed. The hub defaults to a 2 GiB physical
  database/WAL budget shared by telemetry, users, enrollment and alerts. Retention
  protects the newest hour; when it cannot reclaim enough space, ingestion returns
  busy without acknowledging records. Existing oversized databases enter pressure
  mode; they are not destructively vacuumed at startup. Atomic writes may briefly
  grow WAL before checkpointing; blocked checkpoints surface pressure and stop
  subsequent ingestion rather than permitting continued growth.
- Receipt freshness is `max(15 seconds, 3 × expected delivery interval)`. Unknown cadence
  uses a disclosed 15-second fallback. Connection state, collection availability and
  clock skew remain separate. Browser/hub clocks are not calibrated.
- Process detail uses renewable viewer interests and one shared collector scan. Pausing,
  hiding/closing a view, session termination or lease expiry releases extra work.
  Process detail is never persisted. CPU is 100% per core; machine CPU is normalized
  across logical cores. Missing/inaccessible readings remain unavailable.
- User sessions are in memory with a fixed twelve-hour lifetime; a hub restart requires
  signing in again. Users/scopes/password hashes and collector credentials persist.
- Physical checks cover the Ubuntu RTX 3090 rig and the laptop’s RTX 5060 plus Radeon
  860M iGPU. Intel sensor and ARM/RISC-V naming paths have deterministic fixture coverage;
  physical validation for those paths remains outstanding. See the [hardware support notes](docs/installation.md#hardware).
- There is no fixed machine-count limit. The default concurrent collector connection
  budget is 100; [scale validation](docs/scale-validation.md) records synthetic
  workloads up to 75 machines on one host and is not a capacity guarantee.
  Remote kill/renice, service control, hub federation and clustering are out
  of scope for this read-only release.

## Alerts and administration

Administration requires an explicit administrator role as well as machine scope.
Use it to manage accounts/scopes, display names/tags, collector revocation and
short-lived enrollment/replacement grants. User changes revoke existing browser
sessions; credential changes close invalid collector connections. Wildcard scope
alone does not grant administration.

Alerts support above/below thresholds, duration, unavailable readings, missing
receipts, maintenance and recovery. Value, unavailable and stale-receipt rules
apply to every machine the hub tracks. Replay never opens historical incidents.
Configure one HTTPS webhook channel in Alerts (loopback HTTP only for controlled
test sinks). URL, headers and HMAC secret are write-only. Delivery is at-least-once:
receivers should deduplicate the `Idempotency-Key`. Six attempts, a five-second
request deadline and bounded persistent queues prevent unbounded delivery work.

See [operations](docs/operations.md) for packaging, private service configuration,
boot enablement, backups, tested restores, upgrades/rollback and measurements.
The managed installer starts services by default; low-level archive tools print service commands for explicit execution.
