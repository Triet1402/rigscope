package procfs

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// ErrMalformed marks input that does not look like the expected procfs/sysfs
// file. Callers turn it into an unavailable reading instead of extrapolating.
var ErrMalformed = errors.New("malformed procfs data")

var (
	cpuStatLineRE  = regexp.MustCompile(`^cpu(\d*)\s+(.*)$`)
	meminfoLineRE  = regexp.MustCompile(`^([A-Za-z0-9()_]+):\s+(\d+)\s+kB\s*$`)
	osReleaseRE    = regexp.MustCompile(`^PRETTY_NAME=(.*)$`)
	mountinfoEscRE = regexp.MustCompile(`\\([0-7]{3})`)
)

// CpuCounters holds the /proc/stat accumulators that matter for usage math.
type CpuCounters struct {
	// Busy is user+nice+system+irq+softirq+steal. Guest time is already part
	// of user, so it is not counted again.
	Busy uint64
	// Idle is idle+iowait.
	Idle uint64
	// IOwait is also kept separately so the sampler can report its share.
	IOwait uint64
}

// CpuCoreCounters is one per-core line.
type CpuCoreCounters struct {
	Index int
	CpuCounters
}

// ProcStat is the parsed /proc/stat CPU section.
type ProcStat struct {
	Total CpuCounters
	// PerCore is sorted by core index. Sparse indexes are allowed (hotplug).
	PerCore []CpuCoreCounters
	// CoreCount is the number of per-core lines, not maxIndex+1.
	CoreCount int
}

// ReadProcStat reads and parses /proc/stat.
func ReadProcStat(src Source) (ProcStat, error) {
	text, err := readText(src, "/proc/stat")
	if err != nil {
		return ProcStat{}, err
	}
	return ParseProcStat([]byte(text))
}

// ParseProcStat parses the aggregate `cpu` line and every `cpuN` line.
//
// The aggregate line is required. Malformed numeric fields, a duplicate
// aggregate line and impossible core indexes reject the whole snapshot, as in
// the TypeScript parser: a partially read counter set would fabricate rates.
func ParseProcStat(text []byte) (ProcStat, error) {
	var (
		total  CpuCounters
		seen   bool
		perCpu []CpuCoreCounters
	)
	for _, rawLine := range strings.Split(string(text), "\n") {
		line := strings.TrimSpace(rawLine)
		match := cpuStatLineRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		values, err := cpuStatFields(match[2])
		if err != nil {
			return ProcStat{}, err
		}
		counters, err := countersFromFields(values)
		if err != nil {
			return ProcStat{}, err
		}
		if match[1] == "" {
			if seen {
				return ProcStat{}, fmt.Errorf("%w: duplicate aggregate cpu line", ErrMalformed)
			}
			seen = true
			total = counters
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil || index < 0 || index > 4095 {
			return ProcStat{}, fmt.Errorf("%w: bad cpu index %q", ErrMalformed, match[1])
		}
		perCpu = append(perCpu, CpuCoreCounters{Index: index, CpuCounters: counters})
	}
	if !seen {
		return ProcStat{}, fmt.Errorf("%w: no aggregate cpu line", ErrMalformed)
	}
	sort.SliceStable(perCpu, func(i, j int) bool { return perCpu[i].Index < perCpu[j].Index })
	return ProcStat{Total: total, PerCore: perCpu, CoreCount: len(perCpu)}, nil
}

// cpuStatFields returns the first ten /proc/stat CPU columns. Fewer than eight
// columns is malformed; missing guest columns are zero, like the kernel's
// older layout.
func cpuStatFields(text string) ([10]uint64, error) {
	var values [10]uint64
	fields := strings.Fields(text)
	if len(fields) < 8 {
		return values, fmt.Errorf("%w: cpu line has %d fields, want at least 8", ErrMalformed, len(fields))
	}
	for i, field := range fields {
		if i == len(values) {
			break
		}
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return values, fmt.Errorf("%w: bad cpu field %q", ErrMalformed, field)
		}
		values[i] = value
	}
	return values, nil
}

func countersFromFields(values [10]uint64) (CpuCounters, error) {
	var busy uint64
	for _, value := range []uint64{values[0], values[1], values[2], values[5], values[6], values[7]} {
		sum, ok := addUint(busy, value)
		if !ok {
			return CpuCounters{}, fmt.Errorf("%w: cpu busy counters overflow", ErrMalformed)
		}
		busy = sum
	}
	idle, ok := addUint(values[3], values[4])
	if !ok {
		return CpuCounters{}, fmt.Errorf("%w: cpu idle counters overflow", ErrMalformed)
	}
	return CpuCounters{Busy: busy, Idle: idle, IOwait: values[4]}, nil
}

func addUint(a, b uint64) (uint64, bool) {
	if a > math.MaxUint64-b {
		return 0, false
	}
	return a + b, true
}

// Meminfo is the parsed /proc/meminfo subset. Missing fields stay nil.
type Meminfo struct {
	TotalBytes     *int64
	AvailableBytes *int64
	SwapTotalBytes *int64
	SwapFreeBytes  *int64
}

// ReadMeminfo reads and parses /proc/meminfo.
func ReadMeminfo(src Source) (Meminfo, error) {
	text, err := readText(src, "/proc/meminfo")
	if err != nil {
		return Meminfo{}, err
	}
	return ParseMeminfo([]byte(text))
}

// ParseMeminfo keeps only the four kB fields the readings need. Any other kB
// field still proves the file parses; malformed lines are ignored, matching
// the kernel's own tolerance for extra fields.
func ParseMeminfo(text []byte) (Meminfo, error) {
	var (
		meminfo Meminfo
		sawAny  bool
	)
	for _, rawLine := range strings.Split(string(text), "\n") {
		match := meminfoLineRE.FindStringSubmatch(strings.TrimSpace(rawLine))
		if match == nil {
			continue
		}
		sawAny = true
		kilobytes, err := strconv.ParseUint(match[2], 10, 64)
		if err != nil || kilobytes > uint64(protocol.MaxSafeInteger/1024) {
			// A byte count above JavaScript's safe-integer range is exported as
			// absent, not as a truncated or fabricated number.
			continue
		}
		bytes := int64(kilobytes) * 1024
		switch match[1] {
		case "MemTotal":
			meminfo.TotalBytes = &bytes
		case "MemAvailable":
			meminfo.AvailableBytes = &bytes
		case "SwapTotal":
			meminfo.SwapTotalBytes = &bytes
		case "SwapFree":
			meminfo.SwapFreeBytes = &bytes
		}
	}
	if !sawAny {
		return Meminfo{}, fmt.Errorf("%w: no kB fields", ErrMalformed)
	}
	return meminfo, nil
}

// NetCounters are the cumulative byte counters for one interface.
type NetCounters struct {
	RXBytes uint64
	TXBytes uint64
}

// ReadNetDev reads and parses /proc/net/dev.
func ReadNetDev(src Source) (map[string]NetCounters, error) {
	text, err := readText(src, "/proc/net/dev")
	if err != nil {
		return nil, err
	}
	return ParseNetDev([]byte(text)), nil
}

// ParseNetDev parses byte counters. Unparsable lines are skipped; an empty
// result is an empty host, not an error.
func ParseNetDev(text []byte) map[string]NetCounters {
	counters := make(map[string]NetCounters)
	for _, rawLine := range strings.Split(string(text), "\n") {
		colon := strings.IndexByte(rawLine, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(rawLine[:colon])
		if name == "" || name == "Inter-|   Receive" || strings.HasPrefix(name, "face") {
			continue
		}
		fields := strings.Fields(rawLine[colon+1:])
		if len(fields) < 9 {
			continue
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			continue
		}
		counters[name] = NetCounters{RXBytes: rx, TXBytes: tx}
	}
	return counters
}

// DiskCounters are the cumulative /proc/diskstats counters for one device.
// Sector counters are required; operation counts and times are nil when the
// kernel line omits them.
type DiskCounters struct {
	ReadSectors  uint64
	WriteSectors uint64
	ReadOps      *uint64
	WriteOps     *uint64
	ReadTimeMs   *uint64
	WriteTimeMs  *uint64
	IOTimeMs     *uint64
}

// ReadDiskstats reads and parses /proc/diskstats.
func ReadDiskstats(src Source) (map[string]DiskCounters, error) {
	text, err := readText(src, "/proc/diskstats")
	if err != nil {
		return nil, err
	}
	return ParseDiskstats([]byte(text)), nil
}

// ParseDiskstats parses every listed device, whole devices and partitions
// alike. Callers filter to whole devices.
func ParseDiskstats(text []byte) map[string]DiskCounters {
	counters := make(map[string]DiskCounters)
	for _, rawLine := range strings.Split(string(text), "\n") {
		fields := strings.Fields(rawLine)
		if len(fields) < 10 {
			continue
		}
		name := fields[2]
		if name == "" {
			continue
		}
		readSectors, err := strconv.ParseUint(fields[5], 10, 64)
		if err != nil {
			continue
		}
		writeSectors, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}
		counters[name] = DiskCounters{
			ReadSectors:  readSectors,
			WriteSectors: writeSectors,
			ReadOps:      optionalCounter(fields, 3),
			ReadTimeMs:   optionalCounter(fields, 6),
			WriteOps:     optionalCounter(fields, 7),
			WriteTimeMs:  optionalCounter(fields, 10),
			IOTimeMs:     optionalCounter(fields, 12),
		}
	}
	return counters
}

func optionalCounter(fields []string, index int) *uint64 {
	if index >= len(fields) {
		return nil
	}
	value, err := strconv.ParseUint(fields[index], 10, 64)
	if err != nil {
		return nil
	}
	return &value
}

// MountInfoEntry is one /proc/self/mountinfo line.
type MountInfoEntry struct {
	MountPoint string
	// Device is the kernel device path (/dev/nvme0n1p2) or a pseudo source
	// (tmpfs).
	Device     string
	FSType     string
	MajorMinor string
	Root       string
}

// ReadMountinfo reads and parses /proc/self/mountinfo.
func ReadMountinfo(src Source) ([]MountInfoEntry, error) {
	text, err := readText(src, "/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	return ParseMountinfo([]byte(text)), nil
}

// ParseMountinfo parses the mount table. Optional per-mount fields precede the
// `-` separator; filesystem type and source follow it.
func ParseMountinfo(text []byte) []MountInfoEntry {
	var entries []MountInfoEntry
	for _, rawLine := range strings.Split(string(text), "\n") {
		fields := strings.Fields(rawLine)
		if len(fields) < 10 {
			continue
		}
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || separator+2 >= len(fields) {
			continue
		}
		entries = append(entries, MountInfoEntry{
			MountPoint: DecodeMountInfoPath(fields[4]),
			Device:     DecodeMountInfoPath(fields[separator+2]),
			FSType:     fields[separator+1],
			MajorMinor: fields[2],
			Root:       DecodeMountInfoPath(fields[3]),
		})
	}
	return entries
}

// DecodeMountInfoPath decodes the octal escapes mountinfo uses for spaces and
// other awkward characters (`\040` is a space).
func DecodeMountInfoPath(value string) string {
	return mountinfoEscRE.ReplaceAllStringFunc(value, func(match string) string {
		code, err := strconv.ParseUint(match[1:], 8, 8)
		if err != nil {
			return match
		}
		return string(rune(code))
	})
}

// ParseCpuinfoModel returns the first usable CPU model string. x86 uses
// "model name"; ARM/RISC-V commonly expose Hardware, cpu model or uarch. A
// purely numeric value is a processor index, not a model.
func ParseCpuinfoModel(text []byte) (string, bool) {
	lines := strings.Split(string(text), "\n")
	for _, key := range []string{"model name", "Hardware", "cpu model", "uarch", "Processor"} {
		for _, line := range lines {
			colon := strings.IndexByte(line, ':')
			if colon < 0 {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(line[:colon]), key) {
				continue
			}
			value := strings.TrimSpace(line[colon+1:])
			if value == "" || isAllDigits(value) {
				continue
			}
			return value, true
		}
	}
	return "", false
}

// ParseOSRelease returns PRETTY_NAME from /etc/os-release with quotes
// stripped. The first PRETTY_NAME line wins; an empty value is not a name.
func ParseOSRelease(text []byte) (string, bool) {
	for _, rawLine := range strings.Split(string(text), "\n") {
		match := osReleaseRE.FindStringSubmatch(strings.TrimSpace(rawLine))
		if match == nil {
			continue
		}
		value := strings.TrimSpace(match[1])
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if value == "" {
			return "", false
		}
		return value, true
	}
	return "", false
}

// ParseBootID returns the kernel boot id, used to detect reboots across a
// collector restart. It is a UUID-shaped token; this parser only trims it.
func ParseBootID(text []byte) (string, bool) {
	value := strings.TrimSpace(string(text))
	if value == "" {
		return "", false
	}
	return value, true
}

// ReadBootID reads /proc/sys/kernel/random/boot_id.
func ReadBootID(src Source) (string, error) {
	text, err := readText(src, "/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	value, ok := ParseBootID([]byte(text))
	if !ok {
		return "", fmt.Errorf("%w: empty boot id", ErrMalformed)
	}
	return value, nil
}

// LoadAverage is the /proc/loadavg 1/5/15-minute averages.
type LoadAverage struct {
	One     float64
	Five    float64
	Fifteen float64
}

// ParseLoadAvg parses the three averages in /proc/loadavg.
func ParseLoadAvg(text []byte) (LoadAverage, error) {
	fields := strings.Fields(string(text))
	if len(fields) < 3 {
		return LoadAverage{}, fmt.Errorf("%w: loadavg needs three fields", ErrMalformed)
	}
	var values [3]float64
	for i := range values {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return LoadAverage{}, fmt.Errorf("%w: bad loadavg field %q", ErrMalformed, fields[i])
		}
		values[i] = value
	}
	return LoadAverage{One: values[0], Five: values[1], Fifteen: values[2]}, nil
}

// ParseUptime parses the uptime seconds from /proc/uptime (the idle seconds
// are ignored).
func ParseUptime(text []byte) (time.Duration, error) {
	fields := strings.Fields(string(text))
	if len(fields) == 0 {
		return 0, fmt.Errorf("%w: empty uptime", ErrMalformed)
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return 0, fmt.Errorf("%w: bad uptime %q", ErrMalformed, fields[0])
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func isAllDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}
