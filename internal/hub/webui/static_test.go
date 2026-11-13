package webui

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func bigFixturePage() string {
	var builder strings.Builder
	builder.WriteString("<!doctype html><title>Fleet</title><p>")
	for index := 0; index < 600; index++ {
		builder.WriteString("fleet ")
	}
	builder.WriteString("</p>")
	return builder.String()
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func diskFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "index.html", bigFixturePage())
	writeFile(t, root, "login.html", "<!doctype html><title>Sign in</title>")
	writeFile(t, root, "404.html", "<!doctype html><title>Not found</title>")
	writeFile(t, root, "machines.html", "<!doctype html><title>Machine</title>")
	writeFile(t, root, "_next/static/chunks/a.js", "console.log('a');\n")
	writeFile(t, root, "_next/static/app.css", "body{color:red}\n")
	writeFile(t, root, "logo.png", "\x89PNG\r\n\x1a\n")
	return root
}

func newDiskServer(t *testing.T, root string) *httptest.Server {
	t.Helper()
	handler, err := NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

type rawResponse struct {
	status  int
	headers http.Header
	body    []byte
}

func rawRequest(t *testing.T, base, path, method string, headers map[string]string) rawResponse {
	t.Helper()
	request, err := http.NewRequest(method, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Prevent Go's client from silently decompressing the body so tests can
	// assert on Content-Encoding and the raw gzip stream.
	request.Header.Del("Accept-Encoding")
	for key, value := range headers {
		if key == "Accept-Encoding" && value == "" {
			request.Header["Accept-Encoding"] = []string{""}
			continue
		}
		request.Header.Set(key, value)
	}
	client := &http.Client{
		Transport: &http.Transport{DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return rawResponse{status: response.StatusCode, headers: response.Header, body: body}
}

func TestServesExportedDocumentsAndHashedAssets(t *testing.T) {
	server := newDiskServer(t, diskFixture(t))

	for _, tc := range []struct {
		path        string
		contentType string
		cache       string
		contains    string
	}{
		{"/", "text/html", "no-cache", "Fleet"},
		{"/login", "text/html", "no-cache", "Sign in"},
		{"/login/", "text/html", "no-cache", "Sign in"},
		{"/machines", "text/html", "no-cache", "Machine"},
		{"/machines?machineId=abc-123", "text/html", "no-cache", "Machine"},
		{"/machines/?machineId=abc-123", "text/html", "no-cache", "Machine"},
		{"/machines?machineId=host.example", "text/html", "no-cache", "Machine"},
		{"/_next/static/chunks/a.js", "text/javascript", immutableCache, "console.log"},
		{"/_next/static/app.css", "text/css", immutableCache, "body{"},
	} {
		response := rawRequest(t, server.URL, tc.path, http.MethodGet, nil)
		if response.status != http.StatusOK {
			t.Fatalf("%s: status %d", tc.path, response.status)
		}
		if got := response.headers.Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
			t.Fatalf("%s: content type %q", tc.path, got)
		}
		if got := response.headers.Get("Cache-Control"); got != tc.cache {
			t.Fatalf("%s: cache %q", tc.path, got)
		}
		if !bytes.Contains(response.body, []byte(tc.contains)) {
			t.Fatalf("%s: body %q", tc.path, response.body)
		}
	}
}

func TestLegacyMachineRedirects(t *testing.T) {
	server := newDiskServer(t, diskFixture(t))
	cases := []struct{ path, location string }{
		{"/machines/abc-123", "/machines?machineId=abc-123"},
		{"/machines/abc-123/", "/machines?machineId=abc-123"},
		{"/machines/host.example", "/machines?machineId=host.example"},
		{"/machines/a%2Fb", "/machines?machineId=a%2Fb"},
		{"/machines/abc-123?range=1h", "/machines?range=1h&machineId=abc-123"},
		{"/machines/abc-123?range=1h&machineId=ignored", "/machines?range=1h&machineId=abc-123"},
	}
	for _, tc := range cases {
		response := rawRequest(t, server.URL, tc.path, http.MethodGet, nil)
		if response.status != http.StatusPermanentRedirect {
			t.Fatalf("%s: status %d", tc.path, response.status)
		}
		if got := response.headers.Get("Location"); got != tc.location {
			t.Fatalf("%s: location %q, want %q", tc.path, got, tc.location)
		}
		if len(response.body) != 0 {
			t.Fatalf("%s: body %q", tc.path, response.body)
		}
	}

	head := rawRequest(t, server.URL, "/machines/abc-123", http.MethodHead, nil)
	if head.status != http.StatusPermanentRedirect || len(head.body) != 0 {
		t.Fatalf("HEAD redirect: status %d body %q", head.status, head.body)
	}
	if got := head.headers.Get("Location"); got != "/machines?machineId=abc-123" {
		t.Fatalf("HEAD location %q", got)
	}

	// Framework data probes are never redirected into an HTML page.
	for _, tc := range []struct {
		path    string
		headers map[string]string
	}{
		{"/machines/abc-123.txt", nil},
		{"/machines/abc-123", map[string]string{"RSC": "1"}},
		{"/machines/abc-123?_rsc=abc", nil},
	} {
		response := rawRequest(t, server.URL, tc.path, http.MethodGet, tc.headers)
		if response.status != http.StatusNotFound {
			t.Fatalf("%s %v: status %d", tc.path, tc.headers, response.status)
		}
	}
}

func TestNotFoundPageAndFrameworkAssets(t *testing.T) {
	server := newDiskServer(t, diskFixture(t))

	unknown := rawRequest(t, server.URL, "/nope", http.MethodGet, nil)
	if unknown.status != http.StatusNotFound {
		t.Fatalf("status %d", unknown.status)
	}
	if got := unknown.headers.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("content type %q", got)
	}
	if !bytes.Contains(unknown.body, []byte("Not found")) {
		t.Fatalf("body %q", unknown.body)
	}

	asset := rawRequest(t, server.URL, "/_next/static/chunks/missing.js", http.MethodGet, nil)
	if asset.status != http.StatusNotFound {
		t.Fatalf("asset status %d", asset.status)
	}
	if got := asset.headers.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("asset content type %q", got)
	}
}

func TestHeadAndMethodNotAllowed(t *testing.T) {
	server := newDiskServer(t, diskFixture(t))

	head := rawRequest(t, server.URL, "/login", http.MethodHead, nil)
	if head.status != http.StatusOK {
		t.Fatalf("HEAD status %d", head.status)
	}
	if got := head.headers.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("content type %q", got)
	}
	if len(head.body) != 0 {
		t.Fatalf("HEAD body %q", head.body)
	}

	post := rawRequest(t, server.URL, "/login", http.MethodPost, nil)
	if post.status != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", post.status)
	}
	if got := post.headers.Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow %q", got)
	}
}

func TestGzipNegotiation(t *testing.T) {
	server := newDiskServer(t, diskFixture(t))

	for _, header := range []string{"gzip", "GZIP", "gzip;q=1", "gzip;q=0.9, identity;q=0.1", "gzip, identity;q=0", "*", "br, *;q=0.8"} {
		response := rawRequest(t, server.URL, "/", http.MethodGet, map[string]string{"Accept-Encoding": header})
		if response.status != http.StatusOK {
			t.Fatalf("%s: status %d", header, response.status)
		}
		if got := response.headers.Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("%s: encoding %q", header, got)
		}
		if got := response.headers.Get("Content-Length"); got != "" {
			t.Fatalf("%s: content length %q", header, got)
		}
		if got := response.headers.Get("Vary"); got != "Accept-Encoding" {
			t.Fatalf("%s: vary %q", header, got)
		}
		reader, err := gzip.NewReader(bytes.NewReader(response.body))
		if err != nil {
			t.Fatalf("%s: gzip: %v", header, err)
		}
		decoded, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("%s: read: %v", header, err)
		}
		if !bytes.Contains(decoded, []byte("Fleet")) {
			t.Fatalf("%s: body %q", header, decoded)
		}
	}

	for _, header := range []string{"", "identity", "gzip;q=0", "gzip;q=0.4", "gzip;q=0.9", "br", "gzip;q=nope"} {
		headers := map[string]string{}
		if header != "" {
			headers["Accept-Encoding"] = header
		}
		response := rawRequest(t, server.URL, "/", http.MethodGet, headers)
		if response.status != http.StatusOK {
			t.Fatalf("%q: status %d", header, response.status)
		}
		if got := response.headers.Get("Content-Encoding"); got != "" {
			t.Fatalf("%q: encoding %q", header, got)
		}
		if response.headers.Get("Content-Length") == "" {
			t.Fatalf("%q: missing content length", header)
		}
		if string(response.body) != bigFixturePage() {
			t.Fatalf("%q: body mismatch", header)
		}
	}

	png := rawRequest(t, server.URL, "/logo.png", http.MethodGet, map[string]string{"Accept-Encoding": "gzip"})
	if png.status != http.StatusOK || png.headers.Get("Content-Encoding") != "" {
		t.Fatalf("png: status %d encoding %q", png.status, png.headers.Get("Content-Encoding"))
	}

	for _, header := range []string{"*;q=0", "identity;q=0", "br, *;q=0"} {
		response := rawRequest(t, server.URL, "/", http.MethodGet, map[string]string{"Accept-Encoding": header})
		if response.status != http.StatusNotAcceptable {
			t.Fatalf("%s: status %d", header, response.status)
		}
		if got := response.headers.Get("Content-Encoding"); got != "" {
			t.Fatalf("%s: encoding %q", header, got)
		}
		if got := response.headers.Get("Vary"); got != "Accept-Encoding" {
			t.Fatalf("%s: vary %q", header, got)
		}
		if string(response.body) != "not acceptable" {
			t.Fatalf("%s: body %q", header, response.body)
		}
	}
}

func TestHeadSharesGzipRepresentation(t *testing.T) {
	server := newDiskServer(t, diskFixture(t))

	gzipHead := rawRequest(t, server.URL, "/", http.MethodHead, map[string]string{"Accept-Encoding": "gzip"})
	if gzipHead.status != http.StatusOK || gzipHead.headers.Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip HEAD: status %d encoding %q", gzipHead.status, gzipHead.headers.Get("Content-Encoding"))
	}
	if gzipHead.headers.Get("Content-Length") != "" || len(gzipHead.body) != 0 {
		t.Fatalf("gzip HEAD: length %q body %q", gzipHead.headers.Get("Content-Length"), gzipHead.body)
	}

	identityHead := rawRequest(t, server.URL, "/", http.MethodHead, nil)
	if identityHead.headers.Get("Content-Encoding") != "" || identityHead.headers.Get("Content-Length") == "" {
		t.Fatalf("identity HEAD: encoding %q length %q", identityHead.headers.Get("Content-Encoding"), identityHead.headers.Get("Content-Length"))
	}

	notAcceptable := rawRequest(t, server.URL, "/", http.MethodHead, map[string]string{"Accept-Encoding": "*;q=0"})
	if notAcceptable.status != http.StatusNotAcceptable || len(notAcceptable.body) != 0 {
		t.Fatalf("406 HEAD: status %d body %q", notAcceptable.status, notAcceptable.body)
	}
}

func TestRejectsTraversal(t *testing.T) {
	root := diskFixture(t)
	outside := filepath.Join(filepath.Dir(root), fmt.Sprintf("rigscope-secret-%d.txt", os.Getpid()))
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })
	server := newDiskServer(t, root)

	for _, path := range []string{"/%2e%2e/secret.txt", "/..%2fsecret.txt", "/%2e%2e%2f%2e%2e%2fetc/passwd"} {
		response := rawRequest(t, server.URL, path, http.MethodGet, nil)
		if response.status == http.StatusOK {
			t.Fatalf("%s: status 200", path)
		}
		if bytes.Contains(response.body, []byte("secret")) || bytes.Contains(response.body, []byte("root:x")) {
			t.Fatalf("%s: leaked %q", path, response.body)
		}
	}
}

func TestRefusesSymlinks(t *testing.T) {
	root := diskFixture(t)
	outsideFile := filepath.Join(filepath.Dir(root), fmt.Sprintf("rigscope-symlink-secret-%d.txt", os.Getpid()))
	outsideDir := filepath.Join(filepath.Dir(root), fmt.Sprintf("rigscope-symlink-dir-%d", os.Getpid()))
	if err := os.WriteFile(outsideFile, []byte("outside secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, outsideDir, "leak.txt", "directory secret")
	writeFile(t, root, "inside.txt", "inside file")
	if err := os.Symlink(outsideFile, filepath.Join(root, "icon.svg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "inside.txt"), filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(outsideFile)
		_ = os.RemoveAll(outsideDir)
	})
	server := newDiskServer(t, root)

	for _, path := range []string{"/icon.svg", "/linked/leak.txt", "/alias.txt"} {
		response := rawRequest(t, server.URL, path, http.MethodGet, nil)
		if response.status != http.StatusNotFound {
			t.Fatalf("%s: status %d", path, response.status)
		}
		if bytes.Contains(response.body, []byte("outside secret")) || bytes.Contains(response.body, []byte("directory secret")) || bytes.Contains(response.body, []byte("inside file")) {
			t.Fatalf("%s: leaked %q", path, response.body)
		}
	}
	head := rawRequest(t, server.URL, "/icon.svg", http.MethodHead, nil)
	if head.status != http.StatusNotFound {
		t.Fatalf("symlink HEAD status %d", head.status)
	}
}

func TestNewDirRequiresIndex(t *testing.T) {
	root := t.TempDir()
	if _, err := NewDir(root); err == nil {
		t.Fatal("NewDir accepted an export without index.html")
	}
	if _, err := NewDir(filepath.Join(root, "missing")); err == nil {
		t.Fatal("NewDir accepted a missing root")
	}
}

func TestFSExport(t *testing.T) {
	export := fstest.MapFS{
		"index.html":           &fstest.MapFile{Data: []byte(bigFixturePage())},
		"machines.html":        &fstest.MapFile{Data: []byte("<!doctype html><title>Machine</title>")},
		"404.html":             &fstest.MapFile{Data: []byte("<!doctype html><title>Not found</title>")},
		"_next/static/app.js":  &fstest.MapFile{Data: []byte("console.log('app')")},
		"_next/static/app.css": &fstest.MapFile{Data: []byte("body{}")},
	}
	handler, err := New(export)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	home := rawRequest(t, server.URL, "/", http.MethodGet, nil)
	if home.status != http.StatusOK || !bytes.Contains(home.body, []byte("Fleet")) {
		t.Fatalf("home: status %d body %q", home.status, home.body)
	}
	asset := rawRequest(t, server.URL, "/_next/static/app.js", http.MethodGet, nil)
	if asset.status != http.StatusOK || asset.headers.Get("Cache-Control") != immutableCache {
		t.Fatalf("asset: status %d cache %q", asset.status, asset.headers.Get("Cache-Control"))
	}
}

func TestAcceptEncodingQualities(t *testing.T) {
	qualities := acceptEncodingQualities("gzip;q=0.7, gzip;q=0.4, identity;q=0.2", true)
	if got := qualities["gzip"]; got != 0.4 {
		t.Fatalf("duplicate gzip quality = %v, want 0.4", got)
	}
	if got := qualities["identity"]; got != 0.2 {
		t.Fatalf("identity quality = %v", got)
	}
	if acceptEncodingQualities("", false) != nil {
		t.Fatal("absent header must yield nil")
	}
}

func TestAvailableAcceptsPlaceholder(t *testing.T) {
	// A source checkout has no real export; Available must not panic and must
	// report false unless the release build copied an index.html in.
	if Available() {
		info, err := fs.Stat(FS(), "index.html")
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("Available true without index.html: %v", err)
		}
	}
}
