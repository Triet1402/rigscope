package procfs

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

func TestParseProcStat(t *testing.T) {
	text := strings.Join([]string{
		"cpu  " + procStatLine(statValues{user: 100, nice: 10, system: 50, idle: 1000, iowait: 20, irq: 5, softirq: 7, steal: 3, guest: 40, guestNice: 2}),
		"cpu0 " + procStatLine(statValues{user: 60, system: 25, idle: 500, iowait: 10, irq: 2, softirq: 3, steal: 1}),
		"cpu1 " + procStatLine(statValues{user: 40, nice: 10, system: 25, idle: 500, iowait: 10, irq: 3, softirq: 4, steal: 2}),
	}, "\n")
	snapshot, err := ParseProcStat([]byte(text))
	if err != nil {
		t.Fatalf("ParseProcStat: %v", err)
	}
	if snapshot.CoreCount != 2 {
		t.Fatalf("CoreCount = %d, want 2", snapshot.CoreCount)
	}
	// guest/guestNice are already included in user and are not added again.
	if want := (CpuCounters{Busy: 175, Idle: 1020, IOwait: 20}); snapshot.Total != want {
		t.Errorf("total = %+v, want %+v", snapshot.Total, want)
	}
	if want := (CpuCoreCounters{Index: 0, CpuCounters: CpuCounters{Busy: 91, Idle: 510, IOwait: 10}}); snapshot.PerCore[0] != want {
		t.Errorf("perCore[0] = %+v, want %+v", snapshot.PerCore[0], want)
	}
	if want := (CpuCoreCounters{Index: 1, CpuCounters: CpuCounters{Busy: 84, Idle: 510, IOwait: 10}}); snapshot.PerCore[1] != want {
		t.Errorf("perCore[1] = %+v, want %+v", snapshot.PerCore[1], want)
	}
}

func TestParseProcStatSortsSparseCores(t *testing.T) {
	text := "cpu 1 0 0 1 0 0 0 0 0 0\n" +
		"cpu3 1 0 0 1 0 0 0 0 0 0\n" +
		"cpu1 1 0 0 1 0 0 0 0 0 0\n"
	snapshot, err := ParseProcStat([]byte(text))
	if err != nil {
		t.Fatalf("ParseProcStat: %v", err)
	}
	if snapshot.CoreCount != 2 {
		t.Fatalf("CoreCount = %d, want 2", snapshot.CoreCount)
	}
	if snapshot.PerCore[0].Index != 1 || snapshot.PerCore[1].Index != 3 {
		t.Fatalf("indexes = %d,%d, want 1,3", snapshot.PerCore[0].Index, snapshot.PerCore[1].Index)
	}
}

func TestParseProcStatRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":               "",
		"short line":          "cpu 1 2 3\n",
		"negative":            "cpu  1 2 3 4 5 6 7 8 0 0\ncpu0 -1 0 0 0 0 0 0 0 0 0\n",
		"not a stat file":     "not a stat file",
		"no aggregate line":   "cpu0 1 2 3 4 5 6 7 8 0 0\n",
		"duplicate aggregate": "cpu 1 2 3 4 5 6 7 8 0 0\ncpu 2 2 3 4 5 6 7 8 0 0\n",
		"impossible index":    "cpu 1 2 3 4 5 6 7 8 0 0\ncpu4096 1 2 3 4 5 6 7 8 0 0\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseProcStat([]byte(text)); err == nil {
				t.Fatal("expected an error")
			} else if !errors.Is(err, ErrMalformed) {
				t.Fatalf("error %v does not wrap ErrMalformed", err)
			}
		})
	}
}

func TestParseMeminfo(t *testing.T) {
	snapshot, err := ParseMeminfo([]byte(meminfoText(nil)))
	if err != nil {
		t.Fatalf("ParseMeminfo: %v", err)
	}
	if snapshot.TotalBytes == nil || *snapshot.TotalBytes != 1000*1024 {
		t.Errorf("TotalBytes = %v, want %d", snapshot.TotalBytes, 1000*1024)
	}
	if snapshot.AvailableBytes == nil || *snapshot.AvailableBytes != 400*1024 {
		t.Errorf("AvailableBytes = %v, want %d", snapshot.AvailableBytes, 400*1024)
	}
	if snapshot.SwapTotalBytes == nil || *snapshot.SwapTotalBytes != 100*1024 {
		t.Errorf("SwapTotalBytes = %v, want %d", snapshot.SwapTotalBytes, 100*1024)
	}
	if snapshot.SwapFreeBytes == nil || *snapshot.SwapFreeBytes != 60*1024 {
		t.Errorf("SwapFreeBytes = %v, want %d", snapshot.SwapFreeBytes, 60*1024)
	}

	noSwap := meminfoText(map[string]*int{"SwapTotal": intp(0), "SwapFree": intp(0)})
	snapshot, err = ParseMeminfo([]byte(noSwap))
	if err != nil {
		t.Fatalf("ParseMeminfo no swap: %v", err)
	}
	if snapshot.SwapTotalBytes == nil || *snapshot.SwapTotalBytes != 0 {
		t.Errorf("SwapTotalBytes = %v, want 0", snapshot.SwapTotalBytes)
	}

	missing := meminfoText(map[string]*int{"MemAvailable": nil})
	snapshot, err = ParseMeminfo([]byte(missing))
	if err != nil {
		t.Fatalf("ParseMeminfo missing available: %v", err)
	}
	if snapshot.AvailableBytes != nil {
		t.Errorf("AvailableBytes = %v, want nil", *snapshot.AvailableBytes)
	}

	if _, err := ParseMeminfo([]byte("MemTotal: 100 MB\n")); err == nil {
		t.Fatal("expected an error for a non-kB field")
	}
	if _, err := ParseMeminfo(nil); err == nil {
		t.Fatal("expected an error for an empty file")
	}
}

func TestParseMeminfoBoundsSafeIntegers(t *testing.T) {
	unsafeKB := strconv.FormatInt(protocol.MaxSafeInteger/1024+1, 10)
	snapshot, err := ParseMeminfo([]byte(
		"MemTotal: " + unsafeKB + " kB\n" +
			"MemAvailable: 1 kB\n" +
			"SwapTotal: " + unsafeKB + " kB\n" +
			"SwapFree: 1 kB\n"))
	if err != nil {
		t.Fatalf("ParseMeminfo: %v", err)
	}
	if snapshot.TotalBytes != nil {
		t.Fatalf("TotalBytes = %d, want nil above the safe-integer bound", *snapshot.TotalBytes)
	}
	if snapshot.SwapTotalBytes != nil {
		t.Fatalf("SwapTotalBytes = %d, want nil above the safe-integer bound", *snapshot.SwapTotalBytes)
	}

	// A value at the bound is still representable and kept.
	safeKB := strconv.FormatInt(protocol.MaxSafeInteger/1024, 10)
	snapshot, err = ParseMeminfo([]byte("MemTotal: " + safeKB + " kB\n"))
	if err != nil {
		t.Fatalf("ParseMeminfo: %v", err)
	}
	if snapshot.TotalBytes == nil || *snapshot.TotalBytes > protocol.MaxSafeInteger {
		t.Fatalf("TotalBytes = %v, want a value within the safe-integer bound", snapshot.TotalBytes)
	}
}

func TestParseMountinfo(t *testing.T) {
	entries := ParseMountinfo([]byte(strings.Join([]string{
		"35 49 0:31 / /run rw,nosuid shared:13 - tmpfs tmpfs rw",
		"49 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw",
		`70 49 8:18 / /srv/my\040storage rw,noatime - btrfs /dev/sdb2 rw,subvolid=5`,
	}, "\n")))
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	want := MountInfoEntry{MountPoint: "/run", Device: "tmpfs", FSType: "tmpfs", MajorMinor: "0:31", Root: "/"}
	if entries[0] != want {
		t.Errorf("entries[0] = %+v, want %+v", entries[0], want)
	}
	if entries[1].MountPoint != "/" || entries[1].Device != "/dev/nvme0n1p2" || entries[1].FSType != "ext4" {
		t.Errorf("entries[1] = %+v", entries[1])
	}
	if entries[2].MountPoint != "/srv/my storage" || entries[2].FSType != "btrfs" {
		t.Errorf("entries[2] = %+v", entries[2])
	}
}

func TestParseNetDevAndDiskstats(t *testing.T) {
	net := ParseNetDev([]byte(netDevText([][3]string{
		{"lo", "100", "200"},
		{"eno1np0", "300", "400"},
	})))
	if got := net["lo"]; got != (NetCounters{RXBytes: 100, TXBytes: 200}) {
		t.Errorf("lo = %+v", got)
	}
	if got := net["eno1np0"]; got != (NetCounters{RXBytes: 300, TXBytes: 400}) {
		t.Errorf("eno1np0 = %+v", got)
	}
	if len(ParseNetDev([]byte("garbage"))) != 0 {
		t.Error("garbage net/dev must be empty")
	}

	disks := ParseDiskstats([]byte(strings.Join([]string{
		"   8       0 sda 10 0 1234 5 10 0 5678 6 0 0 0",
		" 259       0 nvme0n1 10 0 9999 5 10 0 8888 6 0 0 0",
		"  bad line",
	}, "\n")))
	if len(disks) != 2 {
		t.Fatalf("disks = %d, want 2", len(disks))
	}
	want := DiskCounters{
		ReadSectors: 1234, WriteSectors: 5678,
		ReadOps: u64p(10), WriteOps: u64p(10),
		ReadTimeMs: u64p(5), WriteTimeMs: u64p(6), IOTimeMs: u64p(0),
	}
	if got := disks["sda"]; !equalDiskCounters(got, want) {
		t.Errorf("sda = %+v, want %+v", got, want)
	}

	full := ParseDiskstats([]byte("   8       0 sda 20 0 2000 30 40 0 4000 50 0 900 10 0 0 0"))["sda"]
	wantFull := DiskCounters{
		ReadSectors: 2000, WriteSectors: 4000,
		ReadOps: u64p(20), WriteOps: u64p(40),
		ReadTimeMs: u64p(30), WriteTimeMs: u64p(50), IOTimeMs: u64p(900),
	}
	if !equalDiskCounters(full, wantFull) {
		t.Errorf("full sda = %+v, want %+v", full, wantFull)
	}
}

func equalDiskCounters(a, b DiskCounters) bool {
	equal := func(x, y *uint64) bool {
		if x == nil || y == nil {
			return x == y
		}
		return *x == *y
	}
	return a.ReadSectors == b.ReadSectors && a.WriteSectors == b.WriteSectors &&
		equal(a.ReadOps, b.ReadOps) && equal(a.WriteOps, b.WriteOps) &&
		equal(a.ReadTimeMs, b.ReadTimeMs) && equal(a.WriteTimeMs, b.WriteTimeMs) &&
		equal(a.IOTimeMs, b.IOTimeMs)
}

func TestParseCpuinfoModel(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
		ok   bool
	}{
		{"x86", "processor\t: 0\nmodel name\t: AMD EPYC 7402P 24-Core Processor\n", "AMD EPYC 7402P 24-Core Processor", true},
		{"core index only", "processor : 0", "", false},
		{"arm", "processor : 0\nHardware : BCM2835\n", "BCM2835", true},
		{"riscv", "processor : 0\nuarch : sifive,u74-mc\n", "sifive,u74-mc", true},
		{"aarch64", "Processor : AArch64 Processor rev 1\n", "AArch64 Processor rev 1", true},
		{"model name wins over hardware", "Hardware : board\nmodel name : Cortex-A76\n", "Cortex-A76", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseCpuinfoModel([]byte(tc.text))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ParseCpuinfoModel = %q,%v want %q,%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestParseOSRelease(t *testing.T) {
	if got, ok := ParseOSRelease([]byte("NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 26.10\"\n")); !ok || got != "Ubuntu 26.10" {
		t.Fatalf("got %q,%v", got, ok)
	}
	if got, ok := ParseOSRelease([]byte("PRETTY_NAME='Fixture Linux'\n")); !ok || got != "Fixture Linux" {
		t.Fatalf("got %q,%v", got, ok)
	}
	if _, ok := ParseOSRelease([]byte("NAME=Nothing")); ok {
		t.Fatal("expected no distro")
	}
	if _, ok := ParseOSRelease([]byte("PRETTY_NAME=\"\"\n")); ok {
		t.Fatal("an empty name is not a distro")
	}
}

func TestParseBootIDLoadAverageAndUptime(t *testing.T) {
	if got, ok := ParseBootID([]byte("  abc-123\n")); !ok || got != "abc-123" {
		t.Fatalf("boot id = %q,%v", got, ok)
	}
	if _, ok := ParseBootID([]byte(" \n")); ok {
		t.Fatal("empty boot id must be rejected")
	}

	load, err := ParseLoadAvg([]byte("0.50 0.40 0.30 1/100 4242"))
	if err != nil {
		t.Fatalf("ParseLoadAvg: %v", err)
	}
	if load.One != 0.5 || load.Five != 0.4 || load.Fifteen != 0.3 {
		t.Fatalf("load = %+v", load)
	}
	if _, err := ParseLoadAvg([]byte("0.5 0.4")); err == nil {
		t.Fatal("expected an error for a short loadavg")
	}

	uptime, err := ParseUptime([]byte("12345.67 2000.00"))
	if err != nil {
		t.Fatalf("ParseUptime: %v", err)
	}
	want := 12345670 * time.Millisecond
	diff := uptime - want
	if diff < 0 {
		diff = -diff
	}
	if diff > 10*time.Millisecond {
		t.Fatalf("uptime = %v, want about %v", uptime, want)
	}
	if _, err := ParseUptime([]byte("-1 0")); err == nil {
		t.Fatal("negative uptime must be rejected")
	}
}
