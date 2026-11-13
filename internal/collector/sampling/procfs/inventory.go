package procfs

import (
	"runtime"
	"strings"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// HostInfo is the platform identity the inventory reports. It is injected so
// tests do not depend on the machine they run on.
type HostInfo struct {
	Platform string
	Release  string
	Arch     string
	CPUCount int
}

// HostInfoFrom builds HostInfo from a Source: the kernel release comes from
// /proc/sys/kernel/osrelease so fixture trees are deterministic. Platform and
// architecture are the build target's, and CPUCount is the Go runtime's view.
func HostInfoFrom(src Source) HostInfo {
	return HostInfo{
		Platform: runtime.GOOS,
		Release:  strings.TrimSpace(readTextOrZero(src, "/proc/sys/kernel/osrelease")),
		Arch:     runtime.GOARCH,
		CPUCount: runtime.NumCPU(),
	}
}

// InventoryConfig supplies the sources one inventory build reads.
type InventoryConfig struct {
	Source    Source
	StatFS    StatFSFunc
	Addresses InterfaceAddressSource
	Host      *HostInfo
	// AllowFSTypes are extra filesystem types the caller explicitly allows.
	AllowFSTypes []string
}

// BuildInventory builds the non-GPU inventory from current host state.
// Individual sources degrade independently: an unreadable mount table does not
// hide CPU, disks or interfaces. GPUs and their capability entry are added by
// the GPU inventory builder.
func BuildInventory(cfg InventoryConfig) HostInventory {
	src := cfg.Source
	if src == nil {
		src = NewRootSource()
	}
	host := HostInfoFrom(src)
	if cfg.Host != nil {
		host = *cfg.Host
	}
	if host.Platform == "" {
		host.Platform = "linux"
	}

	stat, statErr := ReadProcStat(src)
	statOK := statErr == nil
	meminfo, meminfoErr := ReadMeminfo(src)
	meminfoOK := meminfoErr == nil

	mountEntries, mountErr := ReadMountinfo(src)
	mountOK := mountErr == nil
	if !mountOK {
		mountEntries = nil
	}

	filesystems := make([]FilesystemInventory, 0)
	for _, candidate := range SelectFilesystems(mountEntries, SelectOptions{AllowFSTypes: cfg.AllowFSTypes}) {
		usage := ReadFilesystemUsage(candidate, cfg.StatFS)
		filesystems = append(filesystems, FilesystemInventory{
			ID:         candidate.ID,
			MountPoint: capDisplayText(candidate.MountPoint),
			Device:     capDisplayText(candidate.Device),
			FSType:     CapText(candidate.FSType, maxFSTypeLength),
			TotalBytes: usage.TotalBytes,
		})
	}

	disks := DiscoverDisks(src)
	network := DiscoverInterfaces(cfg.Addresses, src)
	if len(network) > protocol.MaxInterfaces {
		network = network[:protocol.MaxInterfaces]
	}

	cpuCapability := CapabilityState{Status: CapabilitySupported}
	if !statOK {
		cpuCapability = CapabilityState{Status: CapabilityUnsupported, Reason: "procfs unavailable"}
	}
	memoryCapability := CapabilityState{Status: CapabilitySupported}
	if !meminfoOK || meminfo.TotalBytes == nil {
		memoryCapability = CapabilityState{Status: CapabilityUnsupported, Reason: "memory counters unavailable"}
	}
	disksCapability := CapabilityState{Status: CapabilitySupported}
	if !mountOK {
		disksCapability = CapabilityState{Status: CapabilityUnsupported, Reason: "mount information unavailable"}
	}
	networkCapability := CapabilityState{Status: CapabilitySupported}
	if len(network) == 0 {
		networkCapability = CapabilityState{Status: CapabilityUnsupported, Reason: "no network interfaces detected"}
	}
	processesCapability := CapabilityState{Status: CapabilityUnsupported, Reason: "processes need Linux procfs"}
	if host.Platform == "linux" && statOK {
		processesCapability = CapabilityState{Status: CapabilitySupported}
	}

	inventory := HostInventory{
		OS: OSInventory{
			Platform: CapText(host.Platform, 64),
			Release:  CapText(host.Release, protocol.MaxTextLength),
			Distro:   nil,
			Arch:     CapText(host.Arch, 64),
		},
		CPU: CPUInventory{
			LogicalCores: inventoryCores(stat, statOK, host.CPUCount),
		},
		Memory:      MemoryInventory{},
		Filesystems: filesystems,
		Disks:       disks,
		Network:     network,
		Capabilities: InventoryCapabilities{
			CPU:       cpuCapability,
			Memory:    memoryCapability,
			Swap:      memoryCapability,
			Disks:     disksCapability,
			Network:   networkCapability,
			Processes: processesCapability,
		},
	}
	if distro, ok := ParseOSRelease([]byte(readTextOrZero(src, "/etc/os-release"))); ok {
		capped := CapText(distro, protocol.MaxTextLength)
		inventory.OS.Distro = &capped
	}
	if model, ok := ParseCpuinfoModel([]byte(readTextOrZero(src, "/proc/cpuinfo"))); ok {
		capped := CapText(model, protocol.MaxTextLength)
		inventory.CPU.Model = &capped
	}
	if meminfo.TotalBytes != nil {
		total := *meminfo.TotalBytes
		inventory.Memory.TotalBytes = &total
	}
	if meminfo.SwapTotalBytes != nil {
		swap := *meminfo.SwapTotalBytes
		inventory.Memory.SwapTotalBytes = &swap
	}
	return inventory
}

func inventoryCores(stat ProcStat, statOK bool, runtimeCPUCount int) int {
	if statOK && stat.CoreCount > 0 {
		return min(stat.CoreCount, protocol.MaxCores)
	}
	if runtimeCPUCount >= 0 {
		return min(runtimeCPUCount, protocol.MaxCores)
	}
	return 0
}

// CapText strips control characters, trims, and caps a text field. It is used
// for names that reach the wire and must stay bounded. The cap is expressed in
// UTF-16 code units, the unit the contract uses, and never splits a character.
func CapText(value string, max int) string {
	cleaned := make([]rune, 0, len(value))
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			cleaned = append(cleaned, ' ')
			continue
		}
		cleaned = append(cleaned, r)
	}
	text := strings.TrimSpace(string(cleaned))
	if utf16Length(text) <= max {
		return text
	}
	if max <= 3 {
		return truncateUTF16(text, max)
	}
	return truncateUTF16(text, max-3) + "..."
}

// maxFSTypeLength is the contract's bound for a filesystem type name.
const maxFSTypeLength = 64

// capDisplayText caps a display string at the contract's text limit.
func capDisplayText(value string) string {
	return CapText(value, protocol.MaxTextLength)
}

// utf16Length counts UTF-16 code units, which is how the contract measures
// string lengths. Runes outside the BMP need two units (a surrogate pair).
func utf16Length(value string) int {
	units := 0
	for _, r := range value {
		if r > 0xFFFF {
			units += 2
			continue
		}
		units++
	}
	return units
}

// truncateUTF16 returns the longest prefix of value that fits in max UTF-16
// code units without splitting a character.
func truncateUTF16(value string, max int) string {
	if max <= 0 {
		return ""
	}
	units := 0
	for index, r := range value {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if units+width > max {
			return value[:index]
		}
		units += width
	}
	return value
}

// CapReason caps a failure/diagnostic reason at the contract's text limit.
func CapReason(value string) string {
	return CapText(value, protocol.MaxTextLength)
}
