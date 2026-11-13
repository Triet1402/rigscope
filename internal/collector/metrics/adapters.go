package metrics

import (
	"context"
	"errors"
	"math"

	"github.com/Triet1402/rigscope/internal/collector/processes"
	"github.com/Triet1402/rigscope/internal/collector/sampling/gpu"
	"github.com/Triet1402/rigscope/internal/collector/sampling/procfs"
	"github.com/Triet1402/rigscope/internal/protocol"
)

// ErrProcessScanPanicked reports a process scan whose implementation panicked.
var ErrProcessScanPanicked = errors.New("metrics: process scan panicked")

// RecoveringScanner is the process scan adapter: it runs a
// processes.SnapshotScanner and converts a panic into an error. The scheduler
// treats that as a failed scan and retries at its next cadence, so a broken
// scanner cannot escape into the sampling loop. The panic value is deliberately
// not included: it may carry paths or other host details.
type RecoveringScanner struct {
	Scanner processes.SnapshotScanner
}

// Scan satisfies processes.SnapshotScanner.
func (s RecoveringScanner) Scan(ctx context.Context, options processes.ScanOptions) (snapshot processes.ProcessSnapshot, err error) {
	if s.Scanner == nil {
		return processes.ProcessSnapshot{Processes: []processes.ProcessRow{}}, errors.New("metrics: process scanner is required")
	}
	defer func() {
		if recover() != nil {
			snapshot = processes.ProcessSnapshot{Processes: []processes.ProcessRow{}}
			err = ErrProcessScanPanicked
		}
	}()
	return s.Scanner.Scan(ctx, options)
}

// Protocol bounds the adapters enforce. The samplers already clamp most of
// these, but nvidia-smi output and fixture data are external input: a value
// outside the contract becomes an absent/unavailable reading instead of a
// message the strict encoder rejects or, worse, an invented zero.
const (
	maxWirePID         = int64(1 << 31)
	maxProcessTotal    = int64(10_000_000)
	maxProcessCPU      = 102_400.0
	maxProcessThreads  = int64(10_000_000)
	maxProcessCommand  = 4096
	maxProcessName     = 256
	maxProcessUser     = 64
	maxProcessState    = 16
	maxProcessUnit     = 256
	maxRateBytesPerSec = 1e13
)

// ToProtocolMetrics converts one sampler tick into the wire payload. Every
// family is present; an ok local reading whose mandatory values are missing
// becomes unavailable rather than a zero.
func ToProtocolMetrics(readings procfs.Readings, gpus gpu.GpusMetric) protocol.MetricsPayload {
	return protocol.MetricsPayload{
		CPU:     cpuMetric(readings.CPU),
		Memory:  memoryMetric(readings.Memory),
		Swap:    swapMetric(readings.Swap),
		GPUs:    gpusMetric(gpus),
		Disks:   disksMetric(readings.Disks),
		Network: networkMetric(readings.Network),
	}
}

func cpuMetric(reading procfs.CpuMetric) protocol.CpuMetric {
	if reading.Status != procfs.StatusOK || reading.UsagePercent == nil ||
		!inRange(*reading.UsagePercent, 0, 100) {
		return protocol.Unavailable{Reason: unavailableReason(reading.Reason, "cpu usage unavailable")}
	}
	perCore := make([]*float64, len(reading.PerCore))
	for index, value := range reading.PerCore {
		if value != nil && inRange(*value, 0, 100) {
			perCore[index] = value
		}
	}
	return protocol.CpuAvailable{
		UsagePercent:        *reading.UsagePercent,
		PerCore:             perCore,
		TemperatureC:        optionalFloat(reading.TemperatureC, -50, 200),
		PowerWatts:          optionalFloat(reading.PowerWatts, 0, 10_000),
		IowaitPercent:       optionalFloat(reading.IOwaitPercent, 0, 100),
		PressureSomePercent: optionalFloat(reading.PressureSomePercent, 0, 100),
	}
}

func memoryMetric(reading procfs.MemoryMetric) protocol.MemoryMetric {
	if reading.Status != procfs.StatusOK || reading.UsedBytes == nil || reading.TotalBytes == nil {
		return protocol.Unavailable{Reason: unavailableReason(reading.Reason, "memory counters unavailable")}
	}
	used, total := *reading.UsedBytes, *reading.TotalBytes
	if !inRangeInt(used, 0, protocol.MaxSafeInteger) ||
		!inRangeInt(total, 0, protocol.MaxSafeInteger) || used > total {
		return protocol.Unavailable{Reason: unavailableReason(reading.Reason, "memory counters inconsistent")}
	}
	return protocol.MemoryAvailable{
		UsedBytes:           used,
		TotalBytes:          total,
		PressureSomePercent: optionalFloat(reading.PressureSomePercent, 0, 100),
		PressureFullPercent: optionalFloat(reading.PressureFullPercent, 0, 100),
	}
}

func swapMetric(reading procfs.BytesUsageMetric) protocol.BytesUsageMetric {
	if reading.Status != procfs.StatusOK || reading.UsedBytes == nil || reading.TotalBytes == nil {
		return protocol.Unavailable{Reason: unavailableReason(reading.Reason, "swap counters unavailable")}
	}
	used, total := *reading.UsedBytes, *reading.TotalBytes
	if !inRangeInt(used, 0, protocol.MaxSafeInteger) ||
		!inRangeInt(total, 0, protocol.MaxSafeInteger) || used > total {
		return protocol.Unavailable{Reason: unavailableReason(reading.Reason, "swap counters inconsistent")}
	}
	return protocol.BytesUsageAvailable{UsedBytes: used, TotalBytes: total}
}

func gpusMetric(reading gpu.GpusMetric) protocol.GpusMetric {
	if reading.Status != gpu.StatusOK {
		return protocol.Unavailable{Reason: unavailableReason(reading.Reason, "gpu telemetry unavailable")}
	}
	devices := make([]protocol.GpuReading, 0, len(reading.Devices))
	for _, device := range reading.Devices {
		if len(devices) >= protocol.MaxGPUs {
			break
		}
		devices = append(devices, gpuReading(device))
	}
	return protocol.GpusAvailable{Devices: devices}
}

func gpuReading(reading gpu.GpuReading) protocol.GpuReading {
	id := boundedText(reading.ID, protocol.MaxResourceIDLength)
	if id == "" {
		id = "gpu:unknown"
	}
	if reading.Status != gpu.StatusOK {
		return protocol.ResourceUnavailable{
			ID:     id,
			Reason: unavailableReason(reading.Reason, "gpu telemetry unavailable"),
		}
	}
	used := boundedInt(reading.MemoryUsedBytes, 0, protocol.MaxSafeInteger)
	total := boundedInt(reading.MemoryTotalBytes, 0, protocol.MaxSafeInteger)
	if used != nil && total != nil && *used > *total {
		// A used figure above the total is not a reading; the total may still
		// be useful for the UI (it describes the card, not the sample).
		used = nil
	}
	return protocol.GpuAvailable{
		ID:                   id,
		UtilizationPercent:   boundedFloat(reading.UtilizationPercent, 0, 100),
		MemoryUsedBytes:      used,
		MemoryTotalBytes:     total,
		TemperatureC:         boundedFloat(reading.TemperatureC, -50, 200),
		PowerWatts:           boundedFloat(reading.PowerWatts, 0, 10_000),
		JunctionTemperatureC: optionalFloat(reading.JunctionTemperatureC, -50, 200),
		VramTemperatureC:     optionalFloat(reading.VRAMTemperatureC, -50, 200),
	}
}

func disksMetric(reading procfs.DisksMetric) protocol.DisksMetric {
	if reading.Status != procfs.StatusOK {
		return protocol.Unavailable{Reason: unavailableReason(reading.Reason, "disk telemetry unavailable")}
	}
	filesystems := make([]protocol.FilesystemReading, 0, len(reading.Filesystems))
	for _, filesystem := range reading.Filesystems {
		if len(filesystems) >= protocol.MaxFilesystems {
			break
		}
		filesystems = append(filesystems, filesystemReading(filesystem))
	}
	devices := make([]protocol.DiskIoReading, 0, len(reading.Devices))
	for _, device := range reading.Devices {
		if len(devices) >= protocol.MaxDisks {
			break
		}
		devices = append(devices, diskReading(device))
	}
	return protocol.DisksAvailable{
		Filesystems:         filesystems,
		Devices:             devices,
		PressureSomePercent: optionalFloat(reading.PressureSomePercent, 0, 100),
		PressureFullPercent: optionalFloat(reading.PressureFullPercent, 0, 100),
	}
}

func filesystemReading(reading procfs.FilesystemReading) protocol.FilesystemReading {
	id := boundedText(reading.ID, protocol.MaxResourceIDLength)
	if reading.Status != procfs.StatusOK || reading.UsedBytes == nil || reading.TotalBytes == nil {
		return protocol.ResourceUnavailable{
			ID:     resourceID(id, "fs:unknown"),
			Reason: unavailableReason(reading.Reason, "filesystem usage unavailable"),
		}
	}
	used, total := *reading.UsedBytes, *reading.TotalBytes
	if !inRangeInt(used, 0, protocol.MaxSafeInteger) ||
		!inRangeInt(total, 0, protocol.MaxSafeInteger) || used > total {
		return protocol.ResourceUnavailable{
			ID:     resourceID(id, "fs:unknown"),
			Reason: unavailableReason(reading.Reason, "filesystem usage inconsistent"),
		}
	}
	out := protocol.FilesystemAvailable{
		ID:         resourceID(id, "fs:unknown"),
		UsedBytes:  used,
		TotalBytes: total,
	}
	if inodesUsed, inodesTotal := reading.InodesUsed, reading.InodesTotal; inodesUsed != nil && inodesTotal != nil &&
		inRangeInt(*inodesUsed, 0, protocol.MaxSafeInteger) &&
		inRangeInt(*inodesTotal, 0, protocol.MaxSafeInteger) &&
		*inodesUsed <= *inodesTotal {
		out.InodesUsed = protocol.Some(*inodesUsed)
		out.InodesTotal = protocol.Some(*inodesTotal)
	}
	return out
}

func diskReading(reading procfs.DiskIoReading) protocol.DiskIoReading {
	id := boundedText(reading.ID, protocol.MaxResourceIDLength)
	if reading.Status != procfs.StatusOK || reading.ReadBytesPerSec == nil || reading.WriteBytesPerSec == nil {
		return protocol.ResourceUnavailable{
			ID:     resourceID(id, "disk:unknown"),
			Reason: unavailableReason(reading.Reason, "disk I/O unavailable"),
		}
	}
	read, write := float64(*reading.ReadBytesPerSec), float64(*reading.WriteBytesPerSec)
	if !inRange(read, 0, maxRateBytesPerSec) || !inRange(write, 0, maxRateBytesPerSec) {
		return protocol.ResourceUnavailable{
			ID:     resourceID(id, "disk:unknown"),
			Reason: unavailableReason(reading.Reason, "disk I/O unavailable"),
		}
	}
	latencyMin, latencyMax := 0.0, 86_400_000.0
	return protocol.DiskIoAvailable{
		ID:                   resourceID(id, "disk:unknown"),
		ReadBytesPerSec:      read,
		WriteBytesPerSec:     write,
		ReadLatencyMs:        optionalFloat(reading.ReadLatencyMs, latencyMin, latencyMax),
		WriteLatencyMs:       optionalFloat(reading.WriteLatencyMs, latencyMin, latencyMax),
		IoUtilizationPercent: optionalFloat(reading.IOUtilizationPercent, 0, 100),
	}
}

func networkMetric(reading procfs.NetworkMetric) protocol.NetworkMetric {
	if reading.Status != procfs.StatusOK {
		return protocol.Unavailable{Reason: unavailableReason(reading.Reason, "network telemetry unavailable")}
	}
	interfaces := make([]protocol.InterfaceReading, 0, len(reading.Interfaces))
	for _, iface := range reading.Interfaces {
		if len(interfaces) >= protocol.MaxInterfaces {
			break
		}
		interfaces = append(interfaces, interfaceReading(iface))
	}
	return protocol.NetworkAvailable{Interfaces: interfaces}
}

func interfaceReading(reading procfs.InterfaceReading) protocol.InterfaceReading {
	id := boundedText(reading.ID, protocol.MaxResourceIDLength)
	if reading.Status != procfs.StatusOK || reading.RXBytesPerSec == nil || reading.TXBytesPerSec == nil {
		return protocol.ResourceUnavailable{
			ID:     resourceID(id, "net:unknown"),
			Reason: unavailableReason(reading.Reason, "network I/O unavailable"),
		}
	}
	rx, tx := float64(*reading.RXBytesPerSec), float64(*reading.TXBytesPerSec)
	if !inRange(rx, 0, maxRateBytesPerSec) || !inRange(tx, 0, maxRateBytesPerSec) {
		return protocol.ResourceUnavailable{
			ID:     resourceID(id, "net:unknown"),
			Reason: unavailableReason(reading.Reason, "network I/O unavailable"),
		}
	}
	return protocol.InterfaceAvailable{ID: resourceID(id, "net:unknown"), RxBytesPerSec: rx, TxBytesPerSec: tx}
}

// ToProtocolFailures converts and bounds the sampler's failure list.
func ToProtocolFailures(failures []procfs.CollectionFailure) []protocol.CollectionFailure {
	out := make([]protocol.CollectionFailure, 0, len(failures))
	for _, failure := range failures {
		if len(out) >= protocol.MaxCollectionFailures {
			break
		}
		metric := boundedText(failure.Metric, 64)
		if metric == "" {
			continue
		}
		var resourceID *string
		if failure.ResourceID != nil {
			id := boundedText(*failure.ResourceID, protocol.MaxResourceIDLength)
			resourceID = &id
		}
		out = append(out, protocol.CollectionFailure{
			Metric:     metric,
			ResourceID: resourceID,
			Reason:     boundedText(failure.Reason, protocol.MaxTextLength),
		})
	}
	return out
}

// ToProtocolProcessSnapshot converts one scanner snapshot into the wire type.
// Optional values stay nil: unknown is null, never a fabricated zero.
func ToProtocolProcessSnapshot(snapshot processes.ProcessSnapshot) protocol.ProcessSnapshot {
	rows := make([]protocol.ProcessRow, 0, len(snapshot.Processes))
	for _, row := range snapshot.Processes {
		rows = append(rows, ToProtocolProcessRow(row))
	}
	return protocol.ProcessSnapshot{
		TotalProcesses: boundedInt64(int64(snapshot.TotalProcesses), 0, maxProcessTotal),
		Truncated:      snapshot.Truncated,
		Inaccessible:   boundedInt64(int64(snapshot.Inaccessible), 0, maxProcessTotal),
		Processes:      rows,
	}
}

// ToProtocolProcessRow converts one scanner row into the wire type.
func ToProtocolProcessRow(row processes.ProcessRow) protocol.ProcessRow {
	out := protocol.ProcessRow{
		PID:                boundedInt64(int64(row.PID), 0, maxWirePID),
		PPID:               boundedInt64(int64(row.PPID), 0, maxWirePID),
		StartTicks:         boundedInt64(row.StartTicks, 0, protocol.MaxSafeInteger),
		Name:               boundedText(row.Name, maxProcessName),
		State:              boundedText(row.State, maxProcessState),
		CPUPercent:         boundedFloat(row.CPUPercent, 0, maxProcessCPU),
		MemoryRSSBytes:     boundedInt(row.MemoryRSSBytes, 0, protocol.MaxSafeInteger),
		GpuMemoryBytes:     boundedInt(row.GPUMemoryBytes, 0, protocol.MaxSafeInteger),
		IOReadBytesPerSec:  boundedFloat(row.IOReadBytesPerSec, 0, maxRateBytesPerSec),
		IOWriteBytesPerSec: boundedFloat(row.IOWriteBytesPerSec, 0, maxRateBytesPerSec),
	}
	if row.User != nil {
		if user := boundedText(*row.User, maxProcessUser); user != "" {
			out.User = &user
		}
	}
	if row.Threads != nil {
		if threads := int64(*row.Threads); inRangeInt(threads, 0, maxProcessThreads) {
			out.Threads = &threads
		}
	}
	if row.Unit != nil {
		if unit := boundedText(*row.Unit, maxProcessUnit); unit != "" {
			out.Unit = &unit
		}
	}
	if row.Command != nil {
		if command := boundedText(*row.Command, maxProcessCommand); command != "" {
			out.Command = &command
		}
	}
	return out
}

// ToProtocolDetailResult converts one scheduler detail result into the wire
// type. The result's capturedAt is preserved exactly: the scheduler already
// formats the canonical protocol form.
func ToProtocolDetailResult(result processes.DetailResult) protocol.DetailResult {
	return protocol.DetailResult{
		SubscriptionID: boundedText(result.SubscriptionID, protocol.MaxIDLength),
		CapturedAt:     result.CapturedAt,
		Snapshot:       ToProtocolProcessSnapshot(result.Snapshot),
	}
}

func unavailableReason(reason, fallback string) string {
	if reason == "" {
		return fallback
	}
	return boundedText(reason, protocol.MaxTextLength)
}

func resourceID(id, fallback string) string {
	if id == "" {
		return fallback
	}
	return id
}

func boundedText(value string, max int) string {
	return procfs.CapText(value, max)
}

func optionalFloat(value *float64, min, max float64) *protocol.Optional[float64] {
	bounded := boundedFloat(value, min, max)
	if bounded == nil {
		return nil
	}
	return protocol.Some(*bounded)
}

func boundedFloat(value *float64, min, max float64) *float64 {
	if value == nil || !inRange(*value, min, max) {
		return nil
	}
	return value
}

func boundedInt(value *int64, min, max int64) *int64 {
	if value == nil || !inRangeInt(*value, min, max) {
		return nil
	}
	return value
}

func boundedInt64(value, min, max int64) int64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func inRange(value, min, max float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= min && value <= max
}

func inRangeInt(value, min, max int64) bool {
	return value >= min && value <= max
}
