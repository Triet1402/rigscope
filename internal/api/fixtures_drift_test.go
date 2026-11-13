package api_test

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/api/fixturesgen"
)

// TestReencodedFixturesMatch regenerates the Go-encoded response corpus into a
// temporary directory and compares it byte-for-byte with the committed
// testdata/api/reencoded tree. A stale fixture, or a fixture missing from the
// capture corpus, fails CI with instructions to run go generate.
func TestReencodedFixturesMatch(t *testing.T) {
	src := filepath.Join("..", "..", "testdata", "api")
	committed := filepath.Join(src, "reencoded")
	generated := t.TempDir()
	if err := fixturesgen.Generate(src, generated); err != nil {
		t.Fatalf("generate re-encoded fixtures: %v", err)
	}
	want := jsonFiles(t, generated)
	got := jsonFiles(t, committed)
	if len(want) == 0 {
		t.Fatal("generator produced no fixtures")
	}
	if strings.Join(want, "\n") != strings.Join(got, "\n") {
		t.Fatalf("re-encoded fixture set is stale (run go generate ./internal/api)\ngenerated:\n%s\ncommitted:\n%s",
			strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
	for _, rel := range want {
		wantBytes, err := os.ReadFile(filepath.Join(generated, rel))
		if err != nil {
			t.Fatal(err)
		}
		gotBytes, err := os.ReadFile(filepath.Join(committed, rel))
		if err != nil {
			t.Fatalf("%s: %v (run go generate ./internal/api)", rel, err)
		}
		if !bytes.Equal(wantBytes, gotBytes) {
			t.Fatalf("%s is stale (run go generate ./internal/api)\nwant:\n%s\ngot:\n%s", rel, wantBytes, gotBytes)
		}
	}
}

func jsonFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}
