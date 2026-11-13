//go:build linux

package runtime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/policy"
	"github.com/Triet1402/rigscope/internal/collector/processes"
	"github.com/Triet1402/rigscope/internal/collector/sampling/gpu"
	"github.com/Triet1402/rigscope/internal/collector/state"
	"github.com/Triet1402/rigscope/internal/collector/synthetic"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/protocol"
	"github.com/coder/websocket"
)

// fakeHub is a minimal hub: it authenticates one bearer credential, performs
// the hello/welcome exchange, acknowledges every sample and records what the
// collector sent. It also serves the enrollment endpoint.
type fakeHub struct {
	token string

	server *httptest.Server

	mu            sync.Mutex
	conns         map[*websocket.Conn]struct{}
	hellos        []protocol.HelloMessage
	inventories   []protocol.Inventory
	statuses      []protocol.StatusMessage
	samples       []protocol.MetricsRecord
	detailResults []protocol.DetailResult
	connections   int
	enrollments   int
	enrollToken   string
	// sendDetail, when set, is forwarded to the collector after the welcome.
	sendDetail *protocol.DetailSubscribe
}

func newFakeHub(t *testing.T, token string) *fakeHub {
	t.Helper()
	hub := &fakeHub{token: token, conns: map[*websocket.Conn]struct{}{}}
	hub.server = httptest.NewServer(http.HandlerFunc(hub.handle))
	t.Cleanup(func() {
		hub.closeAll()
		hub.server.Close()
	})
	return hub
}

func (h *fakeHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for conn := range h.conns {
		_ = conn.Close(websocket.StatusNormalClosure, "test over")
	}
	h.conns = map[*websocket.Conn]struct{}{}
}

func (h *fakeHub) url() string {
	return "ws" + strings.TrimPrefix(h.server.URL, "http") + "/api/collector/connect"
}

func (h *fakeHub) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/collector/enroll":
		h.handleEnroll(w, r)
	case "/api/collector/connect":
		h.handleConnect(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *fakeHub) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") != "Bearer grant-token" {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	var request struct {
		CollectorID    string `json:"collectorId"`
		RequestID      string `json:"requestId"`
		RecoverySecret string `json:"recoverySecret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "body", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	h.enrollments++
	h.enrollToken = "issued-token"
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = fmt.Fprintf(w, `{"collectorId":%q,"machineId":"machine-1","token":"issued-token"}`, request.CollectorID)
}

func (h *fakeHub) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+h.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	ctx := context.Background()
	defer conn.Close(websocket.StatusNormalClosure, "test over")
	h.mu.Lock()
	h.conns[conn] = struct{}{}
	h.connections++
	detail := h.sendDetail
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.conns, conn)
		h.mu.Unlock()
	}()

	_, data, err := conn.Read(ctx)
	if err != nil {
		return
	}
	first, err := protocol.DecodeCollectorMessage(data)
	if err != nil {
		return
	}
	hello, ok := first.(protocol.HelloMessage)
	if !ok {
		return
	}
	h.mu.Lock()
	h.hellos = append(h.hellos, hello)
	h.mu.Unlock()

	welcome, err := protocol.EncodeHubMessage(protocol.WelcomeMessage{
		HubRunID:           "hub-run-1",
		MachineID:          "machine-1",
		Delivery:           protocol.DeliveryMemory,
		MaxInFlightBatches: 8,
	})
	if err != nil {
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, welcome); err != nil {
		return
	}
	if detail != nil {
		payload, err := protocol.EncodeHubMessage(*detail)
		if err == nil {
			_ = conn.Write(ctx, websocket.MessageText, payload)
		}
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		message, err := protocol.DecodeCollectorMessage(data)
		if err != nil {
			continue
		}
		switch typed := message.(type) {
		case protocol.InventoryMessage:
			h.mu.Lock()
			h.inventories = append(h.inventories, typed.Inventory)
			h.mu.Unlock()
		case protocol.StatusMessage:
			h.mu.Lock()
			h.statuses = append(h.statuses, typed)
			h.mu.Unlock()
		case protocol.SamplesMessage:
			refs := make([]protocol.RecordRef, 0, len(typed.Records))
			for _, record := range typed.Records {
				switch typed := record.(type) {
				case protocol.MetricsRecord:
					refs = append(refs, protocol.RecordRef{RunID: typed.RunID, Sequence: typed.Sequence})
					h.mu.Lock()
					h.samples = append(h.samples, typed)
					h.mu.Unlock()
				case protocol.GapRecord:
					refs = append(refs, protocol.RecordRef{RunID: typed.RunID, Sequence: typed.Sequence})
				}
			}
			ack, err := protocol.EncodeHubMessage(protocol.AckMessage{Accepted: refs})
			if err != nil {
				return
			}
			if err := conn.Write(ctx, websocket.MessageText, ack); err != nil {
				return
			}
		case protocol.DetailResult:
			h.mu.Lock()
			h.detailResults = append(h.detailResults, typed)
			h.mu.Unlock()
		}
	}
}

func (h *fakeHub) snapshot() (hellos []protocol.HelloMessage, samples []protocol.MetricsRecord, inventories []protocol.Inventory, statuses []protocol.StatusMessage, details []protocol.DetailResult, connections int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]protocol.HelloMessage(nil), h.hellos...),
		append([]protocol.MetricsRecord(nil), h.samples...),
		append([]protocol.Inventory(nil), h.inventories...),
		append([]protocol.StatusMessage(nil), h.statuses...),
		append([]protocol.DetailResult(nil), h.detailResults...),
		h.connections
}

func testConfig(hubURL string, syntheticMode bool) *config.Config {
	cfg := config.Default()
	cfg.Components.Hub.Enabled = false
	cfg.Components.Collector.Destinations = []config.Destination{{URL: hubURL}}
	cfg.Components.Collector.SamplingInterval = config.Duration(100 * time.Millisecond)
	cfg.Components.Collector.Synthetic = syntheticMode
	return cfg
}

func newStore(t *testing.T, dir string) *state.Store {
	t.Helper()
	store, err := state.Open(dir, state.Options{})
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	return store
}

// eventually polls condition until the deadline. Tests use it instead of
// fixed sleeps so a slow machine does not make them flaky.
func eventually(t *testing.T, timeout time.Duration, what string, condition func() bool) {
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

func startRuntime(t *testing.T, options Options) (context.CancelFunc, <-chan error) {
	t.Helper()
	collector, err := New(options)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- collector.Run(ctx) }()
	return cancel, done
}

func stopRuntime(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runtime returned an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runtime did not stop")
	}
}

func TestRunSamplesDeliversInventoryAndStopsCleanly(t *testing.T) {
	hub := newFakeHub(t, "secret-token")
	dir := t.TempDir()
	store := newStore(t, dir)
	if err := store.SaveCredential(state.Credential{URL: hub.url(), Token: "secret-token"}); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	var eventsMu sync.Mutex
	var messages []string
	ready := make(chan string, 1)
	cancel, done := startRuntime(t, Options{
		Config:   testConfig(hub.url(), true),
		StateDir: dir,
		Store:    store,
		Logf: func(event Event) {
			eventsMu.Lock()
			messages = append(messages, event.Message)
			eventsMu.Unlock()
		},
		OnReady: func(url string) {
			select {
			case ready <- url:
			default:
			}
		},
	})
	select {
	case url := <-ready:
		if url != hub.url() {
			t.Fatalf("ready for %q, want %q", url, hub.url())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime was never welcomed")
	}

	eventually(t, 10*time.Second, "delivered samples and inventory", func() bool {
		_, samples, inventories, statuses, _, _ := hub.snapshot()
		return len(samples) >= 2 && len(inventories) >= 1 && len(statuses) >= 1
	})
	hellos, samples, inventories, statuses, _, _ := hub.snapshot()
	if len(hellos) != 1 {
		t.Fatalf("hellos = %d, want 1", len(hellos))
	}
	hello := hellos[0]
	if hello.ExpectedIntervalMS != 100 {
		t.Fatalf("expectedIntervalMs = %d, want 100", hello.ExpectedIntervalMS)
	}
	if !containsCapability(hello.Capabilities, protocol.CapabilityMetrics) ||
		!containsCapability(hello.Capabilities, protocol.CapabilityInventory) ||
		!containsCapability(hello.Capabilities, protocol.CapabilityProcesses) {
		t.Fatalf("capabilities = %v", hello.Capabilities)
	}
	if hello.Protocol == nil || *hello.Protocol != protocol.Version {
		t.Fatalf("hello protocol = %v", hello.Protocol)
	}
	first, second := samples[0], samples[1]
	if second.Sequence <= first.Sequence {
		t.Fatalf("sequences do not advance: %d then %d", first.Sequence, second.Sequence)
	}
	if first.Payload.CPU == nil {
		t.Fatal("the record has no CPU family")
	}
	if inventories[0].Capabilities.CPU.Status != protocol.CapabilitySupported {
		t.Fatalf("inventory CPU capability = %q", inventories[0].Capabilities.CPU.Status)
	}
	if statuses[0].Delivery.QueueMode == nil || *statuses[0].Delivery.QueueMode != protocol.QueueModeMemory {
		t.Fatalf("status queue mode = %v, want memory", statuses[0].Delivery.QueueMode)
	}

	stopRuntime(t, cancel, done)
	eventsMu.Lock()
	defer eventsMu.Unlock()
	joined := strings.Join(messages, "\n")
	for _, wanted := range []string{"collector starting", "collector stopping", "collector stopped"} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("logs are missing %q:\n%s", wanted, joined)
		}
	}
}

func TestSecondCollectorOnOneStateDirectoryFails(t *testing.T) {
	hub := newFakeHub(t, "secret-token")
	dir := t.TempDir()
	store := newStore(t, dir)
	if err := store.SaveCredential(state.Credential{URL: hub.url(), Token: "secret-token"}); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	ready := make(chan struct{}, 1)
	cancel, done := startRuntime(t, Options{
		Config:   testConfig(hub.url(), true),
		StateDir: dir,
		Store:    store,
		OnReady: func(string) {
			select {
			case ready <- struct{}{}:
			default:
			}
		},
	})
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the first collector was never welcomed")
	}
	second, err := New(Options{Config: testConfig(hub.url(), true), StateDir: dir, Store: newStore(t, dir)})
	if err != nil {
		t.Fatalf("new second runtime: %v", err)
	}
	if err := second.Run(context.Background()); err == nil {
		t.Fatal("a second collector on the same state directory must fail")
	}
	stopRuntime(t, cancel, done)
}

func TestAwaitingEnrollmentStaysDormantThenStops(t *testing.T) {
	hub := newFakeHub(t, "secret-token")
	dir := t.TempDir()
	var mu sync.Mutex
	var messages []string
	cancel, done := startRuntime(t, Options{
		Config:   testConfig(hub.url(), true),
		StateDir: dir,
		Store:    newStore(t, dir),
		Logf: func(event Event) {
			mu.Lock()
			messages = append(messages, event.Message)
			mu.Unlock()
		},
	})
	eventually(t, 5*time.Second, "the awaiting-enrollment notice", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.Join(messages, "\n"), "awaiting enrollment")
	})
	_, _, _, _, _, connections := hub.snapshot()
	if connections != 0 {
		t.Fatalf("an unenrolled collector connected %d times", connections)
	}
	stopRuntime(t, cancel, done)
}

func TestPendingGrantIsConsumedAtStartup(t *testing.T) {
	hub := newFakeHub(t, "issued-token")
	dir := t.TempDir()
	store := newStore(t, dir)
	requestID, secret, err := state.NewRecoveryProof(rand.Reader)
	if err != nil {
		t.Fatalf("proof: %v", err)
	}
	if err := store.SavePendingEnrollment(state.PendingEnrollment{
		URL:            hub.url(),
		Grant:          "grant-token",
		RequestID:      requestID,
		RecoverySecret: secret,
	}); err != nil {
		t.Fatalf("save pending enrollment: %v", err)
	}
	ready := make(chan struct{}, 1)
	cancel, done := startRuntime(t, Options{
		Config:   testConfig(hub.url(), true),
		StateDir: dir,
		Store:    store,
		OnReady: func(string) {
			select {
			case ready <- struct{}{}:
			default:
			}
		},
	})
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the collector never enrolled and connected")
	}
	credential, found, err := store.Credential(hub.url())
	if err != nil || !found {
		t.Fatalf("credential after enrollment = %+v, %v, %v", credential, found, err)
	}
	if credential.Token != "issued-token" {
		t.Fatalf("stored token = %q", credential.Token)
	}
	pending, stillPending, err := store.PendingEnrollment(hub.url())
	if err != nil || stillPending {
		t.Fatalf("the consumed grant is still pending: %+v, %v, %v", pending, stillPending, err)
	}
	stopRuntime(t, cancel, done)
}

func TestProcessDetailSubscriptionsReachTheHub(t *testing.T) {
	hub := newFakeHub(t, "secret-token")
	hub.sendDetail = &protocol.DetailSubscribe{SubscriptionID: "sub-1", Kind: protocol.DetailKindProcesses, IntervalMS: 1000, LeaseMS: 30000}
	dir := t.TempDir()
	store := newStore(t, dir)
	if err := store.SaveCredential(state.Credential{URL: hub.url(), Token: "secret-token"}); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	ready := make(chan struct{}, 1)
	cfg := testConfig(hub.url(), false)
	cancel, done := startRuntime(t, Options{
		Config:   cfg,
		StateDir: dir,
		Store:    store,
		OnReady: func(string) {
			select {
			case ready <- struct{}{}:
			default:
			}
		},
		NewSource: func(identity state.Identity, sequence *Sequence) (Source, func(), error) {
			sampler, err := synthetic.NewSampler(synthetic.Options{
				CollectorID: identity.CollectorID,
				RunID:       identity.RunID,
				BootID:      identity.BootID,
				Sequence:    sequence,
			})
			return sampler, nil, err
		},
		NewScanner: func() (processes.SnapshotScanner, error) { return fakeScanner{}, nil },
	})
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the collector was never welcomed")
	}
	eventually(t, 10*time.Second, "a process detail result", func() bool {
		_, _, _, _, details, _ := hub.snapshot()
		return len(details) > 0
	})
	_, _, _, _, details, _ := hub.snapshot()
	result := details[0]
	if result.SubscriptionID != "sub-1" {
		t.Fatalf("subscriptionId = %q", result.SubscriptionID)
	}
	if result.Snapshot.TotalProcesses != 1 || len(result.Snapshot.Processes) != 1 {
		t.Fatalf("snapshot = %+v", result.Snapshot)
	}
	if result.Snapshot.Processes[0].Name != "rigscope-test" || result.Snapshot.Processes[0].PID != 4242 {
		t.Fatalf("process row = %+v", result.Snapshot.Processes[0])
	}
	stopRuntime(t, cancel, done)
}

type fakeScanner struct{}

func (fakeScanner) Scan(context.Context, processes.ScanOptions) (processes.ProcessSnapshot, error) {
	return processes.ProcessSnapshot{
		TotalProcesses: 1,
		Processes: []processes.ProcessRow{{
			PID:        4242,
			PPID:       1,
			StartTicks: 10,
			Name:       "rigscope-test",
			State:      "R",
		}},
	}, nil
}

func TestCheckReportsEnrollmentState(t *testing.T) {
	hub := newFakeHub(t, "secret-token")
	dir := t.TempDir()
	store := newStore(t, dir)
	cfg := testConfig(hub.url(), true)

	result, err := Check(cfg, dir)
	if err != nil {
		t.Fatalf("check before enrollment: %v", err)
	}
	if result.Enrolled || result.PendingGrant || result.Destination != hub.url() {
		t.Fatalf("check before enrollment = %+v", result)
	}
	if err := store.SaveCredential(state.Credential{URL: hub.url(), Token: "secret-token"}); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	result, err = Check(cfg, dir)
	if err != nil {
		t.Fatalf("check after enrollment: %v", err)
	}
	if !result.Enrolled {
		t.Fatalf("check after enrollment = %+v", result)
	}
	if _, err := Check(cfg, dir+"/missing"); err == nil {
		t.Fatal("a missing state directory must fail the readiness check")
	}
	invalid := testConfig(hub.url(), true)
	invalid.Components.Collector.SamplingInterval = config.Duration(time.Millisecond)
	if _, err := Check(invalid, dir); err == nil {
		t.Fatal("an invalid configuration must fail the readiness check")
	}
}

func TestSequenceIsSafeForConcurrentUse(t *testing.T) {
	sequence := NewSequence()
	const workers = 8
	const perWorker = 250
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[int64]bool, workers*perWorker)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				value := sequence.Allocate()
				mu.Lock()
				if seen[value] {
					t.Errorf("sequence %d allocated twice", value)
				}
				seen[value] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != workers*perWorker {
		t.Fatalf("allocated %d distinct sequences, want %d", len(seen), workers*perWorker)
	}
	for value := int64(1); value <= workers*perWorker; value++ {
		if !seen[value] {
			t.Fatalf("sequence %d was skipped", value)
		}
	}
}

func containsCapability(capabilities []protocol.CapabilityName, wanted protocol.CapabilityName) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

// blockingSource wraps a synthetic sampler and holds the first Next call until
// the test releases it, modelling a host sample that takes longer than the
// sampling interval.
type blockingSource struct {
	sampler *synthetic.Sampler
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingSource) Next() (protocol.MetricsRecord, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return s.sampler.Next()
}

func (s *blockingSource) Inventory() protocol.Inventory          { return s.sampler.Inventory() }
func (s *blockingSource) InventoryVersion() int64                { return s.sampler.InventoryVersion() }
func (s *blockingSource) Failures() []protocol.CollectionFailure { return s.sampler.Failures() }

func TestSlowSamplingKeepsStatusAndShutdownResponsive(t *testing.T) {
	hub := newFakeHub(t, "secret-token")
	dir := t.TempDir()
	store := newStore(t, dir)
	if err := store.SaveCredential(state.Credential{URL: hub.url(), Token: "secret-token"}); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	source := &blockingSource{started: make(chan struct{}), release: make(chan struct{})}
	ready := make(chan struct{}, 1)
	collector, err := New(Options{
		Config:       testConfig(hub.url(), false),
		StateDir:     dir,
		Store:        store,
		DrainTimeout: 300 * time.Millisecond,
		OnReady: func(string) {
			select {
			case ready <- struct{}{}:
			default:
			}
		},
		NewSource: func(identity state.Identity, sequence *Sequence) (Source, func(), error) {
			sampler, err := synthetic.NewSampler(synthetic.Options{
				CollectorID: identity.CollectorID,
				RunID:       identity.RunID,
				BootID:      identity.BootID,
				Sequence:    sequence,
			})
			if err != nil {
				return nil, nil, err
			}
			source.sampler = sampler
			return source, nil, nil
		},
		NewScanner: func() (processes.SnapshotScanner, error) { return fakeScanner{}, nil },
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- collector.Run(ctx) }()
	defer func() {
		close(source.release)
	}()

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime was never welcomed")
	}
	select {
	case <-source.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the sampling worker never started")
	}

	// The welcome status must arrive even though the first sample is stuck.
	eventually(t, 2*time.Second, "a status while sampling is blocked", func() bool {
		_, _, _, statuses, _, _ := hub.snapshot()
		return len(statuses) > 0
	})
	// Ticks that fire while the worker is busy are skipped and counted, never
	// queued behind the blocked sample.
	eventually(t, 2*time.Second, "skipped sampling ticks", func() bool {
		return collector.Stats().SkippedSamples > 0
	})
	if collector.Stats().Samples != 0 {
		t.Fatalf("samples = %d while the first one is blocked", collector.Stats().Samples)
	}

	startedShutdown := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runtime returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown blocked behind the sampling worker")
	}
	if elapsed := time.Since(startedShutdown); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %s with a 300ms budget", elapsed)
	}
}

func TestStatusIsSentImmediatelyAfterWelcome(t *testing.T) {
	hub := newFakeHub(t, "secret-token")
	dir := t.TempDir()
	store := newStore(t, dir)
	if err := store.SaveCredential(state.Credential{URL: hub.url(), Token: "secret-token"}); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	cfg := testConfig(hub.url(), true)
	cfg.Components.Collector.SamplingInterval = config.Duration(30 * time.Second)
	ready := make(chan struct{}, 1)
	cancel, done := startRuntime(t, Options{
		Config:   cfg,
		StateDir: dir,
		Store:    store,
		OnReady: func(string) {
			select {
			case ready <- struct{}{}:
			default:
			}
		},
	})
	defer stopRuntime(t, cancel, done)
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime was never welcomed")
	}
	started := time.Now()
	eventually(t, 2*time.Second, "a status right after welcome", func() bool {
		_, _, _, statuses, _, _ := hub.snapshot()
		return len(statuses) > 0
	})
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("status took %s after welcome with a 30s sampling interval", elapsed)
	}
}

func TestGPUDiagnosticsObeySharingPolicy(t *testing.T) {
	var mu sync.Mutex
	var messages []string
	collector := &Collector{logf: func(event Event) {
		mu.Lock()
		messages = append(messages, event.Message)
		mu.Unlock()
	}}
	secret := "gpu 0000:01:00.0: NVIDIA GeForce RTX 4090 query failed: \x1b[31mdriver error\x1b[0m\nsecond line"

	collector.logGPUDiagnostic(policy.Policy{GPUs: false}, secret)
	mu.Lock()
	withheld := strings.Join(messages, "\n")
	messages = nil
	mu.Unlock()
	for _, forbidden := range []string{"0000:01:00.0", "NVIDIA", "GeForce", "4090", "driver error", "second line"} {
		if strings.Contains(withheld, forbidden) {
			t.Fatalf("withheld GPU diagnostics leaked %q:\n%s", forbidden, withheld)
		}
	}
	if !strings.Contains(withheld, "withheld") {
		t.Fatalf("withheld GPU diagnostics are not generic:\n%s", withheld)
	}

	collector.logGPUDiagnostic(policy.Policy{GPUs: true}, secret)
	mu.Lock()
	shared := strings.Join(messages, "\n")
	messages = nil
	mu.Unlock()
	if strings.Contains(shared, "\x1b") || strings.Contains(shared, "\n") {
		t.Fatalf("shared GPU diagnostics still carry control characters: %q", shared)
	}

	collector.logGPUDiagnostic(policy.Policy{GPUs: true}, strings.Repeat("x", maxGPUDiagnosticRunes+100))
	mu.Lock()
	bounded := messages[len(messages)-1]
	mu.Unlock()
	if len([]rune(bounded)) > maxGPUDiagnosticRunes {
		t.Fatalf("diagnostic was not bounded: %d runes", len([]rune(bounded)))
	}
}

type contextRunner struct {
	started  chan struct{}
	finished chan struct{}
}

func (r *contextRunner) Run(ctx context.Context, _ string, _ []string, _ time.Duration) (string, string, error) {
	close(r.started)
	<-ctx.Done()
	close(r.finished)
	return "", "", ctx.Err()
}

type stuckRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r *stuckRunner) Run(context.Context, string, []string, time.Duration) (string, string, error) {
	close(r.started)
	<-r.release
	return "", "", nil
}

func TestStopPollerJoinsItsWork(t *testing.T) {
	runner := &contextRunner{started: make(chan struct{}), finished: make(chan struct{})}
	poller := gpu.NewPoller(gpu.PollerOptions{Interval: time.Hour, Runner: runner})
	poller.Start(context.Background())
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the GPU poller never started a cycle")
	}
	stopPoller(poller, time.Second)
	select {
	case <-runner.finished:
	default:
		t.Fatal("stopPoller returned before the GPU cycle was joined")
	}
}

func TestStopPollerHonorsTheShutdownBudget(t *testing.T) {
	runner := &stuckRunner{started: make(chan struct{}), release: make(chan struct{})}
	poller := gpu.NewPoller(gpu.PollerOptions{Interval: time.Hour, Runner: runner})
	poller.Start(context.Background())
	defer close(runner.release)
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the GPU poller never started a cycle")
	}
	started := time.Now()
	stopPoller(poller, 100*time.Millisecond)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stopPoller waited %s with a 100ms budget", elapsed)
	}
}

func TestCheckRejectsCorruptIdentityWithoutMutatingIt(t *testing.T) {
	hub := newFakeHub(t, "secret-token")
	dir := t.TempDir()
	store := newStore(t, dir)
	identityPath := filepath.Join(store.Dir(), "identity.json")
	corrupt := []byte(`{"collectorId":"","hostId":""}`)
	if err := os.WriteFile(identityPath, corrupt, 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	if _, err := Check(testConfig(hub.url(), true), dir); err == nil {
		t.Fatal("a corrupt identity must fail the readiness check")
	} else if !strings.Contains(err.Error(), "identity.json") {
		t.Fatalf("error does not name the identity file: %v", err)
	}
	after, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatalf("read identity after check: %v", err)
	}
	if string(after) != string(corrupt) {
		t.Fatalf("the readiness check mutated the identity:\nbefore %s\nafter  %s", corrupt, after)
	}
}

func TestCheckRequiresAWritableLease(t *testing.T) {
	hub := newFakeHub(t, "secret-token")
	dir := t.TempDir()
	newStore(t, dir)
	if err := os.Mkdir(LeasePath(dir), 0o700); err != nil {
		t.Fatalf("create lease directory: %v", err)
	}
	if _, err := Check(testConfig(hub.url(), true), dir); err == nil {
		t.Fatal("an unwritable runtime lease must fail the readiness check")
	} else if !strings.Contains(err.Error(), "lease") {
		t.Fatalf("error does not name the lease: %v", err)
	}
}

func TestSyntheticSourceObeysTheSharingPolicy(t *testing.T) {
	hub := newFakeHub(t, "secret-token")
	dir := t.TempDir()
	store := newStore(t, dir)
	if err := store.SaveCredential(state.Credential{URL: hub.url(), Token: "secret-token"}); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	cfg := testConfig(hub.url(), true)
	cfg.Components.Collector.Sharing = config.Sharing{}
	cancel, done := startRuntime(t, Options{Config: cfg, StateDir: dir, Store: store})
	defer stopRuntime(t, cancel, done)
	eventually(t, 10*time.Second, "a filtered synthetic sample and inventory", func() bool {
		_, samples, inventories, _, _, _ := hub.snapshot()
		return len(samples) > 0 && len(inventories) > 0
	})
	_, samples, inventories, _, _, _ := hub.snapshot()
	record := samples[0]
	for family, value := range map[string]any{
		"cpu":     record.Payload.CPU,
		"memory":  record.Payload.Memory,
		"swap":    record.Payload.Swap,
		"gpus":    record.Payload.GPUs,
		"disks":   record.Payload.Disks,
		"network": record.Payload.Network,
	} {
		unavailable, ok := value.(protocol.Unavailable)
		if !ok || unavailable.Reason != policy.WithheldReason {
			t.Fatalf("synthetic %s family = %#v, want withheld", family, value)
		}
	}
	inventory := inventories[0]
	if len(inventory.GPUs) != 0 || len(inventory.Filesystems) != 0 || len(inventory.Disks) != 0 || len(inventory.Network) != 0 {
		t.Fatalf("synthetic inventory still carries resources: %+v", inventory)
	}
	for name, capability := range map[string]protocol.CapabilityState{
		"cpu":       inventory.Capabilities.CPU,
		"memory":    inventory.Capabilities.Memory,
		"swap":      inventory.Capabilities.Swap,
		"gpus":      inventory.Capabilities.GPUs,
		"disks":     inventory.Capabilities.Disks,
		"network":   inventory.Capabilities.Network,
		"processes": inventory.Capabilities.Processes,
	} {
		if capability.Status != protocol.CapabilityWithheld {
			t.Fatalf("synthetic %s capability = %q, want withheld", name, capability.Status)
		}
	}
}

type staticSource struct {
	failures  []protocol.CollectionFailure
	inventory protocol.Inventory
}

func (s *staticSource) Next() (protocol.MetricsRecord, error)  { return protocol.MetricsRecord{}, nil }
func (s *staticSource) Inventory() protocol.Inventory          { return s.inventory }
func (s *staticSource) InventoryVersion() int64                { return 0 }
func (s *staticSource) Failures() []protocol.CollectionFailure { return s.failures }

func TestFilteredSourceFiltersFailuresForEverySource(t *testing.T) {
	inner := &staticSource{
		failures: []protocol.CollectionFailure{
			{Metric: "gpus", Reason: "nvidia-smi failed"},
			{Metric: "cpu", Reason: "cpu is available"},
		},
	}
	filtered := &filteredSource{source: inner, sharing: func() policy.Policy { return policy.Policy{} }}
	failures := filtered.Failures()
	if len(failures) != 0 {
		t.Fatalf("filtered failures = %+v, want none", failures)
	}
}

func TestProcessScannerUsesTheHostPageSize(t *testing.T) {
	original := hostPageSize
	hostPageSize = func() int64 { return 16384 }
	t.Cleanup(func() { hostPageSize = original })

	config := (&Collector{}).scannerConfig()
	if config.PageSize != 16384 {
		t.Fatalf("pageSize = %d, want 16384", config.PageSize)
	}
	if config.ClockTicksPerSecond != clockTicksPerSecond {
		t.Fatalf("clock ticks = %v, want %d", config.ClockTicksPerSecond, clockTicksPerSecond)
	}
}

func TestLeaseLivesInCollectorSubtree(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "collector", "collector-runtime.lock")
	if got := LeasePath(root); got != want {
		t.Fatalf("LeasePath = %s, want %s", got, want)
	}
	if LeasePath(root) == filepath.Join(root, "collector-runtime.lock") {
		t.Fatal("lease still lives in the shared state root")
	}
}

func TestCheckReportsUnwritableLease(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode bits")
	}
	root := t.TempDir()
	collectorDir := filepath.Join(root, "collector")
	if err := os.Mkdir(collectorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Equivalent to a root-owned 0755 state root: the invoking account cannot
	// write it, but it can write the collector-owned 0700 subtree.
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0o755) //nolint:errcheck
	cfg := testConfig("ws://127.0.0.1:8081/api/collector/connect", true)
	if _, err := Check(cfg, root); err != nil {
		t.Fatalf("check failed although the lease is writable in the collector subtree: %v", err)
	}
	if _, err := os.Stat(LeasePath(root)); !os.IsNotExist(err) {
		t.Fatalf("the readiness probe created or replaced the live lease: %v", err)
	}
	if err := os.WriteFile(LeasePath(root), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(LeasePath(root), 0o400); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(LeasePath(root), 0o600) //nolint:errcheck
	if _, err := Check(cfg, root); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("unwritable lease was not reported: %v", err)
	}
}
