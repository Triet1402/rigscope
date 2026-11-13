package ingest_test

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/ingest"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

const pairingCollectorID = "local-install"

// pairingFixture is one hub-side colocated grant with a handler in front of it.
type pairingFixture struct {
	handler *ingest.EnrollmentHandler
	token   string
	body    []byte
}

func newPairingFixture(t *testing.T) *pairingFixture {
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
		CollectorID: pairingCollectorID,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := api.EncodeEnrollmentRequest(api.EnrollmentRequest{CollectorID: pairingCollectorID})
	if err != nil {
		t.Fatal(err)
	}
	return &pairingFixture{
		handler: ingest.NewEnrollmentHandler(ingest.EnrollmentOptions{Registry: registry}),
		token:   token,
		body:    body,
	}
}

// request sends one enrollment request as if the TCP socket had the given peer
// address and local address.
func (f *pairingFixture) request(remote string, local net.Addr, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/collector/enroll", bytes.NewReader(f.body))
	r.RemoteAddr = remote
	if local != nil {
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
	}
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, r)
	return recorder
}

// TestAutomaticPairingGrantUsesTheSocketPeer proves the locality rule reads the
// real TCP socket. A forwarding header naming 127.0.0.1 must not turn a remote
// caller into a local one.
func TestAutomaticPairingGrantUsesTheSocketPeer(t *testing.T) {
	f := newPairingFixture(t)
	remote := f.request("203.0.113.5:41234", nil, map[string]string{
		"X-Forwarded-For":   "127.0.0.1",
		"X-Forwarded-Proto": "http",
		"Forwarded":         "for=127.0.0.1",
	})
	if remote.Code != http.StatusForbidden {
		t.Fatalf("remote enrollment with spoofed forwarding headers = %d, want 403\n%s", remote.Code, remote.Body.String())
	}
	local := f.request("127.0.0.1:41234", nil, nil)
	if local.Code != http.StatusCreated {
		t.Fatalf("loopback enrollment = %d, want 201\n%s", local.Code, local.Body.String())
	}
}

// TestEnrollmentAcceptsASelfConnectionToTheHubsOwnAddress is the origin+TLS
// case: the collector dials the hub's origin name, which resolves to one of the
// hub host's own addresses, so the peer address equals the listening socket's
// local address. That is a local connection and must redeem.
func TestEnrollmentAcceptsASelfConnectionToTheHubsOwnAddress(t *testing.T) {
	f := newPairingFixture(t)
	local := &net.TCPAddr{IP: net.ParseIP("10.10.10.2"), Port: 8443}
	response := f.request("10.10.10.2:52100", local, nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("self connection enrollment = %d, want 201\n%s", response.Code, response.Body.String())
	}
}

// TestEnrollmentRejectsAForeignPeerWithALocalAddress proves a remote peer is
// refused even when the local address is one of the host's own addresses.
func TestEnrollmentRejectsAForeignPeerWithALocalAddress(t *testing.T) {
	f := newPairingFixture(t)
	local := &net.TCPAddr{IP: net.ParseIP("10.10.10.2"), Port: 8443}
	response := f.request("198.51.100.7:52100", local, nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("remote enrollment = %d, want 403\n%s", response.Code, response.Body.String())
	}
}

// TestColocatedRedemptionRejectsForwardingHeaders proves a loopback peer that
// reached the hub through a forwarding hop is refused. A reverse proxy on the
// same host must not launder a remote caller into a local one, and the security
// layer records the header's presence before it strips it.
func TestColocatedRedemptionRejectsForwardingHeaders(t *testing.T) {
	cases := map[string]string{
		"forwarded":        "for=203.0.113.9",
		"x-forwarded-for":  "203.0.113.9",
		"x-forwarded-host": "hub.example.com",
		"x-real-ip":        "203.0.113.9",
		"via":              "1.1 squid",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPairingFixture(t)
			header := http.CanonicalHeaderKey(name)
			response := f.request("127.0.0.1:41234", nil, map[string]string{header: value})
			if response.Code != http.StatusForbidden {
				t.Fatalf("%s from a loopback peer = %d, want 403\n%s", name, response.Code, response.Body.String())
			}
		})
	}
}
