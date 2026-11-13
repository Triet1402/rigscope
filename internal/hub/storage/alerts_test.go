package storage

import (
	"testing"
	"time"
)

func float64Ptr(value float64) *float64 { return &value }
func stringPtr(value string) *string    { return &value }
func boolPtr(value bool) *bool          { return &value }

func TestAlertRulesCRUDAndValidation(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	rules := NewRules(db)
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	db.now = func() time.Time { return clock }

	rule, err := rules.Create(ctx, Rule{
		Name:       "CPU high",
		MachineID:  "m-1",
		Metric:     "cpu.usage",
		Condition:  ConditionAbove,
		Threshold:  float64Ptr(90),
		DurationMs: 60_000,
		Enabled:    true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if rule.ID == "" || rule.State != AlertStateOK || !rule.CreatedAt.Equal(clock) {
		t.Fatalf("created rule = %+v", rule)
	}
	got, err := rules.Get(ctx, rule.ID)
	if err != nil || got.Name != "CPU high" || got.Threshold == nil || *got.Threshold != 90 {
		t.Fatalf("get rule = %+v, %v", got, err)
	}
	if _, err := rules.Create(ctx, Rule{
		Name: "No threshold", MachineID: "m-1", Metric: "cpu.usage", Condition: ConditionAbove, Enabled: true,
	}); CodeOf(err) != CodeInvalid {
		t.Fatalf("missing threshold error = %v", err)
	}
	if _, err := rules.Create(ctx, Rule{
		Name: "Bad metric", MachineID: "m-1", Metric: "nope", Condition: ConditionUnavailable, Enabled: true,
	}); CodeOf(err) != CodeInvalid {
		t.Fatalf("unknown metric error = %v", err)
	}
	if _, err := rules.Create(ctx, Rule{
		Name: "Threshold on unavailable", MachineID: "m-1", Metric: "cpu.usage",
		Condition: ConditionUnavailable, Threshold: float64Ptr(1), Enabled: true,
	}); CodeOf(err) != CodeInvalid {
		t.Fatalf("unavailable threshold error = %v", err)
	}
	if _, err := rules.Create(ctx, Rule{
		Name: "Duration", MachineID: "m-1", Metric: "cpu.usage", Condition: ConditionAbove,
		Threshold: float64Ptr(1), DurationMs: -1, Enabled: true,
	}); CodeOf(err) != CodeInvalid {
		t.Fatalf("negative duration error = %v", err)
	}

	rule.State = AlertStateFiring
	rule.LastValue = float64Ptr(95)
	rule.LastObservedAt = &clock
	if err := rules.Update(ctx, rule); err != nil {
		t.Fatalf("update rule: %v", err)
	}
	got, _ = rules.Get(ctx, rule.ID)
	if got.State != AlertStateFiring || got.LastObservedAt == nil || !got.LastObservedAt.Equal(clock) {
		t.Fatalf("updated rule = %+v", got)
	}
	if _, err := rules.Get(ctx, "missing"); CodeOf(err) != CodeNotFound {
		t.Fatalf("missing rule error = %v", err)
	}
	if err := rules.Update(ctx, Rule{ID: "missing", Name: "x", MachineID: "m-1", Metric: "cpu.usage", Condition: ConditionAbove, Threshold: float64Ptr(1), State: AlertStateOK}); CodeOf(err) != CodeNotFound {
		t.Fatalf("update missing error = %v", err)
	}
	listed, err := rules.List(ctx, "m-1")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	if listed, _ := rules.List(ctx, "other"); len(listed) != 0 {
		t.Fatalf("list other = %+v", listed)
	}
	if err := rules.Delete(ctx, rule.ID); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	if err := rules.Delete(ctx, rule.ID); CodeOf(err) != CodeNotFound {
		t.Fatalf("second delete error = %v", err)
	}
}

func TestAlertIncidentsOneOpenPerRule(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	rules := NewRules(db)
	incidents := NewIncidents(db)
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	db.now = func() time.Time { return clock }
	rule, err := rules.Create(ctx, Rule{
		Name: "Memory high", MachineID: "m-1", Metric: "memory.usage",
		Condition: ConditionAbove, Threshold: float64Ptr(90), Enabled: true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	base := Incident{
		RuleID: rule.ID, RuleName: rule.Name, MachineID: "m-1", Metric: "memory.usage",
		Condition: ConditionAbove, Status: IncidentOpen, StartedAt: clock,
		StartedValue: float64Ptr(95), Message: "memory above 90%",
	}
	open, err := incidents.Create(ctx, base)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	if _, err := incidents.Create(ctx, base); CodeOf(err) != CodeConflict {
		t.Fatalf("second open incident error = %v", err)
	}
	got, err := incidents.Get(ctx, open.ID)
	if err != nil || got.Status != IncidentOpen || got.StartedValue == nil || *got.StartedValue != 95 {
		t.Fatalf("get incident = %+v, %v", got, err)
	}
	listed, err := incidents.List(ctx, IncidentFilter{MachineID: "m-1", Status: IncidentOpen})
	if err != nil || len(listed) != 1 {
		t.Fatalf("list open = %+v, %v", listed, err)
	}
	// Resolving releases the partial index, so a new incident may open.
	resolvedAt := clock.Add(time.Hour)
	got.Status = IncidentRecovered
	got.ResolvedAt = &resolvedAt
	got.ResolvedValue = float64Ptr(40)
	got.Message = "memory recovered"
	if err := incidents.Update(ctx, got); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := incidents.Create(ctx, base); err != nil {
		t.Fatalf("open a second incident after recovery: %v", err)
	}
	recovered, err := incidents.List(ctx, IncidentFilter{RuleID: rule.ID, Status: IncidentRecovered, Limit: 10})
	if err != nil || len(recovered) != 1 || recovered[0].ResolvedValue == nil {
		t.Fatalf("recovered list = %+v, %v", recovered, err)
	}
	if _, err := incidents.List(ctx, IncidentFilter{Limit: MaxIncidentPage + 1}); CodeOf(err) != CodeInvalid {
		t.Fatalf("limit error = %v", err)
	}
	if err := incidents.Delete(ctx, open.ID); err != nil {
		t.Fatalf("delete incident: %v", err)
	}
}

func TestAlertWebhookSingleRow(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	webhooks := NewWebhooks(db)
	webhook, err := webhooks.Get(ctx)
	if err != nil || webhook != nil {
		t.Fatalf("initial webhook = %+v, %v", webhook, err)
	}
	config := Webhook{
		URL:     "https://example.invalid/hooks/rigscope",
		Headers: map[string]string{"X-Token": "secret-header"},
		Secret:  stringPtr("hmac-secret"),
		Enabled: true,
	}
	if err := webhooks.Set(ctx, config); err != nil {
		t.Fatalf("set webhook: %v", err)
	}
	stored, err := webhooks.Get(ctx)
	if err != nil || stored == nil {
		t.Fatalf("get webhook = %+v, %v", stored, err)
	}
	if stored.URL != config.URL || stored.Headers["X-Token"] != "secret-header" || stored.Secret == nil || *stored.Secret != "hmac-secret" || !stored.Enabled {
		t.Fatalf("stored webhook = %+v", stored)
	}
	// Replacing keeps exactly one row.
	config.URL = "http://127.0.0.1:9/sink"
	config.Enabled = false
	if err := webhooks.Set(ctx, config); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}
	var count int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM alert_webhook").Scan(&count); err != nil || count != 1 {
		t.Fatalf("webhook rows = %d, %v", count, err)
	}
	if err := webhooks.Set(ctx, Webhook{URL: "ftp://nope"}); CodeOf(err) != CodeInvalid {
		t.Fatalf("invalid URL error = %v", err)
	}
	if err := webhooks.Delete(ctx); err != nil {
		t.Fatalf("delete webhook: %v", err)
	}
	if webhook, err := webhooks.Get(ctx); err != nil || webhook != nil {
		t.Fatalf("webhook after delete = %+v, %v", webhook, err)
	}
}

func TestAlertSchemaIsComplete(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	for _, table := range []string{
		"alert_rules", "alert_incidents", "alert_jobs", "alert_webhook",
		"alert_machine_progress", "alert_observed_resources",
	} {
		var name string
		err := db.QueryRow(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
		if err != nil || name != table {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
	var index string
	if err := db.QueryRow(ctx, "SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'alert_incidents_one_open'").Scan(&index); err != nil {
		t.Fatalf("unique open index missing: %v", err)
	}
}

func TestRepositoriesSurviveReopen(t *testing.T) {
	ctx := testContext(t)
	path := privateTempDir(t) + "/hub.db"
	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	rules := NewRules(db)
	rule, err := rules.Create(ctx, Rule{
		Name: "Persisted", MachineID: "m-1", Metric: "cpu.usage",
		Condition: ConditionBelow, Threshold: float64Ptr(5), Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := NewWebhooks(db).Set(ctx, Webhook{URL: "https://example.invalid/hook", Enabled: true}); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := NewRules(reopened).Get(ctx, rule.ID)
	if err != nil || got.Name != "Persisted" || got.Threshold == nil || *got.Threshold != 5 {
		t.Fatalf("reopened rule = %+v, %v", got, err)
	}
	webhook, err := NewWebhooks(reopened).Get(ctx)
	if err != nil || webhook == nil || webhook.URL != "https://example.invalid/hook" {
		t.Fatalf("reopened webhook = %+v, %v", webhook, err)
	}
}
