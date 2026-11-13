// Command check runs the two release smoke tests that need a real HTTP client
// and a real HTTPS server:
//
//   - hub: start a release binary with a temporary config on a random loopback
//     port, fetch /, and require a 200 HTML response. That proves the UI export
//     is embedded in the binary.
//   - install: serve the release assets from a local HTTPS server with a
//     dedicated test CA, run scripts/install.sh against it into a fake HOME with
//     fake systemctl binaries, then run the installed binary's `version`.
//
// verify.sh invokes it with `go run ./scripts/release/check`. It is a local
// verification tool; it never publishes anything.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Triet1402/rigscope/internal/config"
)

func main() {
	mode := flag.String("mode", "", "hub or install")
	dir := flag.String("dir", "", "release artifact directory")
	binary := flag.String("binary", "", "release binary for -mode hub")
	version := flag.String("version", "", "release version")
	repoRoot := flag.String("root", ".", "repository root")
	qemu := flag.String("qemu", "", "qemu-user runner for a cross-built -mode hub binary")
	flag.Parse()

	var err error
	switch *mode {
	case "hub":
		err = checkHub(*binary, *version, *qemu)
	case "install":
		err = checkInstall(*repoRoot, *dir, *version)
	default:
		err = fmt.Errorf("unknown -mode %q", *mode)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "release check: %v\n", err)
		os.Exit(1)
	}
}

// productEnv builds the environment for a release binary or installer. Only
// values a child needs to run at all are inherited (an absolute-entry PATH, the
// locale, the timezone); every RIGSCOPE_*, DBUS_*, SYSTEMD_*, XDG_*, proxy, CA
// and callback variable from the verifying shell is dropped. Fixtures inject
// what they need explicitly, so a developer's exported runtime override cannot
// change what this smoke test proves.
func productEnv(values map[string]string) []string {
	allowed := []string{"PATH", "LANG", "LC_ALL", "TZ"}
	out := make([]string, 0, len(allowed)+len(values))
	for _, key := range allowed {
		if _, overridden := values[key]; overridden {
			continue
		}
		value, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		if key == "PATH" {
			value = sanitizedPath(value)
		}
		out = append(out, key+"="+value)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

// sanitizedPath keeps only absolute entries so a relative or empty entry cannot
// resolve a fake tool from the verify directory.
func sanitizedPath(path string) string {
	entries := strings.Split(path, string(os.PathListSeparator))
	kept := entries[:0]
	seen := map[string]bool{}
	for _, entry := range entries {
		if !filepath.IsAbs(entry) || seen[entry] {
			continue
		}
		seen[entry] = true
		kept = append(kept, entry)
	}
	return strings.Join(kept, string(os.PathListSeparator))
}

// checkHub starts the release binary's hub on a free loopback port with a
// temporary config and asserts that / serves the embedded UI. The process is
// reaped on every return path, including failures, so a broken candidate never
// leaves a listening hub behind.
func checkHub(binary, version, qemu string) error {
	if binary == "" {
		return errors.New("-binary is required for -mode hub")
	}
	root, err := os.MkdirTemp("", "rigscope-hub-smoke-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	port, err := freeLoopbackPort()
	if err != nil {
		return err
	}
	cfg := config.Default()
	cfg.Components.Hub.Port = port
	cfg.Components.Collector.Enabled = false
	configPath := filepath.Join(root, "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o600); err != nil {
		return err
	}
	stateDir := filepath.Join(root, "state")
	home := filepath.Join(root, "home")
	runDir := filepath.Join(root, "run")
	for _, dir := range []string{stateDir, home, runDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}

	command := newReleaseCommand(qemu, binary, "hub", "--config", configPath, "--state-dir", stateDir)
	command.Env = productEnv(map[string]string{
		"HOME":            home,
		"XDG_RUNTIME_DIR": runDir,
	})
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()

	// exited records that the loop consumed the single Wait result, so the
	// deferred stop does not wait on an already-drained channel.
	var exitErr error
	exited := false
	stopped := false
	// stop always reaps the child. It sends SIGTERM first and kills after a
	// bound, so a hung or already-dead process cannot make the verifier wait.
	stop := func() error {
		if stopped {
			return nil
		}
		stopped = true
		if exited {
			return exitErr
		}
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			return err
		case <-time.After(15 * time.Second):
			_ = command.Process.Kill()
			<-done
			return errors.New("hub did not stop after SIGTERM")
		}
	}
	defer func() { _ = stop() }()

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-done:
			exited = true
			exitErr = err
			return fmt.Errorf("hub exited before serving the UI: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("hub did not serve %s within 30s\nstdout: %s\nstderr: %s", url, stdout.String(), stderr.String())
		}
		response, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK {
				if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
					return fmt.Errorf("GET / content type = %q, want text/html", response.Header.Get("Content-Type"))
				}
				if !bytes.Contains(bytes.ToLower(body), []byte("<html")) {
					return fmt.Errorf("GET / did not return an HTML document")
				}
				break
			}
			if readErr == nil && response.StatusCode != http.StatusOK {
				return fmt.Errorf("GET / status = %d, want 200; body: %s", response.StatusCode, truncate(body, 512))
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := stop(); err != nil {
		return fmt.Errorf("hub did not shut down cleanly: %v\nstderr: %s", err, stderr.String())
	}

	reported, err := binaryVersion(qemu, binary)
	if err != nil {
		return err
	}
	if version != "" && reported != version {
		return fmt.Errorf("release binary reports version %q, expected %q", reported, version)
	}
	fmt.Printf("hub smoke: served embedded UI on 127.0.0.1:%d (version %s)\n", port, reported)
	return nil
}

// newReleaseCommand builds a command for the release binary, optionally through
// a qemu-user runner for a cross-built architecture.
func newReleaseCommand(qemu, binary string, args ...string) *exec.Cmd {
	if qemu == "" {
		return exec.Command(binary, args...)
	}
	return exec.Command(qemu, append([]string{binary}, args...)...)
}

// checkInstall serves the release assets over HTTPS, runs the host installer
// into a fake HOME with fake systemctl commands, and checks the installed
// binary's version.
func checkInstall(repoRoot, dir, version string) error {
	if dir == "" || version == "" {
		return errors.New("-dir and -version are required for -mode install")
	}
	script := filepath.Join(repoRoot, "scripts", "install.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("scripts/install.sh: %w", err)
	}
	root, err := os.MkdirTemp("", "rigscope-install-smoke-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	home := filepath.Join(root, "home")
	runDir := filepath.Join(root, "run")
	binDir := filepath.Join(root, "fake-bin")
	tmpDir := filepath.Join(root, "tmp")
	for _, path := range []string{home, runDir, binDir, tmpDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
	}
	logPath := filepath.Join(root, "commands.log")
	for _, name := range []string{"systemctl", "systemd-sysusers", "udevadm", "loginctl", "journalctl"} {
		content := "#!/bin/sh\nprintf '%s %s\\n' \"" + name + "\" \"$*\" >> \"$RIGSCOPE_TEST_COMMAND_LOG\"\nexit 0\n"
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(content), 0o755); err != nil {
			return err
		}
	}

	handler, err := releaseHandler(dir, version)
	if err != nil {
		return err
	}
	server, caPEM, err := newTLSServer(handler)
	if err != nil {
		return err
	}
	defer server.Close()
	caFile := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		return err
	}

	command := exec.Command("sh", script, "--role", "hub", "--no-start")
	command.Env = productEnv(map[string]string{
		"PATH":                      sanitizedPath(binDir + string(os.PathListSeparator) + os.Getenv("PATH")),
		"HOME":                      home,
		"XDG_RUNTIME_DIR":           runDir,
		"TMPDIR":                    tmpDir,
		"RIGSCOPE_VERSION":          version,
		"RIGSCOPE_INSTALL_BASE_URL": server.URL,
		"RIGSCOPE_TEST_COMMAND_LOG": logPath,
		"CURL_CA_BUNDLE":            caFile,
		"SSL_CERT_FILE":             caFile,
	})
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("install.sh failed: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("Installed Rigscope "+version)) {
		return fmt.Errorf("install.sh did not report version %s:\n%s", version, output)
	}

	installed := filepath.Join(home, ".local", "bin", "rigscope")
	info, err := os.Stat(installed)
	if err != nil {
		return fmt.Errorf("installed binary: %w", err)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("installed binary %s is not executable", installed)
	}
	reported, err := binaryVersion("", installed)
	if err != nil {
		return err
	}
	if reported != version {
		return fmt.Errorf("installed binary reports version %q, expected %q", reported, version)
	}
	fmt.Printf("install smoke: scripts/install.sh installed %s from %s and it reports version %s\n", installed, server.URL, reported)
	return nil
}

// releaseHandler serves exactly the release assets that install.sh requests.
func releaseHandler(dir, version string) (http.Handler, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := "/download/v" + version + "/"
	assets := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		assets[entry.Name()] = filepath.Join(dir, entry.Name())
	}
	if _, ok := assets["checksums.txt"]; !ok {
		return nil, errors.New("artifact directory has no checksums.txt")
	}
	mux := http.NewServeMux()
	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		path, ok := assets[name]
		if name == "" || !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, path)
	})
	return mux, nil
}

// newTLSServer starts an HTTPS server with a dedicated test CA. The leaf
// certificate carries 127.0.0.1 as an IP SAN so curl and wget accept it with
// CURL_CA_BUNDLE/SSL_CERT_FILE.
func newTLSServer(handler http.Handler) (*httptest.Server, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Rigscope Release Test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{leafDER},
		PrivateKey:  leafKey,
	}}}
	server.StartTLS()
	return server, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), nil
}

func freeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("unexpected listener address")
	}
	return address.Port, nil
}

func binaryVersion(qemu, binary string) (string, error) {
	command := newReleaseCommand(qemu, binary, "version")
	command.Env = productEnv(nil)
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("%s version: %w", binary, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return "", fmt.Errorf("%s version printed nothing", binary)
	}
	return fields[len(fields)-1], nil
}

func truncate(data []byte, limit int) string {
	if len(data) <= limit {
		return string(data)
	}
	return string(data[:limit]) + "…"
}
