// Package gpu owns every GPU telemetry source for the collector.
//
// It owns:
//
//   - the asynchronous nvidia-smi poller (poll cache, exponential backoff,
//     ENOENT handling, compute-app memory joins, cost statistics);
//   - the AMD/Intel DRM sysfs reader (`gpu_busy_percent`, VRAM counters,
//     hwmon temperature and power);
//   - the runtime power-management gate for hybrid laptops, which keeps the
//     poller from waking a suspended discrete GPU and lets an idle one sleep;
//   - NVIDIA junction/VRAM register temperatures (gpu-temps), including the
//     PCI BAR0 mapping that replaces the prototype's python3 helper: the map
//     runs in a short-lived `rigscope internal pci-read` child, so a wedged or
//     vanished device cannot stall sampling or kill the collector;
//   - the local telemetry types that mirror the protocol contract.
//
// It does not own: assembling the per-sample `MetricsPayload` or `Inventory`
// (the metrics package does that), the wire encoding of the protocol types
// (`internal/protocol`), or the CLI/group setup for register access.
//
// Absent values stay absent. Optional readings are pointers, never a
// fabricated zero: an unsupported counter, an unreadable attribute and a
// powered-down device are all distinguishable from "the value is zero".
//
// # Runtime PM guarantees
//
// A suspended discrete GPU is not deliberately woken. The gate reads
// power/runtime_status before deciding (that file is kernel bookkeeping and
// does not resume the device), every hardware operation runs under a
// per-device access lease, and a command's leases are released only once its
// child has actually been reaped — so a quiet window can never start
// underneath an nvidia-smi or pci-read process that is still running.
// Immediately before each access (nvidia-smi launch, helper dispatch, DRM
// sysfs counter read) runtime_status is re-read under the lease, and a device
// the kernel no longer reports usable is refused rather than touched.
// An "unsupported" runtime_status only says runtime PM is disabled, which can
// mask a suspended device, so it authorizes access only when the cached PCI
// power_state attribute reads exactly D0; a D3* reading is treated as
// suspended, and a missing or unrecognised one fails closed as unknown.
//
// The residual window between that last re-check and the access is narrow but
// real, and user space cannot close it: nvidia-smi, sysfs and the BAR mapping
// expose no non-waking power reference that would pin the device awake across
// the gap, so the kernel can autosuspend in it. Losing that race wakes the
// device once; the access is refused or fails and is reported unavailable with
// a power-state reason, and sampling resumes. The guarantee is therefore a
// documented best effort, not an atomic one.
//
// Parsers and readers are pure functions over an injected [fs.FS] or
// [SysFS]; commands run behind [Runner] and clocks are injectable, so the
// whole package is tested without root, without systemd and without GPU
// hardware.
package gpu
