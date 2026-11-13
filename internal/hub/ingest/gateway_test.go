package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/collector/delivery"
	"github.com/Triet1402/rigscope/internal/hub/history"
	"github.com/Triet1402/rigscope/internal/hub/ingest"
	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
	"github.com/coder/websocket"
)

type fakeAuth struct {
	session live.Session
}

func (f *fakeAuth) Authenticate(*http.Request) (live.Session, error) { return f.session, nil }

// testHub is one in-process hub with the collector protocol and the browser
// routes mounted over a real HTTP listener.
type testHub struct {
	t         *testing.T
	db        *storage.Database
	telemetry *storage.Telemetry
	registry  *storage.Registry
	state     *live.State
	gateway   *ingest.Gateway
	processes *live.ProcessInterests
	feed      *live.Feed
	history   *history.Handler
	enroll    *ingest.EnrollmentHandler
	auth      *fakeAuth
	server    *httptest.Server
	detailMu  sync.Mutex
	details   []protocol.CollectorMessage
	detailCh  chan protocol.CollectorMessage
	closeOnce sync.Once
}

func newTestHub(t *testing.T, identity storage.Identity, flush time.Duration) *testHub {
	t.Helper()
	return newTestHubWith(t, identity, flush, nil)
}

func newTestHubWith(t *testing.T, identity storage.Identity, flush time.Duration, configure func(*ingest.GatewayOptions)) *testHub {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	registry, err := storage.NewRegistry(ctx, db, storage.RegistryOptions{
		BootstrapIdentities: []storage.Identity{identity},
	})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	if flush == 0 {
		flush = 50 * time.Millisecond
	}
	telemetry, err := storage.NewTelemetry(ctx, db, storage.TelemetryOptions{FlushInterval: flush})
	if err != nil {
		t.Fatalf("open telemetry: %v", err)
	}
	state := live.NewState("hub-run-1", nil)
	if err := state.RegisterMachine(identity.CollectorID, identity.MachineID); err != nil {
		t.Fatalf("register machine: %v", err)
	}
	hub := &testHub{
		t:         t,
		db:        db,
		telemetry: telemetry,
		registry:  registry,
		state:     state,
		detailCh:  make(chan protocol.CollectorMessage, 16),
	}
	hub.auth = &fakeAuth{session: live.Session{
		ID:       "session-1",
		Username: "demo",
		Scope:    api.MachineScope{All: true},
	}}
	var gateway *ingest.Gateway
	hub.processes = live.NewProcessInterests(live.ProcessOptions{
		Auth:  hub.auth,
		State: state,
		Sender: live.DetailSenderFunc(func(machineID string, message protocol.HubMessage) bool {
			if gateway == nil {
				return false
			}
			return gateway.SendDetail(machineID, message)
		}),
		RenewEvery: 50 * time.Millisecond,
		SweepEvery: 20 * time.Millisecond,
	})
	gatewayOptions := ingest.GatewayOptions{
		Registry:     registry,
		Telemetry:    telemetry,
		State:        state,
		OnDetail:     hub.recordDetail,
		OnDisconnect: hub.processes.Disconnect,
		HelloTimeout: 500 * time.Millisecond,
	}
	if configure != nil {
		configure(&gatewayOptions)
	}
	gateway, err = ingest.NewGateway(gatewayOptions)
	if err != nil {
		t.Fatalf("create gateway: %v", err)
	}
	hub.gateway = gateway
	hub.feed = live.NewFeed(live.FeedOptions{
		Auth:      hub.auth,
		State:     state,
		Coalesce:  20 * time.Millisecond,
		Keepalive: 50 * time.Millisecond,
	})
	hub.history = history.New(history.Options{Auth: hub.auth, State: state, Telemetry: telemetry})
	hub.enroll = ingest.NewEnrollmentHandler(ingest.EnrollmentOptions{Registry: registry, State: state})

	mux := http.NewServeMux()
	mux.Handle("/api/collector/enroll", hub.enroll)
	mux.Handle("/api/collector/connect", hub.gateway)
	mux.Handle("/api/live", hub.feed)
	mux.Handle("/api/processes", hub.processes)
	mux.Handle("/api/history/status", hub.history)
	mux.Handle("/api/history/metrics", hub.history)
	mux.Handle("/api/history", hub.history)
	hub.server = httptest.NewServer(mux)
	t.Cleanup(hub.close)
	return hub
}

func (h *testHub) recordDetail(_ string, message protocol.CollectorMessage) {
	h.detailMu.Lock()
	h.details = append(h.details, message)
	h.detailMu.Unlock()
	select {
	case h.detailCh <- message:
	default:
	}
}

func (h *testHub) close() {
	h.closeOnce.Do(func() {
		h.feed.Stop()
		h.processes.Close()
		h.gateway.Close()
		h.server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.telemetry.Close(ctx)
		_ = h.db.Close()
	})
}

func (h *testHub) wsURL() string {
	return "ws" + strings.TrimPrefix(h.server.URL, "http") + "/api/collector/connect"
}

func (h *testHub) httpURL(path string) string { return h.server.URL + path }

// dial opens an authenticated collector socket.
func (h *testHub) dial(token string) *websocket.Conn {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, h.wsURL(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	if err != nil {
		h.t.Fatalf("dial collector socket: %v", err)
	}
	return conn
}

func send(t *testing.T, conn *websocket.Conn, message protocol.CollectorMessage) {
	t.Helper()
	data, err := protocol.EncodeCollectorMessage(message)
	if err != nil {
		t.Fatalf("encode collector message: %v", err)
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, data); err != nil {
		t.Fatalf("write collector message: %v", err)
	}
}

func readHub(t *testing.T, conn *websocket.Conn) protocol.HubMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read hub message: %v", err)
	}
	message, err := protocol.DecodeHubMessage(data)
	if err != nil {
		t.Fatalf("decode hub message: %v", err)
	}
	return message
}

func readAck(t *testing.T, conn *websocket.Conn) protocol.AckMessage {
	t.Helper()
	message := readHub(t, conn)
	ack, ok := message.(protocol.AckMessage)
	if !ok {
		t.Fatalf("expected ack, got %T", message)
	}
	return ack
}

func hello(collectorID, runID, bootID string, version *int64) protocol.HelloMessage {
	return protocol.HelloMessage{
		CollectorID:        collectorID,
		RunID:              runID,
		BootID:             bootID,
		Hostname:           "test-host",
		Capabilities:       []protocol.CapabilityName{protocol.CapabilityMetrics, protocol.CapabilityInventory, protocol.CapabilityProcesses},
		ExpectedIntervalMS: 1000,
		Protocol:           version,
	}
}

func versionPtr(value int64) *int64 { return &value }

func waitFor(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitClosed(t *testing.T, conn *websocket.Conn, timeout time.Duration) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatalf("expected the connection to close")
	}
	return websocket.CloseStatus(err)
}

func sampleRecords(t *testing.T) []protocol.TelemetryRecord {
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
	samples, ok := message.(protocol.SamplesMessage)
	if !ok {
		t.Fatalf("fixture is not samples")
	}
	return samples.Records
}

// metricsRecord builds one valid metrics record from the shared fixture.
func metricsRecord(t *testing.T, collectorID, runID, bootID string, sequence int64, capturedAt time.Time) protocol.MetricsRecord {
	t.Helper()
	for _, record := range sampleRecords(t) {
		metrics, ok := record.(protocol.MetricsRecord)
		if !ok {
			continue
		}
		metrics.CollectorID = collectorID
		metrics.RunID = runID
		metrics.BootID = bootID
		metrics.Sequence = sequence
		metrics.CapturedAt = capturedAt.UTC().Format("2006-01-02T15:04:05.000Z")
		return metrics
	}
	t.Fatalf("fixture has no metrics record")
	return protocol.MetricsRecord{}
}

func inventoryFixture(t *testing.T) protocol.Inventory {
	t.Helper()
	path := filepath.Join("..", "..", "..", "testdata", "protocol", "valid", "collector-inventory-full.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read inventory fixture: %v", err)
	}
	message, err := protocol.DecodeCollectorMessage(data)
	if err != nil {
		t.Fatalf("decode inventory fixture: %v", err)
	}
	inventory, ok := message.(protocol.InventoryMessage)
	if !ok {
		t.Fatalf("fixture is not inventory")
	}
	return inventory.Inventory
}

func statusMessage(capturedAt time.Time) protocol.StatusMessage {
	mode := protocol.QueueModeMemory
	lastAck := capturedAt.Add(-time.Second).UTC().Format("2006-01-02T15:04:05.000Z")
	return protocol.StatusMessage{
		CapturedAt: capturedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Delivery: protocol.DeliveryStatus{
			QueueMode:       &mode,
			PendingRecords:  3,
			PendingBytes:    300,
			InFlightRecords: 1,
			DroppedRecords:  0,
			Replaying:       false,
			LastAckAt:       &lastAck,
		},
		Failures: []protocol.CollectionFailure{},
	}
}

func TestEnrollmentRecovery(t *testing.T) {
	hub := newTestHub(t, storage.Identity{CollectorID: "c-existing", MachineID: "m-existing", Token: "existing-token"}, 0)
	ctx := context.Background()
	grant, err := hub.registry.CreateGrant(ctx, "test", storage.GrantOptions{TTL: time.Minute})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	recoverySecret := strings.Repeat("a", 43)

	enroll := func(body string) (*http.Response, api.EnrollmentIdentity) {
		request, err := http.NewRequest(http.MethodPost, hub.httpURL("/api/collector/enroll"), strings.NewReader(body))
		if err != nil {
			t.Fatalf("build enrollment request: %v", err)
		}
		request.Header.Set("Authorization", "Bearer "+grant.Token)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("enrollment request: %v", err)
		}
		defer response.Body.Close()
		var identity api.EnrollmentIdentity
		if response.StatusCode == http.StatusCreated {
			var raw json.RawMessage
			if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
				t.Fatalf("decode enrollment: %v", err)
			}
			decoded, err := api.DecodeEnrollmentIdentity(raw)
			if err != nil {
				t.Fatalf("strict enrollment identity: %v", err)
			}
			identity = decoded
		}
		return response, identity
	}

	body := fmt.Sprintf(`{"collectorId":"c-new","requestId":"req-1","recoverySecret":%q}`, recoverySecret)
	firstResponse, first := enroll(body)
	if firstResponse.StatusCode != http.StatusCreated {
		t.Fatalf("first enrollment status = %d", firstResponse.StatusCode)
	}
	if first.CollectorID != "c-new" || first.MachineID == "" || first.Token == "" {
		t.Fatalf("enrollment identity = %+v", first)
	}

	// A lost response is recovered by replaying the same grant and proof.
	secondResponse, second := enroll(body)
	if secondResponse.StatusCode != http.StatusCreated {
		t.Fatalf("recovered enrollment status = %d", secondResponse.StatusCode)
	}
	if second.Token != first.Token || second.MachineID != first.MachineID {
		t.Fatalf("recovery must return the same credential")
	}

	// A bad proof is denied without another oracle.
	badResponse, _ := enroll(fmt.Sprintf(`{"collectorId":"c-new","requestId":"req-1","recoverySecret":%q}`, strings.Repeat("b", 43)))
	if badResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("bad recovery proof status = %d", badResponse.StatusCode)
	}

	// A browser Origin is never allowed on the collector endpoint.
	originRequest, _ := http.NewRequest(http.MethodPost, hub.httpURL("/api/collector/enroll"), strings.NewReader(`{"collectorId":"c-origin"}`))
	originRequest.Header.Set("Authorization", "Bearer "+grant.Token)
	originRequest.Header.Set("Content-Type", "application/json")
	originRequest.Header.Set("Origin", "http://localhost:3000")
	originResponse, err := http.DefaultClient.Do(originRequest)
	if err != nil {
		t.Fatalf("origin enrollment request: %v", err)
	}
	defer originResponse.Body.Close()
	if originResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("origin enrollment status = %d", originResponse.StatusCode)
	}

	// The issued credential opens a session for the new machine.
	conn := hub.dial(first.Token)
	defer conn.Close(websocket.StatusNormalClosure, "done")
	send(t, conn, hello("c-new", "run-new", "boot-new", versionPtr(1)))
	welcome := readHub(t, conn).(protocol.WelcomeMessage)
	if welcome.MachineID != first.MachineID {
		t.Fatalf("welcome machineId = %q, want %q", welcome.MachineID, first.MachineID)
	}
}

func TestCollectorDeliveryEndToEnd(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-e2e", MachineID: "m-e2e", Token: "token-e2e"}
	hub := newTestHub(t, identity, 100*time.Millisecond)

	var sequence atomic.Int64
	welcomed := make(chan struct{}, 1)
	client, err := delivery.New(delivery.Config{
		HubURL:     hub.wsURL(),
		Credential: identity.Token,
		Identity: delivery.Identity{
			CollectorID: identity.CollectorID,
			RunID:       "run-e2e",
			BootID:      "boot-e2e",
			Hostname:    "rig-e2e",
		},
		ExpectedInterval: 500 * time.Millisecond,
		Capabilities: []protocol.CapabilityName{
			protocol.CapabilityMetrics,
			protocol.CapabilityInventory,
			protocol.CapabilityProcesses,
		},
		AllocateSequence: func() int64 { return sequence.Add(1) },
		OnWelcome: func() {
			select {
			case welcomed <- struct{}{}:
			default:
			}
		},
		BackoffBase:    20 * time.Millisecond,
		BackoffMax:     100 * time.Millisecond,
		WelcomeTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("create delivery client: %v", err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = client.Run(runCtx); close(runDone) }()
	defer func() {
		client.Close()
		cancelRun()
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Errorf("delivery client did not stop")
		}
	}()
	select {
	case <-welcomed:
	case <-time.After(5 * time.Second):
		t.Fatalf("collector was not welcomed")
	}

	now := time.Now().UTC()
	record := metricsRecord(t, identity.CollectorID, "run-e2e", "boot-e2e", sequence.Add(1), now)
	if err := client.Enqueue(record); err != nil {
		t.Fatalf("enqueue metrics: %v", err)
	}

	// The hub acknowledges only after the durable commit.
	waitFor(t, 5*time.Second, "queue to drain", func() bool {
		stats := client.Stats().Queue
		return stats.PendingRecords == 0 && stats.InFlightRecords == 0
	})
	latest, err := hub.telemetry.LatestLive(context.Background())
	if err != nil {
		t.Fatalf("latest live: %v", err)
	}
	if len(latest) != 1 || latest[0].Sequence != record.Sequence {
		t.Fatalf("latest live = %+v", latest)
	}
	waitFor(t, 2*time.Second, "live snapshot metrics", func() bool {
		snapshot := hub.state.Snapshot()
		return len(snapshot.Machines) == 1 && snapshot.Machines[0].Metrics != nil &&
			snapshot.Machines[0].Metrics.Sequence == record.Sequence
	})
	machine, _ := hub.state.Machine(identity.MachineID)
	if !machine.Connected {
		t.Fatalf("machine must be connected while the collector runs")
	}

	// Inventory is persisted and published.
	inventory := inventoryFixture(t)
	if !client.SetInventory(inventory) {
		t.Fatalf("inventory was not sent")
	}
	waitFor(t, 2*time.Second, "inventory publication", func() bool {
		snapshot := hub.state.Snapshot()
		return len(snapshot.Machines) == 1 && snapshot.Machines[0].Inventory != nil
	})
	stored, err := hub.telemetry.LoadInventory(context.Background(), identity.MachineID)
	if err != nil || stored == nil {
		t.Fatalf("stored inventory = %v, %v", stored, err)
	}

	// Process detail shares the collector connection.
	if !hub.gateway.SendDetail(identity.MachineID, protocol.DetailSubscribe{
		SubscriptionID: "sub-1",
		Kind:           protocol.DetailKindProcesses,
		IntervalMS:     1000,
		LeaseMS:        20000,
	}) {
		t.Fatalf("detail.subscribe was not delivered")
	}
	select {
	case request := <-client.Detail():
		if request.Subscribe == nil || request.Subscribe.SubscriptionID != "sub-1" {
			t.Fatalf("detail request = %+v", request)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("collector did not receive detail.subscribe")
	}
	if !client.SendDetail(protocol.DetailResult{
		SubscriptionID: "sub-1",
		CapturedAt:     now.Format("2006-01-02T15:04:05.000Z"),
		Snapshot:       protocol.ProcessSnapshot{Processes: []protocol.ProcessRow{}},
	}) {
		t.Fatalf("detail result was not sent")
	}
	select {
	case message := <-hub.detailCh:
		if _, ok := message.(protocol.DetailResult); !ok {
			t.Fatalf("detail callback = %T", message)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("hub did not receive detail.result")
	}

	// Status updates the live row.
	client.SendStatusMessage(statusMessage(time.Now().UTC()))
	waitFor(t, 2*time.Second, "delivery status publication", func() bool {
		snapshot := hub.state.Snapshot()
		return len(snapshot.Machines) == 1 && snapshot.Machines[0].Delivery != nil
	})

	// Disconnecting keeps the last readings but marks the machine offline.
	client.Close()
	waitFor(t, 3*time.Second, "collector disconnect", func() bool {
		machine, _ := hub.state.Machine(identity.MachineID)
		return !machine.Connected
	})
	snapshot := hub.state.Snapshot()
	if snapshot.Machines[0].Metrics == nil {
		t.Fatalf("last readings must survive a disconnect")
	}
}

func TestHandshakeNegotiation(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-proto", MachineID: "m-proto", Token: "token-proto"}
	hub := newTestHub(t, identity, 0)

	cases := []struct {
		name     string
		marker   *int64
		expected *int64
	}{
		{name: "missing marker means version 1", marker: nil, expected: nil},
		{name: "current version", marker: versionPtr(1), expected: versionPtr(1)},
		{name: "newer collector is negotiated down", marker: versionPtr(5), expected: versionPtr(1)},
	}
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) {
			conn := hub.dial(identity.Token)
			defer conn.Close(websocket.StatusNormalClosure, "done")
			send(t, conn, hello(identity.CollectorID, "run-"+scenario.name, "boot-1", scenario.marker))
			message := readHub(t, conn)
			welcome, ok := message.(protocol.WelcomeMessage)
			if !ok {
				t.Fatalf("expected welcome, got %T", message)
			}
			if welcome.Delivery != protocol.DeliveryDurable {
				t.Fatalf("delivery = %q", welcome.Delivery)
			}
			if welcome.MaxInFlightBatches != protocol.MaxInFlightBatches {
				t.Fatalf("maxInFlightBatches = %d", welcome.MaxInFlightBatches)
			}
			if welcome.HubRunID != hub.state.HubRunID() {
				t.Fatalf("hubRunId = %q", welcome.HubRunID)
			}
			switch {
			case scenario.expected == nil && welcome.Protocol != nil:
				t.Fatalf("welcome carried protocol %d for an unversioned hello", *welcome.Protocol)
			case scenario.expected != nil && (welcome.Protocol == nil || *welcome.Protocol != *scenario.expected):
				t.Fatalf("welcome protocol = %v, want %d", welcome.Protocol, *scenario.expected)
			}
		})
	}

	if _, _, err := ingest.NegotiateProtocol(versionPtr(1), 2, 2); err == nil {
		t.Fatalf("a version below the compatibility window must be refused")
	} else {
		var unsupported *ingest.UnsupportedProtocolError
		if !asUnsupported(err, &unsupported) {
			t.Fatalf("error type = %T", err)
		}
	}
}

func asUnsupported(err error, target **ingest.UnsupportedProtocolError) bool {
	value, ok := err.(*ingest.UnsupportedProtocolError)
	if ok {
		*target = value
	}
	return ok
}

func TestHandshakeRejections(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-reject", MachineID: "m-reject", Token: "token-reject"}

	t.Run("first message must be hello", func(t *testing.T) {
		hub := newTestHub(t, identity, 0)
		conn := hub.dial(identity.Token)
		defer conn.CloseNow()
		send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{
			metricsRecord(t, identity.CollectorID, "run-1", "boot-1", 1, time.Now()),
		}})
		if code := waitClosed(t, conn, 2*time.Second); code != ingest.CloseProtocolStep {
			t.Fatalf("close code = %d", code)
		}
	})

	t.Run("hello identity mismatch", func(t *testing.T) {
		hub := newTestHub(t, identity, 0)
		conn := hub.dial(identity.Token)
		defer conn.CloseNow()
		send(t, conn, hello("c-imposter", "run-1", "boot-1", versionPtr(1)))
		if code := waitClosed(t, conn, 2*time.Second); code != ingest.CloseIdentityMismatch {
			t.Fatalf("close code = %d", code)
		}
	})

	t.Run("hello deadline", func(t *testing.T) {
		hub := newTestHub(t, identity, 0)
		conn := hub.dial(identity.Token)
		defer conn.CloseNow()
		if code := waitClosed(t, conn, 3*time.Second); code != ingest.CloseHelloTimeout {
			t.Fatalf("close code = %d", code)
		}
	})

	t.Run("duplicate hello", func(t *testing.T) {
		hub := newTestHub(t, identity, 0)
		conn := hub.dial(identity.Token)
		defer conn.CloseNow()
		send(t, conn, hello(identity.CollectorID, "run-1", "boot-1", versionPtr(1)))
		readHub(t, conn)
		send(t, conn, hello(identity.CollectorID, "run-1", "boot-1", versionPtr(1)))
		if code := waitClosed(t, conn, 2*time.Second); code != ingest.CloseProtocolStep {
			t.Fatalf("close code = %d", code)
		}
	})

	t.Run("conflicting session for the same run", func(t *testing.T) {
		hub := newTestHub(t, identity, 0)
		first := hub.dial(identity.Token)
		defer first.CloseNow()
		send(t, first, hello(identity.CollectorID, "run-1", "boot-1", versionPtr(1)))
		readHub(t, first)

		second := hub.dial(identity.Token)
		defer second.CloseNow()
		send(t, second, hello(identity.CollectorID, "run-1", "boot-1", versionPtr(1)))
		if code := waitClosed(t, second, 2*time.Second); code != ingest.CloseSessionConflict {
			t.Fatalf("close code = %d", code)
		}
	})

	t.Run("a new run supersedes the previous socket", func(t *testing.T) {
		hub := newTestHub(t, identity, 0)
		first := hub.dial(identity.Token)
		send(t, first, hello(identity.CollectorID, "run-1", "boot-1", versionPtr(1)))
		readHub(t, first)

		second := hub.dial(identity.Token)
		defer second.CloseNow()
		send(t, second, hello(identity.CollectorID, "run-2", "boot-1", versionPtr(1)))
		if _, ok := readHub(t, second).(protocol.WelcomeMessage); !ok {
			t.Fatalf("new run was not welcomed")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, _, err := first.Read(ctx); err == nil {
			t.Fatalf("superseded socket must be closed")
		}
	})

	t.Run("record identity mismatch closes the session", func(t *testing.T) {
		hub := newTestHub(t, identity, 0)
		conn := hub.dial(identity.Token)
		defer conn.CloseNow()
		send(t, conn, hello(identity.CollectorID, "run-1", "boot-1", versionPtr(1)))
		readHub(t, conn)
		send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{
			metricsRecord(t, identity.CollectorID, "run-other", "boot-1", 1, time.Now()),
		}})
		if code := waitClosed(t, conn, 2*time.Second); code != ingest.CloseInvalidMessage {
			t.Fatalf("close code = %d", code)
		}
	})
}

func TestAcksFollowCommitAndDedup(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-ack", MachineID: "m-ack", Token: "token-ack"}
	hub := newTestHub(t, identity, 600*time.Millisecond)
	conn := hub.dial(identity.Token)
	defer conn.CloseNow()
	send(t, conn, hello(identity.CollectorID, "run-ack", "boot-ack", versionPtr(1)))
	readHub(t, conn)

	captured := time.Now().UTC()
	record := metricsRecord(t, identity.CollectorID, "run-ack", "boot-ack", 1, captured)
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{record}})

	// Live state is published immediately, before the commit.
	waitFor(t, 300*time.Millisecond, "live publication before the ack", func() bool {
		snapshot := hub.state.Snapshot()
		return len(snapshot.Machines) == 1 && snapshot.Machines[0].Metrics != nil &&
			snapshot.Machines[0].Metrics.Sequence == 1
	})
	if latest, err := hub.telemetry.LatestLive(context.Background()); err != nil || len(latest) != 0 {
		t.Fatalf("commit happened before the flush window: %v, %v", latest, err)
	}

	ack := readAck(t, conn)
	if len(ack.Accepted) != 1 || ack.Accepted[0].Sequence != 1 {
		t.Fatalf("ack = %+v", ack)
	}
	if len(ack.Rejected) != 0 {
		t.Fatalf("unexpected rejections: %+v", ack.Rejected)
	}
	if latest, err := hub.telemetry.LatestLive(context.Background()); err != nil || len(latest) != 1 {
		t.Fatalf("ack arrived before the commit: %v, %v", latest, err)
	}

	// Resending the same batch after a lost ack is accepted but not committed
	// twice.
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{record}})
	duplicate := readAck(t, conn)
	if len(duplicate.Accepted) != 1 || duplicate.Accepted[0].Sequence != 1 {
		t.Fatalf("duplicate ack = %+v", duplicate)
	}
	stats := hub.telemetry.Stats()
	if stats.CommittedRecords != 1 || stats.DuplicateRecords != 1 {
		t.Fatalf("committed=%d duplicates=%d", stats.CommittedRecords, stats.DuplicateRecords)
	}
}

func TestRejectionsAreListed(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-reject-records", MachineID: "m-reject-records", Token: "token-reject-records"}
	hub := newTestHub(t, identity, 20*time.Millisecond)
	conn := hub.dial(identity.Token)
	defer conn.CloseNow()
	send(t, conn, hello(identity.CollectorID, "run-r", "boot-r", versionPtr(1)))
	readHub(t, conn)

	now := time.Now().UTC()
	future := metricsRecord(t, identity.CollectorID, "run-r", "boot-r", 1, now.Add(48*time.Hour))
	valid := metricsRecord(t, identity.CollectorID, "run-r", "boot-r", 2, now)
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{future, valid}})
	ack := readAck(t, conn)
	if len(ack.Accepted) != 1 || ack.Accepted[0].Sequence != 2 {
		t.Fatalf("accepted = %+v", ack.Accepted)
	}
	if len(ack.Rejected) != 1 || ack.Rejected[0].Sequence != 1 || !strings.Contains(ack.Rejected[0].Reason, "future") {
		t.Fatalf("rejected = %+v", ack.Rejected)
	}
	if snapshot := hub.state.Snapshot(); snapshot.Machines[0].Metrics == nil || snapshot.Machines[0].Metrics.Sequence != 2 {
		t.Fatalf("live state must carry the accepted record")
	}
}

func TestReplayFillsHistoryWithoutLiveState(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-replay", MachineID: "m-replay", Token: "token-replay"}
	hub := newTestHub(t, identity, 20*time.Millisecond)
	conn := hub.dial(identity.Token)
	defer conn.CloseNow()
	send(t, conn, hello(identity.CollectorID, "run-live", "boot-live", versionPtr(1)))
	readHub(t, conn)

	replayTimestamp := time.Now().UTC().Add(-time.Minute)
	replay := metricsRecord(t, identity.CollectorID, "run-old", "boot-old", 7, replayTimestamp)
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeReplay, Records: []protocol.TelemetryRecord{replay}})
	ack := readAck(t, conn)
	if len(ack.Accepted) != 1 {
		t.Fatalf("replay ack = %+v", ack)
	}
	if snapshot := hub.state.Snapshot(); snapshot.Machines[0].Metrics != nil {
		t.Fatalf("replay must never publish live state")
	}

	live := metricsRecord(t, identity.CollectorID, "run-live", "boot-live", 8, time.Now().UTC())
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{live}})
	readAck(t, conn)
	waitFor(t, 2*time.Second, "live state", func() bool {
		snapshot := hub.state.Snapshot()
		return snapshot.Machines[0].Metrics != nil && snapshot.Machines[0].Metrics.Sequence == 8
	})

	// An older replay batch cannot supersede the newer live record.
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeReplay, Records: []protocol.TelemetryRecord{
		metricsRecord(t, identity.CollectorID, "run-old", "boot-old", 9, replayTimestamp.Add(time.Second)),
	}})
	readAck(t, conn)
	snapshot := hub.state.Snapshot()
	if snapshot.Machines[0].Metrics.Sequence != 8 {
		t.Fatalf("backfill superseded live state: %d", snapshot.Machines[0].Metrics.Sequence)
	}
}

func TestInventoryAndStatus(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-inv", MachineID: "m-inv", Token: "token-inv"}
	hub := newTestHub(t, identity, 20*time.Millisecond)
	conn := hub.dial(identity.Token)
	defer conn.CloseNow()
	send(t, conn, hello(identity.CollectorID, "run-inv", "boot-inv", versionPtr(1)))
	readHub(t, conn)

	inventory := inventoryFixture(t)
	send(t, conn, protocol.InventoryMessage{
		CapturedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Inventory:  inventory,
	})
	waitFor(t, 2*time.Second, "inventory state", func() bool {
		machine, _ := hub.state.Machine(identity.MachineID)
		return machine.Inventory != nil
	})
	if stored, err := hub.telemetry.LoadInventory(context.Background(), identity.MachineID); err != nil || stored == nil {
		t.Fatalf("inventory not persisted: %v, %v", stored, err)
	}

	status := statusMessage(time.Now().UTC())
	send(t, conn, status)
	waitFor(t, 2*time.Second, "delivery status", func() bool {
		snapshot := hub.state.Snapshot()
		return len(snapshot.Machines) == 1 && snapshot.Machines[0].Delivery != nil &&
			snapshot.Machines[0].Delivery.Delivery.PendingRecords == 3
	})
}

func TestRevocationClosesTheSession(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-revoke", MachineID: "m-revoke", Token: "token-revoke"}
	hub := newTestHub(t, identity, 0)
	conn := hub.dial(identity.Token)
	defer conn.CloseNow()
	send(t, conn, hello(identity.CollectorID, "run-revoke", "boot-revoke", versionPtr(1)))
	readHub(t, conn)

	hub.gateway.RevokeCollector(identity.CollectorID)
	if code := waitClosed(t, conn, 3*time.Second); code != ingest.ClosePolicy {
		t.Fatalf("close code = %d", code)
	}
	waitFor(t, 2*time.Second, "session end", func() bool {
		machine, _ := hub.state.Machine(identity.MachineID)
		return !machine.Connected
	})
}

// TestConnectionCapReservationIsAtomic proves the slot is reserved before the
// upgrade, so concurrent upgrades cannot exceed the documented cap.
func TestConnectionCapReservationIsAtomic(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-cap", MachineID: "m-cap", Token: "token-cap"}
	hub := newTestHubWith(t, identity, 0, func(options *ingest.GatewayOptions) {
		options.MaxConnections = 1
		options.HelloTimeout = 5 * time.Second
	})

	type dialOutcome struct {
		conn   *websocket.Conn
		status int
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan dialOutcome, 8)
	for index := 0; index < cap(outcomes); index++ {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, response, err := websocket.Dial(ctx, hub.wsURL(), &websocket.DialOptions{
				HTTPHeader: http.Header{"Authorization": []string{"Bearer " + identity.Token}},
			})
			status := 0
			if response != nil {
				status = response.StatusCode
			}
			outcomes <- dialOutcome{conn: conn, status: status, err: err}
		}()
	}
	close(start)

	succeeded := 0
	var successes []*websocket.Conn
	for index := 0; index < cap(outcomes); index++ {
		outcome := <-outcomes
		if outcome.err == nil {
			succeeded++
			successes = append(successes, outcome.conn)
			continue
		}
		if outcome.status != http.StatusServiceUnavailable {
			t.Fatalf("over-cap dial status = %d, want %d", outcome.status, http.StatusServiceUnavailable)
		}
	}
	for _, conn := range successes {
		conn.CloseNow()
	}
	if succeeded != 1 {
		t.Fatalf("concurrent upgrades succeeded = %d, want 1", succeeded)
	}

	// The slot is released on disconnect.
	var conn *websocket.Conn
	waitFor(t, 3*time.Second, "released connection slot", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		dialed, _, err := websocket.Dial(ctx, hub.wsURL(), &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + identity.Token}},
		})
		if err == nil {
			conn = dialed
			return true
		}
		return false
	})
	conn.CloseNow()
}

// TestIneligibleLiveRecordsAreNeverPublished reproduces the replay-horizon
// bypass: a stale or future record is rejected by storage, so it must never
// appear as fresh live state before the acknowledgment.
func TestIneligibleLiveRecordsAreNeverPublished(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-horizon", MachineID: "m-horizon", Token: "token-horizon"}
	hub := newTestHub(t, identity, 20*time.Millisecond)
	conn := hub.dial(identity.Token)
	defer conn.CloseNow()
	send(t, conn, hello(identity.CollectorID, "run-horizon", "boot-horizon", versionPtr(1)))
	if _, ok := readHub(t, conn).(protocol.WelcomeMessage); !ok {
		t.Fatalf("session was not welcomed")
	}

	now := time.Now().UTC()
	stale := metricsRecord(t, identity.CollectorID, "run-horizon", "boot-horizon", 1, now.Add(-48*time.Hour))
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{stale}})
	ack := readAck(t, conn)
	if len(ack.Rejected) != 1 || !strings.Contains(ack.Rejected[0].Reason, "older than the replay horizon") {
		t.Fatalf("stale ack = %+v", ack)
	}
	if snapshot := hub.state.Snapshot(); snapshot.Machines[0].Metrics != nil {
		t.Fatalf("a record rejected by storage was published live")
	}

	future := metricsRecord(t, identity.CollectorID, "run-horizon", "boot-horizon", 2, now.Add(48*time.Hour))
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{future}})
	ack = readAck(t, conn)
	if len(ack.Rejected) != 1 || !strings.Contains(ack.Rejected[0].Reason, "clock-skew") {
		t.Fatalf("future ack = %+v", ack)
	}
	if snapshot := hub.state.Snapshot(); snapshot.Machines[0].Metrics != nil {
		t.Fatalf("a future record was published live")
	}

	valid := metricsRecord(t, identity.CollectorID, "run-horizon", "boot-horizon", 3, now)
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{valid}})
	ack = readAck(t, conn)
	if len(ack.Accepted) != 1 || len(ack.Rejected) != 0 {
		t.Fatalf("valid ack = %+v", ack)
	}
	if snapshot := hub.state.Snapshot(); snapshot.Machines[0].Metrics == nil || snapshot.Machines[0].Metrics.Sequence != 3 {
		t.Fatalf("eligible record was not published")
	}
}

// TestPresentEmptyOriginRejected covers both collector endpoints: an Origin
// header is rejected by its presence, not by its value.
func TestPresentEmptyOriginRejected(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-origin", MachineID: "m-origin", Token: "token-origin"}
	hub := newTestHub(t, identity, 0)

	grant, err := hub.registry.CreateGrant(context.Background(), "test", storage.GrantOptions{TTL: time.Minute})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, hub.httpURL("/api/collector/enroll"),
		strings.NewReader(`{"collectorId":"c-origin-new"}`))
	if err != nil {
		t.Fatalf("build enrollment request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+grant.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Header["Origin"] = []string{""}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("enrollment request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("present-but-empty Origin enrollment status = %d", response.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, connectResponse, connectErr := websocket.Dial(ctx, hub.wsURL(), &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + identity.Token},
			"Origin":        []string{""},
		},
	})
	if conn != nil {
		conn.CloseNow()
	}
	if connectErr == nil {
		t.Fatalf("present-but-empty Origin completed the collector upgrade")
	}
	if connectResponse == nil || connectResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("present-but-empty Origin connect status = %v", connectResponse)
	}
}

// TestRevocationRemovesLiveState proves admin revocation drops the row, its
// readings and its inventory instead of leaving a stale offline machine.
func TestRevocationRemovesLiveState(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-remove", MachineID: "m-remove", Token: "token-remove"}
	hub := newTestHub(t, identity, 20*time.Millisecond)
	conn := hub.dial(identity.Token)
	defer conn.CloseNow()
	send(t, conn, hello(identity.CollectorID, "run-remove", "boot-remove", versionPtr(1)))
	readHub(t, conn)

	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{
		metricsRecord(t, identity.CollectorID, "run-remove", "boot-remove", 1, time.Now().UTC()),
	}})
	readAck(t, conn)
	send(t, conn, protocol.InventoryMessage{
		CapturedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Inventory:  inventoryFixture(t),
	})
	waitFor(t, 2*time.Second, "live row", func() bool {
		machine, ok := hub.state.Machine(identity.MachineID)
		return ok && machine.Connected && machine.Inventory != nil
	})

	hub.gateway.RevokeCollector(identity.CollectorID)
	waitFor(t, 2*time.Second, "live row removal", func() bool {
		_, ok := hub.state.Machine(identity.MachineID)
		return !ok
	})
	if code := waitClosed(t, conn, 3*time.Second); code != ingest.ClosePolicy {
		t.Fatalf("close code = %d", code)
	}
	if snapshot := hub.state.Snapshot(); len(snapshot.Machines) != 0 {
		t.Fatalf("revoked collector remains in the snapshot: %+v", snapshot.Machines)
	}
}

// TestLiveBatchPublishesHighestSequence selects the record with the highest
// sequence in the authenticated run, matching storage ordering, even when an
// older-sequence record was captured more recently.
func TestLiveBatchPublishesHighestSequence(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-order", MachineID: "m-order", Token: "token-order"}
	hub := newTestHub(t, identity, 20*time.Millisecond)
	conn := hub.dial(identity.Token)
	defer conn.CloseNow()
	send(t, conn, hello(identity.CollectorID, "run-order", "boot-order", versionPtr(1)))
	readHub(t, conn)

	now := time.Now().UTC()
	highest := metricsRecord(t, identity.CollectorID, "run-order", "boot-order", 7, now.Add(-20*time.Second))
	recent := metricsRecord(t, identity.CollectorID, "run-order", "boot-order", 5, now.Add(-time.Second))
	send(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{highest, recent}})
	ack := readAck(t, conn)
	if len(ack.Accepted) != 2 || len(ack.Rejected) != 0 {
		t.Fatalf("ack = %+v", ack)
	}
	waitFor(t, 2*time.Second, "live record", func() bool {
		snapshot := hub.state.Snapshot()
		return snapshot.Machines[0].Metrics != nil
	})
	if got := hub.state.Snapshot().Machines[0].Metrics.Sequence; got != 7 {
		t.Fatalf("live sequence = %d, want the highest sequence 7", got)
	}
}
