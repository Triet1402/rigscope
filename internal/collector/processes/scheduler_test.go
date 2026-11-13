package processes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- deterministic clock and timers ---------------------------------------

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	nextID int
	timers map[int]*fakeTimer
}

type fakeTimer struct {
	clock *fakeClock
	id    int
	at    time.Time
	fn    func()
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start, timers: make(map[int]*fakeTimer)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(delay time.Duration, fn func()) Timer {
	if delay < 0 {
		delay = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	timer := &fakeTimer{clock: c, id: c.nextID, at: c.now.Add(delay), fn: fn}
	c.timers[timer.id] = timer
	return timer
}

func (t *fakeTimer) Stop() {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	delete(t.clock.timers, t.id)
}

// Advance runs every timer due up to now+d, earliest first, and sets the clock
// to the target. Callbacks run without the clock lock, so a callback that
// blocks (a slow scan) does not block another advance.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()

	for {
		c.mu.Lock()
		var chosen *fakeTimer
		for _, timer := range c.timers {
			if timer.at.After(target) {
				continue
			}
			if chosen == nil || timer.at.Before(chosen.at) || (timer.at.Equal(chosen.at) && timer.id < chosen.id) {
				chosen = timer
			}
		}
		if chosen == nil {
			if c.now.Before(target) {
				c.now = target
			}
			c.mu.Unlock()
			return
		}
		delete(c.timers, chosen.id)
		if chosen.at.After(c.now) {
			c.now = chosen.at
		}
		c.mu.Unlock()
		chosen.fn()
	}
}

// --- scanner and harness ---------------------------------------------------

type stubScanner struct {
	mu     sync.Mutex
	calls  []ScanOptions
	scanFn func(context.Context, ScanOptions) (ProcessSnapshot, error)
}

func (s *stubScanner) Scan(ctx context.Context, options ScanOptions) (ProcessSnapshot, error) {
	s.mu.Lock()
	s.calls = append(s.calls, options)
	fn := s.scanFn
	s.mu.Unlock()
	return fn(ctx, options)
}

func (s *stubScanner) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubScanner) call(index int) ScanOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[index]
}

type harness struct {
	t         *testing.T
	clock     *fakeClock
	scheduler *Scheduler
	scanner   *stubScanner

	mu   sync.Mutex
	sent []DetailMessage
}

type harnessOptions struct {
	scanner          *stubScanner
	policy           func() Policy
	send             func(DetailMessage) bool
	minInterval      time.Duration
	maxSubscriptions int
}

func newHarness(t *testing.T, options harnessOptions) *harness {
	t.Helper()
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	scanner := options.scanner
	if scanner == nil {
		scanner = &stubScanner{scanFn: func(context.Context, ScanOptions) (ProcessSnapshot, error) {
			return snapshotOf(processRow(10)), nil
		}}
	}
	policy := options.policy
	if policy == nil {
		policy = func() Policy { return Policy{Processes: true} }
	}
	h := &harness{t: t, clock: clock, scanner: scanner}
	events := make(chan Event, 4096)
	scheduler, err := NewScheduler(SchedulerConfig{
		Scanner: scanner,
		Policy:  policy,
		Send: func(message DetailMessage) bool {
			if options.send != nil {
				if !options.send(message) {
					h.record(message)
					return false
				}
			}
			h.record(message)
			return true
		},
		MinInterval:      options.minInterval,
		MaxSubscriptions: options.maxSubscriptions,
		Clock:            clock.Now,
		Now:              clock.Now,
		NewTimer:         clock.NewTimer,
		OnEvent:          func(event Event) { events <- event },
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	h.scheduler = scheduler
	t.Cleanup(scheduler.Stop)
	return h
}

func (h *harness) record(message DetailMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent = append(h.sent, message)
}

func (h *harness) messages() []DetailMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]DetailMessage(nil), h.sent...)
}

func (h *harness) results() []DetailResult {
	var out []DetailResult
	for _, message := range h.messages() {
		if result, ok := message.(DetailResult); ok {
			out = append(out, result)
		}
	}
	return out
}

func (h *harness) ended() []DetailEnded {
	var out []DetailEnded
	for _, message := range h.messages() {
		if ended, ok := message.(DetailEnded); ok {
			out = append(out, ended)
		}
	}
	return out
}

func (h *harness) subscribe(id string, intervalMS, leaseMS int64) {
	h.scheduler.Subscribe(DetailSubscribe{
		Type:           "detail.subscribe",
		SubscriptionID: id,
		Kind:           "processes",
		IntervalMS:     intervalMS,
		LeaseMS:        leaseMS,
	})
}

func (h *harness) advance(milliseconds int64) {
	h.clock.Advance(time.Duration(milliseconds) * time.Millisecond)
}

func processRow(pid int, overrides ...func(*ProcessRow)) ProcessRow {
	row := ProcessRow{
		PID:            pid,
		PPID:           1,
		StartTicks:     int64(pid),
		Name:           fmt.Sprintf("proc-%d", pid),
		User:           ptr("alice"),
		State:          "S",
		CPUPercent:     floatPtr(1),
		MemoryRSSBytes: int64Ptr(1024),
		Threads:        intPtr(1),
	}
	for _, override := range overrides {
		override(&row)
	}
	return row
}

func snapshotOf(rows ...ProcessRow) ProcessSnapshot {
	return ProcessSnapshot{TotalProcesses: len(rows), Processes: rows}
}

func floatPtr(value float64) *float64 { return &value }
func int64Ptr(value int64) *int64     { return &value }
func intPtr(value int) *int           { return &value }

func resultIDs(results []DetailResult) []string {
	ids := make([]string, len(results))
	for index, result := range results {
		ids[index] = result.SubscriptionID
	}
	return ids
}

func endedReasons(ended []DetailEnded) []DetailEndedReason {
	reasons := make([]DetailEndedReason, len(ended))
	for index, message := range ended {
		reasons[index] = message.Reason
	}
	return reasons
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// --- tests -----------------------------------------------------------------

func TestSchedulerCoalescesSubscriptionsIntoOneScan(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.subscribe("a", 2000, 20_000)
	h.subscribe("b", 2000, 20_000)
	h.advance(0)

	if calls := h.scanner.callCount(); calls != 1 {
		t.Fatalf("scans = %d, want one shared scan", calls)
	}
	if got := sortedStrings(resultIDs(h.results())); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("result ids = %v, want [a b]", got)
	}
	stats := h.scheduler.Stats()
	if stats.Active != 2 || stats.Scans != 1 || stats.Scanning {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.LastScan.IsZero() || stats.LastScanDuration != 0 {
		t.Fatalf("last scan = %v / %v", stats.LastScan, stats.LastScanDuration)
	}

	first := h.results()[0]
	if first.Type != "detail.result" || first.CapturedAt != "2023-11-14T22:13:20.000Z" {
		t.Fatalf("result envelope = %+v", first)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["type"] != "detail.result" {
		t.Errorf("wire type = %v", decoded["type"])
	}
	snapshot, ok := decoded["snapshot"].(map[string]any)
	if !ok || snapshot["totalProcesses"] != float64(1) {
		t.Errorf("wire snapshot = %v", decoded["snapshot"])
	}

	h.advance(2000)
	if calls := h.scanner.callCount(); calls != 2 {
		t.Fatalf("scans = %d, want the next cadence tick", calls)
	}
	if results := h.results(); len(results) != 4 {
		t.Fatalf("results = %d, want 4", len(results))
	}
}

func TestSchedulerStripsCommandsWhenPolicyWithdrawsThem(t *testing.T) {
	secret := "run --token=secret"
	shared := snapshotOf(processRow(10, func(row *ProcessRow) { row.Command = &secret }))
	var policy = Policy{Processes: true, ProcessArguments: true}
	scanner := &stubScanner{scanFn: func(context.Context, ScanOptions) (ProcessSnapshot, error) {
		// The policy is reduced while the scan is in flight.
		policy.ProcessArguments = false
		return shared, nil
	}}
	h := newHarness(t, harnessOptions{
		scanner: scanner,
		policy:  func() Policy { return policy },
	})
	h.subscribe("with-args", 1000, 20_000)
	h.advance(0)

	if !scanner.call(0).IncludeCommand {
		t.Fatal("scan must read commands when the policy allowed them at scan time")
	}
	results := h.results()
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	row := results[0].Snapshot.Processes[0]
	if row.Command != nil {
		t.Fatalf("command leaked after policy withdrawal: %q", *row.Command)
	}
	encoded, err := json.Marshal(results[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "secret") {
		t.Fatal("the encoded result must not contain the command")
	}
	if shared.Processes[0].Command == nil || *shared.Processes[0].Command != secret {
		t.Fatal("the shared snapshot must not be mutated")
	}

	// If permission is granted mid-scan the next scan supplies commands; the
	// current result must not pretend it has them.
	var secondPolicy = Policy{Processes: true}
	secondScanner := &stubScanner{scanFn: func(context.Context, ScanOptions) (ProcessSnapshot, error) {
		secondPolicy.ProcessArguments = true
		return shared, nil
	}}
	second := newHarness(t, harnessOptions{
		scanner: secondScanner,
		policy:  func() Policy { return secondPolicy },
	})
	second.subscribe("late-args", 1000, 20_000)
	second.advance(0)
	if secondScanner.call(0).IncludeCommand {
		t.Fatal("scan must not read commands when the policy did not allow them")
	}
	if row := second.results()[0].Snapshot.Processes[0]; row.Command != nil {
		t.Fatal("a mid-scan permission change must not invent a command")
	}
}

func TestSchedulerWithheldPolicyEndsSubscriptionsWithoutScanning(t *testing.T) {
	var policy = Policy{Processes: false, ProcessArguments: true}
	h := newHarness(t, harnessOptions{policy: func() Policy { return policy }})
	h.subscribe("denied", 1000, 20_000)
	h.advance(0)

	if calls := h.scanner.callCount(); calls != 0 {
		t.Fatalf("scans = %d, a withheld policy must not trigger a scan", calls)
	}
	ended := h.ended()
	if len(ended) != 1 || ended[0].SubscriptionID != "denied" || ended[0].Reason != ReasonWithheld {
		t.Fatalf("ended = %+v", ended)
	}

	policy = Policy{Processes: true}
	h.subscribe("allowed", 1000, 20_000)
	h.subscribe("second", 1000, 20_000)
	h.advance(0)
	if calls := h.scanner.callCount(); calls != 1 || h.scheduler.Stats().Active != 2 {
		t.Fatalf("scans/active = %d/%d", calls, h.scheduler.Stats().Active)
	}

	// Reducing the policy ends every active subscription before another scan.
	policy = Policy{}
	h.advance(1000)
	if calls := h.scanner.callCount(); calls != 1 {
		t.Fatalf("scans = %d, policy must be checked before spending a scan", calls)
	}
	reasons := endedReasons(h.ended()[1:])
	if len(reasons) != 2 || reasons[0] != ReasonWithheld || reasons[1] != ReasonWithheld {
		t.Fatalf("withheld reasons = %v", reasons)
	}
	if h.scheduler.Stats().Active != 0 {
		t.Fatal("all subscriptions must be gone")
	}
}

func TestSchedulerRenewalExtendsLeaseAndExpiryStopsScans(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.subscribe("renew", 1000, 5000)
	h.advance(0)
	h.advance(4000)
	if len(h.ended()) != 0 {
		t.Fatal("nothing may expire before the lease ends")
	}

	h.subscribe("renew", 1000, 5000) // now = 4000, expiry becomes 9000
	h.advance(4000)                  // now = 8000
	if len(h.ended()) != 0 {
		t.Fatal("renewal must extend the lease")
	}

	h.advance(1500) // crosses expiry at 9000
	ended := h.ended()
	if len(ended) != 1 || ended[0].Reason != ReasonExpired {
		t.Fatalf("ended = %+v, want one expired", ended)
	}
	if active := h.scheduler.Stats().Active; active != 0 {
		t.Fatalf("active = %d", active)
	}

	scans := h.scheduler.Stats().Scans
	h.advance(10_000)
	if h.scheduler.Stats().Scans != scans {
		t.Fatal("no scans after the last lease")
	}
}

func TestSchedulerCancelEndsOneSubscriptionAndCancelAllStopsScans(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.subscribe("one", 1000, 20_000)
	h.subscribe("two", 1000, 20_000)
	h.advance(0)
	if active := h.scheduler.Stats().Active; active != 2 {
		t.Fatalf("active = %d", active)
	}

	h.scheduler.Cancel("one")
	if active := h.scheduler.Stats().Active; active != 1 {
		t.Fatalf("after cancel active = %d", active)
	}
	ended := h.ended()
	if len(ended) != 1 || ended[0].SubscriptionID != "one" || ended[0].Reason != ReasonCancelled {
		t.Fatalf("cancel ended = %+v", ended)
	}

	h.advance(1000)
	if calls := h.scanner.callCount(); calls != 2 {
		t.Fatalf("scans = %d, the remaining subscription keeps scanning", calls)
	}

	h.scheduler.CancelAll()
	if active := h.scheduler.Stats().Active; active != 0 {
		t.Fatalf("after cancel all active = %d", active)
	}
	if ended := h.ended(); len(ended) != 2 || ended[1].SubscriptionID != "two" || ended[1].Reason != ReasonCancelled {
		t.Fatalf("cancel all ended = %+v", ended)
	}
	scans := h.scheduler.Stats().Scans
	h.advance(10_000)
	if h.scheduler.Stats().Scans != scans {
		t.Fatal("no scans after CancelAll")
	}
}

func TestSchedulerSubscriptionBudgetReportsBusy(t *testing.T) {
	h := newHarness(t, harnessOptions{maxSubscriptions: 2})
	h.subscribe("a", 1000, 20_000)
	h.subscribe("b", 1000, 20_000)
	h.subscribe("c", 1000, 20_000)
	h.subscribe("d", 1000, 20_000)
	h.advance(0)

	var busy []string
	for _, message := range h.ended() {
		if message.Reason == ReasonBusy {
			busy = append(busy, message.SubscriptionID)
		}
	}
	if got := sortedStrings(busy); len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Fatalf("busy = %v, want [c d]", got)
	}
	if got := sortedStrings(resultIDs(h.results())); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("results = %v", got)
	}
	if active := h.scheduler.Stats().Active; active != 2 {
		t.Fatalf("active = %d", active)
	}
}

func TestSchedulerSlowScanNeverOverlapsAndReschedulesAfterItSettles(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	first := true
	scanner := &stubScanner{scanFn: func(context.Context, ScanOptions) (ProcessSnapshot, error) {
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()
		if isFirst {
			close(started)
			<-release
		}
		return snapshotOf(processRow(10)), nil
	}}
	h := newHarness(t, harnessOptions{scanner: scanner})
	h.subscribe("slow", 1000, 20_000)

	advanceDone := make(chan struct{})
	go func() {
		h.advance(0)
		close(advanceDone)
	}()
	<-started
	if !h.scheduler.Stats().Scanning {
		t.Fatal("scanning must be visible while the scan is in flight")
	}

	h.advance(5000)
	if calls := scanner.callCount(); calls != 1 {
		t.Fatalf("scans = %d, no overlapping scan while one is in flight", calls)
	}
	if !h.scheduler.Stats().Scanning {
		t.Fatal("still scanning")
	}

	close(release)
	<-advanceDone
	stats := h.scheduler.Stats()
	if stats.Scanning {
		t.Fatal("scan must be settled")
	}
	if len(h.results()) != 1 {
		t.Fatalf("results = %d", len(h.results()))
	}
	if stats.LastScanDuration != 5000*time.Millisecond {
		t.Fatalf("last scan duration = %v, want 5s", stats.LastScanDuration)
	}

	h.advance(1000)
	if calls := scanner.callCount(); calls != 2 {
		t.Fatalf("scans = %d, want a fresh cadence after the overrun", calls)
	}
	if h.scheduler.Stats().Scanning {
		t.Fatal("the second scan completes synchronously in this fake")
	}
}

func TestSchedulerRetriesFailuresAndEndsAfterThree(t *testing.T) {
	scanner := &stubScanner{scanFn: func(context.Context, ScanOptions) (ProcessSnapshot, error) {
		return ProcessSnapshot{}, fmt.Errorf("scan exploded")
	}}
	h := newHarness(t, harnessOptions{scanner: scanner})
	h.subscribe("failing", 1000, 20_000)
	h.advance(0)
	if calls := scanner.callCount(); calls != 1 {
		t.Fatalf("scans = %d", calls)
	}
	if len(h.ended()) != 0 || h.scheduler.Stats().Active != 1 {
		t.Fatal("the first failure only retries")
	}

	h.advance(1000)
	if calls := scanner.callCount(); calls != 2 || len(h.ended()) != 0 {
		t.Fatal("the second failure only retries")
	}

	h.advance(1000)
	if calls := scanner.callCount(); calls != 3 {
		t.Fatalf("scans = %d", calls)
	}
	ended := h.ended()
	if len(ended) != 1 || ended[0].Reason != ReasonFailed {
		t.Fatalf("ended = %+v", ended)
	}
	if h.scheduler.Stats().Active != 0 || h.scheduler.Stats().Scans != 3 {
		t.Fatalf("stats = %+v", h.scheduler.Stats())
	}

	h.advance(10_000)
	if calls := scanner.callCount(); calls != 3 {
		t.Fatal("no scans after the failed subscription is removed")
	}
}

func TestSchedulerDeliversAtEachSubscriptionsOwnInterval(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.subscribe("fast", 2000, 20_000)
	h.subscribe("slow", 5000, 20_000)
	h.advance(6000)

	if calls := h.scanner.callCount(); calls != 4 {
		t.Fatalf("scans = %d, want 4 (0, 2000, 4000, 6000)", calls)
	}
	perSubscription := make(map[string]int)
	for _, result := range h.results() {
		perSubscription[result.SubscriptionID]++
	}
	if perSubscription["fast"] != 4 {
		t.Errorf("fast results = %d, want 4", perSubscription["fast"])
	}
	if perSubscription["slow"] != 2 {
		t.Errorf("slow results = %d, want 2", perSubscription["slow"])
	}
}

func TestSchedulerUnsentResultRetriesOnTheNextScan(t *testing.T) {
	var allow bool
	h := newHarness(t, harnessOptions{send: func(message DetailMessage) bool {
		if _, ok := message.(DetailResult); !ok {
			return true
		}
		return allow
	}})
	h.subscribe("retry", 1000, 20_000)
	h.advance(0)
	if results := h.results(); len(results) != 1 {
		t.Fatalf("attempts = %d", len(results))
	}
	if h.scheduler.Stats().Scans != 1 || h.scheduler.Stats().Active != 1 {
		t.Fatalf("stats = %+v", h.scheduler.Stats())
	}

	allow = true
	h.advance(1000)
	if results := h.results(); len(results) != 2 {
		t.Fatalf("attempts = %d, want the retry to be sent", len(results))
	}
}

func TestSchedulerDetailResultFitsTheByteLimit(t *testing.T) {
	command := strings.Repeat("x", 4000)
	rows := make([]ProcessRow, 400)
	for index := range rows {
		pid := 100 + index
		rows[index] = processRow(pid, func(row *ProcessRow) {
			row.Command = &command
			row.CPUPercent = floatPtr(float64(400 - index))
		})
	}
	shared := snapshotOf(rows...)
	scanner := &stubScanner{scanFn: func(context.Context, ScanOptions) (ProcessSnapshot, error) {
		return shared, nil
	}}
	h := newHarness(t, harnessOptions{
		scanner: scanner,
		policy:  func() Policy { return Policy{Processes: true, ProcessArguments: true} },
	})
	h.subscribe("big", 1000, 20_000)
	h.advance(0)

	results := h.results()
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	encoded, err := json.Marshal(results[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(encoded) > maxDetailBytes {
		t.Fatalf("encoded result = %d bytes, limit %d", len(encoded), maxDetailBytes)
	}
	snapshot := results[0].Snapshot
	if !snapshot.Truncated {
		t.Fatal("a size-truncated result must say so")
	}
	if snapshot.TotalProcesses != 400 {
		t.Errorf("totalProcesses = %d, want the full count", snapshot.TotalProcesses)
	}
	if len(snapshot.Processes) >= len(rows) {
		t.Errorf("rows = %d, want fewer than %d", len(snapshot.Processes), len(rows))
	}
	if len(snapshot.Processes) == 0 {
		t.Error("the limit is large enough for at least one row")
	}
	if snapshot.Processes[0].PID != 100 {
		t.Errorf("first row pid = %d, want the highest CPU row", snapshot.Processes[0].PID)
	}
}

func TestFitSnapshotToBytesKeepsRankingAndFullCount(t *testing.T) {
	rows := []ProcessRow{
		processRow(1, func(row *ProcessRow) { row.CPUPercent = floatPtr(5); row.MemoryRSSBytes = int64Ptr(10) }),
		processRow(2, func(row *ProcessRow) { row.CPUPercent = floatPtr(50); row.MemoryRSSBytes = int64Ptr(20) }),
		processRow(3, func(row *ProcessRow) { row.CPUPercent = nil; row.MemoryRSSBytes = int64Ptr(30) }),
	}
	snapshot := snapshotOf(rows...)
	full := detailSize("sub", "2023-11-14T22:13:20.000Z", snapshot)
	fitted := fitSnapshotToBytes("sub", "2023-11-14T22:13:20.000Z", snapshot, full)
	if fitted.Truncated || len(fitted.Processes) != 3 {
		t.Fatalf("a snapshot at the limit must be unchanged: %+v", fitted)
	}

	fitted = fitSnapshotToBytes("sub", "2023-11-14T22:13:20.000Z", snapshot, full-20)
	if !fitted.Truncated {
		t.Fatal("a dropped row must set truncated")
	}
	if fitted.TotalProcesses != 3 || fitted.Inaccessible != snapshot.Inaccessible {
		t.Errorf("full counts changed: %+v", fitted)
	}
	if len(fitted.Processes) >= 3 {
		t.Fatalf("rows = %d, want a truncated prefix", len(fitted.Processes))
	}
	if len(fitted.Processes) > 0 && fitted.Processes[0].PID != 2 {
		t.Errorf("first kept row = %d, want pid 2 (highest CPU)", fitted.Processes[0].PID)
	}
	if size := detailSize("sub", "2023-11-14T22:13:20.000Z", fitted); size > full-20 {
		t.Errorf("fitted size = %d, limit %d", size, full-20)
	}
}

func TestSchedulerClampsSubscriptionBounds(t *testing.T) {
	if got := clampDetailInterval(1); got != minDetailIntervalMS*time.Millisecond {
		t.Errorf("interval below minimum = %v", got)
	}
	if got := clampDetailInterval(10_000_000); got != maxDetailIntervalMS*time.Millisecond {
		t.Errorf("interval above maximum = %v", got)
	}
	if got := clampDetailLease(1); got != minDetailLeaseMS*time.Millisecond {
		t.Errorf("lease below minimum = %v", got)
	}
	if got := clampDetailLease(10_000_000); got != maxDetailLeaseMS*time.Millisecond {
		t.Errorf("lease above maximum = %v", got)
	}

	// A too-fast interval is honored at the protocol floor: no scan after
	// 999 ms, one after a full second.
	h := newHarness(t, harnessOptions{})
	h.subscribe("fast", 1, 20_000)
	h.advance(0)
	h.advance(999)
	if calls := h.scanner.callCount(); calls != 1 {
		t.Fatalf("scans after 999ms = %d, want 1", calls)
	}
	h.advance(1)
	if calls := h.scanner.callCount(); calls != 2 {
		t.Fatalf("scans after 1s = %d, want 2", calls)
	}
}

func TestSchedulerRefreshesPolicyBeforeEverySend(t *testing.T) {
	secret := "run --token=secret"
	shared := snapshotOf(processRow(10, func(row *ProcessRow) { row.Command = &secret }))
	scanner := &stubScanner{scanFn: func(context.Context, ScanOptions) (ProcessSnapshot, error) {
		return shared, nil
	}}
	var policy = Policy{Processes: true, ProcessArguments: true}
	h := newHarness(t, harnessOptions{
		scanner: scanner,
		policy:  func() Policy { return policy },
		send: func(message DetailMessage) bool {
			// The policy is narrowed between the two sends of one delivery.
			if result, ok := message.(DetailResult); ok && result.SubscriptionID == "a" {
				policy.ProcessArguments = false
			}
			return true
		},
	})
	h.subscribe("a", 1000, 20_000)
	h.subscribe("b", 1000, 20_000)
	h.advance(0)

	results := h.results()
	if len(results) != 2 {
		t.Fatalf("results = %d, want one per subscription", len(results))
	}
	for _, result := range results {
		row := result.Snapshot.Processes[0]
		switch result.SubscriptionID {
		case "a":
			if row.Command == nil || *row.Command != secret {
				t.Errorf("a = %v, want the arguments it was still allowed", row.Command)
			}
		case "b":
			if row.Command != nil {
				t.Fatalf("b received arguments after the policy withdrew them: %q", *row.Command)
			}
		}
	}
	if shared.Processes[0].Command == nil {
		t.Fatal("the shared snapshot must not be mutated")
	}
}

func TestSchedulerEndsEverythingWhenProcessDetailIsWithdrawnMidDelivery(t *testing.T) {
	scanner := &stubScanner{scanFn: func(context.Context, ScanOptions) (ProcessSnapshot, error) {
		return snapshotOf(processRow(10)), nil
	}}
	var policy = Policy{Processes: true}
	h := newHarness(t, harnessOptions{
		scanner: scanner,
		policy:  func() Policy { return policy },
		send: func(message DetailMessage) bool {
			if result, ok := message.(DetailResult); ok && result.SubscriptionID == "a" {
				policy.Processes = false
			}
			return true
		},
	})
	h.subscribe("a", 1000, 20_000)
	h.subscribe("b", 1000, 20_000)
	h.advance(0)

	results := h.results()
	if len(results) != 1 || results[0].SubscriptionID != "a" {
		t.Fatalf("results = %v, want only the send that preceded the withdrawal", resultIDs(results))
	}
	ended := h.ended()
	if len(ended) != 2 {
		t.Fatalf("ended = %+v, want both subscriptions ended", ended)
	}
	for _, message := range ended {
		if message.Reason != ReasonWithheld {
			t.Errorf("ended %s reason = %s, want withheld", message.SubscriptionID, message.Reason)
		}
	}
	if active := h.scheduler.Stats().Active; active != 0 {
		t.Fatalf("active = %d", active)
	}
	scans := h.scheduler.Stats().Scans
	h.advance(5000)
	if h.scheduler.Stats().Scans != scans {
		t.Fatal("no scan may follow a withdrawn policy")
	}
}

func TestSchedulerExpiresLeasesWhileAScanIsInFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	scanner := &stubScanner{scanFn: func(context.Context, ScanOptions) (ProcessSnapshot, error) {
		close(started)
		<-release
		return snapshotOf(processRow(10)), nil
	}}
	h := newHarness(t, harnessOptions{scanner: scanner})
	h.subscribe("short", 1000, 5000)

	advanceDone := make(chan struct{})
	go func() {
		h.advance(0)
		close(advanceDone)
	}()
	<-started
	if !h.scheduler.Stats().Scanning {
		t.Fatal("the scan must be in flight")
	}

	// The 5 s lease is due while the scan blocks in /proc.
	h.advance(6000)
	ended := h.ended()
	if len(ended) != 1 || ended[0].SubscriptionID != "short" || ended[0].Reason != ReasonExpired {
		t.Fatalf("ended = %+v, want one expired subscription during the scan", ended)
	}
	if active := h.scheduler.Stats().Active; active != 0 {
		t.Fatalf("active = %d, the lease must not outlive its expiry", active)
	}

	close(release)
	<-advanceDone
	if results := h.results(); len(results) != 0 {
		t.Fatalf("results = %d, the late result for an expired subscription must be suppressed", len(results))
	}
	if h.scheduler.Stats().Scanning {
		t.Fatal("the scan must be settled")
	}
}

func TestSchedulerStopCancelsAndWaitsForAnInFlightScan(t *testing.T) {
	started := make(chan struct{})
	scanResult := make(chan error, 1)
	scanner := &stubScanner{scanFn: func(ctx context.Context, _ ScanOptions) (ProcessSnapshot, error) {
		close(started)
		<-ctx.Done()
		scanResult <- ctx.Err()
		return ProcessSnapshot{}, ctx.Err()
	}}
	h := newHarness(t, harnessOptions{scanner: scanner})
	h.subscribe("stuck", 1000, 20_000)

	advanceDone := make(chan struct{})
	go func() {
		h.advance(0)
		close(advanceDone)
	}()
	<-started

	stopped := make(chan struct{})
	go func() {
		h.scheduler.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return; it must cancel and wait for the scan")
	}
	if err := <-scanResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error = %v, want context.Canceled", err)
	}
	<-advanceDone
}

func TestFitPlanMatchesTheExactEncodedSize(t *testing.T) {
	command := strings.Repeat("x", 300)
	rows := make([]ProcessRow, 40)
	for index := range rows {
		pid := 100 + index
		rows[index] = processRow(pid, func(row *ProcessRow) {
			row.Command = &command
			row.CPUPercent = floatPtr(float64(index))
			row.MemoryRSSBytes = int64Ptr(int64(index) * 4096)
		})
	}
	snapshot := snapshotOf(rows...)
	capturedAt := "2023-11-14T22:13:20.000Z"

	plan := newFitPlan(snapshot, maxDetailBytes)
	full := plan.fitted("sub", capturedAt, true)
	if got, want := detailSize("sub", capturedAt, full), detailSize("sub", capturedAt, snapshot); got != want {
		t.Fatalf("an unconstrained plan changed the result: %d bytes, want %d", got, want)
	}
	if full.Truncated || len(full.Processes) != len(rows) {
		t.Fatalf("an unconstrained plan must keep every row: %+v", full.Truncated)
	}

	for _, withCommands := range []bool{true, false} {
		variantRows, cumulative := plan.ranked, plan.rankedSizes
		if !withCommands && plan.stripped != nil {
			variantRows, cumulative = plan.stripped, plan.strippedSizes
		}
		// The size model must agree byte for byte with encoding the result.
		variantSnapshot := plan.snapshot(variantRows, snapshot.Truncated)
		modelled := plan.envelopeSize("sub-with-a-longer-id", capturedAt, snapshot.Truncated) + cumulative[len(cumulative)-1]
		if actual := detailSize("sub-with-a-longer-id", capturedAt, variantSnapshot); modelled != actual {
			t.Fatalf("withCommands %v: modelled %d bytes, encoded %d", withCommands, modelled, actual)
		}
		for _, limit := range []int{modelled / 4, modelled / 2, modelled - 10} {
			plan := newFitPlan(snapshot, limit)
			fitted := plan.fitted("sub-with-a-longer-id", capturedAt, withCommands)
			if size := detailSize("sub-with-a-longer-id", capturedAt, fitted); size > limit {
				t.Fatalf("limit %d, withCommands %v: encoded %d bytes", limit, withCommands, size)
			}
			if !fitted.Truncated {
				t.Fatalf("limit %d, withCommands %v: nothing was dropped", limit, withCommands)
			}
			if fitted.TotalProcesses != len(rows) || fitted.Inaccessible != snapshot.Inaccessible {
				t.Errorf("full counts changed: %+v", fitted)
			}
			if len(fitted.Processes) == 0 {
				t.Fatalf("limit %d: not even one row fits", limit)
			}

			// The chosen prefix is maximal: the next ranked row would exceed
			// the limit once the envelope accounts for it.
			next := len(fitted.Processes) + 1
			candidate := fitted
			candidate.Processes = variantRows[:next]
			if size := detailSize("sub-with-a-longer-id", capturedAt, candidate); size <= limit {
				t.Fatalf("limit %d, withCommands %v: prefix %d fits at %d bytes, %d rows were dropped needlessly",
					limit, withCommands, next, size, len(rows)-next)
			}
		}
	}
}

func TestFitPlanNeverSelectsAnUnencodableRow(t *testing.T) {
	rows := []ProcessRow{
		processRow(1, func(row *ProcessRow) { row.CPUPercent = floatPtr(0) }),
		processRow(2, func(row *ProcessRow) { row.CPUPercent = floatPtr(50) }),
	}
	// encoding/json refuses NaN, so this row can never be sent.
	nan := math.NaN()
	rows[0].CPUPercent = &nan
	snapshot := snapshotOf(rows...)
	fitted := newFitPlan(snapshot, maxDetailBytes).fitted("sub", "2023-11-14T22:13:20.000Z", true)
	for _, row := range fitted.Processes {
		if row.PID == 1 {
			t.Fatal("an unencodable row must not be selected")
		}
	}
	if !fitted.Truncated {
		t.Fatal("dropping an unencodable row must set truncated")
	}
	if _, err := json.Marshal(fitted); err != nil {
		t.Fatalf("the fitted snapshot must still encode: %v", err)
	}
}

// BenchmarkFitPlan6000LongCommands measures the shared fit work for a large,
// command-heavy snapshot: one plan, two subscriptions and both variants.
func BenchmarkFitPlan6000LongCommands(b *testing.B) {
	builder := strings.Builder{}
	for index := 0; index < 4000; index++ {
		builder.WriteByte('x')
	}
	command := builder.String()
	rows := make([]ProcessRow, 6000)
	for index := range rows {
		rows[index] = processRow(100+index, func(row *ProcessRow) {
			row.Command = &command
			row.CPUPercent = floatPtr(float64(index % 100))
			row.MemoryRSSBytes = int64Ptr(int64(index) * 4096)
		})
	}
	snapshot := snapshotOf(rows...)
	capturedAt := "2023-11-14T22:13:20.000Z"

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		plan := newFitPlan(snapshot, maxDetailBytes)
		plan.fitted("sub-a", capturedAt, true)
		plan.fitted("sub-b", capturedAt, true)
		plan.fitted("sub-a", capturedAt, false)
		plan.fitted("sub-b", capturedAt, false)
	}
}

// TestSchedulerSharesOneFittedSnapshotPerVariant proves the expensive fitting
// work and its result are shared: two subscriptions of one scan get prefixes of
// the same ranked slice rather than two independently fitted copies.
func TestSchedulerSharesOneFittedSnapshotPerVariant(t *testing.T) {
	command := strings.Repeat("y", 4000)
	rows := make([]ProcessRow, 400)
	for index := range rows {
		rows[index] = processRow(100+index, func(row *ProcessRow) {
			row.Command = &command
			row.CPUPercent = floatPtr(float64(400 - index))
		})
	}
	shared := snapshotOf(rows...)
	scanner := &stubScanner{scanFn: func(context.Context, ScanOptions) (ProcessSnapshot, error) {
		return shared, nil
	}}
	h := newHarness(t, harnessOptions{
		scanner: scanner,
		policy:  func() Policy { return Policy{Processes: true, ProcessArguments: true} },
	})
	h.subscribe("a", 1000, 20_000)
	h.subscribe("a-much-longer-subscription-id", 1000, 20_000)
	h.advance(0)

	results := h.results()
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	for _, result := range results {
		if !result.Snapshot.Truncated || len(result.Snapshot.Processes) == 0 {
			t.Fatalf("%s: snapshot was not fitted", result.SubscriptionID)
		}
		if got := result.Snapshot.Processes[0].PID; got != 100 {
			t.Errorf("%s first row pid = %d, want the highest CPU row", result.SubscriptionID, got)
		}
	}
	if &results[0].Snapshot.Processes[0] != &results[1].Snapshot.Processes[0] {
		t.Fatal("subscriptions of one snapshot must share the fitted rows")
	}
}

func TestSchedulerRejectsMissingScanner(t *testing.T) {
	if _, err := NewScheduler(SchedulerConfig{}); err == nil {
		t.Fatal("a missing scanner must be an error")
	}
}
