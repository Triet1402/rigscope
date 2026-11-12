# Monitoring and alerts

This guide explains what Rigscope collects, how long it keeps data, and how
access and alerts work. For setup, see [Installation](installation.md).

## Collection and sharing

Each collector samples its machine once and sends readings independently to up
to eight hubs. Collection continues when no browser is open. Each hub owns its
users, permissions, history and alerts.

Managed enrollment shares CPU, memory, swap, GPU, disk and network readings,
plus on-demand process details. Network addresses and full process arguments
start disabled. Sharing is set separately for each hub; see the
[configuration reference](configuration.md#sharing-settings).

Missing or inaccessible readings stay unavailable. GPU support depends on the
driver and readable counters; see [Hardware](installation.md#hardware) and
[Sensor readings](bottlenecks.md).

## History and delivery

History is **RAM-only by default**. Live readings update immediately, recent
charts come from a bounded memory budget, and telemetry history disappears when
the hub restarts. Users, credentials, enrollment, settings, alert records,
machine names/tags and inventory remain persistent.

In memory mode the collector's acknowledgement means the hub accepted the sample
into RAM. The collector queues pending samples in RAM and retries after a
disconnect, so it gains no disk work from the hub's choice and writes no queue
of its own. Deduplication
uses a compact per-run interval set for the hub process lifetime. Because both
history and receipts disappear together on restart, a replay cannot double-count
restored chart data.

Set `HUB_TELEMETRY_MODE=persistent` to opt into disk history. The hub buffers a
five-second window, publishes live state immediately, writes one compact
transaction, and only then acknowledges. Replay deduplication uses durable
receipts whose lifetime is independent of chart retention (default seven days).
Replayed samples fill History without replacing current live readings or opening
alerts for past threshold crossings. A graceful shutdown flushes pending
summaries; after a crash the collector replays anything the hub never
acknowledged, provided the collector itself kept it: only
`COLLECTOR_QUEUE_MODE=durable` survives a collector restart.

Default memory history:

| Resolution | Kept for |
| --- | --- |
| 1-second detail | 2 minutes |
| 10-second summaries | 1 hour |
| One-minute summaries | 24 hours |
| One-hour summaries | 7 days |

Per-core CPU keeps less detail; machine-wide and capacity metrics keep more.
Persistent mode writes compact ten-second summaries and folds them into
one-minute and one-hour tiers; it does not write a raw row per reading. It keeps
10-second summaries for 2 hours, one-minute summaries for 30 days and hourly
summaries for 400 days by default. Both modes retain minimum and maximum values,
so short spikes remain visible, and both preserve explicit unavailable readings
and gaps.

RAM history is bounded by `HUB_HISTORY_MEMORY_BYTES` (default 128 MiB) across all
machines. When the budget fills, the oldest fine-grained tiers are dropped first
and new cardinality beyond the sustainable budget is rejected with visible
pressure. History queries limit points, series, scanned rows and elapsed time;
the UI marks partial results.

### Recording

Use **History → Recording** to start an explicit capture for the current
machine and selected metrics. Choose a resolution from 1 second to 1 hour,
a duration and whether to include recent RAM history. The capture is bounded by
duration and bytes, survives restart through its private persisted output, and
can be downloaded by its creator. Recording also works when chart history is
temporary, which is the intended way to keep an incident snapshot. See
[telemetry storage](telemetry-storage.md#recording).

Hover over a History or Overview recent-usage chart to inspect a recorded time
and its bucket averages. The time marker follows across charts in the same view;
per-core heatmaps show the bucket peak at that time. Missing readings stay
unavailable. With a chart focused, use Left/Right arrows to move between readings,
Home/End to jump to the first/last reading, and Escape to dismiss the marker.

### Storage limits

Each collector queue keeps up to ten minutes or 64 MiB of queued JSON per hub,
with a 256 MiB total JSON limit. Physical SQLite files, including WAL files,
have separate limits of 80 MiB per hub and 640 MiB across eight hubs.

In persistent mode the hub's default database limit is 2 GiB, including WAL and
all control tables. Retention protects the newest hour; if old-data cleanup
cannot free enough space, the hub stops acknowledging new samples and collectors
retry. Existing oversized databases report pressure; startup does not run a
destructive cleanup. In memory mode old historical rows may still occupy the
file until you run `ops reset-telemetry`; they are not read or updated.

These are application limits, not filesystem quotas. A transaction can briefly
grow the WAL before checkpointing. Blocked checkpoints stop further ingestion
instead of allowing continued growth. Keep disk space for backups and sidecar
files. See [Storage pressure recovery](operations.md#storage-budgets-and-pressure-recovery).

## Live readings and process details

A receipt becomes stale after `max(15 seconds, 3 × expected delivery interval)`.
If the cadence is unknown, Rigscope uses 15 seconds and discloses that fallback.
Connection status, unavailable readings and clock skew are reported separately.
Rigscope does not synchronize the browser and hub clocks.

Process details are scanned while viewers request them. Viewers share one
collector scan. Pausing, hiding or closing the view releases the extra work;
expired viewer requests and ended sessions do the same. Process details are
never persisted.

Process CPU uses 100% per logical core. Machine CPU is normalized across all
logical cores. See [Exports](exports.md) for CSV and JSON behavior.

## Laptop discrete GPUs

On hybrid laptops the discrete GPU powers down when it is idle (runtime D3).
Each `nvidia-smi` query, and each read of amdgpu's busy, memory or hwmon files,
wakes it up again. The collector therefore never samples a GPU whose kernel
runtime-PM status is `suspended`; its readings show "GPU powered down" instead.

Sampling at the normal cadence would also stop an awake, idle GPU from ever
reaching that state. After a few idle samples, the collector pauses that GPU for
about 20 seconds so the driver can power it down. If it powers down, sampling
stays off until another program wakes it. If it stays awake, something else is
holding it (an external display or a persistence daemon, for example), and the
collector resumes with pauses backed off from 5 minutes up to an hour.

This applies only to GPUs that are under runtime power management
(`power/control` is `auto`) and are not the boot display adapter. It does not
apply to NVIDIA GPUs whose driver reports Runtime D3 disabled. Desktops and
dGPU-only setups keep full-rate telemetry. `power/control` is checked on every
sample, so tools such as TLP that switch it when the charger is unplugged take
effect immediately.

## GPU junction and VRAM temperatures

On NVIDIA cards with GDDR6, GDDR6X or GDDR7 memory, Rigscope can also report
the **junction** (hottest die hotspot) and **VRAM** (hottest memory source)
temperatures. nvidia-smi does not expose these, so the collector reads them
from the GPU's undocumented thermal registers, the same technique as
[gddr6-core-junction-vram-temps](https://github.com/ThomasBaruzier/gddr6-core-junction-vram-temps)
(with credits there for the register findings). Register reads go through each
GPU's sysfs BAR0 file (`/sys/bus/pci/devices/<bdf>/resource0`), using a read-only memory mapping in a bounded, unprivileged Python 3 helper.
Python 3 must be installed on the collector machine. No kernel
`iomem=relaxed` flag, compiled addon or root daemon is needed. Readings
join the dashboard, History charts (`Junction`, `VRAM temp`), CSV/JSON exports
and alerts (`GPU junction hot`, `GPU VRAM hot`).

The BAR0 file is root-owned by default. User-scope collector installation and
upgrade automatically install the persistent udev rules and grant the account
running the collector access. Setup asks for sudo when a grant needs repair.
The collector launcher activates new group memberships using `sg`, without a
logout or reboot, entering every granted group it is missing. The helper runs
with the collector's user privileges. When a system does not provide `sg`
(shipped in the shadow-utils, shadow or login package), the collector still
starts and logs one warning, but the new groups take effect only after a
reboot: startup that continues after logout keeps the systemd user manager,
and its old group list, alive across logins.

The manual controls remain available for inspection and explicit changes:

```sh
rigscope gpu-temps status     # also: enable|disable
rigscope cpu-power status    # also: enable|disable
```

`status` probes the mapping and sensors, distinguishing mapping failures from
unsupported register values. `enable` asks for sudo and installs the group,
membership and persistent udev rule. Restart a running collector after a manual
grant. The terminal's own group list can remain stale until a new login, even
though the collector launcher activates the granted groups immediately.

A `rigscope try` trial is covered by the same user grant. Without access, or
when the registers do not decode, the feature reports nothing: junction and
VRAM show as *Not reported* and appear in nothing (the collector logs a single
diagnostic line per state change). Set `COLLECTOR_GPU_TEMPS=off` in the
collector environment to disable the reads entirely.

Register maps are undocumented and card-specific. The decoder rejects zero,
all-ones, poison and out-of-range words, and known GDDR7/Blackwell cards are
selected by PCI device id, but a card whose registers follow a different map
can still report nothing or an implausible value. Verify the readings on your
hardware, and set `COLLECTOR_GPU_TEMPS=off` if they look wrong. Once the grant
is in place, `gpu-temps status` reports an unsupported card as "nothing to
enable"; without the grant it cannot distinguish that case from missing
permission, so it asks for the grant.

## Users and administration

Use **Administration** to manage users, machine access, names, tags, collector
revocation and enrollment grants. Administration requires an administrator role
and the appropriate machine access. Access to all machines alone does not make
a user an administrator.

User changes revoke existing browser sessions. Collector credential changes
close connections that no longer have access. User records and collector
credentials persist in SQLite. Browser sessions stay in memory, expire after
12 hours and end when the hub restarts.

## Alerts and webhooks

Alerts support above/below thresholds, durations, unavailable readings, missing
receipts, maintenance and recovery. Value, unavailable and stale-receipt rules
apply to the machines tracked by that hub. Replayed samples do not open past
incidents. Alert rules, incidents and pending notifications stay durable in both
history modes; with no rules configured, live ingestion does not write per-sample
alert progress. Enabling rules may write alert state on evaluation, which is
operational state rather than telemetry history.

In **Alerts**, configure one HTTPS webhook channel. Its URL, headers and HMAC
secret are write-only. Loopback HTTP is allowed for controlled test sinks.
Delivery may be repeated: receivers should deduplicate the `Idempotency-Key`.
Rigscope makes up to six attempts, with a five-second deadline per request and
bounded persistent queues.

## Scale and limits

There is no fixed machine-count limit. The default limit is 100 concurrent
collector connections. [Scale validation](scale-validation.md) records synthetic
workloads of up to 75 machines on one host; those results are not a capacity
guarantee.

Rigscope does not kill processes, change their priorities, control remote
services, federate hubs or provide a clustered hub. See the
[hardware notes](installation.md#hardware) for physical testing and sensor limits.
