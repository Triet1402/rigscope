package gpu

import "time"

// GpuStatus is the discriminant of a reading or metric family.
type GpuStatus string

const (
	// StatusOK marks a reading that carries sampled values.
	StatusOK GpuStatus = "ok"
	// StatusUnavailable marks a reading or family that carries no values.
	StatusUnavailable GpuStatus = "unavailable"
)

// GpuReading mirrors the protocol `GpuReading` union. Status selects the
// meaning of the other fields:
//
//   - StatusOK: Reason is empty and the optional fields hold the sample;
//   - StatusUnavailable: Reason says why and every value field is nil.
//
// Optional values are pointers so an absent field is never a zero. Wire
// encoding belongs to internal/protocol, not to these types.
type GpuReading struct {
	// ID is the stable resource id: "gpu:<nvidia-uuid>" or "pci:<bdf>".
	ID     string
	Status GpuStatus
	// Reason is set only when Status is StatusUnavailable.
	Reason string

	UtilizationPercent *float64
	MemoryUsedBytes    *int64
	MemoryTotalBytes   *int64
	TemperatureC       *float64
	PowerWatts         *float64
	// JunctionTemperatureC is the hottest die hotspot, when register
	// temperatures are readable. Absent (nil) on machines without the
	// feature; it is not the same as "reported as null".
	JunctionTemperatureC *float64
	// VRAMTemperatureC is the hottest VRAM source, when readable.
	VRAMTemperatureC *float64
}

// UnavailableReading builds a reading that reports no values.
func UnavailableReading(id, reason string) GpuReading {
	return GpuReading{ID: id, Status: StatusUnavailable, Reason: reason}
}

// copyReading returns an independent copy of a reading. Snapshot consumers
// hold the result across samples, so the optional values must not alias the
// poller's cache: writing through them would race with the next cycle and
// corrupt what every other reader sees.
func copyReading(reading GpuReading) GpuReading {
	copied := GpuReading{
		ID:     reading.ID,
		Status: reading.Status,
		Reason: reading.Reason,
	}
	if reading.UtilizationPercent != nil {
		copied.UtilizationPercent = pointer(*reading.UtilizationPercent)
	}
	if reading.MemoryUsedBytes != nil {
		copied.MemoryUsedBytes = pointer(*reading.MemoryUsedBytes)
	}
	if reading.MemoryTotalBytes != nil {
		copied.MemoryTotalBytes = pointer(*reading.MemoryTotalBytes)
	}
	if reading.TemperatureC != nil {
		copied.TemperatureC = pointer(*reading.TemperatureC)
	}
	if reading.PowerWatts != nil {
		copied.PowerWatts = pointer(*reading.PowerWatts)
	}
	if reading.JunctionTemperatureC != nil {
		copied.JunctionTemperatureC = pointer(*reading.JunctionTemperatureC)
	}
	if reading.VRAMTemperatureC != nil {
		copied.VRAMTemperatureC = pointer(*reading.VRAMTemperatureC)
	}
	return copied
}

// copyReadings copies a reading slice value by value, including every pointer.
func copyReadings(readings []GpuReading) []GpuReading {
	if readings == nil {
		return nil
	}
	copied := make([]GpuReading, len(readings))
	for index, reading := range readings {
		copied[index] = copyReading(reading)
	}
	return copied
}

// GpusMetric mirrors the protocol `GpusMetric` union.
type GpusMetric struct {
	Status  GpuStatus
	Reason  string
	Devices []GpuReading
}

// GpusOK builds an available GPUs metric.
func GpusOK(devices []GpuReading) GpusMetric {
	return GpusMetric{Status: StatusOK, Devices: devices}
}

// GpusUnavailable builds an unavailable GPUs metric.
func GpusUnavailable(reason string) GpusMetric {
	return GpusMetric{Status: StatusUnavailable, Reason: reason}
}

// GpuVendor mirrors the protocol `GpuInventory.vendor` values.
type GpuVendor string

const (
	VendorNVIDIA GpuVendor = "nvidia"
	VendorAMD    GpuVendor = "amd"
	VendorIntel  GpuVendor = "intel"
	VendorASPEED GpuVendor = "aspeed"
	VendorOther  GpuVendor = "other"
)

// GpuTelemetry mirrors the protocol `GpuInventory.telemetry` values.
type GpuTelemetry string

const (
	TelemetrySupported   GpuTelemetry = "supported"
	TelemetryUnsupported GpuTelemetry = "unsupported"
)

// GpuInventory mirrors the protocol `GpuInventory`. It is built by the
// inventory assembler; the GPU package supplies the pieces (device info from
// the nvidia-smi poller and sysfs readings) that it joins.
type GpuInventory struct {
	ID               string
	Name             string
	Vendor           GpuVendor
	PCIBusID         *string
	Telemetry        GpuTelemetry
	MemoryTotalBytes *int64
	Reason           *string
}

// Availability reports whether nvidia-smi telemetry can be read at all.
type Availability string

const (
	// AvailabilityUnknown means no cycle has finished yet (or the last one
	// failed for a reason that is not "tooling is missing").
	AvailabilityUnknown Availability = "unknown"
	// AvailabilityYes means the last nvidia-smi query succeeded.
	AvailabilityYes Availability = "yes"
	// AvailabilityNo means nvidia-smi is not installed. Polling continues at
	// a rare cadence in case it is installed later.
	AvailabilityNo Availability = "no"
)

// DeviceInfo is the slow-changing description of one NVIDIA device.
type DeviceInfo struct {
	UUID             string
	Name             string
	PCIBusID         *string
	MemoryTotalBytes *int64
}

// Snapshot is one completed GPU polling cycle.
type Snapshot struct {
	// AtMonotonic is the monotonic reading taken when the cycle completed.
	// Consumers compare it with their own monotonic clock to detect staleness.
	AtMonotonic time.Duration
	// Devices is ordered like nvidia-smi reported them, followed by any
	// devices the runtime-PM gate kept asleep.
	Devices []GpuReading
}

// CostStats reports what nvidia-smi polling costs. Durations are in
// milliseconds, matching the prototype's diagnostics.
type CostStats struct {
	// Runs counts nvidia-smi invocations (GPU and compute-app queries).
	Runs     int
	Failures int

	LastDurationMs float64
	AvgDurationMs  float64
	MaxDurationMs  float64
}

func pointer[T any](value T) *T { return &value }
