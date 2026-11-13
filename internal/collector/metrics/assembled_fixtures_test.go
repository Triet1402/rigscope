package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/policy"
	"github.com/Triet1402/rigscope/internal/collector/processes"
	"github.com/Triet1402/rigscope/internal/collector/sampling/gpu"
	"github.com/Triet1402/rigscope/internal/collector/synthetic"
	"github.com/Triet1402/rigscope/internal/protocol"
)

// updateAssembled writes the generated fixtures instead of comparing them.
//
//	go test ./internal/collector/metrics -run TestAssembledFixtures -update-assembled
var updateAssembled = flag.Bool("update-assembled", false, "rewrite testdata/protocol/assembled")

// TestAssembledFixtures is the cross-language integration check: it assembles
// messages from the captured procfs fixture, the GPU fixtures, the process
// fixture and synthetic mode, and proves the committed JSON is exactly what
// the Go encoder produces. The TypeScript fixture test parses every file in
// the same directory with the strict wire parsers.
func TestAssembledFixtures(t *testing.T) {
	files := buildAssembledFixtures(t)
	directory := testdataPath("protocol", "assembled")

	if *updateAssembled {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
		existing, _ := os.ReadDir(directory)
		keep := make(map[string]bool, len(files))
		for name, data := range files {
			keep[name+".json"] = true
			if err := os.WriteFile(filepath.Join(directory, name+".json"), data, 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		for _, entry := range existing {
			if entry.IsDir() || keep[entry.Name()] {
				continue
			}
			if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
				t.Fatalf("remove stale %s: %v", entry.Name(), err)
			}
		}
		return
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read assembled fixtures: %v", err)
	}
	committed := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		committed[name] = true
		want, ok := files[name]
		if !ok {
			t.Fatalf("stale assembled fixture %s (run with -update-assembled)", entry.Name())
		}
		got, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("assembled fixture %s is stale (run with -update-assembled)", entry.Name())
		}
	}
	for name := range files {
		if !committed[name] {
			t.Fatalf("missing assembled fixture %s.json (run with -update-assembled)", name)
		}
	}
}

func buildAssembledFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)

	// Captured 48-core Linux host: procfs, sysfs bandwidth/drive fixtures,
	// NVIDIA sysfs display and nvidia-smi values from the GPU fixture.
	clock := newFixedClock()
	hostCollector := mustCollector(t, Options{
		Identity: Identity{CollectorID: "collector-assembled", RunID: "run-assembled", BootID: "boot-assembled"},
		Sequence: NewSequence(0),
		Clock:    clock,
		Proc:     fixtureSampler(t, "linux-host", clock),
		GPU:      nvidiaGPUFromCSV(t, clock),
		GPUFS:    fixtureGPUSysFS(t, "nvidia-mixed"),
	})
	if _, err := hostCollector.Next(); err != nil {
		t.Fatalf("warm-up tick: %v", err)
	}
	clock.advance(time.Second)
	hostRecord, err := hostCollector.Next()
	if err != nil {
		t.Fatalf("linux-host tick: %v", err)
	}
	files["collector-samples-assembled-linux-host"] = prettyMessage(t, protocol.SamplesMessage{
		Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{hostRecord},
	})
	files["collector-inventory-assembled-linux-host"] = prettyMessage(t, protocol.InventoryMessage{
		CapturedAt: hostRecord.CapturedAt, Inventory: hostCollector.Inventory(),
	})

	// AMD DRM counters straight out of the GPU sysfs fixture.
	amdClock := newFixedClock()
	amdCollector := mustCollector(t, Options{
		Identity: Identity{CollectorID: "collector-assembled", RunID: "run-amd", BootID: "boot-assembled"},
		Sequence: NewSequence(0),
		Clock:    amdClock,
		Proc:     fixtureSampler(t, "minimal", amdClock),
		GPUFS:    fixtureGPUSysFS(t, "amd-580"),
	})
	amdRecord, err := amdCollector.Next()
	if err != nil {
		t.Fatalf("amd tick: %v", err)
	}
	files["collector-samples-assembled-gpu-amd"] = prettyMessage(t, protocol.SamplesMessage{
		Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{amdRecord},
	})
	files["collector-inventory-assembled-gpu-amd"] = prettyMessage(t, protocol.InventoryMessage{
		CapturedAt: amdRecord.CapturedAt, Inventory: amdCollector.Inventory(),
	})

	// A host that cannot read procfs: every family unavailable, no values.
	unavailableClock := newFixedClock()
	unavailableCollector := mustCollector(t, Options{
		Identity: Identity{CollectorID: "collector-assembled", RunID: "run-unavailable", BootID: "boot-assembled"},
		Sequence: NewSequence(0),
		Clock:    unavailableClock,
		Proc:     newUnavailableSampler(unavailableClock),
		GPUFS:    gpu.RootFS{FS: fstest.MapFS{}},
	})
	unavailableRecord, err := unavailableCollector.Next()
	if err != nil {
		t.Fatalf("unavailable tick: %v", err)
	}
	files["collector-samples-assembled-unavailable"] = prettyMessage(t, protocol.SamplesMessage{
		Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{unavailableRecord},
	})
	files["collector-inventory-assembled-unavailable"] = prettyMessage(t, protocol.InventoryMessage{
		CapturedAt: unavailableRecord.CapturedAt, Inventory: unavailableCollector.Inventory(),
	})

	// Available families with empty collection lists.
	emptyClock := newFixedClock()
	emptyCollector := mustCollector(t, Options{
		Identity: Identity{CollectorID: "collector-assembled", RunID: "run-empty", BootID: "boot-assembled"},
		Sequence: NewSequence(0),
		Clock:    emptyClock,
		Proc:     &fakeProc{readings: minimalReadings(), inventory: emptyHostInventory()},
		GPUFS:    gpu.RootFS{FS: fstest.MapFS{}},
	})
	emptyRecord, err := emptyCollector.Next()
	if err != nil {
		t.Fatalf("empty tick: %v", err)
	}
	files["collector-samples-assembled-empty"] = prettyMessage(t, protocol.SamplesMessage{
		Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{emptyRecord},
	})

	// Every collection at its contract limit.
	maxClock := newFixedClock()
	maxCollector := mustCollector(t, Options{
		Identity: Identity{CollectorID: "collector-assembled", RunID: "run-max", BootID: "boot-assembled"},
		Sequence: NewSequence(0),
		Clock:    maxClock,
		Proc:     &fakeProc{readings: maxReadingsFixture(), inventory: emptyHostInventory()},
		GPU: &fakeGPU{
			availability: gpu.AvailabilityYes,
			snapshot:     &gpu.Snapshot{AtMonotonic: 0, Devices: maxGPUReadings(protocol.MaxGPUs)},
			devices:      []gpu.DeviceInfo{},
		},
		GPUFS: gpu.RootFS{FS: fstest.MapFS{}},
	})
	maxRecord, err := maxCollector.Next()
	if err != nil {
		t.Fatalf("max tick: %v", err)
	}
	files["collector-samples-assembled-max"] = prettyMessage(t, protocol.SamplesMessage{
		Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{maxRecord},
	})
	maxSharing := policy.Default()
	maxSharing.NetworkAddresses = true
	files["collector-inventory-assembled-max"] = prettyMessage(t, protocol.InventoryMessage{
		CapturedAt: maxRecord.CapturedAt,
		Inventory:  policy.FilterInventory(maxInventoryFixture(), maxSharing),
	})

	// Process detail from the captured process fixture, capped rows, with
	// command lines withheld by the default policy.
	detail := basicDetailResult(t)
	sharedDetail, ok := policy.ApplyDetail(detail, policy.Default())
	if !ok {
		t.Fatal("default policy dropped the assembled detail")
	}
	files["collector-detail-result-assembled-basic"] = prettyMessage(t, sharedDetail)
	files["collector-detail-result-assembled-empty"] = prettyMessage(t, protocol.DetailResult{
		SubscriptionID: "sub-empty",
		CapturedAt:     "2026-11-12T10:00:00.000Z",
		Snapshot:       protocol.ProcessSnapshot{Processes: []protocol.ProcessRow{}},
	})
	// A 7,000-row scan trimmed by the scheduler to MaxDetailBytes. This is the
	// wire shape the hub actually receives when the process table is too big;
	// it is written compactly because the message itself is near one MiB.
	files["collector-detail-result-assembled-trimmed"] = compactMessage(t, trimmedDetailResult(t))

	// Synthetic mode, pinned so the fixture is reproducible.
	syntheticSampler, err := synthetic.NewSampler(synthetic.Options{
		CollectorID: "mock-collector",
		RunID:       "mock-run",
		BootID:      "mock-boot",
		Sequence:    NewSequence(0),
		Now:         func() time.Time { return time.UnixMilli(1_700_000_000_000).UTC() },
		Rand:        func() float64 { return 0.25 },
	})
	if err != nil {
		t.Fatalf("synthetic sampler: %v", err)
	}
	syntheticRecord, err := syntheticSampler.Next()
	if err != nil {
		t.Fatalf("synthetic tick: %v", err)
	}
	files["collector-samples-assembled-synthetic"] = prettyMessage(t, protocol.SamplesMessage{
		Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{syntheticRecord},
	})
	files["collector-inventory-assembled-synthetic"] = prettyMessage(t, protocol.InventoryMessage{
		CapturedAt: syntheticRecord.CapturedAt, Inventory: syntheticSampler.Inventory(),
	})

	return files
}

func mustCollector(t *testing.T, options Options) *Collector {
	t.Helper()
	collector, err := NewCollector(options)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	return collector
}

// nvidiaGPUFromCSV derives the poller snapshot from the checked-in nvidia-smi
// fixture, then maps it to the single NVIDIA display in the nvidia-mixed
// sysfs tree so inventory can join it.
func nvidiaGPUFromCSV(t *testing.T, clock Clock) *fakeGPU {
	t.Helper()
	data, err := os.ReadFile(testdataPath("gpu", "nvidia", "gpu-3090.csv"))
	if err != nil {
		t.Fatalf("read nvidia fixture: %v", err)
	}
	rows := gpu.ParseGPUCSV(string(data))
	if len(rows) == 0 {
		t.Fatal("nvidia fixture produced no rows")
	}
	row := rows[0]
	devices := []gpu.GpuReading{{
		ID:                   "gpu:" + row.UUID,
		Status:               gpu.StatusOK,
		UtilizationPercent:   row.UtilizationPercent,
		MemoryUsedBytes:      row.MemoryUsedBytes,
		MemoryTotalBytes:     row.MemoryTotalBytes,
		TemperatureC:         row.TemperatureC,
		PowerWatts:           row.PowerWatts,
		JunctionTemperatureC: protocol.Ptr(61.5),
		VRAMTemperatureC:     protocol.Ptr(64.0),
	}}
	return &fakeGPU{
		availability: gpu.AvailabilityYes,
		pollInterval: 2 * time.Second,
		devices: []gpu.DeviceInfo{{
			UUID:             row.UUID,
			Name:             row.Name,
			PCIBusID:         protocol.Ptr("00000000:03:00.0"),
			MemoryTotalBytes: row.MemoryTotalBytes,
		}},
		snapshot: &gpu.Snapshot{AtMonotonic: clock.Monotonic(), Devices: devices},
	}
}

func basicDetailResult(t *testing.T) protocol.DetailResult {
	t.Helper()
	scanner, err := processes.NewScanner(processes.ScannerConfig{
		Proc:                os.DirFS(testdataPath("procs", "basic")),
		Users:               fixtureUsers{0: "root", 1000: "alice"},
		Clock:               func() time.Time { return time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC) },
		ClockTicksPerSecond: 100,
		PageSize:            4096,
		MaxRows:             10,
	})
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	snapshot, err := scanner.Scan(context.Background(), processes.ScanOptions{IncludeCommand: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return ToProtocolDetailResult(processes.DetailResult{
		Type:           "detail.result",
		SubscriptionID: "sub-basic",
		CapturedAt:     "2026-11-12T10:00:00.000Z",
		Snapshot:       snapshot,
	})
}

// trimmedDetailResult drives the real scheduler with a process table larger
// than the detail byte limit and returns the exact result it would deliver.
func trimmedDetailResult(t *testing.T) protocol.DetailResult {
	t.Helper()
	const rowCount = 7_000
	rows := make([]processes.ProcessRow, rowCount)
	for index := range rows {
		command := strings.Repeat("x", 96)
		rows[index] = processes.ProcessRow{
			PID: index + 100, PPID: 1, StartTicks: int64(index + 100),
			Name: "worker", State: "S", Command: &command,
		}
	}
	scanner := &oversizedScanner{snapshot: processes.ProcessSnapshot{
		TotalProcesses: rowCount,
		Processes:      rows,
	}}
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	var pending func()
	delivered := make([]processes.DetailMessage, 0, 1)
	scheduler, err := processes.NewScheduler(processes.SchedulerConfig{
		Scanner: scanner,
		Policy: func() processes.Policy {
			return processes.Policy{Processes: true, ProcessArguments: true}
		},
		Send: func(message processes.DetailMessage) bool {
			delivered = append(delivered, message)
			return true
		},
		Clock:       func() time.Time { return now },
		Now:         func() time.Time { return now },
		MinInterval: time.Second,
		NewTimer: func(_ time.Duration, fn func()) processes.Timer {
			pending = fn
			return manualTimer{}
		},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	defer scheduler.Stop()
	scheduler.Subscribe(processes.DetailSubscribe{
		Type: "detail.subscribe", SubscriptionID: "sub-trimmed",
		Kind: "processes", IntervalMS: 1000, LeaseMS: 60000,
	})
	if pending == nil {
		t.Fatal("subscription did not arm a timer")
	}
	pending()
	if len(delivered) != 1 {
		t.Fatalf("delivered %d detail messages, want 1", len(delivered))
	}
	result, ok := delivered[0].(processes.DetailResult)
	if !ok {
		t.Fatalf("delivered %T, want a detail result", delivered[0])
	}
	if !result.Snapshot.Truncated {
		t.Fatal("oversized table was not trimmed")
	}
	if len(result.Snapshot.Processes) == 0 || len(result.Snapshot.Processes) > protocol.MaxProcessRows {
		t.Fatalf("trimmed rows = %d, want 1..%d", len(result.Snapshot.Processes), protocol.MaxProcessRows)
	}
	converted := ToProtocolDetailResult(result)
	encoded, err := protocol.EncodeCollectorMessage(converted)
	if err != nil {
		t.Fatalf("encode trimmed detail: %v", err)
	}
	if len(encoded) > protocol.MaxDetailBytes {
		t.Fatalf("trimmed detail = %d bytes, limit %d", len(encoded), protocol.MaxDetailBytes)
	}
	return converted
}

func compactMessage(t *testing.T, message protocol.CollectorMessage) []byte {
	t.Helper()
	encoded, err := protocol.EncodeCollectorMessage(message)
	if err != nil {
		t.Fatalf("encode assembled fixture: %v", err)
	}
	out := append([]byte(nil), encoded...)
	return append(out, '\n')
}

func prettyMessage(t *testing.T, message protocol.CollectorMessage) []byte {
	t.Helper()
	encoded, err := protocol.EncodeCollectorMessage(message)
	if err != nil {
		t.Fatalf("encode assembled fixture: %v", err)
	}
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, encoded, "", "  "); err != nil {
		t.Fatalf("indent assembled fixture: %v", err)
	}
	buffer.WriteByte('\n')
	return buffer.Bytes()
}
