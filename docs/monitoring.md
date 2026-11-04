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

The hub acknowledges samples after committing them to SQLite. A collector keeps
unacknowledged samples in a persistent queue for each hub and retries after a
disconnect. The hub removes duplicates using the collector, run and sequence IDs.
Replayed samples fill History without replacing current live readings or opening
alerts for past threshold crossings.

Default retention:

| Resolution | Kept for |
| --- | --- |
| Raw samples | 48 hours |
| One-minute summaries | 30 days |
| One-hour summaries | 400 days |

Summaries retain minimum and maximum values. History queries limit the number
of points, series, scanned rows and time spent. The UI marks partial results.

### Storage limits

Each collector queue keeps up to ten minutes or 64 MiB of queued JSON per hub,
with a 256 MiB total JSON limit. Physical SQLite files, including WAL files,
have separate limits of 80 MiB per hub and 640 MiB across eight hubs.

The hub's default database limit is 2 GiB, including its WAL file and all tables.
Retention protects the newest hour of telemetry. If old-data cleanup cannot
free enough space, the hub stops acknowledging new samples and collectors retry.
Existing oversized databases report pressure; startup does not run a destructive
cleanup.

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
incidents.

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
