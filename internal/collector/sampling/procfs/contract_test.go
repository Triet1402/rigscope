package procfs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// contractDisks renders an available disks family with the arrays the contract
// requires, which the mirrored struct tags omit when empty.
func contractDisks(metric DisksMetric) any {
	if metric.Status != StatusOK {
		return metric
	}
	filesystems := metric.Filesystems
	if filesystems == nil {
		filesystems = []FilesystemReading{}
	}
	devices := metric.Devices
	if devices == nil {
		devices = []DiskIoReading{}
	}
	return map[string]any{
		"status":              "ok",
		"filesystems":         filesystems,
		"devices":             devices,
		"pressureSomePercent": metric.PressureSomePercent,
		"pressureFullPercent": metric.PressureFullPercent,
	}
}

// contractNetwork renders an available network family the same way.
func contractNetwork(metric NetworkMetric) any {
	if metric.Status != StatusOK {
		return metric
	}
	interfaces := metric.Interfaces
	if interfaces == nil {
		interfaces = []InterfaceReading{}
	}
	return map[string]any{"status": "ok", "interfaces": interfaces}
}

// requireContractMetrics maps one sampler tick onto the protocol payload shape
// and runs the real strict parser, so array sizes, id/text lengths and numeric
// ranges are checked against the wire contract rather than a local copy.
func requireContractMetrics(t *testing.T, readings Readings) {
	t.Helper()
	payload := map[string]any{
		"cpu":     readings.CPU,
		"memory":  readings.Memory,
		"swap":    readings.Swap,
		"gpus":    map[string]any{"status": "unavailable", "reason": "no GPUs detected"},
		"disks":   contractDisks(readings.Disks),
		"network": contractNetwork(readings.Network),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal metrics: %v", err)
	}
	if _, err := protocol.ParseMetricsPayload(data); err != nil {
		t.Fatalf("sampler output violates the metrics contract: %v\npayload: %s", err, data)
	}
}

// requireContractInventory maps the non-GPU inventory onto the protocol shape
// and runs the real strict parser.
func requireContractInventory(t *testing.T, inventory HostInventory) {
	t.Helper()
	data, err := json.Marshal(inventory)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal inventory: %v", err)
	}
	capabilities, ok := raw["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("inventory capabilities = %v", raw["capabilities"])
	}
	// The GPU inventory builder owns this entry; this package does not.
	capabilities["gpus"] = map[string]any{"status": "unsupported", "reason": "no GPUs detected"}
	raw["gpus"] = []any{}
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal inventory with gpus: %v", err)
	}
	if _, err := protocol.ParseInventory(data); err != nil {
		t.Fatalf("inventory violates the contract: %v\npayload: %s", err, data)
	}
}

func TestSamplerCapturedFixtureObeysContract(t *testing.T) {
	src := fixtureSource(t, "linux-host")
	clock := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	statfs := func(string) (StatFSSnapshot, error) {
		return StatFSSnapshot{Bsize: 4096, Blocks: 1_000_000, Bfree: 250_000, Files: 1000, Ffree: 750}, nil
	}
	host := capturedHost()
	sampler := NewSampler(SamplerOptions{
		Source: src, StatFS: statfs, Host: &host,
		Addresses: func() (map[string][]InterfaceAddress, error) { return map[string][]InterfaceAddress{}, nil },
		Monotonic: func() (time.Time, bool) { return clock, true },
	})

	requireContractMetrics(t, sampler.Sample())
	clock = clock.Add(time.Second)
	second := sampler.Sample()
	requireContractMetrics(t, second)
	if len(second.CPU.PerCore) != 48 {
		t.Fatalf("perCore length = %d, want 48", len(second.CPU.PerCore))
	}
	requireContractInventory(t, sampler.Inventory())
}

func TestSamplerManyInterfacesObeysContract(t *testing.T) {
	src := fixtureSource(t, "many")
	clock := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	host := HostInfo{Platform: "linux", Release: "fixture-kernel", Arch: "x86_64", CPUCount: 1}
	sampler := NewSampler(SamplerOptions{
		Source: src, Host: &host,
		StatFS: func(string) (StatFSSnapshot, error) {
			return StatFSSnapshot{Bsize: 4096, Blocks: 1000, Bfree: 400}, nil
		},
		Addresses: func() (map[string][]InterfaceAddress, error) { return map[string][]InterfaceAddress{}, nil },
		Monotonic: func() (time.Time, bool) { return clock, true },
	})

	requireContractMetrics(t, sampler.Sample())
	clock = clock.Add(time.Second)
	readings := sampler.Sample()
	requireContractMetrics(t, readings)
	if len(readings.Network.Interfaces) > protocol.MaxInterfaces {
		t.Fatalf("interfaces = %d, want <= %d", len(readings.Network.Interfaces), protocol.MaxInterfaces)
	}
	requireContractInventory(t, sampler.Inventory())
}

func TestSamplerEdgeTreesObeyContract(t *testing.T) {
	longMount := "/srv/" + strings.Repeat("nested/", 50) + "storage"
	statfsCalls := make([]string, 0, 1)
	src := mapSource(map[string]string{
		"/proc/stat": "cpu " + procStatLine(statValues{user: 100, idle: 100}) + "\n" +
			"cpu0 " + procStatLine(statValues{user: 50, idle: 50}) + "\n" +
			"cpu2 " + procStatLine(statValues{user: 50, idle: 50}) + "\n",
		"/proc/meminfo":        "MemTotal: 1000 kB\nMemAvailable: 400 kB\nSwapTotal: 100 kB\nSwapFree: 60 kB\n",
		"/proc/net/dev":        netDevText([][3]string{{"lo", "1000", "2000"}}),
		"/proc/self/mountinfo": "49 1 259:2 / " + longMount + " rw - ext4 /dev/nvme0n1p2 rw\n",
		"/proc/diskstats":      diskstatsText([][3]string{{"sda", "100", "50"}}),
		"/sys/block/sda/size":  "2000\n",
		"/etc/os-release":      "PRETTY_NAME=\"" + strings.Repeat("😀", 200) + "\"\n",
	}, map[string][]string{
		"/sys/block":     {"sda"},
		"/sys/class/net": {"lo"},
	})

	clock := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	host := HostInfo{Platform: "linux", Release: "fixture-kernel", Arch: "x86_64", CPUCount: 1}
	sampler := NewSampler(SamplerOptions{
		Source: src, Host: &host,
		StatFS: func(path string) (StatFSSnapshot, error) {
			statfsCalls = append(statfsCalls, path)
			// Above JavaScript's safe-integer bound: the reading must turn
			// unavailable rather than carry a fabricated value.
			return StatFSSnapshot{Bsize: 4096, Blocks: protocol.MaxSafeInteger/4096 + 1, Bfree: 0}, nil
		},
		Addresses: func() (map[string][]InterfaceAddress, error) { return map[string][]InterfaceAddress{}, nil },
		Monotonic: func() (time.Time, bool) { return clock, true },
	})

	requireContractMetrics(t, sampler.Sample())
	clock = clock.Add(time.Second)
	readings := sampler.Sample()
	requireContractMetrics(t, readings)

	// Sparse per-core indexes keep the contract's index-as-core-number rule.
	if len(readings.CPU.PerCore) != 3 || readings.CPU.PerCore[1] != nil {
		t.Fatalf("perCore = %v, want [50, nil, 50]", readings.CPU.PerCore)
	}

	if len(statfsCalls) == 0 || statfsCalls[0] != longMount {
		t.Fatalf("statfs paths = %q, want the full mount point %q", statfsCalls, longMount)
	}

	inventory := sampler.Inventory()
	requireContractInventory(t, inventory)
	if len(inventory.Filesystems) != 1 {
		t.Fatalf("filesystems = %+v", inventory.Filesystems)
	}
	filesystem := inventory.Filesystems[0]
	if units := utf16Length(filesystem.ID); units > protocol.MaxResourceIDLength {
		t.Fatalf("filesystem id %q is %d units, want <= %d", filesystem.ID, units, protocol.MaxResourceIDLength)
	}
	if units := utf16Length(filesystem.MountPoint); units > protocol.MaxTextLength {
		t.Fatalf("mount point is %d units, want <= %d", units, protocol.MaxTextLength)
	}
	if filesystem.TotalBytes != nil {
		t.Fatalf("unsafe filesystem total = %d, want nil", *filesystem.TotalBytes)
	}
	if inventory.OS.Distro == nil {
		t.Fatal("emoji distro was dropped")
	}
	if units := utf16Length(*inventory.OS.Distro); units > protocol.MaxTextLength {
		t.Fatalf("distro is %d units, want <= %d", units, protocol.MaxTextLength)
	}
}
