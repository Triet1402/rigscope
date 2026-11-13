package cli

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/alerts"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
)

func overflowRuntime(engine *alerts.Engine, logger *slog.Logger) *alertRuntime {
	return &alertRuntime{
		engine:       engine,
		logger:       logger,
		observe:      make(chan alertObservation, 1),
		stop:         make(chan struct{}),
		workerDone:   make(chan struct{}),
		notifyCtx:    context.Background(),
		notifyCancel: func() {},
		broken:       make(map[string]continuityBreak),
		brokenReady:  make(chan struct{}, 1),
	}
}

func overflowRecord(sequence int64) protocol.MetricsRecord {
	return protocol.MetricsRecord{
		RecordEnvelope: protocol.RecordEnvelope{
			CollectorID: "collector-alert",
			RunID:       "run-alert",
			Sequence:    sequence,
		},
		Payload: protocol.MetricsPayload{
			CPU: protocol.CpuAvailable{UsagePercent: 99},
		},
	}
}

// cliAlertAuthority creates the administrator session alert mutations re-check
// inside their write transaction.
func cliAlertAuthority(t *testing.T, db *storage.Database) storage.SessionAuthority {
	t.Helper()
	ctx := context.Background()
	users := storage.NewUsers(db)
	const username = "cli-alerts-admin"
	if err := users.Create(ctx, "test", storage.User{
		Username: username, PasswordHash: "test:cli-password",
		Scope: storage.AllMachines(), Admin: true,
	}); err != nil {
		t.Fatalf("create cli alert admin: %v", err)
	}
	session := storage.Session{
		TokenHash: "cli-alerts-session",
		Username:  username,
		CSRFToken: "cli-alerts-csrf",
		CreatedAt: time.Unix(0, 0),
		ExpiresAt: time.Now().Add(100 * 365 * 24 * time.Hour),
	}
	if err := storage.NewSessions(db).Create(ctx, session); err != nil {
		t.Fatalf("create cli alert session: %v", err)
	}
	return storage.SessionAuthority{Actor: username, TokenHash: session.TokenHash}
}

func TestAlertQueueOverflowResetsContinuityAndCountsDiagnostics(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine, err := alerts.New(alerts.Options{Database: db, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		engine.Stop()
		_ = db.Close()
	})
	authority := cliAlertAuthority(t, db)
	threshold := 1.0
	rule, err := engine.CreateRule(ctx, authority, alerts.RuleInput{
		Name: "Overflow", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: &threshold, DurationMs: 60_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	receivedAt := time.Now()
	engine.ObserveAt("m1", overflowRecord(1), alerts.Live, receivedAt)
	if got, err := engine.GetRule(ctx, rule.ID); err != nil || got.State != api.AlertRuleStatePending {
		t.Fatalf("rule before overflow = %+v, %v", got, err)
	}

	runtime := overflowRuntime(engine, logger)
	runtime.Observe("m1", overflowRecord(2), receivedAt)
	runtime.Observe("m1", overflowRecord(3), receivedAt)
	if got := runtime.DroppedObservations(); got != 1 {
		t.Fatalf("dropped observations = %d, want 1", got)
	}
	runtime.applyContinuityBreaks()
	if got := runtime.ContinuityResets(); got != 1 {
		t.Fatalf("continuity resets = %d, want 1", got)
	}
	diagnostics := runtime.Diagnostics()
	if diagnostics.DroppedObservations != 1 || diagnostics.ContinuityResets != 1 {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
	if got, err := engine.GetRule(ctx, rule.ID); err != nil || got.State != api.AlertRuleStateOK || got.PendingSince != nil {
		t.Fatalf("rule after overflow = %+v, %v; want a reset pending duration", got, err)
	}

	// A repeat drop for the same machine coalesces into one reset because the
	// machine's continuity is already broken.
	runtime.brokenReady = make(chan struct{}, 1)
	runtime.Observe("m1", overflowRecord(4), receivedAt)
	runtime.Observe("m1", overflowRecord(5), receivedAt)
	runtime.applyContinuityBreaks()
	if got := runtime.ContinuityResets(); got != 2 {
		t.Fatalf("continuity resets after a second overflow = %d, want 2", got)
	}
	if got := runtime.DroppedObservations(); got != 3 {
		t.Fatalf("dropped observations after a second overflow = %d, want 3", got)
	}
}

// alertTestClock is safe to read from the engine's background delivery
// goroutine while a test advances it.
type alertTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *alertTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *alertTestClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}

func TestAlertTickDoesNotClampQueuedReceiptToTickTime(t *testing.T) {
	ctx := context.Background()
	clock := &alertTestClock{now: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	db, err := storage.Open(ctx, ":memory:", storage.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine, err := alerts.New(alerts.Options{Database: db, Now: clock.Now, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		engine.Stop()
		_ = db.Close()
	})
	authority := cliAlertAuthority(t, db)
	threshold := 1.0
	rule, err := engine.CreateRule(ctx, authority, alerts.RuleInput{
		Name: "Queued receipt", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: &threshold, DurationMs: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := clock.Now()
	engine.ObserveAt("m1", overflowRecord(1), alerts.Live, start)
	if got, err := engine.GetRule(ctx, rule.ID); err != nil || got.State != api.AlertRuleStatePending {
		t.Fatalf("rule before tick = %+v, %v", got, err)
	}

	// The ticker fires 60 s later while the sample received at start+2 s is
	// still queued. It must be evaluated at its receipt time, not the tick
	// horizon.
	clock.Advance(time.Minute)
	runtime := overflowRuntime(engine, logger)
	runtime.observe = make(chan alertObservation, 4)
	runtime.observe <- alertObservation{machineID: "m1", record: overflowRecord(2), receivedAt: start.Add(2 * time.Second)}
	runtime.tick()
	if got, err := engine.GetRule(ctx, rule.ID); err != nil || got.State != api.AlertRuleStatePending {
		t.Fatalf("queued receipt was clamped to the tick: %+v, %v", got, err)
	}
	if incidents, _ := engine.ListIncidents(ctx, alerts.AllMachines(), 200); len(incidents) != 0 {
		t.Fatalf("tick manufactured an incident: %+v", incidents)
	}
	if got := runtime.Diagnostics().LastTickAt; got.IsZero() {
		t.Fatal("last tick time was not recorded")
	}
}

func TestFailedContinuityResetIsRetriedAndBlocksPreGapDuration(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine, err := alerts.New(alerts.Options{Database: db, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		engine.Stop()
		_ = db.Close()
	})
	authority := cliAlertAuthority(t, db)
	threshold := 1.0
	rule, err := engine.CreateRule(ctx, authority, alerts.RuleInput{
		Name: "Continuity retry", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: &threshold, DurationMs: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(time.Second)
	engine.ObserveAt("m1", overflowRecord(1), alerts.Live, start)
	if got, err := engine.GetRule(ctx, rule.ID); err != nil || got.State != api.AlertRuleStatePending {
		t.Fatalf("rule before overflow = %+v, %v", got, err)
	}

	runtime := overflowRuntime(engine, logger)
	runtime.Observe("m1", overflowRecord(2), start.Add(12*time.Second))
	runtime.Observe("m1", overflowRecord(3), start.Add(12*time.Second))
	if got := runtime.DroppedObservations(); got != 1 {
		t.Fatalf("dropped observations = %d, want 1", got)
	}

	// Fail only the pending -> ok reset write, so observations remain durable
	// while the break is unresolved.
	if err := db.Write(ctx, func(tx *storage.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER fail_alert_reset BEFORE UPDATE ON alert_rules
			WHEN OLD.state = 'pending' AND NEW.state = 'ok'
			BEGIN SELECT RAISE(ABORT, 'injected continuity reset failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runtime.applyContinuityBreaks()

	diagnostics := runtime.Diagnostics()
	if diagnostics.DroppedObservations != 1 || diagnostics.ContinuityResets != 0 || diagnostics.ContinuityResetFailures != 1 {
		t.Fatalf("diagnostics after a failed reset = %+v", diagnostics)
	}
	if len(diagnostics.UnresolvedBreaks) != 1 || diagnostics.UnresolvedBreaks[0] != "m1" {
		t.Fatalf("unresolved breaks = %v, want [m1]", diagnostics.UnresolvedBreaks)
	}

	// While unresolved, a high sample at the gap boundary must start a fresh
	// pending interval instead of accumulating the pre-gap one.
	engine.ObserveAt("m1", overflowRecord(4), alerts.Live, start.Add(12*time.Second))
	if got, err := engine.GetRule(ctx, rule.ID); err != nil || got.State != api.AlertRuleStatePending {
		t.Fatalf("rule during an unresolved break = %+v, %v; want pending", got, err)
	}
	if incidents, _ := engine.ListIncidents(ctx, alerts.AllMachines(), 200); len(incidents) != 0 {
		t.Fatalf("unresolved break allowed an incident: %+v", incidents)
	}

	// Reconciliation retried after the storage failure commits the break.
	if err := db.Write(ctx, func(tx *storage.Tx) error {
		_, err := tx.Exec("DROP TRIGGER fail_alert_reset")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runtime.applyContinuityBreaks()
	diagnostics = runtime.Diagnostics()
	if diagnostics.ContinuityResets != 1 || len(diagnostics.UnresolvedBreaks) != 0 {
		t.Fatalf("diagnostics after retry = %+v", diagnostics)
	}
	if got, err := engine.GetRule(ctx, rule.ID); err != nil || got.State != api.AlertRuleStateOK {
		t.Fatalf("rule after retried reset = %+v, %v; want ok", got, err)
	}

	engine.ObserveAt("m1", overflowRecord(5), alerts.Live, start.Add(12*time.Second))
	if got, err := engine.GetRule(ctx, rule.ID); err != nil || got.State != api.AlertRuleStatePending {
		t.Fatalf("rule after post-gap evidence = %+v, %v; want pending", got, err)
	}
	if incidents, _ := engine.ListIncidents(ctx, alerts.AllMachines(), 200); len(incidents) != 0 {
		t.Fatalf("post-gap evidence fired across the dropped sample: %+v", incidents)
	}
}

func TestShutdownCountsAbandonedObservations(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine, err := alerts.New(alerts.Options{Database: db, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		engine.Stop()
		_ = db.Close()
	})
	runtime := overflowRuntime(engine, logger)
	runtime.observe = make(chan alertObservation, 2)
	runtime.observe <- alertObservation{machineID: "m1", record: overflowRecord(1), receivedAt: time.Now()}
	runtime.observe <- alertObservation{machineID: "m1", record: overflowRecord(2), receivedAt: time.Now()}
	close(runtime.workerDone)
	runtime.Stop()

	if got := runtime.ObservationsAbandoned(); got != 2 {
		t.Fatalf("abandoned observations = %d, want 2", got)
	}
	diagnostics := runtime.Diagnostics()
	if diagnostics.QueueDepth != 0 || diagnostics.QueueCapacity != 2 || diagnostics.ObservationsAbandoned != 2 {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
}

func TestShutdownWaitsForObservationPastStopCheck(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine, err := alerts.New(alerts.Options{Database: db, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		engine.Stop()
		_ = db.Close()
	})
	runtime := overflowRuntime(engine, logger)
	runtime.observe = make(chan alertObservation, 2)
	// No worker: the test owns the queue state.
	close(runtime.workerDone)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	runtime.testAfterAdmissionCheck = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	observed := make(chan struct{})
	go func() {
		runtime.Observe("m1", overflowRecord(1), time.Now())
		close(observed)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("observation never passed the stopped check")
	}

	stopped := make(chan struct{})
	go func() {
		runtime.Stop()
		close(stopped)
	}()
	// Stop must not drain and return while an admission that already passed the
	// stopped check still owns a send.
	select {
	case <-stopped:
		t.Fatal("Stop completed before the in-flight observation was admitted")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-observed:
	case <-time.After(2 * time.Second):
		t.Fatal("observation did not finish")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not finish")
	}
	if got := runtime.ObservationsAbandoned(); got != 1 {
		t.Fatalf("abandoned observations = %d, want 1", got)
	}
}

func TestShutdownAccountsForEveryRacingObservation(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine, err := alerts.New(alerts.Options{Database: db, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		engine.Stop()
		_ = db.Close()
	})
	runtime := overflowRuntime(engine, logger)
	runtime.observe = make(chan alertObservation, 8)
	// No worker: every admitted observation is either still queued at shutdown
	// and counted abandoned, or refused because the queue was full.
	close(runtime.workerDone)

	const (
		producers = 8
		each      = 256
	)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for producer := 0; producer < producers; producer++ {
		wait.Add(1)
		go func(producer int) {
			defer wait.Done()
			<-start
			for i := 0; i < each; i++ {
				runtime.Observe("m1", overflowRecord(int64(producer*each+i)), time.Now())
			}
		}(producer)
	}
	close(start)
	runtime.Stop()
	wait.Wait()

	produced := uint64(producers * each)
	accounted := runtime.DroppedObservations() + runtime.ObservationsAbandoned()
	if accounted != produced {
		t.Fatalf("accounted observations = %d, want %d (dropped=%d abandoned=%d)",
			accounted, produced, runtime.DroppedObservations(), runtime.ObservationsAbandoned())
	}
}

func TestAlertDrainSnapshotsQueueReceiptCutoff(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine, err := alerts.New(alerts.Options{Database: db, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		engine.Stop()
		_ = db.Close()
	})
	runtime := overflowRuntime(engine, logger)
	runtime.observe = make(chan alertObservation, 4)
	start := time.Now().UTC().Truncate(time.Millisecond)
	runtime.observe <- alertObservation{machineID: "m1", record: overflowRecord(1), receivedAt: start.Add(2 * time.Second)}
	runtime.observe <- alertObservation{machineID: "m1", record: overflowRecord(2), receivedAt: start.Add(5 * time.Second)}
	cutoff := runtime.drainObservations()
	if !cutoff.Equal(start.Add(5 * time.Second)) {
		t.Fatalf("cutoff = %s, want the queued tail receipt", cutoff)
	}
	if depth := len(runtime.observe); depth != 0 {
		t.Fatalf("queue depth after drain = %d, want 0", depth)
	}
}
