package procfs

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Triet1402/rigscope/internal/protocol"
)

func capturedHost() HostInfo {
	return HostInfo{Platform: "linux", Release: "7.3.0-10-generic", Arch: "x86_64", CPUCount: 8}
}

func TestBuildInventoryMinimal(t *testing.T) {
	statfs := func(path string) (StatFSSnapshot, error) {
		switch path {
		case "/":
			return StatFSSnapshot{Bsize: 4096, Blocks: 1000, Bfree: 400}, nil
		case "/srv/fixture":
			return StatFSSnapshot{Bsize: 4096, Blocks: 2000, Bfree: 500}, nil
		default:
			return StatFSSnapshot{}, errors.New("not mounted")
		}
	}
	inventory := BuildInventory(InventoryConfig{
		Source: fixtureSource(t, "minimal"),
		StatFS: statfs,
		Host:   &HostInfo{Platform: "linux", Release: "fixture-kernel", Arch: "x86_64", CPUCount: 4},
	})

	if inventory.OS.Platform != "linux" || inventory.OS.Arch != "x86_64" {
		t.Errorf("os = %+v", inventory.OS)
	}
	if inventory.OS.Release != "fixture-kernel" {
		t.Errorf("release = %q", inventory.OS.Release)
	}
	if inventory.OS.Distro == nil || *inventory.OS.Distro != "Fixture Linux 1.0" {
		t.Errorf("distro = %v", inventory.OS.Distro)
	}
	if inventory.CPU.Model == nil || *inventory.CPU.Model != "Fixture Single Core CPU" {
		t.Errorf("model = %v", inventory.CPU.Model)
	}
	if inventory.CPU.LogicalCores != 1 {
		t.Errorf("logicalCores = %d, want 1 (from /proc/stat, not the fallback)", inventory.CPU.LogicalCores)
	}
	if inventory.Memory.TotalBytes == nil || *inventory.Memory.TotalBytes != 8192000*1024 {
		t.Errorf("memory total = %v", inventory.Memory.TotalBytes)
	}
	if inventory.Memory.SwapTotalBytes != nil {
		t.Errorf("swap total = %v, want nil for a host without swap fields", *inventory.Memory.SwapTotalBytes)
	}

	ids := make([]string, len(inventory.Filesystems))
	for i, filesystem := range inventory.Filesystems {
		ids[i] = filesystem.ID
	}
	want := []string{"fs:/", "fs:/srv/fixture"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("filesystems = %v, want %v", ids, want)
	}
	if inventory.Filesystems[0].TotalBytes == nil || *inventory.Filesystems[0].TotalBytes != 1000*4096 {
		t.Errorf("filesystem total = %v", inventory.Filesystems[0].TotalBytes)
	}
	if len(inventory.Disks) != 1 || inventory.Disks[0].ID != "disk:sda" {
		t.Errorf("disks = %+v", inventory.Disks)
	}
	if len(inventory.Network) != 2 || inventory.Network[0].Name != "eth0" || inventory.Network[1].Name != "lo" {
		t.Errorf("network = %+v", inventory.Network)
	}
	for _, name := range []string{"cpu", "memory", "swap", "disks", "network", "processes"} {
		var state CapabilityState
		switch name {
		case "cpu":
			state = inventory.Capabilities.CPU
		case "memory":
			state = inventory.Capabilities.Memory
		case "swap":
			state = inventory.Capabilities.Swap
		case "disks":
			state = inventory.Capabilities.Disks
		case "network":
			state = inventory.Capabilities.Network
		case "processes":
			state = inventory.Capabilities.Processes
		}
		if state.Status != CapabilitySupported {
			t.Errorf("%s capability = %+v, want supported", name, state)
		}
	}
}

func TestBuildInventoryCapturedHost(t *testing.T) {
	statfs := func(path string) (StatFSSnapshot, error) {
		switch path {
		case "/":
			return StatFSSnapshot{Bsize: 4096, Blocks: 1_000_000, Bfree: 250_000, Files: 1000, Ffree: 750}, nil
		case "/boot/efi":
			return StatFSSnapshot{Bsize: 512, Blocks: 1000, Bfree: 900}, nil
		case "/srv/storage":
			return StatFSSnapshot{Bsize: 4096, Blocks: 2_000_000, Bfree: 500_000}, nil
		default:
			return StatFSSnapshot{}, errors.New("not a fixture mount")
		}
	}
	src := fixtureSource(t, "linux-host")
	host := capturedHost()
	inventory := BuildInventory(InventoryConfig{Source: src, StatFS: statfs, Host: &host})

	if inventory.OS.Release != "7.3.0-10-generic" {
		t.Errorf("release = %q", inventory.OS.Release)
	}
	if inventory.OS.Distro == nil || *inventory.OS.Distro != "Ubuntu 26.10" {
		t.Errorf("distro = %v", inventory.OS.Distro)
	}
	if inventory.CPU.Model == nil || *inventory.CPU.Model != "AMD EPYC 7402P 24-Core Processor" {
		t.Errorf("model = %v", inventory.CPU.Model)
	}
	if inventory.CPU.LogicalCores != 48 {
		t.Errorf("logicalCores = %d, want 48", inventory.CPU.LogicalCores)
	}
	if inventory.Memory.TotalBytes == nil || *inventory.Memory.TotalBytes != 127296100*1024 {
		t.Errorf("memory total = %v", inventory.Memory.TotalBytes)
	}
	if inventory.Memory.SwapTotalBytes == nil || *inventory.Memory.SwapTotalBytes != 8388604*1024 {
		t.Errorf("swap total = %v", inventory.Memory.SwapTotalBytes)
	}

	ids := make([]string, len(inventory.Filesystems))
	for i, filesystem := range inventory.Filesystems {
		ids[i] = filesystem.ID
	}
	want := []string{"fs:/", "fs:/boot/efi", "fs:/srv/storage"}
	if len(ids) != len(want) {
		t.Fatalf("filesystems = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("filesystems = %v, want %v", ids, want)
		}
	}

	diskIDs := make([]string, len(inventory.Disks))
	for i, disk := range inventory.Disks {
		diskIDs[i] = disk.ID
	}
	if len(diskIDs) != 3 || diskIDs[0] != "disk:nvme0n1" || diskIDs[1] != "disk:sda" || diskIDs[2] != "disk:sdb" {
		t.Fatalf("disks = %v", diskIDs)
	}

	byName := map[string]InterfaceInventory{}
	for _, iface := range inventory.Network {
		byName[iface.Name] = iface
	}
	if len(byName) != 5 {
		t.Fatalf("network = %+v", inventory.Network)
	}
	if eno := byName["eno1np0"]; eno.State != "up" || eno.SpeedMbps == nil || *eno.SpeedMbps != 1000 {
		t.Errorf("eno1np0 = %+v", eno)
	}
	if eno := byName["eno2np1"]; eno.SpeedMbps == nil || *eno.SpeedMbps != 10000 {
		t.Errorf("eno2np1 = %+v", eno)
	}
	if mac := byName["eno1np0"].MAC; mac == nil || *mac != "02:00:00:00:01:01" {
		t.Errorf("sanitized mac = %v", mac)
	}
	if enx := byName["enx020000000103"]; enx.State != "down" || enx.SpeedMbps != nil {
		t.Errorf("down link = %+v", enx)
	}
	if lo := byName["lo"]; !lo.Virtual {
		t.Errorf("loopback must be marked virtual: %+v", lo)
	}
	if tailscale := byName["tailscale0"]; !tailscale.Virtual || tailscale.SpeedMbps != nil {
		t.Errorf("tailscale0 = %+v", tailscale)
	}

	bootID, err := ReadBootID(src)
	if err != nil {
		t.Fatalf("ReadBootID: %v", err)
	}
	if bootID != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("boot id = %q", bootID)
	}
	if got := ReadCPUTemperature(src); got == nil || *got != 31.88 {
		t.Errorf("captured CPU temperature = %v, want 31.88", got)
	}
}

func TestBuildInventoryDegradesIndependently(t *testing.T) {
	src := mapSource(map[string]string{
		"/proc/meminfo": meminfoText(nil),
	}, nil)
	inventory := BuildInventory(InventoryConfig{
		Source: src,
		Host:   &HostInfo{Platform: "linux", Release: "fixture-kernel", Arch: "x86_64", CPUCount: 3},
	})
	if inventory.Capabilities.CPU.Status != CapabilityUnsupported || inventory.Capabilities.CPU.Reason != "procfs unavailable" {
		t.Errorf("cpu capability = %+v", inventory.Capabilities.CPU)
	}
	if inventory.CPU.LogicalCores != 3 {
		t.Errorf("logicalCores = %d, want the host fallback 3", inventory.CPU.LogicalCores)
	}
	if inventory.Capabilities.Processes.Status != CapabilityUnsupported {
		t.Errorf("processes capability = %+v", inventory.Capabilities.Processes)
	}
	if inventory.Capabilities.Disks.Status != CapabilityUnsupported || inventory.Capabilities.Disks.Reason != "mount information unavailable" {
		t.Errorf("disks capability = %+v", inventory.Capabilities.Disks)
	}
	if inventory.Capabilities.Network.Status != CapabilityUnsupported {
		t.Errorf("network capability = %+v", inventory.Capabilities.Network)
	}
	// Memory still works even though /proc/stat and mounts are absent.
	if inventory.Capabilities.Memory.Status != CapabilitySupported {
		t.Errorf("memory capability = %+v", inventory.Capabilities.Memory)
	}
	if inventory.Capabilities.Swap.Status != CapabilitySupported {
		t.Errorf("swap capability = %+v", inventory.Capabilities.Swap)
	}
	if inventory.OS.Platform != "linux" || inventory.OS.Arch != "x86_64" {
		t.Errorf("os fallback = %+v", inventory.OS)
	}
}

func TestCapText(t *testing.T) {
	if got := CapText("  hello\nworld\x00  ", 64); got != "hello world" {
		t.Errorf("CapText = %q", got)
	}
	long := strings.Repeat("a", 100)
	if got := CapText(long, 10); got != "aaaaaaa..." || len(got) != 10 {
		t.Errorf("CapText long = %q", got)
	}
	if got := CapReason(strings.Repeat("é", 300)); utf8.RuneCountInString(got) != protocol.MaxTextLength || !strings.HasSuffix(got, "...") {
		t.Errorf("CapReason = %q (%d runes)", got, utf8.RuneCountInString(got))
	}
}

func TestCapTextUsesUTF16CodeUnits(t *testing.T) {
	// An emoji outside the BMP is two UTF-16 code units in the contract even
	// though it is one rune.
	emoji := strings.Repeat("😀", 200)
	if utf16Length("😀") != 2 {
		t.Fatalf("utf16Length(emoji) = %d, want 2", utf16Length("😀"))
	}
	got := CapText(emoji, 10)
	if units := utf16Length(got); units > 10 {
		t.Fatalf("emoji cap = %q (%d units), want <= 10", got, units)
	}
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "...") {
		t.Fatalf("emoji cap = %q, want valid UTF-8 ending in ...", got)
	}
	if want := strings.Repeat("😀", 3) + "..."; got != want {
		t.Fatalf("emoji cap = %q, want %q", got, want)
	}

	// A tiny cap must not split the surrogate pair.
	if got := CapText("😀😀", 3); got != "😀" {
		t.Fatalf("tiny emoji cap = %q, want one whole emoji", got)
	}

	// CJK characters are inside the BMP: one code unit each, so a 256-unit
	// reason keeps 253 characters plus the ellipsis.
	cjk := strings.Repeat("漢", 300)
	capped := CapReason(cjk)
	if units := utf16Length(capped); units != protocol.MaxTextLength {
		t.Fatalf("CJK cap = %d units, want %d", units, protocol.MaxTextLength)
	}
	if utf8.RuneCountInString(capped) != protocol.MaxTextLength || !strings.HasSuffix(capped, "...") {
		t.Fatalf("CJK cap = %q (%d runes)", capped, utf8.RuneCountInString(capped))
	}
}
