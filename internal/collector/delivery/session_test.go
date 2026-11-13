package delivery

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
	"github.com/coder/websocket"
)

const hubToken = "destination-secret-token"

type fakeHub struct {
	t       *testing.T
	token   string
	status  int
	welcome protocol.WelcomeMessage
	// serveSession, when set, replaces the default handshake and is called on
	// the raw upgraded connection.
	sessions chan *hubConn
	server   *httptest.Server
}

func newFakeHub(t *testing.T, mutate func(*fakeHub)) *fakeHub {
	t.Helper()
	hub := &fakeHub{
		t:     t,
		token: hubToken,
		welcome: protocol.WelcomeMessage{
			HubRunID:           "hub-run-1",
			MachineID:          "machine-1",
			Delivery:           protocol.DeliveryMemory,
			MaxInFlightBatches: 8,
		},
		sessions: make(chan *hubConn, 16),
	}
	if mutate != nil {
		mutate(hub)
	}
	hub.server = httptest.NewServer(http.HandlerFunc(hub.serve))
	t.Cleanup(hub.server.Close)
	return hub
}

// testCertificate returns a server certificate signed by a fresh CA and the
// CA's PEM. httptest's built-in certificate is shared by every server, which
// cannot test that pinning one CA rejects a certificate from another.
func testCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "rigscope-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}),
	)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	return certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func newTLSFakeHub(t *testing.T, mutate func(*fakeHub)) (*fakeHub, string) {
	t.Helper()
	hub := &fakeHub{
		t:     t,
		token: hubToken,
		welcome: protocol.WelcomeMessage{
			HubRunID:           "hub-run-1",
			MachineID:          "machine-1",
			Delivery:           protocol.DeliveryMemory,
			MaxInFlightBatches: 8,
		},
		sessions: make(chan *hubConn, 16),
	}
	if mutate != nil {
		mutate(hub)
	}
	certificate, caPEM := testCertificate(t)
	hub.server = httptest.NewUnstartedServer(http.HandlerFunc(hub.serve))
	hub.server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	hub.server.StartTLS()
	t.Cleanup(hub.server.Close)
	caFile := t.TempDir() + "/ca.pem"
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	return hub, caFile
}

func (h *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/collector/connect" {
		http.NotFound(w, r)
		return
	}
	if h.status != 0 {
		http.Error(w, "rejected", h.status)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+h.token {
		http.Error(w, "invalid bearer credentials", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "hub done")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, data, err := conn.Read(ctx)
	cancel()
	if err != nil {
		return
	}
	decoded, err := protocol.DecodeCollectorMessage(data)
	if err != nil {
		return
	}
	hello, ok := decoded.(protocol.HelloMessage)
	if !ok {
		return
	}
	welcomeData, err := protocol.EncodeHubMessage(h.welcome)
	if err != nil {
		h.t.Errorf("encode welcome: %v", err)
		return
	}
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 2*time.Second)
	err = conn.Write(writeCtx, websocket.MessageText, welcomeData)
	cancelWrite()
	if err != nil {
		return
	}

	session := &hubConn{
		conn:     conn,
		hello:    hello,
		messages: make(chan []byte, 64),
		done:     make(chan struct{}),
	}
	select {
	case h.sessions <- session:
	default:
		h.t.Errorf("session queue full")
		return
	}
	for {
		_, data, err := conn.Read(context.Background())
		if err != nil {
			close(session.done)
			return
		}
		select {
		case session.messages <- data:
		case <-session.done:
			return
		}
	}
}

type hubConn struct {
	conn     *websocket.Conn
	hello    protocol.HelloMessage
	messages chan []byte
	done     chan struct{}
}

func (h *hubConn) send(t *testing.T, message protocol.HubMessage) {
	t.Helper()
	data, err := protocol.EncodeHubMessage(message)
	if err != nil {
		t.Fatalf("encode hub message: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write hub message: %v", err)
	}
}

func (h *hubConn) next(t *testing.T, timeout time.Duration) protocol.CollectorMessage {
	t.Helper()
	select {
	case data := <-h.messages:
		message, err := protocol.DecodeCollectorMessage(data)
		if err != nil {
			t.Fatalf("decode collector message: %v", err)
		}
		return message
	case <-h.done:
		t.Fatal("hub connection closed while waiting for a message")
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a collector message")
	}
	return nil
}

func (h *hubConn) expectNoMessage(t *testing.T, duration time.Duration) {
	t.Helper()
	select {
	case data := <-h.messages:
		t.Fatalf("unexpected collector message: %s", data)
	case <-h.done:
	case <-time.After(duration):
	}
}

func (h *hubConn) close(t *testing.T) {
	t.Helper()
	_ = h.conn.Close(websocket.StatusGoingAway, "test reconnect")
}

func (h *fakeHub) waitSession(t *testing.T, timeout time.Duration) *hubConn {
	t.Helper()
	select {
	case session := <-h.sessions:
		return session
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a hub session")
	}
	return nil
}

func nextSamples(t *testing.T, session *hubConn) protocol.SamplesMessage {
	t.Helper()
	message := session.next(t, 2*time.Second)
	samples, ok := message.(protocol.SamplesMessage)
	if !ok {
		t.Fatalf("expected samples, got %T", message)
	}
	return samples
}

func (h *fakeHub) url() string {
	return "ws" + strings.TrimPrefix(h.server.URL, "http") + "/api/collector/connect"
}

func (h *fakeHub) tlsURL() string {
	return "wss" + strings.TrimPrefix(h.server.URL, "https") + "/api/collector/connect"
}

type clientFixture struct {
	client *Client
	logs   *logSink
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, strings.TrimSpace(formatLog(format, args...)))
}

func (s *logSink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *logSink) count(substring string) int {
	count := 0
	for _, line := range s.all() {
		if strings.Contains(line, substring) {
			count++
		}
	}
	return count
}

func formatLog(format string, args ...any) string {
	return strings.TrimSpace(fmt.Sprintf(format, args...))
}

func newClientFixture(t *testing.T, url string, mutate func(*Config)) *clientFixture {
	t.Helper()
	logs := &logSink{}
	sequence := int64(1_000_000)
	cfg := Config{
		HubURL:                  url,
		Credential:              hubToken,
		Identity:                Identity{CollectorID: "collector-1", RunID: "run-a", BootID: "boot-a", Hostname: "host-1"},
		AllocateSequence:        func() int64 { sequence++; return sequence },
		ExpectedInterval:        time.Second,
		Capabilities:            []protocol.CapabilityName{protocol.CapabilityMetrics, protocol.CapabilityInventory},
		Clock:                   func() time.Time { return queueTestT0 },
		Logf:                    logs.logf,
		WelcomeTimeout:          time.Second,
		IdleTimeout:             5 * time.Second,
		PingInterval:            2 * time.Second,
		AckTimeout:              100 * time.Millisecond,
		ReplayBatchesPerSecond:  1000,
		MaxLocalInFlightBatches: 8,
		BackoffBase:             5 * time.Millisecond,
		BackoffMax:              10 * time.Millisecond,
		QueueLimits:             QueueLimits{MaxAge: time.Minute, MaxBytes: 1 << 20, MaxGapRecords: 8},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fixture := &clientFixture{client: client, logs: logs}
	t.Cleanup(func() {
		client.Close()
		select {
		case <-client.runDone:
		case <-time.After(3 * time.Second):
			t.Error("delivery client did not stop")
		}
	})
	return fixture
}

func (f *clientFixture) start(t *testing.T, ctx context.Context) {
	t.Helper()
	go func() {
		_ = f.client.Run(ctx)
	}()
}

func waitFor(t *testing.T, timeout time.Duration, what string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func ackSamples(session *hubConn, t *testing.T, samples protocol.SamplesMessage, rejected []protocol.RejectedRecord, retryAfter *int64) {
	t.Helper()
	accepted := make([]protocol.RecordRef, 0, len(samples.Records))
	for _, record := range samples.Records {
		if ref, ok := refOf(record); ok {
			accepted = append(accepted, ref)
		}
	}
	session.send(t, protocol.AckMessage{Accepted: accepted, Rejected: rejected, RetryAfterMS: retryAfter})
}

func TestHandshakeSendsProtocolOneAndBearerHeader(t *testing.T) {
	hub := newFakeHub(t, nil)
	fixture := newClientFixture(t, hub.url(), nil)
	fixture.start(t, context.Background())

	session := hub.waitSession(t, 2*time.Second)
	if session.hello.CollectorID != "collector-1" || session.hello.RunID != "run-a" ||
		session.hello.BootID != "boot-a" || session.hello.Hostname != "host-1" {
		t.Fatalf("hello identity = %+v", session.hello)
	}
	if session.hello.Protocol == nil || *session.hello.Protocol != protocol.Version {
		t.Fatalf("hello protocol = %v, want %d", session.hello.Protocol, protocol.Version)
	}
	if session.hello.ExpectedIntervalMS != 1000 {
		t.Fatalf("expectedIntervalMs = %d", session.hello.ExpectedIntervalMS)
	}
	if len(session.hello.Capabilities) != 2 || session.hello.Capabilities[0] != protocol.CapabilityMetrics {
		t.Fatalf("capabilities = %v", session.hello.Capabilities)
	}
	waitFor(t, time.Second, "a welcomed client", func() bool {
		stats := fixture.client.Stats()
		return stats.Connected && stats.Welcomed
	})
	// The fake hub accepted the bearer header and answered a welcome without
	// the optional protocol marker (a 0.2-compatible hub).
	if stats := fixture.client.Stats(); stats.LastError != "" {
		t.Fatalf("last error = %q", stats.LastError)
	}
}

func TestAuthFailureExplainsReenrollmentOnce(t *testing.T) {
	hub := newFakeHub(t, func(h *fakeHub) { h.status = http.StatusUnauthorized })
	fixture := newClientFixture(t, hub.url(), nil)
	fixture.start(t, context.Background())

	waitFor(t, 2*time.Second, "a 401 error", func() bool {
		return strings.Contains(fixture.client.Stats().LastError, "401")
	})
	if count := fixture.logs.count("rejected this collector's credential (401)"); count != 1 {
		t.Fatalf("guidance lines = %d, want exactly 1: %v", count, fixture.logs.all())
	}
	stats := fixture.client.Stats()
	if stats.Connected {
		t.Fatal("a 401 must not mark the client connected")
	}
	if !strings.Contains(stats.LastError, "reconnecting") && stats.Attempts < 1 {
		t.Fatalf("attempts = %d", stats.Attempts)
	}
}

func TestLiveIsSentBeforeReplayAndTheWindowIsRespected(t *testing.T) {
	hub := newFakeHub(t, func(h *fakeHub) { h.welcome.MaxInFlightBatches = 4 })
	fixture := newClientFixture(t, hub.url(), func(c *Config) {
		// No ack: hold the window open for the whole assertion.
		c.AckTimeout = 10 * time.Second
	})
	for sequence := int64(1); sequence <= 100; sequence++ {
		if err := fixture.client.Enqueue(metricsRecord(sequence, queueTestT0)); err != nil {
			t.Fatal(err)
		}
	}
	fixture.start(t, context.Background())
	session := hub.waitSession(t, 2*time.Second)

	first := nextSamples(t, session)
	if first.Mode != protocol.SampleModeLive {
		t.Fatalf("first batch mode = %s, want live", first.Mode)
	}
	if len(first.Records) != 1 {
		t.Fatalf("live batch has %d records", len(first.Records))
	}
	if metrics := first.Records[0].(protocol.MetricsRecord); metrics.Sequence != 100 {
		t.Fatalf("live sequence = %d, want 100", metrics.Sequence)
	}
	second := nextSamples(t, session)
	third := nextSamples(t, session)
	if second.Mode != protocol.SampleModeReplay || third.Mode != protocol.SampleModeReplay {
		t.Fatalf("batches = %s, %s", second.Mode, third.Mode)
	}
	for _, batch := range []protocol.SamplesMessage{second, third} {
		if len(batch.Records) > protocol.MaxBatchRecords {
			t.Fatalf("replay batch has %d records", len(batch.Records))
		}
	}
	session.expectNoMessage(t, 100*time.Millisecond)

	// The reserved slot still accepts a fresh record while replay fills the
	// rest of the window.
	if err := fixture.client.Enqueue(metricsRecord(101, queueTestT0)); err != nil {
		t.Fatal(err)
	}
	live := nextSamples(t, session)
	if live.Mode != protocol.SampleModeLive {
		t.Fatalf("third batch mode = %s, want the fresh live record", live.Mode)
	}
	if metrics := live.Records[0].(protocol.MetricsRecord); metrics.Sequence != 101 {
		t.Fatalf("live sequence = %d, want 101", metrics.Sequence)
	}
}

func TestRejectedRecordsAreRemovedAndLogged(t *testing.T) {
	hub := newFakeHub(t, nil)
	fixture := newClientFixture(t, hub.url(), nil)
	if err := fixture.client.Enqueue(metricsRecord(1, queueTestT0)); err != nil {
		t.Fatal(err)
	}
	fixture.start(t, context.Background())
	session := hub.waitSession(t, 2*time.Second)

	samples := nextSamples(t, session)
	ref, _ := refOf(samples.Records[0])
	rejected := []protocol.RejectedRecord{{RecordRef: ref, Reason: "contract violation"}}
	session.send(t, protocol.AckMessage{Rejected: rejected})
	waitFor(t, time.Second, "the queue to drain", func() bool {
		stats := fixture.client.Stats()
		return stats.Queue.PendingRecords == 0 && stats.Queue.InFlightRecords == 0
	})
	if fixture.logs.count("hub rejected") == 0 {
		t.Fatalf("rejection was not logged: %v", fixture.logs.all())
	}
}

func TestLostAckTimesOutAndResendsTheSameRefs(t *testing.T) {
	hub := newFakeHub(t, nil)
	fixture := newClientFixture(t, hub.url(), func(c *Config) { c.AckTimeout = 60 * time.Millisecond })
	if err := fixture.client.Enqueue(metricsRecord(1, queueTestT0)); err != nil {
		t.Fatal(err)
	}
	fixture.start(t, context.Background())
	session := hub.waitSession(t, 2*time.Second)

	first := nextSamples(t, session)
	firstRef, _ := refOf(first.Records[0])
	resent := nextSamples(t, session)
	resentRef, _ := refOf(resent.Records[0])
	if firstRef != resentRef {
		t.Fatalf("resend ref = %+v, want %+v", resentRef, firstRef)
	}
	session.send(t, protocol.AckMessage{Accepted: []protocol.RecordRef{firstRef}})
	waitFor(t, time.Second, "ack to clear the queue", func() bool {
		return fixture.client.Stats().Queue.PendingRecords == 0
	})
	// A duplicate/delayed ack is harmless.
	session.send(t, protocol.AckMessage{Accepted: []protocol.RecordRef{firstRef}})
	if stats := fixture.client.Stats(); stats.Queue.PendingRecords != 0 {
		t.Fatalf("duplicate ack changed the queue: %+v", stats.Queue)
	}
}

func TestRetryAfterPausesReplayButNotLive(t *testing.T) {
	hub := newFakeHub(t, nil)
	fixture := newClientFixture(t, hub.url(), func(c *Config) {
		// Hold every batch in flight: this test drives acknowledgements itself.
		c.AckTimeout = 10 * time.Second
	})
	for sequence := int64(1); sequence <= 60; sequence++ {
		if err := fixture.client.Enqueue(metricsRecord(sequence, queueTestT0)); err != nil {
			t.Fatal(err)
		}
	}
	fixture.start(t, context.Background())
	session := hub.waitSession(t, 2*time.Second)

	live := nextSamples(t, session)
	if live.Mode != protocol.SampleModeLive {
		t.Fatalf("first batch = %s", live.Mode)
	}
	replay := nextSamples(t, session)
	if replay.Mode != protocol.SampleModeReplay {
		t.Fatalf("second batch = %s", replay.Mode)
	}
	// Drain every batch the client already sent on this connection, so the
	// next message observable after the retry response is unambiguously new.
	for {
		select {
		case <-session.messages:
		case <-time.After(50 * time.Millisecond):
			goto quiet
		}
	}
quiet:
	retry := int64(250)
	session.send(t, protocol.AckMessage{
		Accepted:     []protocol.RecordRef{},
		Rejected:     []protocol.RejectedRecord{},
		RetryAfterMS: &retry,
	})
	// Wait until the client processed the retry response before enqueuing.
	waitFor(t, time.Second, "replay pause log", func() bool {
		return fixture.logs.count("retry after") > 0
	})

	if err := fixture.client.Enqueue(metricsRecord(61, queueTestT0)); err != nil {
		t.Fatal(err)
	}
	next := nextSamples(t, session)
	if next.Mode != protocol.SampleModeLive {
		t.Fatalf("batch during the retry pause = %s, want live", next.Mode)
	}
	if metrics := next.Records[0].(protocol.MetricsRecord); metrics.Sequence != 61 {
		t.Fatalf("live sequence = %d, want 61", metrics.Sequence)
	}

	// Replay resumes after the pause.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("replay did not resume after retryAfterMs")
		}
		message := session.next(t, time.Until(deadline))
		if samples, ok := message.(protocol.SamplesMessage); ok && samples.Mode == protocol.SampleModeReplay {
			return
		}
	}
}

func TestReconnectResendsPendingRecordsWithBackoff(t *testing.T) {
	hub := newFakeHub(t, nil)
	fixture := newClientFixture(t, hub.url(), nil)
	if err := fixture.client.Enqueue(metricsRecord(1, queueTestT0)); err != nil {
		t.Fatal(err)
	}
	fixture.start(t, context.Background())

	first := hub.waitSession(t, 2*time.Second)
	firstSamples := nextSamples(t, first)
	firstRef, _ := refOf(firstSamples.Records[0])
	first.close(t)

	second := hub.waitSession(t, 2*time.Second)
	secondSamples := nextSamples(t, second)
	secondRef, _ := refOf(secondSamples.Records[0])
	if firstRef != secondRef {
		t.Fatalf("resent ref = %+v, want %+v", secondRef, firstRef)
	}
	firstStats := fixture.client.Stats()
	if firstStats.Reconnects < 1 {
		t.Fatalf("reconnects = %d", firstStats.Reconnects)
	}
	second.send(t, protocol.AckMessage{Accepted: []protocol.RecordRef{secondRef}})
	waitFor(t, time.Second, "the queue to drain after reconnect", func() bool {
		return fixture.client.Stats().Queue.PendingRecords == 0
	})
}

func TestQueueOverflowProducesGapRecordsOnTheWire(t *testing.T) {
	one := jsonBytes(t, metricsRecord(1, queueTestT0))
	hub := newFakeHub(t, nil)
	fixture := newClientFixture(t, hub.url(), func(c *Config) {
		c.QueueLimits = QueueLimits{MaxAge: time.Minute, MaxBytes: one * 3, MaxGapRecords: 4}
	})
	for sequence := int64(1); sequence <= 20; sequence++ {
		if err := fixture.client.Enqueue(metricsRecord(sequence, queueTestT0)); err != nil {
			t.Fatal(err)
		}
	}
	fixture.start(t, context.Background())
	session := hub.waitSession(t, 2*time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		message := session.next(t, time.Until(deadline))
		samples, ok := message.(protocol.SamplesMessage)
		if !ok {
			continue
		}
		for _, record := range samples.Records {
			if gap, ok := record.(protocol.GapRecord); ok {
				if gap.Payload.Reason != protocol.GapOutboxBytes {
					t.Fatalf("gap reason = %s", gap.Payload.Reason)
				}
				if gap.Payload.DroppedRecords < 1 {
					t.Fatalf("gap droppedRecords = %d", gap.Payload.DroppedRecords)
				}
				return
			}
		}
	}
	t.Fatal("no gap record reached the hub")
}

func TestDrainFlushesAndStops(t *testing.T) {
	hub := newFakeHub(t, nil)
	fixture := newClientFixture(t, hub.url(), nil)
	for sequence := int64(1); sequence <= 10; sequence++ {
		if err := fixture.client.Enqueue(metricsRecord(sequence, queueTestT0)); err != nil {
			t.Fatal(err)
		}
	}
	fixture.start(t, context.Background())
	session := hub.waitSession(t, 2*time.Second)

	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case data, ok := <-session.messages:
				if !ok {
					return
				}
				message, err := protocol.DecodeCollectorMessage(data)
				if err != nil {
					continue
				}
				if samples, ok := message.(protocol.SamplesMessage); ok {
					accepted := make([]protocol.RecordRef, 0, len(samples.Records))
					for _, record := range samples.Records {
						ref, _ := refOf(record)
						accepted = append(accepted, ref)
					}
					session.send(t, protocol.AckMessage{Accepted: accepted})
				}
			}
		}
	}()
	defer close(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := fixture.client.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	stats := fixture.client.Stats()
	if stats.Queue.PendingRecords != 0 || stats.Queue.InFlightRecords != 0 {
		t.Fatalf("queue after drain = %+v", stats.Queue)
	}
	if stats.Connected {
		t.Fatal("drain must close the connection")
	}
	// New records are refused once draining.
	if err := fixture.client.Enqueue(metricsRecord(11, queueTestT0)); err == nil {
		t.Fatal("Enqueue succeeded during drain")
	}
}

func TestDrainIsBoundedWhenTheHubNeverAcknowledges(t *testing.T) {
	hub := newFakeHub(t, nil)
	fixture := newClientFixture(t, hub.url(), func(c *Config) {
		c.AckTimeout = 10 * time.Second
	})
	if err := fixture.client.Enqueue(metricsRecord(1, queueTestT0)); err != nil {
		t.Fatal(err)
	}
	fixture.start(t, context.Background())
	hub.waitSession(t, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := fixture.client.Drain(ctx)
	if err == nil {
		t.Fatal("Drain succeeded with an unacknowledged record")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Drain overran its deadline: %s", elapsed)
	}
	stats := fixture.client.Stats()
	if stats.Queue.PendingRecords+stats.Queue.InFlightRecords == 0 {
		t.Fatal("the final stats hid the undelivered record")
	}
}

func TestDetailRequestsAreRoutedAndResultsSent(t *testing.T) {
	hub := newFakeHub(t, nil)
	fixture := newClientFixture(t, hub.url(), func(c *Config) {
		c.Capabilities = []protocol.CapabilityName{protocol.CapabilityMetrics, protocol.CapabilityProcesses}
	})
	fixture.start(t, context.Background())
	session := hub.waitSession(t, 2*time.Second)

	subscribe := protocol.DetailSubscribe{
		SubscriptionID: "sub-1",
		Kind:           protocol.DetailKindProcesses,
		IntervalMS:     1000,
		LeaseMS:        10_000,
	}
	session.send(t, subscribe)
	select {
	case request := <-fixture.client.Detail():
		if request.Subscribe == nil || request.Subscribe.SubscriptionID != "sub-1" {
			t.Fatalf("detail request = %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("detail.subscribe was not routed")
	}

	result := protocol.DetailResult{
		SubscriptionID: "sub-1",
		CapturedAt:     formatTimestamp(queueTestT0),
		Snapshot: protocol.ProcessSnapshot{
			TotalProcesses: 0,
			Truncated:      false,
			Inaccessible:   0,
			Processes:      []protocol.ProcessRow{},
		},
	}
	if !fixture.client.SendDetail(result) {
		t.Fatal("SendDetail returned false")
	}
	message := session.next(t, time.Second)
	detail, ok := message.(protocol.DetailResult)
	if !ok || detail.SubscriptionID != "sub-1" {
		t.Fatalf("detail result message = %T %+v", message, message)
	}

	cancel := protocol.DetailCancel{SubscriptionID: "sub-1"}
	session.send(t, cancel)
	select {
	case request := <-fixture.client.Detail():
		if request.Cancel == nil || request.Cancel.SubscriptionID != "sub-1" {
			t.Fatalf("detail cancel = %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("detail.cancel was not routed")
	}
	if !fixture.client.SendDetailEnded(protocol.DetailEnded{
		SubscriptionID: "sub-1",
		Reason:         protocol.DetailEndedCancelled,
	}) {
		t.Fatal("SendDetailEnded returned false")
	}
	ended := session.next(t, time.Second)
	if endedDetail, ok := ended.(protocol.DetailEnded); !ok || endedDetail.Reason != protocol.DetailEndedCancelled {
		t.Fatalf("detail.ended message = %T %+v", ended, ended)
	}
}

func TestInventoryAndStatusAreConnectionScoped(t *testing.T) {
	hub := newFakeHub(t, nil)
	fixture := newClientFixture(t, hub.url(), nil)
	inventory := protocol.Inventory{
		OS:          protocol.OSInventory{Platform: "linux", Release: "6.1", Arch: "amd64"},
		CPU:         protocol.CPUInventory{Model: protocol.Ptr("cpu"), LogicalCores: 4},
		Memory:      protocol.MemoryInventory{TotalBytes: protocol.Ptr(int64(8000))},
		GPUs:        []protocol.GpuInventory{},
		Filesystems: []protocol.FilesystemInventory{},
		Disks:       []protocol.DiskInventory{},
		Network:     []protocol.InterfaceInventory{},
		Capabilities: protocol.Capabilities{
			CPU:       protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Memory:    protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Swap:      protocol.CapabilityState{Status: protocol.CapabilitySupported},
			GPUs:      protocol.CapabilityState{Status: protocol.CapabilityUnsupported},
			Disks:     protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Network:   protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Processes: protocol.CapabilityState{Status: protocol.CapabilitySupported},
		},
	}
	if fixture.client.SetInventory(inventory) {
		t.Fatal("inventory before welcome must be remembered, not sent")
	}
	fixture.start(t, context.Background())
	session := hub.waitSession(t, 2*time.Second)

	message := session.next(t, time.Second)
	sent, ok := message.(protocol.InventoryMessage)
	if !ok {
		t.Fatalf("expected inventory after welcome, got %T", message)
	}
	if sent.Inventory.CPU.LogicalCores != 4 {
		t.Fatalf("inventory lost data: %+v", sent.Inventory)
	}
	if !fixture.client.SetInventory(inventory) {
		t.Fatal("inventory while welcomed was not sent")
	}
	if _, ok := session.next(t, time.Second).(protocol.InventoryMessage); !ok {
		t.Fatal("second inventory was not received")
	}

	resource := "fs:/"
	if !fixture.client.SendStatus([]protocol.CollectionFailure{{
		Metric:     "disks",
		ResourceID: &resource,
		Reason:     "permission denied",
	}}) {
		t.Fatal("SendStatus returned false")
	}
	statusMessage, ok := session.next(t, time.Second).(protocol.StatusMessage)
	if !ok {
		t.Fatal("status message was not received")
	}
	if statusMessage.Delivery.QueueMode == nil || *statusMessage.Delivery.QueueMode != protocol.QueueModeMemory {
		t.Fatalf("queue mode = %v", statusMessage.Delivery.QueueMode)
	}
	if len(statusMessage.Failures) != 1 || statusMessage.Failures[0].Metric != "disks" {
		t.Fatalf("failures = %+v", statusMessage.Failures)
	}
}

func TestPinnedCATrustsTheHub(t *testing.T) {
	hub, caFile := newTLSFakeHub(t, nil)
	fixture := newClientFixture(t, hub.tlsURL(), func(c *Config) {
		c.CAFile = caFile
	})
	fixture.start(t, context.Background())
	session := hub.waitSession(t, 3*time.Second)
	if session.hello.CollectorID != "collector-1" {
		t.Fatalf("hello = %+v", session.hello)
	}

	untrusted := newClientFixture(t, hub.tlsURL(), nil)
	untrusted.start(t, context.Background())
	waitFor(t, 3*time.Second, "an untrusted TLS failure", func() bool {
		return strings.Contains(untrusted.client.Stats().LastError, "certificate") ||
			strings.Contains(untrusted.client.Stats().LastError, "x509")
	})
	if untrusted.client.Stats().Connected {
		t.Fatal("client connected without trusting the pinned CA")
	}
}

func TestUnsupportedWelcomeProtocolIsRefused(t *testing.T) {
	version := int64(2)
	hub := newFakeHub(t, func(h *fakeHub) { h.welcome.Protocol = &version })
	fixture := newClientFixture(t, hub.url(), func(c *Config) {
		c.BackoffBase = time.Hour
		c.BackoffMax = time.Hour
	})
	fixture.start(t, context.Background())
	hub.waitSession(t, 2*time.Second)
	waitFor(t, 2*time.Second, "the protocol hint", func() bool {
		return fixture.logs.count("supports") > 0
	})
	if fixture.client.Stats().Welcomed {
		t.Fatal("client accepted an unsupported welcome protocol")
	}
}

func TestPlaintextRemoteHubIsRejectedAtConstruction(t *testing.T) {
	_, err := New(Config{
		HubURL:     "ws://hub.example.com/api/collector/connect",
		Credential: hubToken,
		Identity:   Identity{CollectorID: "c", RunID: "r"},
	})
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("New = %v", err)
	}
}

func TestWebSocketRedirectIsRejected(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		http.Error(w, "redirect target must never be dialled", http.StatusInternalServerError)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A same-host plaintext redirect is the dangerous case: Go would keep
		// the Authorization header on it.
		http.Redirect(w, r, target.URL+"/api/collector/connect", http.StatusFound)
	}))
	defer redirect.Close()
	hubURL := "ws" + strings.TrimPrefix(redirect.URL, "http") + "/api/collector/connect"

	fixture := newClientFixture(t, hubURL, func(c *Config) {
		c.BackoffBase = time.Hour
		c.BackoffMax = time.Hour
	})
	fixture.start(t, context.Background())
	waitFor(t, 3*time.Second, "the redirect to be rejected", func() bool {
		return fixture.client.Stats().LastError != ""
	})
	if stats := fixture.client.Stats(); stats.Connected || stats.Welcomed {
		t.Fatalf("a redirect was followed into a session: %+v", stats)
	}
	if hits := reached.Load(); hits != 0 {
		t.Fatalf("the redirect target was dialled %d time(s); redirects must be refused", hits)
	}
	if !strings.Contains(strings.ToLower(fixture.client.Stats().LastError), "redirect") &&
		!strings.Contains(fixture.client.Stats().LastError, "302") {
		t.Fatalf("last error = %q, want a redirect rejection", fixture.client.Stats().LastError)
	}
}

func TestRetryAfterPauseSurvivesReconnect(t *testing.T) {
	hub := newFakeHub(t, nil)
	fixture := newClientFixture(t, hub.url(), func(c *Config) {
		// Hold batches so this test drives acknowledgements itself.
		c.AckTimeout = 10 * time.Second
		c.BackoffBase = 5 * time.Millisecond
		c.BackoffMax = 10 * time.Millisecond
	})
	for sequence := int64(1); sequence <= 40; sequence++ {
		if err := fixture.client.Enqueue(metricsRecord(sequence, queueTestT0)); err != nil {
			t.Fatal(err)
		}
	}
	fixture.start(t, context.Background())
	first := hub.waitSession(t, 2*time.Second)
	// Drain what the first connection sent, then ask it to pause replay.
	for {
		select {
		case <-first.messages:
		case <-time.After(50 * time.Millisecond):
			goto quiet
		}
	}
quiet:
	retry := int64(700)
	first.send(t, protocol.AckMessage{RetryAfterMS: &retry})
	waitFor(t, time.Second, "the replay pause", func() bool {
		return fixture.logs.count("retry after") > 0
	})

	first.close(t)
	second := hub.waitSession(t, 2*time.Second)
	// The new connection inherits the pause; replay must not restart early.
	second.expectNoMessage(t, 150*time.Millisecond)

	// Live delivery is not paused.
	if err := fixture.client.Enqueue(metricsRecord(41, queueTestT0)); err != nil {
		t.Fatal(err)
	}
	live := nextSamples(t, second)
	if live.Mode != protocol.SampleModeLive {
		t.Fatalf("batch during a reconnected pause = %s, want live", live.Mode)
	}
	if metrics := live.Records[0].(protocol.MetricsRecord); metrics.Sequence != 41 {
		t.Fatalf("live sequence = %d, want 41", metrics.Sequence)
	}

	// Replay resumes once the original deadline expires.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("replay did not resume after the reconnected retryAfterMs")
		}
		message := second.next(t, time.Until(deadline))
		if samples, ok := message.(protocol.SamplesMessage); ok && samples.Mode == protocol.SampleModeReplay {
			return
		}
	}
}

func TestPinnedCARejectsACertificateFromAnotherCA(t *testing.T) {
	otherHub, _ := newTLSFakeHub(t, nil)
	_, firstCA := newTLSFakeHub(t, nil)

	fixture := newClientFixture(t, otherHub.tlsURL(), func(c *Config) {
		c.CAFile = firstCA
		c.BackoffBase = time.Hour
		c.BackoffMax = time.Hour
	})
	fixture.start(t, context.Background())
	waitFor(t, 3*time.Second, "an x509 failure", func() bool {
		err := fixture.client.Stats().LastError
		return strings.Contains(err, "certificate") || strings.Contains(err, "x509")
	})
	if fixture.client.Stats().Connected {
		t.Fatal("a certificate from another CA was accepted while a CA was pinned")
	}
}

func TestLocallyUnsendableRecordIsReportedAsAGap(t *testing.T) {
	hub := newFakeHub(t, nil)
	fixture := newClientFixture(t, hub.url(), func(c *Config) {
		// Smaller than one large metrics record, larger than a gap record.
		c.MaxBatchBytes = 512
	})
	if err := fixture.client.Enqueue(largeRecord(1, queueTestT0)); err != nil {
		t.Fatal(err)
	}
	fixture.start(t, context.Background())
	session := hub.waitSession(t, 2*time.Second)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		message := session.next(t, time.Until(deadline))
		samples, ok := message.(protocol.SamplesMessage)
		if !ok {
			continue
		}
		for _, record := range samples.Records {
			if gap, ok := record.(protocol.GapRecord); ok {
				if gap.Payload.Reason != protocol.GapOutboxBytes || gap.Payload.DroppedRecords != 1 {
					t.Fatalf("unsendable gap = %+v", gap.Payload)
				}
				waitFor(t, time.Second, "the drop accounting", func() bool {
					return fixture.client.Stats().Queue.DroppedRecords == 1
				})
				return
			}
			if metrics, ok := record.(protocol.MetricsRecord); ok && metrics.Sequence == 1 {
				t.Fatal("an unsendable record was reported as delivered")
			}
		}
	}
	t.Fatal("no gap record covering the unsendable sample reached the hub")
}

func TestOnDisconnectRunsForAWelcomedSession(t *testing.T) {
	hub := newFakeHub(t, nil)
	disconnected := make(chan struct{}, 4)
	fixture := newClientFixture(t, hub.url(), func(c *Config) {
		c.BackoffBase = time.Hour
		c.BackoffMax = time.Hour
		c.OnDisconnect = func() { disconnected <- struct{}{} }
	})
	fixture.start(t, context.Background())
	session := hub.waitSession(t, 2*time.Second)
	session.close(t)
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("OnDisconnect was not called")
	}
}
