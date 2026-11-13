package alerts

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
)

const (
	fixtureBootID      = "00000000-0000-4000-8000-000000000001"
	fixtureRunID       = "run-alert"
	fixtureCollectorID = "collector-alert"
)

func floatPtr(value float64) *float64 { return &value }
func int64Ptr(value int64) *int64     { return &value }
func stringPtr(value string) *string  { return &value }

func basePayload() protocol.MetricsPayload {
	return protocol.MetricsPayload{
		CPU: protocol.CpuAvailable{
			UsagePercent: 50,
			PerCore:      []*float64{floatPtr(50), floatPtr(50)},
		},
		Memory: protocol.MemoryAvailable{UsedBytes: 8, TotalBytes: 16},
		Swap:   protocol.BytesUsageAvailable{UsedBytes: 2, TotalBytes: 4},
		GPUs: protocol.GpusAvailable{Devices: []protocol.GpuReading{
			protocol.GpuAvailable{
				ID:                 "gpu:GPU-1",
				UtilizationPercent: floatPtr(40),
				MemoryUsedBytes:    int64Ptr(100),
				MemoryTotalBytes:   int64Ptr(200),
				TemperatureC:       floatPtr(60),
				PowerWatts:         floatPtr(100),
			},
		}},
		Disks: protocol.DisksAvailable{
			Filesystems: []protocol.FilesystemReading{
				protocol.FilesystemAvailable{ID: "fs:/", UsedBytes: 50, TotalBytes: 100},
			},
			Devices: []protocol.DiskIoReading{
				protocol.DiskIoAvailable{ID: "disk:sda", ReadBytesPerSec: 1, WriteBytesPerSec: 2},
			},
		},
		Network: protocol.NetworkAvailable{Interfaces: []protocol.InterfaceReading{
			protocol.InterfaceAvailable{ID: "net:eno1", RxBytesPerSec: 3, TxBytesPerSec: 4},
		}},
	}
}

func cpuPayload(usage float64) protocol.MetricsPayload {
	payload := basePayload()
	payload.CPU = protocol.CpuAvailable{UsagePercent: usage, PerCore: []*float64{floatPtr(usage), floatPtr(usage)}}
	return payload
}

func cpuUnavailablePayload(reason string) protocol.MetricsPayload {
	payload := basePayload()
	payload.CPU = protocol.Unavailable{Reason: reason}
	return payload
}

func metricsRecord(payload protocol.MetricsPayload, sequence int64) protocol.MetricsRecord {
	return protocol.MetricsRecord{
		RecordEnvelope: protocol.RecordEnvelope{
			CollectorID: fixtureCollectorID,
			RunID:       fixtureRunID,
			BootID:      fixtureBootID,
			Sequence:    sequence,
			CapturedAt:  "1970-01-01T00:00:00.000Z",
		},
		Payload: payload,
	}
}

func machineState(machineID string, lastReceived *time.Time, expectedIntervalMs *int64, metrics *MachineMetrics) MachineState {
	return MachineState{
		MachineID:          machineID,
		LastReceivedAt:     lastReceived,
		ExpectedIntervalMs: expectedIntervalMs,
		Metrics:            metrics,
	}
}

// testClock is safe to read from the engine's background delivery goroutine
// while the test advances it.
type testClock struct {
	mu sync.Mutex
	ms int64
}

func newTestClock(ms int64) *testClock { return &testClock{ms: ms} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.UnixMilli(c.ms).UTC()
}

func (c *testClock) Advance(ms int64) {
	c.mu.Lock()
	c.ms += ms
	c.mu.Unlock()
}

func (c *testClock) Set(ms int64) {
	c.mu.Lock()
	c.ms = ms
	c.mu.Unlock()
}

type recordingSender struct {
	mu      sync.Mutex
	calls   []WebhookRequest
	result  func(WebhookRequest) WebhookResult
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *recordingSender) Send(ctx context.Context, request WebhookRequest) WebhookResult {
	s.mu.Lock()
	s.calls = append(s.calls, request)
	result := s.result
	s.mu.Unlock()
	if s.started != nil {
		s.once.Do(func() { close(s.started) })
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
		}
	}
	if result != nil {
		return result(request)
	}
	status := 200
	return WebhookResult{OK: true, Status: &status}
}

func (s *recordingSender) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *recordingSender) Calls() []WebhookRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]WebhookRequest(nil), s.calls...)
}

type harness struct {
	t         *testing.T
	db        *storage.Database
	engine    *Engine
	clock     *testClock
	sender    *recordingSender
	authority storage.SessionAuthority
	seq       int64
}

// testAuthority creates an enabled administrator and one durable session that
// engine-level mutation tests can pass to the write APIs. The session row is
// what each mutation transaction re-checks.
func testAuthority(t *testing.T, db *storage.Database) storage.SessionAuthority {
	t.Helper()
	ctx := context.Background()
	users := storage.NewUsers(db)
	const username = "alerts-test-admin"
	if err := users.Create(ctx, "test", storage.User{
		Username: username, PasswordHash: "test:alerts-password",
		Scope: storage.AllMachines(), Admin: true,
	}); err != nil {
		t.Fatalf("create alert test admin: %v", err)
	}
	session := storage.Session{
		TokenHash: "alerts-test-session",
		Username:  username,
		CSRFToken: "alerts-test-csrf",
		CreatedAt: time.Unix(0, 0),
		// The engine under test may use a synthetic clock far from the wall
		// clock; a deliberately distant expiry keeps authorization valid.
		ExpiresAt: time.Now().Add(100 * 365 * 24 * time.Hour),
	}
	if err := storage.NewSessions(db).Create(ctx, session); err != nil {
		t.Fatalf("create alert test session: %v", err)
	}
	return storage.SessionAuthority{Actor: username, TokenHash: session.TokenHash}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := storage.Open(context.Background(), ":memory:", storage.Options{})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	clock := newTestClock(time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC).UnixMilli())
	sender := &recordingSender{}
	engine, err := New(Options{Database: db, Now: clock.Now, Sender: sender})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	authority := testAuthority(t, db)
	if _, err := engine.ConfigureWebhook(context.Background(), authority, WebhookConfig{URL: "http://127.0.0.1:9/alerts-sink"}); err != nil {
		t.Fatalf("configure webhook: %v", err)
	}
	h := &harness{t: t, db: db, engine: engine, clock: clock, sender: sender, authority: authority}
	t.Cleanup(func() {
		engine.Stop()
		_ = db.Close()
	})
	return h
}

func (h *harness) record(payload protocol.MetricsPayload) protocol.MetricsRecord {
	h.seq++
	return metricsRecord(payload, h.seq)
}

func (h *harness) observe(machineID string, payload protocol.MetricsPayload) {
	h.engine.ObserveAt(machineID, h.record(payload), Live, h.clock.Now())
}

func (h *harness) process() {
	h.t.Helper()
	if err := h.engine.ProcessNotifications(context.Background()); err != nil {
		h.t.Fatalf("process notifications: %v", err)
	}
}

func (h *harness) rule(name string, machineID string, metric api.AlertMetric, condition api.AlertCondition, threshold *float64, durationMs int64) string {
	h.t.Helper()
	rule, err := h.engine.CreateRule(context.Background(), h.authority, RuleInput{
		Name:       name,
		MachineID:  machineID,
		Metric:     metric,
		Condition:  condition,
		Threshold:  threshold,
		DurationMs: durationMs,
	})
	if err != nil {
		h.t.Fatalf("create rule: %v", err)
	}
	return rule.ID
}
