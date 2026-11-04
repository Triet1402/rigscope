# Bottleneck telemetry

Overview shows available bottleneck readings alongside existing resource panels.
History records the same readings and offers separate charts for units such as
temperature, watts and milliseconds. Select **CPU cores** in History to compare cores; the selection is saved per machine and applies to exports. Up to eight series use lines; larger selections use a scrollable heatmap. Drawing groups
points toward a 150-point target per core while preserving extrema and gaps;
downloads retain the loaded stored buckets rather than the drawing reduction.

Readings are optional. An older collector, missing kernel interface, unreadable
file or reset counter produces an unavailable reading rather than zero. GPU
power and CPU package power are separate measurements; neither is whole-machine
wall power.

| Reading | Source and meaning |
| --- | --- |
| CPU temperature | Readable CPU hwmon sensors; package/Tctl/Tdie sensors take precedence over core sensors. Reports the hottest reading in the preferred class. |
| CPU package power | Change in top-level RAPL package energy over monotonic elapsed time, summed across readable packages. Package subdomains are excluded. Requires two valid observations. |
| CPU I/O wait | Change in procfs CPU iowait ticks, normalized by elapsed time and logical core count. |
| CPU pressure | Linux PSI `some avg10`: percentage of time with tasks stalled for CPU over the recent ten-second window. |
| Memory / I/O pressure | PSI `some avg10` and `full avg10`; `full` measures simultaneous stalls of all non-idle tasks. |
| Disk read / write latency | Change in diskstats operation milliseconds divided by completed operations. No completed operations means unavailable latency. |
| Disk utilization | Change in diskstats I/O-active milliseconds divided by elapsed time, capped at 100%. This is not a throughput-capacity estimate for parallel devices. |
| Filesystem inode use | Used inode count divided by a positive total from `statfs`. Filesystems without usable inode totals remain unavailable. |
| Memory / filesystem use | Used bytes divided by a positive total, on the 0–100 percent scale. |

CPU temperature supports known CPU hwmon drivers, including `coretemp` and
`k10temp`. CPU package power uses the Linux powercap `intel-rapl:N` interface,
including compatible AMD implementations. Many systems restrict `energy_uj` to
root. The collector remains unprivileged. The optional
[system installer](installation.md#system-installation) grants its dedicated account persistent
read-only package-counter access; ordinary user installation leaves permissions
under the operator's control.

Counter resets invalidate the affected derived reading until a valid interval is
available. RAPL wrap handling requires a readable energy range and a previous
counter near its ceiling. Monitoring continues when an optional sensor fails.

The new fields belong to the existing CPU, memory and disks sharing families.
Withholding a family withholds its bottleneck fields for that destination too.

Enhanced fields are negotiated independently for each hub. Upgraded collectors
advertise wire revision `0.3.0`; upgraded hubs opt in with `bottleneck-v1` in their
welcome. Older hubs receive the original metric shape, including replay. Older
collectors receive the original welcome shape. Upgrade the hub and collector to
see enhanced readings; an independent older destination continues receiving its
existing metrics. Projection affects transmission, not the durable outbox record.

Kernel references: [PSI](https://docs.kernel.org/accounting/psi.html),
[powercap](https://docs.kernel.org/power/powercap/powercap.html), and
[disk statistics](https://docs.kernel.org/admin-guide/iostats.html).

## Warning presets

In Alerts, choose a preset while creating a rule. The preset fills editable
fields; **Create rule** is still required. Presets include high CPU, memory or
disk nearly full, inode use, hot CPU/GPU, offline collectors, pressure stalls,
I/O wait and disk latency/utilization. Resource presets use the selected machine's
inventory, preferring its root filesystem when available.

Percentage thresholds use 0–100. Preset thresholds and durations are starting
points to review for the workload and hardware, not manufacturer safety limits.
Unavailable readings/resources are identified before applying a preset.
