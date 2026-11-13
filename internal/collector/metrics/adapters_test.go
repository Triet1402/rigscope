package metrics

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Triet1402/rigscope/internal/collector/processes"
	"github.com/Triet1402/rigscope/internal/collector/sampling/gpu"
	"github.com/Triet1402/rigscope/internal/collector/sampling/procfs"
	"github.com/Triet1402/rigscope/internal/protocol"
)

func envelope() protocol.RecordEnvelope {
	return protocol.RecordEnvelope{
		CollectorID: "collector-adapter",
		RunID:       "run-adapter",
		BootID:      "boot-adapter",
		Sequence:    1,
		CapturedAt:  "2026-11-12T10:00:00.000Z",
	}
}

func encodeMetrics(t *testing.T, payload protocol.MetricsPayload) {
	t.Helper()
	if _, err := protocol.EncodeCollectorMessage(protocol.SamplesMessage{
		Mode:    protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{protocol.MetricsRecord{RecordEnvelope: envelope(), Payload: payload}},
	}); err != nil {
		t.Fatalf("encode assembled metrics: %v", err)
	}
}

func TestAdaptersUnavailableFamiliesCarryNoValues(t *testing.T) {
	readings := procfs.Readings{
		CPU:     procfs.CpuMetric{Status: procfs.StatusUnavailable, Reason: "cpu counters unavailable"},
		Memory:  procfs.MemoryMetric{Status: procfs.StatusUnavailable, Reason: "memory counters unavailable"},
		Swap:    procfs.BytesUsageMetric{Status: procfs.StatusUnavailable, Reason: "swap counters unavailable"},
		Disks:   procfs.DisksMetric{Status: procfs.StatusUnavailable, Reason: "disk discovery unavailable"},
		Network: procfs.NetworkMetric{Status: procfs.StatusUnavailable, Reason: "network counters unavailable"},
	}
	payload := ToProtocolMetrics(readings, gpu.GpusUnavailable("no GPUs detected"))
	if _, ok := payload.CPU.(protocol.Unavailable); !ok {
		t.Fatalf("cpu = %#v, want unavailable", payload.CPU)
	}
	if _, ok := payload.Memory.(protocol.Unavailable); !ok {
		t.Fatalf("memory = %#v, want unavailable", payload.Memory)
	}
	if _, ok := payload.Swap.(protocol.Unavailable); !ok {
		t.Fatalf("swap = %#v, want unavailable", payload.Swap)
	}
	if _, ok := payload.GPUs.(protocol.Unavailable); !ok {
		t.Fatalf("gpus = %#v, want unavailable", payload.GPUs)
	}
	if _, ok := payload.Disks.(protocol.Unavailable); !ok {
		t.Fatalf("disks = %#v, want unavailable", payload.Disks)
	}
	if _, ok := payload.Network.(protocol.Unavailable); !ok {
		t.Fatalf("network = %#v, want unavailable", payload.Network)
	}
	encodeMetrics(t, payload)
}

func TestAdaptersOKFamiliesWithMissingRequiredValuesBecomeUnavailable(t *testing.T) {
	readings := procfs.Readings{
		CPU:     procfs.CpuMetric{Status: procfs.StatusOK},
		Memory:  procfs.MemoryMetric{Status: procfs.StatusOK, UsedBytes: int64Ptr(10)},
		Swap:    procfs.BytesUsageMetric{Status: procfs.StatusOK, UsedBytes: int64Ptr(10), TotalBytes: int64Ptr(5)},
		Disks:   procfs.DisksMetric{Status: procfs.StatusOK},
		Network: procfs.NetworkMetric{Status: procfs.StatusOK},
	}
	payload := ToProtocolMetrics(readings, gpu.GpusOK(nil))
	for family, value := range map[string]any{
		"cpu": payload.CPU, "memory": payload.Memory, "swap": payload.Swap,
	} {
		if _, ok := value.(protocol.Unavailable); !ok {
			t.Fatalf("%s = %#v, want unavailable", family, value)
		}
	}
	disks, ok := payload.Disks.(protocol.DisksAvailable)
	if !ok || disks.Filesystems == nil || disks.Devices == nil {
		t.Fatalf("disks = %#v, want empty arrays", payload.Disks)
	}
	network, ok := payload.Network.(protocol.NetworkAvailable)
	if !ok || network.Interfaces == nil {
		t.Fatalf("network = %#v, want empty array", payload.Network)
	}
	encodeMetrics(t, payload)
}

func TestAdaptersDropOutOfRangeGPUValuesInsteadOfEmittingThem(t *testing.T) {
	tooHot := 500.0
	tooFast := 200.0
	badUsed := int64(20)
	total := int64(10)
	payload := ToProtocolMetrics(procfs.Readings{
		CPU:     procfs.CpuMetric{Status: procfs.StatusUnavailable, Reason: "x"},
		Memory:  procfs.MemoryMetric{Status: procfs.StatusUnavailable, Reason: "x"},
		Swap:    procfs.BytesUsageMetric{Status: procfs.StatusUnavailable, Reason: "x"},
		Disks:   procfs.DisksMetric{Status: procfs.StatusUnavailable, Reason: "x"},
		Network: procfs.NetworkMetric{Status: procfs.StatusUnavailable, Reason: "x"},
	}, gpu.GpusOK([]gpu.GpuReading{{
		ID:                   "gpu:GPU-edge",
		Status:               gpu.StatusOK,
		UtilizationPercent:   &tooFast,
		MemoryUsedBytes:      &badUsed,
		MemoryTotalBytes:     &total,
		TemperatureC:         &tooHot,
		JunctionTemperatureC: &tooHot,
	}}))
	devices := payload.GPUs.(protocol.GpusAvailable).Devices
	reading, ok := devices[0].(protocol.GpuAvailable)
	if !ok {
		t.Fatalf("device = %#v, want available", devices[0])
	}
	if reading.UtilizationPercent != nil {
		t.Errorf("utilization = %v, want absent", *reading.UtilizationPercent)
	}
	if reading.TemperatureC != nil {
		t.Errorf("temperature = %v, want absent", *reading.TemperatureC)
	}
	if reading.MemoryUsedBytes != nil {
		t.Errorf("used = %v, want absent when it exceeds total", *reading.MemoryUsedBytes)
	}
	if reading.MemoryTotalBytes == nil || *reading.MemoryTotalBytes != 10 {
		t.Errorf("total = %v, want 10", reading.MemoryTotalBytes)
	}
	if reading.JunctionTemperatureC != nil {
		t.Error("junction temperature must be absent when out of range")
	}
	encodeMetrics(t, payload)
}

func TestAdaptersEmptyAndMaxCollectionsStayInsideTheContract(t *testing.T) {
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
	payload := ToProtocolMetrics(procfs.Readings{
		CPU:     procfs.CpuMetric{Status: procfs.StatusOK, UsagePercent: &usage, PerCore: []*float64{&usage, nil}},
		Memory:  procfs.MemoryMetric{Status: procfs.StatusOK, UsedBytes: int64Ptr(1), TotalBytes: int64Ptr(2)},
		Swap:    procfs.BytesUsageMetric{Status: procfs.StatusOK, UsedBytes: int64Ptr(0), TotalBytes: int64Ptr(0)},
		Disks:   procfs.DisksMetric{Status: procfs.StatusOK, Filesystems: filesystems, Devices: devices},
		Network: procfs.NetworkMetric{Status: procfs.StatusOK, Interfaces: interfaces},
	}, gpu.GpusOK(maxGPUReadings(protocol.MaxGPUs)))
	encodeMetrics(t, payload)

	empty := ToProtocolMetrics(procfs.Readings{
		CPU:     procfs.CpuMetric{Status: procfs.StatusOK, UsagePercent: &usage},
		Memory:  procfs.MemoryMetric{Status: procfs.StatusOK, UsedBytes: int64Ptr(1), TotalBytes: int64Ptr(2)},
		Swap:    procfs.BytesUsageMetric{Status: procfs.StatusOK, UsedBytes: int64Ptr(0), TotalBytes: int64Ptr(0)},
		Disks:   procfs.DisksMetric{Status: procfs.StatusOK},
		Network: procfs.NetworkMetric{Status: procfs.StatusOK},
	}, gpu.GpusOK(nil))
	encodeMetrics(t, empty)
}

func maxGPUReadings(count int) []gpu.GpuReading {
	readings := make([]gpu.GpuReading, 0, count)
	for index := 0; index < count; index++ {
		usage := float64(index)
		temperature := 40.0
		memory := int64(index * 1024)
		readings = append(readings, gpu.GpuReading{
			ID:                 fmt.Sprintf("gpu:GPU-%d", index),
			Status:             gpu.StatusOK,
			UtilizationPercent: &usage,
			MemoryUsedBytes:    &memory,
			MemoryTotalBytes:   &memory,
			TemperatureC:       &temperature,
		})
	}
	return readings
}

func TestAdaptersProcessSnapshotKeepsUnknownValuesNull(t *testing.T) {
	name := "worker"
	user := "alice"
	state := "S"
	cpu := 12.5
	rss := int64(4096)
	threads := 4
	unit := "worker.service"
	command := "/usr/bin/worker --serve"
	snapshot := processes.ProcessSnapshot{
		TotalProcesses: 1,
		Inaccessible:   2,
		Processes: []processes.ProcessRow{{
			PID: 42, PPID: 1, StartTicks: 900, Name: name, User: &user, State: state,
			CPUPercent: &cpu, MemoryRSSBytes: &rss, Threads: &threads, Unit: &unit, Command: &command,
		}, {
			PID: 43, PPID: 1, StartTicks: 901, Name: "idle", State: "S",
		}},
	}
	converted := ToProtocolProcessSnapshot(snapshot)
	if converted.TotalProcesses != 1 || converted.Inaccessible != 2 || len(converted.Processes) != 2 {
		t.Fatalf("snapshot = %#v", converted)
	}
	first := converted.Processes[0]
	if first.User == nil || *first.User != "alice" || first.Command == nil || *first.Command != command {
		t.Fatalf("first row = %#v", first)
	}
	second := converted.Processes[1]
	if second.User != nil || second.CPUPercent != nil || second.MemoryRSSBytes != nil ||
		second.Threads != nil || second.Unit != nil || second.Command != nil {
		t.Fatalf("second row invented values: %#v", second)
	}
	if _, err := protocol.EncodeCollectorMessage(protocol.DetailResult{
		SubscriptionID: "sub-1",
		CapturedAt:     "2026-11-12T10:00:00.000Z",
		Snapshot:       converted,
	}); err != nil {
		t.Fatalf("encode detail: %v", err)
	}
}

func TestAdaptersCapFailures(t *testing.T) {
	resource := "disk:sda"
	failures := make([]procfs.CollectionFailure, 0, protocol.MaxCollectionFailures+10)
	for index := 0; index < protocol.MaxCollectionFailures+10; index++ {
		failures = append(failures, procfs.CollectionFailure{
			Metric: "disks", ResourceID: &resource, Reason: "reason",
		})
	}
	converted := ToProtocolFailures(failures)
	if len(converted) != protocol.MaxCollectionFailures {
		t.Fatalf("failures = %d, want %d", len(converted), protocol.MaxCollectionFailures)
	}
	for _, failure := range converted {
		if failure.Reason == "" || failure.Metric == "" || failure.ResourceID == nil {
			t.Fatalf("failure lost a field: %#v", failure)
		}
	}
}

type panickingScanner struct{}

func (panickingScanner) Scan(context.Context, processes.ScanOptions) (processes.ProcessSnapshot, error) {
	panic("process scan exploded")
}

func TestRecoveringScannerTurnsAPanicIntoAnError(t *testing.T) {
	adapter := RecoveringScanner{Scanner: panickingScanner{}}
	snapshot, err := adapter.Scan(context.Background(), processes.ScanOptions{})
	if !errors.Is(err, ErrProcessScanPanicked) {
		t.Fatalf("error = %v, want ErrProcessScanPanicked", err)
	}
	if len(snapshot.Processes) != 0 {
		t.Fatalf("snapshot = %#v, want no partial rows", snapshot)
	}

	if _, err := (RecoveringScanner{}).Scan(context.Background(), processes.ScanOptions{}); err == nil {
		t.Fatal("a missing scanner must return an error")
	}
}

func int64Ptr(value int64) *int64 { return &value }
