package ingest_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/alerts"
	"github.com/Triet1402/rigscope/internal/hub/ingest"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
)

// TestLiveIngestionFeedsAlertEngine drives the real collector gateway and
// verifies that only published LIVE records reach the alert engine: an incident
// opens and its webhook is delivered, while a replay batch leaves state alone.
func TestLiveIngestionFeedsAlertEngine(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-alerts", MachineID: "m-alerts", Token: "token-alerts"}
	var engine *alerts.Engine
	hub := newTestHubWith(t, identity, 20*time.Millisecond, func(options *ingest.GatewayOptions) {
		options.OnLiveSample = func(machineID string, record protocol.MetricsRecord, receivedAt time.Time) {
			if engine != nil {
				engine.ObserveAt(machineID, record, alerts.Live, receivedAt)
			}
		}
	})

	deliveries := make(chan string, 8)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case deliveries <- string(body):
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)

	var err error
	engine, err = alerts.New(alerts.Options{Database: hub.db})
	if err != nil {
		t.Fatalf("create alert engine: %v", err)
	}
	t.Cleanup(engine.Stop)
	authority := wiringAuthority(t, hub.db)
	if _, err := engine.ConfigureWebhook(context.Background(), authority, alerts.WebhookConfig{URL: sink.URL}); err != nil {
		t.Fatalf("configure webhook: %v", err)
	}
	if _, err := engine.CreateRule(context.Background(), authority, alerts.RuleInput{
		Name: "CPU high", MachineID: identity.MachineID, Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: float64Ptr(80), DurationMs: 0,
	}); err != nil {
		t.Fatalf("create CPU rule: %v", err)
	}
	if _, err := engine.CreateRule(context.Background(), authority, alerts.RuleInput{
		Name: "Memory high", MachineID: identity.MachineID, Metric: api.AlertMetricMemoryUsage,
		Condition: api.AlertConditionAbove, Threshold: float64Ptr(90), DurationMs: 0,
	}); err != nil {
		t.Fatalf("create memory rule: %v", err)
	}

	conn := hub.dial(identity.Token)
	defer conn.CloseNow()
	send(t, conn, hello(identity.CollectorID, "run-alerts", "boot-alerts", versionPtr(1)))
	readHub(t, conn)

	// A published LIVE sample opens an incident and delivers its webhook.
	cpuHigh := metricsRecord(t, identity.CollectorID, "run-alerts", "boot-alerts", 1, time.Now().UTC())
	cpuHigh.Payload.CPU = protocol.CpuAvailable{
		UsagePercent: 95,
		PerCore:      []*float64{float64Ptr(95), float64Ptr(95)},
	}
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{cpuHigh}})
	readAck(t, conn)
	waitDelivery(t, deliveries, `"name":"CPU high"`, `"event":"fired"`)

	// Replay is history: it must not reach the alert engine or open a second
	// incident for the memory rule.
	replay := metricsRecord(t, identity.CollectorID, "run-old", "boot-old", 50, time.Now().UTC().Add(-time.Minute))
	replay.Payload.Memory = protocol.MemoryAvailable{UsedBytes: 15, TotalBytes: 16}
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeReplay, Records: []protocol.TelemetryRecord{replay}})
	readAck(t, conn)
	select {
	case body := <-deliveries:
		t.Fatalf("replay produced a webhook delivery: %s", body)
	case <-time.After(300 * time.Millisecond):
	}
	incidents, err := engine.ListIncidents(context.Background(), alerts.AllMachines(), 200)
	if err != nil {
		t.Fatalf("list incidents: %v", err)
	}
	if len(incidents) != 1 {
		t.Fatalf("replay changed incidents: %+v", incidents)
	}

	// The same condition delivered as LIVE opens the memory incident.
	memoryHigh := metricsRecord(t, identity.CollectorID, "run-alerts", "boot-alerts", 2, time.Now().UTC())
	memoryHigh.Payload.Memory = protocol.MemoryAvailable{UsedBytes: 15, TotalBytes: 16}
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{memoryHigh}})
	readAck(t, conn)
	waitDelivery(t, deliveries, `"name":"Memory high"`, `"event":"fired"`)

	incidents, err = engine.ListIncidents(context.Background(), alerts.AllMachines(), 200)
	if err != nil {
		t.Fatalf("list incidents: %v", err)
	}
	if len(incidents) != 2 {
		t.Fatalf("incidents = %+v", incidents)
	}
}

func waitDelivery(t *testing.T, deliveries <-chan string, needles ...string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case body := <-deliveries:
			matched := true
			for _, needle := range needles {
				if !strings.Contains(body, needle) {
					matched = false
					break
				}
			}
			if matched {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for webhook delivery containing %v", needles)
		}
	}
}

func float64Ptr(value float64) *float64 { return &value }

// wiringAuthority creates the administrator session that alert mutations
// re-check inside their write transaction.
func wiringAuthority(t *testing.T, db *storage.Database) storage.SessionAuthority {
	t.Helper()
	ctx := context.Background()
	users := storage.NewUsers(db)
	const username = "alerts-wiring-admin"
	if err := users.Create(ctx, "test", storage.User{
		Username: username, PasswordHash: "test:wiring-password",
		Scope: storage.AllMachines(), Admin: true,
	}); err != nil {
		t.Fatalf("create wiring admin: %v", err)
	}
	session := storage.Session{
		TokenHash: "alerts-wiring-session",
		Username:  username,
		CSRFToken: "alerts-wiring-csrf",
		CreatedAt: time.Unix(0, 0),
		ExpiresAt: time.Now().Add(100 * 365 * 24 * time.Hour),
	}
	if err := storage.NewSessions(db).Create(ctx, session); err != nil {
		t.Fatalf("create wiring session: %v", err)
	}
	return storage.SessionAuthority{Actor: username, TokenHash: session.TokenHash}
}
