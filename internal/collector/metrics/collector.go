package metrics

import (
	"errors"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/policy"
	"github.com/Triet1402/rigscope/internal/collector/sampling/gpu"
	"github.com/Triet1402/rigscope/internal/collector/sampling/procfs"
	"github.com/Triet1402/rigscope/internal/protocol"
)

const (
	// samplerPanicReason explains an unavailable family whose host sampler
	// panicked. The panic value is deliberately not forwarded: it can contain
	// paths or other host details the sharing policy never reviewed.
	samplerPanicReason = "host sampling panicked; readings unavailable"
	// gpuPanicReason explains an unavailable GPU family or device.
	gpuPanicReason = "gpu sampling panicked; readings unavailable"
)

// Identity is the record envelope identity of one collector run.
type Identity struct {
	CollectorID string
	RunID       string
	BootID      string
}

// GPUSource is the assembler's view of the GPU package's poller. The poller's
// snapshots and the assembler's clock must share one monotonic origin; build
// the poller with MonotonicDuration(clock).
type GPUSource interface {
	// Latest returns the last completed polling cycle, or nil.
	Latest() *gpu.Snapshot
	// Available reports whether nvidia-smi can be used.
	Available() gpu.Availability
	// PollInterval is the configured polling cadence.
	PollInterval() time.Duration
	// DeviceInfo returns the slow-changing NVIDIA device descriptions.
	DeviceInfo() []gpu.DeviceInfo
}

// ProcSampler is the assembler's view of procfs.Sampler. Tests substitute a
// fixed implementation; production uses NewProcFSSampler.
type ProcSampler interface {
	Sample() procfs.Readings
	Inventory() procfs.HostInventory
	Failures() []procfs.CollectionFailure
}

// Options injects the Collector's dependencies.
type Options struct {
	Identity Identity
	// Sequence is the run's single record allocator. Required: sharing it with
	// gap records is what keeps (runId, sequence) unique.
	Sequence Sequencer
	// Clock is the shared wall+monotonic clock. Default NewSystemClock().
	Clock Clock
	// Proc samples the host. Required unless the caller uses another sampler
	// implementation; the Collector does not silently fall back to /proc so a
	// wiring mistake is visible at construction.
	Proc ProcSampler
	// GPU is optional; nil means no NVIDIA tooling.
	GPU GPUSource
	// GPUFS is the sysfs view used for PCI/DRM GPU inventory and AMD/Intel
	// readings. Default gpu.HostFS().
	GPUFS gpu.SysFS
	// Gate is the optional runtime-PM gate shared with the GPU poller.
	Gate gpu.PowerGate
	// Policy supplies the local sharing policy. It is re-read every tick so a
	// narrowed policy takes effect on the next assembled record. nil selects
	// policy.Default(); a panicking source fails closed.
	Policy func() policy.Policy
	// GPUStale overrides the cache-age limit. Zero selects
	// max(2×poller interval, 10s).
	GPUStale time.Duration
}

// Collector assembles one record per tick and the current machine inventory.
// It is not safe for concurrent use: one sampling loop owns it, as with
// procfs.Sampler.
type Collector struct {
	identity Identity
	sequence Sequencer
	clock    Clock
	proc     ProcSampler
	gpu      GPUSource
	gpuFS    gpu.SysFS
	gate     gpu.PowerGate
	policy   func() policy.Policy
	gpuStale time.Duration

	failures       []protocol.CollectionFailure
	seenFailures   map[string]bool
	lastInventory  protocol.Inventory
	lastSignature  string
	inventoryCount int64
	pciNames       string
	pciNamesRead   bool
}

// NewCollector validates the options and returns a Collector.
func NewCollector(options Options) (*Collector, error) {
	if options.Identity.CollectorID == "" {
		return nil, errors.New("metrics: Identity.CollectorID is required")
	}
	if options.Identity.RunID == "" {
		return nil, errors.New("metrics: Identity.RunID is required")
	}
	if options.Sequence == nil {
		return nil, errors.New("metrics: Options.Sequence is required")
	}
	if options.Proc == nil {
		return nil, errors.New("metrics: Options.Proc is required")
	}
	clock := options.Clock
	if clock == nil {
		clock = NewSystemClock()
	}
	gpuFS := options.GPUFS
	if gpuFS == nil {
		gpuFS = gpu.HostFS()
	}
	stale := options.GPUStale
	if stale <= 0 {
		interval := 2 * time.Second
		if options.GPU != nil {
			if reported := options.GPU.PollInterval(); reported > 0 {
				interval = reported
			}
		}
		stale = 2 * interval
		if stale < 10*time.Second {
			stale = 10 * time.Second
		}
	}
	return &Collector{
		identity: options.Identity,
		sequence: options.Sequence,
		clock:    clock,
		proc:     options.Proc,
		gpu:      options.GPU,
		gpuFS:    gpuFS,
		gate:     options.Gate,
		policy:   options.Policy,
		gpuStale: stale,
	}, nil
}

// Next samples one tick and returns the assembled metrics record with the
// local sharing policy applied. Inventory is refreshed first, so GPU inventory
// joins use the current machine state, exactly like the prototype sampler.
// A sampler that panics degrades only the families it feeds: the tick still
// produces a record with explicit unavailable readings instead of escaping and
// killing the sampling loop.
func (c *Collector) Next() (protocol.MetricsRecord, error) {
	sharing := c.currentPolicy()
	rawInventory := c.refreshInventory(sharing)

	c.failures = nil
	c.seenFailures = make(map[string]bool)
	readings, sampled := c.sampleProc()
	if !sampled {
		for _, family := range protocol.MetricFamilies {
			c.addFailure(string(family), nil, samplerPanicReason)
		}
	}
	for _, failure := range ToProtocolFailures(c.safeProcFailures()) {
		c.addFailure(failure.Metric, failure.ResourceID, failure.Reason)
	}
	gpus := c.sampleGPUs(rawInventory)
	payload := ToProtocolMetrics(readings, gpus)
	payload = policy.ApplyMetricsPayload(payload, sharing)

	capturedAt, err := FormatCapturedAt(c.clock.Now())
	if err != nil {
		return protocol.MetricsRecord{}, err
	}
	sequence, err := c.sequence.Next()
	if err != nil {
		return protocol.MetricsRecord{}, err
	}
	return protocol.MetricsRecord{
		RecordEnvelope: protocol.RecordEnvelope{
			CollectorID: c.identity.CollectorID,
			RunID:       c.identity.RunID,
			BootID:      c.identity.BootID,
			Sequence:    sequence,
			CapturedAt:  capturedAt,
		},
		Payload: payload,
	}, nil
}

// Inventory refreshes and returns the current machine inventory with the
// sharing policy applied.
func (c *Collector) Inventory() protocol.Inventory {
	sharing := c.currentPolicy()
	return policy.FilterInventory(c.refreshInventory(sharing), sharing)
}

// InventoryVersion counts inventory changes. It increments only when the
// policy-filtered signature changes, so the runtime can send inventory only
// when it differs from what the hub already has.
func (c *Collector) InventoryVersion() int64 {
	return c.inventoryCount
}

// Failures returns the collection failures observed during the last Next
// call, bounded to the protocol limit and filtered by the current sharing
// policy: a withheld family must not leak its resource ids or reasons.
func (c *Collector) Failures() []protocol.CollectionFailure {
	return policy.FilterFailures(c.failures, c.currentPolicy())
}

// sampleProc samples the host sampler, turning a panic into unavailable
// readings for every family the sample feeds.
func (c *Collector) sampleProc() (readings procfs.Readings, ok bool) {
	defer func() {
		if recover() != nil {
			readings = unavailableReadings(samplerPanicReason)
			ok = false
		}
	}()
	return c.proc.Sample(), true
}

// safeProcFailures reads the sampler's failure list without letting a panic
// escape the tick. A failure list is diagnostic; losing it is better than
// losing the record.
func (c *Collector) safeProcFailures() (failures []procfs.CollectionFailure) {
	defer func() {
		if recover() != nil {
			failures = nil
		}
	}()
	return c.proc.Failures()
}

// unavailableReadings is the honest non-GPU reading set for a failed sample:
// every family carries a reason and no values.
func unavailableReadings(reason string) procfs.Readings {
	return procfs.Readings{
		CPU:     procfs.CpuMetric{Status: procfs.StatusUnavailable, Reason: reason},
		Memory:  procfs.MemoryMetric{Status: procfs.StatusUnavailable, Reason: reason},
		Swap:    procfs.BytesUsageMetric{Status: procfs.StatusUnavailable, Reason: reason},
		Disks:   procfs.DisksMetric{Status: procfs.StatusUnavailable, Reason: reason},
		Network: procfs.NetworkMetric{Status: procfs.StatusUnavailable, Reason: reason},
	}
}

// refreshInventory rebuilds raw inventory, records a version bump when the
// filtered form changed and returns the raw (unfiltered) inventory used for
// GPU joins.
func (c *Collector) refreshInventory(sharing policy.Policy) protocol.Inventory {
	raw := c.buildInventory()
	filtered := policy.FilterInventory(raw, sharing)
	signature := inventorySignature(filtered)
	if signature != c.lastSignature {
		c.inventoryCount++
		c.lastSignature = signature
	}
	c.lastInventory = raw
	return raw
}

func (c *Collector) currentPolicy() (sharing policy.Policy) {
	if c.policy == nil {
		return policy.Default()
	}
	defer func() {
		if recover() != nil {
			// A broken policy source fails closed, like the process scheduler.
			sharing = policy.Policy{}
		}
	}()
	return c.policy()
}

func (c *Collector) addFailure(metric string, resourceID *string, reason string) {
	if len(c.failures) >= protocol.MaxCollectionFailures {
		return
	}
	metric = boundedText(metric, 64)
	reason = boundedText(reason, protocol.MaxTextLength)
	if resourceID != nil {
		bounded := boundedText(*resourceID, protocol.MaxResourceIDLength)
		resourceID = &bounded
	}
	key := metric + "\x00" + reason
	if resourceID != nil {
		key = metric + "\x00" + *resourceID + "\x00" + reason
	}
	if c.seenFailures[key] {
		return
	}
	c.seenFailures[key] = true
	c.failures = append(c.failures, protocol.CollectionFailure{
		Metric:     metric,
		ResourceID: resourceID,
		Reason:     reason,
	})
}
