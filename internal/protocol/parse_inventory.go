package protocol

// parseInventory strictly validates an inventory object.
func (p *parser) parseInventory(v any) (Inventory, error) {
	o, err := p.object(v)
	if err != nil {
		return Inventory{}, err
	}
	if err := p.keys(o, []string{"os", "cpu", "memory", "gpus", "filesystems", "disks", "network", "capabilities"}, nil); err != nil {
		return Inventory{}, err
	}
	var out Inventory
	if out.OS, err = p.child("os").parseOSInventory(o["os"]); err != nil {
		return Inventory{}, err
	}
	if out.CPU, err = p.child("cpu").parseCPUInventory(o["cpu"]); err != nil {
		return Inventory{}, err
	}
	if out.Memory, err = p.child("memory").parseMemoryInventory(o["memory"]); err != nil {
		return Inventory{}, err
	}
	if out.Capabilities, err = p.child("capabilities").parseCapabilities(o["capabilities"]); err != nil {
		return Inventory{}, err
	}
	if out.GPUs, err = p.child("gpus").parseGpuInventories(o["gpus"]); err != nil {
		return Inventory{}, err
	}
	if out.Filesystems, err = p.child("filesystems").parseFilesystemInventories(o["filesystems"]); err != nil {
		return Inventory{}, err
	}
	if out.Disks, err = p.child("disks").parseDiskInventories(o["disks"]); err != nil {
		return Inventory{}, err
	}
	if out.Network, err = p.child("network").parseInterfaceInventories(o["network"]); err != nil {
		return Inventory{}, err
	}
	return out, nil
}

func (p *parser) parseOSInventory(v any) (OSInventory, error) {
	o, err := p.object(v)
	if err != nil {
		return OSInventory{}, err
	}
	if err := p.keys(o, []string{"platform", "release", "distro", "arch"}, nil); err != nil {
		return OSInventory{}, err
	}
	var out OSInventory
	if out.Platform, err = p.child("platform").text(o["platform"], 0, 64); err != nil {
		return OSInventory{}, err
	}
	if out.Release, err = p.child("release").text(o["release"], 0, MaxTextLength); err != nil {
		return OSInventory{}, err
	}
	if out.Distro, err = p.child("distro").nullableText(o["distro"], 0, MaxTextLength); err != nil {
		return OSInventory{}, err
	}
	if out.Arch, err = p.child("arch").text(o["arch"], 0, 64); err != nil {
		return OSInventory{}, err
	}
	return out, nil
}

func (p *parser) parseCPUInventory(v any) (CPUInventory, error) {
	o, err := p.object(v)
	if err != nil {
		return CPUInventory{}, err
	}
	if err := p.keys(o, []string{"model", "logicalCores"}, nil); err != nil {
		return CPUInventory{}, err
	}
	var out CPUInventory
	if out.Model, err = p.child("model").nullableText(o["model"], 0, MaxTextLength); err != nil {
		return CPUInventory{}, err
	}
	if out.LogicalCores, err = p.child("logicalCores").integer(o["logicalCores"], 0, MaxCores); err != nil {
		return CPUInventory{}, err
	}
	return out, nil
}

func (p *parser) parseMemoryInventory(v any) (MemoryInventory, error) {
	o, err := p.object(v)
	if err != nil {
		return MemoryInventory{}, err
	}
	if err := p.keys(o, []string{"totalBytes", "swapTotalBytes"}, nil); err != nil {
		return MemoryInventory{}, err
	}
	var out MemoryInventory
	if out.TotalBytes, err = p.child("totalBytes").nullableInteger(o["totalBytes"], 0, MaxSafeInteger); err != nil {
		return MemoryInventory{}, err
	}
	if out.SwapTotalBytes, err = p.child("swapTotalBytes").nullableInteger(o["swapTotalBytes"], 0, MaxSafeInteger); err != nil {
		return MemoryInventory{}, err
	}
	return out, nil
}

func (p *parser) parseCapabilities(v any) (Capabilities, error) {
	o, err := p.object(v)
	if err != nil {
		return Capabilities{}, err
	}
	if err := p.keys(o, []string{"cpu", "memory", "swap", "gpus", "disks", "network", "processes"}, nil); err != nil {
		return Capabilities{}, err
	}
	var out Capabilities
	if out.CPU, err = p.child("cpu").parseCapabilityState(o["cpu"]); err != nil {
		return Capabilities{}, err
	}
	if out.Memory, err = p.child("memory").parseCapabilityState(o["memory"]); err != nil {
		return Capabilities{}, err
	}
	if out.Swap, err = p.child("swap").parseCapabilityState(o["swap"]); err != nil {
		return Capabilities{}, err
	}
	if out.GPUs, err = p.child("gpus").parseCapabilityState(o["gpus"]); err != nil {
		return Capabilities{}, err
	}
	if out.Disks, err = p.child("disks").parseCapabilityState(o["disks"]); err != nil {
		return Capabilities{}, err
	}
	if out.Network, err = p.child("network").parseCapabilityState(o["network"]); err != nil {
		return Capabilities{}, err
	}
	if out.Processes, err = p.child("processes").parseCapabilityState(o["processes"]); err != nil {
		return Capabilities{}, err
	}
	return out, nil
}

func (p *parser) parseCapabilityState(v any) (CapabilityState, error) {
	o, err := p.object(v)
	if err != nil {
		return CapabilityState{}, err
	}
	if err := p.keys(o, []string{"status"}, []string{"reason"}); err != nil {
		return CapabilityState{}, err
	}
	var out CapabilityState
	status, err := p.child("status").oneOf(
		o["status"],
		string(CapabilitySupported),
		string(CapabilityUnsupported),
		string(CapabilityWithheld),
	)
	if err != nil {
		return CapabilityState{}, err
	}
	out.Status = CapabilityStatus(status)
	if reason, ok := o["reason"]; ok {
		text, err := p.child("reason").text(reason, 0, MaxTextLength)
		if err != nil {
			return CapabilityState{}, err
		}
		out.Reason = &text
	}
	return out, nil
}

func (p *parser) parseGpuInventories(v any) ([]GpuInventory, error) {
	raw, err := p.array(v, 0, MaxGPUs)
	if err != nil {
		return nil, err
	}
	out := make([]GpuInventory, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for i, item := range raw {
		gpu, err := p.at(i).parseGpuInventory(item)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[gpu.ID]; duplicate {
			return nil, p.errf("duplicate resource id %s", describe(gpu.ID))
		}
		seen[gpu.ID] = struct{}{}
		out[i] = gpu
	}
	return out, nil
}

func (p *parser) parseGpuInventory(v any) (GpuInventory, error) {
	o, err := p.object(v)
	if err != nil {
		return GpuInventory{}, err
	}
	if err := p.keys(
		o,
		[]string{"id", "name", "vendor", "pciBusId", "telemetry", "memoryTotalBytes"},
		[]string{"reason"},
	); err != nil {
		return GpuInventory{}, err
	}
	var out GpuInventory
	if out.ID, err = p.child("id").resourceID(o["id"]); err != nil {
		return GpuInventory{}, err
	}
	if out.Name, err = p.child("name").text(o["name"], 0, MaxTextLength); err != nil {
		return GpuInventory{}, err
	}
	vendor, err := p.child("vendor").oneOf(
		o["vendor"],
		string(GpuVendorNvidia),
		string(GpuVendorAMD),
		string(GpuVendorIntel),
		string(GpuVendorAspeed),
		string(GpuVendorOther),
	)
	if err != nil {
		return GpuInventory{}, err
	}
	out.Vendor = GpuVendor(vendor)
	if out.PciBusID, err = p.child("pciBusId").nullableText(o["pciBusId"], 1, 64); err != nil {
		return GpuInventory{}, err
	}
	telemetry, err := p.child("telemetry").oneOf(
		o["telemetry"],
		string(TelemetrySupported),
		string(TelemetryUnsupported),
	)
	if err != nil {
		return GpuInventory{}, err
	}
	out.Telemetry = TelemetrySupport(telemetry)
	if out.MemoryTotalBytes, err = p.child("memoryTotalBytes").nullableInteger(o["memoryTotalBytes"], 0, MaxSafeInteger); err != nil {
		return GpuInventory{}, err
	}
	if reason, ok := o["reason"]; ok {
		text, err := p.child("reason").text(reason, 0, MaxTextLength)
		if err != nil {
			return GpuInventory{}, err
		}
		out.Reason = &text
	}
	return out, nil
}

func (p *parser) parseFilesystemInventories(v any) ([]FilesystemInventory, error) {
	raw, err := p.array(v, 0, MaxFilesystems)
	if err != nil {
		return nil, err
	}
	out := make([]FilesystemInventory, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for i, item := range raw {
		fs, err := p.at(i).parseFilesystemInventory(item)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[fs.ID]; duplicate {
			return nil, p.errf("duplicate resource id %s", describe(fs.ID))
		}
		seen[fs.ID] = struct{}{}
		out[i] = fs
	}
	return out, nil
}

func (p *parser) parseFilesystemInventory(v any) (FilesystemInventory, error) {
	o, err := p.object(v)
	if err != nil {
		return FilesystemInventory{}, err
	}
	if err := p.keys(o, []string{"id", "mountPoint", "device", "fsType", "totalBytes"}, nil); err != nil {
		return FilesystemInventory{}, err
	}
	var out FilesystemInventory
	if out.ID, err = p.child("id").resourceID(o["id"]); err != nil {
		return FilesystemInventory{}, err
	}
	if out.MountPoint, err = p.child("mountPoint").text(o["mountPoint"], 1, MaxTextLength); err != nil {
		return FilesystemInventory{}, err
	}
	if out.Device, err = p.child("device").text(o["device"], 0, MaxTextLength); err != nil {
		return FilesystemInventory{}, err
	}
	if out.FsType, err = p.child("fsType").text(o["fsType"], 0, 64); err != nil {
		return FilesystemInventory{}, err
	}
	if out.TotalBytes, err = p.child("totalBytes").nullableInteger(o["totalBytes"], 0, MaxSafeInteger); err != nil {
		return FilesystemInventory{}, err
	}
	return out, nil
}

func (p *parser) parseDiskInventories(v any) ([]DiskInventory, error) {
	raw, err := p.array(v, 0, MaxDisks)
	if err != nil {
		return nil, err
	}
	out := make([]DiskInventory, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for i, item := range raw {
		disk, err := p.at(i).parseDiskInventory(item)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[disk.ID]; duplicate {
			return nil, p.errf("duplicate resource id %s", describe(disk.ID))
		}
		seen[disk.ID] = struct{}{}
		out[i] = disk
	}
	return out, nil
}

func (p *parser) parseDiskInventory(v any) (DiskInventory, error) {
	o, err := p.object(v)
	if err != nil {
		return DiskInventory{}, err
	}
	if err := p.keys(o, []string{"id", "name", "model", "rotational", "sizeBytes"}, nil); err != nil {
		return DiskInventory{}, err
	}
	var out DiskInventory
	if out.ID, err = p.child("id").resourceID(o["id"]); err != nil {
		return DiskInventory{}, err
	}
	if out.Name, err = p.child("name").text(o["name"], 1, 64); err != nil {
		return DiskInventory{}, err
	}
	if out.Model, err = p.child("model").nullableText(o["model"], 0, MaxTextLength); err != nil {
		return DiskInventory{}, err
	}
	if out.Rotational, err = p.child("rotational").nullableBool(o["rotational"]); err != nil {
		return DiskInventory{}, err
	}
	if out.SizeBytes, err = p.child("sizeBytes").nullableInteger(o["sizeBytes"], 0, MaxSafeInteger); err != nil {
		return DiskInventory{}, err
	}
	return out, nil
}

func (p *parser) parseInterfaceInventories(v any) ([]InterfaceInventory, error) {
	raw, err := p.array(v, 0, MaxInterfaces)
	if err != nil {
		return nil, err
	}
	out := make([]InterfaceInventory, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for i, item := range raw {
		iface, err := p.at(i).parseInterfaceInventory(item)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[iface.ID]; duplicate {
			return nil, p.errf("duplicate resource id %s", describe(iface.ID))
		}
		seen[iface.ID] = struct{}{}
		out[i] = iface
	}
	return out, nil
}

func (p *parser) parseInterfaceInventory(v any) (InterfaceInventory, error) {
	o, err := p.object(v)
	if err != nil {
		return InterfaceInventory{}, err
	}
	if err := p.keys(o, []string{"id", "name", "mac", "addresses", "virtual", "state", "speedMbps"}, nil); err != nil {
		return InterfaceInventory{}, err
	}
	var out InterfaceInventory
	if out.ID, err = p.child("id").resourceID(o["id"]); err != nil {
		return InterfaceInventory{}, err
	}
	if out.Name, err = p.child("name").text(o["name"], 1, 64); err != nil {
		return InterfaceInventory{}, err
	}
	if out.Mac, err = p.child("mac").nullableText(o["mac"], 1, 64); err != nil {
		return InterfaceInventory{}, err
	}
	rawAddresses, err := p.child("addresses").array(o["addresses"], 0, MaxAddressesPerInterface)
	if err != nil {
		return InterfaceInventory{}, err
	}
	out.Addresses = make([]InterfaceAddress, len(rawAddresses))
	for i, item := range rawAddresses {
		address, err := p.child("addresses").at(i).parseInterfaceAddress(item)
		if err != nil {
			return InterfaceInventory{}, err
		}
		out.Addresses[i] = address
	}
	if out.Virtual, err = p.child("virtual").boolean(o["virtual"]); err != nil {
		return InterfaceInventory{}, err
	}
	state, err := p.child("state").oneOf(
		o["state"],
		string(InterfaceStateUp),
		string(InterfaceStateDown),
		string(InterfaceStateUnknown),
	)
	if err != nil {
		return InterfaceInventory{}, err
	}
	out.State = InterfaceState(state)
	if out.SpeedMbps, err = p.child("speedMbps").nullableInteger(o["speedMbps"], 0, 10_000_000); err != nil {
		return InterfaceInventory{}, err
	}
	return out, nil
}

func (p *parser) parseInterfaceAddress(v any) (InterfaceAddress, error) {
	o, err := p.object(v)
	if err != nil {
		return InterfaceAddress{}, err
	}
	if err := p.keys(o, []string{"family", "address", "prefix"}, nil); err != nil {
		return InterfaceAddress{}, err
	}
	family, err := p.child("family").oneOf(
		o["family"],
		string(AddressFamilyIPv4),
		string(AddressFamilyIPv6),
	)
	if err != nil {
		return InterfaceAddress{}, err
	}
	var out InterfaceAddress
	out.Family = AddressFamily(family)
	if out.Address, err = p.child("address").text(o["address"], 2, 64); err != nil {
		return InterfaceAddress{}, err
	}
	maxPrefix := int64(128)
	if out.Family == AddressFamilyIPv4 {
		maxPrefix = 32
	}
	if out.Prefix, err = p.child("prefix").integer(o["prefix"], 0, maxPrefix); err != nil {
		return InterfaceAddress{}, err
	}
	return out, nil
}
