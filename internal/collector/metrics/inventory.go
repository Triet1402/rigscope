package metrics

import (
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/Triet1402/rigscope/internal/collector/sampling/gpu"
	"github.com/Triet1402/rigscope/internal/collector/sampling/procfs"
	"github.com/Triet1402/rigscope/internal/protocol"
)

// PCIDisplayDevicesDir is where Linux enumerates PCI functions.
const PCIDisplayDevicesDir = "/sys/bus/pci/devices"

var (
	pciBusIDPattern = regexp.MustCompile(`^(?:([0-9a-f]{4,8}):)?([0-9a-f]{2}:[0-9a-f]{2}\.[0-7])$`)
	pciHeadingRE    = regexp.MustCompile(`^([0-9a-f]{4})\s+`)
	pciDeviceRE     = regexp.MustCompile(`^\t([0-9a-f]{4})\s+(.+)$`)
)

// buildInventory assembles the machine inventory from the host sampler and
// the GPU package. Individual sources degrade independently; a sampler that
// panics falls back to the last good inventory or an honest empty one instead
// of taking the collector down.
func (c *Collector) buildInventory() (inventory protocol.Inventory) {
	defer func() {
		if recover() == nil {
			return
		}
		if c.lastInventory.OS.Platform != "" {
			inventory = c.lastInventory
			return
		}
		inventory = fallbackInventory()
	}()
	host := c.proc.Inventory()
	gpus := c.buildGPUInventory()
	return protocol.Inventory{
		OS: protocol.OSInventory{
			Platform: boundedText(host.OS.Platform, 64),
			Release:  boundedText(host.OS.Release, protocol.MaxTextLength),
			Distro:   optionalText(host.OS.Distro, protocol.MaxTextLength),
			Arch:     boundedText(host.OS.Arch, 64),
		},
		CPU: protocol.CPUInventory{
			Model:        optionalText(host.CPU.Model, protocol.MaxTextLength),
			LogicalCores: boundedInt64(int64(host.CPU.LogicalCores), 0, protocol.MaxCores),
		},
		Memory: protocol.MemoryInventory{
			TotalBytes:     boundedInt(host.Memory.TotalBytes, 0, protocol.MaxSafeInteger),
			SwapTotalBytes: boundedInt(host.Memory.SwapTotalBytes, 0, protocol.MaxSafeInteger),
		},
		GPUs:         gpus,
		Filesystems:  filesystemInventory(host.Filesystems),
		Disks:        diskInventory(host.Disks),
		Network:      networkInventory(host.Network),
		Capabilities: inventoryCapabilities(host.Capabilities, gpus, c.gpu),
	}
}

// fallbackInventory is the inventory used when no sampler can be read at all.
// It reports every capability as unsupported rather than inventing resources.
func fallbackInventory() protocol.Inventory {
	capability := func() protocol.CapabilityState {
		reason := "inventory unavailable"
		return protocol.CapabilityState{Status: protocol.CapabilityUnsupported, Reason: &reason}
	}
	return protocol.Inventory{
		OS:          protocol.OSInventory{Platform: boundedText(runtime.GOOS, 64)},
		GPUs:        []protocol.GpuInventory{},
		Filesystems: []protocol.FilesystemInventory{},
		Disks:       []protocol.DiskInventory{},
		Network:     []protocol.InterfaceInventory{},
		Capabilities: protocol.Capabilities{
			CPU: capability(), Memory: capability(), Swap: capability(), GPUs: capability(),
			Disks: capability(), Network: capability(), Processes: capability(),
		},
	}
}

func optionalText(value *string, max int) *string {
	if value == nil {
		return nil
	}
	bounded := boundedText(*value, max)
	return &bounded
}

// optionalNonEmptyText is for fields whose contract minimum length is one,
// such as an interface MAC. An empty string is not a value.
func optionalNonEmptyText(value *string, max int) *string {
	if value == nil {
		return nil
	}
	bounded := boundedText(*value, max)
	if bounded == "" {
		return nil
	}
	return &bounded
}

func filesystemInventory(entries []procfs.FilesystemInventory) []protocol.FilesystemInventory {
	out := make([]protocol.FilesystemInventory, 0, len(entries))
	for _, entry := range entries {
		if len(out) >= protocol.MaxFilesystems {
			break
		}
		out = append(out, protocol.FilesystemInventory{
			ID:         boundedText(entry.ID, protocol.MaxResourceIDLength),
			MountPoint: boundedText(entry.MountPoint, protocol.MaxTextLength),
			Device:     boundedText(entry.Device, protocol.MaxTextLength),
			FsType:     boundedText(entry.FSType, 64),
			TotalBytes: boundedInt(entry.TotalBytes, 0, protocol.MaxSafeInteger),
		})
	}
	return out
}

func diskInventory(entries []procfs.DiskInventory) []protocol.DiskInventory {
	out := make([]protocol.DiskInventory, 0, len(entries))
	for _, entry := range entries {
		if len(out) >= protocol.MaxDisks {
			break
		}
		out = append(out, protocol.DiskInventory{
			ID:         boundedText(entry.ID, protocol.MaxResourceIDLength),
			Name:       boundedText(entry.Name, 64),
			Model:      optionalText(entry.Model, protocol.MaxTextLength),
			Rotational: entry.Rotational,
			SizeBytes:  boundedInt(entry.SizeBytes, 0, protocol.MaxSafeInteger),
		})
	}
	return out
}

func networkInventory(entries []procfs.InterfaceInventory) []protocol.InterfaceInventory {
	out := make([]protocol.InterfaceInventory, 0, len(entries))
	for _, entry := range entries {
		if len(out) >= protocol.MaxInterfaces {
			break
		}
		addresses := make([]protocol.InterfaceAddress, 0, len(entry.Addresses))
		for _, address := range entry.Addresses {
			if len(addresses) >= protocol.MaxAddressesPerInterface {
				break
			}
			family := protocol.AddressFamily(strings.ToLower(address.Family))
			if family != protocol.AddressFamilyIPv4 && family != protocol.AddressFamilyIPv6 {
				continue
			}
			maxPrefix := int64(32)
			if family == protocol.AddressFamilyIPv6 {
				maxPrefix = 128
			}
			prefix := int64(address.Prefix)
			if prefix < 0 || prefix > maxPrefix {
				continue
			}
			text := boundedText(address.Address, 64)
			if text == "" {
				continue
			}
			addresses = append(addresses, protocol.InterfaceAddress{Family: family, Address: text, Prefix: prefix})
		}
		state := protocol.InterfaceState(strings.ToLower(entry.State))
		switch state {
		case protocol.InterfaceStateUp, protocol.InterfaceStateDown, protocol.InterfaceStateUnknown:
		default:
			state = protocol.InterfaceStateUnknown
		}
		out = append(out, protocol.InterfaceInventory{
			ID:        boundedText(entry.ID, protocol.MaxResourceIDLength),
			Name:      boundedText(entry.Name, 64),
			Mac:       optionalNonEmptyText(entry.MAC, 64),
			Addresses: addresses,
			Virtual:   entry.Virtual,
			State:     state,
			SpeedMbps: boundedInt(entry.SpeedMbps, 0, 10_000_000),
		})
	}
	return out
}

func inventoryCapabilities(host procfs.InventoryCapabilities, gpus []protocol.GpuInventory, source GPUSource) protocol.Capabilities {
	return protocol.Capabilities{
		CPU:       capabilityState(host.CPU),
		Memory:    capabilityState(host.Memory),
		Swap:      capabilityState(host.Swap),
		GPUs:      gpuCapability(gpus, source),
		Disks:     capabilityState(host.Disks),
		Network:   capabilityState(host.Network),
		Processes: capabilityState(host.Processes),
	}
}

func capabilityState(state procfs.CapabilityState) protocol.CapabilityState {
	status := protocol.CapabilityUnsupported
	if state.Status == procfs.CapabilitySupported {
		status = protocol.CapabilitySupported
	}
	out := protocol.CapabilityState{Status: status}
	if state.Reason != "" {
		reason := boundedText(state.Reason, protocol.MaxTextLength)
		out.Reason = &reason
	}
	return out
}

// --- GPU inventory ---------------------------------------------------------

type pciDisplayDevice struct {
	address  string
	vendor   protocol.GpuVendor
	vendorID string
}

// buildGPUInventory enumerates PCI display functions and joins NVIDIA devices
// to the poller's device descriptions. AMD/Intel devices contribute their DRM
// counters; management adapters are listed as unsupported, never with
// fabricated readings.
func (c *Collector) buildGPUInventory() []protocol.GpuInventory {
	devices := enumeratePCIDisplays(c.gpuFS)
	if len(devices) == 0 {
		return []protocol.GpuInventory{}
	}
	previous := make(map[string]protocol.GpuInventory, len(c.lastInventory.GPUs))
	for _, entry := range c.lastInventory.GPUs {
		if entry.PciBusID != nil {
			previous[*entry.PciBusID] = entry
		}
	}
	nvidia := make(map[string]gpu.DeviceInfo)
	if c.gpu != nil {
		for _, info := range c.gpu.DeviceInfo() {
			if info.PCIBusID != nil && *info.PCIBusID != "" {
				nvidia[normalizePCIBusID(*info.PCIBusID)] = info
			}
		}
	}
	pciNames := c.readPCINames()

	result := make([]protocol.GpuInventory, 0, len(devices))
	seen := make(map[string]bool, len(devices))
	for _, device := range devices {
		entry := c.gpuInventoryEntry(device, nvidia, previous, pciNames)
		if seen[entry.ID] {
			continue
		}
		seen[entry.ID] = true
		result = append(result, entry)
		if len(result) >= protocol.MaxGPUs {
			break
		}
	}
	return result
}

func (c *Collector) gpuInventoryEntry(
	device pciDisplayDevice,
	nvidia map[string]gpu.DeviceInfo,
	previous map[string]protocol.GpuInventory,
	pciNames string,
) protocol.GpuInventory {
	pciBusID := device.address
	if device.vendor == protocol.GpuVendorNvidia {
		if info, ok := nvidia[normalizePCIBusID(device.address)]; ok {
			return protocol.GpuInventory{
				ID:               boundedText("gpu:"+info.UUID, protocol.MaxResourceIDLength),
				Name:             boundedText(joinedGPUName(info.Name, device.vendor), protocol.MaxTextLength),
				Vendor:           device.vendor,
				PciBusID:         &pciBusID,
				Telemetry:        protocol.TelemetrySupported,
				MemoryTotalBytes: boundedInt(info.MemoryTotalBytes, 0, protocol.MaxSafeInteger),
			}
		}
	}

	if c.gate != nil && !c.gate.Decide(device.address).Sample {
		// Reading DRM counters would wake a powered-down GPU. Keep the last
		// description when it was readable, otherwise state that telemetry is
		// expected until the device is next awake.
		if last, ok := previous[device.address]; ok && last.Telemetry == protocol.TelemetrySupported {
			return last
		}
		return protocol.GpuInventory{
			ID:        boundedText("pci:"+device.address, protocol.MaxResourceIDLength),
			Name:      boundedText(c.gpuDisplayName(device, pciNames), protocol.MaxTextLength),
			Vendor:    device.vendor,
			PciBusID:  &pciBusID,
			Telemetry: protocol.TelemetrySupported,
		}
	}

	reading, accessErr := gpu.ReadSysfsGpuGuarded(gpu.GateAccess(c.gate), c.gpuFS, device.address)
	if accessErr != nil {
		// The arbiter denied access after the gate decision (a revocation
		// raced it). Keep the last readable description, otherwise state that
		// telemetry is expected once the device is allowed again.
		if last, ok := previous[device.address]; ok && last.Telemetry == protocol.TelemetrySupported {
			return last
		}
		return protocol.GpuInventory{
			ID:        boundedText("pci:"+device.address, protocol.MaxResourceIDLength),
			Name:      boundedText(c.gpuDisplayName(device, pciNames), protocol.MaxTextLength),
			Vendor:    device.vendor,
			PciBusID:  &pciBusID,
			Telemetry: protocol.TelemetrySupported,
		}
	}
	entry := protocol.GpuInventory{
		ID:        boundedText("pci:"+device.address, protocol.MaxResourceIDLength),
		Name:      boundedText(c.gpuDisplayName(device, pciNames), protocol.MaxTextLength),
		Vendor:    device.vendor,
		PciBusID:  &pciBusID,
		Telemetry: protocol.TelemetrySupported,
	}
	if reading.Status == gpu.StatusOK {
		entry.MemoryTotalBytes = boundedInt(reading.MemoryTotalBytes, 0, protocol.MaxSafeInteger)
		return entry
	}
	entry.Telemetry = protocol.TelemetryUnsupported
	reason := ""
	if device.vendor == protocol.GpuVendorAMD || device.vendor == protocol.GpuVendorIntel {
		reason = reading.Reason
	}
	if reason == "" {
		reason = unsupportedGPUReason(device.vendor, c.gpu)
	}
	reason = boundedText(reason, protocol.MaxTextLength)
	entry.Reason = &reason
	return entry
}

func (c *Collector) gpuDisplayName(device pciDisplayDevice, pciNames string) string {
	if product, ok := c.gpuFS.ReadText(PCIDisplayDevicesDir + "/" + device.address + "/product_name"); ok {
		if trimmed := strings.TrimSpace(product); trimmed != "" {
			return trimmed
		}
	}
	if deviceID, ok := c.gpuFS.ReadText(PCIDisplayDevicesDir + "/" + device.address + "/device"); ok {
		if name := pciDeviceName(pciNames, device.vendorID, strings.TrimSpace(deviceID)); name != "" {
			return name
		}
	}
	return fallbackGPUName(device.vendor)
}

func joinedGPUName(name string, vendor protocol.GpuVendor) string {
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		return trimmed
	}
	return fallbackGPUName(vendor)
}

func (c *Collector) readPCINames() string {
	if c.pciNamesRead {
		return c.pciNames
	}
	c.pciNamesRead = true
	for _, path := range []string{"/usr/share/hwdata/pci.ids", "/usr/share/misc/pci.ids"} {
		if text, ok := c.gpuFS.ReadText(path); ok {
			c.pciNames = text
			return c.pciNames
		}
	}
	return ""
}

func enumeratePCIDisplays(sysfs gpu.SysFS) []pciDisplayDevice {
	if sysfs == nil {
		return nil
	}
	names, err := sysfs.ReadDir(PCIDisplayDevicesDir)
	if err != nil {
		return nil
	}
	sort.Strings(names)
	if len(names) > 256 {
		names = names[:256]
	}
	devices := make([]pciDisplayDevice, 0, len(names))
	for _, address := range names {
		base := PCIDisplayDevicesDir + "/" + address
		classText, ok := sysfs.ReadText(base + "/class")
		if !ok || !isDisplayClass(classText) {
			continue
		}
		vendorText, _ := sysfs.ReadText(base + "/vendor")
		vendorID := strings.ToLower(strings.TrimSpace(vendorText))
		devices = append(devices, pciDisplayDevice{
			address:  address,
			vendor:   vendorForID(vendorID),
			vendorID: vendorID,
		})
	}
	return devices
}

// isDisplayClass accepts PCI base class 0x03 (VGA/3D controllers).
func isDisplayClass(raw string) bool {
	value, ok := parsePCIHex(raw)
	if !ok {
		return false
	}
	return value>>16 == 0x03
}

func parsePCIHex(raw string) (uint32, bool) {
	text := strings.TrimSpace(strings.ToLower(raw))
	text = strings.TrimPrefix(text, "0x")
	if text == "" {
		return 0, false
	}
	value, err := strconv.ParseUint(text, 16, 32)
	if err != nil {
		return 0, false
	}
	return uint32(value), true
}

func vendorForID(raw string) protocol.GpuVendor {
	switch strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "0x"))) {
	case "10de":
		return protocol.GpuVendorNvidia
	case "1002":
		return protocol.GpuVendorAMD
	case "8086":
		return protocol.GpuVendorIntel
	case "1a03":
		return protocol.GpuVendorAspeed
	default:
		return protocol.GpuVendorOther
	}
}

func fallbackGPUName(vendor protocol.GpuVendor) string {
	switch vendor {
	case protocol.GpuVendorNvidia:
		return "NVIDIA display adapter"
	case protocol.GpuVendorAMD:
		return "AMD display adapter"
	case protocol.GpuVendorIntel:
		return "Intel display adapter"
	case protocol.GpuVendorAspeed:
		return "ASPEED management VGA"
	default:
		return "Display adapter"
	}
}

func unsupportedGPUReason(vendor protocol.GpuVendor, source GPUSource) string {
	if vendor == protocol.GpuVendorAspeed {
		return "management adapter; no telemetry"
	}
	if vendor == protocol.GpuVendorNvidia && (source == nil || source.Available() == gpu.AvailabilityNo) {
		return "no supported GPU telemetry (nvidia-smi not found)"
	}
	return "no supported telemetry"
}

// pciDeviceName resolves a vendor/device pair from the distro's pci.ids
// database. It returns "" when the database does not describe the device.
func pciDeviceName(database, vendorID, deviceID string) string {
	if database == "" || deviceID == "" {
		return ""
	}
	vendorWanted := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(vendorID)), "0x")
	deviceWanted := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(deviceID)), "0x")
	matchVendor := false
	for _, line := range strings.Split(database, "\n") {
		if heading := pciHeadingRE.FindStringSubmatch(line); heading != nil {
			matchVendor = heading[1] == vendorWanted
			continue
		}
		if !matchVendor {
			continue
		}
		device := pciDeviceRE.FindStringSubmatch(line)
		if device == nil || device[1] != deviceWanted {
			continue
		}
		return boundedText(device[2], protocol.MaxTextLength)
	}
	return ""
}

// gpuCapability reports whether any GPU exposes telemetry. It mirrors the
// prototype: a supported device wins, then the reason explains why none is.
func gpuCapability(gpus []protocol.GpuInventory, source GPUSource) protocol.CapabilityState {
	for _, entry := range gpus {
		if entry.Telemetry == protocol.TelemetrySupported {
			return protocol.CapabilityState{Status: protocol.CapabilitySupported}
		}
	}
	if len(gpus) == 0 {
		return capabilityWithReason("no GPUs detected")
	}
	hasNvidia := false
	for _, entry := range gpus {
		if entry.Vendor == protocol.GpuVendorNvidia {
			hasNvidia = true
			break
		}
	}
	if !hasNvidia {
		return capabilityWithReason("no readable DRM GPU counters")
	}
	if source == nil || source.Available() == gpu.AvailabilityNo || source.Available() == gpu.AvailabilityUnknown {
		return capabilityWithReason("no supported GPU telemetry (nvidia-smi not found)")
	}
	return capabilityWithReason("no supported GPU telemetry")
}

func capabilityWithReason(reason string) protocol.CapabilityState {
	capped := boundedText(reason, protocol.MaxTextLength)
	return protocol.CapabilityState{Status: protocol.CapabilityUnsupported, Reason: &capped}
}

// normalizePCIBusID preserves the PCI domain, unlike the GPU package's
// bus:function join key. Joining NVIDIA descriptions must not transfer a
// device from one domain to a same-numbered function in another.
func normalizePCIBusID(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	match := pciBusIDPattern.FindStringSubmatch(lower)
	if match == nil {
		return lower
	}
	domain := "0"
	if match[1] != "" {
		domain = match[1]
	}
	domain = strings.TrimLeft(domain, "0")
	if domain == "" {
		domain = "0"
	}
	for len(domain) < 4 {
		domain = "0" + domain
	}
	return domain + ":" + match[2]
}

// inventorySignature is a stable, order-preserving digest of exactly the
// fields the prototype tracked. It answers "did the hub's inventory change?"
// without hashing unrelated capacity values that fluctuate per mount.
func inventorySignature(inventory protocol.Inventory) string {
	var builder strings.Builder
	writeString := func(value string) {
		fmt.Fprintf(&builder, "%d:%s;", len(value), value)
	}
	writeText := func(value *string) {
		if value == nil {
			builder.WriteString("-;")
			return
		}
		writeString(*value)
	}
	writeInt := func(value *int64) {
		if value == nil {
			builder.WriteString("-;")
			return
		}
		fmt.Fprintf(&builder, "%d;", *value)
	}

	builder.WriteString("gpus:")
	for _, entry := range inventory.GPUs {
		writeString(entry.ID)
		writeString(string(entry.Vendor))
		writeString(string(entry.Telemetry))
		writeInt(entry.MemoryTotalBytes)
		writeText(entry.PciBusID)
	}
	builder.WriteString("filesystems:")
	for _, entry := range inventory.Filesystems {
		writeString(entry.ID)
		writeInt(entry.TotalBytes)
	}
	builder.WriteString("disks:")
	for _, entry := range inventory.Disks {
		writeString(entry.ID)
		writeInt(entry.SizeBytes)
	}
	builder.WriteString("network:")
	for _, entry := range inventory.Network {
		writeString(entry.ID)
		writeString(string(entry.State))
		writeInt(entry.SpeedMbps)
		for _, address := range entry.Addresses {
			writeString(string(address.Family))
			writeString(address.Address)
			fmt.Fprintf(&builder, "%d;", address.Prefix)
		}
	}
	builder.WriteString("capabilities:")
	capabilities := []protocol.CapabilityState{
		inventory.Capabilities.CPU,
		inventory.Capabilities.Memory,
		inventory.Capabilities.Swap,
		inventory.Capabilities.GPUs,
		inventory.Capabilities.Disks,
		inventory.Capabilities.Network,
		inventory.Capabilities.Processes,
	}
	for _, capability := range capabilities {
		writeString(string(capability.Status))
		writeText(capability.Reason)
	}
	return builder.String()
}
