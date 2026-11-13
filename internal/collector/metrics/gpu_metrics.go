package metrics

import (
	"github.com/Triet1402/rigscope/internal/collector/sampling/gpu"
	"github.com/Triet1402/rigscope/internal/protocol"
)

const (
	noGPUsReason          = "no GPUs detected"
	noNvidiaToolingReason = "no supported GPU telemetry (nvidia-smi not found)"
	gpuNotReadyReason     = "gpu telemetry not ready"
	gpuStaleReason        = "gpu reading stale"
)

// sampleGPUs assembles the GPUs family. AMD/Intel devices are read through
// DRM sysfs; NVIDIA devices come from the poller's cache. A user-visible
// failure is recorded for every unavailable reading that was attempted. Each
// source is isolated: a panicking sysfs read or poller call becomes an
// unavailable reading for that device, and the rest of the tick survives.
func (c *Collector) sampleGPUs(inventory protocol.Inventory) (metric gpu.GpusMetric) {
	defer func() {
		if recover() != nil {
			c.addFailure("gpus", nil, gpuPanicReason)
			metric = gpu.GpusUnavailable(gpuPanicReason)
		}
	}()

	drm := make([]protocol.GpuInventory, 0, len(inventory.GPUs))
	for _, entry := range inventory.GPUs {
		if entry.Vendor == protocol.GpuVendorAMD || entry.Vendor == protocol.GpuVendorIntel {
			drm = append(drm, entry)
		}
	}
	if len(drm) == 0 {
		return c.sampleNvidia(inventory)
	}

	devices := make([]gpu.GpuReading, 0, len(drm)+len(inventory.GPUs))
	panicked := false
	for _, entry := range drm {
		reading, sourcePanicked := c.sampleDRM(entry)
		panicked = panicked || sourcePanicked
		devices = append(devices, reading)
	}
	if hasNvidia(inventory.GPUs) {
		nvidia := c.sampleNvidia(inventory)
		if nvidia.Status == gpu.StatusOK {
			devices = append(devices, nvidia.Devices...)
		} else {
			panicked = panicked || nvidia.Reason == gpuPanicReason
			for _, entry := range inventory.GPUs {
				if entry.Vendor != protocol.GpuVendorNvidia {
					continue
				}
				devices = append(devices, gpu.UnavailableReading(entry.ID, nvidia.Reason))
			}
		}
	}
	if panicked && !anyReadingAvailable(devices) {
		return gpu.GpusUnavailable(gpuPanicReason)
	}
	return gpu.GpusOK(devices)
}

func (c *Collector) sampleDRM(entry protocol.GpuInventory) (reading gpu.GpuReading, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
			reading = gpu.UnavailableReading(entry.ID, gpuPanicReason)
			c.addFailure("gpus", &entry.ID, gpuPanicReason)
		}
	}()

	if entry.PciBusID == nil || *entry.PciBusID == "" {
		reason := "GPU has no PCI address"
		c.addFailure("gpus", &entry.ID, reason)
		return gpu.UnavailableReading(entry.ID, reason), false
	}
	address := *entry.PciBusID
	if c.gate != nil {
		decision := c.gate.Decide(address)
		if !decision.Sample {
			// A sleeping GPU is a deliberate omission, not a collection
			// failure; the reason is still shown to the user.
			return gpu.UnavailableReading(entry.ID, decision.Reason), false
		}
	}
	// The arbiter lease keeps this read inside the gate's current decision.
	reading, _ = gpu.ReadSysfsGpuGuarded(gpu.GateAccess(c.gate), c.gpuFS, address)
	reading.ID = entry.ID
	if reading.Status != gpu.StatusOK {
		c.addFailure("gpus", &entry.ID, reading.Reason)
	}
	if c.gate != nil {
		idle := reading.Status != gpu.StatusOK ||
			reading.UtilizationPercent == nil || *reading.UtilizationPercent == 0
		c.gate.Record(address, idle)
	}
	return reading, false
}

func (c *Collector) sampleNvidia(inventory protocol.Inventory) (metric gpu.GpusMetric) {
	defer func() {
		if recover() != nil {
			c.addFailure("gpus", nil, gpuPanicReason)
			metric = gpu.GpusUnavailable(gpuPanicReason)
		}
	}()
	hasGPU := len(inventory.GPUs) > 0
	if c.gpu == nil {
		return c.gpusUnavailable(hasGPU, noNvidiaToolingReason)
	}
	if c.gpu.Available() == gpu.AvailabilityNo {
		return c.gpusUnavailable(hasGPU, noNvidiaToolingReason)
	}
	snapshot := c.gpu.Latest()
	if snapshot == nil {
		if hasGPU {
			return c.gpusUnavailable(true, gpuNotReadyReason)
		}
		return c.gpusUnavailable(false, noGPUsReason)
	}
	if len(snapshot.Devices) == 0 {
		if hasGPU {
			return c.gpusUnavailable(true, "no supported GPU telemetry")
		}
		return c.gpusUnavailable(false, noGPUsReason)
	}
	age := c.clock.Monotonic() - snapshot.AtMonotonic
	if age > c.gpuStale {
		devices := make([]gpu.GpuReading, 0, len(snapshot.Devices))
		for _, device := range snapshot.Devices {
			id := device.ID
			c.addFailure("gpus", &id, gpuStaleReason)
			devices = append(devices, gpu.UnavailableReading(id, gpuStaleReason))
		}
		return gpu.GpusOK(devices)
	}
	devices := make([]gpu.GpuReading, len(snapshot.Devices))
	copy(devices, snapshot.Devices)
	return gpu.GpusOK(devices)
}

func (c *Collector) gpusUnavailable(hasGPU bool, whenPresent string) gpu.GpusMetric {
	reason := noGPUsReason
	if hasGPU {
		reason = whenPresent
	}
	c.addFailure("gpus", nil, reason)
	return gpu.GpusUnavailable(reason)
}

func anyReadingAvailable(devices []gpu.GpuReading) bool {
	for _, device := range devices {
		if device.Status == gpu.StatusOK {
			return true
		}
	}
	return false
}

func hasNvidia(entries []protocol.GpuInventory) bool {
	for _, entry := range entries {
		if entry.Vendor == protocol.GpuVendorNvidia {
			return true
		}
	}
	return false
}
