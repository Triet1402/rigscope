package processes

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

const (
	defaultMinInterval      = time.Second
	defaultMaxSubscriptions = 32
	scanFailureThreshold    = 3
)

// Policy is the local authorization for process detail. A collector talks to
// one hub in 1.0, so one policy applies to every subscription. The zero value
// fails closed.
type Policy struct {
	Processes        bool
	ProcessArguments bool
}

// SnapshotScanner is the scheduler's view of a Scanner.
type SnapshotScanner interface {
	Scan(context.Context, ScanOptions) (ProcessSnapshot, error)
}

// SendFunc delivers one detail message. Returning false means the transport
// cannot take it now; the scheduler retries on the next cadence and does not
// mark it as delivered. It must not call back into the Scheduler.
type SendFunc func(message DetailMessage) bool

// Timer is one-shot wake-up timer. Stop cancels a timer that has not fired.
type Timer interface {
	Stop()
}

// TimerFactory schedules fn after delay. Tests inject a fake clock and timer
// queue so cadence and leases are deterministic.
type TimerFactory func(delay time.Duration, fn func()) Timer

func afterFuncTimer(delay time.Duration, fn func()) Timer {
	return stopOnlyTimer{timer: time.AfterFunc(delay, fn)}
}

type stopOnlyTimer struct {
	timer *time.Timer
}

func (t stopOnlyTimer) Stop() {
	t.timer.Stop()
}

// EventType identifies an observability event.
type EventType string

const (
	EventSubscribed  EventType = "subscribed"
	EventEnded       EventType = "ended"
	EventScan        EventType = "scan"
	EventSendSkipped EventType = "send-skipped"
)

// Event is an observability event. Which fields apply depends on Type:
//
//	subscribed   SubscriptionID, Renewed
//	ended        SubscriptionID, Reason
//	scan         Scans, Subscriptions, Duration, Error
//	send-skipped SubscriptionID
type Event struct {
	Type           EventType
	SubscriptionID string
	Renewed        bool
	Reason         DetailEndedReason
	Scans          int
	Subscriptions  int
	Duration       time.Duration
	Error          string
}

// SchedulerConfig configures a Scheduler.
type SchedulerConfig struct {
	// Scanner performs shared scans. Required.
	Scanner SnapshotScanner
	// Policy is re-read at subscribe time, before every scan, and immediately
	// before every result send, so a policy reduction stops work promptly. A
	// nil Policy fails closed (no process detail).
	Policy func() Policy
	// Send delivers results and terminal acknowledgements. Required for
	// delivery; nil drops every message.
	Send SendFunc
	// MinInterval is the local scan floor; default 1s. Zero selects the
	// default. Negative values are treated as zero.
	MinInterval time.Duration
	// MaxSubscriptions is the per-hub subscription budget; default 32. Zero
	// selects the default.
	MaxSubscriptions int
	// Clock is the lease and cadence clock (monotonic in production). Default
	// time.Now.
	Clock func() time.Time
	// Now is the wall clock used for capturedAt. Default Clock.
	Now func() time.Time
	// NewTimer schedules the scheduler's single wake-up. Default a real timer.
	NewTimer TimerFactory
	// OnEvent observes scheduler decisions. It must be fast and must not call
	// back into the Scheduler; errors cannot be reported and are not retried.
	OnEvent func(Event)
}

// SchedulerStats is a point-in-time view of the scheduler.
type SchedulerStats struct {
	Active int
	// Scanning is true while a scan is in flight.
	Scanning bool
	// Scans counts scan attempts, including failed attempts.
	Scans int
	// LastScan is the clock time of the last scan start; zero before the first.
	LastScan time.Time
	// LastScanDuration is the duration of the last attempt; zero before the
	// first completed attempt.
	LastScanDuration time.Duration
}

type subscription struct {
	interval    time.Duration
	lease       time.Duration
	expiresAt   time.Time
	lastSentAt  time.Time
	hasLastSent bool
	failures    int
}

// Scheduler owns the collector's process-detail subscriptions. The hub creates
// bounded, expiring subscriptions; overlapping subscriptions share one scan at
// the fastest locally permitted cadence, and each subscription receives
// results no more often than its own interval.
//
// Unlike the prototype this scheduler serves a single hub, so there is no
// destination dimension and no per-destination policy. Subscription semantics
// (create-or-renew, lease, budgets, failure threshold, cancellation) are
// otherwise unchanged.
type Scheduler struct {
	mu sync.Mutex

	scanner          SnapshotScanner
	policy           func() Policy
	send             SendFunc
	minInterval      time.Duration
	maxSubscriptions int
	clock            func() time.Time
	now              func() time.Time
	newTimer         TimerFactory
	onEvent          func(Event)

	subscriptions map[string]*subscription
	timer         Timer
	timerAt       time.Time

	stopped  bool
	scanning bool
	scans    int

	// scanCancel stops the in-flight scan; scanFinished closes when it has
	// returned. Stop uses both so shutdown does not wait for a whole scan.
	scanCancel   context.CancelFunc
	scanFinished chan struct{}

	lastScan         time.Time
	lastScanDuration time.Duration

	// scanBaseline is the start of the current cadence; reset when idle.
	scanBaseline    time.Time
	hasScanBaseline bool
	nextScanAt      time.Time
	hasNextScan     bool
}

// NewScheduler validates the configuration and returns a scheduler. Call Stop
// to release its timer.
func NewScheduler(config SchedulerConfig) (*Scheduler, error) {
	if config.Scanner == nil {
		return nil, errors.New("processes: SchedulerConfig.Scanner is required")
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	now := config.Now
	if now == nil {
		now = clock
	}
	newTimer := config.NewTimer
	if newTimer == nil {
		newTimer = afterFuncTimer
	}
	minInterval := config.MinInterval
	if minInterval == 0 {
		minInterval = defaultMinInterval
	}
	if minInterval < 0 {
		minInterval = 0
	}
	maxSubscriptions := config.MaxSubscriptions
	if maxSubscriptions == 0 {
		maxSubscriptions = defaultMaxSubscriptions
	}
	if maxSubscriptions < 0 {
		maxSubscriptions = 0
	}
	return &Scheduler{
		scanner:          config.Scanner,
		policy:           config.Policy,
		send:             config.Send,
		minInterval:      minInterval,
		maxSubscriptions: maxSubscriptions,
		clock:            clock,
		now:              now,
		newTimer:         newTimer,
		onEvent:          config.OnEvent,
		subscriptions:    make(map[string]*subscription),
	}, nil
}

// Subscribe creates or renews a subscription. Re-sending an active id extends
// its lease and may adjust its interval, but never bypasses the subscription's
// own delivery interval.
func (s *Scheduler) Subscribe(message DetailSubscribe) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}

	policy := s.policyLocked()
	_, existing := s.subscriptions[message.SubscriptionID]

	if !policy.Processes {
		// A hub that may not see processes must not keep a previously granted
		// subscription alive after its policy was reduced.
		s.withholdAllLocked()
		if !existing {
			s.sendEndedLocked(message.SubscriptionID, ReasonWithheld)
		}
		s.scheduleLocked()
		return
	}

	if !existing && len(s.subscriptions) >= s.maxSubscriptions {
		s.sendEndedLocked(message.SubscriptionID, ReasonBusy)
		return
	}

	interval := clampDetailInterval(message.IntervalMS)
	lease := clampDetailLease(message.LeaseMS)
	expiresAt := s.clock().Add(lease)

	if existing {
		active := s.subscriptions[message.SubscriptionID]
		active.interval = interval
		active.lease = lease
		active.expiresAt = expiresAt
		s.emitLocked(Event{Type: EventSubscribed, SubscriptionID: message.SubscriptionID, Renewed: true})
	} else {
		s.subscriptions[message.SubscriptionID] = &subscription{
			interval:  interval,
			lease:     lease,
			expiresAt: expiresAt,
		}
		s.emitLocked(Event{Type: EventSubscribed, SubscriptionID: message.SubscriptionID})
	}
	s.scheduleLocked()
}

// Cancel ends one subscription and acknowledges it.
func (s *Scheduler) Cancel(subscriptionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	if _, ok := s.subscriptions[subscriptionID]; !ok {
		return
	}
	delete(s.subscriptions, subscriptionID)
	s.sendEndedLocked(subscriptionID, ReasonCancelled)
	s.scheduleLocked()
}

// CancelAll drops every subscription, for example when the hub connection
// closes. Each subscription is acknowledged as cancelled.
func (s *Scheduler) CancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	if len(s.subscriptions) == 0 {
		return
	}
	for id := range s.subscriptions {
		delete(s.subscriptions, id)
		s.sendEndedLocked(id, ReasonCancelled)
	}
	s.scheduleLocked()
}

// Stop ends all work, releases the timer and waits for an in-flight scan to
// return. A scan that is reading /proc is cancelled first; its result is
// discarded. Cancellations are reported through OnEvent only because nothing
// can be sent while shutting down.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		s.stopTimerLocked()
		for id := range s.subscriptions {
			s.emitLocked(Event{Type: EventEnded, SubscriptionID: id, Reason: ReasonCancelled})
		}
		s.subscriptions = make(map[string]*subscription)
		s.scanning = false
		s.hasScanBaseline = false
		s.hasNextScan = false
	}
	cancel := s.scanCancel
	finished := s.scanFinished
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if finished != nil {
		<-finished
	}
}

// Stats returns a consistent snapshot of the scheduler state.
func (s *Scheduler) Stats() SchedulerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SchedulerStats{
		Active:           len(s.subscriptions),
		Scanning:         s.scanning,
		Scans:            s.scans,
		LastScan:         s.lastScan,
		LastScanDuration: s.lastScanDuration,
	}
}

// --- scheduling -----------------------------------------------------------

func (s *Scheduler) cadenceLocked() (time.Duration, bool) {
	if len(s.subscriptions) == 0 {
		return 0, false
	}
	fastest := time.Duration(1<<63 - 1)
	for _, active := range s.subscriptions {
		if active.interval > 0 && active.interval < fastest {
			fastest = active.interval
		}
	}
	if fastest == time.Duration(1<<63-1) {
		return 0, false
	}
	if fastest < s.minInterval {
		fastest = s.minInterval
	}
	return fastest, true
}

func (s *Scheduler) scheduleLocked() {
	if s.stopped {
		return
	}
	if s.scanning {
		// The in-flight scan schedules the next cadence when it returns, but
		// leases must still expire on time while /proc is being read.
		s.armExpiryTimerLocked()
		return
	}
	if len(s.subscriptions) == 0 {
		s.stopTimerLocked()
		s.hasScanBaseline = false
		s.hasNextScan = false
		return
	}
	cadence, ok := s.cadenceLocked()
	if !ok {
		s.stopTimerLocked()
		s.hasNextScan = false
		return
	}

	now := s.clock()
	var nextScan time.Time
	if !s.hasScanBaseline {
		// A fresh subscription gets its first result immediately; later ones
		// are paced by the shared cadence.
		nextScan = now
	} else {
		nextScan = s.scanBaseline.Add(cadence)
		// A scan that took longer than the cadence must not queue back-to-back
		// work; wait one fresh cadence after the overrun was noticed.
		if !nextScan.After(now) {
			nextScan = now.Add(cadence)
		}
	}

	at := nextScan
	if expiry, ok := s.nextExpiryLocked(); ok && expiry.Before(at) {
		at = expiry
	}

	s.nextScanAt = nextScan
	s.hasNextScan = true
	s.armTimerLocked(at)
}

// nextExpiryLocked returns the nearest lease expiry, if any.
func (s *Scheduler) nextExpiryLocked() (time.Time, bool) {
	var next time.Time
	for _, active := range s.subscriptions {
		if next.IsZero() || active.expiresAt.Before(next) {
			next = active.expiresAt
		}
	}
	return next, !next.IsZero()
}

// armExpiryTimerLocked wakes the scheduler at the nearest lease expiry, so a
// scan in flight cannot keep an expired subscription alive.
func (s *Scheduler) armExpiryTimerLocked() {
	expiry, ok := s.nextExpiryLocked()
	if !ok {
		s.stopTimerLocked()
		return
	}
	s.armTimerLocked(expiry)
}

func (s *Scheduler) armTimerLocked(at time.Time) {
	if s.timer != nil && s.timerAt.Equal(at) {
		return
	}
	s.stopTimerLocked()

	delay := at.Sub(s.clock())
	if delay < 0 {
		delay = 0
	}
	var handle Timer
	handle = s.newTimer(delay, func() {
		s.mu.Lock()
		if s.timer != handle {
			s.mu.Unlock()
			return
		}
		s.timer = nil
		s.mu.Unlock()
		s.tick()
	})
	s.timer = handle
	s.timerAt = at
}

func (s *Scheduler) stopTimerLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

func (s *Scheduler) tick() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	now := s.clock()
	s.expireDueLocked(now)
	if len(s.subscriptions) == 0 {
		s.scheduleLocked()
		s.mu.Unlock()
		return
	}
	if s.scanning {
		// Keep the lease timer armed; the scan schedules the next cadence
		// when it finishes.
		s.armExpiryTimerLocked()
		s.mu.Unlock()
		return
	}
	if s.hasNextScan && !now.Before(s.nextScanAt) {
		s.runScanLocked(now)
		return
	}
	s.scheduleLocked()
	s.mu.Unlock()
}

func (s *Scheduler) expireDueLocked(now time.Time) {
	for id, active := range s.subscriptions {
		if now.Before(active.expiresAt) {
			continue
		}
		delete(s.subscriptions, id)
		s.sendEndedLocked(id, ReasonExpired)
	}
}

// runScanLocked starts one shared scan. It is called with the lock held and
// returns with the lock released. The scan itself runs without the lock so
// Stats, policy changes and lease expiries stay responsive while /proc is
// being read.
func (s *Scheduler) runScanLocked(started time.Time) {
	policy := s.policyLocked()
	if !policy.Processes {
		s.withholdAllLocked()
	}
	if len(s.subscriptions) == 0 {
		s.scanning = false
		s.scheduleLocked()
		s.mu.Unlock()
		return
	}

	includeCommand := policy.ProcessArguments
	s.scans++
	s.lastScan = started
	s.scanBaseline = started
	s.hasScanBaseline = true
	s.scanning = true
	s.hasNextScan = false

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	s.scanCancel = cancel
	s.scanFinished = finished

	scanner := s.scanner
	s.armExpiryTimerLocked()
	s.mu.Unlock()

	snapshot, err := scanner.Scan(ctx, ScanOptions{IncludeCommand: includeCommand})
	finishedAt := s.clock()
	cancel()

	s.mu.Lock()
	s.scanCancel = nil
	s.scanFinished = nil
	s.lastScanDuration = finishedAt.Sub(started)
	if s.lastScanDuration < 0 {
		s.lastScanDuration = 0
	}
	if s.stopped {
		s.mu.Unlock()
		close(finished)
		return
	}

	if err != nil {
		for id, active := range s.subscriptions {
			active.failures++
			if active.failures < scanFailureThreshold {
				continue
			}
			delete(s.subscriptions, id)
			s.sendEndedLocked(id, ReasonFailed)
		}
		s.scanning = false
		s.scheduleLocked()
		s.emitScanLocked(err)
		s.mu.Unlock()
		close(finished)
		return
	}

	for _, active := range s.subscriptions {
		active.failures = 0
	}
	s.mu.Unlock()

	// Ranking and row measurement are expensive and identical for every
	// subscription of this snapshot, so they happen once, outside the mutex.
	plan := newFitPlan(snapshot, maxDetailBytes)
	s.deliver(plan, includeCommand)

	s.mu.Lock()
	s.scanning = false
	s.scheduleLocked()
	s.emitScanLocked(nil)
	s.mu.Unlock()
	close(finished)
}

func (s *Scheduler) emitScanLocked(err error) {
	event := Event{
		Type:          EventScan,
		Scans:         s.scans,
		Subscriptions: len(s.subscriptions),
		Duration:      s.lastScanDuration,
	}
	if err != nil {
		event.Error = err.Error()
	}
	s.emitLocked(event)
}

// deliver sends one snapshot to every subscription that is due. It re-reads
// the policy immediately before each send: a policy reduced after the scan
// must not leak arguments, and withdrawing process visibility ends the
// subscriptions instead of sending.
func (s *Scheduler) deliver(plan *fitPlan, scanIncludedCommand bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || len(s.subscriptions) == 0 {
		return
	}

	ids := make([]string, 0, len(s.subscriptions))
	for id := range s.subscriptions {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		policy := s.policyLocked()
		if !policy.Processes {
			s.withholdAllLocked()
			return
		}

		active, ok := s.subscriptions[id]
		if !ok {
			continue
		}
		now := s.clock()
		if !now.Before(active.expiresAt) {
			delete(s.subscriptions, id)
			s.sendEndedLocked(id, ReasonExpired)
			continue
		}
		if active.hasLastSent && now.Sub(active.lastSentAt) < active.interval {
			continue
		}

		capturedAt := s.now().UTC().Format(capturedAtLayout)
		// The scan only has commands when the policy allowed them before the
		// scan. If permission was granted mid-scan the next scan supplies
		// them rather than presenting absent data as if it had been requested.
		withCommands := policy.ProcessArguments && scanIncludedCommand
		result := DetailResult{
			Type:           "detail.result",
			SubscriptionID: id,
			CapturedAt:     capturedAt,
			Snapshot:       plan.fitted(id, capturedAt, withCommands),
		}
		if s.safeSendLocked(result) {
			active.lastSentAt = now
			active.hasLastSent = true
		} else {
			// Never mark an unsent result as delivered; the next scan retries.
			s.emitLocked(Event{Type: EventSendSkipped, SubscriptionID: id})
		}
	}
}

// withholdAllLocked ends every subscription because the policy no longer
// allows process visibility.
func (s *Scheduler) withholdAllLocked() {
	for id := range s.subscriptions {
		delete(s.subscriptions, id)
		s.sendEndedLocked(id, ReasonWithheld)
	}
}

func (s *Scheduler) policyLocked() Policy {
	if s.policy == nil {
		return Policy{}
	}
	// A broken policy source fails closed, as in the prototype.
	policy, ok := s.callPolicy()
	if !ok {
		return Policy{}
	}
	return Policy{Processes: policy.Processes, ProcessArguments: policy.ProcessArguments}
}

func (s *Scheduler) callPolicy() (policy Policy, ok bool) {
	defer func() {
		if recover() != nil {
			policy, ok = Policy{}, false
		}
	}()
	return s.policy(), true
}

func (s *Scheduler) sendEndedLocked(subscriptionID string, reason DetailEndedReason) {
	s.safeSendLocked(DetailEnded{Type: "detail.ended", SubscriptionID: subscriptionID, Reason: reason})
	s.emitLocked(Event{Type: EventEnded, SubscriptionID: subscriptionID, Reason: reason})
}

func (s *Scheduler) safeSendLocked(message DetailMessage) (sent bool) {
	if s.send == nil {
		return false
	}
	// A transport callback that panics must not take the scheduler down.
	defer func() {
		if recover() != nil {
			sent = false
		}
	}()
	return s.send(message)
}

func (s *Scheduler) emitLocked(event Event) {
	if s.onEvent == nil {
		return
	}
	// Observability must never affect scheduling or delivery.
	defer func() {
		_ = recover()
	}()
	s.onEvent(event)
}

func clampDetailInterval(milliseconds int64) time.Duration {
	if milliseconds <= 0 {
		milliseconds = minDetailIntervalMS
	}
	if milliseconds < minDetailIntervalMS {
		milliseconds = minDetailIntervalMS
	}
	if milliseconds > maxDetailIntervalMS {
		milliseconds = maxDetailIntervalMS
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func clampDetailLease(milliseconds int64) time.Duration {
	if milliseconds <= 0 {
		milliseconds = minDetailLeaseMS
	}
	if milliseconds < minDetailLeaseMS {
		milliseconds = minDetailLeaseMS
	}
	if milliseconds > maxDetailLeaseMS {
		milliseconds = maxDetailLeaseMS
	}
	return time.Duration(milliseconds) * time.Millisecond
}
