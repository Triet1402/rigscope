package procfs

import (
	"hash/fnv"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// realFSTypes are filesystems with real backing storage and locally useful
// capacity. Pseudo filesystems, snap/loop mounts and container overlays are
// excluded by omission.
var realFSTypes = map[string]bool{
	"ext2":  true,
	"ext3":  true,
	"ext4":  true,
	"xfs":   true,
	"btrfs": true,
	"vfat":  true,
	"exfat": true,
	"f2fs":  true,
	"zfs":   true,
	"ntfs":  true,
	"ntfs3": true,
}

var (
	pseudoDeviceRE = regexp.MustCompile(`^/dev/(loop|ram|zram)`)
	diskNameRE     = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	pseudoDiskRE   = regexp.MustCompile(`^(loop|ram|zram)`)
)

// FilesystemCandidate is a selected mount point, before statfs.
type FilesystemCandidate struct {
	ID         string
	MountPoint string
	Device     string
	FSType     string
}

// SelectOptions configures filesystem selection.
type SelectOptions struct {
	// AllowFSTypes are extra filesystem types explicitly allowed by the
	// caller (for example "fuseblk").
	AllowFSTypes []string
}

// SelectFilesystems keeps real local filesystems, deduplicated by backing
// device. For btrfs subvolumes the shortest mount point wins, which is closest
// to the filesystem root and avoids reporting one device several times.
func SelectFilesystems(entries []MountInfoEntry, options SelectOptions) []FilesystemCandidate {
	allowed := make(map[string]bool, len(realFSTypes)+len(options.AllowFSTypes))
	for fsType := range realFSTypes {
		allowed[fsType] = true
	}
	for _, fsType := range options.AllowFSTypes {
		allowed[fsType] = true
	}

	byDevice := make(map[string]FilesystemCandidate)
	for _, entry := range entries {
		if !allowed[entry.FSType] {
			continue
		}
		if pseudoDeviceRE.MatchString(entry.Device) {
			continue
		}
		if entry.MountPoint == "" || entry.Device == "" {
			continue
		}
		candidate := FilesystemCandidate{
			ID:         filesystemID(entry.MountPoint),
			MountPoint: entry.MountPoint,
			Device:     entry.Device,
			FSType:     entry.FSType,
		}
		existing, ok := byDevice[entry.Device]
		if !ok || shorterMount(candidate.MountPoint, existing.MountPoint) {
			byDevice[entry.Device] = candidate
		}
	}

	selected := make([]FilesystemCandidate, 0, len(byDevice))
	for _, candidate := range byDevice {
		selected = append(selected, candidate)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].MountPoint < selected[j].MountPoint })
	if len(selected) > protocol.MaxFilesystems {
		selected = selected[:protocol.MaxFilesystems]
	}
	return selected
}

// filesystemID returns the stable resource id for a mount point. The full path
// is kept on the candidate for statfs; only the id is bounded to the contract's
// resource-id limit. Long paths keep a readable prefix and add a deterministic
// hash of the full path so two paths sharing that prefix stay distinct.
func filesystemID(mountPoint string) string {
	id := "fs:" + mountPoint
	if utf16Length(id) <= protocol.MaxResourceIDLength {
		return id
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(mountPoint))
	suffix := "~" + strconv.FormatUint(hash.Sum64(), 16)
	budget := protocol.MaxResourceIDLength - utf16Length("fs:") - utf16Length(suffix)
	return "fs:" + truncateUTF16(mountPoint, budget) + suffix
}

func shorterMount(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// FilesystemUsage is one filesystem reading plus the capacity the inventory
// needs (nil when statfs failed or returned inconsistent numbers).
type FilesystemUsage struct {
	Reading    FilesystemReading
	TotalBytes *int64
}

// ReadFilesystemUsage reads capacity for one candidate through statfs.
// Failures stay per-resource.
func ReadFilesystemUsage(candidate FilesystemCandidate, statfs StatFSFunc) FilesystemUsage {
	unavailable := func(reason string) FilesystemUsage {
		return FilesystemUsage{
			Reading: FilesystemReading{ID: candidate.ID, Status: StatusUnavailable, Reason: reason},
		}
	}
	if statfs == nil {
		return unavailable("filesystem statistics unavailable")
	}
	snapshot, err := statfs(candidate.MountPoint)
	if err != nil {
		return unavailable("filesystem statistics unavailable")
	}
	if snapshot.Bsize <= 0 || snapshot.Blocks < 0 || snapshot.Bfree < 0 || snapshot.Bfree > snapshot.Blocks {
		return unavailable("filesystem statistics inconsistent")
	}
	if snapshot.Blocks > math.MaxInt64/snapshot.Bsize {
		return unavailable("filesystem statistics inconsistent")
	}
	totalBytes := snapshot.Blocks * snapshot.Bsize
	usedBytes := (snapshot.Blocks - snapshot.Bfree) * snapshot.Bsize
	if usedBytes < 0 || usedBytes > totalBytes {
		return unavailable("filesystem statistics inconsistent")
	}
	// The contract carries JavaScript-safe integers; a derived capacity above
	// that bound cannot be represented honestly.
	if totalBytes > protocol.MaxSafeInteger || usedBytes > protocol.MaxSafeInteger {
		return unavailable("filesystem statistics inconsistent")
	}
	total := totalBytes
	used := usedBytes
	reading := FilesystemReading{
		ID:         candidate.ID,
		Status:     StatusOK,
		UsedBytes:  &used,
		TotalBytes: &total,
	}
	// Inode counts are optional. Both must be present, consistent and inside
	// the safe-integer range; otherwise the fields stay absent rather than
	// reporting a fabricated zero.
	if snapshot.Files > 0 && snapshot.Ffree >= 0 && snapshot.Ffree <= snapshot.Files {
		inodesTotal := snapshot.Files
		inodesUsed := snapshot.Files - snapshot.Ffree
		if inodesTotal <= protocol.MaxSafeInteger && inodesUsed <= protocol.MaxSafeInteger {
			reading.InodesTotal = &inodesTotal
			reading.InodesUsed = &inodesUsed
		}
	}
	return FilesystemUsage{Reading: reading, TotalBytes: &total}
}

// DiscoverDisks lists whole block devices from sysfs. loop/ram/zram devices
// are excluded; device-mapper devices (dm-*) are included.
func DiscoverDisks(src Source) []DiskInventory {
	names, err := src.ReadDir("/sys/block")
	if err != nil {
		return []DiskInventory{}
	}
	sort.Strings(names)
	disks := make([]DiskInventory, 0, len(names))
	for _, name := range names {
		if !diskNameRE.MatchString(name) {
			continue
		}
		if pseudoDiskRE.MatchString(name) {
			continue
		}
		base := "/sys/block/" + name
		var model *string
		if raw := strings.TrimSpace(readTextOrZero(src, base+"/device/model")); raw != "" {
			capped := CapText(raw, protocol.MaxTextLength)
			if capped != "" {
				model = &capped
			}
		}
		var rotational *bool
		switch strings.TrimSpace(readTextOrZero(src, base+"/queue/rotational")) {
		case "0":
			value := false
			rotational = &value
		case "1":
			value := true
			rotational = &value
		}
		var sizeBytes *int64
		if raw := strings.TrimSpace(readTextOrZero(src, base+"/size")); raw != "" {
			if sectors, err := strconv.ParseUint(raw, 10, 64); err == nil && sectors <= uint64(protocol.MaxSafeInteger/512) {
				bytes := int64(sectors) * 512
				sizeBytes = &bytes
			}
		}
		disks = append(disks, DiskInventory{
			ID:         "disk:" + name,
			Name:       name,
			Model:      model,
			Rotational: rotational,
			SizeBytes:  sizeBytes,
		})
		if len(disks) >= protocol.MaxDisks {
			break
		}
	}
	return disks
}

// DiskCountersFor filters /proc/diskstats counters to the discovered whole
// devices.
func DiskCountersFor(counters map[string]DiskCounters, disks []DiskInventory) map[string]DiskCounters {
	filtered := make(map[string]DiskCounters, len(disks))
	for _, disk := range disks {
		if counter, ok := counters[disk.Name]; ok {
			filtered[disk.Name] = counter
		}
	}
	return filtered
}

// diskSignature identifies the device shape across a hotplug/reformat. The
// size is enough for the counters the sampler derives; a changed device is
// treated as a baseline reset.
func diskSignature(disk DiskInventory) string {
	if disk.SizeBytes == nil {
		return "unknown"
	}
	return strconv.FormatInt(*disk.SizeBytes, 10)
}
