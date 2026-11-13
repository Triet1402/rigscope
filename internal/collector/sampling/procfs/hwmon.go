package procfs

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	hwmonRoot = "/sys/class/hwmon"
	raplRoot  = "/sys/class/powercap"

	maxHwmonDevices = 64
	maxHwmonTemps   = 32
	maxRaplDomains  = 16
	// maxPlausibleWatts is the contract ceiling for a CPU package reading.
	maxPlausibleWatts = 10_000
	// wrapMinPreviousFraction: a real RAPL wrap is seen just after the
	// previous sample approached the range ceiling. A decrease from anywhere
	// else is a counter reset, not a wrap.
	wrapMinPreviousFraction = 0.9
)

var (
	hwmonDeviceRE = regexp.MustCompile(`^hwmon(\d+)$`)
	hwmonTempRE   = regexp.MustCompile(`^temp(\d+)_input$`)
	raplDomainRE  = regexp.MustCompile(`^intel-rapl:\d+$`)
)

// cpuHwmonNames are the hwmon drivers that describe CPU thermal sensors.
var cpuHwmonNames = map[string]bool{
	"coretemp":    true,
	"k10temp":     true,
	"zenpower":    true,
	"cpu_thermal": true,
	"soc_thermal": true,
}

// ReadCPUTemperature returns the best available CPU temperature in Celsius.
// The most specific label class wins (package/Tctl/Tdie, then core, then an
// unlabelled CPU sensor); within a class the hottest socket/core is reported.
// nil means no CPU hwmon sensor is present or readable.
func ReadCPUTemperature(src Source) *float64 {
	entries, err := src.ReadDir(hwmonRoot)
	if err != nil {
		return nil
	}
	devices := make([]string, 0, len(entries))
	for _, entry := range entries {
		if hwmonDeviceRE.MatchString(entry) {
			devices = append(devices, entry)
		}
	}
	sort.SliceStable(devices, func(i, j int) bool { return hwmonOrder(devices[i]) < hwmonOrder(devices[j]) })
	if len(devices) > maxHwmonDevices {
		devices = devices[:maxHwmonDevices]
	}

	type candidate struct {
		rank    int
		celsius float64
	}
	var candidates []candidate
	for _, device := range devices {
		base := hwmonRoot + "/" + device
		driver := strings.TrimSpace(readTextOrZero(src, base+"/name"))
		if !cpuHwmonNames[strings.ToLower(driver)] {
			continue
		}
		files, err := src.ReadDir(base)
		if err != nil {
			continue
		}
		type tempInput struct {
			name  string
			index int
		}
		var inputs []tempInput
		for _, file := range files {
			match := hwmonTempRE.FindStringSubmatch(file)
			if match == nil {
				continue
			}
			index, err := strconv.Atoi(match[1])
			if err != nil {
				continue
			}
			inputs = append(inputs, tempInput{name: file, index: index})
		}
		sort.SliceStable(inputs, func(i, j int) bool { return inputs[i].index < inputs[j].index })
		if len(inputs) > maxHwmonTemps {
			inputs = inputs[:maxHwmonTemps]
		}
		for _, input := range inputs {
			celsius, ok := readCandidateTemperature(src, base+"/"+input.name)
			if !ok {
				continue
			}
			label := readTextOrZero(src, base+"/temp"+strconv.Itoa(input.index)+"_label")
			rank := labelRank(label)
			candidates = append(candidates, candidate{rank: rank, celsius: celsius})
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	bestRank := candidates[0].rank
	for _, candidate := range candidates[1:] {
		if candidate.rank < bestRank {
			bestRank = candidate.rank
		}
	}
	hottest := math.Inf(-1)
	for _, candidate := range candidates {
		if candidate.rank == bestRank && candidate.celsius > hottest {
			hottest = candidate.celsius
		}
	}
	value := round2(hottest)
	return &value
}

func hwmonOrder(name string) int {
	match := hwmonDeviceRE.FindStringSubmatch(name)
	if match == nil {
		return math.MaxInt
	}
	order, err := strconv.Atoi(match[1])
	if err != nil {
		return math.MaxInt
	}
	return order
}

// labelRank orders temperature labels by how well they describe a package.
func labelRank(label string) int {
	text := strings.ToLower(strings.TrimSpace(label))
	if text == "" {
		return 2
	}
	if strings.Contains(text, "package") || strings.HasPrefix(text, "tctl") || strings.HasPrefix(text, "tdie") {
		return 0
	}
	if text == "core" || strings.HasPrefix(text, "core ") {
		return 1
	}
	// A known CPU driver may expose other legitimate die/CCD labels.
	return 3
}

func readCandidateTemperature(src Source, path string) (float64, bool) {
	text := strings.TrimSpace(readTextOrZero(src, path))
	if text == "" {
		return 0, false
	}
	millidegrees, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(millidegrees) || math.IsInf(millidegrees, 0) {
		return 0, false
	}
	celsius := millidegrees / 1000
	if math.IsNaN(celsius) || math.IsInf(celsius, 0) || celsius < -50 || celsius > 200 {
		return 0, false
	}
	return round2(celsius), true
}

type raplBaseline struct {
	energyUj uint64
	at       time.Time
}

type raplReading struct {
	energyUj   uint64
	maxRangeUj *uint64
}

// RaplPowerMeter derives package watts from top-level RAPL energy counters.
// Each top-level package contributes its own delta, so multiple sockets are
// summed while subdomains are never added to their parent.
type RaplPowerMeter struct {
	src       Source
	baselines map[string]raplBaseline
}

// NewRaplPowerMeter returns a meter reading /sys/class/powercap through src.
func NewRaplPowerMeter(src Source) *RaplPowerMeter {
	return &RaplPowerMeter{src: src, baselines: make(map[string]raplBaseline)}
}

// Sample returns package watts for this interval, or nil while warming up or
// when the interface is unavailable. haveClock mirrors the sampler's clock
// availability; a missing clock must not advance a baseline.
func (m *RaplPowerMeter) Sample(at time.Time, haveClock bool) *float64 {
	if !haveClock {
		return nil
	}
	entries, err := m.src.ReadDir(raplRoot)
	if err != nil {
		m.baselines = make(map[string]raplBaseline)
		return nil
	}
	domains := make([]string, 0, len(entries))
	for _, entry := range entries {
		if raplDomainRE.MatchString(entry) {
			domains = append(domains, entry)
		}
	}
	sort.Strings(domains)
	if len(domains) > maxRaplDomains {
		domains = domains[:maxRaplDomains]
	}

	current := make(map[string]raplReading, len(domains))
	for _, domain := range domains {
		base := raplRoot + "/" + domain
		energyUj, ok := readCounter(m.src, base+"/energy_uj")
		// An unreadable package counter (often root-only) or one package
		// missing makes whole-package power unknown rather than partial.
		if !ok {
			m.baselines = make(map[string]raplBaseline)
			return nil
		}
		var maxRangeUj *uint64
		if value, ok := readCounter(m.src, base+"/max_energy_range_uj"); ok {
			maxRangeUj = &value
		}
		current[domain] = raplReading{energyUj: energyUj, maxRangeUj: maxRangeUj}
	}
	if len(current) == 0 {
		m.baselines = make(map[string]raplBaseline)
		return nil
	}

	totalWatts := 0.0
	ready := true
	for _, domain := range domains {
		reading := current[domain]
		previous, existed := m.baselines[domain]
		m.baselines[domain] = raplBaseline{energyUj: reading.energyUj, at: at}
		if !existed {
			ready = false
			continue
		}
		elapsed := at.Sub(previous.at)
		if elapsed <= 0 {
			ready = false
			continue
		}
		deltaUj := reading.energyUj - previous.energyUj
		if reading.energyUj < previous.energyUj {
			if reading.maxRangeUj == nil ||
				*reading.maxRangeUj <= reading.energyUj ||
				previous.energyUj > *reading.maxRangeUj ||
				float64(previous.energyUj) < float64(*reading.maxRangeUj)*wrapMinPreviousFraction {
				ready = false
				continue
			}
			deltaUj = *reading.maxRangeUj - previous.energyUj + reading.energyUj
		}
		// 1 J = 1e6 uJ; watts = joules / seconds.
		watts := float64(deltaUj) / (elapsed.Seconds() * 1e6)
		if math.IsNaN(watts) || math.IsInf(watts, 0) || watts < 0 || watts > maxPlausibleWatts {
			// Most often a counter reset that looked like a wrap; restart cleanly.
			ready = false
			continue
		}
		totalWatts += watts
	}

	for domain := range m.baselines {
		if _, ok := current[domain]; !ok {
			delete(m.baselines, domain)
		}
	}
	if !ready {
		return nil
	}
	value := round2(totalWatts)
	return &value
}

func readCounter(src Source, path string) (uint64, bool) {
	text := strings.TrimSpace(readTextOrZero(src, path))
	if text == "" {
		return 0, false
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// round2 rounds to two decimals with JavaScript's Math.round semantics:
// halves go toward +Infinity. Go's math.Round rounds halves away from zero,
// which would turn -0.005 °C into -0.01 instead of the 0.00 the TS collector
// reports.
func round2(value float64) float64 {
	return jsRound(value*100) / 100
}

// jsRound matches Math.round for finite values: floor(x + 0.5). NaN and
// infinities are passed through so callers' own range checks still reject them.
func jsRound(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return value
	}
	return math.Floor(value + 0.5)
}
