package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/config"
)

func writeTestCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "rigscope-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certPath := filepath.Join(directory, "cert.pem")
	keyPath := filepath.Join(directory, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestTLSListener(t *testing.T) {
	certPath, keyPath := writeTestCertificate(t)
	server := newServer(t, config.Hub{
		Listen:  "127.0.0.1",
		Port:    0,
		TLSCert: certPath,
		TLSKey:  keyPath,
	})
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	address := server.Addr().String()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // test-only self-signed certificate
		MinVersion:         tls.VersionTLS12,
	}}}
	response, err := client.Get("https://" + address + "/api/health")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	if response.TLS == nil || response.TLS.Version < tls.VersionTLS12 {
		t.Fatalf("TLS version %v", response.TLS)
	}
	if got := response.Header.Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Fatalf("HSTS %q", got)
	}

	// The listener refuses TLS versions below 1.2.
	oldClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS10,
		MaxVersion:         tls.VersionTLS11,
	}}}
	if _, err := oldClient.Get("https://" + address + "/api/health"); err == nil {
		t.Fatal("TLS 1.1 handshake succeeded against a TLS 1.2 listener")
	}
}

func TestNonLoopbackRequiresTLSOrTrustedProxy(t *testing.T) {
	base := config.Hub{Listen: "0.0.0.0", Port: 8081, Origin: "https://hub.example.com"}
	if _, err := New(Options{Hub: base, UI: testUI(), Logger: discardLogger()}); err == nil {
		t.Fatal("accepted a non-loopback plaintext listener without a trusted proxy")
	}

	withProxy := base
	withProxy.TrustedProxies = []string{"10.0.0.0/8"}
	server, err := New(Options{Hub: withProxy, UI: testUI(), Logger: discardLogger()})
	if err != nil {
		t.Fatalf("trusted reverse proxy was refused: %v", err)
	}
	if server.tls != nil {
		t.Fatal("plaintext reverse-proxy mode unexpectedly enabled TLS")
	}

	certPath, keyPath := writeTestCertificate(t)
	withTLS := base
	withTLS.TLSCert = certPath
	withTLS.TLSKey = keyPath
	if _, err := New(Options{Hub: withTLS, UI: testUI(), Logger: discardLogger()}); err != nil {
		t.Fatalf("TLS listener was refused: %v", err)
	}

	withoutOrigin := config.Hub{Listen: "0.0.0.0", Port: 8081}
	if _, err := New(Options{Hub: withoutOrigin, UI: testUI(), Logger: discardLogger()}); err == nil {
		t.Fatal("accepted a non-loopback listener without a canonical origin")
	}
}

func TestOriginAndTLSValidation(t *testing.T) {
	for _, origin := range []string{
		"https://Hub.Example.com/",
		"https://hub.example.com/path",
		"ftp://hub.example.com",
		"not a url",
	} {
		if _, err := New(Options{Hub: config.Hub{Listen: "127.0.0.1", Port: 8081, Origin: origin}, UI: testUI(), Logger: discardLogger()}); err == nil {
			t.Fatalf("accepted origin %q", origin)
		}
	}
	certPath, _ := writeTestCertificate(t)
	if _, err := New(Options{Hub: config.Hub{Listen: "127.0.0.1", Port: 8081, TLSCert: certPath}, UI: testUI(), Logger: discardLogger()}); err == nil {
		t.Fatal("accepted a certificate without a key")
	}
}

func TestMissingUIEmbeddedExport(t *testing.T) {
	// Passing an explicit filesystem without index.html must fail rather than
	// start and answer every document with 404.
	_, err := New(Options{
		Hub:    config.Hub{Listen: "127.0.0.1", Port: 8081},
		UI:     emptyFS{},
		Logger: discardLogger(),
	})
	if err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("expected a missing index.html error, got %v", err)
	}
}

type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, os.ErrNotExist }
