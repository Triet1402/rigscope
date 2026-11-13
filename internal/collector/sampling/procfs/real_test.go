package procfs

import (
	"os"
	"testing"
	"time"
)

// TestRealHost is the optional live-host smoke test. It is skipped unless
// RIGSCOPE_REAL_HW=1 because it reads the machine it runs on and waits 1s.
// It needs no root, specific hardware or GPU.
func TestRealHost(t *testing.T) {
	if os.Getenv("RIGSCOPE_REAL_HW") != "1" {
		t.Skip("set RIGSCOPE_REAL_HW=1 to read the live host")
	}
	sampler := NewSampler(SamplerOptions{})
	first := sampler.Sample()
	if first.Memory.Status != StatusOK {
		t.Fatalf("real memory unavailable: %+v", first.Memory)
	}
	if first.CPU.Status != StatusUnavailable {
		t.Fatalf("first CPU sample should need a second reading: %+v", first.CPU)
	}
	time.Sleep(time.Second)
	second := sampler.Sample()
	if second.CPU.Status != StatusOK {
		t.Fatalf("real cpu unavailable: %+v", second.CPU)
	}
	if len(second.CPU.PerCore) == 0 {
		t.Fatal("real cpu reported no per-core readings")
	}
	if second.CPU.UsagePercent == nil || *second.CPU.UsagePercent < 0 || *second.CPU.UsagePercent > 100 {
		t.Fatalf("real cpu usage out of range: %v", second.CPU.UsagePercent)
	}
	if second.Network.Status != StatusOK {
		t.Fatalf("real network unavailable: %+v", second.Network)
	}
	okInterfaces := 0
	for _, iface := range second.Network.Interfaces {
		if iface.Status == StatusOK {
			okInterfaces++
		}
	}
	if okInterfaces == 0 {
		t.Fatal("real network produced no interface rates")
	}
	if second.Disks.Status != StatusOK {
		t.Fatalf("real disks unavailable: %+v", second.Disks)
	}
	okDisks := 0
	for _, device := range second.Disks.Devices {
		if device.Status == StatusOK {
			okDisks++
		}
	}
	if okDisks == 0 {
		t.Fatal("real disks produced no device rates")
	}

	inventory := sampler.Inventory()
	if inventory.CPU.LogicalCores == 0 {
		t.Fatal("real inventory reported no logical cores")
	}
	if len(inventory.Filesystems) == 0 {
		t.Fatal("real inventory reported no filesystems")
	}
	if len(inventory.Network) == 0 {
		t.Fatal("real inventory reported no network interfaces")
	}
	if _, err := ReadBootID(NewRootSource()); err != nil {
		t.Fatalf("real boot id: %v", err)
	}
}
