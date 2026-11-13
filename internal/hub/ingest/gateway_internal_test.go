package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
	"github.com/coder/websocket"
)

// gatewayHarness is the minimal in-process hub the in-package regression tests
// need; it exists so those tests can install the private pause hooks.
type gatewayHarness struct {
	t         *testing.T
	db        *storage.Database
	telemetry *storage.Telemetry
	registry  *storage.Registry
	state     *live.State
	gateway   *Gateway
	server    *httptest.Server
}

func TestMemoryWelcomeAndAcknowledgement(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-ram", MachineID: "m-ram", Token: "token-ram"}
	h := newGatewayHarnessWithTelemetry(t, identity, &storage.TelemetryOptions{Memory: true}, nil)
	conn := h.dial(identity.Token)
	defer conn.CloseNow()
	internalSend(t, conn, internalHello(identity.CollectorID, "run-ram"))
	welcome, ok := internalRead(t, conn).(protocol.WelcomeMessage)
	if !ok || welcome.Delivery != protocol.DeliveryMemory {
		t.Fatalf("welcome: %+v", welcome)
	}
	record := internalRecord(t, identity.CollectorID, "run-ram", 1, time.Now().UTC())
	internalSend(t, conn, protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{record}})
	ack, ok := internalRead(t, conn).(protocol.AckMessage)
	if !ok || len(ack.Accepted) != 1 {
		t.Fatalf("RAM acknowledgement: %+v", ack)
	}
	var count int
	if err := h.db.QueryRow(context.Background(), "SELECT COUNT(*) FROM sample_receipts").Scan(&count); err != nil || count != 0 {
		t.Fatalf("durable receipts: %d, %v", count, err)
	}
}

func newGatewayHarness(t *testing.T, identity storage.Identity, configure func(*GatewayOptions)) *gatewayHarness {
	return newGatewayHarnessWithTelemetry(t, identity, nil, configure)
}

func newGatewayHarnessWithTelemetry(t *testing.T, identity storage.Identity, telemetryOptions *storage.TelemetryOptions, configure func(*GatewayOptions)) *gatewayHarness {
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
	options := storage.TelemetryOptions{FlushInterval: 10 * time.Millisecond}
	if telemetryOptions != nil {
		options = *telemetryOptions
		if options.FlushInterval == 0 {
			options.FlushInterval = 10 * time.Millisecond
		}
	}
	telemetry, err := storage.NewTelemetry(ctx, db, options)
	if err != nil {
		t.Fatalf("open telemetry: %v", err)
	}
	state := live.NewState("hub-run-1", nil)
	if err := state.RegisterMachine(identity.CollectorID, identity.MachineID); err != nil {
		t.Fatalf("register machine: %v", err)
	}
	opts := GatewayOptions{
		Registry:     registry,
		Telemetry:    telemetry,
		State:        state,
		HelloTimeout: 5 * time.Second,
	}
	if configure != nil {
		configure(&opts)
	}
	gateway, err := NewGateway(opts)
	if err != nil {
		t.Fatalf("create gateway: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/collector/connect", gateway)
	server := httptest.NewServer(mux)
	harness := &gatewayHarness{
		t: t, db: db, telemetry: telemetry, registry: registry, state: state,
		gateway: gateway, server: server,
	}
	t.Cleanup(func() {
		gateway.Close()
		server.Close()
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = telemetry.Close(closeCtx)
		_ = db.Close()
	})
	return harness
}

func (h *gatewayHarness) wsURL() string {
	return "ws" + strings.TrimPrefix(h.server.URL, "http") + "/api/collector/connect"
}

func (h *gatewayHarness) dial(token string) *websocket.Conn {
	h.t.Helper()
	conn, status, err := h.tryDial(token, nil)
	if err != nil {
		h.t.Fatalf("dial collector socket (status %d): %v", status, err)
	}
	return conn
}

func (h *gatewayHarness) tryDial(token string, headers http.Header) (*websocket.Conn, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Authorization", "Bearer "+token)
	conn, response, err := websocket.Dial(ctx, h.wsURL(), &websocket.DialOptions{HTTPHeader: headers})
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	if conn != nil {
		conn.SetReadLimit(protocol.MaxDetailBytes)
	}
	return conn, status, err
}

func internalHello(collectorID, runID string) protocol.HelloMessage {
	return protocol.HelloMessage{
		CollectorID:        collectorID,
		RunID:              runID,
		BootID:             "boot-1",
		Hostname:           "rig-1",
		Capabilities:       []protocol.CapabilityName{protocol.CapabilityMetrics, protocol.CapabilityInventory},
		ExpectedIntervalMS: 1000,
	}
}

func internalSend(t *testing.T, conn *websocket.Conn, message protocol.CollectorMessage) {
	t.Helper()
	if err := internalTrySend(conn, message); err != nil {
		t.Fatalf("write collector message: %v", err)
	}
}

func internalTrySend(conn *websocket.Conn, message protocol.CollectorMessage) error {
	data, err := protocol.EncodeCollectorMessage(message)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

func internalRead(t *testing.T, conn *websocket.Conn) protocol.HubMessage {
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

func internalRecord(t *testing.T, collectorID, runID string, sequence int64, capturedAt time.Time) protocol.MetricsRecord {
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
	for _, record := range samples.Records {
		metrics, ok := record.(protocol.MetricsRecord)
		if !ok {
			continue
		}
		metrics.CollectorID = collectorID
		metrics.RunID = runID
		metrics.BootID = "boot-1"
		metrics.Sequence = sequence
		metrics.CapturedAt = capturedAt.UTC().Format("2006-01-02T15:04:05.000Z")
		return metrics
	}
	t.Fatalf("fixture has no metrics record")
	return protocol.MetricsRecord{}
}

func internalInventory(t *testing.T, platform string) protocol.Inventory {
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
	inventory.Inventory.OS.Platform = platform
	return inventory.Inventory
}

func (h *gatewayHarness) waitFor(timeout time.Duration, what string, condition func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

// socketState is one collector socket under the concurrent-hello regression.
type socketState struct {
	run    string
	conn   *websocket.Conn
	closed chan struct{}
	acks   chan struct{}
}

// TestRevocationDuringUpgradeIsRefused pauses a connection after it reserved a
// slot but before the WebSocket upgrade, revokes the credential, and then lets
// the upgrade continue. The per-collector credential generation must refuse it.
func TestRevocationDuringUpgradeIsRefused(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-upgrade", MachineID: "m-upgrade", Token: "token-upgrade"}
	paused := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	harness := newGatewayHarness(t, identity, func(opts *GatewayOptions) {
		opts.testAfterReserve = func() {
			once.Do(func() {
				close(paused)
				<-release
			})
		}
	})

	type dialOutcome struct {
		status int
		err    error
	}
	outcomes := make(chan dialOutcome, 1)
	go func() {
		conn, status, err := harness.tryDial(identity.Token, nil)
		if conn != nil {
			_ = conn.CloseNow()
		}
		outcomes <- dialOutcome{status: status, err: err}
	}()

	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatalf("connection never reached the pre-upgrade pause")
	}
	harness.gateway.RevokeCollector(identity.CollectorID)
	close(release)

	var outcome dialOutcome
	select {
	case outcome = <-outcomes:
	case <-time.After(5 * time.Second):
		t.Fatalf("in-flight upgrade did not finish")
	}
	if outcome.err == nil {
		t.Fatalf("a revoked credential completed the upgrade")
	}
	if outcome.status != http.StatusUnauthorized {
		t.Fatalf("upgrade status = %d, want %d", outcome.status, http.StatusUnauthorized)
	}
	if got := harness.gateway.ConnectedCount(); got != 0 {
		t.Fatalf("revoked connection left %d sessions", got)
	}
	if got := harness.state.ConnectedCount(); got != 0 {
		t.Fatalf("revoked connection left %d live sessions", got)
	}
	if _, ok := harness.state.Machine(identity.MachineID); ok {
		t.Fatalf("revoked collector remains in live state")
	}
}

// TestConcurrentHellosKeepOneLiveSession reproduces the double-termination
// race: two hellos for one collector are both inside handleHello at the same
// time. Exactly one session must survive, and it must still acknowledge
// samples; the superseded socket must close.
func TestConcurrentHellosKeepOneLiveSession(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-race", MachineID: "m-race", Token: "token-race"}
	var arrived sync.WaitGroup
	arrived.Add(2)
	release := make(chan struct{})
	harness := newGatewayHarness(t, identity, func(opts *GatewayOptions) {
		opts.testBeforeActivate = func() {
			arrived.Done()
			<-release
		}
	})

	first := harness.dial(identity.Token)
	defer first.CloseNow()
	second := harness.dial(identity.Token)
	defer second.CloseNow()

	sockets := []*socketState{
		{run: "run-a", conn: first, closed: make(chan struct{}), acks: make(chan struct{}, 1)},
		{run: "run-b", conn: second, closed: make(chan struct{}), acks: make(chan struct{}, 1)},
	}

	var sendWG sync.WaitGroup
	sendWG.Add(2)
	start := make(chan struct{})
	for index := range sockets {
		socket := sockets[index]
		go func() {
			defer sendWG.Done()
			<-start
			_ = internalTrySend(socket.conn, internalHello(identity.CollectorID, socket.run))
		}()
	}
	close(start)
	arrived.Wait()
	close(release)
	sendWG.Wait()

	for _, socket := range sockets {
		socket := socket
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for {
				_, data, err := socket.conn.Read(ctx)
				if err != nil {
					close(socket.closed)
					return
				}
				message, decodeErr := protocol.DecodeHubMessage(data)
				if decodeErr != nil {
					continue
				}
				if ack, ok := message.(protocol.AckMessage); ok && len(ack.Accepted) == 1 {
					select {
					case socket.acks <- struct{}{}:
					default:
					}
				}
			}
		}()
	}

	closures := socketClosures(sockets)
	var closedIndex int
	select {
	case closedIndex = <-closures:
	case <-time.After(5 * time.Second):
		t.Fatalf("neither superseded nor surviving socket closed")
	}
	// The loser closes within the same transition; a second close means both
	// sessions were terminated.
	select {
	case other := <-closures:
		t.Fatalf("both sockets closed (%d and %d)", closedIndex, other)
	case <-time.After(200 * time.Millisecond):
	}

	harness.waitFor(2*time.Second, "one live gateway session", func() bool {
		return harness.gateway.ConnectedCount() == 1 && harness.state.ConnectedCount() == 1
	})
	live := sockets[1-closedIndex]
	if err := internalTrySend(live.conn, protocol.SamplesMessage{
		Mode:    protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{internalRecord(t, identity.CollectorID, live.run, 1, time.Now())},
	}); err != nil {
		t.Fatalf("surviving session rejected samples: %v", err)
	}
	select {
	case <-live.acks:
	case <-time.After(5 * time.Second):
		t.Fatalf("surviving session never acknowledged a sample")
	}
}

// closedIndex multiplexes both socket close channels into one channel.
func socketClosures(sockets []*socketState) <-chan int {
	out := make(chan int, len(sockets))
	for index, socket := range sockets {
		go func(index int, socket *socketState) {
			<-socket.closed
			out <- index
		}(index, socket)
	}
	return out
}

// TestInventoryFromReplacedSessionIsNotApplied pauses an inventory write after
// its commit, replaces the collector session, then resumes the old write. The
// session-aware state application must refuse it so the newer inventory wins.
func TestInventoryFromReplacedSessionIsNotApplied(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-inventory", MachineID: "m-inventory", Token: "token-inventory"}
	paused := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	outcomes := make(chan bool, 4)
	harness := newGatewayHarness(t, identity, func(opts *GatewayOptions) {
		opts.testAfterInventoryPersist = func() {
			if calls.Add(1) == 1 {
				close(paused)
				<-release
			}
		}
		opts.testAfterInventoryApply = func(applied bool) {
			outcomes <- applied
		}
	})

	old := harness.dial(identity.Token)
	defer old.CloseNow()
	internalSend(t, old, internalHello(identity.CollectorID, "run-old"))
	if _, ok := internalRead(t, old).(protocol.WelcomeMessage); !ok {
		t.Fatalf("old session was not welcomed")
	}
	internalSend(t, old, protocol.InventoryMessage{
		CapturedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Inventory:  internalInventory(t, "old-platform"),
	})
	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatalf("inventory write never paused")
	}

	replacement := harness.dial(identity.Token)
	defer replacement.CloseNow()
	internalSend(t, replacement, internalHello(identity.CollectorID, "run-new"))
	if _, ok := internalRead(t, replacement).(protocol.WelcomeMessage); !ok {
		t.Fatalf("replacement session was not welcomed")
	}
	internalSend(t, replacement, protocol.InventoryMessage{
		CapturedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Inventory:  internalInventory(t, "new-platform"),
	})
	harness.waitFor(3*time.Second, "replacement inventory", func() bool {
		machine, ok := harness.state.Machine(identity.MachineID)
		return ok && machine.Inventory != nil && machine.Inventory.OS.Platform == "new-platform"
	})

	close(release)
	var sawApplied, sawRefused bool
	for seen := 0; seen < 2; seen++ {
		select {
		case applied := <-outcomes:
			if applied {
				sawApplied = true
			} else {
				sawRefused = true
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("inventory application outcome %d did not arrive", seen)
		}
	}
	if !sawApplied || !sawRefused {
		t.Fatalf("outcomes applied=%v refused=%v", sawApplied, sawRefused)
	}
	machine, ok := harness.state.Machine(identity.MachineID)
	if !ok || machine.Inventory == nil || machine.Inventory.OS.Platform != "new-platform" {
		t.Fatalf("old session overwrote the replacement inventory: %+v", machine.Inventory)
	}
}
