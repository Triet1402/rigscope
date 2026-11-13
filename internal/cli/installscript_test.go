package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestInstallScriptRealArchive runs scripts/install.sh under sh against a real
// tar.gz served over HTTPS, exactly as curl|sh does in production. It covers
// the GNU tar size-column bug: the script must list members with -tzf, reject
// unexpected paths, extract only rigscope and run it with the original args.
func TestInstallScriptRealArchive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("install.sh is Linux-only")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, err := exec.LookPath("shasum"); err != nil {
			t.Skip("no SHA-256 tool available")
		}
	}
	arch, ok := scriptArch()
	if !ok {
		t.Skip("unsupported test architecture")
	}

	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("install.sh: %v", err)
	}

	t.Run("valid archive", func(t *testing.T) {
		argsFile := filepath.Join(t.TempDir(), "args")
		fake := []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$RIGSCOPE_TEST_ARGS_FILE\"\n")
		archive := scriptArchive(t, map[string]scriptMember{
			"rigscope": {content: fake, mode: 0o755},
		})
		server, caFile := serveArchive(t, "1.0.0", arch, archive)
		runScript(t, sh, script, server, caFile, argsFile, "--role", "collector")
		got, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatalf("installer was not run: %v", err)
		}
		if string(got) != "install\n--role\ncollector\n" {
			t.Fatalf("installer args = %q", got)
		}
	})

	t.Run("rejects extra member", func(t *testing.T) {
		argsFile := filepath.Join(t.TempDir(), "args")
		fake := []byte("#!/bin/sh\ntouch \"$RIGSCOPE_TEST_ARGS_FILE\"\n")
		archive := scriptArchive(t, map[string]scriptMember{
			"rigscope": {content: fake, mode: 0o755},
			"extra":    {content: []byte("x"), mode: 0o644},
		})
		server, caFile := serveArchive(t, "1.0.0", arch, archive)
		if err := runScriptExpectError(sh, script, server, caFile, argsFile); err == nil {
			t.Fatal("archive with an extra member was accepted")
		}
		if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
			t.Fatal("installer ran from a rejected archive")
		}
	})

	t.Run("rejects path traversal", func(t *testing.T) {
		argsFile := filepath.Join(t.TempDir(), "args")
		fake := []byte("#!/bin/sh\ntouch \"$RIGSCOPE_TEST_ARGS_FILE\"\n")
		archive := scriptArchive(t, map[string]scriptMember{
			"../rigscope": {content: fake, mode: 0o755},
		})
		server, caFile := serveArchive(t, "1.0.0", arch, archive)
		if err := runScriptExpectError(sh, script, server, caFile, argsFile); err == nil {
			t.Fatal("path traversal archive was accepted")
		}
	})
}

type scriptMember struct {
	content []byte
	mode    int64
}

func scriptArch() (string, bool) {
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return "", false
	}
	switch strings.TrimSpace(string(out)) {
	case "x86_64", "amd64":
		return "amd64", true
	case "aarch64", "arm64":
		return "arm64", true
	case "armv7l", "armv7":
		return "armv7", true
	default:
		return "", false
	}
}

func scriptArchive(t *testing.T, members map[string]scriptMember) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gz)
	for name, member := range members {
		header := &tar.Header{Name: name, Mode: member.mode, Size: int64(len(member.content)), Typeflag: tar.TypeReg}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(member.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func serveArchive(t *testing.T, version, arch string, archive []byte) (*httptest.Server, string) {
	t.Helper()
	asset := fmt.Sprintf("rigscope_%s_linux_%s.tar.gz", version, arch)
	sum := sha256.Sum256(archive)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n")
	mux := http.NewServeMux()
	mux.HandleFunc("/download/v"+version+"/"+asset, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/download/v"+version+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(checksums)
	})
	server, caPEM := newTLSServer(t, mux)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return server, caFile
}

// newTLSServer starts an HTTPS server with a dedicated test CA. httptest's own
// certificate carries a serverAuth EKU, which current OpenSSL refuses to use
// as a CA; a purpose-built CA with no EKU verifies in curl and wget alike.
func newTLSServer(t *testing.T, handler http.Handler) (*httptest.Server, []byte) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Rigscope Test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{leafDER},
		PrivateKey:  leafKey,
	}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func scriptEnv(server *httptest.Server, caFile, argsFile string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + os.TempDir(),
		"RIGSCOPE_VERSION=1.0.0",
		"RIGSCOPE_INSTALL_BASE_URL=" + server.URL,
		"RIGSCOPE_TEST_ARGS_FILE=" + argsFile,
		"CURL_CA_BUNDLE=" + caFile,
		"SSL_CERT_FILE=" + caFile,
	}
}

func runScript(t *testing.T, sh, script string, server *httptest.Server, caFile, argsFile string, args ...string) {
	t.Helper()
	if err := runScriptExpectError(sh, script, server, caFile, argsFile, args...); err != nil {
		t.Fatalf("install.sh failed: %v", err)
	}
}

func runScriptExpectError(sh, script string, server *httptest.Server, caFile, argsFile string, args ...string) error {
	command := exec.Command(sh, append([]string{script}, args...)...)
	command.Env = scriptEnv(server, caFile, argsFile)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, output)
	}
	return nil
}
