package history_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/history"
	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
)

type fakeAuth struct {
	session live.Session
}

func (f *fakeAuth) Authenticate(*http.Request) (live.Session, error) { return f.session, nil }

type historyHub struct {
	t         *testing.T
	db        *storage.Database
	telemetry *storage.Telemetry
	state     *live.State
	auth      *fakeAuth
	handler   *history.Handler
	server    *httptest.Server
}

func newHistoryHub(t *testing.T) *historyHub {
	t.Helper()
	return newHistoryHubWithClock(t, nil)
}

func newHistoryHubWithClock(t *testing.T, now func() time.Time) *historyHub {
	return newHistoryHubWithMode(t, now, false)
}

func newHistoryHubWithMode(t *testing.T, now func() time.Time, memory bool) *historyHub {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	telemetry, err := storage.NewTelemetry(ctx, db, storage.TelemetryOptions{Memory: memory, FlushInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("open telemetry: %v", err)
	}
	state := live.NewState("hub-run-1", nil)
	if err := state.RegisterMachine("c-1", "m-1"); err != nil {
		t.Fatalf("register machine: %v", err)
	}
	auth := &fakeAuth{session: live.Session{
		ID:       "session-1",
		Username: "demo",
		Scope:    api.MachineScope{All: true},
	}}
	handler := history.New(history.Options{
		Auth:            auth,
		State:           state,
		Telemetry:       telemetry,
		FlushIntervalMS: 20,
		Now:             now,
	})
	server := httptest.NewServer(handler)
	hub := &historyHub{t: t, db: db, telemetry: telemetry, state: state, auth: auth, handler: handler, server: server}
	t.Cleanup(func() {
		server.Close()
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = telemetry.Close(closeCtx)
		_ = db.Close()
	})
	return hub
}

func TestMemoryHistoryStatusReportsVolatileBackend(t *testing.T) {
	hub := newHistoryHubWithMode(t, nil, true)
	response, body := hub.get(t, "/api/history/status")
	status, err := api.DecodeHistoryStatus(body)
	if response.StatusCode != http.StatusOK || err != nil {
		t.Fatalf("status: %s, %v", body, err)
	}
	if status.Mode != api.HistoryModeMemory || status.PersistentHistory || status.Persistence != nil || status.Memory == nil {
		t.Fatalf("memory status: %+v", status)
	}
	if status.Memory.BudgetBytes != storage.DefaultMemoryDatabaseBytes+hub.telemetry.RingStats().BudgetBytes {
		t.Fatalf("budget: %+v", status.Memory)
	}
	if status.AvailableFromMs != nil || status.AvailableToMs != nil {
		t.Fatal("empty RAM backend advertises history")
	}
	now := time.Now().UTC()
	record := sampleMetrics(t, "c-1", "run-1", "boot-1", 1, now, 42.5)
	if _, err := hub.telemetry.Enqueue(context.Background(), storage.Batch{MachineID: "m-1", CollectorID: "c-1", Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{record}}); err != nil {
		t.Fatal(err)
	}
	_, body = hub.get(t, "/api/history/status")
	status, err = api.DecodeHistoryStatus(body)
	if err != nil || status.AcceptedRecords != 1 || status.Memory.OldestMs == nil || status.Memory.NewestMs == nil {
		t.Fatalf("populated status: %s, %v", body, err)
	}
}

func (h *historyHub) get(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	response, err := http.Get(h.server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	var buffer []byte
	buffer = make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := response.Body.Read(chunk)
		buffer = append(buffer, chunk[:n]...)
		if err != nil {
			break
		}
	}
	return response, buffer
}

func sampleMetrics(t *testing.T, collectorID, runID, bootID string, sequence int64, capturedAt time.Time, usage float64) protocol.MetricsRecord {
	t.Helper()
	path := filepath.Join("..", "..", "..", "testdata", "protocol", "valid", "collector-samples-live-full.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	message, err := protocol.DecodeCollectorMessage(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	samples := message.(protocol.SamplesMessage)
	for _, record := range samples.Records {
		metrics, ok := record.(protocol.MetricsRecord)
		if !ok {
			continue
		}
		metrics.CollectorID = collectorID
		metrics.RunID = runID
		metrics.BootID = bootID
		metrics.Sequence = sequence
		metrics.CapturedAt = capturedAt.UTC().Format("2006-01-02T15:04:05.000Z")
		cpu := metrics.Payload.CPU.(protocol.CpuAvailable)
		cpu.UsagePercent = usage
		metrics.Payload.CPU = cpu
		return metrics
	}
	t.Fatalf("fixture has no metrics record")
	return protocol.MetricsRecord{}
}

func (h *historyHub) seed(t *testing.T, at time.Time, usage float64) {
	t.Helper()
	interval := int64(1000)
	hostname := "rig-1"
	_, err := h.telemetry.Enqueue(context.Background(), storage.Batch{
		MachineID:          "m-1",
		CollectorID:        "c-1",
		Mode:               protocol.SampleModeLive,
		ExpectedIntervalMs: &interval,
		Hostname:           &hostname,
		Records: []protocol.TelemetryRecord{
			sampleMetrics(t, "c-1", "run-1", "boot-1", 1, at, usage),
		},
	})
	if err != nil {
		t.Fatalf("enqueue sample: %v", err)
	}
}

func TestHistoryQueries(t *testing.T) {
	hub := newHistoryHub(t)
	at := time.Now().UTC().Add(-2 * time.Second)
	hub.seed(t, at, 42.5)

	response, body := hub.get(t, "/api/history/metrics?machineId=m-1")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", response.StatusCode, body)
	}
	catalog, err := api.DecodeMetricCatalogResponse(body)
	if err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(catalog.Metrics) != len(api.KnownMetrics()) {
		t.Fatalf("catalog size = %d", len(catalog.Metrics))
	}
	if catalog.Metrics["cpu.usage"].Unit != api.MetricUnitPercent {
		t.Fatalf("cpu.usage unit = %q", catalog.Metrics["cpu.usage"].Unit)
	}

	// The ring can answer only a range it fully covers, and this test's ring
	// starts at the seeded sample; ask from there so the 1s tier is eligible.
	from := at.UnixMilli()
	to := at.Add(2 * time.Second).UnixMilli()
	path := fmt.Sprintf("/api/history?machineId=m-1&metrics=cpu.usage,memory.used&from=%d&to=%d", from, to)
	response, body = hub.get(t, path)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("history status = %d: %s", response.StatusCode, body)
	}
	result, err := api.DecodeHistoryResult(body)
	if err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if result.Tier != api.HistoryTierOneSecond {
		t.Fatalf("tier = %q", result.Tier)
	}
	if result.StepMs != 1000 {
		t.Fatalf("stepMs = %d", result.StepMs)
	}
	var found bool
	for _, series := range result.Series {
		if series.Metric != "cpu.usage" || series.ResourceID != "" {
			continue
		}
		found = true
		if len(series.Points) == 0 || series.Points[0].Avg == nil || *series.Points[0].Avg != 42.5 {
			t.Fatalf("cpu.usage points = %+v", series.Points)
		}
	}
	if !found {
		t.Fatalf("cpu.usage series missing: %+v", result.Series)
	}

	// The status document names the persistent backend and its tiers.
	response, body = hub.get(t, "/api/history/status")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	status, err := api.DecodeHistoryStatus(body)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Mode != api.HistoryModePersistent || status.Persistence == nil || status.Memory != nil {
		t.Fatalf("status = %+v", status)
	}
	if status.Persistence.StorageTier != "summary" {
		t.Fatalf("storage tier = %q", status.Persistence.StorageTier)
	}
	if status.AcceptedRecords != 1 {
		t.Fatalf("acceptedRecords = %d", status.AcceptedRecords)
	}
	if len(status.Persistence.Tiers) != 4 || status.Persistence.Tiers[0].Tier != api.HistoryTierOneSecond {
		t.Fatalf("tiers = %+v", status.Persistence.Tiers)
	}
}

// TestHistoryCoreIDsPreserveAbsentVersusEmpty is the HTTP-level regression for
// the CoreIDs pointer: absent coreIds means "all cores", while an explicitly
// empty coreIds means "no cpu.core series".
func TestHistoryCoreIDsPreserveAbsentVersusEmpty(t *testing.T) {
	hub := newHistoryHub(t)
	at := time.Now().UTC().Add(-2 * time.Second)
	hub.seed(t, at, 42.5)
	from := at.UnixMilli()
	to := at.Add(2 * time.Second).UnixMilli()
	base := fmt.Sprintf("/api/history?machineId=m-1&metrics=cpu.core,memory.used&from=%d&to=%d", from, to)

	// Absent: every core series is eligible.
	response, body := hub.get(t, base)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("absent coreIds status = %d: %s", response.StatusCode, body)
	}
	all, err := api.DecodeHistoryResult(body)
	if err != nil {
		t.Fatalf("decode absent coreIds: %v", err)
	}
	cores := 0
	memory := false
	for _, series := range all.Series {
		switch series.Metric {
		case "cpu.core":
			cores++
		case "memory.used":
			memory = true
		}
	}
	if cores == 0 {
		t.Fatalf("absent coreIds dropped every cpu.core series: %+v", all.Series)
	}
	if !memory {
		t.Fatalf("absent coreIds dropped the machine-level series: %+v", all.Series)
	}

	// Explicitly empty: no cpu.core series, other families untouched.
	response, body = hub.get(t, base+"&coreIds=")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("empty coreIds status = %d: %s", response.StatusCode, body)
	}
	none, err := api.DecodeHistoryResult(body)
	if err != nil {
		t.Fatalf("decode empty coreIds: %v", err)
	}
	for _, series := range none.Series {
		if series.Metric == "cpu.core" {
			t.Fatalf("explicitly empty coreIds returned %q", series.ResourceID)
		}
	}
	foundMemory := false
	for _, series := range none.Series {
		if series.Metric == "memory.used" {
			foundMemory = true
		}
	}
	if !foundMemory {
		t.Fatalf("explicitly empty coreIds dropped non-core series: %+v", none.Series)
	}

	// A non-empty selection still narrows to the requested core.
	response, body = hub.get(t, base+"&coreIds=core:0")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("selected coreIds status = %d: %s", response.StatusCode, body)
	}
	selected, err := api.DecodeHistoryResult(body)
	if err != nil {
		t.Fatalf("decode selected coreIds: %v", err)
	}
	selectedCores := 0
	for _, series := range selected.Series {
		if series.Metric != "cpu.core" {
			continue
		}
		selectedCores++
		if series.ResourceID != "core:0" {
			t.Fatalf("selected coreIds returned %q", series.ResourceID)
		}
	}
	if selectedCores != 1 {
		t.Fatalf("selected coreIds series = %d, want 1", selectedCores)
	}
}

func TestHistoryQueryErrors(t *testing.T) {
	hub := newHistoryHub(t)
	at := time.Now().UTC()
	from := at.Add(-time.Minute).UnixMilli()
	to := at.UnixMilli()

	// A scoped session cannot see another machine.
	hub.auth.session.Scope = api.MachineScope{IDs: []string{"m-1"}}
	response, body := hub.get(t, "/api/history?machineId=m-other&metrics=cpu.usage&from="+itoa(from)+"&to="+itoa(to))
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("out-of-scope history status = %d: %s", response.StatusCode, body)
	}
	response, body = hub.get(t, "/api/history/metrics?machineId=m-other")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("out-of-scope metrics status = %d: %s", response.StatusCode, body)
	}
	hub.auth.session.Scope = api.MachineScope{All: true}

	// In scope but unknown to the live state is 404.
	for _, path := range []string{
		"/api/history?machineId=m-missing&metrics=cpu.usage&from=" + itoa(from) + "&to=" + itoa(to),
		"/api/history/metrics?machineId=m-missing",
	} {
		response, body := hub.get(t, path)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d: %s", path, response.StatusCode, body)
		}
	}

	// Once the machine exists, query validation applies.
	hub.state.SetInventory("m-1", protocol.Inventory{})
	bad := []string{
		fmt.Sprintf("/api/history?machineId=m-1&metrics=nope&from=%d&to=%d", from, to),
		fmt.Sprintf("/api/history?machineId=m-1&metrics=cpu.usage&from=%d&to=%d", to, from),
		fmt.Sprintf("/api/history?machineId=m-1&metrics=cpu.usage&from=%d&to=%d&maxPoints=0", from, to),
	}
	for _, path := range bad {
		response, body := hub.get(t, path)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d: %s", path, response.StatusCode, body)
		}
	}

	response, body = hub.get(t, "/api/history/status")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status after empty history = %d: %s", response.StatusCode, body)
	}
	status, err := api.DecodeHistoryStatus(body)
	if err != nil {
		t.Fatalf("decode empty status: %v", err)
	}
	if status.AvailableFromMs == nil || status.AvailableToMs == nil {
		t.Fatalf("available window missing: %+v", status)
	}

	request, _ := http.NewRequest(http.MethodPost, hub.server.URL+"/api/history/status", nil)
	postResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post status: %v", err)
	}
	defer postResponse.Body.Close()
	if postResponse.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("post status code = %d", postResponse.StatusCode)
	}
}

func TestHistoryRateLimit(t *testing.T) {
	// A frozen clock keeps the per-session refill from hiding the over-budget
	// request when the race detector makes eight scans take longer than the
	// 250 ms refill interval.
	frozen := time.Now().UTC()
	hub := newHistoryHubWithClock(t, func() time.Time { return frozen })
	hub.auth.session.ID = "rate-session"
	at := time.Now().UTC()
	hub.seed(t, at, 10)
	from := at.Add(-time.Minute).UnixMilli()
	to := at.Add(time.Minute).UnixMilli()
	path := fmt.Sprintf("/api/history?machineId=m-1&metrics=cpu.usage&from=%d&to=%d", from, to)
	for index := 0; index < int(history.QueryBudgetTokens); index++ {
		response, body := hub.get(t, path)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d: %s", index, response.StatusCode, body)
		}
	}
	response, body := hub.get(t, path)
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-budget status = %d: %s", response.StatusCode, body)
	}
	if string(body) == "" {
		t.Fatalf("missing rate limit message")
	}
}

func itoa(value int64) string { return fmt.Sprintf("%d", value) }
