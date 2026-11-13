package server_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/ingest"
	"github.com/Triet1402/rigscope/internal/hub/server"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

func pairingUI() fstest.MapFS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>Rigscope</title>")},
		"login.html": &fstest.MapFile{Data: []byte("<!doctype html><title>Sign in</title>")},
	}
}

// newPairingHub serves one colocated enrollment grant through the full security
// middleware and returns the bound handler plus the grant to redeem.
func newPairingHub(t *testing.T, hub config.Hub) (*server.Server, string, []byte) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	token, hash, err := storage.NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	registry, err := storage.NewRegistry(ctx, db, storage.RegistryOptions{BootstrapGrants: []storage.BootstrapGrant{{
		TokenHash:   hash,
		ExpiresAt:   time.Now().Add(time.Hour),
		Use:         storage.BootstrapGrantUseColocatedPairing,
		CollectorID: "local-install",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if hub.Listen == "" {
		hub.Listen = "127.0.0.1"
	}
	if hub.Port == 0 {
		hub.Port = 8081
	}
	runtime, err := server.New(server.Options{
		Hub:      hub,
		Version:  "test-version",
		HubRunID: "00000000-0000-4000-8000-000000000002",
		UI:       pairingUI(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	handler := ingest.NewEnrollmentHandler(ingest.EnrollmentOptions{Registry: registry})
	if err := runtime.Mount("/api/collector/enroll", handler); err != nil {
		t.Fatalf("mount enrollment handler: %v", err)
	}
	body, err := api.EncodeEnrollmentRequest(api.EnrollmentRequest{CollectorID: "local-install"})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, token, body
}

func redeem(runtime *server.Server, token string, body []byte, host, remote string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/collector/enroll", bytes.NewReader(body))
	r.RequestURI = r.URL.RequestURI()
	r.Host = host
	r.RemoteAddr = remote
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	runtime.Handler().ServeHTTP(recorder, r)
	return recorder
}

// TestColocatedRedemptionRefusesATrustedProxyPeer proves a reverse proxy on the
// hub's own machine cannot redeem a colocated grant on a remote caller's
// behalf, even though the proxy connection itself arrives from loopback.
func TestColocatedRedemptionRefusesATrustedProxyPeer(t *testing.T) {
	runtime, token, body := newPairingHub(t, config.Hub{TrustedProxies: []string{"127.0.0.1"}})
	response := redeem(runtime, token, body, "127.0.0.1:8081", "127.0.0.1:40990", map[string]string{
		"X-Forwarded-For": "203.0.113.9",
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("trusted proxy redemption = %d, want 403\n%s", response.Code, response.Body.String())
	}

	// A direct loopback peer with no loopback proxy trusted is still accepted, so
	// the refusal above is about the proxy hop and not about loopback.
	direct, directToken, directBody := newPairingHub(t, config.Hub{})
	accepted := redeem(direct, directToken, directBody, "127.0.0.1:8081", "127.0.0.1:40991", nil)
	if accepted.Code != http.StatusCreated {
		t.Fatalf("direct loopback redemption = %d, want 201\n%s", accepted.Code, accepted.Body.String())
	}
}

// TestColocatedRedemptionRefusesAForwardingHeaderOnALoopbackPeer proves the
// security layer records a forwarded header's presence before stripping it, so
// the enrollment handler can refuse a proxied request even when no trusted proxy
// is configured.
func TestColocatedRedemptionRefusesAForwardingHeaderOnALoopbackPeer(t *testing.T) {
	runtime, token, body := newPairingHub(t, config.Hub{})
	response := redeem(runtime, token, body, "127.0.0.1:8081", "127.0.0.1:40992", map[string]string{
		"Forwarded": "for=203.0.113.9;proto=https",
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("proxied loopback redemption = %d, want 403\n%s", response.Code, response.Body.String())
	}
}
