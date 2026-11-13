package storage

import (
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// payloadOptions mirrors the TypeScript storage fixture so the ported cases
// describe the same shapes.
type payloadOptions struct {
	cpuUsage           *float64
	cores              int
	coreValue          *float64
	coreNullIndex      *int
	cpuTemperature     *protocol.Optional[float64]
	cpuPower           *protocol.Optional[float64]
	cpuIowait          *protocol.Optional[float64]
	cpuPressure        *protocol.Optional[float64]
	memoryUsed         int64
	memoryTotal        int64
	memoryPressureSome *protocol.Optional[float64]
	memoryPressureFull *protocol.Optional[float64]
	diskPressureSome   *protocol.Optional[float64]
	diskPressureFull   *protocol.Optional[float64]
	swapUsed           int64
	swapTotal          int64
	gpus               []gpuFixture
	filesystems        []filesystemFixture
	disks              []diskFixture
	interfaces         []interfaceFixture
	unavailableCPU     bool
	unavailableMemory  bool
	unavailableSwap    bool
	unavailableGPUs    bool
	unavailableDisks   bool
	unavailableNetwork bool
}

type gpuFixture struct {
	id          string
	utilization *float64
	memoryUsed  *int64
	memoryTotal *int64
	temperature *float64
	power       *float64
	junction    *protocol.Optional[float64]
	vram        *protocol.Optional[float64]
	unavailable bool
}

type filesystemFixture struct {
	id          string
	used        int64
	total       int64
	inodesUsed  *protocol.Optional[int64]
	inodesTotal *protocol.Optional[int64]
	unavailable bool
}

type diskFixture struct {
	id           string
	read         float64
	write        float64
	readLatency  *protocol.Optional[float64]
	writeLatency *protocol.Optional[float64]
	utilization  *protocol.Optional[float64]
	unavailable  bool
}

type interfaceFixture struct {
	id          string
	rx          float64
	tx          float64
	unavailable bool
}

func f64(value float64) *float64 { return &value }
func i64(value int64) *int64     { return &value }

func makePayload(options payloadOptions) protocol.MetricsPayload {
	if options.cpuUsage == nil {
		options.cpuUsage = f64(10)
	}
	if options.cores == 0 {
		options.cores = 2
	}
	if options.coreValue == nil {
		options.coreValue = f64(5)
	}
	if options.memoryUsed == 0 && options.memoryTotal == 0 {
		options.memoryUsed = 400
		options.memoryTotal = 1000
	}
	if options.swapUsed == 0 && options.swapTotal == 0 {
		options.swapUsed = 100
		options.swapTotal = 500
	}

	var cpu protocol.CpuMetric
	if options.unavailableCPU {
		cpu = protocol.Unavailable{Reason: "test unavailable"}
	} else {
		perCore := make([]*float64, options.cores)
		for index := range perCore {
			if options.coreNullIndex != nil && index == *options.coreNullIndex {
				perCore[index] = nil
			} else {
				perCore[index] = options.coreValue
			}
		}
		available := protocol.CpuAvailable{UsagePercent: *options.cpuUsage, PerCore: perCore}
		if options.cpuTemperature != nil {
			available.TemperatureC = options.cpuTemperature
		}
		if options.cpuPower != nil {
			available.PowerWatts = options.cpuPower
		}
		if options.cpuIowait != nil {
			available.IowaitPercent = options.cpuIowait
		}
		if options.cpuPressure != nil {
			available.PressureSomePercent = options.cpuPressure
		}
		cpu = available
	}

	var memory protocol.MemoryMetric
	if options.unavailableMemory {
		memory = protocol.Unavailable{Reason: "test unavailable"}
	} else {
		memory = protocol.MemoryAvailable{
			UsedBytes:           options.memoryUsed,
			TotalBytes:          options.memoryTotal,
			PressureSomePercent: options.memoryPressureSome,
			PressureFullPercent: options.memoryPressureFull,
		}
	}

	var swap protocol.BytesUsageMetric
	if options.unavailableSwap {
		swap = protocol.Unavailable{Reason: "test unavailable"}
	} else {
		swap = protocol.BytesUsageAvailable{UsedBytes: options.swapUsed, TotalBytes: options.swapTotal}
	}

	var gpus protocol.GpusMetric
	if options.unavailableGPUs {
		gpus = protocol.Unavailable{Reason: "test unavailable"}
	} else {
		devices := make([]protocol.GpuReading, 0, len(options.gpus))
		for _, gpu := range options.gpus {
			if gpu.unavailable {
				devices = append(devices, protocol.ResourceUnavailable{ID: gpu.id, Reason: "test unavailable"})
				continue
			}
			devices = append(devices, protocol.GpuAvailable{
				ID:                   gpu.id,
				UtilizationPercent:   gpu.utilization,
				MemoryUsedBytes:      gpu.memoryUsed,
				MemoryTotalBytes:     gpu.memoryTotal,
				TemperatureC:         gpu.temperature,
				PowerWatts:           gpu.power,
				JunctionTemperatureC: gpu.junction,
				VramTemperatureC:     gpu.vram,
			})
		}
		gpus = protocol.GpusAvailable{Devices: devices}
	}

	var disksMetric protocol.DisksMetric
	if options.unavailableDisks {
		disksMetric = protocol.Unavailable{Reason: "test unavailable"}
	} else {
		filesystems := make([]protocol.FilesystemReading, 0, len(options.filesystems))
		for _, fs := range options.filesystems {
			if fs.unavailable {
				filesystems = append(filesystems, protocol.ResourceUnavailable{ID: fs.id, Reason: "test unavailable"})
				continue
			}
			filesystems = append(filesystems, protocol.FilesystemAvailable{
				ID: fs.id, UsedBytes: fs.used, TotalBytes: fs.total,
				InodesUsed: fs.inodesUsed, InodesTotal: fs.inodesTotal,
			})
		}
		devices := make([]protocol.DiskIoReading, 0, len(options.disks))
		for _, disk := range options.disks {
			if disk.unavailable {
				devices = append(devices, protocol.ResourceUnavailable{ID: disk.id, Reason: "test unavailable"})
				continue
			}
			devices = append(devices, protocol.DiskIoAvailable{
				ID: disk.id, ReadBytesPerSec: disk.read, WriteBytesPerSec: disk.write,
				ReadLatencyMs: disk.readLatency, WriteLatencyMs: disk.writeLatency,
				IoUtilizationPercent: disk.utilization,
			})
		}
		disksMetric = protocol.DisksAvailable{
			Filesystems:         filesystems,
			Devices:             devices,
			PressureSomePercent: options.diskPressureSome,
			PressureFullPercent: options.diskPressureFull,
		}
	}

	var network protocol.NetworkMetric
	if options.unavailableNetwork {
		network = protocol.Unavailable{Reason: "test unavailable"}
	} else {
		interfaces := make([]protocol.InterfaceReading, 0, len(options.interfaces))
		for _, iface := range options.interfaces {
			if iface.unavailable {
				interfaces = append(interfaces, protocol.ResourceUnavailable{ID: iface.id, Reason: "test unavailable"})
				continue
			}
			interfaces = append(interfaces, protocol.InterfaceAvailable{
				ID: iface.id, RxBytesPerSec: iface.rx, TxBytesPerSec: iface.tx,
			})
		}
		network = protocol.NetworkAvailable{Interfaces: interfaces}
	}

	return protocol.MetricsPayload{CPU: cpu, Memory: memory, Swap: swap, GPUs: gpus, Disks: disksMetric, Network: network}
}

func isoAt(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

type recordOptions struct {
	collectorID string
	runID       string
	bootID      string
	sequence    int64
	capturedAt  int64
	payload     *protocol.MetricsPayload
}

func makeMetricsRecord(options recordOptions) protocol.MetricsRecord {
	if options.collectorID == "" {
		options.collectorID = "collector-1"
	}
	if options.runID == "" {
		options.runID = "run-1"
	}
	if options.bootID == "" {
		options.bootID = "boot-1"
	}
	payload := makePayload(payloadOptions{})
	if options.payload != nil {
		payload = *options.payload
	}
	return protocol.MetricsRecord{
		RecordEnvelope: protocol.RecordEnvelope{
			CollectorID: options.collectorID,
			RunID:       options.runID,
			BootID:      options.bootID,
			Sequence:    options.sequence,
			CapturedAt:  isoAt(options.capturedAt),
		},
		Payload: payload,
	}
}

type gapOptions struct {
	collectorID string
	runID       string
	sequence    int64
	capturedAt  int64
	fromMs      int64
	toMs        int64
	dropped     int64
	reason      protocol.GapReason
}

func makeGapRecord(options gapOptions) protocol.GapRecord {
	if options.collectorID == "" {
		options.collectorID = "collector-1"
	}
	if options.runID == "" {
		options.runID = "run-1"
	}
	if options.dropped == 0 {
		options.dropped = 1
	}
	if options.reason == "" {
		options.reason = protocol.GapOutboxBytes
	}
	return protocol.GapRecord{
		RecordEnvelope: protocol.RecordEnvelope{
			CollectorID: options.collectorID,
			RunID:       options.runID,
			BootID:      "boot-1",
			Sequence:    options.sequence,
			CapturedAt:  isoAt(options.capturedAt),
		},
		Payload: protocol.GapPayload{
			FromCapturedAt: isoAt(options.fromMs),
			ToCapturedAt:   isoAt(options.toMs),
			DroppedRecords: options.dropped,
			Reason:         options.reason,
		},
	}
}

func makeInventoryFixture() protocol.Inventory {
	supported := protocol.CapabilityState{Status: protocol.CapabilitySupported}
	return protocol.Inventory{
		OS:          protocol.OSInventory{Platform: "linux", Release: "6.1.0", Arch: "x86_64"},
		CPU:         protocol.CPUInventory{LogicalCores: 2},
		Memory:      protocol.MemoryInventory{TotalBytes: i64(8 << 30), SwapTotalBytes: i64(1 << 30)},
		GPUs:        []protocol.GpuInventory{},
		Filesystems: []protocol.FilesystemInventory{},
		Disks:       []protocol.DiskInventory{},
		Network:     []protocol.InterfaceInventory{},
		Capabilities: protocol.Capabilities{
			CPU: supported, Memory: supported, Swap: supported, GPUs: supported,
			Disks: supported, Network: supported, Processes: supported,
		},
	}
}

func liveBatch(records ...protocol.TelemetryRecord) Batch {
	return Batch{MachineID: "machine-1", CollectorID: "collector-1", Mode: protocol.SampleModeLive, Records: records}
}

func replayBatch(records ...protocol.TelemetryRecord) Batch {
	batch := liveBatch(records...)
	batch.Mode = protocol.SampleModeReplay
	return batch
}
