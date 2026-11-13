package protocol

// CapabilityName is a capability a collector may advertise in hello.
type CapabilityName string

const (
	CapabilityMetrics   CapabilityName = "metrics"
	CapabilityInventory CapabilityName = "inventory"
	CapabilityProcesses CapabilityName = "processes"
)

// CapabilityNames lists every capability name in wire order.
var CapabilityNames = []CapabilityName{CapabilityMetrics, CapabilityInventory, CapabilityProcesses}

// MetricFamily names a metrics family; families are also the keys of
// MetricsPayload and of the inventory capability record.
type MetricFamily string

const (
	FamilyCPU     MetricFamily = "cpu"
	FamilyMemory  MetricFamily = "memory"
	FamilySwap    MetricFamily = "swap"
	FamilyGPUs    MetricFamily = "gpus"
	FamilyDisks   MetricFamily = "disks"
	FamilyNetwork MetricFamily = "network"
)

// MetricFamilies lists every metric family in wire order.
var MetricFamilies = []MetricFamily{FamilyCPU, FamilyMemory, FamilySwap, FamilyGPUs, FamilyDisks, FamilyNetwork}

// MetricStatus is the availability discriminator on a metric family or reading.
type MetricStatus string

const (
	MetricStatusOK          MetricStatus = "ok"
	MetricStatusUnavailable MetricStatus = "unavailable"
)

// GapReason says why records were dropped.
type GapReason string

const (
	GapOutboxAge    GapReason = "outbox-age"
	GapOutboxBytes  GapReason = "outbox-bytes"
	GapDiskBudget   GapReason = "disk-budget"
	GapWriteFailure GapReason = "write-failure"
	GapRejected     GapReason = "rejected"
)

// GapReasons lists every gap reason in wire order.
var GapReasons = []GapReason{GapOutboxAge, GapOutboxBytes, GapDiskBudget, GapWriteFailure, GapRejected}

// SampleMode distinguishes batches that may update live state from history-only
// replay batches.
type SampleMode string

const (
	SampleModeLive   SampleMode = "live"
	SampleModeReplay SampleMode = "replay"
)

// RecordKind discriminates telemetry records.
type RecordKind string

const (
	RecordKindMetrics RecordKind = "metrics"
	RecordKindGap     RecordKind = "gap"
)

// DeliveryMode is the hub's durable-commit guarantee advertised in welcome.
type DeliveryMode string

const (
	DeliveryDurable DeliveryMode = "durable"
	DeliveryMemory  DeliveryMode = "memory"
)

// QueueMode is how a collector retains unacknowledged records.
type QueueMode string

const (
	QueueModeMemory  QueueMode = "memory"
	QueueModeDurable QueueMode = "durable"
)

// CapabilityStatus is the availability state of one capability in inventory.
type CapabilityStatus string

const (
	CapabilitySupported   CapabilityStatus = "supported"
	CapabilityUnsupported CapabilityStatus = "unsupported"
	CapabilityWithheld    CapabilityStatus = "withheld"
)

// DetailKind is the kind of detail subscription. "processes" is the only kind.
type DetailKind string

const DetailKindProcesses DetailKind = "processes"

// DetailEndedReason explains why a detail subscription ended.
type DetailEndedReason string

const (
	DetailEndedExpired     DetailEndedReason = "expired"
	DetailEndedCancelled   DetailEndedReason = "cancelled"
	DetailEndedWithheld    DetailEndedReason = "withheld"
	DetailEndedUnsupported DetailEndedReason = "unsupported"
	DetailEndedBusy        DetailEndedReason = "busy"
	DetailEndedFailed      DetailEndedReason = "failed"
)

// GpuVendor is a GPU vendor in inventory.
type GpuVendor string

const (
	GpuVendorNvidia GpuVendor = "nvidia"
	GpuVendorAMD    GpuVendor = "amd"
	GpuVendorIntel  GpuVendor = "intel"
	GpuVendorAspeed GpuVendor = "aspeed"
	GpuVendorOther  GpuVendor = "other"
)

// TelemetrySupport says whether a GPU inventory entry exposes telemetry.
type TelemetrySupport string

const (
	TelemetrySupported   TelemetrySupport = "supported"
	TelemetryUnsupported TelemetrySupport = "unsupported"
)

// AddressFamily is an interface address family.
type AddressFamily string

const (
	AddressFamilyIPv4 AddressFamily = "ipv4"
	AddressFamilyIPv6 AddressFamily = "ipv6"
)

// InterfaceState is an interface link state in inventory.
type InterfaceState string

const (
	InterfaceStateUp      InterfaceState = "up"
	InterfaceStateDown    InterfaceState = "down"
	InterfaceStateUnknown InterfaceState = "unknown"
)
