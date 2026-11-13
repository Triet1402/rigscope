package synthetic

import (
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/metrics"
	"github.com/Triet1402/rigscope/internal/protocol"
)

func fixedOptions() Options {
	return Options{
		CollectorID: "mock-collector",
		RunID:       "mock-run",
		BootID:      "mock-boot",
		Sequence:    metrics.NewSequence(0),
		Now:         func() time.Time { return time.UnixMilli(1_700_000_000_000).UTC() },
		Rand:        func() float64 { return 0.25 },
	}
}

func TestSyntheticSamplerProducesValidRecordsAndInventory(t *testing.T) {
	sampler, err := NewSampler(fixedOptions())
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	first, err := sampler.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	second, err := sampler.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("sequences = %d/%d, want 1/2", first.Sequence, second.Sequence)
	}
	if first.CapturedAt != "2023-11-14T22:13:20.000Z" {
		t.Fatalf("capturedAt = %q", first.CapturedAt)
	}
	if _, ok := first.Payload.CPU.(protocol.CpuAvailable); !ok {
		t.Fatalf("cpu = %#v, want available", first.Payload.CPU)
	}
	if _, ok := first.Payload.GPUs.(protocol.GpusAvailable); !ok {
		t.Fatalf("gpus = %#v, want available", first.Payload.GPUs)
	}
	if _, err := protocol.EncodeCollectorMessage(protocol.SamplesMessage{
		Mode:    protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{first, second},
	}); err != nil {
		t.Fatalf("encode samples: %v", err)
	}

	inventory := sampler.Inventory()
	if _, err := protocol.EncodeCollectorMessage(protocol.InventoryMessage{
		CapturedAt: first.CapturedAt, Inventory: inventory,
	}); err != nil {
		t.Fatalf("encode inventory: %v", err)
	}
	if inventory.CPU.LogicalCores != 8 {
		t.Fatalf("logical cores = %d, want 8", inventory.CPU.LogicalCores)
	}
	if len(inventory.GPUs) != 1 || inventory.GPUs[0].ID != "gpu:SYNTHETIC-0" {
		t.Fatalf("gpus = %#v", inventory.GPUs)
	}
	if !strings.HasPrefix(inventory.GPUs[0].Name, "synthetic") ||
		inventory.CPU.Model == nil || !strings.HasPrefix(*inventory.CPU.Model, "synthetic") ||
		!strings.HasPrefix(inventory.Disks[0].Name, "synthetic") ||
		!strings.HasPrefix(inventory.Network[0].Name, "synthetic") {
		t.Fatalf("synthetic inventory is not clearly fake: %#v", inventory)
	}
	if sampler.InventoryVersion() != 1 {
		t.Fatalf("version = %d, want 1", sampler.InventoryVersion())
	}
	if len(sampler.Failures()) != 0 {
		t.Fatalf("failures = %#v, want none", sampler.Failures())
	}
}

func TestSyntheticSamplerIsDeterministicForAPinnedClock(t *testing.T) {
	first, err := NewSampler(fixedOptions())
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	second, err := NewSampler(fixedOptions())
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	a, err := first.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	b, err := second.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if a.Payload.CPU.(protocol.CpuAvailable).UsagePercent != b.Payload.CPU.(protocol.CpuAvailable).UsagePercent {
		t.Fatal("CPU usage differs for a pinned clock and random source")
	}
	if a.Payload.Memory.(protocol.MemoryAvailable).UsedBytes != b.Payload.Memory.(protocol.MemoryAvailable).UsedBytes {
		t.Fatal("memory usage differs for a pinned clock and random source")
	}
	if a.Payload.GPUs.(protocol.GpusAvailable).Devices[0].(protocol.GpuAvailable).UtilizationPercent == nil {
		t.Fatal("synthetic GPU utilization must be present")
	}
}

func TestSyntheticSamplerRequiresIdentityAndSequence(t *testing.T) {
	options := fixedOptions()
	options.CollectorID = ""
	if _, err := NewSampler(options); err == nil {
		t.Fatal("missing collector id accepted")
	}
	options = fixedOptions()
	options.Sequence = nil
	if _, err := NewSampler(options); err == nil {
		t.Fatal("missing sequence accepted")
	}
}
