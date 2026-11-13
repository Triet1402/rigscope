package webui

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func hashOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func TestInlineScriptHashes(t *testing.T) {
	document := []byte(`<!doctype html>
<script src="/_next/static/app.js"></script>
<script>first()</script>
<SCRIPT type="text/javascript">second()</SCRIPT>
<script data-note="a > b">third()</script>
<script src="x" async></script>
<script></script>
<script>first()</script>`)
	hashes := InlineScriptHashes(document)
	want := []string{hashOf("first()"), hashOf("second()"), hashOf("third()")}
	if len(hashes) != len(want) {
		t.Fatalf("hashes = %v, want %v", hashes, want)
	}
	for index := range want {
		if hashes[index] != want[index] {
			t.Fatalf("hash %d = %q, want %q", index, hashes[index], want[index])
		}
	}
}

func TestContentSecurityPolicy(t *testing.T) {
	hash := hashOf("boot()")
	policy := contentSecurityPolicy([]string{hash})
	for _, directive := range []string{
		"default-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"object-src 'none'",
		"img-src 'self' data:",
		"connect-src 'self'",
		"script-src 'self' 'sha256-" + hash + "'",
	} {
		if !strings.Contains(policy, directive) {
			t.Fatalf("policy %q is missing %q", policy, directive)
		}
	}
	if strings.Contains(policy, "script-src 'self' 'unsafe-inline'") || strings.Contains(policy, "script-src 'unsafe-inline'") {
		t.Fatalf("script policy allows unsafe-inline: %q", policy)
	}
	if !strings.Contains(policy, "style-src 'self' 'unsafe-inline'") {
		t.Fatalf("React style attributes need the documented style policy: %q", policy)
	}
}

func TestCSPHeaderPerDocument(t *testing.T) {
	root := t.TempDir()
	first := `<html><body><script>alpha()</script></body></html>`
	second := `<html><body><script src="/_next/a.js"></script><script>beta()</script></body></html>`
	writeFile(t, root, "index.html", first)
	writeFile(t, root, "machines.html", second)
	writeFile(t, root, "_next/a.js", "console.log('a')")

	server := newDiskServer(t, root)
	home := rawRequest(t, server.URL, "/", http.MethodGet, nil)
	policy := home.headers.Get("Content-Security-Policy")
	if !strings.Contains(policy, "'sha256-"+hashOf("alpha()")+"'") {
		t.Fatalf("home CSP has no alpha hash: %q", policy)
	}
	if strings.Contains(policy, "'sha256-"+hashOf("beta()")+"'") {
		t.Fatalf("home CSP leaked another document's hash: %q", policy)
	}

	machines := rawRequest(t, server.URL, "/machines?machineId=x", http.MethodGet, nil)
	policy = machines.headers.Get("Content-Security-Policy")
	if !strings.Contains(policy, "'sha256-"+hashOf("beta()")+"'") {
		t.Fatalf("machines CSP has no beta hash: %q", policy)
	}

	// Assets and plain-text errors do not carry the document policy.
	asset := rawRequest(t, server.URL, "/_next/a.js", http.MethodGet, nil)
	if got := asset.headers.Get("Content-Security-Policy"); got != "" {
		t.Fatalf("asset CSP %q", got)
	}

	// The hash manifest is never served.
	manifest := rawRequest(t, server.URL, "/"+CSPManifestName, http.MethodGet, nil)
	if manifest.status != http.StatusNotFound {
		t.Fatalf("manifest status %d", manifest.status)
	}
}

func TestCSPManifestIsPreferred(t *testing.T) {
	export := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte(`<html><script>real()</script></html>`)},
		CSPManifestName: &fstest.MapFile{Data: []byte(`{"index.html":["manifest-hash"]}`)},
	}
	handler, err := New(export)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	response := rawRequest(t, server.URL, "/", http.MethodGet, nil)
	if !strings.Contains(response.headers.Get("Content-Security-Policy"), "'sha256-manifest-hash'") {
		t.Fatalf("manifest was not used: %q", response.headers.Get("Content-Security-Policy"))
	}
}

func TestDiskExportServesTheOpenedHandle(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "index.html", "original")
	handler, err := NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()

	ref, err := handler.findFile("/index.html")
	if err != nil || ref == nil {
		t.Fatalf("findFile: %v %v", ref, err)
	}
	// Replace the path after the check with a rename, so the old inode stays
	// alive. The response must still come from the handle that was opened and
	// stat'ed, not from a second path lookup.
	replacement := filepath.Join(root, "replacement.tmp")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, filepath.Join(root, "index.html")); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/index.html", nil)
	handler.sendFile(recorder, request, ref, http.StatusOK, documentCache)
	if recorder.Body.String() != "original" {
		t.Fatalf("body = %q, want the opened handle", recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Length"); got != "8" {
		t.Fatalf("content length = %q, want the opened handle size", got)
	}
}

func TestCSPManifestJSONShape(t *testing.T) {
	data, err := MarshalCSPManifest(map[string][]string{
		"machines.html": {"b", "a"},
		"index.html":    {"c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string][]string
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded["machines.html"]) != 2 || decoded["index.html"][0] != "c" {
		t.Fatalf("manifest = %s", data)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatal("manifest lacks a trailing newline")
	}
}

func TestDotPathsAreNotServed(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "index.html", "<html></html>")
	writeFile(t, root, ".env", "SECRET=1")
	server := newDiskServer(t, root)
	response := rawRequest(t, server.URL, "/.env", http.MethodGet, nil)
	if response.status != http.StatusNotFound {
		t.Fatalf("dotfile status %d", response.status)
	}
}

func TestNewDirClosesRootOnFailure(t *testing.T) {
	root := t.TempDir()
	if _, err := NewDir(root); err == nil {
		t.Fatal("NewDir accepted an export without index.html")
	}
	// A successful close is idempotent.
	writeFile(t, root, "index.html", "<html></html>")
	handler, err := NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handler.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "index.html")); err != nil {
		t.Fatal(err)
	}
}
