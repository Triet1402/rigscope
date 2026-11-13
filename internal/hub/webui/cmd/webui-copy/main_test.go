package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/hub/webui"
)

func writeExportFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCopyExportWritesCSPManifest(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "dist")
	index := `<!doctype html><script src="/_next/a.js"></script><script>boot()</script>`
	login := `<!doctype html><script>login()</script>`
	writeExportFile(t, source, "index.html", index)
	writeExportFile(t, source, "login.html", login)
	writeExportFile(t, source, "_next/a.js", "console.log('a')")

	if err := copyExport(source, destination); err != nil {
		t.Fatalf("copyExport: %v", err)
	}

	for _, name := range []string{"index.html", "login.html", "_next/a.js", ".keep"} {
		info, err := os.Stat(filepath.Join(destination, name))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("%s was not copied: %v", name, err)
		}
	}

	data, err := os.ReadFile(filepath.Join(destination, webui.CSPManifestName))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var manifest map[string][]string
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest["index.html"]) != 1 || len(manifest["login.html"]) != 1 {
		t.Fatalf("manifest = %s", data)
	}
	wantIndex := webui.InlineScriptHashes([]byte(index))
	if manifest["index.html"][0] != wantIndex[0] {
		t.Fatalf("index hash = %q, want %q", manifest["index.html"][0], wantIndex[0])
	}

	// A rebuild that drops a document must not keep its old hash.
	writeExportFile(t, source, "index.html", `<!doctype html><script>reboot()</script>`)
	if err := copyExport(source, destination); err != nil {
		t.Fatalf("second copyExport: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(destination, webui.CSPManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), wantIndex[0]) {
		t.Fatalf("stale hash survived: %s", data)
	}
}

func TestCopyExportRejectsMissingIndex(t *testing.T) {
	if err := copyExport(t.TempDir(), filepath.Join(t.TempDir(), "dist")); err == nil {
		t.Fatal("copyExport accepted an export without index.html")
	}
}
