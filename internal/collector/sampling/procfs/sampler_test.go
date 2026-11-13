package procfs

import (
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

func requireFloat(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("value = nil, want %v", want)
	}
	if *got != want {
		t.Fatalf("value = %v, want %v", *got, want)
	}
}

func requireInt(t *testing.T, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("value = nil, want %v", want)
	}
	if *got != want {
		t.Fatalf("value = %v, want %v", *got, want)
	}
}

func TestSamplerFirstIntervalThenRates(t *testing.T) {
	h := newHarness(t)
	first := h.sampler.Sample()

	if first.CPU.Status != StatusUnavailable {
		t.Fatalf("first cpu = %+v", first.CPU)
	}
	requireReason(t, first.CPU.Reason, "first interval")
	if first.Memory.Status != StatusOK {
		t.Fatalf("first memory = %+v", first.Memory)
	}
	requireInt(t, first.Memory.UsedBytes, (1000-400)*1024)
	requireInt(t, first.Memory.TotalBytes, 1000*1024)
	if first.Memory.PressureSomePercent != nil || first.Memory.PressureFullPercent != nil {
		t.Errorf("memory pressure = %v/%v, want nil", first.Memory.PressureSomePercent, first.Memory.PressureFullPercent)
	}
	if first.Swap.Status != StatusOK {
		t.Fatalf("first swap = %+v", first.Swap)
	}
	requireInt(t, first.Swap.UsedBytes, (100-60)*1024)
	requireInt(t, first.Swap.TotalBytes, 100*1024)

	if first.Disks.Status != StatusOK {
		t.Fatalf("first disks = %+v", first.Disks)
	}
	if len(first.Disks.Filesystems) != 1 || first.Disks.Filesystems[0].Status != StatusOK {
		t.Fatalf("first filesystems = %+v", first.Disks.Filesystems)
	}
	if len(first.Disks.Devices) != 1 || first.Disks.Devices[0].Status != StatusUnavailable {
		t.Fatalf("first devices = %+v", first.Disks.Devices)
	}
	requireReason(t, first.Disks.Devices[0].Reason, "first interval")

	if first.Network.Status != StatusOK || len(first.Network.Interfaces) != 1 {
		t.Fatalf("first network = %+v", first.Network)
	}
	if first.Network.Interfaces[0].Status != StatusUnavailable {
		t.Fatalf("first interface = %+v", first.Network.Interfaces[0])
	}
	requireReason(t, first.Network.Interfaces[0].Reason, "first interval")

	failures := h.sampler.Failures()
	if len(failures) == 0 || len(failures) > 32 {
		t.Fatalf("failures = %d", len(failures))
	}
	for _, failure := range failures {
		if len(failure.Reason) > 256 {
			t.Fatalf("failure reason too long: %q", failure.Reason)
		}
	}

	h.advance(1000)
	h.set("/proc/stat", cpuLine(200, 200, 110, 90, 90, 110))
	h.set("/proc/diskstats", diskstatsText([][3]string{{"sda", "200", "100"}}))
	h.set("/proc/net/dev", netDevText([][3]string{{"lo", "2000", "2500"}}))
	second := h.sampler.Sample()

	if second.CPU.Status != StatusOK {
		t.Fatalf("second cpu = %+v", second.CPU)
	}
	requireFloat(t, second.CPU.UsagePercent, 50)
	if len(second.CPU.PerCore) != 2 {
		t.Fatalf("perCore = %v", second.CPU.PerCore)
	}
	requireFloat(t, second.CPU.PerCore[0], 60)
	requireFloat(t, second.CPU.PerCore[1], 40)
	if second.CPU.TemperatureC != nil || second.CPU.PowerWatts != nil || second.CPU.PressureSomePercent != nil {
		t.Errorf("optional sensors = %v/%v/%v, want nil", second.CPU.TemperatureC, second.CPU.PowerWatts, second.CPU.PressureSomePercent)
	}
	requireFloat(t, second.CPU.IOwaitPercent, 0)

	if second.Disks.Status != StatusOK || len(second.Disks.Devices) != 1 {
		t.Fatalf("second disks = %+v", second.Disks)
	}
	device := second.Disks.Devices[0]
	if device.Status != StatusOK || device.ID != "disk:sda" {
		t.Fatalf("second device = %+v", device)
	}
	requireInt(t, device.ReadBytesPerSec, 51_200)
	requireInt(t, device.WriteBytesPerSec, 25_600)
	if device.ReadLatencyMs != nil || device.WriteLatencyMs != nil {
		t.Errorf("latency = %v/%v, want nil (no completed operations)", device.ReadLatencyMs, device.WriteLatencyMs)
	}
	requireFloat(t, device.IOUtilizationPercent, 0)

	if second.Network.Status != StatusOK || len(second.Network.Interfaces) != 1 {
		t.Fatalf("second network = %+v", second.Network)
	}
	iface := second.Network.Interfaces[0]
	if iface.Status != StatusOK || iface.ID != "net:lo" {
		t.Fatalf("second interface = %+v", iface)
	}
	requireInt(t, iface.RXBytesPerSec, 1_000)
	requireInt(t, iface.TXBytesPerSec, 500)

	if failures := h.sampler.Failures(); len(failures) != 0 {
		t.Fatalf("second tick failures = %+v, want none", failures)
	}
}

func TestSamplerRegressedCoreBecomesNull(t *testing.T) {
	h := newHarness(t)
	h.sampler.Sample()
	h.advance(1000)
	h.set("/proc/stat", cpuLine(200, 200, 110, 90, 90, 110))
	second := h.sampler.Sample()
	requireFloat(t, second.CPU.PerCore[0], 60)
	requireFloat(t, second.CPU.PerCore[1], 40)

	h.advance(1000)
	// core1's cumulative busy counter decreased; the total still advanced by 50.
	h.set("/proc/stat", cpuLine(250, 250, 160, 140, 50, 110))
	third := h.sampler.Sample()
	if third.CPU.Status != StatusOK {
		t.Fatalf("third cpu = %+v", third.CPU)
	}
	requireFloat(t, third.CPU.UsagePercent, 25)
	if len(third.CPU.PerCore) != 2 || third.CPU.PerCore[1] != nil {
		t.Fatalf("perCore = %v, want [50, nil]", third.CPU.PerCore)
	}
	requireFloat(t, third.CPU.PerCore[0], 50)
}

func TestSamplerCPUResetAndCoreCountChange(t *testing.T) {
	h := newHarness(t)
	h.sampler.Sample()
	h.advance(1000)
	h.set("/proc/stat", cpuLine(200, 200, 110, 90, 90, 110))
	requireFloat(t, h.sampler.Sample().CPU.UsagePercent, 50)

	h.advance(1000)
	h.set("/proc/stat", cpuLine(100, 100, 50, 50, 50, 50))
	regressed := h.sampler.Sample()
	if regressed.CPU.Status != StatusUnavailable {
		t.Fatalf("regressed cpu = %+v", regressed.CPU)
	}
	requireReason(t, regressed.CPU.Reason, "decreased")

	h.advance(1000)
	h.set("/proc/stat", cpuLine(200, 200, 110, 90, 90, 110))
	requireFloat(t, h.sampler.Sample().CPU.UsagePercent, 50)

	h.advance(1000)
	h.set("/proc/stat", "cpu  "+procStatLine(statValues{user: 220, idle: 220})+"\n"+
		"cpu0 "+procStatLine(statValues{user: 120, idle: 100})+"\n")
	changed := h.sampler.Sample()
	if changed.CPU.Status != StatusUnavailable {
		t.Fatalf("changed cpu = %+v", changed.CPU)
	}
	requireReason(t, changed.CPU.Reason, "core count changed")

	h.advance(1000)
	h.set("/proc/stat", cpuLine(300, 300, 160, 140, 140, 160))
	restored := h.sampler.Sample()
	if restored.CPU.Status != StatusUnavailable {
		t.Fatalf("restored cpu = %+v", restored.CPU)
	}
	requireReason(t, restored.CPU.Reason, "core count changed")

	h.advance(1000)
	h.set("/proc/stat", cpuLine(400, 400, 210, 190, 190, 210))
	recovered := h.sampler.Sample()
	if recovered.CPU.Status != StatusOK {
		t.Fatalf("recovered cpu = %+v", recovered.CPU)
	}
	requireFloat(t, recovered.CPU.UsagePercent, 50)
}

func TestSamplerDiskResetOnlyAffectsThatDevice(t *testing.T) {
	h := newHarness(t)
	h.dirs["/sys/block"] = []string{"sda", "sdb"}
	h.set("/sys/block/sdb/device/model", "Fixture Disk 2\n")
	h.set("/sys/block/sdb/queue/rotational", "1\n")
	h.set("/sys/block/sdb/size", "4000\n")
	h.set("/proc/diskstats", diskstatsText([][3]string{{"sda", "100", "50"}, {"sdb", "100", "50"}}))
	h.sampler.Sample()

	h.advance(1000)
	h.set("/proc/diskstats", diskstatsText([][3]string{{"sda", "300", "150"}, {"sdb", "300", "150"}}))
	second := h.sampler.Sample()
	for _, device := range second.Disks.Devices {
		if device.Status != StatusOK {
			t.Fatalf("second devices = %+v", second.Disks.Devices)
		}
	}

	h.advance(1000)
	h.set("/proc/diskstats", diskstatsText([][3]string{{"sda", "10", "5"}, {"sdb", "400", "200"}}))
	reset := h.sampler.Sample()
	if len(reset.Disks.Devices) != 2 {
		t.Fatalf("reset devices = %+v", reset.Disks.Devices)
	}
	if reset.Disks.Devices[0].Status != StatusUnavailable {
		t.Fatalf("reset sda = %+v", reset.Disks.Devices[0])
	}
	requireReason(t, reset.Disks.Devices[0].Reason, "decreased")
	if reset.Disks.Devices[1].Status != StatusOK {
		t.Fatalf("sdb should survive sda's reset: %+v", reset.Disks.Devices[1])
	}

	h.advance(1000)
	h.set("/proc/diskstats", diskstatsText([][3]string{{"sda", "20", "10"}, {"sdb", "500", "250"}}))
	resumed := h.sampler.Sample()
	requireInt(t, resumed.Disks.Devices[0].ReadBytesPerSec, 5_120)
	requireInt(t, resumed.Disks.Devices[0].WriteBytesPerSec, 2_560)
	requireInt(t, resumed.Disks.Devices[1].ReadBytesPerSec, 51_200)
}

func TestSamplerMemoryAndSwapFailIndependently(t *testing.T) {
	h := newHarness(t)
	h.set("/proc/meminfo", meminfoText(map[string]*int{"MemAvailable": nil}))
	first := h.sampler.Sample()
	if first.Memory.Status != StatusUnavailable {
		t.Fatalf("memory = %+v", first.Memory)
	}
	if first.Swap.Status != StatusOK {
		t.Fatalf("swap = %+v", first.Swap)
	}

	h.set("/proc/meminfo", meminfoText(map[string]*int{"SwapFree": intp(200)}))
	second := h.sampler.Sample()
	if second.Memory.Status != StatusOK {
		t.Fatalf("memory = %+v", second.Memory)
	}
	if second.Swap.Status != StatusUnavailable {
		t.Fatalf("swap = %+v", second.Swap)
	}
	requireReason(t, second.Swap.Reason, "inconsistent")

	h.set("/proc/meminfo", meminfoText(map[string]*int{"SwapTotal": intp(0), "SwapFree": intp(0)}))
	third := h.sampler.Sample()
	if third.Swap.Status != StatusOK {
		t.Fatalf("swap = %+v", third.Swap)
	}
	requireInt(t, third.Swap.UsedBytes, 0)
	requireInt(t, third.Swap.TotalBytes, 0)

	h.delete("/proc/meminfo")
	fourth := h.sampler.Sample()
	if fourth.Memory.Status != StatusUnavailable || fourth.Swap.Status != StatusUnavailable {
		t.Fatalf("missing meminfo = %+v / %+v", fourth.Memory, fourth.Swap)
	}
}

func TestSamplerSwapRequiresExplicitCounters(t *testing.T) {
	h := newHarness(t)

	// A missing SwapTotal must not look like a host without swap.
	h.set("/proc/meminfo", meminfoText(map[string]*int{"SwapTotal": nil}))
	readings := h.sampler.Sample()
	if readings.Swap.Status != StatusUnavailable {
		t.Fatalf("missing SwapTotal = %+v, want unavailable", readings.Swap)
	}
	requireReason(t, readings.Swap.Reason, "swap total counters unavailable")
	if readings.Swap.UsedBytes != nil || readings.Swap.TotalBytes != nil {
		t.Fatalf("missing SwapTotal values = %v/%v, want nil", readings.Swap.UsedBytes, readings.Swap.TotalBytes)
	}

	// A non-empty swap area without SwapFree must not report 100% used.
	h.set("/proc/meminfo", meminfoText(map[string]*int{"SwapFree": nil}))
	readings = h.sampler.Sample()
	if readings.Swap.Status != StatusUnavailable {
		t.Fatalf("missing SwapFree = %+v, want unavailable", readings.Swap)
	}
	requireReason(t, readings.Swap.Reason, "swap free counters unavailable")
	if readings.Swap.UsedBytes != nil || readings.Swap.TotalBytes != nil {
		t.Fatalf("missing SwapFree values = %v/%v, want nil", readings.Swap.UsedBytes, readings.Swap.TotalBytes)
	}

	// Only an explicit zero total means "no swap", and it is an honest 0/0.
	h.set("/proc/meminfo", meminfoText(map[string]*int{"SwapTotal": intp(0), "SwapFree": nil}))
	readings = h.sampler.Sample()
	if readings.Swap.Status != StatusOK {
		t.Fatalf("explicit zero swap = %+v", readings.Swap)
	}
	requireInt(t, readings.Swap.UsedBytes, 0)
	requireInt(t, readings.Swap.TotalBytes, 0)

	// A free counter that contradicts a zero total is inconsistent, not 0/0.
	h.set("/proc/meminfo", meminfoText(map[string]*int{"SwapTotal": intp(0), "SwapFree": intp(60)}))
	readings = h.sampler.Sample()
	if readings.Swap.Status != StatusUnavailable {
		t.Fatalf("zero total with free swap = %+v, want unavailable", readings.Swap)
	}
	requireReason(t, readings.Swap.Reason, "inconsistent")
}

func TestSamplerSparseCoreIndexesKeepPositions(t *testing.T) {
	h := newHarness(t)
	h.set("/proc/stat", "cpu "+procStatLine(statValues{user: 100, idle: 100})+"\n"+
		"cpu0 "+procStatLine(statValues{user: 50, idle: 50})+"\n"+
		"cpu2 "+procStatLine(statValues{user: 50, idle: 50})+"\n")
	h.sampler.Sample()

	h.advance(1000)
	h.set("/proc/stat", "cpu "+procStatLine(statValues{user: 200, idle: 200})+"\n"+
		"cpu0 "+procStatLine(statValues{user: 100, idle: 100})+"\n"+
		"cpu2 "+procStatLine(statValues{user: 100, idle: 100})+"\n")
	readings := h.sampler.Sample()

	if len(readings.CPU.PerCore) != 3 {
		t.Fatalf("perCore length = %d, want 3 with index 1 as a hole", len(readings.CPU.PerCore))
	}
	requireFloat(t, readings.CPU.PerCore[0], 50)
	if readings.CPU.PerCore[1] != nil {
		t.Fatalf("perCore[1] = %v, want nil for a missing cpu1", *readings.CPU.PerCore[1])
	}
	requireFloat(t, readings.CPU.PerCore[2], 50)
}

func TestSamplerCoreIndexesBeyondContractStayAbsent(t *testing.T) {
	h := newHarness(t)
	line := func(busy, idle int) string {
		return "cpu " + procStatLine(statValues{user: busy, idle: idle}) + "\n" +
			"cpu0 " + procStatLine(statValues{user: busy / 2, idle: idle / 2}) + "\n" +
			"cpu1500 " + procStatLine(statValues{user: busy / 2, idle: idle / 2}) + "\n"
	}
	h.set("/proc/stat", line(100, 100))
	h.sampler.Sample()
	h.advance(1000)
	h.set("/proc/stat", line(200, 200))
	readings := h.sampler.Sample()

	if len(readings.CPU.PerCore) != protocol.MaxCores {
		t.Fatalf("perCore length = %d, want the contract cap %d", len(readings.CPU.PerCore), protocol.MaxCores)
	}
	requireFloat(t, readings.CPU.PerCore[0], 50)
	for index := 1; index < len(readings.CPU.PerCore); index++ {
		if readings.CPU.PerCore[index] != nil {
			t.Fatalf("perCore[%d] = %v, want nil", index, *readings.CPU.PerCore[index])
		}
	}
	found := false
	for _, failure := range h.sampler.Failures() {
		if failure.Reason == "cpu core indexes exceed the contract limit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("failures = %+v, want the core-index limit reason", h.sampler.Failures())
	}
}

func TestSamplerClockAndProcfsFailures(t *testing.T) {
	h := newHarness(t)
	h.clockOK = false
	noClock := h.sampler.Sample()
	if noClock.CPU.Status != StatusUnavailable {
		t.Fatalf("no clock cpu = %+v", noClock.CPU)
	}
	requireReason(t, noClock.CPU.Reason, "monotonic elapsed clock unavailable")
	if noClock.Network.Status != StatusUnavailable {
		t.Fatalf("no clock network = %+v", noClock.Network)
	}
	requireReason(t, noClock.Network.Reason, "monotonic clock unavailable")
	if noClock.Disks.Status != StatusOK || len(noClock.Disks.Devices) != 1 {
		t.Fatalf("no clock disks = %+v", noClock.Disks)
	}
	requireReason(t, noClock.Disks.Devices[0].Reason, "disk monotonic clock unavailable")

	// Recovering the clock starts from a fresh CPU baseline.
	h.clockOK = true
	h.advance(1000)
	pending := h.sampler.Sample()
	requireReason(t, pending.CPU.Reason, "first interval")

	h.delete("/proc/stat")
	missing := h.sampler.Sample()
	if missing.CPU.Status != StatusUnavailable {
		t.Fatalf("missing stat cpu = %+v", missing.CPU)
	}
	requireReason(t, missing.CPU.Reason, "cpu counters unavailable")
	if missing.Memory.Status != StatusOK {
		t.Fatalf("missing stat must not hide memory: %+v", missing.Memory)
	}
}

func TestSamplerMinimalFixture(t *testing.T) {
	src := fixtureSource(t, "minimal")
	clock := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	statfs := func(path string) (StatFSSnapshot, error) {
		if path == "/" {
			return StatFSSnapshot{Bsize: 4096, Blocks: 1000, Bfree: 400}, nil
		}
		return StatFSSnapshot{Bsize: 4096, Blocks: 2000, Bfree: 500}, nil
	}
	host := HostInfo{Platform: "linux", Release: "fixture-kernel", Arch: "x86_64", CPUCount: 1}
	sampler := NewSampler(SamplerOptions{
		Source: src, StatFS: statfs, Host: &host,
		Monotonic: func() (time.Time, bool) { return clock, true },
	})

	first := sampler.Sample()
	if first.CPU.Status != StatusUnavailable {
		t.Fatalf("first cpu = %+v", first.CPU)
	}
	requireReason(t, first.CPU.Reason, "first interval")
	if first.Memory.Status != StatusOK {
		t.Fatalf("memory = %+v", first.Memory)
	}
	requireInt(t, first.Memory.TotalBytes, 8192000*1024)
	if first.Swap.Status != StatusUnavailable {
		t.Fatalf("swap without SwapTotal = %+v, want unavailable", first.Swap)
	}
	requireReason(t, first.Swap.Reason, "swap total counters unavailable")
	if len(first.Disks.Filesystems) != 2 {
		t.Fatalf("filesystems = %+v", first.Disks.Filesystems)
	}
	if first.Disks.Devices[0].Status != StatusUnavailable {
		t.Fatalf("first disk = %+v", first.Disks.Devices[0])
	}
	if first.Network.Interfaces[0].Status != StatusUnavailable {
		t.Fatalf("first interface = %+v", first.Network.Interfaces[0])
	}

	clock = clock.Add(time.Second)
	second := sampler.Sample()
	if second.CPU.Status != StatusOK {
		t.Fatalf("second cpu = %+v", second.CPU)
	}
	requireFloat(t, second.CPU.UsagePercent, 0)
	if len(second.CPU.PerCore) != 1 {
		t.Fatalf("perCore = %v", second.CPU.PerCore)
	}
	requireFloat(t, second.CPU.PerCore[0], 0)
	if second.CPU.TemperatureC != nil || second.CPU.PowerWatts != nil || second.CPU.PressureSomePercent != nil {
		t.Errorf("optional sensors = %v/%v/%v, want nil", second.CPU.TemperatureC, second.CPU.PowerWatts, second.CPU.PressureSomePercent)
	}
	requireInt(t, second.Disks.Devices[0].ReadBytesPerSec, 0)
	requireInt(t, second.Network.Interfaces[0].RXBytesPerSec, 0)
}

func TestSamplerCapturedHost(t *testing.T) {
	src := fixtureSource(t, "linux-host")
	clock := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	statfs := func(string) (StatFSSnapshot, error) {
		return StatFSSnapshot{Bsize: 4096, Blocks: 1000, Bfree: 400, Files: 1000, Ffree: 750}, nil
	}
	host := capturedHost()
	sampler := NewSampler(SamplerOptions{
		Source: src, StatFS: statfs, Host: &host,
		Monotonic: func() (time.Time, bool) { return clock, true },
	})

	first := sampler.Sample()
	if first.CPU.Status != StatusUnavailable {
		t.Fatalf("first cpu = %+v", first.CPU)
	}
	requireReason(t, first.CPU.Reason, "first interval")
	if first.Memory.Status != StatusOK || first.Swap.Status != StatusOK {
		t.Fatalf("memory/swap = %+v / %+v", first.Memory, first.Swap)
	}
	if first.Disks.Status != StatusOK || len(first.Disks.Filesystems) != 3 || len(first.Disks.Devices) != 3 {
		t.Fatalf("first disks = %+v", first.Disks)
	}
	for _, device := range first.Disks.Devices {
		requireReason(t, device.Reason, "first interval")
	}

	clock = clock.Add(time.Second)
	second := sampler.Sample()
	if second.CPU.Status != StatusOK {
		t.Fatalf("second cpu = %+v", second.CPU)
	}
	requireFloat(t, second.CPU.UsagePercent, 0)
	if len(second.CPU.PerCore) != 48 {
		t.Fatalf("perCore length = %d, want 48", len(second.CPU.PerCore))
	}
	requireFloat(t, second.CPU.IOwaitPercent, 0)
	requireFloat(t, second.CPU.PressureSomePercent, 0)
	requireFloat(t, second.CPU.TemperatureC, 31.88)
	// RAPL starts one tick after CPU rates: power needs a previous energy
	// counter, and the first tick never reaches optional sensors.
	if second.CPU.PowerWatts != nil {
		t.Fatalf("second tick power = %v, want nil while warming up", *second.CPU.PowerWatts)
	}
	for _, device := range second.Disks.Devices {
		requireOK(t, device.Status)
		requireInt(t, device.ReadBytesPerSec, 0)
	}
	for _, iface := range second.Network.Interfaces {
		requireOK(t, iface.Status)
		requireInt(t, iface.RXBytesPerSec, 0)
	}
	if failures := sampler.Failures(); len(failures) != 0 {
		t.Fatalf("captured second tick failures = %+v", failures)
	}

	clock = clock.Add(time.Second)
	third := sampler.Sample()
	requireOK(t, third.CPU.Status)
	requireFloat(t, third.CPU.PowerWatts, 0)
}

func TestSamplerHotplugAndInventory(t *testing.T) {
	h := newHarness(t)
	inventory := h.sampler.Inventory()
	if len(inventory.Disks) != 1 {
		t.Fatalf("inventory disks = %+v", inventory.Disks)
	}

	h.dirs["/sys/block"] = []string{"sda", "sdb"}
	h.set("/sys/block/sdb/device/model", "Fixture Disk 2\n")
	h.set("/sys/block/sdb/queue/rotational", "1\n")
	h.set("/sys/block/sdb/size", "4000\n")
	h.set("/proc/diskstats", diskstatsText([][3]string{{"sda", "100", "50"}, {"sdb", "100", "50"}}))

	inventory = h.sampler.Inventory()
	if len(inventory.Disks) != 2 {
		t.Fatalf("hotplugged inventory disks = %+v", inventory.Disks)
	}
	h.sampler.Sample()
	h.advance(1000)
	h.set("/proc/diskstats", diskstatsText([][3]string{{"sda", "200", "100"}, {"sdb", "200", "100"}}))
	second := h.sampler.Sample()
	if len(second.Disks.Devices) != 2 {
		t.Fatalf("hotplugged devices = %+v", second.Disks.Devices)
	}
	if second.Disks.Devices[0].Status != StatusOK || second.Disks.Devices[1].Status != StatusOK {
		t.Fatalf("hotplugged device status = %+v", second.Disks.Devices)
	}

	h.dirs["/sys/block"] = []string{"sda"}
	h.delete("/sys/block/sdb/device/model")
	h.delete("/sys/block/sdb/queue/rotational")
	h.delete("/sys/block/sdb/size")
	h.set("/proc/diskstats", diskstatsText([][3]string{{"sda", "300", "150"}}))
	h.advance(1000)
	third := h.sampler.Sample()
	if len(third.Disks.Devices) != 1 || third.Disks.Devices[0].Status != StatusOK {
		t.Fatalf("removed device = %+v", third.Disks.Devices)
	}
}

func TestSamplerDiskLatencyAndUtilization(t *testing.T) {
	h := newHarness(t)
	h.set("/proc/diskstats", diskstatsValues([]diskStatValues{
		{name: "sda", readSectors: 100, writeSectors: 50, reads: 10, readTimeMs: 100, writes: 10, writeTimeMs: 200, ioTimeMs: 100},
	}))
	h.sampler.Sample()

	h.advance(1000)
	h.set("/proc/diskstats", diskstatsValues([]diskStatValues{
		{name: "sda", readSectors: 200, writeSectors: 100, reads: 18, readTimeMs: 180, writes: 14, writeTimeMs: 220, ioTimeMs: 600},
	}))
	second := h.sampler.Sample()
	device := second.Disks.Devices[0]
	requireOK(t, device.Status)
	requireInt(t, device.ReadBytesPerSec, 51_200)
	requireInt(t, device.WriteBytesPerSec, 25_600)
	requireFloat(t, device.ReadLatencyMs, 10)
	requireFloat(t, device.WriteLatencyMs, 5)
	requireFloat(t, device.IOUtilizationPercent, 50)

	// A reset of the optional operation counters invalidates only latency and
	// utilization; throughput stays useful and the baseline recovers.
	h.advance(1000)
	h.set("/proc/diskstats", diskstatsValues([]diskStatValues{
		{name: "sda", readSectors: 250, writeSectors: 120, reads: 5, readTimeMs: 50, writes: 4, writeTimeMs: 20, ioTimeMs: 100},
	}))
	reset := h.sampler.Sample().Disks.Devices[0]
	requireOK(t, reset.Status)
	requireInt(t, reset.ReadBytesPerSec, 25_600)
	requireInt(t, reset.WriteBytesPerSec, 10_240)
	if reset.ReadLatencyMs != nil || reset.WriteLatencyMs != nil || reset.IOUtilizationPercent != nil {
		t.Fatalf("reset optional counters = %v/%v/%v, want nil", reset.ReadLatencyMs, reset.WriteLatencyMs, reset.IOUtilizationPercent)
	}

	h.advance(1000)
	h.set("/proc/diskstats", diskstatsValues([]diskStatValues{
		{name: "sda", readSectors: 300, writeSectors: 140, reads: 7, readTimeMs: 70, writes: 6, writeTimeMs: 30, ioTimeMs: 300},
	}))
	recovered := h.sampler.Sample().Disks.Devices[0]
	requireOK(t, recovered.Status)
	requireFloat(t, recovered.ReadLatencyMs, 10)
	requireFloat(t, recovered.WriteLatencyMs, 5)
	requireFloat(t, recovered.IOUtilizationPercent, 20)
}

func TestSamplerIowaitInconsistencyLeavesTheFieldUnknown(t *testing.T) {
	h := newHarness(t)
	h.set("/proc/stat", "cpu "+procStatLine(statValues{user: 100, idle: 80, iowait: 20})+"\n"+
		"cpu0 "+procStatLine(statValues{user: 50, idle: 40, iowait: 10})+"\n"+
		"cpu1 "+procStatLine(statValues{user: 50, idle: 40, iowait: 10})+"\n")
	h.sampler.Sample()
	h.advance(1000)
	// Busy advanced but iowait went backwards: usage stays usable, iowait is
	// unknown and the failure is reported.
	h.set("/proc/stat", "cpu "+procStatLine(statValues{user: 200, idle: 210, iowait: 5})+"\n"+
		"cpu0 "+procStatLine(statValues{user: 110, idle: 90})+"\n"+
		"cpu1 "+procStatLine(statValues{user: 90, idle: 110})+"\n")
	readings := h.sampler.Sample()
	requireOK(t, readings.CPU.Status)
	if readings.CPU.IOwaitPercent != nil {
		t.Fatalf("iowait = %v, want nil", *readings.CPU.IOwaitPercent)
	}
	found := false
	for _, failure := range h.sampler.Failures() {
		if failure.Reason == "cpu iowait counters inconsistent" {
			found = true
		}
	}
	if !found {
		t.Fatalf("failures = %+v, want the iowait reason", h.sampler.Failures())
	}
}

func TestMeanLatencyMsBounds(t *testing.T) {
	if got := meanLatencyMs(nil, nil); got != nil {
		t.Fatalf("missing counters = %v, want nil", got)
	}
	if got := meanLatencyMs(u64p(0), u64p(100)); got != nil {
		t.Fatalf("zero operations = %v, want nil", got)
	}
	if got := meanLatencyMs(u64p(2), u64p(20)); got == nil || *got != 10 {
		t.Fatalf("latency = %v, want 10", got)
	}
	if got := meanLatencyMs(u64p(1), u64p(uint64(maxLatencyMs)+1)); got != nil {
		t.Fatalf("latency above the contract ceiling = %v, want nil", got)
	}
}

func TestSamplerFailureListIsBounded(t *testing.T) {
	h := newHarness(t)
	children := make([]string, 70)
	for i := range children {
		children[i] = "vd" + itoaTest(i)
	}
	h.dirs["/sys/block"] = children
	h.set("/proc/diskstats", "")
	readings := h.sampler.Sample()
	if len(readings.Disks.Devices) != protocol.MaxDisks {
		t.Fatalf("devices = %d, want %d", len(readings.Disks.Devices), protocol.MaxDisks)
	}
	failures := h.sampler.Failures()
	if len(failures) != protocol.MaxCollectionFailures {
		t.Fatalf("failures = %d, want %d", len(failures), protocol.MaxCollectionFailures)
	}
}

func TestSamplerManyInterfaces(t *testing.T) {
	src := fixtureSource(t, "many")
	clock := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	host := HostInfo{Platform: "linux", Release: "fixture-kernel", Arch: "x86_64", CPUCount: 1}
	statfs := func(string) (StatFSSnapshot, error) {
		return StatFSSnapshot{Bsize: 4096, Blocks: 1000, Bfree: 400}, nil
	}
	sampler := NewSampler(SamplerOptions{
		Source: src, StatFS: statfs, Host: &host,
		Addresses: func() (map[string][]InterfaceAddress, error) { return map[string][]InterfaceAddress{}, nil },
		Monotonic: func() (time.Time, bool) { return clock, true },
	})
	readings := sampler.Sample()
	if len(readings.Network.Interfaces) != protocol.MaxInterfaces {
		t.Fatalf("sampled interfaces = %d, want the inventory's %d-interface selection", len(readings.Network.Interfaces), protocol.MaxInterfaces)
	}
	inventory := sampler.Inventory()
	if len(inventory.Network) != protocol.MaxInterfaces {
		t.Fatalf("inventory interfaces = %d, want %d", len(inventory.Network), protocol.MaxInterfaces)
	}
	for index := range readings.Network.Interfaces {
		if readings.Network.Interfaces[index].ID != inventory.Network[index].ID {
			t.Fatalf("interface %d = %q in metrics but %q in inventory", index, readings.Network.Interfaces[index].ID, inventory.Network[index].ID)
		}
	}
}
