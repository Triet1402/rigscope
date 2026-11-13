package protocol

// familyStatus checks the shared status discriminator of a metric family.
// ok is true for the available variant; otherwise the unavailable reading is
// returned after validating its key set.
func (p *parser) familyStatus(o map[string]any) (ok bool, unavailable Unavailable, err error) {
	status, err := p.child("status").oneOf(o["status"], string(MetricStatusOK), string(MetricStatusUnavailable))
	if err != nil {
		return false, Unavailable{}, err
	}
	if status == string(MetricStatusUnavailable) {
		if err := p.keys(o, []string{"status", "reason"}, nil); err != nil {
			return false, Unavailable{}, err
		}
		reason, err := p.child("reason").text(o["reason"], 0, MaxTextLength)
		if err != nil {
			return false, Unavailable{}, err
		}
		return false, Unavailable{Reason: reason}, nil
	}
	return true, Unavailable{}, nil
}

// resourceStatus checks the shared id + status discriminator of a resource
// reading. The id is parsed before the status, matching the TypeScript parser.
func (p *parser) resourceStatus(o map[string]any) (id string, ok bool, unavailable ResourceUnavailable, err error) {
	id, err = p.child("id").resourceID(o["id"])
	if err != nil {
		return "", false, ResourceUnavailable{}, err
	}
	status, err := p.child("status").oneOf(o["status"], string(MetricStatusOK), string(MetricStatusUnavailable))
	if err != nil {
		return "", false, ResourceUnavailable{}, err
	}
	if status == string(MetricStatusUnavailable) {
		if err := p.keys(o, []string{"id", "status", "reason"}, nil); err != nil {
			return "", false, ResourceUnavailable{}, err
		}
		reason, err := p.child("reason").text(o["reason"], 0, MaxTextLength)
		if err != nil {
			return "", false, ResourceUnavailable{}, err
		}
		return id, false, ResourceUnavailable{ID: id, Reason: reason}, nil
	}
	return id, true, ResourceUnavailable{}, nil
}

func (p *parser) parseMetricsPayload(v any) (MetricsPayload, error) {
	o, err := p.object(v)
	if err != nil {
		return MetricsPayload{}, err
	}
	if err := p.keys(o, []string{"cpu", "memory", "swap", "gpus", "disks", "network"}, nil); err != nil {
		return MetricsPayload{}, err
	}
	var out MetricsPayload
	if out.CPU, err = p.child("cpu").parseCpuMetric(o["cpu"]); err != nil {
		return MetricsPayload{}, err
	}
	if out.Memory, err = p.child("memory").parseMemoryMetric(o["memory"]); err != nil {
		return MetricsPayload{}, err
	}
	if out.Swap, err = p.child("swap").parseSwapMetric(o["swap"]); err != nil {
		return MetricsPayload{}, err
	}
	if out.GPUs, err = p.child("gpus").parseGpusMetric(o["gpus"]); err != nil {
		return MetricsPayload{}, err
	}
	if out.Disks, err = p.child("disks").parseDisksMetric(o["disks"]); err != nil {
		return MetricsPayload{}, err
	}
	if out.Network, err = p.child("network").parseNetworkMetric(o["network"]); err != nil {
		return MetricsPayload{}, err
	}
	return out, nil
}

func (p *parser) parseCpuMetric(v any) (CpuMetric, error) {
	o, err := p.object(v)
	if err != nil {
		return nil, err
	}
	ok, unavailable, err := p.familyStatus(o)
	if err != nil {
		return nil, err
	}
	if !ok {
		return unavailable, nil
	}
	if err := p.keys(
		o,
		[]string{"status", "usagePercent", "perCore"},
		[]string{"temperatureC", "powerWatts", "iowaitPercent", "pressureSomePercent"},
	); err != nil {
		return nil, err
	}
	usage, err := p.child("usagePercent").number(o["usagePercent"], 0, 100)
	if err != nil {
		return nil, err
	}
	rawCores, err := p.child("perCore").array(o["perCore"], 0, MaxCores)
	if err != nil {
		return nil, err
	}
	perCore := make([]*float64, len(rawCores))
	for i, raw := range rawCores {
		core, err := p.child("perCore").at(i).nullableNumber(raw, 0, 100)
		if err != nil {
			return nil, err
		}
		perCore[i] = core
	}
	out := CpuAvailable{Status: MetricStatusOK, UsagePercent: usage, PerCore: perCore}
	if out.TemperatureC, err = optionalNumber(p, o, "temperatureC", -50, 200); err != nil {
		return nil, err
	}
	if out.PowerWatts, err = optionalNumber(p, o, "powerWatts", 0, 10_000); err != nil {
		return nil, err
	}
	if out.IowaitPercent, err = optionalNumber(p, o, "iowaitPercent", 0, 100); err != nil {
		return nil, err
	}
	if out.PressureSomePercent, err = optionalNumber(p, o, "pressureSomePercent", 0, 100); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *parser) parseMemoryMetric(v any) (MemoryMetric, error) {
	o, err := p.object(v)
	if err != nil {
		return nil, err
	}
	ok, unavailable, err := p.familyStatus(o)
	if err != nil {
		return nil, err
	}
	if !ok {
		return unavailable, nil
	}
	if err := p.keys(
		o,
		[]string{"status", "usedBytes", "totalBytes"},
		[]string{"pressureSomePercent", "pressureFullPercent"},
	); err != nil {
		return nil, err
	}
	total, err := p.child("totalBytes").integer(o["totalBytes"], 0, MaxSafeInteger)
	if err != nil {
		return nil, err
	}
	used, err := p.child("usedBytes").integer(o["usedBytes"], 0, MaxSafeInteger)
	if err != nil {
		return nil, err
	}
	if used > total {
		return nil, p.errf("usedBytes exceeds totalBytes")
	}
	out := MemoryAvailable{Status: MetricStatusOK, UsedBytes: used, TotalBytes: total}
	if out.PressureSomePercent, err = optionalNumber(p, o, "pressureSomePercent", 0, 100); err != nil {
		return nil, err
	}
	if out.PressureFullPercent, err = optionalNumber(p, o, "pressureFullPercent", 0, 100); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *parser) parseSwapMetric(v any) (BytesUsageMetric, error) {
	o, err := p.object(v)
	if err != nil {
		return nil, err
	}
	ok, unavailable, err := p.familyStatus(o)
	if err != nil {
		return nil, err
	}
	if !ok {
		return unavailable, nil
	}
	if err := p.keys(o, []string{"status", "usedBytes", "totalBytes"}, nil); err != nil {
		return nil, err
	}
	total, err := p.child("totalBytes").integer(o["totalBytes"], 0, MaxSafeInteger)
	if err != nil {
		return nil, err
	}
	used, err := p.child("usedBytes").integer(o["usedBytes"], 0, MaxSafeInteger)
	if err != nil {
		return nil, err
	}
	if used > total {
		return nil, p.errf("usedBytes exceeds totalBytes")
	}
	return BytesUsageAvailable{Status: MetricStatusOK, UsedBytes: used, TotalBytes: total}, nil
}

func (p *parser) parseGpusMetric(v any) (GpusMetric, error) {
	o, err := p.object(v)
	if err != nil {
		return nil, err
	}
	ok, unavailable, err := p.familyStatus(o)
	if err != nil {
		return nil, err
	}
	if !ok {
		return unavailable, nil
	}
	if err := p.keys(o, []string{"status", "devices"}, nil); err != nil {
		return nil, err
	}
	raw, err := p.child("devices").array(o["devices"], 0, MaxGPUs)
	if err != nil {
		return nil, err
	}
	devices := make([]GpuReading, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for i, item := range raw {
		device, err := p.child("devices").at(i).parseGpuReading(item)
		if err != nil {
			return nil, err
		}
		id := gpuReadingID(device)
		if _, duplicate := seen[id]; duplicate {
			return nil, p.errf("duplicate resource id %s", describe(id))
		}
		seen[id] = struct{}{}
		devices[i] = device
	}
	return GpusAvailable{Status: MetricStatusOK, Devices: devices}, nil
}

func gpuReadingID(r GpuReading) string {
	switch v := r.(type) {
	case GpuAvailable:
		return v.ID
	case ResourceUnavailable:
		return v.ID
	default:
		return ""
	}
}

func filesystemReadingID(r FilesystemReading) string {
	switch v := r.(type) {
	case FilesystemAvailable:
		return v.ID
	case ResourceUnavailable:
		return v.ID
	default:
		return ""
	}
}

func diskIoReadingID(r DiskIoReading) string {
	switch v := r.(type) {
	case DiskIoAvailable:
		return v.ID
	case ResourceUnavailable:
		return v.ID
	default:
		return ""
	}
}

func interfaceReadingID(r InterfaceReading) string {
	switch v := r.(type) {
	case InterfaceAvailable:
		return v.ID
	case ResourceUnavailable:
		return v.ID
	default:
		return ""
	}
}

func (p *parser) parseGpuReading(v any) (GpuReading, error) {
	o, err := p.object(v)
	if err != nil {
		return nil, err
	}
	id, ok, unavailable, err := p.resourceStatus(o)
	if err != nil {
		return nil, err
	}
	if !ok {
		return unavailable, nil
	}
	if err := p.keys(
		o,
		[]string{"id", "status", "utilizationPercent", "memoryUsedBytes", "memoryTotalBytes", "temperatureC", "powerWatts"},
		[]string{"junctionTemperatureC", "vramTemperatureC"},
	); err != nil {
		return nil, err
	}
	used, err := p.child("memoryUsedBytes").nullableInteger(o["memoryUsedBytes"], 0, MaxSafeInteger)
	if err != nil {
		return nil, err
	}
	total, err := p.child("memoryTotalBytes").nullableInteger(o["memoryTotalBytes"], 0, MaxSafeInteger)
	if err != nil {
		return nil, err
	}
	if used != nil && total != nil && *used > *total {
		return nil, p.errf("memoryUsedBytes exceeds totalBytes")
	}
	out := GpuAvailable{Status: MetricStatusOK, ID: id}
	if out.UtilizationPercent, err = p.child("utilizationPercent").nullableNumber(o["utilizationPercent"], 0, 100); err != nil {
		return nil, err
	}
	out.MemoryUsedBytes = used
	out.MemoryTotalBytes = total
	if out.TemperatureC, err = p.child("temperatureC").nullableNumber(o["temperatureC"], -50, 200); err != nil {
		return nil, err
	}
	if out.PowerWatts, err = p.child("powerWatts").nullableNumber(o["powerWatts"], 0, 10_000); err != nil {
		return nil, err
	}
	if out.JunctionTemperatureC, err = optionalNumber(p, o, "junctionTemperatureC", -50, 200); err != nil {
		return nil, err
	}
	if out.VramTemperatureC, err = optionalNumber(p, o, "vramTemperatureC", -50, 200); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *parser) parseDisksMetric(v any) (DisksMetric, error) {
	o, err := p.object(v)
	if err != nil {
		return nil, err
	}
	ok, unavailable, err := p.familyStatus(o)
	if err != nil {
		return nil, err
	}
	if !ok {
		return unavailable, nil
	}
	if err := p.keys(
		o,
		[]string{"status", "filesystems", "devices"},
		[]string{"pressureSomePercent", "pressureFullPercent"},
	); err != nil {
		return nil, err
	}
	rawFilesystems, err := p.child("filesystems").array(o["filesystems"], 0, MaxFilesystems)
	if err != nil {
		return nil, err
	}
	filesystems := make([]FilesystemReading, len(rawFilesystems))
	seenFilesystems := make(map[string]struct{}, len(rawFilesystems))
	for i, item := range rawFilesystems {
		reading, err := p.child("filesystems").at(i).parseFilesystemReading(item)
		if err != nil {
			return nil, err
		}
		id := filesystemReadingID(reading)
		if _, duplicate := seenFilesystems[id]; duplicate {
			return nil, p.errf("duplicate resource id %s", describe(id))
		}
		seenFilesystems[id] = struct{}{}
		filesystems[i] = reading
	}
	rawDevices, err := p.child("devices").array(o["devices"], 0, MaxDisks)
	if err != nil {
		return nil, err
	}
	devices := make([]DiskIoReading, len(rawDevices))
	seenDevices := make(map[string]struct{}, len(rawDevices))
	for i, item := range rawDevices {
		reading, err := p.child("devices").at(i).parseDiskIoReading(item)
		if err != nil {
			return nil, err
		}
		id := diskIoReadingID(reading)
		if _, duplicate := seenDevices[id]; duplicate {
			return nil, p.errf("duplicate resource id %s", describe(id))
		}
		seenDevices[id] = struct{}{}
		devices[i] = reading
	}
	out := DisksAvailable{Status: MetricStatusOK, Filesystems: filesystems, Devices: devices}
	if out.PressureSomePercent, err = optionalNumber(p, o, "pressureSomePercent", 0, 100); err != nil {
		return nil, err
	}
	if out.PressureFullPercent, err = optionalNumber(p, o, "pressureFullPercent", 0, 100); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *parser) parseFilesystemReading(v any) (FilesystemReading, error) {
	o, err := p.object(v)
	if err != nil {
		return nil, err
	}
	id, ok, unavailable, err := p.resourceStatus(o)
	if err != nil {
		return nil, err
	}
	if !ok {
		return unavailable, nil
	}
	if err := p.keys(
		o,
		[]string{"id", "status", "usedBytes", "totalBytes"},
		[]string{"inodesUsed", "inodesTotal"},
	); err != nil {
		return nil, err
	}
	total, err := p.child("totalBytes").integer(o["totalBytes"], 0, MaxSafeInteger)
	if err != nil {
		return nil, err
	}
	used, err := p.child("usedBytes").integer(o["usedBytes"], 0, MaxSafeInteger)
	if err != nil {
		return nil, err
	}
	if used > total {
		return nil, p.errf("usedBytes exceeds totalBytes")
	}
	out := FilesystemAvailable{Status: MetricStatusOK, ID: id, UsedBytes: used, TotalBytes: total}
	if out.InodesUsed, err = optionalInteger(p, o, "inodesUsed", 0, MaxSafeInteger); err != nil {
		return nil, err
	}
	if out.InodesTotal, err = optionalInteger(p, o, "inodesTotal", 0, MaxSafeInteger); err != nil {
		return nil, err
	}
	if out.InodesUsed != nil && out.InodesTotal != nil && out.InodesUsed.Valid && out.InodesTotal.Valid &&
		out.InodesUsed.Value > out.InodesTotal.Value {
		return nil, p.errf("inodesUsed exceeds inodesTotal")
	}
	return out, nil
}

func (p *parser) parseDiskIoReading(v any) (DiskIoReading, error) {
	o, err := p.object(v)
	if err != nil {
		return nil, err
	}
	id, ok, unavailable, err := p.resourceStatus(o)
	if err != nil {
		return nil, err
	}
	if !ok {
		return unavailable, nil
	}
	if err := p.keys(
		o,
		[]string{"id", "status", "readBytesPerSec", "writeBytesPerSec"},
		[]string{"readLatencyMs", "writeLatencyMs", "ioUtilizationPercent"},
	); err != nil {
		return nil, err
	}
	read, err := p.child("readBytesPerSec").number(o["readBytesPerSec"], 0, 1e13)
	if err != nil {
		return nil, err
	}
	write, err := p.child("writeBytesPerSec").number(o["writeBytesPerSec"], 0, 1e13)
	if err != nil {
		return nil, err
	}
	out := DiskIoAvailable{Status: MetricStatusOK, ID: id, ReadBytesPerSec: read, WriteBytesPerSec: write}
	if out.ReadLatencyMs, err = optionalNumber(p, o, "readLatencyMs", 0, 86_400_000); err != nil {
		return nil, err
	}
	if out.WriteLatencyMs, err = optionalNumber(p, o, "writeLatencyMs", 0, 86_400_000); err != nil {
		return nil, err
	}
	if out.IoUtilizationPercent, err = optionalNumber(p, o, "ioUtilizationPercent", 0, 100); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *parser) parseNetworkMetric(v any) (NetworkMetric, error) {
	o, err := p.object(v)
	if err != nil {
		return nil, err
	}
	ok, unavailable, err := p.familyStatus(o)
	if err != nil {
		return nil, err
	}
	if !ok {
		return unavailable, nil
	}
	if err := p.keys(o, []string{"status", "interfaces"}, nil); err != nil {
		return nil, err
	}
	raw, err := p.child("interfaces").array(o["interfaces"], 0, MaxInterfaces)
	if err != nil {
		return nil, err
	}
	interfaces := make([]InterfaceReading, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for i, item := range raw {
		reading, err := p.child("interfaces").at(i).parseInterfaceReading(item)
		if err != nil {
			return nil, err
		}
		id := interfaceReadingID(reading)
		if _, duplicate := seen[id]; duplicate {
			return nil, p.errf("duplicate resource id %s", describe(id))
		}
		seen[id] = struct{}{}
		interfaces[i] = reading
	}
	return NetworkAvailable{Status: MetricStatusOK, Interfaces: interfaces}, nil
}

func (p *parser) parseInterfaceReading(v any) (InterfaceReading, error) {
	o, err := p.object(v)
	if err != nil {
		return nil, err
	}
	id, ok, unavailable, err := p.resourceStatus(o)
	if err != nil {
		return nil, err
	}
	if !ok {
		return unavailable, nil
	}
	if err := p.keys(o, []string{"id", "status", "rxBytesPerSec", "txBytesPerSec"}, nil); err != nil {
		return nil, err
	}
	rx, err := p.child("rxBytesPerSec").number(o["rxBytesPerSec"], 0, 1e13)
	if err != nil {
		return nil, err
	}
	tx, err := p.child("txBytesPerSec").number(o["txBytesPerSec"], 0, 1e13)
	if err != nil {
		return nil, err
	}
	return InterfaceAvailable{Status: MetricStatusOK, ID: id, RxBytesPerSec: rx, TxBytesPerSec: tx}, nil
}
