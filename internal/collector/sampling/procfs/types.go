package procfs

// Status is the contract's reading discriminant.
type Status string

const (
	// StatusOK marks a reading whose values are present and trustworthy.
	StatusOK Status = "ok"
	// StatusUnavailable marks a reading with a reason and no values.
	StatusUnavailable Status = "unavailable"
)

// CapabilityStatus is the contract's inventory capability discriminant.
type CapabilityStatus string

const (
	CapabilitySupported   CapabilityStatus = "supported"
	CapabilityUnsupported CapabilityStatus = "unsupported"
)

// CpuMetric maps 1:1 to the contract's CpuMetric. Optional sensors are
// pointers: nil means absent/unreadable, never zero.
type CpuMetric struct {
	Status              Status     `json:"status"`
	Reason              string     `json:"reason,omitempty"`
	UsagePercent        *float64   `json:"usagePercent,omitempty"`
	PerCore             []*float64 `json:"perCore,omitempty"`
	TemperatureC        *float64   `json:"temperatureC,omitempty"`
	PowerWatts          *float64   `json:"powerWatts,omitempty"`
	IOwaitPercent       *float64   `json:"iowaitPercent,omitempty"`
	PressureSomePercent *float64   `json:"pressureSomePercent,omitempty"`
}

// BytesUsageMetric maps 1:1 to the contract's BytesUsageMetric (used by swap).
type BytesUsageMetric struct {
	Status     Status `json:"status"`
	Reason     string `json:"reason,omitempty"`
	UsedBytes  *int64 `json:"usedBytes,omitempty"`
	TotalBytes *int64 `json:"totalBytes,omitempty"`
}

// MemoryMetric maps 1:1 to the contract's MemoryMetric.
type MemoryMetric struct {
	Status              Status   `json:"status"`
	Reason              string   `json:"reason,omitempty"`
	UsedBytes           *int64   `json:"usedBytes,omitempty"`
	TotalBytes          *int64   `json:"totalBytes,omitempty"`
	PressureSomePercent *float64 `json:"pressureSomePercent,omitempty"`
	PressureFullPercent *float64 `json:"pressureFullPercent,omitempty"`
}

// FilesystemReading maps 1:1 to the contract's FilesystemReading.
type FilesystemReading struct {
	Status      Status `json:"status"`
	Reason      string `json:"reason,omitempty"`
	ID          string `json:"id,omitempty"`
	UsedBytes   *int64 `json:"usedBytes,omitempty"`
	TotalBytes  *int64 `json:"totalBytes,omitempty"`
	InodesUsed  *int64 `json:"inodesUsed,omitempty"`
	InodesTotal *int64 `json:"inodesTotal,omitempty"`
}

// DiskIoReading maps 1:1 to the contract's DiskIoReading.
type DiskIoReading struct {
	Status               Status   `json:"status"`
	Reason               string   `json:"reason,omitempty"`
	ID                   string   `json:"id,omitempty"`
	ReadBytesPerSec      *int64   `json:"readBytesPerSec,omitempty"`
	WriteBytesPerSec     *int64   `json:"writeBytesPerSec,omitempty"`
	ReadLatencyMs        *float64 `json:"readLatencyMs,omitempty"`
	WriteLatencyMs       *float64 `json:"writeLatencyMs,omitempty"`
	IOUtilizationPercent *float64 `json:"ioUtilizationPercent,omitempty"`
}

// DisksMetric maps 1:1 to the contract's DisksMetric.
type DisksMetric struct {
	Status              Status              `json:"status"`
	Reason              string              `json:"reason,omitempty"`
	Filesystems         []FilesystemReading `json:"filesystems,omitempty"`
	Devices             []DiskIoReading     `json:"devices,omitempty"`
	PressureSomePercent *float64            `json:"pressureSomePercent,omitempty"`
	PressureFullPercent *float64            `json:"pressureFullPercent,omitempty"`
}

// InterfaceReading maps 1:1 to the contract's InterfaceReading.
type InterfaceReading struct {
	Status        Status `json:"status"`
	Reason        string `json:"reason,omitempty"`
	ID            string `json:"id,omitempty"`
	RXBytesPerSec *int64 `json:"rxBytesPerSec,omitempty"`
	TXBytesPerSec *int64 `json:"txBytesPerSec,omitempty"`
}

// NetworkMetric maps 1:1 to the contract's NetworkMetric.
type NetworkMetric struct {
	Status     Status             `json:"status"`
	Reason     string             `json:"reason,omitempty"`
	Interfaces []InterfaceReading `json:"interfaces,omitempty"`
}

// Readings is one sampler tick: the non-GPU part of MetricsPayload.
type Readings struct {
	CPU     CpuMetric
	Memory  MemoryMetric
	Swap    BytesUsageMetric
	Disks   DisksMetric
	Network NetworkMetric
}

// CapabilityState maps 1:1 to the contract's CapabilityState.
type CapabilityState struct {
	Status CapabilityStatus `json:"status"`
	Reason string           `json:"reason,omitempty"`
}

// OSInventory maps 1:1 to the contract's Inventory.os.
type OSInventory struct {
	Platform string  `json:"platform"`
	Release  string  `json:"release"`
	Distro   *string `json:"distro"`
	Arch     string  `json:"arch"`
}

// CPUInventory maps 1:1 to the contract's Inventory.cpu.
type CPUInventory struct {
	Model        *string `json:"model"`
	LogicalCores int     `json:"logicalCores"`
}

// MemoryInventory maps 1:1 to the contract's Inventory.memory.
type MemoryInventory struct {
	TotalBytes     *int64 `json:"totalBytes"`
	SwapTotalBytes *int64 `json:"swapTotalBytes"`
}

// FilesystemInventory maps 1:1 to the contract's FilesystemInventory.
type FilesystemInventory struct {
	ID         string `json:"id"`
	MountPoint string `json:"mountPoint"`
	Device     string `json:"device"`
	FSType     string `json:"fsType"`
	TotalBytes *int64 `json:"totalBytes"`
}

// DiskInventory maps 1:1 to the contract's DiskInventory.
type DiskInventory struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Model      *string `json:"model"`
	Rotational *bool   `json:"rotational"`
	SizeBytes  *int64  `json:"sizeBytes"`
}

// InterfaceAddress maps 1:1 to the contract's InterfaceAddress.
type InterfaceAddress struct {
	Family  string `json:"family"`
	Address string `json:"address"`
	Prefix  int    `json:"prefix"`
}

// InterfaceInventory maps 1:1 to the contract's InterfaceInventory.
type InterfaceInventory struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	MAC       *string            `json:"mac"`
	Addresses []InterfaceAddress `json:"addresses"`
	Virtual   bool               `json:"virtual"`
	State     string             `json:"state"`
	SpeedMbps *int64             `json:"speedMbps"`
}

// InventoryCapabilities is the non-GPU part of Inventory.capabilities. The
// GPU entry belongs to the GPU inventory builder.
type InventoryCapabilities struct {
	CPU       CapabilityState `json:"cpu"`
	Memory    CapabilityState `json:"memory"`
	Swap      CapabilityState `json:"swap"`
	Disks     CapabilityState `json:"disks"`
	Network   CapabilityState `json:"network"`
	Processes CapabilityState `json:"processes"`
}

// HostInventory is the non-GPU part of the contract's Inventory. The metrics
// assembler merges its GPU inventory and gpus capability into this value.
type HostInventory struct {
	OS           OSInventory           `json:"os"`
	CPU          CPUInventory          `json:"cpu"`
	Memory       MemoryInventory       `json:"memory"`
	Filesystems  []FilesystemInventory `json:"filesystems"`
	Disks        []DiskInventory       `json:"disks"`
	Network      []InterfaceInventory  `json:"network"`
	Capabilities InventoryCapabilities `json:"capabilities"`
}

// CollectionFailure mirrors the contract's CollectionFailure. The sampler
// keeps the list so the assembler can report it without re-deriving reasons.
type CollectionFailure struct {
	Metric     string  `json:"metric"`
	ResourceID *string `json:"resourceId"`
	Reason     string  `json:"reason"`
}
