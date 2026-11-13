// Package procfs owns the pure Linux procfs/sysfs parsers and the derived
// readings they feed: CPU, memory, swap, disk, network and the CPU-adjacent
// PSI/hwmon/RAPL sensors.
//
// What it owns:
//
//   - Parsers from bytes: /proc/stat, /proc/meminfo, /proc/net/dev,
//     /proc/diskstats, /proc/self/mountinfo, /proc/cpuinfo,
//     /proc/pressure/*, /proc/loadavg, /proc/uptime, /etc/os-release,
//     /proc/sys/kernel/random/boot_id.
//   - A small fs.FS-rooted reader (Source) so tests use testdata trees instead
//     of the live host, plus the OS-backed default.
//   - Filesystem/block-device/network-interface inventory discovery.
//   - hwmon CPU temperature and top-level RAPL package power.
//   - A stateful Sampler that holds the previous counters and an injectable
//     monotonic clock, and derives rates from counter deltas.
//
// What it does not own:
//
//   - GPUs (enumeration, nvidia-smi, AMD/Intel sysfs telemetry, temperatures).
//   - Process table scanning.
//   - Wire payload assembly: the Sampler returns local reading types whose
//     fields map 1:1 to the telemetry contract, but it does not allocate
//     sequences, stamp records or encode messages.
//
// Every unavailable reading carries a status and a reason. A missing counter,
// sensor or clock never becomes a fabricated zero.
package procfs
