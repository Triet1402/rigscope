//go:build linux

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/delivery"
	"github.com/Triet1402/rigscope/internal/collector/enroll"
	"github.com/Triet1402/rigscope/internal/collector/metrics"
	"github.com/Triet1402/rigscope/internal/collector/policy"
	"github.com/Triet1402/rigscope/internal/collector/processes"
	"github.com/Triet1402/rigscope/internal/collector/state"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/lock"
	"github.com/Triet1402/rigscope/internal/protocol"
)

const (
	// statusInterval is how often the collector reports its delivery state.
	// The hub keeps it as the machine's live delivery status.
	statusInterval = 5 * time.Second
	// dormantPollInterval is how often an unenrolled collector re-reads its
	// private state, so an operator can run `rigscope collector enroll` (or
	// install a grant) without restarting the service.
	dormantPollInterval = 5 * time.Second
	// A failed enrollment request is retried after firstEnrollmentRetry,
	// doubling up to enrollmentRetryInterval. The first retries are quick
	// because install starts the collector right after the hub, which may not
	// be listening yet. The saved recovery proof makes a retry safe.
	firstEnrollmentRetry    = 2 * time.Second
	enrollmentRetryInterval = 60 * time.Second
	// awaitingLogInterval limits the "awaiting enrollment" line to one per
	// minute instead of one per poll.
	awaitingLogInterval = 60 * time.Second
	// defaultDrainTimeout bounds the graceful drain before exit. It is shorter
	// than the systemd stop timeout and the runtime-test grace window, so a
	// normal stop always exits zero.
	defaultDrainTimeout = 3 * time.Second
	// leaseFileName serialises collector processes on one state directory. It
	// lives inside the collector-owned subtree so a system-scope collector can
	// create it under ReadWritePaths even though the state root is root-owned.
	leaseFileName = "collector-runtime.lock"
	// welcomeStatusDelay is how soon after a welcome the first status is
	// captured. The delivery session sends its inventory and starts pumping
	// immediately after the welcome callback, so a short delay reports queued
	// records as in flight instead of still pending at the handshake instant.
	welcomeStatusDelay = 100 * time.Millisecond
)

// LeasePath returns the process lease for one state root. The file lives in
// <stateRoot>/collector/, the same 0700 directory that holds the collector's
// credentials, never in the shared (possibly root-owned) state root.
func LeasePath(stateRoot string) string {
	return filepath.Join(stateRoot, "collector", leaseFileName)
}

// Field is one structured log field.
type Field struct {
	Key   string
	Value any
}

// F builds a structured log field.
func F(key string, value any) Field { return Field{Key: key, Value: value} }

// Event is one structured log line. The CLI encodes it as JSON on stderr so
// journald keeps it structured; tests can read the same events directly.
type Event struct {
	Level   string
	Message string
	Fields  []Field
}

// LogFunc receives one event. It must be safe for concurrent use.
type LogFunc func(Event)

// Source is the sampling source of one run. Both the real assembler
// (metrics.Collector) and the explicit synthetic sampler implement it.
type Source interface {
	Next() (protocol.MetricsRecord, error)
	Inventory() protocol.Inventory
	InventoryVersion() int64
	Failures() []protocol.CollectionFailure
}

// Options configures one collector runtime.
type Options struct {
	// Config is the validated configuration of this installation.
	Config *config.Config
	// StateDir is the collector state root (the directory holding
	// collector/credentials.json).
	StateDir string
	// Store is the collector-private state store.
	Store *state.Store
	// Hostname overrides the hostname advertised in hello. Empty uses the
	// machine hostname. It is an operational override (containers, tests).
	Hostname string
	// Logf receives structured events. Nil discards them.
	Logf LogFunc
	// OnReady is called once per process, after the first session is welcomed
	// by the hub. The CLI uses it for one readiness line on stdout so a
	// supervisor (or the runtime test harness) can observe connectivity
	// without parsing structured logs.
	OnReady func(hubURL string)
	// Now is the wall clock used for the readiness line and diagnostics.
	Now func() time.Time
	// DrainTimeout bounds the graceful drain. Zero selects 3s.
	DrainTimeout time.Duration
	// NewSource overrides the sampling source (tests). Nil builds the real or
	// synthetic source from the configuration.
	NewSource SourceFactory
	// NewScanner overrides the process-detail scanner (tests). Nil builds the
	// real /proc scanner, or an empty scanner in synthetic mode.
	NewScanner func() (processes.SnapshotScanner, error)
}

// Collector is one composed collector process.
type Collector struct {
	cfg          *config.Config
	stateDir     string
	store        *state.Store
	hostname     string
	logf         LogFunc
	now          func() time.Time
	drainTimeout time.Duration
	newSource    SourceFactory
	newScanner   func() (processes.SnapshotScanner, error)
	onReady      func(string)
	// welcoming reports the first welcome of this process exactly once, so the
	// CLI can print a readiness line without repeating it on every reconnect.
	welcomed bool
	// samples and skippedSamples are the scheduling counters exposed by Stats.
	// A skipped sample is a cadence whose tick fired while the previous sample
	// was still running; the timer loop never waits for the worker.
	samples        atomic.Int64
	skippedSamples atomic.Int64
}

// Stats is a point-in-time view of the runtime's sampling schedule.
type Stats struct {
	// Samples counts completed sampling attempts, including failed ones.
	Samples int64
	// SkippedSamples counts ticks that were skipped because the previous
	// sample had not returned yet.
	SkippedSamples int64
}

// Stats returns the runtime's scheduling counters. It is safe to call while
// Run is executing.
func (c *Collector) Stats() Stats {
	return Stats{
		Samples:        c.samples.Load(),
		SkippedSamples: c.skippedSamples.Load(),
	}
}

// New validates the options and returns a runtime. Nothing starts until Run.
func New(options Options) (*Collector, error) {
	if options.Config == nil {
		return nil, errors.New("runtime: a configuration is required")
	}
	if options.Store == nil {
		return nil, errors.New("runtime: a state store is required")
	}
	if options.StateDir == "" {
		return nil, errors.New("runtime: a state directory is required")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	drain := options.DrainTimeout
	if drain <= 0 {
		drain = defaultDrainTimeout
	}
	if options.Logf == nil {
		options.Logf = func(Event) {}
	}
	return &Collector{
		cfg:          options.Config,
		stateDir:     options.StateDir,
		store:        options.Store,
		hostname:     options.Hostname,
		logf:         options.Logf,
		now:          now,
		drainTimeout: drain,
		newSource:    options.NewSource,
		newScanner:   options.NewScanner,
		onReady:      options.OnReady,
	}, nil
}

// Run owns the collector process until ctx is cancelled. It acquires the
// process lease, waits for a destination and credential (enrolling from a
// pending grant when one is staged), samples and delivers at the configured
// cadence, serves process-detail subscriptions, and drains on shutdown. A
// cancelled context is a clean stop: Run returns nil and the process exits 0.
func (c *Collector) Run(ctx context.Context) error {
	if !c.cfg.Components.Collector.Enabled {
		c.logf(Event{Level: "info", Message: "collector is disabled in the configuration"})
		return nil
	}
	held, err := lock.Acquire(LeasePath(c.stateDir))
	if err != nil {
		return fmt.Errorf("runtime: another collector owns %s: %w", c.stateDir, err)
	}
	defer func() { _ = held.Release() }()

	identity, err := c.store.NewRunIdentity()
	if err != nil {
		return err
	}
	if c.hostname != "" {
		identity.Hostname = c.hostname
	}
	c.logf(Event{
		Level:   "info",
		Message: "collector starting",
		Fields: []Field{
			F("collectorId", identity.CollectorID),
			F("runId", identity.RunID),
			F("bootId", identity.BootID),
			F("hostname", identity.Hostname),
			F("samplingInterval", c.cfg.Components.Collector.SamplingInterval.Value().String()),
			F("synthetic", c.cfg.Components.Collector.Synthetic),
		},
	})

	destination, credential, ok := c.awaitDestination(ctx)
	if !ok {
		c.logf(Event{Level: "info", Message: "collector stopped before a destination was available"})
		return nil
	}
	return c.runSession(ctx, identity, destination, credential)
}

// awaitDestination returns the configured destination and its credential.
// When no credential is stored yet it consumes the installer's pending grant;
// otherwise it stays dormant and re-checks, so enrollment can complete while
// the service is running. It returns ok=false only when ctx is cancelled.
func (c *Collector) awaitDestination(ctx context.Context) (config.Destination, string, bool) {
	logged := ""
	loggedAt := time.Time{}
	note := func(reason string) {
		if reason == logged && c.now().Sub(loggedAt) < awaitingLogInterval {
			return
		}
		logged = reason
		loggedAt = c.now()
		c.logf(Event{Level: "info", Message: "collector awaiting enrollment: " + reason})
	}
	retry := firstEnrollmentRetry
	for {
		if ctx.Err() != nil {
			return config.Destination{}, "", false
		}
		destinations := c.cfg.Components.Collector.Destinations
		if len(destinations) == 0 {
			note("no destination is configured; run rigscope install --hub URL --token TOKEN")
			if !sleep(ctx, dormantPollInterval) {
				return config.Destination{}, "", false
			}
			continue
		}
		destination := destinations[0]
		credential, found, err := c.store.Credential(destination.URL)
		if err != nil {
			note("the stored credential could not be read: " + err.Error())
			if !sleep(ctx, dormantPollInterval) {
				return config.Destination{}, "", false
			}
			continue
		}
		if found {
			return destination, credential.Token, true
		}
		pending, hasPending, err := c.store.PendingEnrollment(destination.URL)
		if err != nil {
			note("the enrollment grant could not be read: " + err.Error())
			if !sleep(ctx, dormantPollInterval) {
				return config.Destination{}, "", false
			}
			continue
		}
		if !hasPending || pending.Grant == "" {
			note("no credential is stored for " + destination.URL + " and no enrollment grant is staged")
			if !sleep(ctx, dormantPollInterval) {
				return config.Destination{}, "", false
			}
			continue
		}
		c.logf(Event{
			Level:   "info",
			Message: "enrolling with the staged grant",
			Fields:  []Field{F("hub", destination.URL)},
		})
		result, err := enroll.Run(ctx, enroll.Options{
			Store:  c.store,
			HubURL: destination.URL,
			Grant:  pending.Grant,
			CAFile: destination.CAFile,
			Logf:   c.formatLogf("enroll"),
		})
		if err != nil {
			if ctx.Err() != nil {
				return config.Destination{}, "", false
			}
			c.logf(Event{
				Level:   "error",
				Message: "enrollment failed; the saved proof makes a retry safe",
				Fields:  []Field{F("hub", destination.URL), F("error", err.Error())},
			})
			if !sleep(ctx, retry) {
				return config.Destination{}, "", false
			}
			retry = min(retry*2, enrollmentRetryInterval)
			continue
		}
		return destination, result.Credential.Token, true
	}
}

// runSession samples and delivers until ctx is cancelled, then drains.
func (c *Collector) runSession(ctx context.Context, identity state.Identity, destination config.Destination, credential string) error {
	sequence := NewSequence()
	source, stopSource, err := c.buildSource(ctx, identity, sequence)
	if err != nil {
		return err
	}
	if stopSource != nil {
		defer stopSource()
	}
	scanner, err := c.processScanner()
	if err != nil {
		return err
	}
	sharing := sharingPolicy(c.cfg.Components.Collector.Sharing)
	interval := c.cfg.Components.Collector.SamplingInterval.Value()

	// Failures are cached from the sampling worker instead of read from the
	// source by the timer loop: a source is not safe for concurrent use, and
	// the status path must never wait for an in-flight sample.
	var failuresMu sync.Mutex
	lastFailures := source.Failures()
	currentFailures := func() []protocol.CollectionFailure {
		failuresMu.Lock()
		defer failuresMu.Unlock()
		return lastFailures
	}

	// welcome wakes the scheduler so a status is sent just after each welcome.
	// Until a session is welcomed the client drops control messages.
	welcome := make(chan struct{}, 1)

	var scheduler *processes.Scheduler
	client, err := delivery.New(delivery.Config{
		HubURL:           destination.URL,
		Credential:       credential,
		CAFile:           destination.CAFile,
		Identity:         delivery.Identity{CollectorID: identity.CollectorID, RunID: identity.RunID, BootID: identity.BootID, Hostname: identity.Hostname},
		ExpectedInterval: interval,
		Capabilities:     capabilitiesFor(sharing),
		AllocateSequence: sequence.Allocate,
		Logf:             c.formatLogf("delivery"),
		OnWelcome: func() {
			c.readyOnce(destination.URL)()
			select {
			case welcome <- struct{}{}:
			default:
			}
		},
		OnDisconnect: func() {
			if scheduler != nil {
				scheduler.CancelAll()
			}
		},
	})
	if err != nil {
		return err
	}

	scheduler, err = processes.NewScheduler(processes.SchedulerConfig{
		Scanner: scanner,
		Policy:  func() processes.Policy { return policy.ProcessesPolicy(sharing) },
		Send:    func(message processes.DetailMessage) bool { return sendDetail(client, message) },
		OnEvent: func(event processes.Event) { c.logProcessEvent(event) },
	})
	if err != nil {
		return err
	}
	defer scheduler.Stop()

	// Inventory is connection-scoped: seeding it before Run means every
	// welcome (including reconnects) re-announces the current machine.
	client.SetInventory(source.Inventory())
	lastInventory := source.InventoryVersion()

	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if err := client.Run(runCtx); err != nil {
			c.logf(Event{Level: "error", Message: "delivery stopped unexpectedly", Fields: []Field{F("error", err.Error())}})
		}
	}()
	go c.detailLoop(runCtx, client, scheduler)

	// Sampling runs on exactly one worker goroutine. The timer loop only
	// requests a sample; if the worker is still busy at the next cadence the
	// tick is skipped and counted, so a slow host sample can never delay
	// status delivery, detail subscriptions or signal handling.
	sampleCtx, cancelSampling := context.WithCancel(runCtx)
	sampleRequests := make(chan struct{}, 1)
	sampleDone := make(chan struct{})
	var sampling atomic.Bool
	requestSample := func() bool {
		if !sampling.CompareAndSwap(false, true) {
			return false
		}
		select {
		case sampleRequests <- struct{}{}:
			return true
		default:
			sampling.Store(false)
			return false
		}
	}
	go func() {
		defer close(sampleDone)
		for {
			select {
			case <-sampleCtx.Done():
				sampling.Store(false)
				return
			case <-sampleRequests:
				if sampleCtx.Err() != nil {
					sampling.Store(false)
					return
				}
				c.sample(client, source, &lastInventory)
				failures := source.Failures()
				failuresMu.Lock()
				lastFailures = failures
				failuresMu.Unlock()
				c.samples.Add(1)
				sampling.Store(false)
			}
		}
	}()

	// One timer drives both cadences so the status and the sample that land on
	// the same instant have a deterministic order: the status snapshot is taken
	// before the sample is requested, and the message is sent after the sample
	// request. A status therefore never reports the record it was produced next
	// to as backlog, and no record ever leaves after the report without being
	// part of it. The timer is always armed for the earliest due deadline.
	now := c.now()
	sampleAt := now.Add(interval)
	statusAt := now.Add(statusInterval)
	timer := time.NewTimer(nextDelay(now, sampleAt, statusAt))
	defer timer.Stop()

	requestSample()

	for {
		select {
		case <-ctx.Done():
			cancelSampling()
			return c.shutdown(client, scheduler, sampleDone, cancelRun, runDone)
		case <-welcome:
			// Queue the welcome status just after the callback returns rather
			// than capturing it inside OnWelcome: the delivery session sends
			// inventory and pumps queued records immediately afterwards.
			statusAt = c.now().Add(welcomeStatusDelay)
			resetTimer(timer, nextDelay(c.now(), sampleAt, statusAt))
		case <-timer.C:
			now := c.now()
			var status *protocol.StatusMessage
			if !now.Before(statusAt) {
				snapshot := client.StatusSnapshot(currentFailures())
				status = &snapshot
				statusAt = now.Add(statusInterval)
			}
			if !now.Before(sampleAt) {
				if !requestSample() {
					skipped := c.skippedSamples.Add(1)
					c.logf(Event{
						Level:   "warn",
						Message: "sampling is still running; skipping this cadence",
						Fields:  []Field{F("skippedSamples", skipped)},
					})
				}
				sampleAt = now.Add(interval)
			}
			if status != nil {
				client.SendStatusMessage(*status)
			}
			resetTimer(timer, nextDelay(c.now(), sampleAt, statusAt))
		}
	}
}

// sample assembles one record, admits it to the delivery queue under the local
// sharing policy and refreshes inventory when its version moved. A sampling
// failure is logged and the next cadence retries; it never stops delivery.
func (c *Collector) sample(client *delivery.Client, source Source, lastInventory *int64) {
	defer func() {
		if recovered := recover(); recovered != nil {
			c.logf(Event{Level: "error", Message: "sampling panicked; the next cadence retries", Fields: []Field{F("panic", fmt.Sprint(recovered))}})
		}
	}()
	record, err := source.Next()
	if err != nil {
		c.logf(Event{Level: "error", Message: "metrics sampling failed; the next cadence retries", Fields: []Field{F("error", err.Error())}})
		return
	}
	if err := client.Enqueue(record); err != nil {
		c.logf(Event{Level: "error", Message: "record could not be queued", Fields: []Field{F("sequence", record.Sequence), F("error", err.Error())}})
	}
	if version := source.InventoryVersion(); version != *lastInventory {
		// The client keeps the snapshot even when it is offline and announces
		// it on the next welcome, so the version is consumed here either way.
		client.SetInventory(source.Inventory())
		*lastInventory = version
	}
}

// shutdown stops the sampling worker within the shutdown deadline, drains the
// delivery queue with the remaining budget, then stops the session. Pending
// records are dropped by design when the drain window expires; the hub
// reports the hole as missing samples.
func (c *Collector) shutdown(client *delivery.Client, scheduler *processes.Scheduler, sampleDone <-chan struct{}, cancelRun func(), runDone <-chan struct{}) error {
	deadline := time.Now().Add(c.drainTimeout)
	c.logf(Event{Level: "info", Message: "collector stopping; draining the delivery queue"})
	if !waitUntil(sampleDone, deadline) {
		c.logf(Event{Level: "warn", Message: "the sampling worker did not stop before the shutdown deadline"})
	}
	scheduler.Stop()
	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), remaining)
	err := client.Drain(drainCtx)
	cancel()
	if err != nil {
		c.logf(Event{Level: "warn", Message: "delivery queue was not fully drained; pending samples are discarded", Fields: []Field{F("error", err.Error())}})
	}
	cancelRun()
	waitUntil(runDone, deadline)
	stats := client.Stats()
	fields := []Field{
		F("pendingRecords", stats.Queue.PendingRecords),
		F("inFlightRecords", stats.Queue.InFlightRecords),
		F("droppedRecords", stats.Queue.DroppedRecords),
		F("gapRecords", stats.Queue.GapRecords),
		F("samples", c.samples.Load()),
		F("skippedSamples", c.skippedSamples.Load()),
	}
	if stats.Queue.LastAckAt != nil {
		fields = append(fields, F("lastAckAt", stats.Queue.LastAckAt.UTC().Format(time.RFC3339Nano)))
	}
	c.logf(Event{Level: "info", Message: "collector stopped", Fields: fields})
	return nil
}

// detailLoop routes hub detail subscriptions and cancellations to the
// scheduler. The delivery channel closes when the client stops.
func (c *Collector) detailLoop(ctx context.Context, client *delivery.Client, scheduler *processes.Scheduler) {
	for {
		select {
		case <-ctx.Done():
			return
		case request, ok := <-client.Detail():
			if !ok {
				return
			}
			if request.Subscribe != nil {
				scheduler.Subscribe(processes.DetailSubscribe{
					Type:           "detail.subscribe",
					SubscriptionID: request.Subscribe.SubscriptionID,
					Kind:           string(request.Subscribe.Kind),
					IntervalMS:     request.Subscribe.IntervalMS,
					LeaseMS:        request.Subscribe.LeaseMS,
				})
			}
			if request.Cancel != nil {
				scheduler.Cancel(request.Cancel.SubscriptionID)
			}
		}
	}
}

// sendDetail converts one scheduler message to the wire contract. A result
// with an unsupported shape is dropped rather than stalling the subscription.
func sendDetail(client *delivery.Client, message processes.DetailMessage) bool {
	switch typed := message.(type) {
	case processes.DetailResult:
		result := metrics.ToProtocolDetailResult(typed)
		if !client.SendDetail(result) {
			return false
		}
		return true
	case processes.DetailEnded:
		return client.SendDetailEnded(protocol.DetailEnded{
			SubscriptionID: typed.SubscriptionID,
			Reason:         protocol.DetailEndedReason(typed.Reason),
		})
	default:
		return false
	}
}

// capabilitiesFor advertises what this collector can do. Process detail is
// only advertised when the local policy shares processes.
func capabilitiesFor(sharing policy.Policy) []protocol.CapabilityName {
	capabilities := []protocol.CapabilityName{protocol.CapabilityMetrics, protocol.CapabilityInventory}
	if sharing.Processes {
		capabilities = append(capabilities, protocol.CapabilityProcesses)
	}
	return capabilities
}

// sharingPolicy converts the configuration's explicit booleans into the
// effective local policy.
func sharingPolicy(sharing config.Sharing) policy.Policy {
	return policy.Policy{
		CPU:              sharing.CPU,
		Memory:           sharing.Memory,
		Swap:             sharing.Swap,
		GPUs:             sharing.GPUs,
		Disks:            sharing.Disks,
		Network:          sharing.Network,
		NetworkAddresses: sharing.NetworkAddresses,
		Processes:        sharing.Processes,
		ProcessArguments: sharing.ProcessArguments,
	}
}

// formatLogf adapts the packages that take printf-style callbacks to the
// structured logger.
func (c *Collector) formatLogf(component string) func(format string, args ...any) {
	return func(format string, args ...any) {
		c.logf(Event{Level: "info", Message: fmt.Sprintf(format, args...), Fields: []Field{F("component", component)}})
	}
}

func (c *Collector) logProcessEvent(event processes.Event) {
	fields := []Field{F("component", "processes"), F("event", string(event.Type))}
	if event.SubscriptionID != "" {
		fields = append(fields, F("subscriptionId", event.SubscriptionID))
	}
	if event.Reason != "" {
		fields = append(fields, F("reason", string(event.Reason)))
	}
	if event.Error != "" {
		fields = append(fields, F("error", event.Error))
	}
	if event.Scans > 0 {
		fields = append(fields, F("scans", event.Scans))
	}
	c.logf(Event{Level: "info", Message: "process detail", Fields: fields})
}

// sleep waits for duration or ctx cancellation. It reports false when ctx was
// cancelled first.
func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// nextDelay returns the delay to the earliest of the deadlines, measured from
// a fresh reading of now, with a minimum of one millisecond so a deadline that
// just passed does not busy-loop the timer.
func nextDelay(now time.Time, deadlines ...time.Time) time.Duration {
	earliest := deadlines[0]
	for _, deadline := range deadlines[1:] {
		if deadline.Before(earliest) {
			earliest = deadline
		}
	}
	delay := earliest.Sub(now)
	if delay < time.Millisecond {
		return time.Millisecond
	}
	return delay
}

// waitUntil waits for done or deadline. It reports false when the deadline
// expired first.
func waitUntil(done <-chan struct{}, deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// resetTimer restarts a one-shot timer at a new delay, draining a value that
// fired but was not received yet.
func resetTimer(timer *time.Timer, duration time.Duration) {
	if duration < time.Millisecond {
		duration = time.Millisecond
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

// CheckResult describes what a readiness probe found. It never carries a
// failure for "awaiting enrollment": an unenrolled collector is healthy, it
// simply has nothing to deliver to yet.
type CheckResult struct {
	Enabled      bool
	Destination  string
	Enrolled     bool
	PendingGrant bool
}

// Check is the readiness probe used by the upgrade transaction: the
// configuration must load strictly and the collector state must be readable.
// It returns an error (exit 1) only for those two conditions.
func Check(cfg *config.Config, stateDir string) (CheckResult, error) {
	if cfg == nil {
		return CheckResult{}, errors.New("runtime: no configuration was loaded")
	}
	if err := cfg.Validate(); err != nil {
		return CheckResult{}, fmt.Errorf("runtime: invalid configuration: %w", err)
	}
	info, err := os.Stat(stateDir)
	if err != nil {
		return CheckResult{}, fmt.Errorf("runtime: state directory %s: %w", stateDir, err)
	}
	if !info.IsDir() {
		return CheckResult{}, fmt.Errorf("runtime: state directory %s is not a directory", stateDir)
	}
	store, err := state.Open(stateDir, state.Options{})
	if err != nil {
		return CheckResult{}, fmt.Errorf("runtime: %w", err)
	}
	// Validate the stored identity read-only: a readiness probe must never
	// create an installation id, adopt this machine's fingerprint or discard a
	// cloned installation's credentials.
	if err := store.ValidateIdentity(); err != nil {
		return CheckResult{}, fmt.Errorf("runtime: %w", err)
	}
	// The readiness probe must catch the permission problem that stops the
	// service: the lease has to be creatable by the collector account inside
	// its own state subtree.
	if err := checkLeaseWritable(stateDir); err != nil {
		return CheckResult{}, err
	}
	credentials, err := store.Credentials()
	if err != nil {
		return CheckResult{}, fmt.Errorf("runtime: %w", err)
	}
	pending, err := store.PendingEnrollments()
	if err != nil {
		return CheckResult{}, fmt.Errorf("runtime: %w", err)
	}
	result := CheckResult{Enabled: cfg.Components.Collector.Enabled}
	if destinations := cfg.Components.Collector.Destinations; len(destinations) > 0 {
		result.Destination = destinations[0].URL
	}
	for _, credential := range credentials {
		if credential.URL == result.Destination {
			result.Enrolled = true
		}
	}
	for _, enrollment := range pending {
		if enrollment.URL == result.Destination {
			result.PendingGrant = true
		}
	}
	return result, nil
}

// checkLeaseWritable proves the collector can create and write its lease. It
// mirrors the open the runtime performs, so a root-owned state root with a
// collector-owned subdirectory passes while the old in-root lease path fails.
func checkLeaseWritable(stateRoot string) error {
	lease := LeasePath(stateRoot)
	// If the lease already exists, prove it can be opened for writing. The
	// probe must not create or replace it: creating a root-owned 0600 lease
	// would stop the collector account opening it, and replacing a live lock
	// inode would break mutual exclusion.
	file, err := os.OpenFile(lease, os.O_WRONLY, 0)
	if err == nil {
		if err := file.Close(); err != nil {
			return fmt.Errorf("runtime: collector lease %s: %w", lease, err)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("runtime: collector lease %s is not writable: %w", lease, err)
	}
	// No lease yet: prove the collector-owned directory is writable with a
	// uniquely named probe that is removed immediately.
	probe, err := os.CreateTemp(filepath.Dir(lease), ".lease-check-*")
	if err != nil {
		return fmt.Errorf("runtime: collector lease directory %s is not writable: %w", filepath.Dir(lease), err)
	}
	name := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		return fmt.Errorf("runtime: collector lease probe %s: %w", name, closeErr)
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("runtime: remove collector lease probe %s: %w", name, removeErr)
	}
	return nil
}

// readyOnce returns a welcome hook that calls OnReady at most once per process.
func (c *Collector) readyOnce(hubURL string) func() {
	return func() {
		if c.welcomed {
			return
		}
		c.welcomed = true
		if c.onReady != nil {
			c.onReady(hubURL)
		}
	}
}
