package procfs

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/protocol"
)

func TestSelectFilesystems(t *testing.T) {
	entries := ParseMountinfo([]byte(`1 0 0:1 / /tmp rw - tmpfs tmpfs rw
2 0 259:2 / / rw - ext4 /dev/nvme0n1p2 rw
3 0 259:2 /home /home rw - ext4 /dev/nvme0n1p2 rw
4 0 8:18 / /srv/storage rw - btrfs /dev/sdb2 rw
5 0 8:18 /sub /srv/storage/sub rw - btrfs /dev/sdb2 rw
6 0 259:1 / /boot/efi rw - vfat /dev/nvme0n1p1 rw
7 0 7:0 / /snap/core ro - squashfs /dev/loop0 ro
8 0 0:99 / /var/lib/docker/overlay2 rw - overlay overlay rw
9 0 8:32 / /data rw - fuseblk /dev/sdc1 rw
10 0 8:33 / /ramdisk rw - ext4 /dev/ram0 rw`))

	selected := SelectFilesystems(entries, SelectOptions{})
	ids := make([]string, len(selected))
	for i, candidate := range selected {
		ids[i] = candidate.ID
	}
	want := []string{"fs:/", "fs:/boot/efi", "fs:/srv/storage"}
	if len(ids) != len(want) {
		t.Fatalf("selected = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("selected = %v, want %v", ids, want)
		}
	}
	if selected[0].Device != "/dev/nvme0n1p2" || selected[2].Device != "/dev/sdb2" {
		t.Fatalf("selected devices = %+v", selected)
	}

	withFuse := SelectFilesystems(entries, SelectOptions{AllowFSTypes: []string{"fuseblk"}})
	found := false
	for _, candidate := range withFuse {
		if candidate.ID == "fs:/data" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fuseblk was not allowed: %+v", withFuse)
	}
}

func TestReadFilesystemUsage(t *testing.T) {
	candidate := FilesystemCandidate{ID: "fs:/fixture", MountPoint: "/fixture", Device: "/dev/sda1", FSType: "ext4"}
	usage := ReadFilesystemUsage(candidate, func(string) (StatFSSnapshot, error) {
		return StatFSSnapshot{Bsize: 4096, Blocks: 1000, Bfree: 400, Files: 1000, Ffree: 750}, nil
	})
	requireOK(t, usage.Reading.Status)
	if usage.Reading.UsedBytes == nil || *usage.Reading.UsedBytes != 600*4096 {
		t.Errorf("UsedBytes = %v", usage.Reading.UsedBytes)
	}
	if usage.Reading.TotalBytes == nil || *usage.Reading.TotalBytes != 1000*4096 {
		t.Errorf("TotalBytes = %v", usage.Reading.TotalBytes)
	}
	if usage.Reading.InodesUsed == nil || *usage.Reading.InodesUsed != 250 {
		t.Errorf("InodesUsed = %v", usage.Reading.InodesUsed)
	}
	if usage.Reading.InodesTotal == nil || *usage.Reading.InodesTotal != 1000 {
		t.Errorf("InodesTotal = %v", usage.Reading.InodesTotal)
	}

	// Filesystems without an inode table (btrfs, vfat) omit inode readings.
	for _, fsType := range []string{"btrfs", "vfat"} {
		candidate.FSType = fsType
		usage := ReadFilesystemUsage(candidate, func(string) (StatFSSnapshot, error) {
			return StatFSSnapshot{Bsize: 4096, Blocks: 100, Bfree: 50}, nil
		})
		requireOK(t, usage.Reading.Status)
		if usage.Reading.InodesTotal != nil || usage.Reading.InodesUsed != nil {
			t.Fatalf("%s inodes = %v/%v, want nil", fsType, usage.Reading.InodesUsed, usage.Reading.InodesTotal)
		}
	}

	usage = ReadFilesystemUsage(candidate, func(string) (StatFSSnapshot, error) {
		return StatFSSnapshot{}, errors.New("statfs failed")
	})
	if usage.Reading.Status != StatusUnavailable || usage.Reading.Reason != "filesystem statistics unavailable" {
		t.Fatalf("failed statfs = %+v", usage.Reading)
	}
	if usage.TotalBytes != nil {
		t.Fatalf("TotalBytes = %v, want nil", usage.TotalBytes)
	}

	usage = ReadFilesystemUsage(candidate, func(string) (StatFSSnapshot, error) {
		return StatFSSnapshot{Bsize: 4096, Blocks: 100, Bfree: 200}, nil
	})
	if usage.Reading.Status != StatusUnavailable || usage.Reading.Reason != "filesystem statistics inconsistent" {
		t.Fatalf("inconsistent statfs = %+v", usage.Reading)
	}

	usage = ReadFilesystemUsage(candidate, nil)
	if usage.Reading.Status != StatusUnavailable {
		t.Fatalf("nil statfs = %+v", usage.Reading)
	}
}

func TestDiscoverDisksFromFixtures(t *testing.T) {
	disks := DiscoverDisks(fixtureSource(t, "minimal"))
	if len(disks) != 1 || disks[0].ID != "disk:sda" {
		t.Fatalf("minimal disks = %+v", disks)
	}
	disk := disks[0]
	if disk.Model == nil || *disk.Model != "Fixture Disk" {
		t.Errorf("model = %v", disk.Model)
	}
	if disk.Rotational == nil || *disk.Rotational {
		t.Errorf("rotational = %v", disk.Rotational)
	}
	if disk.SizeBytes == nil || *disk.SizeBytes != 2000*512 {
		t.Errorf("size = %v", disk.SizeBytes)
	}

	disks = DiscoverDisks(fixtureSource(t, "linux-host"))
	ids := make([]string, len(disks))
	for i, disk := range disks {
		ids[i] = disk.ID
	}
	want := []string{"disk:nvme0n1", "disk:sda", "disk:sdb"}
	if len(ids) != len(want) {
		t.Fatalf("captured disks = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("captured disks = %v, want %v", ids, want)
		}
	}
}

func TestDiscoverDisksCaps(t *testing.T) {
	children := make([]string, 70)
	for i := range children {
		children[i] = "vd" + itoaTest(i)
	}
	src := mapSource(nil, map[string][]string{"/sys/block": children})
	disks := DiscoverDisks(src)
	if len(disks) != protocol.MaxDisks {
		t.Fatalf("disks = %d, want %d", len(disks), protocol.MaxDisks)
	}
}

func TestSelectFilesystemsBoundsLongResourceIDs(t *testing.T) {
	longPath := "/srv/" + strings.Repeat("nested/", 60) + "storage"
	otherPath := "/srv/" + strings.Repeat("nested/", 60) + "other"
	entries := ParseMountinfo([]byte(
		"1 0 8:1 / " + longPath + " rw - ext4 /dev/sda1 rw\n" +
			"2 0 8:2 / " + otherPath + " rw - ext4 /dev/sdb1 rw\n"))
	selected := SelectFilesystems(entries, SelectOptions{})
	if len(selected) != 2 {
		t.Fatalf("selected = %+v", selected)
	}
	ids := make(map[string]bool)
	for _, candidate := range selected {
		if units := utf16Length(candidate.ID); units > protocol.MaxResourceIDLength {
			t.Fatalf("id %q is %d UTF-16 units, want <= %d", candidate.ID, units, protocol.MaxResourceIDLength)
		}
		if !strings.HasPrefix(candidate.ID, "fs:") || !strings.Contains(candidate.ID, "~") {
			t.Fatalf("long id %q is not the bounded hashed form", candidate.ID)
		}
		if ids[candidate.ID] {
			t.Fatalf("duplicate bounded id %q", candidate.ID)
		}
		ids[candidate.ID] = true
		if candidate.MountPoint == "" || !strings.HasPrefix(candidate.MountPoint, "/srv/nested/") {
			t.Fatalf("full mount point was not kept: %q", candidate.MountPoint)
		}
	}

	// The id is deterministic across calls.
	again := SelectFilesystems(entries, SelectOptions{})
	for i := range selected {
		if selected[i].ID != again[i].ID {
			t.Fatalf("id changed between calls: %q vs %q", selected[i].ID, again[i].ID)
		}
	}
}

func TestReadFilesystemUsageRejectsUnsafeIntegers(t *testing.T) {
	candidate := FilesystemCandidate{ID: "fs:/unsafe", MountPoint: "/unsafe", Device: "/dev/sda1", FSType: "ext4"}

	// A capacity above JavaScript's safe-integer bound is not representable.
	usage := ReadFilesystemUsage(candidate, func(string) (StatFSSnapshot, error) {
		return StatFSSnapshot{Bsize: 4096, Blocks: protocol.MaxSafeInteger/4096 + 1, Bfree: 0}, nil
	})
	if usage.Reading.Status != StatusUnavailable || usage.TotalBytes != nil {
		t.Fatalf("unsafe capacity = %+v / %v, want unavailable", usage.Reading, usage.TotalBytes)
	}

	// Inode counters above the bound stay absent while the byte reading lives.
	usage = ReadFilesystemUsage(candidate, func(string) (StatFSSnapshot, error) {
		return StatFSSnapshot{
			Bsize: 4096, Blocks: 100, Bfree: 50,
			Files: protocol.MaxSafeInteger + 1, Ffree: 0,
		}, nil
	})
	requireOK(t, usage.Reading.Status)
	if usage.Reading.InodesTotal != nil || usage.Reading.InodesUsed != nil {
		t.Fatalf("unsafe inodes = %v/%v, want nil", usage.Reading.InodesUsed, usage.Reading.InodesTotal)
	}

	// A free count larger than the total is inconsistent, not zero inodes used.
	usage = ReadFilesystemUsage(candidate, func(string) (StatFSSnapshot, error) {
		return StatFSSnapshot{Bsize: 4096, Blocks: 100, Bfree: 50, Files: 10, Ffree: 11}, nil
	})
	requireOK(t, usage.Reading.Status)
	if usage.Reading.InodesTotal != nil || usage.Reading.InodesUsed != nil {
		t.Fatalf("inconsistent inodes = %v/%v, want nil", usage.Reading.InodesUsed, usage.Reading.InodesTotal)
	}
}

func TestDiscoverDisksRejectsUnsafeSizes(t *testing.T) {
	src := mapSource(map[string]string{
		"/sys/block/sda/size": strconv.FormatUint(uint64(protocol.MaxSafeInteger/512)+1, 10),
	}, map[string][]string{"/sys/block": {"sda"}})
	disks := DiscoverDisks(src)
	if len(disks) != 1 {
		t.Fatalf("disks = %+v", disks)
	}
	if disks[0].SizeBytes != nil {
		t.Fatalf("size = %d, want nil above the safe-integer bound", *disks[0].SizeBytes)
	}
}

func TestDiskCountersFor(t *testing.T) {
	counters := map[string]DiskCounters{
		"sda":     {ReadSectors: 1},
		"sdb":     {ReadSectors: 2},
		"sda1":    {ReadSectors: 3},
		"nvme0n1": {ReadSectors: 4},
	}
	disks := []DiskInventory{{ID: "disk:sda", Name: "sda"}, {ID: "disk:nvme0n1", Name: "nvme0n1"}}
	filtered := DiskCountersFor(counters, disks)
	if len(filtered) != 2 {
		t.Fatalf("filtered = %+v", filtered)
	}
	if filtered["sda"].ReadSectors != 1 || filtered["nvme0n1"].ReadSectors != 4 {
		t.Fatalf("filtered = %+v", filtered)
	}
}
