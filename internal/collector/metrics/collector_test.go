package metrics

import (
	"errors"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/policy"
	"github.com/Triet1402/rigscope/internal/collector/sampling/gpu"
	"github.com/Triet1402/rigscope/internal/collector/sampling/procfs"
	"github.com/Triet1402/rigscope/internal/protocol"
)

func testIdentity() Identity {
	return Identity{CollectorID: "collector-fixture", RunID: "run-fixture", BootID: "boot-fixture"}
}

func TestCollectorAssemblesLinuxHostFixture(t *testing.T) {
	clock := newFixedClock()
	proc := fixtureSampler(t, "linux-host", clock)
	gpuSource := &fakeGPU{
		availability: gpu.AvailabilityYes,
		devices: []gpu.DeviceInfo{{
			UUID:             "GPU-fixture-0",
			Name:             "NVIDIA GeForce RTX 3090",
			PCIBusID:         protocol.Ptr("00000000:03:00.0"),
			MemoryTotalBytes: protocol.Ptr(int64(24 * 1024 * 1024 * 1024)),
		}},
		snapshot: &gpu.Snapshot{AtMonotonic: 0, Devices: []gpu.GpuReading{{
			ID:                 "gpu:GPU-fixture-0",
			Status:             gpu.StatusOK,
			UtilizationPercent: protocol.Ptr(25.0),
			MemoryUsedBytes:    protocol.Ptr(int64(1024)),
			MemoryTotalBytes:   protocol.Ptr(int64(24 * 1024 * 1024 * 1024)),
			TemperatureC:       protocol.Ptr(45.0),
		}}},
	}
	collector, err := NewCollector(Options{
		Identity: testIdentity(),
		Sequence: NewSequence(0),
		Clock:    clock,
		Proc:     proc,
		GPU:      gpuSource,
		GPUFS:    fixtureGPUSysFS(t, "nvidia-mixed"),
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	first, err := collector.Next()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if first.Sequence != 1 || first.CapturedAt != "2026-11-12T10:00:00.000Z" {
		t.Fatalf("envelope = %#v", first.RecordEnvelope)
	}
	if _, unavailable := first.Payload.CPU.(protocol.Unavailable); !unavailable {
		t.Fatalf("first cpu = %#v, want first-interval unavailable", first.Payload.CPU)
	}
	if _, ok := first.Payload.Memory.(protocol.MemoryAvailable); !ok {
		t.Fatalf("memory = %#v, want available", first.Payload.Memory)
	}
	disks, ok := first.Payload.Disks.(protocol.DisksAvailable)
	if !ok || len(disks.Filesystems) == 0 {
		t.Fatalf("disks = %#v, want fixture filesystems", first.Payload.Disks)
	}
	network, ok := first.Payload.Network.(protocol.NetworkAvailable)
	if !ok || len(network.Interfaces) == 0 {
		t.Fatalf("network = %#v, want fixture interfaces", first.Payload.Network)
	}
	gpus, ok := first.Payload.GPUs.(protocol.GpusAvailable)
	if !ok || len(gpus.Devices) != 2 {
		t.Fatalf("gpus = %#v, want AMD plus NVIDIA", first.Payload.GPUs)
	}
	if _, unavailable := gpus.Devices[0].(protocol.ResourceUnavailable); !unavailable {
		t.Fatalf("AMD reading = %#v, want unavailable DRM counters", gpus.Devices[0])
	}
	nvidia, ok := gpus.Devices[1].(protocol.GpuAvailable)
	if !ok || nvidia.ID != "gpu:GPU-fixture-0" || nvidia.MemoryUsedBytes == nil || *nvidia.MemoryUsedBytes != 1024 {
		t.Fatalf("NVIDIA reading = %#v", gpus.Devices[1])
	}
	if len(collector.Failures()) == 0 || len(collector.Failures()) > protocol.MaxCollectionFailures {
		t.Fatalf("failures = %#v, want a bounded non-empty list", collector.Failures())
	}

	inventory := collector.Inventory()
	if inventory.OS.Release == "" || inventory.OS.Platform != "linux" {
		t.Fatalf("os inventory = %#v", inventory.OS)
	}
	if inventory.CPU.LogicalCores < 1 {
		t.Fatalf("logical cores = %d", inventory.CPU.LogicalCores)
	}
	if len(inventory.GPUs) != 2 {
		t.Fatalf("inventory gpus = %#v, want AMD and NVIDIA", inventory.GPUs)
	}
	foundNvidia := false
	for _, entry := range inventory.GPUs {
		if entry.ID == "gpu:GPU-fixture-0" {
			foundNvidia = entry.Telemetry == protocol.TelemetrySupported &&
				entry.Vendor == protocol.GpuVendorNvidia &&
				entry.PciBusID != nil && *entry.PciBusID == "0000:03:00.0"
		}
	}
	if !foundNvidia {
		t.Fatalf("NVIDIA inventory entry missing or unjoined: %#v", inventory.GPUs)
	}
	if inventory.Capabilities.GPUs.Status != protocol.CapabilitySupported {
		t.Fatalf("gpu capability = %#v, want supported", inventory.Capabilities.GPUs)
	}
	if v := collector.InventoryVersion(); v == 0 {
		t.Fatal("inventory version must advance after the first build")
	}

	clock.advance(time.Second)
	second, err := collector.Next()
	if err != nil {
		t.Fatalf("second Next: %v", err)
	}
	if second.Sequence != 2 {
		t.Fatalf("second sequence = %d, want 2", second.Sequence)
	}
	if _, ok := second.Payload.CPU.(protocol.CpuAvailable); !ok {
		t.Fatalf("second cpu = %#v, want available", second.Payload.CPU)
	}

	// Both assembled messages must survive the validated wire round trip.
	if _, err := protocol.EncodeCollectorMessage(protocol.SamplesMessage{
		Mode:    protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{first, second},
	}); err != nil {
		t.Fatalf("encode samples: %v", err)
	}
	if _, err := protocol.EncodeCollectorMessage(protocol.InventoryMessage{
		CapturedAt: first.CapturedAt,
		Inventory:  inventory,
	}); err != nil {
		t.Fatalf("encode inventory: %v", err)
	}
	decoded := decodeSamples(t, first.Payload, first.CapturedAt)
	if decoded.CapturedAt != first.CapturedAt || decoded.Sequence != 1 {
		t.Fatalf("round trip envelope = %#v", decoded.RecordEnvelope)
	}
}

func TestCollectorAppliesSharingPolicyToMetricsAndInventory(t *testing.T) {
	clock := newFixedClock()
	proc := fixtureSampler(t, "minimal", clock)
	sharing := policy.Default()
	sharing.CPU = false
	sharing.NetworkAddresses = false
	collector, err := NewCollector(Options{
		Identity: testIdentity(),
		Sequence: NewSequence(0),
		Clock:    clock,
		Proc:     proc,
		GPUFS:    fixtureGPUSysFS(t, "nvidia-mixed"),
		Policy:   func() policy.Policy { return sharing },
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	record, err := collector.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	unavailable, ok := record.Payload.CPU.(protocol.Unavailable)
	if !ok || unavailable.Reason != policy.WithheldReason {
		t.Fatalf("cpu = %#v, want withheld", record.Payload.CPU)
	}
	if _, ok := record.Payload.Memory.(protocol.MemoryAvailable); !ok {
		t.Fatalf("memory = %#v, want shared", record.Payload.Memory)
	}
	inventory := collector.Inventory()
	if inventory.Capabilities.CPU.Status != protocol.CapabilityWithheld || inventory.CPU.LogicalCores != 0 {
		t.Fatalf("cpu inventory = %#v / %#v, want withheld", inventory.CPU, inventory.Capabilities.CPU)
	}
	for _, iface := range inventory.Network {
		if len(iface.Addresses) != 0 {
			t.Fatalf("addresses leaked: %#v", iface.Addresses)
		}
	}
}

func TestInventoryVersionTracksChangesOnly(t *testing.T) {
	clock := newFixedClock()
	proc := &fakeProc{inventory: emptyHostInventory()}
	sharing := policy.Default()
	collector, err := NewCollector(Options{
		Identity: testIdentity(),
		Sequence: NewSequence(0),
		Clock:    clock,
		Proc:     proc,
		GPUFS:    gpu.RootFS{FS: fstest.MapFS{}},
		Policy:   func() policy.Policy { return sharing },
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	if version := collector.InventoryVersion(); version != 0 {
		t.Fatalf("initial version = %d, want 0", version)
	}
	collector.Inventory()
	first := collector.InventoryVersion()
	if first != 1 {
		t.Fatalf("first version = %d, want 1", first)
	}
	collector.Inventory()
	if version := collector.InventoryVersion(); version != first {
		t.Fatalf("unchanged inventory bumped version: %d -> %d", first, version)
	}

	model := "Changed CPU"
	proc.inventory.CPU.Model = &model
	collector.Inventory()
	if version := collector.InventoryVersion(); version != first {
		t.Fatalf("a CPU model change is outside the signature but bumped version: %d -> %d", first, version)
	}

	proc.inventory.Disks = []procfs.DiskInventory{{ID: "disk:new", Name: "new"}}
	collector.Inventory()
	if version := collector.InventoryVersion(); version <= first {
		t.Fatalf("disk hotplug did not bump version: %d -> %d", first, version)
	}

	before := collector.InventoryVersion()
	sharing.CPU = false
	collector.Inventory()
	if version := collector.InventoryVersion(); version <= before {
		t.Fatalf("policy change did not bump version: %d -> %d", before, version)
	}
}

func TestCollectorReportsStaleGPUReadingsPerDevice(t *testing.T) {
	clock := newFixedClock()
	clock.advance(11 * time.Second)
	gpuSource := &fakeGPU{
		availability: gpu.AvailabilityYes,
		snapshot: &gpu.Snapshot{AtMonotonic: 0, Devices: []gpu.GpuReading{
			{ID: "gpu:GPU-a", Status: gpu.StatusOK, UtilizationPercent: protocol.Ptr(10.0)},
			{ID: "gpu:GPU-b", Status: gpu.StatusOK, UtilizationPercent: protocol.Ptr(20.0)},
		}},
	}
	collector, err := NewCollector(Options{
		Identity: testIdentity(),
		Sequence: NewSequence(0),
		Clock:    clock,
		Proc:     &fakeProc{readings: minimalReadings()},
		GPU:      gpuSource,
		GPUFS:    gpu.RootFS{FS: fstest.MapFS{}},
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	record, err := collector.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	gpus, ok := record.Payload.GPUs.(protocol.GpusAvailable)
	if !ok || len(gpus.Devices) != 2 {
		t.Fatalf("gpus = %#v, want two stale devices", record.Payload.GPUs)
	}
	for _, device := range gpus.Devices {
		unavailable, ok := device.(protocol.ResourceUnavailable)
		if !ok || unavailable.Reason != gpuStaleReason {
			t.Fatalf("device = %#v, want stale unavailable", device)
		}
	}
	staleFailures := 0
	for _, failure := range collector.Failures() {
		if failure.Reason == gpuStaleReason {
			staleFailures++
		}
	}
	if staleFailures != 2 {
		t.Fatalf("stale failures = %d, want 2 (%#v)", staleFailures, collector.Failures())
	}
}

func TestCollectorUnavailableHostProducesValidUnavailableFamilies(t *testing.T) {
	clock := newFixedClock()
	proc := NewProcFSSampler(clock, procfs.SamplerOptions{
		Source: procfs.NewFSSource(fstest.MapFS{}),
		StatFS: func(string) (procfs.StatFSSnapshot, error) {
			return procfs.StatFSSnapshot{}, errors.New("no statfs in fixture")
		},
		Addresses: func() (map[string][]procfs.InterfaceAddress, error) {
			return map[string][]procfs.InterfaceAddress{}, nil
		},
	})
	collector, err := NewCollector(Options{
		Identity: testIdentity(),
		Sequence: NewSequence(0),
		Clock:    clock,
		Proc:     proc,
		GPUFS:    gpu.RootFS{FS: fstest.MapFS{}},
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	record, err := collector.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	for name, value := range map[string]any{
		"cpu": record.Payload.CPU, "memory": record.Payload.Memory, "swap": record.Payload.Swap,
		"gpus": record.Payload.GPUs, "disks": record.Payload.Disks, "network": record.Payload.Network,
	} {
		if _, ok := value.(protocol.Unavailable); !ok {
			t.Fatalf("%s = %#v, want unavailable", name, value)
		}
	}
	inventory := collector.Inventory()
	if inventory.Capabilities.CPU.Status != protocol.CapabilityUnsupported {
		t.Fatalf("cpu capability = %#v, want unsupported", inventory.Capabilities.CPU)
	}
	if inventory.Capabilities.GPUs.Status != protocol.CapabilityUnsupported {
		t.Fatalf("gpu capability = %#v, want unsupported", inventory.Capabilities.GPUs)
	}
	if _, err := protocol.EncodeCollectorMessage(protocol.SamplesMessage{
		Mode:    protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{record},
	}); err != nil {
		t.Fatalf("encode samples: %v", err)
	}
	if _, err := protocol.EncodeCollectorMessage(protocol.InventoryMessage{
		CapturedAt: record.CapturedAt, Inventory: inventory,
	}); err != nil {
		t.Fatalf("encode inventory: %v", err)
	}
}

func TestCollectorRefusesExhaustedSequenceAndBrokenClock(t *testing.T) {
	clock := newFixedClock()
	newCollector := func(sequence Sequencer, clock Clock) *Collector {
		collector, err := NewCollector(Options{
			Identity: testIdentity(),
			Sequence: sequence,
			Clock:    clock,
			Proc:     &fakeProc{readings: minimalReadings()},
			GPUFS:    gpu.RootFS{FS: fstest.MapFS{}},
		})
		if err != nil {
			t.Fatalf("NewCollector: %v", err)
		}
		return collector
	}
	if _, err := newCollector(NewSequence(protocol.MaxSafeInteger), clock).Next(); !errors.Is(err, ErrSequenceExhausted) {
		t.Fatalf("exhausted sequence error = %v", err)
	}

	brokenClock := &fixedClock{now: time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC)}
	if _, err := newCollector(NewSequence(0), brokenClock).Next(); err == nil {
		t.Fatal("a clock outside the protocol window must be rejected")
	}
}

func TestNewCollectorValidatesRequiredWiring(t *testing.T) {
	if _, err := NewCollector(Options{Sequence: NewSequence(0), Proc: &fakeProc{}}); err == nil {
		t.Fatal("missing collector id accepted")
	}
	if _, err := NewCollector(Options{Identity: testIdentity(), Proc: &fakeProc{}}); err == nil {
		t.Fatal("missing sequence accepted")
	}
	if _, err := NewCollector(Options{Identity: testIdentity(), Sequence: NewSequence(0)}); err == nil {
		t.Fatal("missing proc sampler accepted")
	}
}

// emptyHostInventory is a stable no-resource inventory for change-detection
// tests.
func emptyHostInventory() procfs.HostInventory {
	capability := procfs.CapabilityState{Status: procfs.CapabilityUnsupported, Reason: "not in fixture"}
	return procfs.HostInventory{
		OS:     procfs.OSInventory{Platform: "linux", Release: "fixture", Arch: "amd64"},
		CPU:    procfs.CPUInventory{},
		Memory: procfs.MemoryInventory{},
		Capabilities: procfs.InventoryCapabilities{
			CPU: capability, Memory: capability, Swap: capability,
			Disks: capability, Network: capability, Processes: capability,
		},
	}
}

func TestCollectorFiltersCollectionFailuresBySharingPolicy(t *testing.T) {
	clock := newFixedClock()
	address := "192.0.2.10"
	proc := &fakeProc{
		readings: minimalReadings(),
		failures: []procfs.CollectionFailure{
			{Metric: "disks", ResourceID: protocol.Ptr("fs:/secret"), Reason: "mount reader failed for fs:/secret"},
			{Metric: "disks", ResourceID: protocol.Ptr("disk:sda"), Reason: "disk counters unavailable"},
			{Metric: "network", ResourceID: protocol.Ptr("net:eth0"), Reason: "probe to " + address + " timed out"},
			{Metric: "cpu", Reason: "cpu counters unavailable"},
		},
	}
	sharing := policy.Default()
	sharing.Disks = false
	sharing.NetworkAddresses = false
	collector, err := NewCollector(Options{
		Identity: testIdentity(),
		Sequence: NewSequence(0),
		Clock:    clock,
		Proc:     proc,
		GPUFS:    gpu.RootFS{FS: fstest.MapFS{}},
		Policy:   func() policy.Policy { return sharing },
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	if _, err := collector.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	failures := collector.Failures()
	for _, failure := range failures {
		if failure.Metric == "disks" {
			t.Fatalf("disk failure leaked past a withheld disk family: %#v", failure)
		}
		if failure.Metric == "network" && failure.Reason == "probe to "+address+" timed out" {
			t.Fatalf("network address leaked in a reason: %q", failure.Reason)
		}
	}
	foundCPU := false
	for _, failure := range failures {
		if failure.Metric == "cpu" {
			foundCPU = true
		}
	}
	if !foundCPU {
		t.Fatalf("shared-family failure was dropped: %#v", failures)
	}

	// The stored list is filtered at exposure, so re-widening the policy
	// restores the same tick's failures without re-sampling.
	sharing.Disks = true
	sharing.NetworkAddresses = true
	widened := collector.Failures()
	foundDisk, foundAddress := false, false
	for _, failure := range widened {
		if failure.Metric == "disks" {
			foundDisk = true
		}
		if failure.Metric == "network" && failure.Reason == "probe to "+address+" timed out" {
			foundAddress = true
		}
	}
	if !foundDisk || !foundAddress {
		t.Fatalf("widened policy failures = %#v, want the disk and address failures", widened)
	}
}

type panickingProc struct{}

func (panickingProc) Sample() procfs.Readings              { panic("host sample exploded") }
func (panickingProc) Inventory() procfs.HostInventory      { panic("inventory exploded") }
func (panickingProc) Failures() []procfs.CollectionFailure { panic("failures exploded") }

func TestCollectorRecoversFromAPanickingProcSampler(t *testing.T) {
	clock := newFixedClock()
	collector, err := NewCollector(Options{
		Identity: testIdentity(),
		Sequence: NewSequence(0),
		Clock:    clock,
		Proc:     panickingProc{},
		GPUFS:    gpu.RootFS{FS: fstest.MapFS{}},
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	record, err := collector.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	for name, value := range map[string]any{
		"cpu": record.Payload.CPU, "memory": record.Payload.Memory, "swap": record.Payload.Swap,
		"disks": record.Payload.Disks, "network": record.Payload.Network,
	} {
		unavailable, ok := value.(protocol.Unavailable)
		if !ok || unavailable.Reason != samplerPanicReason {
			t.Fatalf("%s = %#v, want a panic-unavailable reading", name, value)
		}
	}
	if _, err := protocol.EncodeCollectorMessage(protocol.SamplesMessage{
		Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{record},
	}); err != nil {
		t.Fatalf("encode panic-degraded record: %v", err)
	}

	failures := collector.Failures()
	for _, family := range []string{"cpu", "memory", "swap", "disks", "network"} {
		found := false
		for _, failure := range failures {
			if failure.Metric == family && failure.Reason == samplerPanicReason {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s has no collection failure after a panic: %#v", family, failures)
		}
	}

	if _, err := collector.Next(); err != nil {
		t.Fatalf("a second tick after a panic: %v", err)
	}
}

func TestCollectorRecoversFromAPanickingGPUSource(t *testing.T) {
	clock := newFixedClock()
	collector, err := NewCollector(Options{
		Identity: testIdentity(),
		Sequence: NewSequence(0),
		Clock:    clock,
		Proc:     &fakeProc{readings: minimalReadings()},
		GPUFS:    fixtureGPUSysFS(t, "amd-580"),
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	inventory := collector.Inventory()
	foundAMD := false
	for _, entry := range inventory.GPUs {
		if entry.Vendor == protocol.GpuVendorAMD {
			foundAMD = true
		}
	}
	if !foundAMD {
		t.Fatalf("fixture discovery found no AMD GPU: %#v", inventory.GPUs)
	}

	// Discovery succeeded; now the sysfs source panics on every read. The
	// fallback inventory keeps the AMD device, and sampling isolates it.
	collector.gpuFS = panickingSysFS{}
	record, err := collector.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	gpus, ok := record.Payload.GPUs.(protocol.Unavailable)
	if !ok || gpus.Reason != gpuPanicReason {
		t.Fatalf("gpus = %#v, want the family unavailable after a panic", record.Payload.GPUs)
	}
	if _, ok := record.Payload.CPU.(protocol.CpuAvailable); !ok {
		t.Fatalf("cpu = %#v, want the unaffected family intact", record.Payload.CPU)
	}
	if _, ok := record.Payload.Memory.(protocol.MemoryAvailable); !ok {
		t.Fatalf("memory = %#v, want the unaffected family intact", record.Payload.Memory)
	}
	foundFailure := false
	for _, failure := range collector.Failures() {
		if failure.Metric == "gpus" && failure.Reason == gpuPanicReason {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Fatalf("no GPU panic failure recorded: %#v", collector.Failures())
	}
	if _, err := collector.Next(); err != nil {
		t.Fatalf("a second tick after a GPU panic: %v", err)
	}
}

type panickingSysFS struct{}

func (panickingSysFS) ReadText(string) (string, bool)   { panic("sysfs read exploded") }
func (panickingSysFS) ReadDir(string) ([]string, error) { panic("sysfs readdir exploded") }

func TestCollectorFallsBackToAnHonestInventoryWhenASamplerPanics(t *testing.T) {
	clock := newFixedClock()
	collector, err := NewCollector(Options{
		Identity: testIdentity(),
		Sequence: NewSequence(0),
		Clock:    clock,
		Proc:     &fakeProc{readings: minimalReadings()},
		GPUFS:    panickingSysFS{},
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	inventory := collector.Inventory()
	if inventory.OS.Platform != "linux" {
		t.Fatalf("platform = %q, want the fallback platform", inventory.OS.Platform)
	}
	if inventory.Capabilities.CPU.Status != protocol.CapabilityUnsupported ||
		inventory.Capabilities.GPUs.Status != protocol.CapabilityUnsupported {
		t.Fatalf("capabilities = %#v, want all unsupported", inventory.Capabilities)
	}
	if _, err := protocol.EncodeCollectorMessage(protocol.InventoryMessage{
		CapturedAt: "2026-11-12T10:00:00.000Z", Inventory: inventory,
	}); err != nil {
		t.Fatalf("encode fallback inventory: %v", err)
	}
	if _, err := collector.Next(); err != nil {
		t.Fatalf("Next after a panicking inventory sampler: %v", err)
	}
}
