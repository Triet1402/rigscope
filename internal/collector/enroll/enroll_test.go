package enroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/state"
)

const testGrant = "one-time-enrollment-grant"

func newStore(t *testing.T) *state.Store {
	t.Helper()
	return openStoreAt(t, t.TempDir())
}

func openStoreAt(t *testing.T, root string) *state.Store {
	t.Helper()
	store, err := state.Open(root, state.Options{HostID: func() string { return "test-host" }})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	return store
}

func hubURLFor(server *httptest.Server, secure bool) string {
	scheme := "ws"
	if secure {
		scheme = "wss"
	}
	target := server.URL
	target = strings.TrimPrefix(target, "http://")
	target = strings.TrimPrefix(target, "https://")
	return scheme + "://" + target + state.ConnectPath
}

// enrollmentHub is a small stand-in for the Node hub's registry: a grant is
// bound to the first recovery proof, and replaying the same proof returns the
// same credential.
type enrollmentHub struct {
	mu          sync.Mutex
	grant       string
	credential  string
	machineID   string
	proof       *proofKey
	requests    int
	abortFirst  bool
	status      int
	response    func(collectorID string) []byte
	lastRequest enrollRequest
}

type proofKey struct {
	collectorID string
	requestID   string
	secret      string
}

func (h *enrollmentHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != EnrollPath {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+h.grant {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "json required", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(body) > 1024 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var request enrollRequest
	if err := decodeStrict(body, &request); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	h.requests++
	h.lastRequest = request
	if h.status != 0 {
		status := h.status
		h.mu.Unlock()
		http.Error(w, "denied", status)
		return
	}
	current := proofKey{request.CollectorID, request.RequestID, request.RecoverySecret}
	if h.proof == nil {
		h.proof = &current
		h.credential = "issued-token-" + request.CollectorID
		h.machineID = "machine-" + request.CollectorID
		abort := h.abortFirst
		h.abortFirst = false
		collectorID := request.CollectorID
		response := h.response
		h.mu.Unlock()
		if abort {
			// Simulate a response lost after the hub persisted the
			// enrollment: no bytes reach the collector.
			panic(http.ErrAbortHandler)
		}
		writeEnrollmentResponse(w, collectorID, "machine-"+collectorID, "issued-token-"+collectorID, response)
		return
	}
	if *h.proof != current {
		h.mu.Unlock()
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	collectorID := request.CollectorID
	machineID := h.machineID
	credential := h.credential
	response := h.response
	h.mu.Unlock()
	writeEnrollmentResponse(w, collectorID, machineID, credential, response)
}

func writeEnrollmentResponse(w http.ResponseWriter, collectorID, machineID, token string, custom func(string) []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if custom != nil {
		_, _ = w.Write(custom(collectorID))
		return
	}
	_ = json.NewEncoder(w).Encode(enrollResponse{CollectorID: collectorID, MachineID: machineID, Token: token})
}

func runEnroll(t *testing.T, store *state.Store, url string, mutate func(*Options)) (Result, error) {
	t.Helper()
	options := Options{
		Store:   store,
		HubURL:  url,
		Grant:   testGrant,
		Timeout: 2 * time.Second,
		Random:  rand.Reader,
	}
	if mutate != nil {
		mutate(&options)
	}
	return Run(context.Background(), options)
}

func TestEnrollmentStoresCredentialAndConsumesProof(t *testing.T) {
	hub := &enrollmentHub{grant: testGrant}
	server := httptest.NewServer(hub)
	defer server.Close()
	store := newStore(t)
	url := hubURLFor(server, false)

	result, err := runEnroll(t, store, url, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Credential.URL != url {
		t.Fatalf("credential URL = %q, want %q", result.Credential.URL, url)
	}
	if result.MachineID == "" {
		t.Fatal("empty machine id")
	}
	credential, ok, err := store.Credential(url)
	if err != nil || !ok {
		t.Fatalf("stored credential = %v, %v", ok, err)
	}
	if credential.Token == "" || credential.URL != url {
		t.Fatalf("stored credential = %+v", credential)
	}
	if _, ok, err := store.PendingEnrollment(url); err != nil || ok {
		t.Fatalf("proof survived success: ok %v, err %v", ok, err)
	}
	if hub.requests != 1 {
		t.Fatalf("requests = %d, want 1", hub.requests)
	}
	if hub.lastRequest.CollectorID == "" || hub.lastRequest.RequestID == "" ||
		len(hub.lastRequest.RecoverySecret) < 32 {
		t.Fatalf("incomplete request: %+v", hub.lastRequest)
	}
	if result.Recovered {
		t.Fatal("a first attempt is not a recovery")
	}
}

func TestLegacyFlatGrantIsUpgradedBeforeTheEnrollmentRequest(t *testing.T) {
	hub := &enrollmentHub{grant: testGrant}
	server := httptest.NewServer(hub)
	defer server.Close()
	url := hubURLFor(server, false)

	root := t.TempDir()
	store := openStoreAt(t, root)
	dir := filepath.Join(root, "collector")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"version":1,"url":"` + url + `","grant":"` + testGrant + `"}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "enrollment.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := runEnroll(t, store, url, nil)
	if err != nil {
		t.Fatalf("Run with a legacy grant: %v", err)
	}
	if result.Credential.Token == "" {
		t.Fatal("empty credential")
	}
	if hub.lastRequest.RequestID == "" || len(hub.lastRequest.RecoverySecret) < 32 {
		t.Fatalf("legacy upgrade did not generate a recovery proof: %+v", hub.lastRequest)
	}
	if _, ok, err := store.PendingEnrollment(url); err != nil || ok {
		t.Fatalf("proof survived success: ok %v, err %v", ok, err)
	}
}

func TestLostResponseIsRecoveredWithTheSameCredential(t *testing.T) {
	hub := &enrollmentHub{grant: testGrant, abortFirst: true}
	server := httptest.NewServer(hub)
	defer server.Close()
	root := t.TempDir()
	store := openStoreAt(t, root)
	url := hubURLFor(server, false)

	if _, err := runEnroll(t, store, url, nil); err == nil {
		t.Fatal("expected the first attempt to fail when the response is lost")
	}
	pending, ok, err := store.PendingEnrollment(url)
	if err != nil || !ok {
		t.Fatalf("the proof must survive a lost response: ok %v, err %v", ok, err)
	}
	if _, ok, _ := store.Credential(url); ok {
		t.Fatal("no credential may be stored for a lost response")
	}
	firstProof := pending.RequestID

	// The retry may happen after a process restart: a fresh store over the
	// same private directory must recover the same proof and credential.
	restarted := openStoreAt(t, root)
	result, err := runEnroll(t, restarted, url, nil)
	if err != nil {
		t.Fatalf("recovery Run: %v", err)
	}
	if !result.Recovered {
		t.Fatal("the retry should report itself as a recovery")
	}
	if hub.requests != 2 {
		t.Fatalf("requests = %d, want 2", hub.requests)
	}
	if hub.lastRequest.RequestID != firstProof {
		t.Fatal("the retry must reuse the saved request id")
	}
	credential, ok, err := restarted.Credential(url)
	if err != nil || !ok {
		t.Fatalf("credential after recovery = %v, %v", ok, err)
	}
	if credential.Token != hub.credential {
		t.Fatalf("token = %q, want the credential issued to the first attempt", credential.Token)
	}
	if _, ok, err := restarted.PendingEnrollment(url); err != nil || ok {
		t.Fatalf("proof survived recovery: ok %v, err %v", ok, err)
	}
}

func TestEnrollmentRejectionKeepsTheProofForRetry(t *testing.T) {
	hub := &enrollmentHub{grant: testGrant, status: http.StatusForbidden}
	server := httptest.NewServer(hub)
	defer server.Close()
	store := newStore(t)
	url := hubURLFor(server, false)

	_, err := runEnroll(t, store, url, nil)
	var rejection *RejectionError
	if err == nil || !errors.As(err, &rejection) || rejection.Status != http.StatusForbidden {
		t.Fatalf("expected a 403 RejectionError, got %v", err)
	}
	if strings.Contains(err.Error(), testGrant) {
		t.Fatal("the error leaked the enrollment grant")
	}
	if _, ok, err := store.PendingEnrollment(url); err != nil || !ok {
		t.Fatalf("the proof should remain for a retry: ok %v, err %v", ok, err)
	}
	if _, ok, _ := store.Credential(url); ok {
		t.Fatal("a rejected enrollment must not store a credential")
	}

	// A corrected grant replaces the old proof rather than reusing it.
	hub.mu.Lock()
	hub.status = 0
	hub.grant = "replacement-grant"
	hub.proof = nil
	hub.mu.Unlock()
	_, err = runEnroll(t, store, url, func(options *Options) { options.Grant = "replacement-grant" })
	if err != nil {
		t.Fatalf("enrollment with a replacement grant: %v", err)
	}
	if hub.lastRequest.RequestID == "" {
		t.Fatal("missing request id")
	}
}

func TestInvalidResponsesNeverStoreACredential(t *testing.T) {
	cases := map[string]func(string) []byte{
		"other collector": func(string) []byte {
			return []byte(`{"collectorId":"someone-else","machineId":"m","token":"t"}`)
		},
		"unknown field": func(collectorID string) []byte {
			return []byte(fmt.Sprintf(`{"collectorId":%q,"machineId":"m","token":"t","extra":1}`, collectorID))
		},
		"empty token": func(collectorID string) []byte {
			return []byte(fmt.Sprintf(`{"collectorId":%q,"machineId":"m","token":""}`, collectorID))
		},
		"oversized": func(string) []byte {
			return []byte(`{"collectorId":"` + strings.Repeat("x", maxResponseBytes) + `"}`)
		},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			hub := &enrollmentHub{grant: testGrant, response: response}
			server := httptest.NewServer(hub)
			defer server.Close()
			store := newStore(t)
			url := hubURLFor(server, false)
			if _, err := runEnroll(t, store, url, nil); err == nil {
				t.Fatal("invalid response was accepted")
			}
			if _, ok, _ := store.Credential(url); ok {
				t.Fatal("invalid response stored a credential")
			}
		})
	}
}

func TestPlaintextRemoteURLIsRejectedBeforeAnyRequest(t *testing.T) {
	store := newStore(t)
	_, err := runEnroll(t, store, "ws://hub.example.com/api/collector/connect", nil)
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("remote plaintext enrollment = %v", err)
	}
}

func TestTLSRequiresTrustedPinnedCA(t *testing.T) {
	hub := &enrollmentHub{grant: testGrant}
	caPEM, server := newTLSServer(t, hub)
	defer server.Close()
	store := newStore(t)
	url := hubURLFor(server, true)

	if _, err := runEnroll(t, store, url, nil); err == nil {
		t.Fatal("enrollment succeeded without trusting the test CA")
	}
	if _, ok, _ := store.Credential(url); ok {
		t.Fatal("untrusted TLS stored a credential")
	}

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runEnroll(t, store, url, func(options *Options) { options.CAFile = caFile }); err != nil {
		t.Fatalf("enrollment with a pinned CA: %v", err)
	}
	if _, ok, err := store.Credential(url); err != nil || !ok {
		t.Fatalf("credential after trusted TLS = %v, %v", ok, err)
	}
}

func TestInvalidCAFileIsReported(t *testing.T) {
	store := newStore(t)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runEnroll(t, store, "wss://127.0.0.1:1/api/collector/connect", func(options *Options) {
		options.CAFile = caFile
	})
	if err == nil || !strings.Contains(err.Error(), "CA file") {
		t.Fatalf("invalid CA file = %v", err)
	}
}

// newTLSServer returns a CA PEM and an HTTPS test server whose leaf certificate
// is valid for 127.0.0.1.
func newTLSServer(t *testing.T, handler http.Handler) ([]byte, *httptest.Server) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "rigscope test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "rigscope test hub"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	return caPEM, server
}
