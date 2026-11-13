package storage

import (
	"sort"
	"sync"
	"time"
)

const (
	defaultRingRetention = 15 * time.Minute
	defaultRingMachines  = 64
	defaultRingBytes     = 64 * 1024 * 1024
	ringStepMS           = int64(1000)
	ringBucketBytes      = int64(48)
	ringSeriesOverhead   = int64(96)
)

// RingOptions configures the in-memory one-second history ring. Retention is
// the per-machine window; MaxMachines and MaxBytes bound the whole hub so a
// machine that grows hundreds of disks or cores cannot exhaust memory.
type RingOptions struct {
	Retention   time.Duration
	MaxMachines int
	MaxBytes    int64
}

// RingStats reports the ring's bounded memory use.
type RingStats struct {
	Retention       time.Duration
	StepMS          int64
	Capacity        int
	BudgetBytes     int64
	UsedBytes       int64
	Machines        int
	Series          int
	EvictedMachines int64
	EvictedSeries   int64
	RejectedSeries  int64
	OldestMs        int64
	NewestMs        int64
}

type ringPoint struct {
	machineID  string
	metric     string
	resourceID string
	atMs       int64
	value      *float64
}

type ringBucket struct {
	stamp int64 // bucket index + 1; zero means empty
	min   float64
	max   float64
	sum   float64
	count uint32
	nulls uint32
}

type ringSeries struct {
	metric     string
	resourceID string
	buckets    []ringBucket
	lastIndex  int64
	// earliestMs is the oldest timestamp this series ever stored since the
	// process started. Together with the oldest retained slot it proves
	// whether the ring covers a requested range instead of only part of it.
	earliestMs int64
}

type ringMachine struct {
	series   map[string]*ringSeries
	lastSeen uint64
}

// Ring is the bounded in-memory 1-second summary history used for the most
// recent minutes. Buckets keep min, max, sum, count and explicit unavailable
// count, so spikes and unknown readings survive at full resolution. It is not
// durable: a restart loses the ring together with the live snapshot, while the
// persistent summary tiers keep the coarser history.
type Ring struct {
	mu              sync.Mutex
	opts            RingOptions
	order           uint64
	bytes           int64
	machines        map[string]*ringMachine
	evictedMachines int64
	evictedSeries   int64
	rejectedSeries  int64
}

// NewRing creates an empty ring. A zero-value option field takes its default.
func NewRing(opts RingOptions) *Ring {
	if opts.Retention <= 0 {
		opts.Retention = defaultRingRetention
	}
	if opts.MaxMachines <= 0 {
		opts.MaxMachines = defaultRingMachines
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultRingBytes
	}
	if opts.Retention < time.Second {
		opts.Retention = time.Second
	}
	return &Ring{opts: opts, machines: make(map[string]*ringMachine)}
}

// Enabled reports whether the ring retains any data.
func (r *Ring) Enabled() bool { return r != nil && r.opts.Retention > 0 }

// Retention is the per-machine ring window.
func (r *Ring) Retention() time.Duration { return r.opts.Retention }

// Capacity is the number of one-second slots per series.
func (r *Ring) Capacity() int {
	return int((r.opts.Retention + time.Second - 1) / time.Second)
}

func (r *Ring) seriesBytes() int64 {
	return int64(r.Capacity())*ringBucketBytes + ringSeriesOverhead
}

// Add folds one reading into the machine's ring. A nil value is an explicit
// unavailable reading.
func (r *Ring) Add(machineID, metric, resourceID string, atMs int64, value *float64) {
	if r == nil || !r.Enabled() || machineID == "" || metric == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order++
	machine := r.machines[machineID]
	if machine == nil {
		if len(r.machines) >= r.opts.MaxMachines {
			r.evictOldestMachineLocked("")
		}
		machine = &ringMachine{series: make(map[string]*ringSeries)}
		r.machines[machineID] = machine
	}
	machine.lastSeen = r.order

	key := metric + "\x00" + resourceID
	series := machine.series[key]
	if series == nil {
		needed := r.seriesBytes()
		for r.bytes+needed > r.opts.MaxBytes {
			if !r.evictOldestMachineLocked(machineID) {
				r.rejectedSeries++
				return
			}
		}
		series = &ringSeries{
			metric:     metric,
			resourceID: resourceID,
			buckets:    make([]ringBucket, r.Capacity()),
		}
		machine.series[key] = series
		r.bytes += needed
	}

	index := atMs / ringStepMS
	// Samples older than the retained window are late and dropped. Without
	// this guard an old replay would land in the slot of a much newer bucket
	// and erase it.
	if series.lastIndex != 0 && index < series.lastIndex-int64(len(series.buckets))+1 {
		return
	}
	if index > series.lastIndex {
		series.lastIndex = index
	}
	if series.earliestMs == 0 || atMs < series.earliestMs {
		series.earliestMs = atMs
	}
	slot := int(((index % int64(len(series.buckets))) + int64(len(series.buckets))) % int64(len(series.buckets)))
	bucket := &series.buckets[slot]
	if bucket.stamp != index+1 {
		*bucket = ringBucket{stamp: index + 1}
	}
	if value == nil {
		bucket.nulls++
		return
	}
	if bucket.count == 0 {
		bucket.min = *value
		bucket.max = *value
	} else {
		if *value < bucket.min {
			bucket.min = *value
		}
		if *value > bucket.max {
			bucket.max = *value
		}
	}
	bucket.sum += *value
	bucket.count++
}

// Apply folds a committed flush into the ring. It runs only after COMMIT, so a
// failed transaction never leaves ring-only samples behind.
func (r *Ring) Apply(points []ringPoint) {
	for _, point := range points {
		r.Add(point.machineID, point.metric, point.resourceID, point.atMs, point.value)
	}
}

func (r *Ring) evictOldestMachineLocked(except string) bool {
	var (
		oldestID    string
		oldestOrder uint64
		found       bool
	)
	for id, machine := range r.machines {
		if id == except {
			continue
		}
		if !found || machine.lastSeen < oldestOrder {
			oldestID, oldestOrder, found = id, machine.lastSeen, true
		}
	}
	if !found {
		return false
	}
	evicted := int64(len(r.machines[oldestID].series))
	r.bytes -= evicted * r.seriesBytes()
	delete(r.machines, oldestID)
	r.evictedMachines++
	r.evictedSeries += evicted
	return true
}

// read returns the retained buckets for one series inside [fromMs, toMs].
func (r *Ring) read(machineID, metric, resourceID string, fromMs, toMs int64) []ringBucket {
	if r == nil || !r.Enabled() {
		return nil
	}
	r.mu.Lock()
	machine := r.machines[machineID]
	var series *ringSeries
	if machine != nil {
		series = machine.series[metric+"\x00"+resourceID]
	}
	var buckets []ringBucket
	if series != nil {
		fromIndex := fromMs / ringStepMS
		toIndex := toMs / ringStepMS
		oldestIndex := series.lastIndex - int64(len(series.buckets)) + 1
		for _, bucket := range series.buckets {
			if bucket.stamp == 0 {
				continue
			}
			index := bucket.stamp - 1
			if index < fromIndex || index > toIndex || index < oldestIndex {
				continue
			}
			if bucket.count == 0 && bucket.nulls == 0 {
				continue
			}
			buckets = append(buckets, bucket)
		}
	}
	r.mu.Unlock()
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].stamp < buckets[j].stamp })
	return buckets
}

// covers reports whether every requested series has retained ring data from
// fromMs onward. A partially populated ring (a fresh process, an evicted
// series or a machine that started sampling late) must not answer a wider
// query, because the durable summary tiers still hold the older history.
func (r *Ring) covers(machineID string, series []KnownSeries, fromMs int64) bool {
	if r == nil || !r.Enabled() || len(series) == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	machine := r.machines[machineID]
	if machine == nil {
		return false
	}
	for _, item := range series {
		stored := machine.series[item.Metric+"\x00"+item.ResourceID]
		if stored == nil {
			return false
		}
		oldestRetained := (stored.lastIndex - int64(len(stored.buckets)) + 1) * ringStepMS
		earliest := stored.earliestMs
		if oldestRetained > earliest {
			earliest = oldestRetained
		}
		if earliest > fromMs {
			return false
		}
	}
	return true
}

func (r *Ring) countRows(machineID string, series []KnownSeries, fromMs, toMs int64, budget int) int {
	if r == nil || !r.Enabled() {
		return 0
	}
	total := 0
	for _, item := range series {
		total += len(r.read(machineID, item.Metric, item.ResourceID, fromMs, toMs))
		if total > budget {
			return total
		}
	}
	return total
}

// Seed makes a series visible in the ring's bounded bookkeeping without
// recording a point. It is used when a query needs to count ring rows for a
// series that has no recent data.
func (r *Ring) Stats() RingStats {
	if r == nil {
		return RingStats{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stats := RingStats{
		Retention:       r.opts.Retention,
		StepMS:          ringStepMS,
		Capacity:        r.Capacity(),
		BudgetBytes:     r.opts.MaxBytes,
		UsedBytes:       r.bytes,
		Machines:        len(r.machines),
		EvictedMachines: r.evictedMachines,
		EvictedSeries:   r.evictedSeries,
		RejectedSeries:  r.rejectedSeries,
	}
	oldest, newest := int64(0), int64(0)
	for _, machine := range r.machines {
		for _, series := range machine.series {
			stats.Series++
			for _, bucket := range series.buckets {
				if bucket.stamp == 0 || (bucket.count == 0 && bucket.nulls == 0) {
					continue
				}
				at := (bucket.stamp - 1) * ringStepMS
				if oldest == 0 || at < oldest {
					oldest = at
				}
				if at > newest {
					newest = at
				}
			}
		}
	}
	stats.OldestMs = oldest
	stats.NewestMs = newest
	return stats
}
