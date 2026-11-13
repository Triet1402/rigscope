package live_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/protocol"
)

type fakeAuth struct {
	mu      sync.Mutex
	session live.Session
	err     error
}

func (f *fakeAuth) Authenticate(*http.Request) (live.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return live.Session{}, f.err
	}
	return f.session, nil
}

func newSession() (live.Session, chan struct{}) {
	revoked := make(chan struct{})
	return live.Session{
		ID:       "session-1",
		Username: "demo",
		Scope:    api.MachineScope{All: true},
		Revoked:  revoked,
	}, revoked
}

func newState(t *testing.T) (*live.State, live.SessionInfo) {
	t.Helper()
	state := live.NewState("hub-run-1", nil)
	if err := state.RegisterMachine("c-1", "m-1"); err != nil {
		t.Fatalf("register machine: %v", err)
	}
	result, err := state.StartSession(live.StartSessionInput{
		SessionID:          "collector-session-1",
		CollectorID:        "c-1",
		RunID:              "run-1",
		BootID:             "boot-1",
		Hostname:           "rig-1",
		ExpectedIntervalMS: 1000,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	return state, result.Session
}

func metricsRecord(collectorID, runID, bootID string, sequence int64, capturedAt time.Time) protocol.MetricsRecord {
	temperature := &protocol.Optional[float64]{Valid: true, Value: 61.5}
	pressure := &protocol.Optional[float64]{Valid: false}
	return protocol.MetricsRecord{
		RecordEnvelope: protocol.RecordEnvelope{
			CollectorID: collectorID,
			RunID:       runID,
			BootID:      bootID,
			Sequence:    sequence,
			CapturedAt:  capturedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		},
		Payload: protocol.MetricsPayload{
			CPU: protocol.CpuAvailable{
				UsagePercent:        12.5,
				PerCore:             []*float64{floatPtr(10), floatPtr(15)},
				TemperatureC:        temperature,
				PressureSomePercent: pressure,
			},
			Memory: protocol.MemoryAvailable{UsedBytes: 4_000, TotalBytes: 8_000},
			Swap:   protocol.BytesUsageAvailable{UsedBytes: 0, TotalBytes: 0},
			GPUs:   protocol.GpusAvailable{Devices: []protocol.GpuReading{}},
			Disks: protocol.DisksAvailable{
				Filesystems: []protocol.FilesystemReading{},
				Devices:     []protocol.DiskIoReading{},
			},
			Network: protocol.NetworkAvailable{Interfaces: []protocol.InterfaceReading{}},
		},
	}
}

func floatPtr(value float64) *float64 { return &value }

func TestMachineTelemetryAdaptsLastReceiptAndPayload(t *testing.T) {
	state := live.NewState("hub-run-1", nil)
	if err := state.RegisterMachine("c-1", "m-1"); err != nil {
		t.Fatalf("register machine: %v", err)
	}
	if err := state.RegisterMachine("c-2", "m-2"); err != nil {
		t.Fatalf("register second machine: %v", err)
	}
	telemetry := state.MachineTelemetry()
	if len(telemetry) != 2 || telemetry[0].MachineID != "m-1" || telemetry[1].MachineID != "m-2" {
		t.Fatalf("telemetry = %+v", telemetry)
	}
	if telemetry[0].LastReceivedAt != nil || telemetry[0].Metrics != nil {
		t.Fatalf("a machine without a record must have no receipt or metrics: %+v", telemetry[0])
	}

	result, err := state.StartSession(live.StartSessionInput{
		SessionID:          "collector-session-1",
		CollectorID:        "c-1",
		RunID:              "run-1",
		BootID:             "boot-1",
		Hostname:           "rig-1",
		ExpectedIntervalMS: 1000,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	receivedAt := time.Now()
	record := metricsRecord("c-1", "run-1", "boot-1", 1, receivedAt)
	if !state.ApplyMetrics(result.Session.SessionID, record, receivedAt) {
		t.Fatalf("record must apply")
	}

	telemetry = state.MachineTelemetry()
	if len(telemetry) != 2 {
		t.Fatalf("telemetry = %+v", telemetry)
	}
	machine := telemetry[0]
	if machine.MachineID != "m-1" || machine.LastReceivedAt == nil || machine.MetricsReceivedAt == nil {
		t.Fatalf("telemetry = %+v", machine)
	}
	if machine.LastReceivedAt.UnixMilli() != receivedAt.UnixMilli() ||
		machine.MetricsReceivedAt.UnixMilli() != receivedAt.UnixMilli() {
		t.Fatalf("receipt times = %v, %v", machine.LastReceivedAt, machine.MetricsReceivedAt)
	}
	if machine.ExpectedIntervalMS == nil || *machine.ExpectedIntervalMS != 1000 {
		t.Fatalf("expected interval = %v", machine.ExpectedIntervalMS)
	}
	if machine.Metrics == nil {
		t.Fatalf("payload is nil")
	}
	cpu, ok := machine.Metrics.CPU.(protocol.CpuAvailable)
	if !ok || cpu.UsagePercent != 12.5 {
		t.Fatalf("payload CPU = %#v", machine.Metrics.CPU)
	}
}

func TestStateOrderingAndSessionSupersession(t *testing.T) {
	state, info := newState(t)
	now := time.Now()

	if !state.ApplyMetrics(info.SessionID, metricsRecord("c-1", "run-1", "boot-1", 2, now), now) {
		t.Fatalf("first record must apply")
	}
	if state.ApplyMetrics(info.SessionID, metricsRecord("c-1", "run-1", "boot-1", 2, now), now) {
		t.Fatalf("a duplicate sequence must not apply twice")
	}
	if state.ApplyMetrics(info.SessionID, metricsRecord("c-1", "run-1", "boot-1", 1, now), now) {
		t.Fatalf("a stale sequence must not apply")
	}
	if state.ApplyMetrics(info.SessionID, metricsRecord("c-1", "run-other", "boot-1", 3, now), now) {
		t.Fatalf("a foreign run must not apply to the session")
	}

	// A new run supersedes the session and clears its live readings.
	if _, err := state.StartSession(live.StartSessionInput{
		SessionID:          "collector-session-2",
		CollectorID:        "c-1",
		RunID:              "run-2",
		BootID:             "boot-1",
		Hostname:           "rig-1",
		ExpectedIntervalMS: 2000,
	}); err != nil {
		t.Fatalf("supersede session: %v", err)
	}
	snapshot := state.Snapshot()
	if len(snapshot.Machines) != 1 || snapshot.Machines[0].Metrics != nil {
		t.Fatalf("new run must clear the old live reading")
	}
	if snapshot.Machines[0].Connected != true {
		t.Fatalf("replacement session must be connected")
	}
	if state.ApplyMetrics(info.SessionID, metricsRecord("c-1", "run-1", "boot-1", 9, now), now) {
		t.Fatalf("a replaced session must not publish")
	}

	// Ending a session keeps the last readings and marks the machine offline.
	if !state.ApplyMetrics("collector-session-2", metricsRecord("c-1", "run-2", "boot-1", 3, now), now) {
		t.Fatalf("current session must publish")
	}
	state.EndSession("collector-session-2")
	snapshot = state.Snapshot()
	if snapshot.Machines[0].Connected {
		t.Fatalf("ended session must be disconnected")
	}
	if snapshot.Machines[0].Metrics == nil || snapshot.Machines[0].Metrics.Sequence != 3 {
		t.Fatalf("last readings must survive a disconnect")
	}
	if snapshot.Machines[0].LastReceivedAt == nil {
		t.Fatalf("lastReceivedAt must be preserved")
	}
}

func TestStartSessionReturnsExactPredecessor(t *testing.T) {
	state := live.NewState("hub-run-1", nil)
	if err := state.RegisterMachine("c-1", "m-1"); err != nil {
		t.Fatalf("register machine: %v", err)
	}
	first, err := state.StartSession(live.StartSessionInput{
		SessionID: "session-1", CollectorID: "c-1", RunID: "run-1", BootID: "boot-1",
		Hostname: "rig-1", ExpectedIntervalMS: 1000,
	})
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	if first.ReplacedSessionID != "" {
		t.Fatalf("first session replaced %q", first.ReplacedSessionID)
	}
	second, err := state.StartSession(live.StartSessionInput{
		SessionID: "session-2", CollectorID: "c-1", RunID: "run-2", BootID: "boot-1",
		Hostname: "rig-1", ExpectedIntervalMS: 1000,
	})
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	if second.ReplacedSessionID != "session-1" {
		t.Fatalf("second predecessor = %q, want session-1", second.ReplacedSessionID)
	}
	third, err := state.StartSession(live.StartSessionInput{
		SessionID: "session-3", CollectorID: "c-1", RunID: "run-3", BootID: "boot-1",
		Hostname: "rig-1", ExpectedIntervalMS: 1000,
	})
	if err != nil {
		t.Fatalf("third session: %v", err)
	}
	if third.ReplacedSessionID != "session-2" {
		t.Fatalf("third predecessor = %q, want session-2", third.ReplacedSessionID)
	}
	if _, ok := state.CurrentSession("session-3"); !ok {
		t.Fatalf("newest session is not current")
	}
}

func TestRemoveCollectorDropsLiveState(t *testing.T) {
	state, info := newState(t)
	now := time.Now()
	state.ApplyMetrics(info.SessionID, metricsRecord("c-1", "run-1", "boot-1", 1, now), now)
	state.SetInventory(info.MachineID, protocol.Inventory{OS: protocol.OSInventory{Platform: "linux"}})

	machineID, removed := state.RemoveCollector("c-1")
	if !removed || machineID != info.MachineID {
		t.Fatalf("removal = %q, %v", machineID, removed)
	}
	if _, ok := state.Machine(info.MachineID); ok {
		t.Fatalf("removed machine is still in live state")
	}
	if _, ok := state.CurrentSession(info.SessionID); ok {
		t.Fatalf("removed collector still holds a session")
	}
	if state.ConnectedCount() != 0 || state.ConfiguredCount() != 0 {
		t.Fatalf("state counts = %d/%d", state.ConnectedCount(), state.ConfiguredCount())
	}
	if _, removed := state.RemoveCollector("c-1"); removed {
		t.Fatalf("removal must be idempotent")
	}
}

type sseClient struct {
	t      *testing.T
	cancel context.CancelFunc
	body   io.ReadCloser
	reader *bufio.Reader
}

func openSSE(t *testing.T, url string, headers map[string]string) (*sseClient, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build SSE request: %v", err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	client := &http.Client{Timeout: 0}
	response, err := client.Do(request)
	if err != nil {
		cancel()
		t.Fatalf("open SSE: %v", err)
	}
	sse := &sseClient{t: t, cancel: cancel, body: response.Body, reader: bufio.NewReader(response.Body)}
	t.Cleanup(func() {
		cancel()
		_ = response.Body.Close()
	})
	return sse, response
}

// nextFrame reads one SSE block and returns its raw form.
func (c *sseClient) nextBlock() string {
	c.t.Helper()
	var block strings.Builder
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read SSE block: %v", err)
		}
		if line == "\n" {
			return block.String()
		}
		block.WriteString(line)
	}
}

func (c *sseClient) nextFrame() api.LiveFrame {
	c.t.Helper()
	for {
		block := c.nextBlock()
		frame, ok, err := api.DecodeLiveFrame([]byte(block))
		if err != nil {
			c.t.Fatalf("decode SSE frame %q: %v", block, err)
		}
		if ok {
			return frame
		}
	}
}

func TestLiveFeedFrames(t *testing.T) {
	state, info := newState(t)
	session, _ := newSession()
	feed := live.NewFeed(live.FeedOptions{
		Auth:      &fakeAuth{session: session},
		State:     state,
		Coalesce:  5 * time.Millisecond,
		Keepalive: 40 * time.Millisecond,
	})
	server := httptest.NewServer(feed)
	t.Cleanup(server.Close)
	t.Cleanup(feed.Stop)

	client, response := openSSE(t, server.URL, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	if got := response.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q", got)
	}
	first := client.nextFrame()
	if first.Event != api.LiveEventSnapshot {
		t.Fatalf("first event = %q", first.Event)
	}
	if first.ID.HubRunID != "hub-run-1" {
		t.Fatalf("event id hubRunId = %q", first.ID.HubRunID)
	}
	if len(first.Data.Machines) != 1 || first.Data.Machines[0].MachineID != "m-1" {
		t.Fatalf("initial snapshot machines = %+v", first.Data.Machines)
	}
	previousCursor := first.ID.Cursor

	// A state change produces one coalesced full update with a new cursor.
	now := time.Now()
	state.ApplyMetrics(info.SessionID, metricsRecord("c-1", "run-1", "boot-1", 1, now), now)
	update := client.nextFrame()
	if update.Event != api.LiveEventUpdate {
		t.Fatalf("second event = %q", update.Event)
	}
	if update.Data.Machines[0].Metrics == nil {
		t.Fatalf("update must carry the new metrics")
	}
	if update.ID.Cursor == previousCursor {
		t.Fatalf("cursor did not advance")
	}

	// Keepalives are comments, then an identical snapshot is skipped.
	state.SetMetadata("m-1", stringPtr("rig"), []string{"lab"})
	metadata := client.nextFrame()
	if metadata.Data.Machines[0].DisplayName == nil || !metadata.Data.Machines[0].DisplayName.Valid {
		t.Fatalf("metadata was not published")
	}

	// A reconnect with Last-Event-ID starts with a full reset snapshot.
	resetClient, _ := openSSE(t, server.URL, map[string]string{"Last-Event-ID": "hub-run-1:42"})
	reset := resetClient.nextFrame()
	if reset.Event != api.LiveEventReset {
		t.Fatalf("first event after Last-Event-ID = %q", reset.Event)
	}
	if len(reset.Data.Machines) != 1 {
		t.Fatalf("reset must carry a full snapshot")
	}
}

func stringPtr(value string) *string { return &value }

func TestLiveFeedScopeAndAuth(t *testing.T) {
	state, _ := newState(t)
	if err := state.RegisterMachine("c-2", "m-2"); err != nil {
		t.Fatalf("register second machine: %v", err)
	}
	session, _ := newSession()
	session.Scope = api.MachineScope{IDs: []string{"m-2"}}
	feed := live.NewFeed(live.FeedOptions{Auth: &fakeAuth{session: session}, State: state})
	server := httptest.NewServer(feed)
	t.Cleanup(server.Close)
	t.Cleanup(feed.Stop)

	client, _ := openSSE(t, server.URL, nil)
	frame := client.nextFrame()
	if len(frame.Data.Machines) != 1 || frame.Data.Machines[0].MachineID != "m-2" {
		t.Fatalf("scoped snapshot leaked another machine: %+v", frame.Data.Machines)
	}

	emptySession, _ := newSession()
	emptySession.Scope = api.MachineScope{IDs: []string{}}
	emptyFeed := live.NewFeed(live.FeedOptions{Auth: &fakeAuth{session: emptySession}, State: state})
	emptyServer := httptest.NewServer(emptyFeed)
	t.Cleanup(emptyServer.Close)
	t.Cleanup(emptyFeed.Stop)
	response, err := http.Get(emptyServer.URL)
	if err != nil {
		t.Fatalf("empty scope request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("empty scope status = %d", response.StatusCode)
	}

	unauthFeed := live.NewFeed(live.FeedOptions{Auth: &fakeAuth{err: live.ErrUnauthenticated}, State: state})
	unauthServer := httptest.NewServer(unauthFeed)
	t.Cleanup(unauthServer.Close)
	unauthResponse, err := http.Get(unauthServer.URL)
	if err != nil {
		t.Fatalf("unauthenticated request: %v", err)
	}
	defer unauthResponse.Body.Close()
	if unauthResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthResponse.StatusCode)
	}

	postRequest, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("{}"))
	postResponse, err := http.DefaultClient.Do(postRequest)
	if err != nil {
		t.Fatalf("post request: %v", err)
	}
	defer postResponse.Body.Close()
	if postResponse.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", postResponse.StatusCode)
	}
}

func TestLiveFeedEndsOnSessionRevocation(t *testing.T) {
	state, _ := newState(t)
	session, revoked := newSession()
	feed := live.NewFeed(live.FeedOptions{Auth: &fakeAuth{session: session}, State: state})
	server := httptest.NewServer(feed)
	t.Cleanup(server.Close)
	t.Cleanup(feed.Stop)

	client, response := openSSE(t, server.URL, nil)
	client.nextFrame()
	close(revoked)
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, response.Body)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("revocation did not end the stream")
	}
}

func TestLiveFeedMaxClients(t *testing.T) {
	state, _ := newState(t)
	session, _ := newSession()
	feed := live.NewFeed(live.FeedOptions{Auth: &fakeAuth{session: session}, State: state, MaxClients: 1})
	server := httptest.NewServer(feed)
	t.Cleanup(server.Close)
	t.Cleanup(feed.Stop)

	first, _ := openSSE(t, server.URL, nil)
	first.nextFrame()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("second client request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-budget status = %d", response.StatusCode)
	}
}

func TestSnapshotJSONContract(t *testing.T) {
	state, info := newState(t)
	now := time.Now()
	state.ApplyMetrics(info.SessionID, metricsRecord("c-1", "run-1", "boot-1", 1, now), now)
	snapshot := state.Snapshot()
	data, err := api.EncodeHubSnapshot(snapshot)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	decoded, err := api.DecodeHubSnapshot(data)
	if err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if len(decoded.Machines) != 1 {
		t.Fatalf("machines = %d", len(decoded.Machines))
	}
	machine := decoded.Machines[0]
	if machine.ExpectedIntervalMs == nil || !machine.ExpectedIntervalMs.Valid || machine.ExpectedIntervalMs.Value != 1000 {
		t.Fatalf("expectedIntervalMs = %+v", machine.ExpectedIntervalMs)
	}
	if machine.Metrics == nil || machine.Metrics.ReceivedAt != *machine.LastReceivedAt {
		t.Fatalf("metrics receipt disagrees with lastReceivedAt")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode raw snapshot: %v", err)
	}
}
