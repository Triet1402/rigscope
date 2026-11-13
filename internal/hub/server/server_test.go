package server

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Triet1402/rigscope/internal/config"
)

func testUI() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           &fstest.MapFile{Data: []byte("<!doctype html><title>Fleet</title><p>home</p>")},
		"login.html":           &fstest.MapFile{Data: []byte("<!doctype html><title>Sign in</title>")},
		"404.html":             &fstest.MapFile{Data: []byte("<!doctype html><title>Not found</title>")},
		"_next/static/app.js":  &fstest.MapFile{Data: []byte("console.log('app')")},
		"_next/static/app.css": &fstest.MapFile{Data: []byte("body{}")},
	}
}

func newServer(t *testing.T, hub config.Hub, options ...func(*Options)) *Server {
	t.Helper()
	if hub.Listen == "" {
		hub.Listen = "127.0.0.1"
	}
	opts := Options{
		Hub:      hub,
		Version:  "test-version",
		HubRunID: "00000000-0000-4000-8000-000000000001",
		UI:       testUI(),
		Logger:   discardLogger(),
	}
	for _, apply := range options {
		apply(&opts)
	}
	server, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func request(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	// httptest.NewRequest copies the absolute target into RequestURI. A real
	// client sends origin-form, which is the only target form the boundary
	// accepts, so tests must send the same.
	req.RequestURI = req.URL.RequestURI()
	return req
}

func serve(server *Server, req *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	return recorder
}

func TestHealthDocument(t *testing.T) {
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081})
	response := serve(server, request(http.MethodGet, "http://127.0.0.1:8081/api/health"))
	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	want := `{"status":"ok","version":"test-version","mode":"go","hubRunId":"00000000-0000-4000-8000-000000000001"}`
	if response.Body.String() != want {
		t.Fatalf("body %q, want %q", response.Body.String(), want)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content type %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache %q", got)
	}

	head := serve(server, request(http.MethodHead, "http://127.0.0.1:8081/api/health"))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD: status %d body %q", head.Code, head.Body.String())
	}
	if got := head.Header().Get("Content-Length"); got != "" {
		t.Fatalf("HEAD content length %q (expected Go to send none for a manual HEAD)", got)
	}

	post := serve(server, request(http.MethodPost, "http://127.0.0.1:8081/api/health"))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", post.Code)
	}
	if got := post.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow %q", got)
	}
	if post.Body.String() != "method not allowed" {
		t.Fatalf("body %q", post.Body.String())
	}
}

func TestMountsAndFallbacks(t *testing.T) {
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081})
	if err := server.Mount("/api/things", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "api %s", r.URL.Path)
	})); err != nil {
		t.Fatal(err)
	}
	if err := server.Mount("/api/collector/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "collector")
	})); err != nil {
		t.Fatal(err)
	}
	if err := server.Mount("/api/specific", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "specific")
	})); err != nil {
		t.Fatal(err)
	}

	api := serve(server, request(http.MethodGet, "http://127.0.0.1:8081/api/things"))
	if api.Code != http.StatusOK || api.Body.String() != "api /api/things" {
		t.Fatalf("api: %d %q", api.Code, api.Body.String())
	}
	collector := serve(server, request(http.MethodGet, "http://127.0.0.1:8081/api/collector/other"))
	if collector.Body.String() != "collector" {
		t.Fatalf("collector mount: %q", collector.Body.String())
	}
	specific := serve(server, request(http.MethodGet, "http://127.0.0.1:8081/api/specific"))
	if specific.Body.String() != "specific" {
		t.Fatalf("specific mount: %q", specific.Body.String())
	}
	health := serve(server, request(http.MethodGet, "http://127.0.0.1:8081/api/health"))
	if !strings.Contains(health.Body.String(), `"status":"ok"`) {
		t.Fatalf("health was shadowed: %q", health.Body.String())
	}

	unknown := serve(server, request(http.MethodGet, "http://127.0.0.1:8081/api/unknown"))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown API status %d", unknown.Code)
	}
	if got := unknown.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("unknown API content type %q", got)
	}

	home := serve(server, request(http.MethodGet, "http://127.0.0.1:8081/"))
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "Fleet") {
		t.Fatalf("UI: %d %q", home.Code, home.Body.String())
	}
}

func TestCollectorHTTPWithoutUpgrade(t *testing.T) {
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081})
	response := serve(server, request(http.MethodGet, "http://127.0.0.1:8081/api/collector/connect"))
	if response.Code != http.StatusUpgradeRequired {
		t.Fatalf("status %d", response.Code)
	}
	if got := response.Header().Get("Upgrade"); got != "websocket" {
		t.Fatalf("Upgrade %q", got)
	}
}

func TestLoopbackHostMatrix(t *testing.T) {
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081})
	for _, host := range []string{"127.0.0.1:8081", "localhost:8081", "[::1]:8081"} {
		req := request(http.MethodGet, "http://127.0.0.1:8081/api/health")
		req.Host = host
		response := serve(server, req)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status %d body %q", host, response.Code, response.Body.String())
		}
	}

	for _, host := range []string{"127.0.0.1:8082", "evil.example.com:8081", "127.0.0.1"} {
		req := request(http.MethodGet, "http://127.0.0.1:8081/api/health")
		req.Host = host
		response := serve(server, req)
		if response.Code != http.StatusMisdirectedRequest {
			t.Fatalf("%s: status %d, want 421", host, response.Code)
		}
	}

	missing := request(http.MethodGet, "http://127.0.0.1:8081/api/health")
	missing.Host = ""
	missing.Header.Del("Host")
	if response := serve(server, missing); response.Code != http.StatusBadRequest {
		t.Fatalf("missing host: status %d", response.Code)
	}

	duplicate := request(http.MethodGet, "http://127.0.0.1:8081/api/health")
	duplicate.Header["Host"] = []string{"127.0.0.1:8081", "evil.example.com"}
	if response := serve(server, duplicate); response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate host: status %d", response.Code)
	}
}

func TestLoopbackOriginMatrix(t *testing.T) {
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081})
	if err := server.Mount("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})); err != nil {
		t.Fatal(err)
	}
	base := "http://127.0.0.1:8081/api/mutation"

	same := request(http.MethodPost, base)
	same.Header.Set("Origin", "http://127.0.0.1:8081")
	if response := serve(server, same); response.Code != http.StatusOK {
		t.Fatalf("same origin: status %d body %q", response.Code, response.Body.String())
	}

	missing := request(http.MethodPost, base)
	if response := serve(server, missing); response.Code != http.StatusForbidden {
		t.Fatalf("missing origin: status %d", response.Code)
	}
	for _, origin := range []string{
		"http://evil.example",
		"http://127.0.0.1:8082",
		"https://127.0.0.1:8081",
		"null",
		"http://127.0.0.1:8081/path",
	} {
		foreign := request(http.MethodPost, base)
		foreign.Header.Set("Origin", origin)
		if response := serve(server, foreign); response.Code != http.StatusForbidden {
			t.Fatalf("%s: status %d", origin, response.Code)
		}
	}

	// Safe reads do not need an Origin in loopback mode (matching the Node hub).
	read := request(http.MethodGet, base)
	read.Header.Set("Origin", "http://evil.example")
	if response := serve(server, read); response.Code != http.StatusOK {
		t.Fatalf("foreign Origin on GET: status %d", response.Code)
	}

	// Collector endpoints carry a grant instead of a browser Origin.
	enroll := request(http.MethodPost, "http://127.0.0.1:8081/api/collector/enroll")
	if response := serve(server, enroll); response.Code == http.StatusForbidden {
		t.Fatalf("collector enroll was origin-rejected: %d", response.Code)
	}
}

func TestRemoteOriginMatrix(t *testing.T) {
	const origin = "https://rig.example.test:8443"
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081, Origin: origin})
	if err := server.Mount("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})); err != nil {
		t.Fatal(err)
	}
	requestFor := func(method, host, headerOrigin string) *http.Request {
		req := request(method, "https://"+host+"/api/mutation")
		req.Host = host
		if headerOrigin != "" {
			req.Header.Set("Origin", headerOrigin)
		}
		return req
	}

	if response := serve(server, requestFor(http.MethodGet, "rig.example.test:8443", "")); response.Code != http.StatusOK {
		t.Fatalf("plain read: status %d", response.Code)
	}
	if response := serve(server, requestFor(http.MethodGet, "rig.example.test:8443", origin)); response.Code != http.StatusOK {
		t.Fatalf("same-origin read: status %d", response.Code)
	}
	if response := serve(server, requestFor(http.MethodGet, "rig.example.test:8443", "https://evil.example.test")); response.Code != http.StatusForbidden {
		t.Fatalf("foreign Origin on read: status %d", response.Code)
	}
	if response := serve(server, requestFor(http.MethodPost, "rig.example.test:8443", origin)); response.Code != http.StatusOK {
		t.Fatalf("mutation same origin: status %d", response.Code)
	}
	if response := serve(server, requestFor(http.MethodPost, "rig.example.test:8443", "")); response.Code != http.StatusForbidden {
		t.Fatalf("mutation without Origin: status %d", response.Code)
	}
	// The loopback fallback Host is no longer accepted in remote mode: only the
	// canonical Host is.
	if response := serve(server, requestFor(http.MethodGet, "127.0.0.1:8081", "")); response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("loopback Host in remote mode: status %d", response.Code)
	}
	if response := serve(server, requestFor(http.MethodPost, "127.0.0.1:8081", origin)); response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("mutation on loopback Host: status %d", response.Code)
	}
	if response := serve(server, requestFor(http.MethodGet, "evil.example.test", "")); response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host: status %d", response.Code)
	}

	// The collector on the hub's own machine keeps its loopback destination:
	// collector endpoints accept the loopback Host from a direct loopback peer
	// that carries no forwarding header. Nothing else does.
	collector := func(path, host, remote string, headers map[string]string) int {
		req := request(http.MethodPost, "http://"+host+path)
		req.Host = host
		req.RemoteAddr = remote
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		return serve(server, req).Code
	}
	for _, path := range []string{"/api/collector/connect", "/api/collector/enroll"} {
		for _, host := range []string{"127.0.0.1:8081", "localhost:8081", "[::1]:8081"} {
			if code := collector(path, host, "127.0.0.1:50000", nil); code == http.StatusMisdirectedRequest {
				t.Fatalf("local collector %s via %s: status %d", path, host, code)
			}
		}
		refused := []struct {
			name    string
			host    string
			remote  string
			headers map[string]string
		}{
			{"remote peer", "127.0.0.1:8081", "192.0.2.7:50000", nil},
			{"wrong port", "127.0.0.1:9999", "127.0.0.1:50000", nil},
			{"non-loopback host", "evil.example.test:8081", "127.0.0.1:50000", nil},
			{"proxied (X-Forwarded-For)", "127.0.0.1:8081", "127.0.0.1:50000", map[string]string{"X-Forwarded-For": "100.64.0.9"}},
			{"proxied (Forwarded)", "127.0.0.1:8081", "127.0.0.1:50000", map[string]string{"Forwarded": "for=100.64.0.9"}},
			{"proxied (Via)", "127.0.0.1:8081", "127.0.0.1:50000", map[string]string{"Via": "1.1 proxy"}},
		}
		for _, tc := range refused {
			if code := collector(path, tc.host, tc.remote, tc.headers); code != http.StatusMisdirectedRequest {
				t.Fatalf("%s %s: status %d, want 421", path, tc.name, code)
			}
		}
	}
	// Browser routes from the same local peer still need the canonical Host.
	if code := collector("/api/mutation", "127.0.0.1:8081", "127.0.0.1:50000", nil); code != http.StatusMisdirectedRequest {
		t.Fatalf("loopback Host on a browser route: status %d, want 421", code)
	}

	// A present-but-empty Origin is a foreign origin on reads too.
	emptyOrigin := request(http.MethodGet, "https://rig.example.test:8443/api/mutation")
	emptyOrigin.Host = "rig.example.test:8443"
	emptyOrigin.Header["Origin"] = []string{""}
	if response := serve(server, emptyOrigin); response.Code != http.StatusForbidden {
		t.Fatalf("empty Origin on read: status %d", response.Code)
	}

	// HTTPS origin behind a trusted proxy earns HSTS.
	read := serve(server, requestFor(http.MethodGet, "rig.example.test:8443", ""))
	if got := read.Header().Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Fatalf("HSTS %q", got)
	}
}

func TestRequestTargetValidation(t *testing.T) {
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081})

	for _, target := range []string{
		"//evil.example/path",
		"ftp://evil.example/path",
		`/\evil.example/path`,
		"/%5Cevil.example/path",
		"/path#fragment",
		"https://evil.example/path",
		"https://evil.example//path",
		"https://user:pass@evil.example/path",
		"http://127.0.0.1:8081/api/health",
	} {
		req := request(http.MethodGet, "http://127.0.0.1:8081/")
		req.RequestURI = target
		if response := serve(server, req); response.Code != http.StatusBadRequest {
			t.Fatalf("%q: status %d, want 400", target, response.Code)
		}
	}

	// CONNECT is never proxied by the hub.
	connect := request(http.MethodConnect, "http://127.0.0.1:8081/")
	connect.RequestURI = "127.0.0.1:8081"
	if response := serve(server, connect); response.Code != http.StatusBadRequest {
		t.Fatalf("CONNECT: status %d", response.Code)
	}

	duplicateOrigin := request(http.MethodPost, "http://127.0.0.1:8081/api/x")
	duplicateOrigin.Header["Origin"] = []string{"http://127.0.0.1:8081", "http://127.0.0.1:8081"}
	if response := serve(server, duplicateOrigin); response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate origin: status %d", response.Code)
	}
}

func TestForwardedHeaderTrust(t *testing.T) {
	var seen http.Header
	var clientIP string
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		clientIP = ClientIP(r)
		w.WriteHeader(http.StatusOK)
	})

	untrusted := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081})
	if err := untrusted.Mount("/api/", capture); err != nil {
		t.Fatal(err)
	}
	req := request(http.MethodGet, "http://127.0.0.1:8081/api/x")
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "evil.example.com")
	req.Header.Set("Forwarded", "for=203.0.113.9;proto=https")
	req.Header.Set("X-Real-IP", "203.0.113.9")
	if response := serve(untrusted, req); response.Code != http.StatusOK {
		t.Fatalf("untrusted status %d", response.Code)
	}
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Forwarded"} {
		if value := seen.Get(header); value != "" {
			t.Fatalf("untrusted peer kept %s=%q", header, value)
		}
	}
	if seen.Get("X-Real-IP") != "203.0.113.9" {
		t.Fatalf("unrelated header was rewritten: %q", seen.Get("X-Real-IP"))
	}
	if clientIP != "192.0.2.10" {
		t.Fatalf("client IP %q, want the TCP peer", clientIP)
	}

	trusted := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081, TrustedProxies: []string{"10.0.0.0/8"}})
	if err := trusted.Mount("/api/", capture); err != nil {
		t.Fatal(err)
	}
	req = request(http.MethodGet, "http://127.0.0.1:8081/api/x")
	req.RemoteAddr = "10.1.2.3:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.1.2.3")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "hub.example.com")
	req.Header.Set("Forwarded", "for=198.51.100.4;proto=https;host=other.example.com")
	if response := serve(trusted, req); response.Code != http.StatusOK {
		t.Fatalf("trusted status %d", response.Code)
	}
	if got := seen.Get("X-Forwarded-For"); got != "203.0.113.9" {
		t.Fatalf("trusted X-Forwarded-For %q", got)
	}
	if got := seen.Get("X-Forwarded-Proto"); got != "https" {
		t.Fatalf("trusted X-Forwarded-Proto %q", got)
	}
	if got := seen.Get("X-Forwarded-Host"); got != "hub.example.com" {
		t.Fatalf("trusted X-Forwarded-Host %q", got)
	}
	if clientIP != "203.0.113.9" {
		t.Fatalf("trusted client IP %q", clientIP)
	}
	if Scheme(req) != "https" {
		t.Fatalf("scheme %q", Scheme(req))
	}

	// A spoofed X-Forwarded-Host can never satisfy the canonical Host check.
	spoofed := request(http.MethodGet, "http://127.0.0.1:8081/api/x")
	spoofed.RemoteAddr = "10.1.2.3:1234"
	spoofed.Host = "evil.example.com"
	spoofed.Header.Set("X-Forwarded-Host", "127.0.0.1:8081")
	if response := serve(trusted, spoofed); response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("spoofed forwarded host: status %d", response.Code)
	}
}

func TestForwardedChainWalksFromTheRight(t *testing.T) {
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Client", ClientIP(r))
		w.Header().Set("X-Seen-Scheme", Scheme(r))
		w.WriteHeader(http.StatusOK)
	})
	server := newServer(t, config.Hub{
		Listen:         "127.0.0.1",
		Port:           8081,
		TrustedProxies: []string{"10.0.0.0/8", "192.0.2.1"},
	})
	if err := server.Mount("/api/", capture); err != nil {
		t.Fatal(err)
	}
	call := func(remote string, header map[string][]string) *httptest.ResponseRecorder {
		req := request(http.MethodGet, "http://127.0.0.1:8081/api/x")
		req.RemoteAddr = remote
		for key, values := range header {
			req.Header[key] = values
		}
		return serve(server, req)
	}

	// The rightmost untrusted hop is the client; trusted hops are skipped and
	// an attacker-supplied leftmost value is never used.
	response := call("10.1.2.3:1234", map[string][]string{
		"X-Forwarded-For": {"6.6.6.6, 203.0.113.9, 10.4.5.6"},
	})
	if got := response.Header().Get("X-Seen-Client"); got != "203.0.113.9" {
		t.Fatalf("client = %q, want 203.0.113.9", got)
	}

	// A single-hop chain with a spoofed prefix: the proxy appends the real
	// client last, so the real client wins.
	response = call("10.1.2.3:1234", map[string][]string{
		"X-Forwarded-For": {"198.51.100.77, 203.0.113.10"},
	})
	if got := response.Header().Get("X-Seen-Client"); got != "203.0.113.10" {
		t.Fatalf("client = %q, want 203.0.113.10", got)
	}

	// A chain made only of trusted addresses falls back to the TCP peer.
	response = call("10.1.2.3:1234", map[string][]string{
		"X-Forwarded-For": {"10.9.9.9, 10.4.5.6"},
	})
	if got := response.Header().Get("X-Seen-Client"); got != "10.1.2.3" {
		t.Fatalf("all-trusted client = %q, want the peer", got)
	}

	// A malformed hop never gets skipped over to a further-left value.
	response = call("10.1.2.3:1234", map[string][]string{
		"X-Forwarded-For": {"203.0.113.11, not-an-ip"},
	})
	if got := response.Header().Get("X-Seen-Client"); got != "10.1.2.3" {
		t.Fatalf("malformed client = %q, want the peer", got)
	}

	// Multiple values for the transport facts are ignored, not guessed.
	response = call("10.1.2.3:1234", map[string][]string{
		"X-Forwarded-For":   {"203.0.113.12"},
		"X-Forwarded-Proto": {"https, http"},
		"X-Forwarded-Host":  {"hub.example.com, evil.example.com"},
	})
	if got := response.Header().Get("X-Seen-Scheme"); got != "http" {
		t.Fatalf("multi-valued proto = %q, want listener scheme", got)
	}
}

func TestForwardedFamilyIsExclusive(t *testing.T) {
	var seen http.Header
	var clientIP string
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		clientIP = ClientIP(r)
		w.WriteHeader(http.StatusOK)
	})

	xForwarded := newServer(t, config.Hub{
		Listen:           "127.0.0.1",
		Port:             8081,
		TrustedProxies:   []string{"10.0.0.0/8"},
		ForwardedHeaders: config.ForwardedHeadersXForwarded,
	})
	if err := xForwarded.Mount("/api/", capture); err != nil {
		t.Fatal(err)
	}
	req := request(http.MethodGet, "http://127.0.0.1:8081/api/x")
	req.RemoteAddr = "10.1.2.3:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.20")
	req.Header.Set("Forwarded", "for=198.51.100.20;proto=https;host=other.example.com")
	if response := serve(xForwarded, req); response.Code != http.StatusOK {
		t.Fatalf("x-forwarded: status %d", response.Code)
	}
	if clientIP != "203.0.113.20" {
		t.Fatalf("x-forwarded client = %q", clientIP)
	}
	if got := seen.Get("Forwarded"); got != "" {
		t.Fatalf("the ignored family survived: %q", got)
	}
	if got := seen.Get("X-Forwarded-Proto"); got != "http" {
		t.Fatalf("scheme came from the ignored family: %q", got)
	}

	forwarded := newServer(t, config.Hub{
		Listen:           "127.0.0.1",
		Port:             8081,
		TrustedProxies:   []string{"10.0.0.0/8"},
		ForwardedHeaders: config.ForwardedHeadersForwarded,
	})
	if err := forwarded.Mount("/api/", capture); err != nil {
		t.Fatal(err)
	}
	req = request(http.MethodGet, "http://127.0.0.1:8081/api/x")
	req.RemoteAddr = "10.1.2.3:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.21")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "evil.example.com")
	req.Header.Set("Forwarded", `for=6.6.6.6, for=198.51.100.21;proto=https;host="hub.example.com"`)
	if response := serve(forwarded, req); response.Code != http.StatusOK {
		t.Fatalf("forwarded: status %d", response.Code)
	}
	if clientIP != "198.51.100.21" {
		t.Fatalf("forwarded client = %q", clientIP)
	}
	if got := seen.Get("X-Forwarded-For"); got != "198.51.100.21" {
		t.Fatalf("forwarded X-Forwarded-For %q", got)
	}
	if got := seen.Get("X-Forwarded-Proto"); got != "https" {
		t.Fatalf("forwarded proto %q", got)
	}
	if got := seen.Get("X-Forwarded-Host"); got != "hub.example.com" {
		t.Fatalf("forwarded host %q", got)
	}
	if got := seen.Values("Forwarded"); len(got) != 0 {
		t.Fatalf("Forwarded was not stripped: %v", got)
	}
}

func TestRequestBodyLimit(t *testing.T) {
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081}, func(options *Options) {
		options.MaxBodyBytes = 16
	})
	if err := server.Mount("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8081/api/upload", bytes.NewReader(bytes.Repeat([]byte("x"), 64)))
	req.RequestURI = req.URL.RequestURI()
	req.Header.Set("Origin", "http://127.0.0.1:8081")
	response := serve(server, req)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", response.Code)
	}
}

func TestSecurityHeadersAndTimeouts(t *testing.T) {
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081})
	response := serve(server, request(http.MethodGet, "http://127.0.0.1:8081/"))
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff %q", got)
	}
	if got := response.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("referrer policy %q", got)
	}
	if got := response.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("frame options %q", got)
	}
	if got := response.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("plaintext response advertised HSTS: %q", got)
	}
	if got := response.Header().Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'self'") || !strings.Contains(got, "frame-ancestors 'none'") {
		t.Fatalf("document CSP %q", got)
	}
	if server.http.WriteTimeout != 0 {
		t.Fatalf("write timeout %v would break SSE", server.http.WriteTimeout)
	}
	if server.http.ReadHeaderTimeout != defaultReadHeaderTimeout {
		t.Fatalf("read header timeout %v", server.http.ReadHeaderTimeout)
	}
	if server.http.ReadTimeout != defaultReadTimeout {
		t.Fatalf("read timeout %v", server.http.ReadTimeout)
	}
	if server.http.IdleTimeout != defaultIdleTimeout {
		t.Fatalf("idle timeout %v", server.http.IdleTimeout)
	}

	// The rejected boundary responses carry the same baseline headers.
	bad := serve(server, request(http.MethodPost, "http://127.0.0.1:8081/api/x"))
	if got := bad.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("rejection referrer policy %q", got)
	}

	// A canonical HTTPS origin behind a trusted proxy emits HSTS.
	remote := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081, Origin: "https://hub.example.test"})
	hsts := serve(remote, request(http.MethodGet, "https://hub.example.test/api/health"))
	if got := hsts.Header().Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Fatalf("HSTS %q", got)
	}
}

func startServer(t *testing.T, server *Server) {
	t.Helper()
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
}

func TestGracefulShutdownDrainsRequestContexts(t *testing.T) {
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 0})
	entered := make(chan struct{})
	if err := server.Mount("/api/stream", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	})); err != nil {
		t.Fatal(err)
	}
	startServer(t, server)
	address := server.Addr().String()

	go func() {
		response, err := http.Get("http://" + address + "/api/stream")
		if err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %v", elapsed)
	}
	select {
	case <-server.Done():
	case <-time.After(time.Second):
		t.Fatal("Done was not closed")
	}
	if _, err := net.DialTimeout("tcp", address, 200*time.Millisecond); err == nil {
		t.Fatal("listener still accepts connections after shutdown")
	}
}

func TestShutdownClosesHijackedUpgrade(t *testing.T) {
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 0})
	release := make(chan struct{})
	if err := server.MountUpgrade("/api/ws", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upgrade writer is not a hijacker")
			return
		}
		connection, buffered, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = buffered.Flush()
		<-release
	})); err != nil {
		t.Fatal(err)
	}
	startServer(t, server)
	address := server.Addr().String()

	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := fmt.Fprintf(connection, "GET /api/ws HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nOrigin: http://%s\r\n\r\n", address, address); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("upgrade status %q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read upgrade headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	close(release)

	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := reader.ReadString('\n'); err == nil {
		t.Fatal("hijacked connection still readable after shutdown")
	}
}

// rawStatus sends literal request bytes and returns the response status line.
func rawStatus(t *testing.T, address, raw string) string {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := connection.Write([]byte(raw)); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	return strings.TrimSpace(line)
}

func TestRawRequestTargetBoundary(t *testing.T) {
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 0})
	startServer(t, server)
	address := server.Addr().String()
	host := address

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "absolute-form target",
			raw:  fmt.Sprintf("GET http://%s/api/health HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host, host),
			want: "400",
		},
		{
			name: "absolute-form target with a foreign authority",
			raw:  fmt.Sprintf("GET http://evil.example/api/health HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host),
			want: "400",
		},
		{
			name: "absolute-form target with userinfo",
			raw:  fmt.Sprintf("GET http://user:pass@%s/api/health HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host, host),
			want: "400",
		},
		{
			name: "CONNECT",
			raw:  fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host, host),
			want: "400",
		},
		{
			name: "HTTP/1.0 without Host",
			raw:  "GET /api/health HTTP/1.0\r\n\r\n",
			want: "400",
		},
		{
			name: "HTTP/1.0 with Host",
			raw:  fmt.Sprintf("GET /api/health HTTP/1.0\r\nHost: %s\r\n\r\n", host),
			want: "200",
		},
		{
			name: "origin-form",
			raw:  fmt.Sprintf("GET /api/health HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host),
			want: "200",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := rawStatus(t, address, tc.raw)
			if !strings.Contains(status, " "+tc.want+" ") {
				t.Fatalf("status line %q, want %s", status, tc.want)
			}
		})
	}
}

func TestUpgradeDetectionCombinesConnectionValues(t *testing.T) {
	req := request(http.MethodGet, "http://127.0.0.1:8081/api/ws")
	req.Header["Connection"] = []string{"keep-alive", "Upgrade"}
	req.Header.Set("Upgrade", "websocket")
	if !isUpgrade(req) {
		t.Fatal("Connection values were not combined")
	}

	req = request(http.MethodGet, "http://127.0.0.1:8081/api/ws")
	req.Header["Connection"] = []string{"keep-alive, Upgrade"}
	req.Header.Set("Upgrade", "websocket")
	if !isUpgrade(req) {
		t.Fatal("comma-separated Connection value was not parsed")
	}

	req = request(http.MethodGet, "http://127.0.0.1:8081/api/ws")
	req.Header["Connection"] = []string{"keep-alive, close"}
	req.Header.Set("Upgrade", "websocket")
	if isUpgrade(req) {
		t.Fatal("a non-upgrade Connection header was treated as an upgrade")
	}
}
