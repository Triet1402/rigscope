package storage

import (
	"math"
	"sort"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// MetricUnit is the display unit stored next to a series in query results.
type MetricUnit string

// Units used by the hub's metric catalog.
const (
	UnitPercent        MetricUnit = "percent"
	UnitBytes          MetricUnit = "bytes"
	UnitBytesPerSecond MetricUnit = "bytes_per_second"
	UnitCelsius        MetricUnit = "celsius"
	UnitWatts          MetricUnit = "watts"
	UnitMilliseconds   MetricUnit = "milliseconds"
)

// metricCatalog is the single mapping from wire metric names to display units.
// Unknown metric names are omitted from storage queries, so a future collector
// metric cannot break an older hub's history.
var metricCatalog = map[string]MetricUnit{
	"cpu.usage":                UnitPercent,
	"cpu.core":                 UnitPercent,
	"cpu.temperature":          UnitCelsius,
	"cpu.power":                UnitWatts,
	"cpu.iowait":               UnitPercent,
	"cpu.pressure":             UnitPercent,
	"memory.used":              UnitBytes,
	"memory.total":             UnitBytes,
	"memory.usage":             UnitPercent,
	"memory.pressure.some":     UnitPercent,
	"memory.pressure.full":     UnitPercent,
	"swap.used":                UnitBytes,
	"swap.total":               UnitBytes,
	"gpu.utilization":          UnitPercent,
	"gpu.memory.used":          UnitBytes,
	"gpu.memory.total":         UnitBytes,
	"gpu.temperature":          UnitCelsius,
	"gpu.junction.temperature": UnitCelsius,
	"gpu.vram.temperature":     UnitCelsius,
	"gpu.power":                UnitWatts,
	"fs.used":                  UnitBytes,
	"fs.total":                 UnitBytes,
	"fs.usage":                 UnitPercent,
	"fs.inodes.usage":          UnitPercent,
	"io.pressure.some":         UnitPercent,
	"io.pressure.full":         UnitPercent,
	"disk.read":                UnitBytesPerSecond,
	"disk.write":               UnitBytesPerSecond,
	"disk.read.latency":        UnitMilliseconds,
	"disk.write.latency":       UnitMilliseconds,
	"disk.utilization":         UnitPercent,
	"net.rx":                   UnitBytesPerSecond,
	"net.tx":                   UnitBytesPerSecond,
}

// MetricNames returns the catalog's metric names in stable order.
func MetricNames() []string {
	names := make([]string, 0, len(metricCatalog))
	for name := range metricCatalog {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MetricUnitFor reports the display unit for a metric.
func MetricUnitFor(name string) (MetricUnit, bool) {
	unit, ok := metricCatalog[name]
	return unit, ok
}

// FlatPoint is one stored series reading. A nil Value is an explicit
// "unavailable" reading, never a fabricated zero.
type FlatPoint struct {
	Metric     string
	ResourceID string
	Value      *float64
}

// KnownSeries is one series already present in a machine's catalog. It is used
// only to turn an unavailable multi-resource family into an explicit NULL
// break for lines that existed before the failure.
type KnownSeries struct {
	Metric     string
	ResourceID string
}

var (
	cpuCoreMetrics = map[string]struct{}{
		"cpu.core": {},
	}
	cpuOptionalMetrics = map[string]struct{}{
		"cpu.temperature": {},
		"cpu.power":       {},
		"cpu.iowait":      {},
		"cpu.pressure":    {},
	}
	memoryPressureMetrics = map[string]struct{}{
		"memory.pressure.some": {},
		"memory.pressure.full": {},
	}
	gpuMetrics = map[string]struct{}{
		"gpu.utilization":          {},
		"gpu.memory.used":          {},
		"gpu.memory.total":         {},
		"gpu.temperature":          {},
		"gpu.junction.temperature": {},
		"gpu.vram.temperature":     {},
		"gpu.power":                {},
	}
	diskFamilyMetrics = map[string]struct{}{
		"fs.used":            {},
		"fs.total":           {},
		"fs.usage":           {},
		"fs.inodes.usage":    {},
		"io.pressure.some":   {},
		"io.pressure.full":   {},
		"disk.read":          {},
		"disk.write":         {},
		"disk.read.latency":  {},
		"disk.write.latency": {},
		"disk.utilization":   {},
	}
	networkMetrics = map[string]struct{}{
		"net.rx": {},
		"net.tx": {},
	}
)

// NeedsKnownSeries reports whether flattening this payload consults the known
// catalog to emit NULL breaks. The common path avoids that allocation.
func NeedsKnownSeries(payload protocol.MetricsPayload) bool {
	_, cpuDown := payload.CPU.(protocol.Unavailable)
	_, gpusDown := payload.GPUs.(protocol.Unavailable)
	_, disksDown := payload.Disks.(protocol.Unavailable)
	_, networkDown := payload.Network.(protocol.Unavailable)
	if cpuDown || gpusDown || disksDown || networkDown {
		return true
	}
	// An optional field can disappear while its family stays available (a GPU
	// sensor that stops reporting, PSI pressure that becomes unsupported). The
	// known catalog is needed to emit a NULL break for the series that existed
	// before instead of silently ending the line.
	switch gpus := payload.GPUs.(type) {
	case protocol.GpusAvailable:
		if gpusNeedKnown(gpus) {
			return true
		}
	case *protocol.GpusAvailable:
		if gpus != nil && gpusNeedKnown(*gpus) {
			return true
		}
	}
	switch disks := payload.Disks.(type) {
	case protocol.DisksAvailable:
		if disks.PressureSomePercent == nil || disks.PressureFullPercent == nil {
			return true
		}
	case *protocol.DisksAvailable:
		if disks != nil && (disks.PressureSomePercent == nil || disks.PressureFullPercent == nil) {
			return true
		}
	}
	return false
}

func gpusNeedKnown(gpus protocol.GpusAvailable) bool {
	for _, device := range gpus.Devices {
		if gpu, ok := device.(protocol.GpuAvailable); ok {
			if gpu.JunctionTemperatureC == nil || gpu.VramTemperatureC == nil {
				return true
			}
		}
	}
	return false
}

// Flatten maps a validated metrics payload onto the flat (metric, resource)
// series space.
//
// Unavailable semantics (never a successful zero):
//   - an unavailable single-resource family means NULL readings of its primary
//     metrics (cpu.usage / memory.used + memory.usage / swap.used);
//   - an unavailable per-resource reading means a NULL primary metric for that
//     resource (gpu.utilization + gpu.memory.used, fs.used, disk.read +
//     disk.write, net.rx + net.tx);
//   - an unavailable multi-resource family produces NULLs only for series the
//     machine already had; on a fresh machine it produces no points and the
//     missing interval is represented by receipts/gap records;
//   - a per-field null (for example a GPU temperature) is a NULL point.
func Flatten(payload protocol.MetricsPayload, known []KnownSeries) []FlatPoint {
	points := make([]FlatPoint, 0, 64)
	push := func(metric, resourceID string, value *float64) {
		points = append(points, FlatPoint{Metric: metric, ResourceID: resourceID, Value: value})
	}
	pushValue := func(metric, resourceID string, value float64) {
		v := value
		push(metric, resourceID, &v)
	}
	pushOptional := func(metric, resourceID string, value *protocol.Optional[float64]) {
		// An absent or invalid optional breaks the line for a series the
		// machine already had. A never-seen optional creates no series; the
		// missing interval is represented by receipts/gap records instead.
		if value == nil || !value.Valid {
			if knownSeriesContains(known, metric, resourceID) {
				push(metric, resourceID, nil)
			}
			return
		}
		v := value.Value
		push(metric, resourceID, &v)
	}
	pushKnownNulls := func(metrics map[string]struct{}) {
		if known == nil {
			return
		}
		for _, item := range known {
			if _, ok := metrics[item.Metric]; !ok {
				continue
			}
			if _, ok := metricCatalog[item.Metric]; !ok {
				continue
			}
			push(item.Metric, item.ResourceID, nil)
		}
	}

	switch cpu := payload.CPU.(type) {
	case protocol.CpuAvailable:
		pushValue("cpu.usage", "", cpu.UsagePercent)
		for index, value := range cpu.PerCore {
			resourceID := coreResourceID(index)
			if value == nil {
				push("cpu.core", resourceID, nil)
			} else {
				pushValue("cpu.core", resourceID, *value)
			}
		}
		pushScalarOptional("cpu.temperature", cpu.TemperatureC, push)
		pushScalarOptional("cpu.power", cpu.PowerWatts, push)
		pushScalarOptional("cpu.iowait", cpu.IowaitPercent, push)
		pushScalarOptional("cpu.pressure", cpu.PressureSomePercent, push)
	case *protocol.CpuAvailable:
		if cpu != nil {
			return Flatten(withCPU(payload, *cpu), known)
		}
		push("cpu.usage", "", nil)
		pushKnownNulls(cpuCoreMetrics)
		pushKnownNulls(cpuOptionalMetrics)
	default:
		push("cpu.usage", "", nil)
		pushKnownNulls(cpuCoreMetrics)
		pushKnownNulls(cpuOptionalMetrics)
	}

	switch memory := payload.Memory.(type) {
	case protocol.MemoryAvailable:
		pushValue("memory.used", "", float64(memory.UsedBytes))
		pushValue("memory.total", "", float64(memory.TotalBytes))
		if usage := percentOf(float64(memory.UsedBytes), float64(memory.TotalBytes)); usage != nil {
			push("memory.usage", "", usage)
		} else {
			push("memory.usage", "", nil)
		}
		pushScalarOptional("memory.pressure.some", memory.PressureSomePercent, push)
		pushScalarOptional("memory.pressure.full", memory.PressureFullPercent, push)
	case *protocol.MemoryAvailable:
		if memory != nil {
			return Flatten(withMemory(payload, *memory), known)
		}
		push("memory.used", "", nil)
		push("memory.usage", "", nil)
		pushKnownNulls(memoryPressureMetrics)
	default:
		push("memory.used", "", nil)
		push("memory.usage", "", nil)
		pushKnownNulls(memoryPressureMetrics)
	}

	switch swap := payload.Swap.(type) {
	case protocol.BytesUsageAvailable:
		pushValue("swap.used", "", float64(swap.UsedBytes))
		pushValue("swap.total", "", float64(swap.TotalBytes))
	case *protocol.BytesUsageAvailable:
		if swap != nil {
			return Flatten(withSwap(payload, *swap), known)
		}
		push("swap.used", "", nil)
	default:
		push("swap.used", "", nil)
	}

	switch gpus := payload.GPUs.(type) {
	case protocol.GpusAvailable:
		for _, device := range gpus.Devices {
			switch gpu := device.(type) {
			case protocol.ResourceUnavailable:
				push("gpu.utilization", gpu.ID, nil)
				push("gpu.memory.used", gpu.ID, nil)
				pushOptional("gpu.junction.temperature", gpu.ID, nil)
				pushOptional("gpu.vram.temperature", gpu.ID, nil)
			case protocol.GpuAvailable:
				push("gpu.utilization", gpu.ID, gpu.UtilizationPercent)
				push("gpu.memory.used", gpu.ID, int64PointerToFloat(gpu.MemoryUsedBytes))
				push("gpu.memory.total", gpu.ID, int64PointerToFloat(gpu.MemoryTotalBytes))
				push("gpu.temperature", gpu.ID, gpu.TemperatureC)
				push("gpu.power", gpu.ID, gpu.PowerWatts)
				pushOptional("gpu.junction.temperature", gpu.ID, gpu.JunctionTemperatureC)
				pushOptional("gpu.vram.temperature", gpu.ID, gpu.VramTemperatureC)
			}
		}
	case *protocol.GpusAvailable:
		if gpus != nil {
			return Flatten(withGPUs(payload, *gpus), known)
		}
		pushKnownNulls(gpuMetrics)
	default:
		pushKnownNulls(gpuMetrics)
	}

	switch disks := payload.Disks.(type) {
	case protocol.DisksAvailable:
		pushOptional("io.pressure.some", "", disks.PressureSomePercent)
		pushOptional("io.pressure.full", "", disks.PressureFullPercent)
		for _, reading := range disks.Filesystems {
			switch fs := reading.(type) {
			case protocol.ResourceUnavailable:
				push("fs.used", fs.ID, nil)
				push("fs.usage", fs.ID, nil)
				push("fs.inodes.usage", fs.ID, nil)
			case protocol.FilesystemAvailable:
				pushValue("fs.used", fs.ID, float64(fs.UsedBytes))
				pushValue("fs.total", fs.ID, float64(fs.TotalBytes))
				push("fs.usage", fs.ID, percentOf(float64(fs.UsedBytes), float64(fs.TotalBytes)))
				if fs.InodesTotal == nil {
					pushOptional("fs.inodes.usage", fs.ID, nil)
				} else if !fs.InodesTotal.Valid {
					push("fs.inodes.usage", fs.ID, nil)
				} else if fs.InodesUsed == nil || !fs.InodesUsed.Valid {
					push("fs.inodes.usage", fs.ID, nil)
				} else {
					push("fs.inodes.usage", fs.ID, percentOf(float64(fs.InodesUsed.Value), float64(fs.InodesTotal.Value)))
				}
			}
		}
		for _, reading := range disks.Devices {
			switch disk := reading.(type) {
			case protocol.ResourceUnavailable:
				push("disk.read", disk.ID, nil)
				push("disk.write", disk.ID, nil)
				push("disk.read.latency", disk.ID, nil)
				push("disk.write.latency", disk.ID, nil)
				push("disk.utilization", disk.ID, nil)
			case protocol.DiskIoAvailable:
				pushValue("disk.read", disk.ID, disk.ReadBytesPerSec)
				pushValue("disk.write", disk.ID, disk.WriteBytesPerSec)
				push("disk.read.latency", disk.ID, optionalToPointer(disk.ReadLatencyMs))
				push("disk.write.latency", disk.ID, optionalToPointer(disk.WriteLatencyMs))
				push("disk.utilization", disk.ID, optionalToPointer(disk.IoUtilizationPercent))
			}
		}
	case *protocol.DisksAvailable:
		if disks != nil {
			return Flatten(withDisks(payload, *disks), known)
		}
		pushKnownNulls(diskFamilyMetrics)
	default:
		pushKnownNulls(diskFamilyMetrics)
	}

	switch network := payload.Network.(type) {
	case protocol.NetworkAvailable:
		for _, reading := range network.Interfaces {
			switch iface := reading.(type) {
			case protocol.ResourceUnavailable:
				push("net.rx", iface.ID, nil)
				push("net.tx", iface.ID, nil)
			case protocol.InterfaceAvailable:
				pushValue("net.rx", iface.ID, iface.RxBytesPerSec)
				pushValue("net.tx", iface.ID, iface.TxBytesPerSec)
			}
		}
	case *protocol.NetworkAvailable:
		if network != nil {
			return Flatten(withNetwork(payload, *network), known)
		}
		pushKnownNulls(networkMetrics)
	default:
		pushKnownNulls(networkMetrics)
	}

	return points
}

// Flatten only needs the concrete available variants; these helpers replace a
// pointer variant with its value so the one implementation handles both.

func withCPU(payload protocol.MetricsPayload, cpu protocol.CpuAvailable) protocol.MetricsPayload {
	payload.CPU = cpu
	return payload
}

func withMemory(payload protocol.MetricsPayload, memory protocol.MemoryAvailable) protocol.MetricsPayload {
	payload.Memory = memory
	return payload
}

func withSwap(payload protocol.MetricsPayload, swap protocol.BytesUsageAvailable) protocol.MetricsPayload {
	payload.Swap = swap
	return payload
}

func withGPUs(payload protocol.MetricsPayload, gpus protocol.GpusAvailable) protocol.MetricsPayload {
	payload.GPUs = gpus
	return payload
}

func withDisks(payload protocol.MetricsPayload, disks protocol.DisksAvailable) protocol.MetricsPayload {
	payload.Disks = disks
	return payload
}

func withNetwork(payload protocol.MetricsPayload, network protocol.NetworkAvailable) protocol.MetricsPayload {
	payload.Network = network
	return payload
}

// pushScalarOptional stores a scalar optional reading. An absent optional is an
// explicit unavailable point, matching the TypeScript store: the line breaks
// rather than silently disappearing.
func pushScalarOptional(metric string, value *protocol.Optional[float64], push func(string, string, *float64)) {
	if value == nil || !value.Valid {
		push(metric, "", nil)
		return
	}
	v := value.Value
	push(metric, "", &v)
}

func knownSeriesContains(known []KnownSeries, metric, resourceID string) bool {
	for _, item := range known {
		if item.Metric == metric && item.ResourceID == resourceID {
			return true
		}
	}
	return false
}

func coreResourceID(index int) string {
	return "core:" + itoa(index)
}

// percentOf derives a bounded percentage; a non-positive total is unknown.
func percentOf(used, total float64) *float64 {
	if math.IsNaN(used) || math.IsNaN(total) || math.IsInf(used, 0) || math.IsInf(total, 0) || total <= 0 {
		return nil
	}
	percent := used / total * 100
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	value := math.Round(percent*100) / 100
	return &value
}

func int64PointerToFloat(value *int64) *float64 {
	if value == nil {
		return nil
	}
	converted := float64(*value)
	return &converted
}

func optionalToPointer(value *protocol.Optional[float64]) *float64 {
	if value == nil || !value.Valid {
		return nil
	}
	converted := value.Value
	return &converted
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}
