package alerts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
)

func TestThresholdDurationOpensOneIncidentAndRecovers(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("CPU high", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(80), 5_000)
	high := cpuPayload(90)
	low := cpuPayload(20)

	h.observe("m1", high)
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 0 {
		t.Fatalf("incident before duration elapsed: %+v", incidents)
	}
	if rule := mustRule(t, h, ruleID); rule.State != api.AlertRuleStatePending {
		t.Fatalf("state = %q, want pending", rule.State)
	}

	h.clock.Advance(4_000)
	h.observe("m1", high)
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 0 {
		t.Fatalf("incident before duration elapsed: %+v", incidents)
	}
	h.clock.Advance(1_000)
	h.observe("m1", high)
	h.process()
	incidents := mustIncidents(t, h, AllMachines())
	if len(incidents) != 1 || incidents[0].Status != api.AlertIncidentOpen {
		t.Fatalf("incidents = %+v", incidents)
	}
	if rule := mustRule(t, h, ruleID); rule.State != api.AlertRuleStateFiring {
		t.Fatalf("state = %q, want firing", rule.State)
	}
	if h.sender.CallCount() != 1 || !strings.Contains(string(h.sender.Calls()[0].Body), `"event":"fired"`) {
		t.Fatalf("deliveries = %+v", h.sender.Calls())
	}
	summary := mustSummary(t, h)
	if summary.Delivered != 1 {
		t.Fatalf("delivered = %d", summary.Delivered)
	}

	h.clock.Advance(1_000)
	h.observe("m1", high)
	h.process()
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 1 {
		t.Fatalf("duplicate open incident: %+v", incidents)
	}
	if h.sender.CallCount() != 1 {
		t.Fatalf("duplicate fired job: %+v", h.sender.Calls())
	}

	h.clock.Advance(1_000)
	h.observe("m1", low)
	h.process()
	incidents = mustIncidents(t, h, AllMachines())
	if len(incidents) != 1 || incidents[0].Status != api.AlertIncidentRecovered {
		t.Fatalf("incidents = %+v", incidents)
	}
	if rule := mustRule(t, h, ruleID); rule.State != api.AlertRuleStateOK {
		t.Fatalf("state = %q, want ok", rule.State)
	}
	if h.sender.CallCount() != 2 || !strings.Contains(string(h.sender.Calls()[1].Body), `"event":"recovered"`) {
		t.Fatalf("deliveries = %+v", h.sender.Calls())
	}
	if summary := mustSummary(t, h); summary.Delivered != 2 {
		t.Fatalf("delivered = %d", summary.Delivered)
	}

	h.clock.Advance(1_000)
	h.observe("m1", low)
	h.process()
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 1 {
		t.Fatalf("recovery duplicated: %+v", incidents)
	}
}

func TestUnavailableConditionsFireAndRecover(t *testing.T) {
	h := newHarness(t)
	unavailableID := h.ruleWithResource("GPU unavailable", "gpu.utilization", "gpu:GPU-1", api.AlertConditionUnavailable, nil, 0)
	thresholdID := h.rule("CPU high", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(80), 0)

	payload := basePayload()
	payload.CPU = protocol.Unavailable{Reason: "cpu counters unavailable"}
	payload.GPUs = protocol.Unavailable{Reason: "no supported GPU telemetry"}
	h.observe("m1", payload)
	h.process()

	incidents := mustIncidents(t, h, AllMachines())
	if len(incidents) != 1 || incidents[0].RuleID != unavailableID {
		t.Fatalf("incidents = %+v", incidents)
	}
	if rule := mustRule(t, h, unavailableID); rule.State != api.AlertRuleStateFiring {
		t.Fatalf("unavailable state = %q", rule.State)
	}
	if rule := mustRule(t, h, thresholdID); rule.State != api.AlertRuleStateOK {
		t.Fatalf("threshold state = %q", rule.State)
	}

	h.clock.Advance(1_000)
	h.observe("m1", basePayload())
	h.process()
	incidents = mustIncidents(t, h, AllMachines())
	if incidents[0].Status != api.AlertIncidentRecovered {
		t.Fatalf("incident = %+v", incidents[0])
	}
	if rule := mustRule(t, h, unavailableID); rule.State != api.AlertRuleStateOK {
		t.Fatalf("state = %q", rule.State)
	}
}

func TestStaleUsesCadenceFloorReceiptTimeAndFreshReceipt(t *testing.T) {
	h := newHarness(t)
	h.rule("Stale", "m1", api.AlertMetricCPUUsage, api.AlertConditionStale, nil, 0)
	h.clock.Advance(60_000)
	now := h.clock.Now()
	twentySecondsAgo := now.Add(-20 * time.Second)
	h.engine.ObserveAt("m1", h.record(basePayload()), Live, twentySecondsAgo)

	h.engine.Tick([]MachineState{machineState("m1", nil, int64Ptr(10_000), nil)})
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 0 {
		t.Fatalf("20 s with a 10 s cadence is not stale: %+v", incidents)
	}

	h.engine.Tick([]MachineState{machineState("m1", nil, int64Ptr(5_000), nil)})
	h.process()
	incidents := mustIncidents(t, h, AllMachines())
	if len(incidents) != 1 || incidents[0].Status != api.AlertIncidentOpen {
		t.Fatalf("incidents = %+v", incidents)
	}

	h.engine.ObserveAt("m1", h.record(basePayload()), Live, now)
	h.process()
	incidents = mustIncidents(t, h, AllMachines())
	if incidents[0].Status != api.AlertIncidentRecovered {
		t.Fatalf("incident = %+v", incidents[0])
	}
}

func TestMaintenanceSuppressesPendingAndIncidents(t *testing.T) {
	h := newHarness(t)
	maintenance := h.clock.Now().Add(60 * time.Second)
	rule, err := h.engine.CreateRule(context.Background(), h.authority, RuleInput{
		Name:             "Maintenance CPU",
		MachineID:        "m1",
		Metric:           api.AlertMetricCPUUsage,
		Condition:        api.AlertConditionAbove,
		Threshold:        floatPtr(80),
		DurationMs:       0,
		MaintenanceUntil: &maintenance,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.observe("m1", cpuPayload(99))
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 0 {
		t.Fatalf("incident during maintenance: %+v", incidents)
	}
	if got := mustRule(t, h, rule.ID); got.State != api.AlertRuleStateOK {
		t.Fatalf("state = %q", got.State)
	}

	h.clock.Advance(61_000)
	h.observe("m1", cpuPayload(99))
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 1 {
		t.Fatalf("no incident after maintenance: %+v", incidents)
	}
	if got := mustRule(t, h, rule.ID); got.State != api.AlertRuleStateFiring {
		t.Fatalf("state = %q", got.State)
	}
}

func TestRestartPreservesOpenIncidentAndPendingDuration(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hub.db")
	clock := newTestClock(2_000_000)
	highRecord := metricsRecord(cpuPayload(95), 1)

	first, err := storage.Open(context.Background(), path, storage.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	authority := testAuthority(t, first)
	firstSender := &recordingSender{}
	firstEngine, err := New(Options{Database: first, Now: clock.Now, Sender: firstSender})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firstEngine.ConfigureWebhook(context.Background(), authority, WebhookConfig{URL: "http://127.0.0.1:9/sink"}); err != nil {
		t.Fatal(err)
	}
	rule, err := firstEngine.CreateRule(context.Background(), authority, RuleInput{
		Name: "Restart", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(80), DurationMs: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstEngine.ObserveAt("m1", highRecord, Live, clock.Now())
	if err := firstEngine.ProcessNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if incidents, _ := firstEngine.ListIncidents(context.Background(), AllMachines(), 200); len(incidents) != 1 {
		t.Fatalf("incidents = %+v", incidents)
	}
	firstEngine.Stop()
	_ = first.Close()

	clock.Advance(1_000)
	second, err := storage.Open(context.Background(), path, storage.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	secondSender := &recordingSender{}
	secondEngine, err := New(Options{Database: second, Now: clock.Now, Sender: secondSender})
	if err != nil {
		t.Fatal(err)
	}
	secondEngine.ObserveAt("m1", highRecord, Live, clock.Now())
	if err := secondEngine.ProcessNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if incidents, _ := secondEngine.ListIncidents(context.Background(), AllMachines(), 200); len(incidents) != 1 {
		t.Fatalf("restart duplicated incident: %+v", incidents)
	}
	if summary, _ := secondEngine.NotificationSummary(context.Background(), AllMachines()); summary.Delivered != 1 {
		t.Fatalf("delivered = %d", summary.Delivered)
	}

	clock.Advance(1_000)
	secondEngine.ObserveAt("m1", metricsRecord(cpuPayload(95), 2), Live, clock.Now())
	if err := secondEngine.ProcessNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if incidents, _ := secondEngine.ListIncidents(context.Background(), AllMachines(), 200); len(incidents) != 1 {
		t.Fatalf("firing state lost: %+v", incidents)
	}
	if got, _ := secondEngine.GetRule(context.Background(), rule.ID); got.State != api.AlertRuleStateFiring {
		t.Fatalf("state = %q", got.State)
	}

	pending, err := secondEngine.CreateRule(context.Background(), authority, RuleInput{
		Name: "Pending restart", MachineID: "m2", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(80), DurationMs: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondEngine.ObserveAt("m2", metricsRecord(cpuPayload(95), 1), Live, clock.Now())
	if got, _ := secondEngine.GetRule(context.Background(), pending.ID); got.State != api.AlertRuleStatePending {
		t.Fatalf("state = %q", got.State)
	}
	secondEngine.Stop()
	_ = second.Close()

	clock.Advance(6_000)
	third, err := storage.Open(context.Background(), path, storage.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	thirdEngine, err := New(Options{Database: third, Now: clock.Now, Sender: &recordingSender{}})
	if err != nil {
		t.Fatal(err)
	}
	thirdEngine.ObserveAt("m2", metricsRecord(cpuPayload(95), 2), Live, clock.Now())
	if got, _ := thirdEngine.GetRule(context.Background(), pending.ID); got.State != api.AlertRuleStatePending {
		t.Fatalf("only 6 of 10 s elapsed, state = %q", got.State)
	}
	clock.Advance(4_000)
	thirdEngine.ObserveAt("m2", metricsRecord(cpuPayload(95), 3), Live, clock.Now())
	if got, _ := thirdEngine.GetRule(context.Background(), pending.ID); got.State != api.AlertRuleStateFiring {
		t.Fatalf("state = %q", got.State)
	}
	thirdEngine.Stop()
	_ = third.Close()
}

func TestRetryStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hub.db")
	clock := newTestClock(7_000_000)
	first, err := storage.Open(context.Background(), path, storage.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	authority := testAuthority(t, first)
	failing := &recordingSender{result: func(WebhookRequest) WebhookResult {
		status := 503
		return WebhookResult{Status: &status, Error: "HTTP 503"}
	}}
	firstEngine, err := New(Options{Database: first, Now: clock.Now, Sender: failing, MaxAttempts: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firstEngine.ConfigureWebhook(context.Background(), authority, WebhookConfig{URL: "http://127.0.0.1:9/sink"}); err != nil {
		t.Fatal(err)
	}
	if _, err := firstEngine.CreateRule(context.Background(), authority, RuleInput{
		Name: "Retry restart", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(1), DurationMs: 0,
	}); err != nil {
		t.Fatal(err)
	}
	firstEngine.ObserveAt("m1", metricsRecord(cpuPayload(99), 1), Live, clock.Now())
	if err := firstEngine.ProcessNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := firstEngine.NotificationSummary(context.Background(), AllMachines())
	if before.Pending != 1 || len(before.Recent) != 1 || before.Recent[0].Attempts != 1 {
		t.Fatalf("summary = %+v", before)
	}
	jobID := before.Recent[0].ID
	firstEngine.Stop()
	_ = first.Close()

	clock.Advance(10_000)
	second, err := storage.Open(context.Background(), path, storage.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	deliveredCalls := 0
	secondEngine, err := New(Options{Database: second, Now: clock.Now, MaxAttempts: 4,
		Sender: &recordingSender{result: func(WebhookRequest) WebhookResult {
			mu.Lock()
			deliveredCalls++
			mu.Unlock()
			status := 200
			return WebhookResult{OK: true, Status: &status}
		}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := secondEngine.ProcessNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := secondEngine.ProcessNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := secondEngine.NotificationSummary(context.Background(), AllMachines())
	if deliveredCalls != 1 || after.Delivered != 1 || after.Pending != 0 {
		t.Fatalf("deliveredCalls=%d summary=%+v", deliveredCalls, after)
	}
	if len(after.Recent) != 1 || after.Recent[0].ID != jobID || after.Recent[0].Attempts != 2 {
		t.Fatalf("job = %+v", after.Recent)
	}
	secondEngine.Stop()
	_ = second.Close()
}

func TestRemoteErrorTextIsRedacted(t *testing.T) {
	db, err := storage.Open(context.Background(), ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	authority := testAuthority(t, db)
	defer func() { _ = db.Close() }()
	clock := newTestClock(8_000_000)
	const (
		configURL = "http://127.0.0.1:9/private-sink?token=url-secret"
		header    = "header-secret"
		secret    = "hmac-secret"
	)
	sender := &recordingSender{result: func(WebhookRequest) WebhookResult {
		status := 500
		return WebhookResult{
			Status: &status,
			Error: "POST " + configURL + " failed; Authorization: Bearer bearer-secret; " +
				"x-channel: " + header + "; signature=" + secret,
		}
	}}
	engine, err := New(Options{Database: db, Now: clock.Now, Sender: sender, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ConfigureWebhook(context.Background(), authority, WebhookConfig{
		URL: configURL, Headers: map[string]string{"x-channel": header}, Secret: stringPtr(secret),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateRule(context.Background(), authority, RuleInput{
		Name: "Redaction", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(1), DurationMs: 0,
	}); err != nil {
		t.Fatal(err)
	}
	engine.ObserveAt("m1", metricsRecord(cpuPayload(99), 1), Live, clock.Now())
	if err := engine.ProcessNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	summary, _ := engine.NotificationSummary(context.Background(), AllMachines())
	stored := ""
	if summary.LastError != nil {
		stored = *summary.LastError
	}
	for _, sensitive := range []string{configURL, header, secret, "bearer-secret", "url-secret"} {
		if strings.Contains(stored, sensitive) {
			t.Fatalf("error %q leaks %q", stored, sensitive)
		}
	}
	if !strings.Contains(stored, "[redacted") {
		t.Fatalf("error was not redacted: %q", stored)
	}
	status, _ := engine.WebhookStatus(context.Background())
	serialized := string(status.Channel) + string(status.Delivery)
	if status.LastError != nil {
		serialized += *status.LastError
	}
	if strings.Contains(serialized, configURL) {
		t.Fatal("webhook status leaks the URL")
	}
	engine.Stop()
}

func TestHistoricalUnavailableAfterRestartIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.ruleWithResource("Historical unavailable", "gpu.utilization", "gpu:GPU-1", api.AlertConditionUnavailable, nil, 0)
	payload := basePayload()
	payload.GPUs = protocol.Unavailable{Reason: "old"}
	now := h.clock.Now()
	received := now.Add(-20 * time.Second)
	h.engine.Tick([]MachineState{machineState("m1", &received, int64Ptr(5_000),
		&MachineMetrics{Payload: payload, ReceivedAt: received})})
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 0 {
		t.Fatalf("historical unavailable fired: %+v", incidents)
	}
}

// TestStaleRuleFiresForMachineThatWentSilentBeforeRestart reproduces a stale
// rule created after a hub restart for a machine whose last receipt predates
// the new process. The previous process had no rule for the machine, so it
// never wrote an alert progress row; machine_latest is the only durable
// evidence that the machine went silent.
func TestStaleRuleFiresForMachineThatWentSilentBeforeRestart(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("Stale after restart", "m-dead", api.AlertMetricCPUUsage, api.AlertConditionStale, nil, 0)
	// The engine started now; the machine's durable last receipt is ten
	// minutes before that, well past the 15 s floor for a 5 s cadence.
	last := h.clock.Now().Add(-10 * time.Minute)
	h.engine.Tick([]MachineState{machineState("m-dead", &last, int64Ptr(5_000), nil)})

	incidents := mustIncidents(t, h, AllMachines())
	if len(incidents) != 1 || incidents[0].RuleID != ruleID {
		t.Fatalf("restart stale rule did not fire: %+v", incidents)
	}
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateFiring {
		t.Fatalf("rule state = %q, want firing", got.State)
	}
}

// TestStaleTickIgnoresUnevaluatedLiveReceiptAfterStart keeps the round-3 rule:
// a live receipt newer than the engine start is not evidence until the engine
// evaluates it, even for a stale rule on a machine with no progress row.
func TestStaleTickIgnoresUnevaluatedLiveReceiptAfterStart(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("Stale live receipt", "m-live", api.AlertMetricCPUUsage, api.AlertConditionStale, nil, 0)
	start := h.clock.Now()
	h.clock.Advance(60_000)
	liveReceipt := start.Add(2 * time.Second)
	h.engine.Tick([]MachineState{machineState("m-live", &liveReceipt, int64Ptr(5_000), nil)})

	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 0 {
		t.Fatalf("tick judged an unevaluated live receipt: %+v", incidents)
	}
	if got := mustRule(t, h, ruleID); got.State == api.AlertRuleStateFiring {
		t.Fatalf("unevaluated live receipt fired the stale rule: %+v", got)
	}
}

func TestDrainBoundsShutdownAndLateCompletionsDoNotWrite(t *testing.T) {
	db, err := storage.Open(context.Background(), ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	authority := testAuthority(t, db)
	clock := newTestClock(10_000_000)
	sender := &recordingSender{started: make(chan struct{}), release: make(chan struct{})}
	engine, err := New(Options{Database: db, Now: clock.Now, Sender: sender, DeliveryTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ConfigureWebhook(context.Background(), authority, WebhookConfig{URL: "http://127.0.0.1:9/sink"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateRule(context.Background(), authority, RuleInput{
		Name: "Drain", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(1), DurationMs: 0,
	}); err != nil {
		t.Fatal(err)
	}
	engine.ObserveAt("m1", metricsRecord(cpuPayload(99), 1), Live, clock.Now())
	select {
	case <-sender.started:
	case <-time.After(2 * time.Second):
		t.Fatal("sender never started")
	}
	engine.Drain(50 * time.Millisecond)
	_ = db.Close()
	close(sender.release)
	if err := engine.ProcessNotifications(context.Background()); err != nil {
		t.Fatalf("post-close processing: %v", err)
	}
}

func TestRuleCreationDoesNotDependOnReceipts(t *testing.T) {
	db, err := storage.Open(context.Background(), ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	authority := testAuthority(t, db)
	defer func() { _ = db.Close() }()
	clock := newTestClock(11_000_000)
	engine, err := New(Options{Database: db, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	now := clock.Now()
	engine.Tick([]MachineState{machineState("m1", &now, int64Ptr(5_000), nil)})
	for _, input := range []RuleInput{
		{Name: "cpu threshold", MachineID: "m1", Metric: api.AlertMetricCPUUsage, Condition: api.AlertConditionAbove, Threshold: floatPtr(80)},
		{Name: "memory unavailable", MachineID: "m1", Metric: api.AlertMetricMemoryUsed, Condition: api.AlertConditionUnavailable},
		{Name: "cpu stale", MachineID: "m1", Metric: api.AlertMetricCPUUsage, Condition: api.AlertConditionStale},
	} {
		if _, err := engine.CreateRule(context.Background(), authority, input); err != nil {
			t.Fatalf("create %q: %v", input.Name, err)
		}
	}
	engine.Stop()
}

func TestBottleneckMetricsKeepResourceRulesAndIOPressureUsesDiskFamily(t *testing.T) {
	h := newHarness(t)
	memory := h.rule("Memory percent", "m1", api.AlertMetricMemoryUsage, api.AlertConditionAbove, floatPtr(90), 0)
	if got := mustRule(t, h, memory); got.ResourceID != "" {
		t.Fatalf("memory resourceId = %q", got.ResourceID)
	}
	if _, err := h.engine.CreateRule(context.Background(), h.authority, RuleInput{
		Name: "Missing resource", MachineID: "m1", Metric: api.AlertMetricFSUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(90),
	}); !IsValidation(err) || !strings.Contains(err.Error(), "requires a resource id") {
		t.Fatalf("missing resource error = %v", err)
	}
	if _, err := h.engine.CreateRule(context.Background(), h.authority, RuleInput{
		Name: "Unexpected resource", MachineID: "m1", Metric: api.AlertMetricMemoryUsage,
		ResourceID: "fs:/", Condition: api.AlertConditionAbove, Threshold: floatPtr(90),
	}); !IsValidation(err) || !strings.Contains(err.Error(), "does not accept a resource id") {
		t.Fatalf("unexpected resource error = %v", err)
	}
	io := h.rule("I/O pressure unavailable", "m1", api.AlertMetricIOPressureSome, api.AlertConditionUnavailable, nil, 0)

	payload := basePayload()
	payload.Disks = protocol.Unavailable{Reason: "disk counters unavailable"}
	h.observe("m1", payload)
	h.process()
	if got := mustRule(t, h, io); got.State != api.AlertRuleStateFiring {
		t.Fatalf("io pressure state = %q", got.State)
	}
}

func TestMissingPreviouslyObservedResourceIsUnavailable(t *testing.T) {
	h := newHarness(t)
	ruleID := h.ruleWithResource("GPU missing", "gpu.utilization", "gpu:GPU-1", api.AlertConditionUnavailable, nil, 0)
	down := basePayload()
	down.GPUs = protocol.Unavailable{Reason: "gpu reading stale"}
	h.observe("m1", down)
	h.process()
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateFiring {
		t.Fatalf("state = %q", got.State)
	}

	h.clock.Advance(1_000)
	missing := basePayload()
	missing.GPUs = protocol.GpusAvailable{Devices: []protocol.GpuReading{
		protocol.GpuAvailable{ID: "gpu:GPU-2", UtilizationPercent: floatPtr(20),
			MemoryUsedBytes: int64Ptr(10), MemoryTotalBytes: int64Ptr(20),
			TemperatureC: floatPtr(40), PowerWatts: floatPtr(50)},
	}}
	h.observe("m1", missing)
	h.process()
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateFiring {
		t.Fatalf("missing resource silently recovered: %q", got.State)
	}
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 1 || incidents[0].Status != api.AlertIncidentOpen {
		t.Fatalf("incidents = %+v", incidents)
	}

	h.clock.Advance(1_000)
	h.observe("m1", basePayload())
	h.process()
	if incidents := mustIncidents(t, h, AllMachines()); incidents[0].Status != api.AlertIncidentRecovered {
		t.Fatalf("incident = %+v", incidents[0])
	}
}

func TestHistoricalSeriesMakesMissingResourceUnavailable(t *testing.T) {
	h := newHarness(t)
	if err := h.db.Write(context.Background(), func(tx *storage.Tx) error {
		_, err := tx.Exec("INSERT INTO series (machine_id, metric, resource_id) VALUES (?, ?, ?)",
			"m1", "gpu.utilization", "gpu:GPU-1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ruleID := h.ruleWithResource("Historical GPU", "gpu.utilization", "gpu:GPU-1", api.AlertConditionUnavailable, nil, 0)
	missing := basePayload()
	missing.GPUs = protocol.GpusAvailable{Devices: []protocol.GpuReading{
		protocol.GpuAvailable{ID: "gpu:GPU-2", UtilizationPercent: floatPtr(20)},
	}}
	h.observe("m1", missing)
	h.process()
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateFiring {
		t.Fatalf("state = %q, want firing from historical series", got.State)
	}
}

func TestFailedDeliveriesBackOffAndStopAtMaxAttempts(t *testing.T) {
	db, err := storage.Open(context.Background(), ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	authority := testAuthority(t, db)
	defer func() { _ = db.Close() }()
	clock := newTestClock(3_000_000)
	sender := &recordingSender{result: func(WebhookRequest) WebhookResult {
		status := 500
		return WebhookResult{Status: &status, Error: "HTTP 500"}
	}}
	engine, err := New(Options{Database: db, Now: clock.Now, Sender: sender, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ConfigureWebhook(context.Background(), authority, WebhookConfig{URL: "http://127.0.0.1:9/sink"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateRule(context.Background(), authority, RuleInput{
		Name: "Failure path", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(1), DurationMs: 0,
	}); err != nil {
		t.Fatal(err)
	}
	engine.ObserveAt("m1", metricsRecord(cpuPayload(99), 1), Live, clock.Now())
	if err := engine.ProcessNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	summary, _ := engine.NotificationSummary(context.Background(), AllMachines())
	if summary.Pending != 1 || summary.Failed != 0 || summary.LastError == nil || *summary.LastError != "HTTP 500" {
		t.Fatalf("summary = %+v", summary)
	}
	if len(summary.Recent) != 1 || summary.Recent[0].Attempts != 1 {
		t.Fatalf("job = %+v", summary.Recent)
	}

	clock.Advance(4_000)
	if err := engine.ProcessNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.CallCount() != 1 {
		t.Fatalf("retry ignored persisted backoff: %d calls", sender.CallCount())
	}
	clock.Advance(2_000)
	if err := engine.ProcessNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	summary, _ = engine.NotificationSummary(context.Background(), AllMachines())
	if summary.Failed != 1 || len(summary.Recent) != 1 ||
		summary.Recent[0].Status != api.AlertJobFailed || summary.Recent[0].Attempts != 2 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Recent[0].LastError == nil || *summary.Recent[0].LastError != "HTTP 500" {
		t.Fatalf("lastError = %v", summary.Recent[0].LastError)
	}
	engine.Stop()
}

func TestWebhookNotConfiguredPostponesWithoutBurningAttempts(t *testing.T) {
	h := newHarness(t)
	if _, err := h.engine.ClearWebhook(context.Background(), h.authority); err != nil {
		t.Fatal(err)
	}
	ruleID := h.rule("No sink", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(1), 0)
	h.observe("m1", cpuPayload(99))
	h.process()
	summary := mustSummary(t, h)
	if summary.Pending != 1 || len(summary.Recent) != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	job := summary.Recent[0]
	if job.Attempts != 0 || job.LastError == nil || *job.LastError != "webhook not configured" {
		t.Fatalf("job = %+v", job)
	}
	if rule := mustRule(t, h, ruleID); rule.State != api.AlertRuleStateFiring {
		t.Fatalf("state = %q", rule.State)
	}
}

// -- helpers -------------------------------------------------------------

func (h *harness) ruleWithResource(name, metric, resourceID string, condition api.AlertCondition, threshold *float64, durationMs int64) string {
	h.t.Helper()
	rule, err := h.engine.CreateRule(context.Background(), h.authority, RuleInput{
		Name:       name,
		MachineID:  "m1",
		Metric:     api.AlertMetric(metric),
		ResourceID: resourceID,
		Condition:  condition,
		Threshold:  threshold,
		DurationMs: durationMs,
	})
	if err != nil {
		h.t.Fatalf("create rule: %v", err)
	}
	return rule.ID
}

func mustRule(t *testing.T, h *harness, id string) api.AlertRule {
	t.Helper()
	rule, err := h.engine.GetRule(context.Background(), id)
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	return rule
}

func mustIncidents(t *testing.T, h *harness, scope Scope) []api.AlertIncident {
	t.Helper()
	incidents, err := h.engine.ListIncidents(context.Background(), scope, 200)
	if err != nil {
		t.Fatalf("list incidents: %v", err)
	}
	return incidents
}

func mustSummary(t *testing.T, h *harness) api.AlertNotificationSummary {
	t.Helper()
	summary, err := h.engine.NotificationSummary(context.Background(), AllMachines())
	if err != nil {
		t.Fatalf("notification summary: %v", err)
	}
	return summary
}

var _ = time.Second

func TestIncidentMessagesMatchReferenceFormat(t *testing.T) {
	percent := validatedRule{
		name: "CPU high", machineID: "m1", metric: api.AlertMetricCPUUsage,
		condition: api.AlertConditionAbove, threshold: floatPtr(0), durationMs: 0,
	}
	if got := incidentMessage(percent, floatPtr(6.82)); got != "CPU high: CPU usage 6.82% is at or above 0% on m1" {
		t.Fatalf("percent message = %q", got)
	}
	bytes := validatedRule{
		name: "Memory high", machineID: "m1", metric: api.AlertMetricMemoryUsed,
		condition: api.AlertConditionAbove, threshold: floatPtr(5 * 1024 * 1024 * 1024),
	}
	if got := incidentMessage(bytes, floatPtr(1024*1024*1024)); got != "Memory high: Memory used 1.0 GiB is at or above 5.0 GiB on m1" {
		t.Fatalf("bytes message = %q", got)
	}
	rate := validatedRule{
		name: "Network", machineID: "m1", metric: api.AlertMetricNetRx, resourceID: "net:eno1",
		condition: api.AlertConditionBelow, threshold: floatPtr(1024),
	}
	if got := incidentMessage(rate, floatPtr(1536)); got != "Network: Network receive (net:eno1) 1.5 KiB/s is at or below 1.0 KiB/s on m1" {
		t.Fatalf("rate message = %q", got)
	}
	temperature := validatedRule{
		name: "GPU hot", machineID: "m1", metric: api.AlertMetricGPUTemperature, resourceID: "gpu:GPU-1",
		condition: api.AlertConditionAbove, threshold: floatPtr(85.4),
	}
	if got := incidentMessage(temperature, floatPtr(90.6)); got != "GPU hot: GPU temperature (gpu:GPU-1) 91 °C is at or above 85 °C on m1" {
		t.Fatalf("temperature message = %q", got)
	}
	latency := validatedRule{
		name: "Latency", machineID: "m1", metric: api.AlertMetricDiskReadLatency, resourceID: "disk:sda",
		condition: api.AlertConditionAbove, threshold: floatPtr(1),
	}
	if got := incidentMessage(latency, floatPtr(12.34)); got != "Latency: Disk read latency (disk:sda) 12.3 ms is at or above 1 ms on m1" {
		t.Fatalf("latency message = %q", got)
	}
	stale := validatedRule{name: "Stale", machineID: "m1", metric: api.AlertMetricCPUUsage, condition: api.AlertConditionStale}
	if got := incidentMessage(stale, nil); got != "Stale: no receipt from m1 within the stale threshold" {
		t.Fatalf("stale message = %q", got)
	}
	unavailable := validatedRule{
		name: "GPU gone", machineID: "m1", metric: api.AlertMetricGPUUtilization,
		resourceID: "gpu:GPU-1", condition: api.AlertConditionUnavailable, durationMs: 300_000,
	}
	if got := incidentMessage(unavailable, nil); got != "GPU gone: GPU utilization (gpu:GPU-1) unavailable on m1 for 5 min" {
		t.Fatalf("unavailable message = %q", got)
	}
}

func TestReplayAndPreStartRecordsNeverEvaluate(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("Live only", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(1), 0)
	record := metricsRecord(cpuPayload(99), 1)
	h.engine.ObserveAt("m1", record, Replay, h.clock.Now())
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateOK {
		t.Fatalf("replay changed state: %q", got.State)
	}
	h.engine.ObserveAt("m1", record, Live, h.clock.Now().Add(-time.Second))
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateOK {
		t.Fatalf("pre-start record changed state: %q", got.State)
	}
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 0 {
		t.Fatalf("incidents = %+v", incidents)
	}
}

func TestRuleUpdateAndDisableReconcileIncidentsSilently(t *testing.T) {
	h := newHarness(t)
	rule := mustRule(t, h, h.rule("Update target", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(1), 0))
	h.observe("m1", cpuPayload(99))
	h.process()
	if h.sender.CallCount() != 1 {
		t.Fatalf("fired deliveries = %d", h.sender.CallCount())
	}

	updated, err := h.engine.UpdateRule(context.Background(), h.authority, rule.ID, RuleInput{
		Name: "Update target", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(100), DurationMs: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != api.AlertRuleStateOK || updated.ActiveIncidentID != nil {
		t.Fatalf("updated rule = %+v", updated)
	}
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 1 || incidents[0].Status != api.AlertIncidentRecovered {
		t.Fatalf("incident was not closed silently: %+v", incidents)
	}
	h.process()
	if h.sender.CallCount() != 1 {
		t.Fatalf("silent close enqueued a delivery: %d", h.sender.CallCount())
	}

	if _, err := h.engine.UpdateRule(context.Background(), h.authority, rule.ID, RuleInput{
		Name: "Update target", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(1), DurationMs: 0,
	}); err != nil {
		t.Fatal(err)
	}
	h.observe("m1", cpuPayload(99))
	h.process()
	if got := mustRule(t, h, rule.ID); got.State != api.AlertRuleStateFiring {
		t.Fatalf("state = %q", got.State)
	}
	disabled := false
	if _, err := h.engine.UpdateRule(context.Background(), h.authority, rule.ID, RuleInput{
		Name: "Update target", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(1), DurationMs: 0, Enabled: &disabled,
	}); err != nil {
		t.Fatal(err)
	}
	if got := mustRule(t, h, rule.ID); got.State != api.AlertRuleStateOK || got.Enabled {
		t.Fatalf("disabled rule = %+v", got)
	}
}

func TestProcessNotificationsReleasesOwnershipAfterErrorWithPendingRepeat(t *testing.T) {
	db, err := storage.Open(context.Background(), ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	authority := testAuthority(t, db)
	clock := newTestClock(12_000_000)
	sender := &recordingSender{started: make(chan struct{}), release: make(chan struct{})}
	engine, err := New(Options{Database: db, Now: clock.Now, Sender: sender, DeliveryTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		engine.Stop()
		_ = db.Close()
	}()
	if _, err := engine.ConfigureWebhook(context.Background(), authority, WebhookConfig{URL: "http://127.0.0.1:9/sink"}); err != nil {
		t.Fatal(err)
	}
	// Insert a due job directly: Observe would also schedule a background run,
	// and this test needs ProcessNotifications to be the single owner.
	if err := db.Write(context.Background(), func(tx *storage.Tx) error {
		return insertJobTx(tx, jobRow{
			id: "job-repeat", ruleID: "rule", incidentID: "incident", machineID: "m1",
			event: "fired", status: jobPending, maxAttempts: 1,
			createdAt: clock.Now(), payloadJSON: `{"event":"fired"}`,
		})
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- engine.ProcessNotifications(ctx) }()
	select {
	case <-sender.started:
	case <-time.After(2 * time.Second):
		t.Fatal("sender never started")
	}
	// Ask for a repeat pass, then make the current pass fail.
	engine.scheduleNotifications()
	cancel()
	close(sender.release)
	select {
	case err := <-first:
		if err == nil {
			t.Fatal("expected the cancelled pass to fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first ProcessNotifications did not return")
	}
	second := make(chan error, 1)
	go func() { second <- engine.ProcessNotifications(context.Background()) }()
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("ProcessNotifications deadlocked after an error with a pending repeat")
	}
}

func TestObserveReadsRuleStateInsideTheWriteTransaction(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("Concurrent delete", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(1), 0)
	loaded := make(chan struct{})
	release := make(chan struct{})
	h.engine.testRulesLoadedHook = func() {
		close(loaded)
		<-release
	}
	observed := make(chan struct{})
	go func() {
		h.engine.ObserveAt("m1", metricsRecord(cpuPayload(99), 1), Live, h.clock.Now())
		close(observed)
	}()
	select {
	case <-loaded:
	case <-time.After(2 * time.Second):
		t.Fatal("observation never loaded rule state")
	}
	deleted := make(chan error, 1)
	go func() { deleted <- h.engine.DeleteRule(context.Background(), h.authority, ruleID) }()
	// Let the delete reach the writer while the observation is paused.
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed:
	case <-time.After(2 * time.Second):
		t.Fatal("observation did not finish")
	}
	if _, err := h.engine.GetRule(context.Background(), ruleID); !IsNotFound(err) {
		t.Fatalf("rule lookup after delete = %v, want not found", err)
	}
	for _, incident := range mustIncidents(t, h, AllMachines()) {
		if incident.RuleID == ruleID && incident.Status == api.AlertIncidentOpen {
			t.Fatalf("observation orphaned an open incident for a deleted rule: %+v", incident)
		}
	}
}

func TestPendingDurationUsesReceiptTimeNotTickHorizon(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("Receipt evidence", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(80), 10_000)
	start := h.clock.Now()

	// The rule is pending with evidence at start.
	h.engine.ObserveAt("m1", h.record(cpuPayload(99)), Live, start)
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStatePending {
		t.Fatalf("state = %q, want pending", got.State)
	}

	// A tick advances wall-clock evaluation by 60 s while a sample received at
	// start+2 s is still queued. Clamping that sample to the tick's horizon
	// would manufacture 60 s of evidence and fire the rule.
	h.clock.Advance(60_000)
	h.engine.Tick(nil)
	h.engine.ObserveAt("m1", h.record(cpuPayload(99)), Live, start.Add(2*time.Second))

	rule := mustRule(t, h, ruleID)
	if rule.State != api.AlertRuleStatePending {
		t.Fatalf("state = %q, want pending after only 2 s of evidence", rule.State)
	}
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 0 {
		t.Fatalf("incident manufactured from the tick horizon: %+v", incidents)
	}
	if rule.PendingSince == nil {
		t.Fatal("pendingSince is nil")
	}
	pendingSince, err := time.Parse(isoLayout, *rule.PendingSince)
	if err != nil {
		t.Fatal(err)
	}
	if !pendingSince.Equal(start) {
		t.Fatalf("pendingSince = %s, want the first evidence time %s", pendingSince, start)
	}
	if rule.LastObservedAt == nil {
		t.Fatal("lastObservedAt is nil")
	}
	lastObserved, err := time.Parse(isoLayout, *rule.LastObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !lastObserved.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("lastObservedAt = %s, want the receipt time %s", lastObserved, start.Add(2*time.Second))
	}

	// Ten seconds of evidence still opens the incident, timestamped from the
	// evidence rather than the tick horizon.
	firedAt := start.Add(10 * time.Second)
	h.engine.ObserveAt("m1", h.record(cpuPayload(99)), Live, firedAt)
	incidents := mustIncidents(t, h, AllMachines())
	if len(incidents) != 1 {
		t.Fatalf("incidents = %+v", incidents)
	}
	started, err := time.Parse(isoLayout, incidents[0].StartedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !started.Equal(firedAt) {
		t.Fatalf("incident startedAt = %s, want the evidence time %s", started, firedAt)
	}
}

func TestObservationOlderThanAppliedStateDoesNotChangeDurations(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("Out of order", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(80), 10_000)
	start := h.clock.Now()

	h.engine.ObserveAt("m1", h.record(cpuPayload(99)), Live, start)
	h.engine.ObserveAt("m1", h.record(cpuPayload(20)), Live, start.Add(5*time.Second))
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateOK {
		t.Fatalf("state = %q, want ok", got.State)
	}

	// A late sample received before the applied record must not restart a
	// pending duration or otherwise change state.
	h.engine.ObserveAt("m1", h.record(cpuPayload(99)), Live, start.Add(2*time.Second))
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateOK || got.PendingSince != nil {
		t.Fatalf("older observation changed state: %+v", got)
	}

	h.engine.ObserveAt("m1", h.record(cpuPayload(99)), Live, start.Add(6*time.Second))
	got := mustRule(t, h, ruleID)
	if got.State != api.AlertRuleStatePending || got.PendingSince == nil {
		t.Fatalf("post-gap state = %+v, want a fresh pending duration", got)
	}
	pendingSince, err := time.Parse(isoLayout, *got.PendingSince)
	if err != nil {
		t.Fatal(err)
	}
	if !pendingSince.Equal(start.Add(6 * time.Second)) {
		t.Fatalf("pendingSince = %s, want %s", pendingSince, start.Add(6*time.Second))
	}
}

func TestTickHonoursReceiptCutoff(t *testing.T) {
	h := newHarness(t)
	h.rule("Stale cutoff", "m1", api.AlertMetricCPUUsage, api.AlertConditionStale, nil, 0)
	h.clock.Advance(60_000)
	now := h.clock.Now()

	last := now.Add(-20 * time.Second)
	h.engine.ObserveAt("m1", h.record(basePayload()), Live, last)
	state := machineState("m1", nil, int64Ptr(5_000), nil)
	// A 5 s cadence has a 15 s stale floor. As of 10 s after the receipt the
	// machine is not stale even though the tick runs at 20 s.
	h.engine.TickAsOf([]MachineState{state}, now.Add(-10*time.Second))
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 0 {
		t.Fatalf("tick judged beyond its receipt cutoff: %+v", incidents)
	}

	h.engine.TickAsOf([]MachineState{state}, now)
	incidents := mustIncidents(t, h, AllMachines())
	if len(incidents) != 1 || incidents[0].Status != api.AlertIncidentOpen {
		t.Fatalf("incidents = %+v", incidents)
	}
}

func TestTickIgnoresUnevaluatedLivePayloadFromAnotherMachine(t *testing.T) {
	h := newHarness(t)
	// The reviewer's scenario: machine A's queued receipt at t+2 is drained,
	// while machine B's first unavailable sample at t+60 has already been
	// published to live state but is still queued for evaluation.
	h.rule("A cpu", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(80), 0)
	unavailable, err := h.engine.CreateRule(context.Background(), h.authority, RuleInput{
		Name: "B unavailable", MachineID: "m2", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionUnavailable, DurationMs: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := h.clock.Now()
	h.engine.ObserveAt("m1", h.record(cpuPayload(99)), Live, start.Add(2*time.Second))
	h.engine.ObserveAt("m2", h.record(basePayload()), Live, start)

	h.clock.Advance(60_000)
	tail := start.Add(60 * time.Second)
	// The live snapshot already holds B's unavailable payload, but the tick's
	// drain cutoff only covers A's t+2 record.
	h.engine.TickAsOf([]MachineState{
		machineState("m2", &tail, int64Ptr(5_000),
			&MachineMetrics{Payload: cpuUnavailablePayload("queued"), ReceivedAt: tail}),
	}, start.Add(2*time.Second))

	if got := mustRule(t, h, unavailable.ID); got.State != api.AlertRuleStateOK {
		t.Fatalf("tick judged B from live state not yet evaluated: %+v", got)
	}
	for _, incident := range mustIncidents(t, h, AllMachines()) {
		if incident.RuleID == unavailable.ID {
			t.Fatalf("tick manufactured a B incident: %+v", incident)
		}
	}

	// Now evaluate B's queued unavailable record at its own receipt time. It
	// starts a fresh 10 s interval; it must not inherit an interval the tick
	// would have backdated to t+2.
	h.engine.ObserveAt("m2", h.record(cpuUnavailablePayload("queued")), Live, tail)
	got := mustRule(t, h, unavailable.ID)
	if got.State != api.AlertRuleStatePending {
		t.Fatalf("queued unavailable sample fired immediately: %+v", got)
	}
	for _, incident := range mustIncidents(t, h, AllMachines()) {
		if incident.RuleID == unavailable.ID {
			t.Fatalf("queued unavailable sample manufactured an incident: %+v", incident)
		}
	}
}

func TestFailedTickDoesNotAdvanceCommittedHorizon(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("Failed tick stale", "m1", api.AlertMetricCPUUsage, api.AlertConditionStale, nil, 0)
	start := h.clock.Now()
	h.engine.ObserveAt("m1", h.record(basePayload()), Live, start)
	h.clock.Advance(60_000)
	now := h.clock.Now()
	state := machineState("m1", nil, int64Ptr(5_000), nil)

	if err := h.db.Write(context.Background(), func(tx *storage.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER fail_alert_tick BEFORE UPDATE ON alert_rules
			BEGIN SELECT RAISE(ABORT, 'injected tick failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.engine.TickAsOf([]MachineState{state}, now)
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 0 {
		t.Fatalf("failed tick committed an incident: %+v", incidents)
	}
	if err := h.db.Write(context.Background(), func(tx *storage.Tx) error {
		_, err := tx.Exec("DROP TRIGGER fail_alert_tick")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The retry uses its own earlier cutoff. A failed tick must not have
	// advanced the committed horizon to t+60.
	h.engine.TickAsOf([]MachineState{state}, start.Add(2*time.Second))
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 0 {
		t.Fatalf("retry evaluated beyond its cutoff: %+v", incidents)
	}
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateOK {
		t.Fatalf("retry made the machine stale: %+v", got)
	}

	// The failure left no durable side effect: the retry at the real horizon
	// still fires.
	h.engine.TickAsOf([]MachineState{state}, now)
	incidents := mustIncidents(t, h, AllMachines())
	if len(incidents) != 1 || incidents[0].Status != api.AlertIncidentOpen {
		t.Fatalf("incidents after retry at the true horizon = %+v", incidents)
	}
}

func TestContinuityStatePrunedAfterRetention(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	for i := 0; i < 1_000; i++ {
		if err := h.engine.BreakContinuityAt(fmt.Sprintf("m-%d", i), now); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(h.engine.barriers); got != 1_000 {
		t.Fatalf("barriers = %d, want 1000", got)
	}
	h.engine.evaluated["m-evidence"] = evaluatedObservation{receipt: now, payload: basePayload()}

	h.clock.Advance(int64((continuityRetention + time.Second) / time.Millisecond))
	h.engine.Tick(nil)

	if got := len(h.engine.barriers); got != 0 {
		t.Fatalf("barriers after retention = %d, want 0", got)
	}
	if got := len(h.engine.unresolvedBreaks); got != 0 {
		t.Fatalf("unresolved breaks after retention = %d, want 0", got)
	}
	if got := len(h.engine.evaluated); got != 0 {
		t.Fatalf("evaluated evidence after retention = %d, want 0", got)
	}
}

func TestPrunedUnresolvedBreakCannotFireOldPendingDuration(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("Old pending", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(80), 10_000)
	start := h.clock.Now()
	h.engine.ObserveAt("m1", h.record(cpuPayload(99)), Live, start)
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStatePending {
		t.Fatalf("state = %q, want pending", got.State)
	}
	if err := h.db.Write(context.Background(), func(tx *storage.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER fail_alert_reset BEFORE UPDATE ON alert_rules
			WHEN OLD.state = 'pending' AND NEW.state = 'ok'
			BEGIN SELECT RAISE(ABORT, 'injected continuity reset failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.BreakContinuityAt("m1", start); err == nil {
		t.Fatal("expected the continuity reset to fail")
	}
	if got := len(h.engine.unresolvedBreaks); got != 1 {
		t.Fatalf("unresolved breaks = %d, want 1", got)
	}
	if err := h.db.Write(context.Background(), func(tx *storage.Tx) error {
		_, err := tx.Exec("DROP TRIGGER fail_alert_reset")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	h.clock.Advance(int64((continuityRetention + time.Second) / time.Millisecond))
	h.engine.Tick(nil)
	if got := len(h.engine.barriers) + len(h.engine.unresolvedBreaks); got != 0 {
		t.Fatalf("continuity state after retention = %d, want 0", got)
	}

	// The durable rule is still pending from the pre-gap sample. A new sample
	// after the retention window must restart the interval rather than fire
	// from the pruned barrier's old evidence.
	at := h.clock.Now()
	h.engine.ObserveAt("m1", h.record(cpuPayload(99)), Live, at)
	got := mustRule(t, h, ruleID)
	if got.State != api.AlertRuleStatePending {
		t.Fatalf("pruned break fired an old pending duration: %+v", got)
	}
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 0 {
		t.Fatalf("pruned break manufactured an incident: %+v", incidents)
	}
}

func TestContinuityBarrierIgnoresPreGapEvidence(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("Barrier", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(80), 10_000)
	start := h.clock.Now()

	h.engine.ObserveAt("m1", h.record(cpuPayload(99)), Live, start)
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStatePending {
		t.Fatalf("state = %q, want pending", got.State)
	}
	if err := h.engine.BreakContinuityAt("m1", start.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateOK {
		t.Fatalf("state after break = %q, want ok", got.State)
	}

	// Pre-gap evidence cannot restart the duration.
	h.engine.ObserveAt("m1", h.record(cpuPayload(99)), Live, start.Add(2*time.Second))
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateOK {
		t.Fatalf("pre-gap evidence restarted the duration: %+v", got)
	}

	// Post-gap evidence starts a fresh duration instead of inheriting the
	// pre-gap pending interval.
	h.engine.ObserveAt("m1", h.record(cpuPayload(99)), Live, start.Add(12*time.Second))
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStatePending {
		t.Fatalf("state = %q, want pending", got.State)
	}
}

func TestBreakContinuityResetsPendingDurationButKeepsOpenIncident(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("Continuity", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(1), 10_000)
	h.observe("m1", cpuPayload(99))
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStatePending {
		t.Fatalf("state = %q, want pending", got.State)
	}
	if err := h.engine.BreakContinuity("m1"); err != nil {
		t.Fatal(err)
	}
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateOK || got.PendingSince != nil {
		t.Fatalf("rule after a continuity break = %+v", got)
	}

	// The duration restarts from the next observation instead of inheriting
	// the pre-gap pending interval.
	h.observe("m1", cpuPayload(99))
	h.clock.Advance(5_000)
	h.observe("m1", cpuPayload(99))
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStatePending {
		t.Fatalf("state = %q, want a restarted pending duration", got.State)
	}
	h.clock.Advance(5_000)
	h.observe("m1", cpuPayload(99))
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateFiring {
		t.Fatalf("state = %q, want firing after the restarted duration", got.State)
	}
	if err := h.engine.BreakContinuity("m1"); err != nil {
		t.Fatal(err)
	}
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateFiring {
		t.Fatalf("continuity break resolved an open incident: state = %q", got.State)
	}
	if incidents := mustIncidents(t, h, AllMachines()); len(incidents) != 1 || incidents[0].Status != api.AlertIncidentOpen {
		t.Fatalf("incidents = %+v", incidents)
	}
}

func TestConcurrentRuleCreatorsCannotExceedTheLimit(t *testing.T) {
	h := newHarness(t)
	const creators = MaxRules + 20
	start := make(chan struct{})
	var wait sync.WaitGroup
	var mu sync.Mutex
	var created, limited int
	var unexpected []error
	for i := 0; i < creators; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, err := h.engine.CreateRule(context.Background(), h.authority, RuleInput{
				Name: fmt.Sprintf("concurrent-%d", index), MachineID: "m1",
				Metric: api.AlertMetricCPUUsage, Condition: api.AlertConditionAbove,
				Threshold: floatPtr(1),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case IsValidation(err) && strings.Contains(err.Error(), "limit"):
				limited++
			default:
				unexpected = append(unexpected, err)
			}
		}(i)
	}
	close(start)
	wait.Wait()
	if len(unexpected) > 0 {
		t.Fatalf("unexpected create errors: %v", unexpected)
	}
	count, err := countRulesDB(context.Background(), h.db)
	if err != nil {
		t.Fatal(err)
	}
	if count != MaxRules || created != MaxRules || limited != creators-MaxRules {
		t.Fatalf("rules=%d created=%d limited=%d, want %d/%d/%d", count, created, limited, MaxRules, MaxRules, creators-MaxRules)
	}
}

func TestObserveUsesEngineClockAsReceiptTime(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("Observer seam", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(1), 0)
	var observer Observer = h.engine
	observer.Observe("m1", metricsRecord(cpuPayload(99), 1), Live)
	if got := mustRule(t, h, ruleID); got.State != api.AlertRuleStateFiring {
		t.Fatalf("state = %q", got.State)
	}
}
