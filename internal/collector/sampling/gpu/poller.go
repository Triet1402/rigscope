package gpu

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

const (
	defaultCommand       = "nvidia-smi"
	defaultTimeout       = 3 * time.Second
	minPollInterval      = time.Second
	maxBackoff           = 5 * time.Minute
	notFoundRetryDelay   = 5 * time.Minute
	computeAppsEveryNthQ = 3
)

// PowerSource pairs a gate with the PCI addresses of the devices it governs,
// in the order they should appear in the sample.
type PowerSource struct {
	Gate PowerGate
	// Devices reports the inventory addresses the gate governs. A nil
	// function with a non-nil Gate is the "discovery failed" case: the plan
	// is empty and restricted, never ungated.
	Devices func() []string
	// Access is the per-device arbiter every hardware operation consults.
	// When nil the poller reuses the gate's arbiter (if it has one) or
	// creates a private one.
	Access *AccessArbiter
}

// PollerOptions configures a [Poller]. Zero values select the production
// defaults: the real nvidia-smi command, a 3 s timeout and a real clock.
type PollerOptions struct {
	// Interval is the collector cadence; the effective interval is at least
	// one second so polling cannot hammer the GPU.
	Interval time.Duration
	// Command is the executable, "nvidia-smi" by default.
	Command string
	// Timeout bounds one invocation, 3 s by default.
	Timeout time.Duration
	// Runner executes commands; [ExecRunner] by default.
	Runner Runner
	// Monotonic is the injectable clock used for snapshots and cost stats.
	Monotonic func() time.Duration
	// OnEvent receives short diagnostic lines.
	OnEvent func(string)
	// SensorSource is optional; nil disables register temperatures.
	SensorSource SensorSource
	// Power is optional; nil means every GPU is queried every cycle.
	Power *PowerSource
}

// Poller owns the nvidia-smi cache.
//
// It runs cycles on its own goroutine after [Poller.Start], exposes the last
// completed snapshot through [Poller.Latest], and at most one query is ever
// in flight: concurrent [Poller.PollOnce] calls share the running cycle.
// Failures back off exponentially, capped at five minutes; a missing
// nvidia-smi disables normal polling but is retried every five minutes in
// case the driver is installed later.
type Poller struct {
	pollInterval time.Duration
	timeout      time.Duration
	command      string
	runner       Runner
	monotonic    func() time.Duration
	onEvent      func(string)
	sensor       SensorSource
	power        *PowerSource
	// access is the per-device arbiter shared with the sensor source. nil
	// means no runtime-PM gate is configured and nothing is revocable.
	access *AccessArbiter

	cycleMu sync.Mutex
	// inflight is the cycle concurrent callers wait on; nil when idle.
	inflight *cycleRun

	// sensorMu guards sensorRun, the single register-temperature operation the
	// poller allows in flight.
	sensorMu  sync.Mutex
	sensorRun *sensorCycle

	// authorization is the live decision every register access consults. It
	// outlives one cycle so a read that outlived its settle deadline stops as
	// soon as a later gate decision narrows the permitted set.
	authorization *liveAuthorization

	lifeMu sync.Mutex
	// cancel stops the scheduling loop; rootCancel stops everything the
	// poller owns, including a sensor cycle started by a manual PollOnce.
	cancel     context.CancelFunc
	rootCtx    context.Context
	rootCancel context.CancelFunc
	done       chan struct{}

	mu                  sync.Mutex
	snapshot            *Snapshot
	info                map[string]DeviceInfo
	apps                map[int]int64
	availability        Availability
	stats               CostStats
	totalDuration       time.Duration
	consecutiveFailures int
	gpuQueries          int
	lastDeviceCount     int
	settleTimeoutSeen   bool
}

// cycleRun lets waiters observe the exact cycle they joined. cancel belongs to
// the poller, not to the caller that happened to start the cycle, so [Poller.Stop]
// can abort a manual query too.
type cycleRun struct {
	done   chan struct{}
	cancel context.CancelFunc
	err    error
}

// sensorCycle is one register-temperature operation. A cycle that outlives its
// settle deadline keeps running, and later polling cycles reuse it instead of
// starting another: a stuck BAR therefore costs one goroutine, not one per
// interval.
type sensorCycle struct {
	done       chan struct{}
	cancel     context.CancelFunc
	generation uint64
}

// leaseSet holds the access leases of one launched command covering several
// devices. The set is handed to the command's reaper, which releases it only
// after the child has actually been reaped: while an unreaped child may still
// be touching the devices, they stay non-drainable and the runtime-PM gate
// reports them busy instead of starting a quiet window. The goroutine that
// created the set defers [leaseSet.ReleaseIfOwned] while it still owns it, so a
// panic on any path that never reached the reaper cannot orphan a lease;
// [leaseSet.transfer] disarms that safety net once the reaper owns it. Release
// is idempotent and safe from the reaper goroutine.
type leaseSet struct {
	mu          sync.Mutex
	entries     []leaseEntry
	released    bool
	transferred bool
}

type leaseEntry struct {
	address string
	lease   *AccessLease
}

func (s *leaseSet) add(address string, lease *AccessLease) {
	if s == nil || lease == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		// The set already settled; a lease added afterwards would leak.
		lease.Release()
		return
	}
	s.entries = append(s.entries, leaseEntry{address: address, lease: lease})
}

// lease returns the lease held for an address, or nil.
func (s *leaseSet) lease(address string) *AccessLease {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.entries {
		if entry.address == address {
			return entry.lease
		}
	}
	return nil
}

// releaseAddress returns one device's lease without touching the others. It is
// used when the pre-access re-check refuses a single device before dispatch.
func (s *leaseSet) releaseAddress(address string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	var removed *AccessLease
	kept := s.entries[:0]
	for _, entry := range s.entries {
		if entry.address == address && removed == nil {
			removed = entry.lease
			continue
		}
		kept = append(kept, entry)
	}
	s.entries = kept
	s.mu.Unlock()
	removed.Release()
}

// Release releases every lease in the set. It is idempotent and is the call a
// command's reaper makes once the child has actually been reaped.
func (s *leaseSet) Release() {
	if s == nil {
		return
	}
	s.mu.Lock()
	entries, ok := s.takeLocked()
	s.mu.Unlock()
	if !ok {
		return
	}
	for _, entry := range entries {
		entry.lease.Release()
	}
}

// ReleaseIfOwned is the deferred safety net of the goroutine that created the
// set: it releases the leases unless ownership was transferred to a command's
// reaper. A panic between leasing a device and handing the set over therefore
// still drains the device instead of orphaning the lease.
func (s *leaseSet) ReleaseIfOwned() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.transferred {
		s.mu.Unlock()
		return
	}
	entries, ok := s.takeLocked()
	s.mu.Unlock()
	if !ok {
		return
	}
	for _, entry := range entries {
		entry.lease.Release()
	}
}

// transfer hands the set to a command's reaper. The creator's deferred
// [leaseSet.ReleaseIfOwned] becomes a no-op; the new owner must call
// [leaseSet.Release] after the child is reaped, because while an unreaped
// child may still be touching the devices they must stay non-drainable.
func (s *leaseSet) transfer() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.transferred = true
	s.mu.Unlock()
}

// takeLocked claims the entries, reporting false when they were already
// released.
func (s *leaseSet) takeLocked() ([]leaseEntry, bool) {
	if s.released {
		return nil, false
	}
	s.released = true
	entries := s.entries
	s.entries = nil
	return entries, true
}

// recheckAll re-reads every leased device's kernel runtime status. It is the
// last check before a command is launched; the first refusal aborts the whole
// command, so no device is touched after any of them suspended.
func (s *leaseSet) recheckAll() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	entries := append([]leaseEntry{}, s.entries...)
	s.mu.Unlock()
	for _, entry := range entries {
		if err := entry.lease.Recheck(); err != nil {
			return err
		}
	}
	return nil
}

// NewPoller builds a poller. Nothing runs until [Poller.Start] or
// [Poller.PollOnce] is called.
func NewPoller(options PollerOptions) *Poller {
	interval := options.Interval
	if interval <= 0 {
		interval = minPollInterval
	}
	if interval < minPollInterval {
		interval = minPollInterval
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	command := options.Command
	if command == "" {
		command = defaultCommand
	}
	runner := options.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	poller := &Poller{
		pollInterval:    interval,
		timeout:         timeout,
		command:         command,
		runner:          runner,
		monotonic:       options.Monotonic,
		onEvent:         options.OnEvent,
		sensor:          options.SensorSource,
		power:           options.Power,
		authorization:   newLiveAuthorization(),
		info:            make(map[string]DeviceInfo),
		apps:            make(map[int]int64),
		availability:    AvailabilityUnknown,
		lastDeviceCount: -1,
	}
	if poller.monotonic == nil {
		start := time.Now()
		poller.monotonic = func() time.Duration { return time.Since(start) }
	}
	poller.rootCtx, poller.rootCancel = context.WithCancel(context.Background())
	if poller.power != nil && poller.power.Gate != nil {
		poller.access = poller.power.Access
		if poller.access == nil {
			poller.access = GateAccess(poller.power.Gate)
		}
		if poller.access == nil {
			poller.access = NewAccessArbiter()
		}
		// The register-temperature source must dispatch its helper under the
		// same arbiter; otherwise a revocation could never see its leases.
		if temps, ok := poller.sensor.(*TempsPoller); ok {
			temps.bindAccess(poller.access)
		}
	}
	return poller
}

// PollInterval is the effective cadence (at least one second).
func (p *Poller) PollInterval() time.Duration { return p.pollInterval }

// Timeout is the per-invocation budget.
func (p *Poller) Timeout() time.Duration { return p.timeout }

// Start begins the polling loop: one cycle immediately, then one per
// interval or per backoff delay. It is idempotent. The loop stops when ctx is
// cancelled or [Poller.Stop] is called.
func (p *Poller) Start(ctx context.Context) {
	p.lifeMu.Lock()
	if p.cancel != nil {
		p.lifeMu.Unlock()
		return
	}
	// A stopped poller can be started again: give it a fresh root, because
	// Stop cancelled the previous one.
	if p.rootCtx == nil || p.rootCtx.Err() != nil {
		p.rootCtx, p.rootCancel = context.WithCancel(context.Background())
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	p.cancel = cancel
	p.done = done
	p.lifeMu.Unlock()

	go func() {
		defer close(done)
		// A register read owns its own context; the loop ending must cancel it
		// as well, or Wait could block on a read nobody will ever finish.
		defer p.cancelSensorCycle()
		for {
			if loopCtx.Err() != nil {
				return
			}
			_ = p.PollOnce(loopCtx)
			if loopCtx.Err() != nil {
				return
			}
			timer := time.NewTimer(p.nextDelay())
			select {
			case <-loopCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// Stop stops scheduling cycles and cancels whatever query is in flight,
// including one started by a manual [Poller.PollOnce] call. [Poller.Wait]
// joins the loop and any register-temperature read.
func (p *Poller) Stop() {
	p.lifeMu.Lock()
	cancel := p.cancel
	rootCancel := p.rootCancel
	p.cancel = nil
	p.lifeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Cancelling the root before the sensor lock is what makes a launch that
	// is racing with Stop fail closed: startSensorCycle re-checks root.Err()
	// while holding sensorMu.
	if rootCancel != nil {
		rootCancel()
	}
	p.cancelInflight()
	p.cancelSensorCycle()
}

// cancelInflight aborts the running cycle, if any.
func (p *Poller) cancelInflight() {
	p.cycleMu.Lock()
	running := p.inflight
	p.cycleMu.Unlock()
	if running != nil {
		running.cancel()
	}
}

// Wait blocks until the loop started by [Poller.Start] has exited and the
// register-temperature read it may have left behind has finished. [Poller.Stop]
// cancels both.
func (p *Poller) Wait() {
	p.lifeMu.Lock()
	done := p.done
	p.lifeMu.Unlock()
	if done != nil {
		<-done
	}
	p.waitSensorCycle()
}

// PollOnce runs one cycle immediately. Concurrent calls share the in-flight
// cycle, so a slow nvidia-smi is never started twice. A caller that gives up
// (its context is cancelled) stops waiting without disturbing the cycle the
// other callers share.
func (p *Poller) PollOnce(ctx context.Context) error {
	p.cycleMu.Lock()
	if running := p.inflight; running != nil {
		p.cycleMu.Unlock()
		select {
		case <-running.done:
			return running.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	cycleCtx, cancel := context.WithCancel(ctx)
	running := &cycleRun{done: make(chan struct{}), cancel: cancel}
	p.inflight = running
	p.cycleMu.Unlock()

	defer func() {
		p.cycleMu.Lock()
		p.inflight = nil
		p.cycleMu.Unlock()
		cancel()
		close(running.done)
	}()
	running.err = p.runCycle(cycleCtx)
	return running.err
}

// Latest returns a copy of the last completed cycle, or nil when no cycle has
// completed (including the "nvidia-smi is missing" case). The copy owns every
// optional value, so a caller may keep or modify it without racing the poller.
func (p *Poller) Latest() *Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.snapshot == nil {
		return nil
	}
	return &Snapshot{
		AtMonotonic: p.snapshot.AtMonotonic,
		Devices:     copyReadings(p.snapshot.Devices),
	}
}

// ComputeApps returns per-PID GPU memory in bytes, summed across devices.
func (p *Poller) ComputeApps() map[int]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	apps := make(map[int]int64, len(p.apps))
	for pid, bytes := range p.apps {
		apps[pid] = bytes
	}
	return apps
}

// SensorDiagnostics reports the last distinct reason register temperatures
// were unavailable. A stuck pci-read helper has its own state here instead of
// being reported as a permissions failure.
func (p *Poller) SensorDiagnostics() SensorDiagnostics {
	temps, ok := p.sensor.(*TempsPoller)
	if !ok || temps == nil {
		return SensorDiagnostics{}
	}
	return temps.Diagnostics()
}

// DeviceInfo returns the slow-changing NVIDIA device descriptions, sorted by
// PCI bus id (or UUID when there is none).
func (p *Poller) DeviceInfo() []DeviceInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	devices := make([]DeviceInfo, 0, len(p.info))
	for _, info := range p.info {
		devices = append(devices, copyDeviceInfo(info))
	}
	sort.Slice(devices, func(i, j int) bool {
		return deviceSortKey(devices[i]) < deviceSortKey(devices[j])
	})
	return devices
}

func deviceSortKey(info DeviceInfo) string {
	if info.PCIBusID != nil && *info.PCIBusID != "" {
		return *info.PCIBusID
	}
	return info.UUID
}

func copyDeviceInfo(info DeviceInfo) DeviceInfo {
	copied := DeviceInfo{UUID: info.UUID, Name: info.Name}
	if info.PCIBusID != nil {
		copied.PCIBusID = pointer(*info.PCIBusID)
	}
	if info.MemoryTotalBytes != nil {
		copied.MemoryTotalBytes = pointer(*info.MemoryTotalBytes)
	}
	return copied
}

// Available reports whether nvidia-smi tooling can be used.
func (p *Poller) Available() Availability {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.availability
}

// CostStats returns a copy of the nvidia-smi cost counters.
func (p *Poller) CostStats() CostStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

type skippedDevice struct {
	address string
	reason  string
}

type powerPlan struct {
	sampled []string
	skipped []skippedDevice
}

// nvidiaSMIAddress spells a canonical PCI address the way nvidia-smi's --id
// filter expects it: an eight-digit domain followed by bus:device.function.
func nvidiaSMIAddress(address string) (string, bool) {
	canonical, ok := CanonicalPCIAddress(address)
	if !ok {
		return "", false
	}
	parts := strings.Split(canonical, ":")
	if len(parts) != 3 {
		return "", false
	}
	return fmt.Sprintf("%08s:%s:%s", parts[0], parts[1], parts[2]), true
}

// powerPlan asks the runtime-PM gate which devices may be queried. nil means
// "no gate" and therefore "query everything".
//
// A configured gate always yields a plan, even when the inventory is empty or
// unknown: that plan permits nothing, which is the fail-closed answer. It is
// never safe to widen a gated cycle to "everything" just because discovery
// returned no addresses or was not wired at all.
func (p *Poller) powerPlan() *powerPlan {
	if p.power == nil || p.power.Gate == nil {
		return nil
	}
	plan := &powerPlan{}
	if p.power.Devices == nil {
		return plan
	}
	for _, address := range p.power.Devices() {
		canonical, ok := CanonicalPCIAddress(address)
		if !ok {
			plan.skipped = append(plan.skipped, skippedDevice{
				address: address,
				reason:  "unrecognized PCI address",
			})
			continue
		}
		decision := p.power.Gate.Decide(canonical)
		if decision.Sample {
			plan.sampled = append(plan.sampled, canonical)
			continue
		}
		plan.skipped = append(plan.skipped, skippedDevice{address: canonical, reason: decision.Reason})
	}
	return plan
}

// authorizeCycle publishes the decision this cycle is allowed to use and
// returns the live scope plus its generation. When the decision changes, any
// register-temperature cycle started under the previous one is cancelled.
// The scope carries the shared arbiter so every device access additionally
// needs a lease even if it raced the publication.
func (p *Poller) authorizeCycle(plan *powerPlan, sampled []string) (SampleScope, uint64) {
	scope := AllDevices()
	if plan != nil {
		scope = PermittedScope(sampled)
		scope.access = p.access
	}
	generation, changed := p.authorization.publish(scope)
	if changed {
		p.cancelSensorCycle()
	}
	return p.authorization.scope(), generation
}

func (p *Poller) runCycle(ctx context.Context) error {
	plan := p.powerPlan()
	sampled := make([]string, 0)
	skipped := make([]skippedDevice, 0)
	var leases *leaseSet
	if plan != nil {
		skipped = append(skipped, plan.skipped...)
		leases = &leaseSet{}
		// Until the command's reaper owns the set, this goroutine does. The
		// deferred release runs even when a reader, clock or runner panics
		// after the leases were acquired, so the devices can still drain.
		defer leases.ReleaseIfOwned()
		// Acquire in canonical order: two devices are always leased in the
		// same order, so a revocation of both can never deadlock.
		for _, address := range sortedUniqueAddresses(plan.sampled) {
			lease, err := p.access.Acquire(ctx, address)
			if err != nil {
				// The gate changed its mind between Decide and dispatch.
				// Fail closed for this device; never widen the query.
				skipped = append(skipped, skippedDevice{
					address: address,
					reason:  accessDeniedReason(err),
				})
				continue
			}
			leases.add(address, lease)
			sampled = append(sampled, address)
		}
		// The gate decided moments ago; the kernel can autosuspend a device
		// between that decision and this point. Re-read the power state under
		// the lease, immediately before dispatch, and refuse the device
		// rather than wake it.
		sampled = p.dropDevicesThatAreNotAwake(leases, sampled, &skipped)
	}

	// Publish this cycle's authorization before anything touches a device. A
	// running sensor cycle whose decision was replaced is cancelled and can no
	// longer access a device: every read and probe needs both the live scope
	// and an arbiter lease.
	scope, generation := p.authorizeCycle(plan, sampled)
	if plan != nil && len(sampled) == 0 {
		// Nothing can be queried without waking a GPU. No process can hold
		// memory on a suspended device, so the per-PID join is cleared too.
		leases.Release()
		at := p.monotonic()
		p.mu.Lock()
		p.apps = make(map[int]int64)
		devices := p.gatedReadingsLocked(skipped)
		p.snapshot = &Snapshot{AtMonotonic: at, Devices: capReadings(devices)}
		p.mu.Unlock()
		return nil
	}

	// A gated cycle always restricts the query to the explicitly leased
	// addresses, even when no listed device is skipped: an unknown or
	// unlisted device must never be touched. nvidia-smi expects its own
	// 8-digit-domain bus id spelling.
	var ids []string
	if plan != nil {
		ids = nvidiaSMIIDs(sampled)
	}

	// Register temperatures are read while the expensive query runs. The scope
	// carries the gate decision into the sensor cycle: a device nvidia-smi is
	// not allowed to touch must not be mapped or probed either.
	sensor := p.startSensorCycle(scope, generation)

	stdout, err := p.exec(ctx, gpuQuery, ids, leases)
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown is not a telemetry failure: do not poison backoff or
			// emit a misleading diagnostic.
			return err
		}
		if reason, refused := powerRefusalReason(err); refused {
			// The kernel suspended a device between the gate decision and the
			// launch, so no command ran. That is not a telemetry failure:
			// report the devices unavailable with the power-state reason and
			// leave the cadence alone.
			unavailable := append([]skippedDevice{}, skipped...)
			for _, address := range sampled {
				unavailable = append(unavailable, skippedDevice{address: address, reason: reason})
			}
			at := p.monotonic()
			p.mu.Lock()
			p.snapshot = &Snapshot{AtMonotonic: at, Devices: capReadings(p.gatedReadingsLocked(unavailable))}
			p.mu.Unlock()
			return nil
		}
		return p.recordFailure(err)
	}
	rows := ParseGPUCSV(stdout)
	p.applyRows(rows, ids != nil)

	p.mu.Lock()
	wasAvailable := p.availability == AvailabilityYes
	p.gpuQueries++
	p.availability = AvailabilityYes
	p.consecutiveFailures = 0
	gpuQueries := p.gpuQueries
	p.mu.Unlock()

	sensors := p.settleSensorCycle(ctx, sensor)
	devices := p.toReadings(rows, sensors)
	if plan != nil {
		p.mu.Lock()
		devices = append(devices, p.gatedReadingsLocked(skipped)...)
		p.mu.Unlock()
	}
	devices = capReadings(devices)

	at := p.monotonic()
	p.mu.Lock()
	p.snapshot = &Snapshot{AtMonotonic: at, Devices: devices}
	deviceCountChanged := len(rows) != p.lastDeviceCount
	p.lastDeviceCount = len(rows)
	p.mu.Unlock()

	// Diagnostics only on state changes: this line would otherwise repeat
	// every healthy cycle.
	if !wasAvailable || deviceCountChanged {
		p.emit(fmt.Sprintf("gpu telemetry: %d device(s)", len(rows)))
	}
	// The compute-app query is another hardware access, so it takes its own
	// leases (and gives them to its own reaper) and re-checks the power state.
	// When it does not complete, no idle sample is recorded: the gate must not
	// start a quiet window while a command that may still touch the device is
	// in flight.
	appsSettled := true
	if gpuQueries%computeAppsEveryNthQ == 0 {
		appsSettled = p.pollComputeApps(ctx, sampled)
	}
	// Idle state is recorded only after every hardware query of the cycle has
	// settled. Recording it earlier would start the gate's quiet window while
	// the compute-app query was still about to wake the device.
	if plan != nil && appsSettled {
		p.recordIdle(sampled, rows)
	}
	return nil
}

// dropDevicesThatAreNotAwake re-reads each leased device's kernel runtime
// status immediately before the command is dispatched. A device that is no
// longer active is refused — no access, no lease — and reported with the
// powered-down (or unknown) reason, so a suspended GPU is never woken by the
// race the gate cannot close.
func (p *Poller) dropDevicesThatAreNotAwake(leases *leaseSet, sampled []string, skipped *[]skippedDevice) []string {
	awake := sampled[:0]
	for _, address := range sampled {
		if err := leases.lease(address).Recheck(); err != nil {
			leases.releaseAddress(address)
			*skipped = append(*skipped, skippedDevice{
				address: address,
				reason:  accessDeniedReason(err),
			})
			continue
		}
		awake = append(awake, address)
	}
	return awake
}

// sortedUniqueAddresses returns canonical device addresses in stable order,
// so a multi-device lease is always acquired in the same order.
func sortedUniqueAddresses(addresses []string) []string {
	seen := make(map[string]struct{}, len(addresses))
	unique := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		unique = append(unique, address)
	}
	sort.Strings(unique)
	return unique
}

func (p *Poller) recordFailure(cause error) error {
	p.mu.Lock()
	p.consecutiveFailures++
	p.lastDeviceCount = -1
	notFound := isNotFound(cause)
	if notFound {
		p.availability = AvailabilityNo
	}
	p.mu.Unlock()
	if notFound {
		p.emit("gpu telemetry unavailable: nvidia-smi not found")
	} else {
		p.emit(fmt.Sprintf("gpu telemetry query failed: %v", cause))
	}
	return cause
}

// startSensorCycle returns the register-temperature operation for this cycle,
// starting one when none is in flight. At most one ever runs: a read that
// outlived its settle deadline is reused, not duplicated, so repeated timeouts
// cannot accumulate goroutines.
//
// A running cycle is reused only while the authorization generation is
// unchanged. When a later gate decision replaced it, the old cycle is
// cancelled (and its live scope refuses further accesses); no replacement is
// launched while it may still be stuck, so a wedged BAR cannot multiply
// goroutines.
//
// The operation owns a context derived from the poller's root rather than
// borrowing the polling cycle's, so a settle timeout does not abort a read
// that is still making progress, and [Poller.Stop] can cancel it regardless of
// who started the cycle. Shutdown is checked while sensorMu is held, so a
// sensor cannot slip in after Stop.
func (p *Poller) startSensorCycle(scope SampleScope, generation uint64) *sensorCycle {
	if p.sensor == nil {
		return nil
	}
	p.lifeMu.Lock()
	root := p.rootCtx
	p.lifeMu.Unlock()

	p.sensorMu.Lock()
	defer p.sensorMu.Unlock()
	if root == nil || root.Err() != nil {
		return nil
	}
	if running := p.sensorRun; running != nil {
		select {
		case <-running.done:
			p.sensorRun = nil
		default:
			if running.generation == generation {
				return running
			}
			running.cancel()
			return nil
		}
	}
	cycleCtx, cancel := context.WithCancel(root)
	running := &sensorCycle{done: make(chan struct{}), cancel: cancel, generation: generation}
	p.sensorRun = running
	go func() {
		defer close(running.done)
		defer cancel()
		_ = p.sensor.PollOnce(cycleCtx, scope)
	}()
	return running
}

// cancelSensorCycle aborts the register-temperature read in flight.
func (p *Poller) cancelSensorCycle() {
	p.sensorMu.Lock()
	running := p.sensorRun
	p.sensorMu.Unlock()
	if running != nil {
		running.cancel()
	}
}

// waitSensorCycle joins the register-temperature read in flight, if any.
func (p *Poller) waitSensorCycle() {
	p.sensorMu.Lock()
	running := p.sensorRun
	p.sensorMu.Unlock()
	if running != nil {
		<-running.done
	}
}

// settleSensorCycle awaits the register-temperature cycle within the same
// budget as one nvidia-smi invocation. A sensor cycle that hangs cannot hold
// up the snapshot; the previous register values are used instead, and the read
// itself keeps running (there is only ever one) until it finishes or the
// poller stops. Sensor failures never invalidate nvidia-smi readings;
// [TempsPoller] reports its own diagnostics.
func (p *Poller) settleSensorCycle(ctx context.Context, cycle *sensorCycle) SensorMap {
	if p.sensor == nil || cycle == nil {
		return nil
	}
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	select {
	case <-cycle.done:
		p.mu.Lock()
		p.settleTimeoutSeen = false
		p.mu.Unlock()
	case <-timer.C:
		p.mu.Lock()
		first := !p.settleTimeoutSeen
		p.settleTimeoutSeen = true
		p.mu.Unlock()
		if first {
			p.emit("gpu register temps: cycle did not settle in time")
		}
	case <-ctx.Done():
		// The cycle owns its own lifetime; use whatever is cached.
	}
	return p.sensor.Latest()
}

// recordIdle tells the gate whether each sampled device looked idle. A
// missing reading counts as idle so a busy-looking device is never assumed.
func (p *Poller) recordIdle(addresses []string, rows []ParsedGPURow) {
	if p.power == nil || p.power.Gate == nil {
		return
	}
	byKey := make(map[string]ParsedGPURow, len(rows))
	for _, row := range rows {
		if key, ok := CanonicalPCIAddress(derefString(row.PCIBusID)); ok {
			byKey[key] = row
		}
	}
	for _, address := range addresses {
		key, ok := CanonicalPCIAddress(address)
		if !ok {
			continue
		}
		row, found := byKey[key]
		if !found {
			continue
		}
		idle := row.UtilizationPercent == nil || *row.UtilizationPercent == 0
		p.power.Gate.Record(address, idle)
	}
}

// pollComputeApps refreshes the per-PID GPU memory join. It reports whether
// the query settled: false means the command did not complete (it was refused,
// timed out, or its child is still unreaped), so the caller must not record an
// idle sample that could open a quiet window underneath it.
func (p *Poller) pollComputeApps(ctx context.Context, addresses []string) bool {
	if ctx.Err() != nil {
		return false
	}
	// A gated cycle queries exactly the devices it still holds leases for.
	// This command needs its own leases: the previous command's leases were
	// released by its reaper the moment it was reaped.
	var leases *leaseSet
	var ids []string
	if p.power != nil && p.power.Gate != nil {
		leases = &leaseSet{}
		// Same ownership rule as the main query: a panic before dispatch must
		// not leave the compute-app leases orphaned.
		defer leases.ReleaseIfOwned()
		available := make([]string, 0, len(addresses))
		for _, address := range sortedUniqueAddresses(addresses) {
			lease, err := p.access.Acquire(ctx, address)
			if err != nil {
				continue
			}
			leases.add(address, lease)
			available = append(available, address)
		}
		skipped := make([]skippedDevice, 0)
		available = p.dropDevicesThatAreNotAwake(leases, available, &skipped)
		if len(available) == 0 {
			leases.Release()
			return false
		}
		ids = nvidiaSMIIDs(available)
	}
	stdout, err := p.exec(ctx, computeAppsQuery, ids, leases)
	if err != nil {
		// GPU readings remain valid; only the process join is missing.
		p.emit(fmt.Sprintf("gpu compute-app query failed: %v", err))
		return false
	}
	apps := make(map[int]int64)
	for _, app := range ParseComputeAppsCSV(stdout) {
		apps[app.PID] += app.UsedBytes
	}
	p.mu.Lock()
	p.apps = apps
	p.mu.Unlock()
	return true
}

// nvidiaSMIIDs spells canonical addresses the way nvidia-smi's --id filter
// expects them, skipping any address it cannot spell.
func nvidiaSMIIDs(addresses []string) []string {
	ids := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if id, ok := nvidiaSMIAddress(address); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// exec runs one query and records its cost. Arguments are the query text
// split on spaces, the CSV format, and an optional --id= restriction.
//
// When leases is non-nil, ownership of the lease set passes to the command:
// the runner's reaper releases it once the child is reaped, on success, on
// timeout and on cancellation alike. A runner that cannot observe the reap
// releases when the call returns.
func (p *Poller) exec(ctx context.Context, query string, ids []string, leases *leaseSet) (string, error) {
	// Ownership is only transferred to a reaper inside runCommand. Until then
	// this goroutine must release the leases however it exits, including on a
	// panic from the clock, the re-check or the synchronous runner.
	defer leases.ReleaseIfOwned()
	started := p.monotonic()
	args := append(strings.Split(query, " "), csvFormat)
	if ids != nil {
		args = append(args, "--id="+strings.Join(ids, ","))
	}
	var stdout, stderr string
	var err error
	// Last-moment check: nothing is launched for a device the kernel no longer
	// reports awake, even though the lease was granted moments ago. It is not
	// a command failure — no command ran — so it does not move the cost stats.
	if err = leases.recheckAll(); err != nil {
		leases.Release()
		return "", err
	}
	stdout, stderr, err = p.runCommand(ctx, args, leases)
	duration := p.monotonic() - started

	p.mu.Lock()
	p.stats.Runs++
	if err != nil {
		p.stats.Failures++
	}
	p.recordDurationLocked(duration)
	p.mu.Unlock()

	if err != nil {
		if detail := strings.TrimSpace(stderr); detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
		return "", err
	}
	return stdout, nil
}

// runCommand launches the command and hands its device leases to the reaper.
func (p *Poller) runCommand(ctx context.Context, args []string, leases *leaseSet) (string, string, error) {
	if leases == nil {
		return p.runner.Run(ctx, p.command, args, p.timeout)
	}
	if observer, ok := p.runner.(reapObserver); ok {
		// The reaper owns the leases from here: it releases them once the
		// child is truly gone, and the caller's deferred release is disarmed.
		leases.transfer()
		return observer.runReaping(ctx, p.command, args, p.timeout, leases.Release)
	}
	// A runner that cannot observe the reap releases when the call returns.
	// The defer also covers a panic from that synchronous fallback.
	defer leases.Release()
	return p.runner.Run(ctx, p.command, args, p.timeout)
}

func (p *Poller) recordDurationLocked(duration time.Duration) {
	if duration < 0 {
		duration = 0
	}
	milliseconds := float64(duration) / float64(time.Millisecond)
	p.totalDuration += duration
	if milliseconds > p.stats.MaxDurationMs {
		p.stats.MaxDurationMs = milliseconds
	}
	p.stats.LastDurationMs = milliseconds
	runs := p.stats.Runs
	if runs == 0 {
		p.stats.AvgDurationMs = 0
	} else {
		p.stats.AvgDurationMs = float64(p.totalDuration) / float64(runs) / float64(time.Millisecond)
	}
}

// applyRows updates the device descriptions. A restricted query must not
// forget the devices it skipped, so partial cycles merge into the cache.
func (p *Poller) applyRows(rows []ParsedGPURow, partial bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	info := make(map[string]DeviceInfo, len(rows))
	if partial {
		for uuid, device := range p.info {
			info[uuid] = device
		}
	}
	for _, row := range rows {
		var busID *string
		if row.PCIBusID != nil {
			busID = pointer(*row.PCIBusID)
		}
		var memory *int64
		if row.MemoryTotalBytes != nil {
			memory = pointer(*row.MemoryTotalBytes)
		}
		info[row.UUID] = DeviceInfo{
			UUID:             row.UUID,
			Name:             row.Name,
			PCIBusID:         busID,
			MemoryTotalBytes: memory,
		}
	}
	p.info = info
}

// gatedReadingsLocked builds the unavailable readings for devices the gate
// kept asleep. IDs match the inventory: once a UUID is known the stable
// "gpu:<uuid>" id is kept even while the device cannot be sampled.
func (p *Poller) gatedReadingsLocked(skipped []skippedDevice) []GpuReading {
	byKey := make(map[string]DeviceInfo, len(p.info))
	for _, info := range p.info {
		if key, ok := CanonicalPCIAddress(derefString(info.PCIBusID)); ok {
			byKey[key] = info
		}
	}
	readings := make([]GpuReading, 0, len(skipped))
	for _, device := range skipped {
		id := "pci:" + device.address
		if key, ok := CanonicalPCIAddress(device.address); ok {
			if info, found := byKey[key]; found {
				id = "gpu:" + info.UUID
			}
		}
		readings = append(readings, UnavailableReading(id, device.reason))
	}
	return readings
}

// capReadings keeps a snapshot inside the protocol's GPU cardinality bound.
// The head of the list wins deterministically: sampled devices come first and
// gate-excluded ones last.
func capReadings(devices []GpuReading) []GpuReading {
	if len(devices) > protocol.MaxGPUs {
		return devices[:protocol.MaxGPUs]
	}
	return devices
}

func (p *Poller) toReadings(rows []ParsedGPURow, sensors SensorMap) []GpuReading {
	devices := make([]GpuReading, 0, len(rows))
	for _, row := range rows {
		reading := GpuReading{
			ID:                 "gpu:" + row.UUID,
			Status:             StatusOK,
			UtilizationPercent: row.UtilizationPercent,
			MemoryUsedBytes:    row.MemoryUsedBytes,
			MemoryTotalBytes:   row.MemoryTotalBytes,
			TemperatureC:       row.TemperatureC,
			PowerWatts:         row.PowerWatts,
		}
		if key, ok := NormalizeBusFunctionKey(derefString(row.PCIBusID)); ok {
			if sensor, found := sensors[key]; found {
				if sensor.JunctionTemperatureC != nil {
					reading.JunctionTemperatureC = sensor.JunctionTemperatureC
				}
				if sensor.VRAMTemperatureC != nil {
					reading.VRAMTemperatureC = sensor.VRAMTemperatureC
				}
			}
		}
		devices = append(devices, reading)
	}
	return devices
}

// nextDelay is the delay before the next scheduled cycle: the cadence on
// success, exponential backoff after failures, and a rare retry when
// nvidia-smi is missing.
func (p *Poller) nextDelay() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.availability == AvailabilityNo {
		return notFoundRetryDelay
	}
	if p.consecutiveFailures > 0 {
		exponent := p.consecutiveFailures - 1
		if exponent > 10 {
			exponent = 10
		}
		delay := time.Duration(float64(p.pollInterval) * math.Pow(2, float64(exponent)))
		if delay <= 0 || delay > maxBackoff {
			return maxBackoff
		}
		return delay
	}
	return p.pollInterval
}

func (p *Poller) emit(line string) {
	if p.onEvent == nil {
		return
	}
	defer func() { _ = recover() }()
	p.onEvent(line)
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
