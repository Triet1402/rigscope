# Monitoring and alerts

Use the dashboard to check live readings, explore History and inspect Processes.
For setup, see [Installation](installation.md).

## Collection and sharing

A collector samples its machine continuously, whether or not a browser is open,
and connects outbound to exactly one hub. The hub owns users, permissions,
history and alerts. The default installation runs a hub and a collector on the
same machine.

The collector applies its local [sharing policy](configuration.md#sharing-policy)
before anything is queued. CPU, memory, swap, GPU, disk and network families and
on-demand process detail are shared by default. Network addresses and full
command lines are withheld by default. A withheld family is sent as
`unavailable` with `withheld by local sharing policy`; it is not silently
omitted, and it is never replaced with zero.

Missing, inaccessible or unsupported readings stay unavailable with a reason.
GPU support depends on the driver and readable counters; Intel GPU readings are
partial. See [Sensor readings](bottlenecks.md).

## Live readings

The dashboard updates automatically while the collector is connected. A machine
becomes stale after `max(15 seconds, 3 × sampling interval)` without readings.
Connection status, unavailable readings and clock skew are shown separately.

<details>
<summary>Live stream limits</summary>

Browsers receive complete snapshots over SSE. Updates are coalesced for 500 ms;
identical snapshots are skipped. Keepalives arrive every 15 seconds. Clients
more than 1 MiB behind reconnect. The hub allows 64 live streams at once;
logout, session expiry, account changes and hub shutdown end them.

</details>

## History

Rigscope defaults to RAM history, lost on hub restart. Run
`rigscope configure --telemetry-mode persistent` for durable SQLite summaries;
the command asks before stopping and restarting running services.
Both modes include a one-second RAM ring and the summary tiers below; there is
no per-sample raw tier. See [Telemetry storage](telemetry-storage.md) for memory
budgets and mode-switch behavior.

| Tier | Resolution | Default retention |
| --- | --- | --- |
| Ring | 1 second | 15 minutes (RAM) |
| Summary | 10 seconds | 2 hours |
| Summary | 1 minute | 30 days |
| Summary | 1 hour | 400 days |

History preserves minimums and maximums so short spikes remain visible at coarser
resolutions. Missing and unavailable readings never become zero. Queries use the
finest tier covering the requested range, up to 400 days; a budget-limited result
is marked partial.

The ring is bounded to 64 MiB and 64 machines by default. For query budgets,
aggregation, delivery and storage behavior, see
[Telemetry storage](telemetry-storage.md).

## Gaps and unknown loss

Three different things are reported, and charts never blur them together:

- **Gap record**: the collector knows it dropped captured samples (queue age,
  byte budget or overflow) and sends a `gap` covering the interval, with a count
  of dropped records.
- **Missing interval**: the hub sees a hole between receipts (more than
  `max(15 seconds, 3 × cadence)`) and reports a `missing` gap.
- **Collector restart**: a new `runId` appears after the previous run's last
  record. That interval is unknown loss. It is reported as `missing` with the
  reason `collector restarted` and **no** dropped-record count, because the
  collector could not observe what was lost.

Charts segment at every recorded gap, so a line never connects across a
collector restart or a known outage. Pending samples live in a bounded RAM queue
(10 minutes or 64 MiB, whichever comes first). A graceful shutdown drains for up
to 3 seconds; a crash or forced stop discards the queue and the next run's
`runId` marks the boundary. Durable collector queues are not part of 1.0.

## Process detail

Open **Processes** to request live process detail. Collection stops when the last
viewer leaves. Offline, unsupported and withheld states are shown explicitly.

Process detail is never saved. Full command lines require the collector's
`processArguments` sharing setting; environment variables are never collected.
[Exports](exports.md) download the visible snapshot.

<details>
<summary>Process viewer limits</summary>

A viewer holds a 15-second lease, renews about every 5 seconds and polls about
once per second. Equivalent viewers share one collector scan. Each session may
hold 8 viewers; the hub allows 256. Exhausting a budget returns HTTP 429 with
`process-viewer-budget`. Ending the last lease cancels the subscription and drops
the snapshot.

</details>

## Laptop discrete GPUs

On hybrid laptops the discrete GPU powers down when idle (runtime D3). Every
`nvidia-smi` invocation and every read of amdgpu's busy, memory or hwmon files
can wake it. The collector therefore never deliberately samples a GPU whose
kernel runtime-PM status is `suspended`; it reports "GPU powered down" instead.

After three consecutive idle samples, the collector revokes that device's access
leases, waits for them to drain, and pauses sampling for a quiet window (20–120
seconds) so the driver can suspend the GPU. If it suspends, sampling stays off
until something else wakes it. If it stays awake, something else is holding it
(a display or a persistence daemon, for example), and the collector resumes with
pauses backed off from 5 minutes up to one hour.

This applies only to GPUs under runtime power management (`power/control` is
`auto`) that are not the boot display adapter. It does not apply to NVIDIA GPUs
whose driver reports runtime D3 disabled. Desktops and dGPU-only setups keep
full-rate telemetry. `power/control` is re-read on each sample, so tools that
switch it when a charger is unplugged take effect immediately.

The last re-check before an access cannot be made atomic with the access; the
documented residual race can wake a suspended GPU once. See
[Security](security.md#runtime-power-management).

## GPU junction and VRAM temperatures

Supported NVIDIA cards can report **junction** (die hotspot) and **VRAM**
(memory hotspot) temperatures through optional read-only register access.
These readings appear in the dashboard, history, exports and alerts.

Enable them with `rigscope install --gpu-registers`. Follow the installer's
permission and log-out instructions; see
[GPU setup](installation.md#gpu-register-temperatures) for details.

Register layouts are undocumented and card-specific. Verify readings on your
hardware. Missing access and undecodable values are reported as unavailable.

## Users and administration

**Administration** manages users, machine access, names and tags, enrollment
grants, collector revocation and audit history. Administrative routes require
the administrator flag, not merely access to all machines. Changing a user,
deleting a user or revoking a collector immediately ends affected sessions and
live streams.

Browser sessions last 12 hours, survive a hub restart, and are stored durably as
token hashes in the hub database. See [Security](security.md) for the full
session, CSRF and cookie rules.

## Alerts and webhooks

Alert rules are machine-scoped. A rule names a metric and resource, a condition
(`above`, `below`, `stale` or `unavailable`), an optional threshold, a duration
and an optional maintenance window. Duration rules must persist before firing;
recovery resolves the incident. **Replayed** samples fill history only: they
never update live state, reopen an old condition or fire a webhook. Stale rules
use the same `max(15 s, 3 × cadence)` freshness threshold.

Alerts, incidents, rule state and pending notifications are durable, so a hub
restart continues a pending duration and does not duplicate a job. Incident and
job history is bounded.

One HTTPS webhook channel is configured per hub. Loopback HTTP is allowed for a
controlled local test sink. The URL, headers and HMAC secret are write-only:
they are never returned or logged. Delivery includes an `Idempotency-Key` and,
when a secret is set, an `X-Rigscope-Signature: sha256=...` header. Delivery is
at-least-once with up to six attempts and a 5-second timeout per request;
receivers should deduplicate by the idempotency key.

Webhook delivery validates the address it actually dials. A hostname that
resolves to loopback, private, link-local or other non-public space is refused
before connecting, which also closes DNS rebinding; redirects and proxy
environment variables are ignored. See the SSRF section in
[Security](security.md#webhook-ssrf-protection).

## Scale and limits

There is no fixed machine-count limit in the product design. The hub bounds
concurrent collector connections and live browser streams (100 and 64 by
default), and per-machine history and query work are bounded. The collector's
footprint scales with sampled resources, not with history, because it keeps only
a bounded queue.

Rigscope does not kill processes, change their priorities, control remote
services, federate hubs or provide a clustered hub. Each hub owns its own
accounts, permissions, history and alerts.
