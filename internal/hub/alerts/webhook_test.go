package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

type receivedDelivery struct {
	headers http.Header
	body    []byte
}

type sinkServer struct {
	*httptest.Server
	mu       sync.Mutex
	received []receivedDelivery
}

func newSink(t *testing.T) *sinkServer {
	t.Helper()
	sink := &sinkServer{}
	sink.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sink.mu.Lock()
		sink.received = append(sink.received, receivedDelivery{headers: r.Header.Clone(), body: body})
		sink.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(sink.Close)
	return sink
}

func (s *sinkServer) Received() []receivedDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]receivedDelivery(nil), s.received...)
}

func TestWebhookURLRulesAcceptHTTPSAndLoopbackHTTPOnly(t *testing.T) {
	cases := []struct {
		raw       string
		transport api.AlertWebhookTransport
	}{
		{"https://example.com/hook", api.AlertWebhookTransportHTTPS},
		{"http://127.0.0.1:9999/sink", api.AlertWebhookTransportLoopbackHTTP},
		{"http://localhost:9999/sink", api.AlertWebhookTransportLoopbackHTTP},
		{"http://[::1]:9999/sink", api.AlertWebhookTransportLoopbackHTTP},
	}
	for _, test := range cases {
		parsed, err := parseWebhookURL(test.raw)
		if err != nil {
			t.Fatalf("%q: %v", test.raw, err)
		}
		if parsed.transport != test.transport {
			t.Fatalf("%q transport = %q, want %q", test.raw, parsed.transport, test.transport)
		}
	}
	for _, raw := range []string{
		"http://example.com/hook",
		"ftp://example.com/hook",
		"https://user:pass@example.com/hook",
		"https://example.com/hook#fragment",
		"http://127.0.0.999/hook",
	} {
		if _, err := parseWebhookURL(raw); err == nil {
			t.Fatalf("%q was accepted", raw)
		}
	}
	for raw, want := range map[string]string{
		"http://example.com/hook":           "loopback HTTP",
		"ftp://example.com/hook":            "HTTPS",
		"https://user:pass@example.com/hk":  "credentials",
		"https://example.com/hook#fragment": "fragment",
	} {
		if _, err := parseWebhookURL(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q error = %v, want %q", raw, err, want)
		}
	}
}

func TestWebhookHeaderAndSecretValidation(t *testing.T) {
	if _, err := parseWebhookHeaders(map[string]string{"Host": "evil"}); err == nil {
		t.Fatal("Host header accepted")
	}
	if _, err := parseWebhookHeaders(map[string]string{"content-type": "text/plain"}); err == nil {
		t.Fatal("Content-Type header accepted")
	}
	if _, err := parseWebhookHeaders(map[string]string{"x bad": "v"}); err == nil {
		t.Fatal("invalid token accepted")
	}
	if _, err := parseWebhookHeaders(map[string]string{"x-ok": "bad\nvalue"}); err == nil {
		t.Fatal("control character accepted")
	}
	if _, err := parseWebhookHeaders(map[string]string{"x-ok": "one", " x-ok": "two"}); err == nil {
		t.Fatal("duplicate after trimming accepted")
	}
	headers, err := parseWebhookHeaders(map[string]string{"x-ok": "value"})
	if err != nil || headers["x-ok"] != "value" {
		t.Fatalf("valid headers = %+v, %v", headers, err)
	}
	if secret, err := parseWebhookSecret(stringPtr("  ")); err != nil || secret != nil {
		t.Fatalf("blank secret = %v, %v", secret, err)
	}
	if _, err := parseWebhookSecret(stringPtr(strings.Repeat("s", MaxWebhookSecret+1))); err == nil {
		t.Fatal("oversized secret accepted")
	}
}

func TestRealDeliveryUsesIdempotencyKeyAndSignature(t *testing.T) {
	sink := newSink(t)
	db, err := storage.Open(context.Background(), ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	authority := testAuthority(t, db)
	clock := newTestClock(4_000_000)
	engine, err := New(Options{Database: db, Now: clock.Now, DeliveryTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	status, err := engine.ConfigureWebhook(context.Background(), authority, WebhookConfig{
		URL:     sink.URL + "/alerts-sink",
		Headers: map[string]string{"x-controlled-sink": "yes"},
		Secret:  stringPtr("local-test-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Configured || status.Transport == nil || *status.Transport != api.AlertWebhookTransportLoopbackHTTP {
		t.Fatalf("status = %+v", status)
	}
	serialized, _ := json.Marshal(status)
	if strings.Contains(string(serialized), "127.0.0.1") || strings.Contains(string(serialized), "local-test-secret") {
		t.Fatalf("status leaks private state: %s", serialized)
	}
	if _, err := engine.CreateRule(context.Background(), authority, RuleInput{
		Name: "Sink CPU", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(1), DurationMs: 0,
	}); err != nil {
		t.Fatal(err)
	}
	engine.ObserveAt("m1", metricsRecord(cpuPayload(99), 1), Live, clock.Now())
	if err := engine.ProcessNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	received := sink.Received()
	if len(received) != 1 {
		t.Fatalf("received = %d", len(received))
	}
	summary, _ := engine.NotificationSummary(context.Background(), AllMachines())
	job := summary.Recent[0]
	delivered := received[0]
	if delivered.headers.Get("Idempotency-Key") != job.ID {
		t.Fatalf("idempotency key = %q, job = %q", delivered.headers.Get("Idempotency-Key"), job.ID)
	}
	if delivered.headers.Get("X-Controlled-Sink") != "yes" {
		t.Fatalf("custom header missing: %+v", delivered.headers)
	}
	if delivered.headers.Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q", delivered.headers.Get("Content-Type"))
	}
	mac := hmac.New(sha256.New, []byte("local-test-secret"))
	_, _ = mac.Write(delivered.body)
	wantSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if delivered.headers.Get("X-Rigscope-Signature") != wantSignature {
		t.Fatalf("signature = %q, want %q", delivered.headers.Get("X-Rigscope-Signature"), wantSignature)
	}
	var payload struct {
		Event string `json:"event"`
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(delivered.body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Event != "fired" || payload.JobID != job.ID {
		t.Fatalf("payload = %+v", payload)
	}

	result, err := engine.TestWebhook(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || len(sink.Received()) != 2 {
		t.Fatalf("test result = %+v, received = %d", result, len(sink.Received()))
	}
	afterTest, _ := engine.WebhookStatus(context.Background())
	if afterTest.LastTestOK == nil || !*afterTest.LastTestOK {
		t.Fatalf("lastTestOk = %v", afterTest.LastTestOK)
	}
	serialized, _ = json.Marshal(afterTest)
	if strings.Contains(string(serialized), sink.URL) {
		t.Fatal("webhook status leaks the URL")
	}
	engine.Stop()
}

func TestTestDeliveryRefusesNonLoopbackEndpoints(t *testing.T) {
	db, err := storage.Open(context.Background(), ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	authority := testAuthority(t, db)
	engine, err := New(Options{Database: db, Now: newTestClock(5_000_000).Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ConfigureWebhook(context.Background(), authority, WebhookConfig{URL: "https://example.invalid/alerts"}); err != nil {
		t.Fatal(err)
	}
	result, err := engine.TestWebhook(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Error == nil || !strings.Contains(*result.Error, "loopback HTTP sink") {
		t.Fatalf("result = %+v", result)
	}
	status, _ := engine.WebhookStatus(context.Background())
	if status.LastTestOK == nil || *status.LastTestOK {
		t.Fatalf("lastTestOk = %v", status.LastTestOK)
	}
	engine.Stop()
}

func TestDeliveryRefusesPrivateAndLoopbackHTTPSByDefault(t *testing.T) {
	sender := NewWebhookSender(WebhookSenderOptions{})
	for _, raw := range []string{
		"https://127.0.0.1:9/hook",
		"https://localhost:9/hook",
		"https://10.0.0.1/hook",
		"https://192.168.1.10/hook",
		"https://[::1]:9/hook",
	} {
		result := sender.Send(context.Background(), WebhookRequest{URL: raw, Body: []byte("{}"), JobID: "j"})
		if result.OK || !strings.Contains(result.Error, "not allowed") {
			t.Fatalf("%q result = %+v", raw, result)
		}
	}
	if result := sender.Send(context.Background(), WebhookRequest{URL: "http://127.0.0.1:9/hook", Body: []byte("{}"), JobID: "j"}); result.OK || strings.Contains(result.Error, "not allowed") {
		t.Fatalf("loopback HTTP sink = %+v", result)
	}
}

func TestAddressPolicyRefusesNonPublicRanges(t *testing.T) {
	sender := &webhookSender{}
	for _, raw := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.1.1",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "::1", "fc00::1", "fe80::1", "::",
	} {
		ip := net.ParseIP(raw)
		if sender.addressAllowed(ip, false) {
			t.Fatalf("%s allowed by default", raw)
		}
		if raw == "127.0.0.1" || raw == "::1" {
			if !sender.addressAllowed(ip, true) {
				t.Fatalf("%s refused for a loopback sink", raw)
			}
		}
	}
	if !sender.addressAllowed(net.ParseIP("8.8.8.8"), false) || !sender.addressAllowed(net.ParseIP("2606:4700:4700::1111"), false) {
		t.Fatal("public address refused")
	}
	private := &webhookSender{allowPrivate: true}
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.1.1", "100.64.0.1", "::1", "fc00::1"} {
		if !private.addressAllowed(net.ParseIP(raw), false) {
			t.Fatalf("%s refused with AllowPrivateTargets", raw)
		}
	}
}

func TestDeliveryDoesNotFollowRedirects(t *testing.T) {
	var targetCalls int
	var mu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targetCalls++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusFound)
	}))
	defer redirect.Close()

	sender := NewWebhookSender(WebhookSenderOptions{})
	result := sender.Send(context.Background(), WebhookRequest{
		URL: redirect.URL + "/hook", Body: []byte("{}"), JobID: "redirect-test", Timeout: time.Second,
	})
	if result.OK || !strings.Contains(result.Error, "redirect") {
		t.Fatalf("result = %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if targetCalls != 0 {
		t.Fatalf("redirect target received %d requests", targetCalls)
	}
}

func TestDeliveryTimeoutIsBounded(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer slow.Close()
	sender := NewWebhookSender(WebhookSenderOptions{})
	started := time.Now()
	result := sender.Send(context.Background(), WebhookRequest{
		URL: slow.URL + "/hook", Body: []byte("{}"), JobID: "timeout-test", Timeout: 50 * time.Millisecond,
	})
	if result.OK || result.Error != "delivery timed out" {
		t.Fatalf("result = %+v", result)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestOversizedJobPayloadIsNotEnqueued(t *testing.T) {
	h := newHarness(t)
	ruleID := h.rule("Large resource", "m1", api.AlertMetricCPUUsage, api.AlertConditionAbove, floatPtr(1), 0)
	if err := h.db.Write(context.Background(), func(tx *storage.Tx) error {
		_, err := tx.Exec("UPDATE alert_rules SET name = ? WHERE id = ?", strings.Repeat("n", 70_000), ruleID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.observe("m1", cpuPayload(99))
	h.process()
	if summary := mustSummary(t, h); len(summary.Recent) != 0 {
		t.Fatalf("oversized payload enqueued: %+v", summary.Recent)
	}
}

type staticResolver struct {
	ips []net.IP
	err error
}

func (r staticResolver) LookupIP(context.Context, string, string) ([]net.IP, error) {
	return r.ips, r.err
}

func TestLoopbackSinkRequiresEveryResolvedAddressToBeLoopback(t *testing.T) {
	mixed := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("8.8.8.8")}
	for name, options := range map[string]WebhookSenderOptions{
		"default policy":     {Resolver: staticResolver{ips: mixed}},
		"private targets on": {AllowPrivateTargets: true, Resolver: staticResolver{ips: mixed}},
		"public only":        {Resolver: staticResolver{ips: []net.IP{net.ParseIP("8.8.8.8")}}},
		"private only":       {Resolver: staticResolver{ips: []net.IP{net.ParseIP("10.0.0.1")}}},
	} {
		t.Run(name, func(t *testing.T) {
			sender := NewWebhookSender(options).(*webhookSender)
			if _, err := sender.resolveAllowed(context.Background(), "localhost", true); !errors.Is(err, errTargetRefused) {
				t.Fatalf("loopback sink resolution error = %v, want a refused target", err)
			}
		})
	}
	allowed := NewWebhookSender(WebhookSenderOptions{
		Resolver: staticResolver{ips: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}},
	}).(*webhookSender)
	addresses, err := allowed.resolveAllowed(context.Background(), "localhost", true)
	if err != nil {
		t.Fatalf("all-loopback resolution: %v", err)
	}
	if len(addresses) != 2 {
		t.Fatalf("validated addresses = %v", addresses)
	}
	// The HTTPS path keeps the operator's private-target policy: a mixed
	// answer is still refused by default.
	strict := NewWebhookSender(WebhookSenderOptions{Resolver: staticResolver{ips: mixed}}).(*webhookSender)
	if _, err := strict.resolveAllowed(context.Background(), "example.test", false); !errors.Is(err, errTargetRefused) {
		t.Fatalf("default HTTPS mixed resolution error = %v, want a refused target", err)
	}
}

func TestDialPolicyChecksResolvedAddressesAndHonoursExplicitAllow(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	address := "rebind.test:" + strconv.Itoa(port)
	resolver := staticResolver{ips: []net.IP{net.ParseIP("127.0.0.1")}}

	strict := NewWebhookSender(WebhookSenderOptions{Resolver: resolver}).(*webhookSender)
	if _, err := strict.dialContext(context.Background(), "tcp", address); !errors.Is(err, errTargetRefused) {
		t.Fatalf("strict dial error = %v, want a refused target", err)
	}
	mixed := NewWebhookSender(WebhookSenderOptions{
		Resolver: staticResolver{ips: []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("127.0.0.1")}},
	}).(*webhookSender)
	if _, err := mixed.dialContext(context.Background(), "tcp", address); !errors.Is(err, errTargetRefused) {
		t.Fatalf("mixed-answer dial error = %v, want a refused target", err)
	}

	accepted := make(chan struct{})
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			_ = connection.Close()
			close(accepted)
		}
	}()
	allowed := NewWebhookSender(WebhookSenderOptions{AllowPrivateTargets: true, Resolver: resolver}).(*webhookSender)
	connection, err := allowed.dialContext(context.Background(), "tcp", address)
	if err != nil {
		t.Fatalf("explicit AllowPrivateTargets dial: %v", err)
	}
	_ = connection.Close()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("listener never accepted the allowed connection")
	}

	// A configured loopback HTTP sink may reach loopback even when private
	// targets are not generally allowed.
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			_ = connection.Close()
		}
	}()
	sinkContext := context.WithValue(context.Background(), loopbackSinkKey{}, true)
	loopback, err := strict.dialContext(sinkContext, "tcp", address)
	if err != nil {
		t.Fatalf("loopback sink dial: %v", err)
	}
	_ = loopback.Close()
}
