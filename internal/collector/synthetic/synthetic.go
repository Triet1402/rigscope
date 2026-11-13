// Package synthetic provides the explicit mock-mode machine used by the
// website demo and by tests. Every value is clearly fake: names are prefixed
// "synthetic" and the only GPU id is gpu:SYNTHETIC-0, so a screenshot or a
// fixture can never be mistaken for real hardware.
//
// It produces the same protocol types as the real assembler so the collector
// composition root can swap implementations without branching on payload
// shape. It owns no delivery, timers or configuration.
package synthetic

import (
	"errors"
	"math"
	"math/rand"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

const (
	syntheticCores      = 8
	syntheticMemory     = int64(32 * 1024 * 1024 * 1024)
	syntheticSwap       = int64(8 * 1024 * 1024 * 1024)
	syntheticFilesystem = int64(500 * 1024 * 1024 * 1024)
	syntheticGPUMemory  = int64(8 * 1024 * 1024 * 1024)
	cpuPeriodMS         = 37_000
	memoryPeriodMS      = 91_000
)

// Sequencer allocates record sequence numbers for the mock run.
type Sequencer interface {
	Next() (int64, error)
}

// Options injects the sampler's identity, clock, randomness and sequence
// allocator. Identity fields are plain strings so this package stays
// independent of the real assembler's types.
type Options struct {
	CollectorID string
	RunID       string
	BootID      string
	// Sequence is the run's record allocator. Required.
	Sequence Sequencer
	// Now is the wall clock; default time.Now.
	Now func() time.Time
	// Rand returns values in [0,1); default a private rand source.
	Rand func() float64
}

// Sampler is the mock-mode sampler. It is not safe for concurrent use.
type Sampler struct {
	collectorID string
	runID       string
	bootID      string
	sequence    Sequencer
	now         func() time.Time
	random      func() float64
	version     int64
}

// NewSampler validates the options and returns the mock sampler.
func NewSampler(options Options) (*Sampler, error) {
	if options.CollectorID == "" || options.RunID == "" {
		return nil, errors.New("synthetic: CollectorID and RunID are required")
	}
	if options.Sequence == nil {
		return nil, errors.New("synthetic: Options.Sequence is required")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	random := options.Rand
	if random == nil {
		source := rand.New(rand.NewSource(time.Now().UnixNano()))
		random = source.Float64
	}
	return &Sampler{
		collectorID: options.CollectorID,
		runID:       options.RunID,
		bootID:      options.BootID,
		sequence:    options.Sequence,
		now:         now,
		random:      random,
	}, nil
}

// Next returns one deterministic fake metrics record for the pinned clock and
// random source.
func (s *Sampler) Next() (protocol.MetricsRecord, error) {
	timestamp := s.now()
	milliseconds := float64(timestamp.UnixMilli())
	cpuWave := math.Sin(math.Mod(milliseconds, cpuPeriodMS) / cpuPeriodMS * 2 * math.Pi)
	memoryWave := (math.Sin(math.Mod(milliseconds, memoryPeriodMS)/memoryPeriodMS*2*math.Pi) + 1) / 2
	if s.version == 0 {
		s.version = 1
	}
	sequence, err := s.sequence.Next()
	if err != nil {
		return protocol.MetricsRecord{}, err
	}
	capturedAt, err := formatCapturedAt(timestamp)
	if err != nil {
		return protocol.MetricsRecord{}, err
	}

	perCore := make([]*float64, syntheticCores)
	for index := range perCore {
		value := percent(18 + 22*math.Sin(cpuWave+float64(index)/3) + (s.random()*3 - 1.5))
		perCore[index] = &value
	}
	cpuUsage := percent(18 + 22*cpuWave + (s.random()*4 - 2))
	cpuTemperature := math.Round(52 + 6*memoryWave)
	cpuPower := math.Round(70 + 30*cpuWave)
	iowait := percent(0.8 + 0.4*math.Abs(cpuWave))
	cpuPressure := percent(2 + 3*math.Abs(cpuWave))
	usedMemory := usage(0.35+0.25*memoryWave+(s.random()*0.01-0.005), syntheticMemory)
	memoryPressure := percent(1 + 2*memoryWave)
	memoryFull := percent(0.2 + 0.3*memoryWave)
	swap := usage(0.1+0.05*memoryWave, syntheticSwap)

	gpuUsage := percent(25 + 30*cpuWave + s.random()*3)
	gpuMemory := int64(math.Round(clamp(0.4+0.2*memoryWave+s.random()*0.01, 0, 1) * float64(syntheticGPUMemory)))
	gpuTemperature := math.Round(45 + 8*memoryWave)
	gpuPower := math.Round(90 + 30*cpuWave)
	junction := math.Round((57+10*memoryWave+s.random())*10) / 10
	vram := math.Round((60+9*memoryWave+s.random())*10) / 10

	filesystemUsed := int64(math.Round((0.4 + 0.1*memoryWave) * float64(syntheticFilesystem)))
	inodesUsed := int64(math.Round(12_000_000 + 1_000_000*memoryWave))
	readRate := float64(math.Round(1_000_000 + 500_000*memoryWave))
	writeRate := float64(math.Round(400_000 + 200_000*memoryWave))
	readLatency := math.Round((0.3+0.2*memoryWave)*1000) / 1000
	writeLatency := math.Round((0.7+0.3*memoryWave)*1000) / 1000
	ioUtilization := percent(8 + 12*math.Abs(cpuWave))
	diskPressure := percent(1.5 + 2*math.Abs(cpuWave))
	diskFull := percent(0.4 + 0.6*math.Abs(cpuWave))
	rxRate := float64(math.Round(900_000 + 300_000*cpuWave))
	txRate := float64(math.Round(120_000 + 40_000*cpuWave))

	payload := protocol.MetricsPayload{
		CPU: protocol.CpuAvailable{
			UsagePercent:        cpuUsage,
			PerCore:             perCore,
			TemperatureC:        protocol.Some(cpuTemperature),
			PowerWatts:          protocol.Some(cpuPower),
			IowaitPercent:       protocol.Some(iowait),
			PressureSomePercent: protocol.Some(cpuPressure),
		},
		Memory: protocol.MemoryAvailable{
			UsedBytes:           usedMemory,
			TotalBytes:          syntheticMemory,
			PressureSomePercent: protocol.Some(memoryPressure),
			PressureFullPercent: protocol.Some(memoryFull),
		},
		Swap: protocol.BytesUsageAvailable{UsedBytes: swap, TotalBytes: syntheticSwap},
		GPUs: protocol.GpusAvailable{Devices: []protocol.GpuReading{
			protocol.GpuAvailable{
				ID:                   "gpu:SYNTHETIC-0",
				UtilizationPercent:   &gpuUsage,
				MemoryUsedBytes:      &gpuMemory,
				MemoryTotalBytes:     protocol.Ptr(syntheticGPUMemory),
				TemperatureC:         &gpuTemperature,
				PowerWatts:           &gpuPower,
				JunctionTemperatureC: protocol.Some(junction),
				VramTemperatureC:     protocol.Some(vram),
			},
		}},
		Disks: protocol.DisksAvailable{
			Filesystems: []protocol.FilesystemReading{
				protocol.FilesystemAvailable{
					ID:          "fs:/",
					UsedBytes:   filesystemUsed,
					TotalBytes:  syntheticFilesystem,
					InodesUsed:  protocol.Some(inodesUsed),
					InodesTotal: protocol.Some(int64(30_000_000)),
				},
			},
			Devices: []protocol.DiskIoReading{
				protocol.DiskIoAvailable{
					ID:                   "disk:synthetic0",
					ReadBytesPerSec:      readRate,
					WriteBytesPerSec:     writeRate,
					ReadLatencyMs:        protocol.Some(readLatency),
					WriteLatencyMs:       protocol.Some(writeLatency),
					IoUtilizationPercent: protocol.Some(ioUtilization),
				},
			},
			PressureSomePercent: protocol.Some(diskPressure),
			PressureFullPercent: protocol.Some(diskFull),
		},
		Network: protocol.NetworkAvailable{Interfaces: []protocol.InterfaceReading{
			protocol.InterfaceAvailable{
				ID:            "net:synthetic0",
				RxBytesPerSec: rxRate,
				TxBytesPerSec: txRate,
			},
		}},
	}

	return protocol.MetricsRecord{
		RecordEnvelope: protocol.RecordEnvelope{
			CollectorID: s.collectorID,
			RunID:       s.runID,
			BootID:      s.bootID,
			Sequence:    sequence,
			CapturedAt:  capturedAt,
		},
		Payload: payload,
	}, nil
}

// Inventory returns a fresh synthetic inventory. Version becomes 1 after the
// first call, like the prototype.
func (s *Sampler) Inventory() protocol.Inventory {
	if s.version == 0 {
		s.version = 1
	}
	return syntheticInventory()
}

// InventoryVersion counts synthetic inventory changes. The fake inventory is
// constant, so it is 0 before the first call and 1 afterwards.
func (s *Sampler) InventoryVersion() int64 {
	return s.version
}

// Failures is always empty: synthetic mode never reports collection failures.
func (s *Sampler) Failures() []protocol.CollectionFailure {
	return []protocol.CollectionFailure{}
}

func syntheticInventory() protocol.Inventory {
	return protocol.Inventory{
		OS: protocol.OSInventory{
			Platform: "synthetic",
			Release:  "synthetic-0",
			Distro:   protocol.Ptr("Synthetic Linux"),
			Arch:     "synthetic",
		},
		CPU: protocol.CPUInventory{
			Model:        protocol.Ptr("synthetic 8-core processor"),
			LogicalCores: syntheticCores,
		},
		Memory: protocol.MemoryInventory{
			TotalBytes:     protocol.Ptr(syntheticMemory),
			SwapTotalBytes: protocol.Ptr(syntheticSwap),
		},
		GPUs: []protocol.GpuInventory{
			{
				ID:               "gpu:SYNTHETIC-0",
				Name:             "synthetic GPU",
				Vendor:           protocol.GpuVendorOther,
				PciBusID:         nil,
				Telemetry:        protocol.TelemetrySupported,
				MemoryTotalBytes: protocol.Ptr(syntheticGPUMemory),
			},
		},
		Filesystems: []protocol.FilesystemInventory{
			{
				ID:         "fs:/",
				MountPoint: "/",
				Device:     "synthetic",
				FsType:     "syntheticfs",
				TotalBytes: protocol.Ptr(syntheticFilesystem),
			},
		},
		Disks: []protocol.DiskInventory{
			{
				ID:         "disk:synthetic0",
				Name:       "synthetic0",
				Model:      protocol.Ptr("synthetic disk"),
				Rotational: protocol.Ptr(false),
				SizeBytes:  protocol.Ptr(syntheticFilesystem),
			},
		},
		Network: []protocol.InterfaceInventory{
			{
				ID:        "net:synthetic0",
				Name:      "synthetic0",
				Mac:       protocol.Ptr("00:00:00:00:00:01"),
				Addresses: []protocol.InterfaceAddress{{Family: protocol.AddressFamilyIPv4, Address: "192.0.2.10", Prefix: 24}},
				Virtual:   false,
				State:     protocol.InterfaceStateUp,
				SpeedMbps: protocol.Ptr(int64(1000)),
			},
		},
		Capabilities: protocol.Capabilities{
			CPU:       protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Memory:    protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Swap:      protocol.CapabilityState{Status: protocol.CapabilitySupported},
			GPUs:      protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Disks:     protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Network:   protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Processes: protocol.CapabilityState{Status: protocol.CapabilitySupported},
		},
	}
}

func clamp(value, minValue, maxValue float64) float64 {
	return math.Min(maxValue, math.Max(minValue, value))
}

func percent(value float64) float64 {
	return math.Round(clamp(value, 0, 100)*100) / 100
}

func usage(fraction float64, total int64) int64 {
	return int64(math.Round(clamp(fraction, 0, 1) * float64(total)))
}

const capturedAtLayout = "2006-01-02T15:04:05.000Z"

// formatCapturedAt renders the canonical JS toISOString form and refuses a
// clock outside the protocol's sanity window.
func formatCapturedAt(t time.Time) (string, error) {
	utc := t.UTC()
	if utc.Year() < 2000 || utc.Year() >= 2200 {
		return "", errors.New("synthetic: capturedAt is outside the protocol window")
	}
	return utc.Format(capturedAtLayout), nil
}
