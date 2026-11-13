package procfs

import (
	"math"
	"sort"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// Linux USER_HZ: /proc/stat counters are in centiseconds.
const userHZ = 100

const (
	// cpuToleranceFraction is the final clamp tolerance for quantized
	// counters (5% of capacity).
	cpuToleranceFraction = 0.05
	maxRateBytesPerSec   = 1e13
	// maxLatencyMs is the contract's ceiling for a mean operation latency.
	maxLatencyMs = 86_400_000
)

// SamplerOptions supplies the sampler's dependencies. Nil fields fall back to
// the live host; tests inject fixture sources, a fake statfs, a fake address
// list and a fake clock.
type SamplerOptions struct {
	Source    Source
	StatFS    StatFSFunc
	Addresses InterfaceAddressSource
	// Host is optional; nil derives it from Source.
	Host *HostInfo
	// AllowFSTypes are extra filesystem types for inventory and metrics.
	AllowFSTypes []string
	// Monotonic is the clock used for counter intervals. It returns false when
	// the clock failed. Nil means the live process clock.
	Monotonic func() (time.Time, bool)
}

// Sampler holds the previous counters and derives one reading set per Sample.
// It is not safe for concurrent use; the collector calls it from one loop.
type Sampler struct {
	src          Source
	statfs       StatFSFunc
	addresses    InterfaceAddressSource
	host         HostInfo
	allowFSTypes []string
	monotonic    func() (time.Time, bool)
	rapl         *RaplPowerMeter

	cpuBaseline   *cpuBaseline
	diskBaselines map[string]diskBaseline
	netBaselines  map[string]netBaseline

	failures     []CollectionFailure
	seenFailures map[string]bool
}

type cpuBaseline struct {
	at        time.Time
	coreCount int
	total     CpuCounters
	perCore   map[int]CpuCounters
}

type diskBaseline struct {
	at           time.Time
	readSectors  uint64
	writeSectors uint64
	readOps      *uint64
	writeOps     *uint64
	readTimeMs   *uint64
	writeTimeMs  *uint64
	ioTimeMs     *uint64
	signature    string
}

type netBaseline struct {
	at      time.Time
	rxBytes uint64
	txBytes uint64
}

// NewSampler builds a sampler. It does not read anything until the first
// Sample or Inventory call.
func NewSampler(options SamplerOptions) *Sampler {
	src := options.Source
	if src == nil {
		src = NewRootSource()
	}
	statfs := options.StatFS
	if statfs == nil {
		statfs = OSStatFS
	}
	addresses := options.Addresses
	if addresses == nil {
		addresses = OSInterfaceAddresses
	}
	host := HostInfoFrom(src)
	if options.Host != nil {
		host = *options.Host
	}
	monotonic := options.Monotonic
	if monotonic == nil {
		monotonic = func() (time.Time, bool) { return time.Now(), true }
	}
	return &Sampler{
		src:           src,
		statfs:        statfs,
		addresses:     addresses,
		host:          host,
		allowFSTypes:  options.AllowFSTypes,
		monotonic:     monotonic,
		rapl:          NewRaplPowerMeter(src),
		diskBaselines: make(map[string]diskBaseline),
		netBaselines:  make(map[string]netBaseline),
	}
}

// Sample reads one tick. The first interval reports CPU, disk and network
// rates as unavailable (they need two counter readings); memory, swap and the
// inventory-ish parts are available immediately.
func (s *Sampler) Sample() Readings {
	s.failures = nil
	s.seenFailures = make(map[string]bool)
	at, clockOK := s.monotonic()
	return Readings{
		CPU:     s.sampleCPU(at, clockOK),
		Memory:  s.sampleMemory(),
		Swap:    s.sampleSwap(),
		Disks:   s.sampleDisks(at, clockOK),
		Network: s.sampleNetwork(at, clockOK),
	}
}

// Inventory builds the non-GPU inventory from the current host state.
func (s *Sampler) Inventory() HostInventory {
	host := s.host
	return BuildInventory(InventoryConfig{
		Source:       s.src,
		StatFS:       s.statfs,
		Addresses:    s.addresses,
		Host:         &host,
		AllowFSTypes: s.allowFSTypes,
	})
}

// Failures returns the deduplicated, bounded failure list from the last
// Sample call.
func (s *Sampler) Failures() []CollectionFailure {
	return append([]CollectionFailure(nil), s.failures...)
}

func (s *Sampler) sampleCPU(at time.Time, clockOK bool) CpuMetric {
	if !clockOK {
		return s.cpuUnavailable("cpu monotonic elapsed clock unavailable")
	}
	snapshot, err := ReadProcStat(s.src)
	if err != nil {
		s.cpuBaseline = nil
		return s.cpuUnavailable("cpu counters unavailable")
	}
	current := &cpuBaseline{
		at:        at,
		coreCount: snapshot.CoreCount,
		total:     snapshot.Total,
		perCore:   coreCounterMap(snapshot.PerCore),
	}
	previous := s.cpuBaseline
	s.cpuBaseline = current
	if previous == nil {
		return s.cpuUnavailable("cpu usage needs two counter readings; first interval pending")
	}
	if previous.coreCount != current.coreCount {
		return s.cpuUnavailable("cpu core count changed; baseline reset")
	}
	if current.coreCount < 1 {
		return s.cpuUnavailable("cpu core counters unavailable")
	}
	elapsedMs := float64(current.at.Sub(previous.at)) / float64(time.Millisecond)
	if elapsedMs <= 0 {
		return s.cpuUnavailable("cpu monotonic elapsed did not advance")
	}
	if current.total.Busy < previous.total.Busy || current.total.Idle < previous.total.Idle {
		return s.cpuUnavailable("cpu counters decreased; baseline reset")
	}
	busyDelta := float64(current.total.Busy - previous.total.Busy)
	idleDelta := float64(current.total.Idle - previous.total.Idle)

	ticksPerCore := elapsedMs * userHZ / 1000
	capacity := ticksPerCore * float64(current.coreCount)
	if capacity <= 0 {
		return s.cpuUnavailable("cpu monotonic elapsed did not advance")
	}
	tolerance := math.Max(capacity*cpuToleranceFraction, float64(current.coreCount)*2)
	if busyDelta > capacity+tolerance {
		return s.cpuUnavailable("cpu busy counters exceed measured capacity; baseline reset")
	}
	if idleDelta > busyDelta+capacity+tolerance {
		return s.cpuUnavailable("cpu idle counters exceed measured capacity")
	}

	usage := roundPercent((busyDelta / capacity) * 100)
	reading := CpuMetric{
		Status:       StatusOK,
		UsagePercent: &usage,
		PerCore:      s.perCoreUsage(previous, current, ticksPerCore),
	}
	if iowaitDelta, ok := cpuIowaitDelta(current, previous, capacity, tolerance); ok {
		value := roundPercent((iowaitDelta / capacity) * 100)
		reading.IOwaitPercent = &value
	} else {
		s.addFailure("cpu", nil, "cpu iowait counters inconsistent")
	}

	if pressure, err := ReadPressure(s.src, "/proc/pressure/cpu"); err == nil {
		reading.PressureSomePercent = pressure.SomeAvg10
	}
	reading.TemperatureC = ReadCPUTemperature(s.src)
	reading.PowerWatts = s.rapl.Sample(at, clockOK)
	return reading
}

// perCoreUsage returns one entry per core index. The contract defines the
// array index as the core number, so a hotplug gap is a nil hole rather than a
// shifted reading. Values beyond the contract's core limit cannot be
// represented; they stay absent and are reported as a collection failure while
// the aggregate counters continue to use the full core count.
func (s *Sampler) perCoreUsage(previous, current *cpuBaseline, ticksPerCore float64) []*float64 {
	maxIndex := -1
	for index := range previous.perCore {
		if index > maxIndex {
			maxIndex = index
		}
	}
	for index := range current.perCore {
		if index > maxIndex {
			maxIndex = index
		}
	}
	if maxIndex < 0 {
		return nil
	}
	length := maxIndex + 1
	if length > protocol.MaxCores {
		length = protocol.MaxCores
		s.addFailure("cpu", nil, "cpu core indexes exceed the contract limit")
	}

	perCore := make([]*float64, length)
	for index := 0; index < length; index++ {
		prev, prevOK := previous.perCore[index]
		cur, curOK := current.perCore[index]
		if !prevOK || !curOK || cur.Busy < prev.Busy {
			continue
		}
		busyDelta := float64(cur.Busy - prev.Busy)
		if ticksPerCore <= 0 || busyDelta > ticksPerCore+math.Max(ticksPerCore*cpuToleranceFraction, 2) {
			continue
		}
		value := roundPercent((busyDelta / ticksPerCore) * 100)
		perCore[index] = &value
	}
	return perCore
}

func cpuIowaitDelta(current, previous *cpuBaseline, capacity, tolerance float64) (float64, bool) {
	if current.total.IOwait < previous.total.IOwait {
		return 0, false
	}
	delta := float64(current.total.IOwait - previous.total.IOwait)
	if delta > capacity+tolerance {
		return 0, false
	}
	return delta, true
}

func coreCounterMap(perCore []CpuCoreCounters) map[int]CpuCounters {
	counters := make(map[int]CpuCounters, len(perCore))
	for _, core := range perCore {
		counters[core.Index] = core.CpuCounters
	}
	return counters
}

func (s *Sampler) sampleMemory() MemoryMetric {
	meminfo, err := ReadMeminfo(s.src)
	if err != nil || meminfo.TotalBytes == nil || meminfo.AvailableBytes == nil {
		return s.memoryUnavailable("memory counters unavailable")
	}
	total := *meminfo.TotalBytes
	available := *meminfo.AvailableBytes
	if total <= 0 || total > protocol.MaxSafeInteger {
		return s.memoryUnavailable("memory total counters unavailable")
	}
	if available < 0 || available > total {
		return s.memoryUnavailable("memory available counters inconsistent")
	}
	used := total - available
	reading := MemoryMetric{
		Status:     StatusOK,
		UsedBytes:  &used,
		TotalBytes: &total,
	}
	if pressure, err := ReadPressure(s.src, "/proc/pressure/memory"); err == nil {
		reading.PressureSomePercent = pressure.SomeAvg10
		reading.PressureFullPercent = pressure.FullAvg10
	}
	return reading
}

func (s *Sampler) sampleSwap() BytesUsageMetric {
	meminfo, err := ReadMeminfo(s.src)
	if err != nil {
		return s.swapUnavailable("swap counters unavailable")
	}
	// SwapTotal is required: a missing field is not the same as a host with
	// swap disabled, and must never become an honest-looking 0/0 reading.
	if meminfo.SwapTotalBytes == nil {
		return s.swapUnavailable("swap total counters unavailable")
	}
	total := *meminfo.SwapTotalBytes
	if total < 0 || total > protocol.MaxSafeInteger {
		return s.swapUnavailable("swap total counters unavailable")
	}
	if total == 0 {
		// Only an explicit SwapTotal: 0 means "no swap"; SwapFree is optional
		// in that case but must not contradict the total.
		if meminfo.SwapFreeBytes != nil && *meminfo.SwapFreeBytes != 0 {
			return s.swapUnavailable("swap counters inconsistent")
		}
		zero := int64(0)
		return BytesUsageMetric{Status: StatusOK, UsedBytes: &zero, TotalBytes: &zero}
	}
	// A non-empty swap area needs its free counter; defaulting it to zero
	// would report the whole area as used.
	if meminfo.SwapFreeBytes == nil {
		return s.swapUnavailable("swap free counters unavailable")
	}
	free := *meminfo.SwapFreeBytes
	if free < 0 || free > total {
		return s.swapUnavailable("swap counters inconsistent")
	}
	used := total - free
	return BytesUsageMetric{Status: StatusOK, UsedBytes: &used, TotalBytes: &total}
}

func (s *Sampler) sampleDisks(at time.Time, clockOK bool) DisksMetric {
	mounts, mountErr := ReadMountinfo(s.src)
	if mountErr != nil {
		mounts = nil
		s.addFailure("disks", nil, "mount reader failed: "+mountErr.Error())
	}
	filesystems := make([]FilesystemReading, 0)
	if mounts == nil {
		s.addFailure("disks", nil, "mount information unavailable")
	} else {
		for _, candidate := range SelectFilesystems(mounts, SelectOptions{AllowFSTypes: s.allowFSTypes}) {
			usage := ReadFilesystemUsage(candidate, s.statfs)
			filesystems = append(filesystems, usage.Reading)
			if usage.Reading.Status == StatusUnavailable {
				s.addFailure("disks", &usage.Reading.ID, usage.Reading.Reason)
			}
		}
	}

	disks := DiscoverDisks(s.src)
	counters, countersErr := ReadDiskstats(s.src)
	if countersErr != nil {
		counters = nil
		s.addFailure("disks", nil, "disk statistics reader failed: "+countersErr.Error())
	}

	devices := make([]DiskIoReading, 0, len(disks))
	if counters == nil {
		for _, disk := range disks {
			devices = append(devices, s.diskUnavailable(disk.ID, "disk statistics unavailable"))
		}
	} else {
		present := make(map[string]bool, len(disks))
		for _, disk := range disks {
			present[disk.Name] = true
		}
		for name := range s.diskBaselines {
			if !present[name] {
				delete(s.diskBaselines, name)
			}
		}
		filtered := DiskCountersFor(counters, disks)
		for _, disk := range disks {
			current, ok := filtered[disk.Name]
			if !ok {
				devices = append(devices, s.diskUnavailable(disk.ID, "disk counters unavailable"))
				continue
			}
			if !clockOK {
				devices = append(devices, s.diskUnavailable(disk.ID, "disk monotonic clock unavailable"))
				continue
			}
			signature := diskSignature(disk)
			previous, hadPrevious := s.diskBaselines[disk.Name]
			s.diskBaselines[disk.Name] = diskBaseline{
				at:           at,
				readSectors:  current.ReadSectors,
				writeSectors: current.WriteSectors,
				readOps:      current.ReadOps,
				writeOps:     current.WriteOps,
				readTimeMs:   current.ReadTimeMs,
				writeTimeMs:  current.WriteTimeMs,
				ioTimeMs:     current.IOTimeMs,
				signature:    signature,
			}
			if !hadPrevious {
				devices = append(devices, s.diskUnavailable(disk.ID, "disk I/O needs two readings; first interval pending"))
				continue
			}
			if previous.signature != signature {
				devices = append(devices, s.diskUnavailable(disk.ID, "disk device changed; baseline reset"))
				continue
			}
			if current.ReadSectors < previous.readSectors || current.WriteSectors < previous.writeSectors {
				devices = append(devices, s.diskUnavailable(disk.ID, "disk counters decreased; baseline reset"))
				continue
			}
			elapsedMs := float64(at.Sub(previous.at)) / float64(time.Millisecond)
			if elapsedMs <= 0 {
				devices = append(devices, s.diskUnavailable(disk.ID, "disk monotonic elapsed did not advance"))
				continue
			}
			readDelta := float64(current.ReadSectors - previous.readSectors)
			writeDelta := float64(current.WriteSectors - previous.writeSectors)
			readRate := roundRate(readDelta * 512 * 1000 / elapsedMs)
			writeRate := roundRate(writeDelta * 512 * 1000 / elapsedMs)
			readLatency := meanLatencyMs(counterDelta(current.ReadOps, previous.readOps), counterDelta(current.ReadTimeMs, previous.readTimeMs))
			writeLatency := meanLatencyMs(counterDelta(current.WriteOps, previous.writeOps), counterDelta(current.WriteTimeMs, previous.writeTimeMs))
			var utilization *float64
			if ioTimeDelta := counterDelta(current.IOTimeMs, previous.ioTimeMs); ioTimeDelta != nil {
				value := roundPercent((float64(*ioTimeDelta) / elapsedMs) * 100)
				utilization = &value
			}
			devices = append(devices, DiskIoReading{
				ID:                   disk.ID,
				Status:               StatusOK,
				ReadBytesPerSec:      &readRate,
				WriteBytesPerSec:     &writeRate,
				ReadLatencyMs:        readLatency,
				WriteLatencyMs:       writeLatency,
				IOUtilizationPercent: utilization,
			})
		}
	}

	if mounts == nil && counters == nil && len(devices) == 0 && len(filesystems) == 0 {
		return s.disksUnavailable("disk discovery unavailable")
	}
	reading := DisksMetric{Status: StatusOK, Filesystems: filesystems, Devices: devices}
	if pressure, err := ReadPressure(s.src, "/proc/pressure/io"); err == nil {
		reading.PressureSomePercent = pressure.SomeAvg10
		reading.PressureFullPercent = pressure.FullAvg10
	}
	return reading
}

func (s *Sampler) sampleNetwork(at time.Time, clockOK bool) NetworkMetric {
	counters, err := ReadNetDev(s.src)
	if err != nil {
		return s.networkUnavailable("network counters unavailable")
	}
	if !clockOK {
		return s.networkUnavailable("network monotonic clock unavailable")
	}
	names := make([]string, 0, len(counters))
	for name := range counters {
		if !interfaceNameRE.MatchString(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	// Match the inventory's deterministic selection: the first 64 names in
	// sorted order, after the same name filter. Rate baselines only follow the
	// selected interfaces so an uncapped host cannot exceed the contract.
	if len(names) > protocol.MaxInterfaces {
		names = names[:protocol.MaxInterfaces]
	}
	present := make(map[string]bool, len(names))
	for _, name := range names {
		present[name] = true
	}
	for name := range s.netBaselines {
		if !present[name] {
			delete(s.netBaselines, name)
		}
	}

	interfaces := make([]InterfaceReading, 0, len(names))
	for _, name := range names {
		current := counters[name]
		previous, hadPrevious := s.netBaselines[name]
		s.netBaselines[name] = netBaseline{at: at, rxBytes: current.RXBytes, txBytes: current.TXBytes}
		id := "net:" + name
		if !hadPrevious {
			interfaces = append(interfaces, s.interfaceUnavailable(id, "network I/O needs two readings; first interval pending"))
			continue
		}
		if current.RXBytes < previous.rxBytes || current.TXBytes < previous.txBytes {
			interfaces = append(interfaces, s.interfaceUnavailable(id, "network counters decreased; baseline reset"))
			continue
		}
		elapsedMs := float64(at.Sub(previous.at)) / float64(time.Millisecond)
		if elapsedMs <= 0 {
			interfaces = append(interfaces, s.interfaceUnavailable(id, "network monotonic elapsed did not advance"))
			continue
		}
		rxRate := roundRate(float64(current.RXBytes-previous.rxBytes) * 1000 / elapsedMs)
		txRate := roundRate(float64(current.TXBytes-previous.txBytes) * 1000 / elapsedMs)
		interfaces = append(interfaces, InterfaceReading{
			ID:            id,
			Status:        StatusOK,
			RXBytesPerSec: &rxRate,
			TXBytesPerSec: &txRate,
		})
	}
	return NetworkMetric{Status: StatusOK, Interfaces: interfaces}
}

func (s *Sampler) cpuUnavailable(reason string) CpuMetric {
	return CpuMetric{Status: StatusUnavailable, Reason: s.failureReason("cpu", nil, reason)}
}

func (s *Sampler) memoryUnavailable(reason string) MemoryMetric {
	return MemoryMetric{Status: StatusUnavailable, Reason: s.failureReason("memory", nil, reason)}
}

func (s *Sampler) swapUnavailable(reason string) BytesUsageMetric {
	return BytesUsageMetric{Status: StatusUnavailable, Reason: s.failureReason("swap", nil, reason)}
}

func (s *Sampler) disksUnavailable(reason string) DisksMetric {
	return DisksMetric{Status: StatusUnavailable, Reason: s.failureReason("disks", nil, reason)}
}

func (s *Sampler) diskUnavailable(id, reason string) DiskIoReading {
	return DiskIoReading{ID: id, Status: StatusUnavailable, Reason: s.failureReason("disks", &id, reason)}
}

func (s *Sampler) networkUnavailable(reason string) NetworkMetric {
	return NetworkMetric{Status: StatusUnavailable, Reason: s.failureReason("network", nil, reason)}
}

func (s *Sampler) interfaceUnavailable(id, reason string) InterfaceReading {
	return InterfaceReading{ID: id, Status: StatusUnavailable, Reason: s.failureReason("network", &id, reason)}
}

// failureReason records a deduplicated, capped failure and returns the capped
// reason for the reading.
func (s *Sampler) failureReason(metric string, resourceID *string, reason string) string {
	capped := CapReason(reason)
	s.addFailure(metric, resourceID, capped)
	return capped
}

func (s *Sampler) addFailure(metric string, resourceID *string, reason string) {
	if len(s.failures) >= protocol.MaxCollectionFailures {
		return
	}
	capped := CapReason(reason)
	key := metric + "\x00" + capped
	if resourceID != nil {
		key = metric + "\x00" + *resourceID + "\x00" + capped
	}
	if s.seenFailures == nil {
		s.seenFailures = make(map[string]bool)
	}
	if s.seenFailures[key] {
		return
	}
	s.seenFailures[key] = true
	s.failures = append(s.failures, CollectionFailure{Metric: metric, ResourceID: resourceID, Reason: capped})
}

func counterDelta(current, previous *uint64) *uint64 {
	if current == nil || previous == nil || *current < *previous {
		return nil
	}
	delta := *current - *previous
	return &delta
}

// meanLatencyMs returns the mean time per completed operation. No completed
// operations, or a mean beyond the contract's plausible ceiling, means unknown,
// not 0 ms.
func meanLatencyMs(operations, timeMs *uint64) *float64 {
	if operations == nil || timeMs == nil || *operations == 0 {
		return nil
	}
	value := math.Round((float64(*timeMs)/float64(*operations))*1000) / 1000
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > maxLatencyMs {
		return nil
	}
	return &value
}

func clampPercent(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Min(100, math.Max(0, value))
}

func roundPercent(value float64) float64 {
	return math.Round(clampPercent(value)*100) / 100
}

func roundRate(bytes float64) int64 {
	if math.IsNaN(bytes) || math.IsInf(bytes, 0) || bytes < 0 {
		return 0
	}
	return int64(math.Round(math.Min(maxRateBytesPerSec, bytes)))
}
