package protocol

import "encoding/json"

// CapabilityState is the availability of one capability in inventory.
// Withheld means the collector's sharing policy hides it from this hub.
type CapabilityState struct {
	Status CapabilityStatus `json:"status"`
	Reason *string          `json:"reason,omitempty"`
}

// Capabilities is the per-family plus processes capability record.
type Capabilities struct {
	CPU       CapabilityState `json:"cpu"`
	Memory    CapabilityState `json:"memory"`
	Swap      CapabilityState `json:"swap"`
	GPUs      CapabilityState `json:"gpus"`
	Disks     CapabilityState `json:"disks"`
	Network   CapabilityState `json:"network"`
	Processes CapabilityState `json:"processes"`
}

// OSInventory describes the operating system.
type OSInventory struct {
	Platform string  `json:"platform"`
	Release  string  `json:"release"`
	Distro   *string `json:"distro"`
	Arch     string  `json:"arch"`
}

// CPUInventory describes the CPU model and logical core count.
type CPUInventory struct {
	Model        *string `json:"model"`
	LogicalCores int64   `json:"logicalCores"`
}

// MemoryInventory describes installed memory and swap capacity.
type MemoryInventory struct {
	TotalBytes     *int64 `json:"totalBytes"`
	SwapTotalBytes *int64 `json:"swapTotalBytes"`
}

// GpuInventory describes one GPU. Management adapters carry telemetry
// "unsupported" and an optional reason.
type GpuInventory struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Vendor           GpuVendor        `json:"vendor"`
	PciBusID         *string          `json:"pciBusId"`
	Telemetry        TelemetrySupport `json:"telemetry"`
	MemoryTotalBytes *int64           `json:"memoryTotalBytes"`
	Reason           *string          `json:"reason,omitempty"`
}

// FilesystemInventory describes one mounted filesystem.
type FilesystemInventory struct {
	ID         string `json:"id"`
	MountPoint string `json:"mountPoint"`
	Device     string `json:"device"`
	FsType     string `json:"fsType"`
	TotalBytes *int64 `json:"totalBytes"`
}

// DiskInventory describes one block device.
type DiskInventory struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Model      *string `json:"model"`
	Rotational *bool   `json:"rotational"`
	SizeBytes  *int64  `json:"sizeBytes"`
}

// InterfaceAddress is one address on an interface.
type InterfaceAddress struct {
	Family  AddressFamily `json:"family"`
	Address string        `json:"address"`
	Prefix  int64         `json:"prefix"`
}

// InterfaceInventory describes one network interface. Addresses are empty when
// withheld by the sharing policy.
type InterfaceInventory struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Mac       *string            `json:"mac"`
	Addresses []InterfaceAddress `json:"addresses"`
	Virtual   bool               `json:"virtual"`
	State     InterfaceState     `json:"state"`
	SpeedMbps *int64             `json:"speedMbps"`
}

// Inventory is the machine inventory sent in an inventory message.
type Inventory struct {
	OS           OSInventory           `json:"os"`
	CPU          CPUInventory          `json:"cpu"`
	Memory       MemoryInventory       `json:"memory"`
	GPUs         []GpuInventory        `json:"gpus"`
	Filesystems  []FilesystemInventory `json:"filesystems"`
	Disks        []DiskInventory       `json:"disks"`
	Network      []InterfaceInventory  `json:"network"`
	Capabilities Capabilities          `json:"capabilities"`
}

// MarshalJSON keeps every list an array instead of null.
func (i Inventory) MarshalJSON() ([]byte, error) {
	if i.GPUs == nil {
		i.GPUs = []GpuInventory{}
	}
	if i.Filesystems == nil {
		i.Filesystems = []FilesystemInventory{}
	}
	if i.Disks == nil {
		i.Disks = []DiskInventory{}
	}
	network := make([]InterfaceInventory, len(i.Network))
	copy(network, i.Network)
	for j := range network {
		if network[j].Addresses == nil {
			network[j].Addresses = []InterfaceAddress{}
		}
	}
	i.Network = network
	type plain Inventory
	return json.Marshal(plain(i))
}
