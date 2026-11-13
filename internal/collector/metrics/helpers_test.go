package metrics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/sampling/gpu"
	"github.com/Triet1402/rigscope/internal/collector/sampling/procfs"
	"github.com/Triet1402/rigscope/internal/protocol"
)

// testdataPath resolves a path under the repository's testdata directory.
func testdataPath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "..", "testdata"}, parts...)...)
}

// fixedClock is a manually advanced wall+monotonic clock.
type fixedClock struct {
	now       time.Time
	monotonic time.Duration
}

func newFixedClock() *fixedClock {
	return &fixedClock{now: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
}

func (c *fixedClock) Now() time.Time { return c.now }

func (c *fixedClock) Monotonic() time.Duration { return c.monotonic }

func (c *fixedClock) advance(delta time.Duration) {
	c.now = c.now.Add(delta)
	c.monotonic += delta
}

// fakeGPU is a GPUSource with an explicit snapshot and availability.
type fakeGPU struct {
	snapshot     *gpu.Snapshot
	availability gpu.Availability
	pollInterval time.Duration
	devices      []gpu.DeviceInfo
}

func (f *fakeGPU) Latest() *gpu.Snapshot {
	if f.snapshot == nil {
		return nil
	}
	copied := *f.snapshot
	copied.Devices = append([]gpu.GpuReading(nil), f.snapshot.Devices...)
	return &copied
}

func (f *fakeGPU) Available() gpu.Availability { return f.availability }

func (f *fakeGPU) PollInterval() time.Duration {
	if f.pollInterval <= 0 {
		return 2 * time.Second
	}
	return f.pollInterval
}

func (f *fakeGPU) DeviceInfo() []gpu.DeviceInfo {
	return append([]gpu.DeviceInfo(nil), f.devices...)
}

// fakeProc is a ProcSampler returning explicit readings and inventory.
type fakeProc struct {
	readings  procfs.Readings
	inventory procfs.HostInventory
	failures  []procfs.CollectionFailure
}

func (f *fakeProc) Sample() procfs.Readings { return f.readings }

func (f *fakeProc) Inventory() procfs.HostInventory {
	if f.inventory.OS.Platform == "" {
		return emptyHostInventory()
	}
	return f.inventory
}

func (f *fakeProc) Failures() []procfs.CollectionFailure { return f.failures }

func fixtureSampler(t *testing.T, host string, clock Clock) *procfs.Sampler {
	t.Helper()
	source := procfs.NewFSSource(os.DirFS(testdataPath("proc", host)))
	return NewProcFSSampler(clock, procfs.SamplerOptions{
		Source: source,
		Host: &procfs.HostInfo{
			Platform: "linux",
			Release:  "fixture-kernel",
			Arch:     "amd64",
			CPUCount: 48,
		},
		StatFS: func(string) (procfs.StatFSSnapshot, error) {
			return procfs.StatFSSnapshot{Bsize: 4096, Blocks: 1000, Bfree: 400, Files: 100, Ffree: 90}, nil
		},
		Addresses: func() (map[string][]procfs.InterfaceAddress, error) {
			return map[string][]procfs.InterfaceAddress{}, nil
		},
	})
}

func fixtureGPUSysFS(t *testing.T, name string) gpu.SysFS {
	t.Helper()
	return gpu.RootFS{FS: os.DirFS(testdataPath("gpu", "sysfs", name))}
}

func decodeSamples(t *testing.T, payload protocol.MetricsPayload, capturedAt string) protocol.MetricsRecord {
	t.Helper()
	encoded, err := protocol.EncodeCollectorMessage(protocol.SamplesMessage{
		Mode: protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{protocol.MetricsRecord{
			RecordEnvelope: protocol.RecordEnvelope{
				CollectorID: "collector-roundtrip",
				RunID:       "run-roundtrip",
				BootID:      "boot-roundtrip",
				Sequence:    1,
				CapturedAt:  capturedAt,
			},
			Payload: payload,
		}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := protocol.DecodeCollectorMessage(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return decoded.(protocol.SamplesMessage).Records[0].(protocol.MetricsRecord)
}

// minimalReadings is a valid one-tick reading set for tests that only need a
// collector to run, not meaningful host data.
func minimalReadings() procfs.Readings {
	usage := 1.0
	used, total := int64(1), int64(2)
	return procfs.Readings{
		CPU:     procfs.CpuMetric{Status: procfs.StatusOK, UsagePercent: &usage},
		Memory:  procfs.MemoryMetric{Status: procfs.StatusOK, UsedBytes: &used, TotalBytes: &total},
		Swap:    procfs.BytesUsageMetric{Status: procfs.StatusOK, UsedBytes: &used, TotalBytes: &total},
		Disks:   procfs.DisksMetric{Status: procfs.StatusOK},
		Network: procfs.NetworkMetric{Status: procfs.StatusOK},
	}
}

// maxInventoryFixture builds an inventory at every collection limit.
func maxInventoryFixture() protocol.Inventory {
	inventory := protocol.Inventory{
		OS:  protocol.OSInventory{Platform: "linux", Release: "6.0", Arch: "amd64"},
		CPU: protocol.CPUInventory{Model: protocol.Ptr("Max CPU"), LogicalCores: protocol.MaxCores},
		Memory: protocol.MemoryInventory{
			TotalBytes: protocol.Ptr(int64(protocol.MaxSafeInteger)), SwapTotalBytes: protocol.Ptr(int64(0)),
		},
		GPUs:        make([]protocol.GpuInventory, 0, protocol.MaxGPUs),
		Filesystems: make([]protocol.FilesystemInventory, 0, protocol.MaxFilesystems),
		Disks:       make([]protocol.DiskInventory, 0, protocol.MaxDisks),
		Network:     make([]protocol.InterfaceInventory, 0, protocol.MaxInterfaces),
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
	for index := 0; index < protocol.MaxGPUs; index++ {
		inventory.GPUs = append(inventory.GPUs, protocol.GpuInventory{
			ID:        fmt.Sprintf("gpu:GPU-%d", index),
			Name:      "GPU",
			Vendor:    protocol.GpuVendorNvidia,
			PciBusID:  protocol.Ptr(fmt.Sprintf("0000:%02x:00.0", index+1)),
			Telemetry: protocol.TelemetrySupported,
		})
	}
	for index := 0; index < protocol.MaxFilesystems; index++ {
		inventory.Filesystems = append(inventory.Filesystems, protocol.FilesystemInventory{
			ID: fmt.Sprintf("fs:/mnt/%d", index), MountPoint: fmt.Sprintf("/mnt/%d", index),
			Device: "/dev/sda", FsType: "ext4", TotalBytes: protocol.Ptr(int64(1000)),
		})
	}
	for index := 0; index < protocol.MaxDisks; index++ {
		inventory.Disks = append(inventory.Disks, protocol.DiskInventory{
			ID: fmt.Sprintf("disk:d%d", index), Name: fmt.Sprintf("d%d", index),
		})
	}
	for index := 0; index < protocol.MaxInterfaces; index++ {
		addresses := make([]protocol.InterfaceAddress, 0, 2)
		for addressIndex := 0; addressIndex < 2; addressIndex++ {
			addresses = append(addresses, protocol.InterfaceAddress{
				Family:  protocol.AddressFamilyIPv6,
				Address: fmt.Sprintf("2001:db8:%x::%x", index, addressIndex+1),
				Prefix:  64,
			})
		}
		inventory.Network = append(inventory.Network, protocol.InterfaceInventory{
			ID: fmt.Sprintf("net:n%d", index), Name: fmt.Sprintf("n%d", index),
			Addresses: addresses, State: protocol.InterfaceStateUp,
		})
	}
	return inventory
}

// maxReadingsFixture builds a metrics reading set at every collection limit.
func maxReadingsFixture() procfs.Readings {
	filesystems := make([]procfs.FilesystemReading, 0, protocol.MaxFilesystems)
	for index := 0; index < protocol.MaxFilesystems; index++ {
		id := fmt.Sprintf("fs:/mnt/%d", index)
		used, total := int64(index), int64(1000)
		filesystems = append(filesystems, procfs.FilesystemReading{
			Status: procfs.StatusOK, ID: id, UsedBytes: &used, TotalBytes: &total,
		})
	}
	devices := make([]procfs.DiskIoReading, 0, protocol.MaxDisks)
	for index := 0; index < protocol.MaxDisks; index++ {
		read, write := int64(index), int64(index*2)
		devices = append(devices, procfs.DiskIoReading{
			Status: procfs.StatusOK, ID: fmt.Sprintf("disk:d%d", index), ReadBytesPerSec: &read, WriteBytesPerSec: &write,
		})
	}
	interfaces := make([]procfs.InterfaceReading, 0, protocol.MaxInterfaces)
	for index := 0; index < protocol.MaxInterfaces; index++ {
		rx, tx := int64(index), int64(index)
		interfaces = append(interfaces, procfs.InterfaceReading{
			Status: procfs.StatusOK, ID: fmt.Sprintf("net:n%d", index), RXBytesPerSec: &rx, TXBytesPerSec: &tx,
		})
	}
	usage := 1.0
	return procfs.Readings{
		CPU:     procfs.CpuMetric{Status: procfs.StatusOK, UsagePercent: &usage, PerCore: []*float64{&usage, nil}},
		Memory:  procfs.MemoryMetric{Status: procfs.StatusOK, UsedBytes: protocol.Ptr(int64(1)), TotalBytes: protocol.Ptr(int64(2))},
		Swap:    procfs.BytesUsageMetric{Status: procfs.StatusOK, UsedBytes: protocol.Ptr(int64(0)), TotalBytes: protocol.Ptr(int64(0))},
		Disks:   procfs.DisksMetric{Status: procfs.StatusOK, Filesystems: filesystems, Devices: devices},
		Network: procfs.NetworkMetric{Status: procfs.StatusOK, Interfaces: interfaces},
	}
}

// newUnavailableSampler is a procfs sampler over an empty tree: every read
// fails, which the assembler must turn into honest unavailable families.
func newUnavailableSampler(clock Clock) *procfs.Sampler {
	return NewProcFSSampler(clock, procfs.SamplerOptions{
		Source: procfs.NewFSSource(fstest.MapFS{}),
		StatFS: func(string) (procfs.StatFSSnapshot, error) {
			return procfs.StatFSSnapshot{}, errors.New("no statfs in fixture")
		},
		Addresses: func() (map[string][]procfs.InterfaceAddress, error) {
			return map[string][]procfs.InterfaceAddress{}, nil
		},
	})
}
