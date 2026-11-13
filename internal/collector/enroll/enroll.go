package enroll

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/state"
)

// EnrollPath is the hub's enrollment endpoint.
const EnrollPath = "/api/collector/enroll"

// DefaultTimeout bounds one enrollment request attempt.
const DefaultTimeout = 5 * time.Second

// maxResponseBytes bounds the credential response; it is three short strings.
const maxResponseBytes = 4096

// Options configures one enrollment attempt.
type Options struct {
	// Store owns the durable installation identity, recovery proof and issued
	// credential. Required.
	Store *state.Store
	// CollectorID is the identity to enroll. Empty uses the stored one.
	CollectorID string
	// HubURL is the collector WebSocket endpoint, for example
	// wss://hub.example/api/collector/connect.
	HubURL string
	// Grant is the one-time enrollment token from the hub.
	Grant string
	// CAFile optionally pins a PEM CA bundle. When set it replaces the system
	// trust store for enrollment; when empty the system roots are used.
	CAFile string
	// HTTPClient overrides the transport (tests). Its TLS configuration is
	// still the caller's responsibility.
	HTTPClient *http.Client
	// Timeout bounds one attempt. Defaults to DefaultTimeout.
	Timeout time.Duration
	// Random is the entropy source for a new recovery proof.
	Random io.Reader
	// Logf receives progress and recovery notices. Optional.
	Logf func(format string, args ...any)
}

// Result is one successful enrollment.
type Result struct {
	Credential state.Credential
	MachineID  string
	// Recovered reports that this attempt reused a previously saved proof, so
	// it may be the retry that recovered a lost response.
	Recovered bool
}

// RejectionError is a non-201 enrollment answer from the hub. The hub
// deliberately answers every denial alike, so the status is the only signal.
type RejectionError struct {
	Status int
}

func (e *RejectionError) Error() string {
	if e.Status == http.StatusForbidden {
		return "enrollment rejected (403): the grant is unknown, expired or already used, " +
			"or this collector is already registered at the hub"
	}
	return fmt.Sprintf("enrollment rejected (HTTP %d); check the grant and hub enrollment state", e.Status)
}

// Run performs one enrollment attempt. It is safe to call again with the same
// Options after any error: the saved proof makes the retry recover the same
// credential.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Store == nil {
		return Result{}, errors.New("enroll: a state store is required")
	}
	hubURL, err := state.ValidateHubURL(opts.HubURL)
	if err != nil {
		return Result{}, fmt.Errorf("enroll: %w", err)
	}
	if !printableASCII(opts.Grant, 1, 256) {
		return Result{}, errors.New("enroll: the enrollment grant must be 1..256 printable ASCII characters")
	}
	collectorID := opts.CollectorID
	if collectorID == "" {
		identity, err := opts.Store.Identity()
		if err != nil {
			return Result{}, err
		}
		collectorID = identity.CollectorID
	}
	if !printableASCII(collectorID, 1, 128) {
		return Result{}, errors.New("enroll: collectorId must be 1..128 printable ASCII characters")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	pending, err := proofFor(opts, hubURL, opts.Grant)
	if err != nil {
		return Result{}, err
	}
	if pending.requestID == "" {
		return Result{}, errors.New("enroll: empty recovery proof")
	}

	client, err := httpClient(opts)
	if err != nil {
		return Result{}, err
	}

	body, err := json.Marshal(enrollRequest{
		CollectorID:    collectorID,
		RequestID:      pending.requestID,
		RecoverySecret: pending.recoverySecret,
	})
	if err != nil {
		return Result{}, fmt.Errorf("enroll: encode request: %w", err)
	}
	endpoint, err := enrollEndpoint(hubURL)
	if err != nil {
		return Result{}, err
	}

	attemptCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return Result{}, fmt.Errorf("enroll: build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+opts.Grant)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)
	if err != nil {
		// The proof stays on disk; the next attempt reuses it.
		return Result{}, fmt.Errorf("enroll: request failed; check the destination and TLS trust: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return Result{}, &RejectionError{Status: response.StatusCode}
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("enroll: read response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return Result{}, fmt.Errorf("enroll: response exceeds %d bytes", maxResponseBytes)
	}
	var issued enrollResponse
	if err := decodeStrict(data, &issued); err != nil {
		return Result{}, fmt.Errorf("enroll: invalid response: %w", err)
	}
	if issued.CollectorID != collectorID {
		return Result{}, errors.New("enroll: hub issued a credential for a different collector id")
	}
	if !printableASCII(issued.MachineID, 1, 128) {
		return Result{}, errors.New("enroll: invalid machine id in response")
	}
	if !printableASCII(issued.Token, 1, 256) {
		return Result{}, errors.New("enroll: invalid credential in response")
	}

	credential := state.Credential{URL: hubURL, Token: issued.Token}
	if err := opts.Store.CompleteEnrollment(hubURL, credential); err != nil {
		return Result{}, fmt.Errorf("enroll: hub enrollment succeeded but the local credential save failed: %w", err)
	}
	logf("enroll: credential stored for %s (machine %s)", hubURL, issued.MachineID)
	return Result{Credential: credential, MachineID: issued.MachineID, Recovered: pending.recovered}, nil
}

type enrollRequest struct {
	CollectorID    string `json:"collectorId"`
	RequestID      string `json:"requestId"`
	RecoverySecret string `json:"recoverySecret"`
}

type enrollResponse struct {
	CollectorID string `json:"collectorId"`
	MachineID   string `json:"machineId"`
	Token       string `json:"token"`
}

type proof struct {
	requestID      string
	recoverySecret string
	recovered      bool
}

// proofFor reuses the saved proof for this URL and grant when present, and
// otherwise writes a fresh one before any request is made.
func proofFor(opts Options, hubURL, grant string) (proof, error) {
	existing, ok, err := opts.Store.PendingEnrollment(hubURL)
	if err != nil {
		return proof{}, err
	}
	if ok && existing.Grant == grant {
		return proof{
			requestID:      existing.RequestID,
			recoverySecret: existing.RecoverySecret,
			recovered:      true,
		}, nil
	}
	requestID, secret, err := state.NewRecoveryProof(opts.Random)
	if err != nil {
		return proof{}, err
	}
	if err := opts.Store.SavePendingEnrollment(state.PendingEnrollment{
		URL:            hubURL,
		Grant:          grant,
		RequestID:      requestID,
		RecoverySecret: secret,
	}); err != nil {
		return proof{}, fmt.Errorf("enroll: save the recovery proof before requesting: %w", err)
	}
	return proof{requestID: requestID, recoverySecret: secret}, nil
}

func enrollEndpoint(hubURL string) (string, error) {
	parsed, err := url.Parse(hubURL)
	if err != nil {
		return "", fmt.Errorf("enroll: invalid hub url: %w", err)
	}
	if parsed.Scheme == "wss" {
		parsed.Scheme = "https"
	} else {
		parsed.Scheme = "http"
	}
	parsed.Path = EnrollPath
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func httpClient(opts Options) (*http.Client, error) {
	if opts.HTTPClient != nil {
		return opts.HTTPClient, nil
	}
	var pool *x509.CertPool
	if opts.CAFile != "" {
		// A pinned bundle replaces the system roots: a certificate issued by
		// any other CA must be rejected even if that CA is system-trusted.
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("enroll: read CA file: %w", err)
		}
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("enroll: CA file contains no usable certificate")
		}
	} else {
		system, err := x509.SystemCertPool()
		if err != nil || system == nil {
			system = x509.NewCertPool()
		}
		pool = system
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func printableASCII(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}
