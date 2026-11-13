package cli

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/alerts"
	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/protocol"
)

const (
	// alertTickInterval matches the reference hub's one-second liveness tick.
	alertTickInterval = time.Second
	// alertObserveQueueDepth bounds queued live observations. A collector that
	// outruns the alert engine loses alert evaluation, never telemetry
	// ingestion: the ingest read loop only performs a non-blocking send. A
	// drop breaks continuity for that machine instead of silently changing a
	// sustained-duration threshold.
	alertObserveQueueDepth = 256
	// alertDrainTimeout bounds in-flight webhook work at shutdown.
	alertDrainTimeout = 2 * time.Second
	// alertNotificationTimeout bounds one explicit delivery run.
	alertNotificationTimeout = 30 * time.Second
	// alertDropLogInterval rate-limits the queue-full diagnostic.
	alertDropLogInterval = 10 * time.Second
)

// alertObservation is one queued LIVE record evaluation.
type alertObservation struct {
	machineID  string
	record     protocol.MetricsRecord
	receivedAt time.Time
}

// AlertDiagnostics reports alert evaluation losses that affect rule semantics.
type AlertDiagnostics struct {
	// DroppedObservations counts LIVE records the bounded queue refused.
	DroppedObservations uint64
	// ContinuityResets counts machines whose sustained-duration state was
	// reset after a dropped observation, so a gap cannot satisfy a duration.
	ContinuityResets uint64
	// ContinuityResetFailures counts continuity reset writes that failed. A
	// failed break stays unresolved and is retried.
	ContinuityResetFailures uint64
	// UnresolvedBreaks lists machines whose continuity reset has not committed.
	UnresolvedBreaks []string
	// QueueDepth and QueueCapacity describe the bounded observation queue.
	QueueDepth    int
	QueueCapacity int
	// LastTickAt is the engine clock of the most recent liveness tick.
	LastTickAt time.Time
	// ObservationsAbandoned counts LIVE record evaluations shutdown refused:
	// records still queued when the worker stopped plus admissions that raced
	// or arrived after shutdown began.
	ObservationsAbandoned uint64
}

// continuityBreak is one machine's pending continuity reset. Generation lets a
// success remove exactly the break it attempted without erasing a newer drop
// that arrived while the reset write was in flight.
type continuityBreak struct {
	generation uint64
	since      time.Time
}

// alertRuntime feeds the alert engine from ingestion and live machine state.
//
// Ingestion calls Observe on the collector read goroutine. The call only
// performs a bounded, non-blocking enqueue; a single worker owns both the
// observation queue and the liveness tick so a SQLite write can never stall a
// collector session and a tick can never overtake an earlier observation.
type alertRuntime struct {
	engine *alerts.Engine
	state  *live.State
	logger *slog.Logger

	observe chan alertObservation
	stop    chan struct{}
	// admissionMu serialises observation admission with shutdown. An enqueuer
	// holds the read lock from its stop check through its send; Stop takes the
	// write lock, marks the runtime stopped and only then drains, so every
	// observation is either evaluated, counted as dropped, or counted as
	// abandoned.
	admissionMu      sync.RWMutex
	admissionStopped bool
	// testAfterAdmissionCheck is a test seam invoked while an observation holds
	// the admission read lock, after it observed the stopped flag.
	testAfterAdmissionCheck func()
	// notifyCtx is cancelled at Stop so an in-flight ProcessNotifications run
	// cannot keep the database busy after shutdown begins.
	notifyCtx    context.Context
	notifyCancel context.CancelFunc

	stopOnce   sync.Once
	workerDone chan struct{}
	notifyWG   sync.WaitGroup

	dropped                 atomic.Uint64
	continuityResets        atomic.Uint64
	continuityResetFailures atomic.Uint64
	abandoned               atomic.Uint64
	lastDropLog             atomic.Int64
	notifyActive            atomic.Bool

	// broken collects machines whose observation stream lost continuity. The
	// set coalesces repeated drops and is applied by the single worker before
	// it evaluates newer observations or a tick. Entries stay until their reset
	// commits, so a failed write is retried rather than forgotten.
	brokenMu    sync.Mutex
	broken      map[string]continuityBreak
	brokenSeq   uint64
	brokenReady chan struct{}
}

// newAlertRuntime starts the observation worker and the liveness ticker.
func newAlertRuntime(engine *alerts.Engine, state *live.State, logger *slog.Logger) *alertRuntime {
	notifyCtx, notifyCancel := context.WithCancel(context.Background())
	runtime := &alertRuntime{
		engine:       engine,
		state:        state,
		logger:       logger,
		observe:      make(chan alertObservation, alertObserveQueueDepth),
		stop:         make(chan struct{}),
		workerDone:   make(chan struct{}),
		notifyCtx:    notifyCtx,
		notifyCancel: notifyCancel,
		broken:       make(map[string]continuityBreak),
		brokenReady:  make(chan struct{}, 1),
	}
	go runtime.consume()
	return runtime
}

// Observe queues one published LIVE record for evaluation. It never blocks the
// caller. A full queue drops the observation, records a diagnostic counter and
// marks the machine's continuity broken so the gap cannot extend a pending
// duration. Admission holds the read side of the shutdown lock, so a send can
// never land in the queue after the final drain has counted it.
func (r *alertRuntime) Observe(machineID string, record protocol.MetricsRecord, receivedAt time.Time) {
	r.admissionMu.RLock()
	defer r.admissionMu.RUnlock()
	if r.admissionStopped {
		// Shutdown already owns the final drain. This observation is abandoned
		// evaluation, never telemetry ingestion; count it so every published
		// LIVE record is accounted for.
		r.abandoned.Add(1)
		return
	}
	if r.testAfterAdmissionCheck != nil {
		r.testAfterAdmissionCheck()
	}
	select {
	case r.observe <- alertObservation{machineID: machineID, record: record, receivedAt: receivedAt}:
	default:
		r.logDrop(machineID, receivedAt)
	}
}

// logDrop counts one dropped observation and emits a rate-limited diagnostic.
func (r *alertRuntime) logDrop(machineID string, receivedAt time.Time) {
	dropped := r.dropped.Add(1)
	r.markContinuityBroken(machineID, receivedAt)
	now := time.Now().UnixNano()
	last := r.lastDropLog.Load()
	if last != 0 && now-last < int64(alertDropLogInterval) {
		return
	}
	if !r.lastDropLog.CompareAndSwap(last, now) {
		return
	}
	r.logger.Warn("alert observation queue full; dropping live sample and resetting continuity",
		"machineId", machineID, "dropped", dropped,
		"continuityResets", r.continuityResets.Load(), "queueDepth", cap(r.observe))
}

// markContinuityBroken coalesces a machine into the pending reset set. The
// dropped record's receipt time is the earliest post-gap evidence that may
// restart a sustained duration; every earlier receipt is pre-gap.
func (r *alertRuntime) markContinuityBroken(machineID string, receivedAt time.Time) {
	if machineID == "" {
		return
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}
	r.brokenMu.Lock()
	r.brokenSeq++
	existing, ok := r.broken[machineID]
	if !ok || receivedAt.After(existing.since) {
		existing.since = receivedAt
	}
	existing.generation = r.brokenSeq
	r.broken[machineID] = existing
	r.brokenMu.Unlock()
	select {
	case r.brokenReady <- struct{}{}:
	default:
	}
}

// continuityBreaks snapshots the unresolved breaks without removing them.
func (r *alertRuntime) continuityBreaks() []struct {
	machineID string
	breakInfo continuityBreak
} {
	r.brokenMu.Lock()
	defer r.brokenMu.Unlock()
	if len(r.broken) == 0 {
		return nil
	}
	out := make([]struct {
		machineID string
		breakInfo continuityBreak
	}, 0, len(r.broken))
	for machineID, info := range r.broken {
		out = append(out, struct {
			machineID string
			breakInfo continuityBreak
		}{machineID: machineID, breakInfo: info})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].machineID < out[j].machineID })
	return out
}

// resolveContinuityBreak removes the exact generation that committed.
func (r *alertRuntime) resolveContinuityBreak(machineID string, generation uint64) {
	r.brokenMu.Lock()
	defer r.brokenMu.Unlock()
	if current, ok := r.broken[machineID]; ok && current.generation == generation {
		delete(r.broken, machineID)
	}
}

// unresolvedBreakIDs returns the sorted machine ids whose reset has not
// committed.
func (r *alertRuntime) unresolvedBreakIDs() []string {
	r.brokenMu.Lock()
	defer r.brokenMu.Unlock()
	if len(r.broken) == 0 {
		return []string{}
	}
	machines := make([]string, 0, len(r.broken))
	for machineID := range r.broken {
		machines = append(machines, machineID)
	}
	sort.Strings(machines)
	return machines
}

// applyContinuityBreaks reconciles rule state from storage for every machine
// whose observation stream lost continuity. A failed reset stays in the set so
// the next pass retries it; the engine also keeps treating the machine as
// broken until that retry commits.
func (r *alertRuntime) applyContinuityBreaks() {
	for _, item := range r.continuityBreaks() {
		if err := r.engine.BreakContinuityAt(item.machineID, item.breakInfo.since); err != nil {
			failures := r.continuityResetFailures.Add(1)
			r.logger.Warn("alert continuity reset failed; will retry",
				"machineId", item.machineID, "failures", failures, "error", err)
			continue
		}
		r.resolveContinuityBreak(item.machineID, item.breakInfo.generation)
		resets := r.continuityResets.Add(1)
		r.logger.Warn("alert observation continuity reset after a queue drop",
			"machineId", item.machineID, "continuityResets", resets)
	}
}

// DroppedObservations reports how many LIVE records were dropped because the
// observation queue was full. Drops affect alert evaluation only, never
// ingestion or history.
func (r *alertRuntime) DroppedObservations() uint64 { return r.dropped.Load() }

// ContinuityResets reports how many machines had sustained-duration state reset
// after a dropped observation.
func (r *alertRuntime) ContinuityResets() uint64 { return r.continuityResets.Load() }

// ContinuityResetFailures reports how many continuity reset writes failed.
func (r *alertRuntime) ContinuityResetFailures() uint64 {
	return r.continuityResetFailures.Load()
}

// ObservationsAbandoned reports how many queued LIVE records shutdown dropped.
func (r *alertRuntime) ObservationsAbandoned() uint64 { return r.abandoned.Load() }

// Diagnostics returns the alert runtime's loss counters for status and
// diagnostic surfaces.
func (r *alertRuntime) Diagnostics() AlertDiagnostics {
	var lastTick time.Time
	if r.engine != nil {
		lastTick = r.engine.LastTickAt()
	}
	return AlertDiagnostics{
		DroppedObservations:     r.dropped.Load(),
		ContinuityResets:        r.continuityResets.Load(),
		ContinuityResetFailures: r.continuityResetFailures.Load(),
		UnresolvedBreaks:        r.unresolvedBreakIDs(),
		QueueDepth:              len(r.observe),
		QueueCapacity:           cap(r.observe),
		LastTickAt:              lastTick,
		ObservationsAbandoned:   r.abandoned.Load(),
	}
}

// APIDiagnostics maps the runtime counters onto the authenticated admin
// diagnostics response.
func (r *alertRuntime) APIDiagnostics() api.AlertDiagnostics {
	diagnostics := r.Diagnostics()
	breaks := diagnostics.UnresolvedBreaks
	if breaks == nil {
		breaks = []string{}
	}
	out := api.AlertDiagnostics{
		GeneratedAt:             time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		DroppedObservations:     diagnostics.DroppedObservations,
		ObservationsAbandoned:   diagnostics.ObservationsAbandoned,
		ContinuityResets:        diagnostics.ContinuityResets,
		ContinuityResetFailures: diagnostics.ContinuityResetFailures,
		UnresolvedBreaks:        breaks,
		QueueDepth:              int64(diagnostics.QueueDepth),
		QueueCapacity:           int64(diagnostics.QueueCapacity),
	}
	if !diagnostics.LastTickAt.IsZero() {
		lastTick := diagnostics.LastTickAt.UTC().Format("2006-01-02T15:04:05.000Z")
		out.LastTickAt = &lastTick
	}
	return out
}

// consume is the single owner of observation evaluation and liveness ticks. It
// drains queued observations before every tick, so a tick can never overtake a
// record that arrived earlier.
func (r *alertRuntime) consume() {
	defer close(r.workerDone)
	ticker := time.NewTicker(alertTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-r.brokenReady:
			r.applyContinuityBreaks()
		case observation := <-r.observe:
			r.applyContinuityBreaks()
			r.engine.ObserveAt(observation.machineID, observation.record, alerts.Live, observation.receivedAt)
		case <-ticker.C:
			r.tick()
		}
	}
}

// tick evaluates the backlog and then the liveness snapshot. The receipt
// cutoff is the newest record that was already queued when the drain started;
// arrivals during the drain stay queued for the next tick, so Tick can never
// judge evidence the hub has received but not yet evaluated.
func (r *alertRuntime) tick() {
	r.applyContinuityBreaks()
	cutoff := r.drainObservations()
	r.applyContinuityBreaks()
	r.engine.TickAsOf(r.machineStates(), cutoff)
	r.driveNotifications()
}

// drainObservations processes exactly the records that were queued when it was
// called and returns their newest receipt time. A snapshot of the queue length
// beforehand keeps arrivals during the drain out of both this evaluation and
// the tick's horizon. The channel is FIFO and this worker is its only consumer.
func (r *alertRuntime) drainObservations() time.Time {
	depth := len(r.observe)
	var cutoff time.Time
	for i := 0; i < depth; i++ {
		select {
		case <-r.stop:
			return cutoff
		case observation := <-r.observe:
			if observation.receivedAt.After(cutoff) {
				cutoff = observation.receivedAt
			}
			r.engine.ObserveAt(observation.machineID, observation.record, alerts.Live, observation.receivedAt)
		default:
			return cutoff
		}
	}
	return cutoff
}

// driveNotifications gives the explicitly-driven delivery loop the same cadence
// as the reference hub's tick. The engine itself also schedules notification
// work from Tick/ObserveAt, so this call coalesces with a run already in flight
// and never delays the next liveness tick.
func (r *alertRuntime) driveNotifications() {
	if !r.notifyActive.CompareAndSwap(false, true) {
		return
	}
	r.notifyWG.Add(1)
	go func() {
		defer r.notifyWG.Done()
		defer r.notifyActive.Store(false)
		ctx, cancel := context.WithTimeout(r.notifyCtx, alertNotificationTimeout)
		defer cancel()
		err := r.engine.ProcessNotifications(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			r.logger.Warn("alert notification run failed", "error", err)
		}
	}()
}

// machineStates adapts the live fleet view to the alert engine's tick input.
// Identity, advertised cadence and the durable last-received time are inputs.
// The engine judges stale and unavailable rules from observations it evaluated
// itself, never from live state that may still contain a record waiting in this
// queue; it uses the last-received time only as a restart fallback for receipts
// older than its own start.
func (r *alertRuntime) machineStates() []alerts.MachineState {
	if r.state == nil {
		return nil
	}
	machines := r.state.MachineTelemetry()
	states := make([]alerts.MachineState, 0, len(machines))
	for _, machine := range machines {
		states = append(states, alerts.MachineState{
			MachineID:          machine.MachineID,
			LastReceivedAt:     machine.LastReceivedAt,
			ExpectedIntervalMs: machine.ExpectedIntervalMS,
		})
	}
	return states
}

// Stop ends the ticker and the observation worker, then stops and drains the
// engine before the composition root flushes telemetry and closes storage.
func (r *alertRuntime) Stop() {
	r.stopOnce.Do(func() {
		// Wait for every admission already past its stopped check, then refuse
		// new ones. A sender that observed the old state has completed its send
		// before the final drain below, so its record is counted.
		r.admissionMu.Lock()
		r.admissionStopped = true
		close(r.stop)
		r.admissionMu.Unlock()
		if r.notifyCancel != nil {
			r.notifyCancel()
		}
		<-r.workerDone
		// The worker has exited; whatever it left in the queue is abandoned
		// evaluation, never telemetry ingestion.
		for {
			select {
			case <-r.observe:
				r.abandoned.Add(1)
			default:
				goto drained
			}
		}
	drained:
		if diagnostics := r.Diagnostics(); diagnostics.DroppedObservations > 0 ||
			diagnostics.ContinuityResets > 0 || diagnostics.ContinuityResetFailures > 0 ||
			diagnostics.ObservationsAbandoned > 0 || len(diagnostics.UnresolvedBreaks) > 0 {
			r.logger.Warn("alert evaluation finished with losses",
				"droppedObservations", diagnostics.DroppedObservations,
				"continuityResets", diagnostics.ContinuityResets,
				"continuityResetFailures", diagnostics.ContinuityResetFailures,
				"unresolvedBreaks", diagnostics.UnresolvedBreaks,
				"observationsAbandoned", diagnostics.ObservationsAbandoned)
		}
		r.engine.Stop()
		r.notifyWG.Wait()
		r.engine.Drain(alertDrainTimeout)
	})
}
