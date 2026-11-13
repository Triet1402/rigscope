package gpu

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Optional Linux DRM/hwmon counters. Missing fields stay nil, including
// dedicated VRAM on integrated GPUs; system RAM is never presented as VRAM.

const (
	// DefaultDRMDevicesDir is the PCI device path the DRM counters live under.
	DefaultDRMDevicesDir = "/sys/bus/pci/devices"

	// maxHwmonEntries bounds how many hwmon directories are inspected per GPU.
	maxHwmonEntries = 8
)

var (
	hwmonPattern = regexp.MustCompile(`^hwmon[0-9]+$`)
)

// ReadSysfsGpu reads AMD/Intel DRM counters for one PCI display device.
//
// Values are reported in protocol units: utilization in percent, memory in
// bytes, temperature in °C and power in watts. A vendor without supported
// DRM telemetry, or a device whose every counter is unreadable, comes back as
// StatusUnavailable with a reason; no value is invented to fill the gap.
func ReadSysfsGpu(sysfs SysFS, address string) GpuReading {
	base := DefaultDRMDevicesDir + "/" + address
	id := "pci:" + address

	vendor, _ := readTextTrimmed(sysfs, base+"/vendor")
	vendor = strings.ToLower(vendor)
	if vendor != "0x1002" && vendor != "0x8086" {
		return UnavailableReading(id, "no supported DRM telemetry")
	}

	reading := GpuReading{
		ID:                 id,
		Status:             StatusOK,
		UtilizationPercent: sysfsPercent(sysfs, base+"/gpu_busy_percent"),
		MemoryUsedBytes:    sysfsInt(sysfs, base+"/mem_info_vram_used"),
		MemoryTotalBytes:   sysfsInt(sysfs, base+"/mem_info_vram_total"),
	}
	// Zero total is not dedicated memory (common on integrated adapters).
	if reading.MemoryTotalBytes != nil && *reading.MemoryTotalBytes == 0 {
		reading.MemoryTotalBytes = nil
	}
	if reading.MemoryTotalBytes == nil ||
		(reading.MemoryUsedBytes != nil && *reading.MemoryUsedBytes > *reading.MemoryTotalBytes) {
		reading.MemoryUsedBytes = nil
	}

	for _, hwmon := range hwmonEntries(sysfs, base+"/hwmon") {
		path := base + "/hwmon/" + hwmon
		if temperature := sysfsIntBetween(sysfs, path+"/temp1_input", -50_000, 200_000); temperature != nil && reading.TemperatureC == nil {
			reading.TemperatureC = pointer(float64(*temperature) / 1000)
		}
		power := sysfsIntBetween(sysfs, path+"/power1_average", 0, 10_000_000_000)
		if power == nil {
			power = sysfsIntBetween(sysfs, path+"/power1_input", 0, 10_000_000_000)
		}
		if power != nil && reading.PowerWatts == nil {
			reading.PowerWatts = pointer(float64(*power) / 1_000_000)
		}
	}

	if reading.UtilizationPercent == nil && reading.MemoryTotalBytes == nil &&
		reading.TemperatureC == nil && reading.PowerWatts == nil {
		return UnavailableReading(id, "GPU detected, but no readable DRM/sysfs counters; "+
			"driver support or permissions may limit telemetry")
	}
	return reading
}

// ReadSysfsGpuGuarded reads AMD/Intel DRM counters under an access lease.
// Reads of gpu_busy_percent, mem_info_* and hwmon attributes resume a runtime-
// suspended device, so they are hardware operations like any other. When the
// arbiter refuses, no attribute is read and the caller gets the distinct
// access-denied reason instead of a permissions message. The lease is also
// re-checked against the kernel's runtime status immediately before the first
// attribute is read, so a device that suspended after the gate decision is
// reported powered down rather than woken.
func ReadSysfsGpuGuarded(arbiter *AccessArbiter, sysfs SysFS, address string) (GpuReading, error) {
	if arbiter == nil {
		return ReadSysfsGpu(sysfs, address), nil
	}
	lease, err := arbiter.Acquire(context.Background(), address)
	if err != nil {
		return UnavailableReading("pci:"+address, AccessDeniedReason), err
	}
	defer lease.Release()
	if err := lease.Recheck(); err != nil {
		return UnavailableReading("pci:"+address, accessDeniedReason(err)), err
	}
	return ReadSysfsGpu(sysfs, address), nil
}

// sysfsInt parses a non-negative integer attribute, bounded like the
// prototype's Number.MAX_SAFE_INTEGER check.
func sysfsInt(sysfs SysFS, path string) *int64 {
	return sysfsIntBetween(sysfs, path, 0, maxSafeIntegerInt64)
}

// maxSafeIntegerInt64 is JavaScript's Number.MAX_SAFE_INTEGER.
const maxSafeIntegerInt64 = int64(1<<53 - 1)

func sysfsIntBetween(sysfs SysFS, path string, min, max int64) *int64 {
	raw, ok := readTextTrimmed(sysfs, path)
	if !ok || !integerPattern(raw) {
		return nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < min || value > max {
		return nil
	}
	return pointer(value)
}

func sysfsPercent(sysfs SysFS, path string) *float64 {
	value := sysfsIntBetween(sysfs, path, 0, 100)
	if value == nil {
		return nil
	}
	return pointer(float64(*value))
}

// integerPattern reports whether the attribute is a plain decimal integer
// (optionally negative). The prototype accepted "[-]?\\d+" and nothing else,
// so "NaN", "1.5" and empty cells stay absent.
func integerPattern(raw string) bool {
	if raw == "" {
		return false
	}
	start := 0
	if raw[0] == '-' {
		if len(raw) == 1 {
			return false
		}
		start = 1
	}
	for i := start; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return false
		}
	}
	return true
}

func hwmonEntries(sysfs SysFS, path string) []string {
	names, err := sysfs.ReadDir(path)
	if err != nil {
		return nil
	}
	entries := make([]string, 0, len(names))
	for _, name := range names {
		if hwmonPattern.MatchString(name) {
			entries = append(entries, name)
		}
	}
	sort.Strings(entries)
	if len(entries) > maxHwmonEntries {
		entries = entries[:maxHwmonEntries]
	}
	return entries
}
