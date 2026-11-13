package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type releaseServer struct {
	version string
	archive []byte
	sums    string
	server  *httptest.Server
}

func newReleaseServer(t *testing.T, version string, binary []byte, checksumOverride string) *releaseServer {
	t.Helper()
	archive := testArchive(t, map[string][]byte{"rigscope": binary})
	asset := fmt.Sprintf("rigscope_%s_linux_amd64.tar.gz", version)
	sum := sha256.Sum256(archive)
	line := hex.EncodeToString(sum[:])
	if checksumOverride != "" {
		line = checksumOverride
	}
	release := &releaseServer{
		version: version,
		archive: archive,
		sums:    line + "  " + asset + "\n",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		release.writeMetadata(t, w, asset)
	})
	mux.HandleFunc("/releases/tags/", func(w http.ResponseWriter, _ *http.Request) {
		release.writeMetadata(t, w, asset)
	})
	mux.HandleFunc("/dl/"+asset, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/dl/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(release.sums))
	})
	release.server = httptest.NewServer(mux)
	t.Cleanup(release.server.Close)
	return release
}

func (r *releaseServer) writeMetadata(t *testing.T, w http.ResponseWriter, asset string) {
	t.Helper()
	response := map[string]any{
		"tag_name": "v" + r.version,
		"assets": []map[string]string{
			{"name": asset, "browser_download_url": r.server.URL + "/dl/" + asset},
			{"name": "checksums.txt", "browser_download_url": r.server.URL + "/dl/checksums.txt"},
		},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Errorf("encode metadata: %v", err)
	}
}

func newTestFetcher(t *testing.T, stateRoot string, server *releaseServer, version string, runVersion func(context.Context, string) (string, error)) *Fetcher {
	t.Helper()
	fetcher, err := NewFetcher(Options{
		APIBase:           server.server.URL + "/releases",
		Client:            server.server.Client(),
		GOARCH:            "amd64",
		StateRoot:         stateRoot,
		AllowInsecureHTTP: true,
		RunVersion: func(ctx context.Context, binary string) (string, error) {
			if runVersion != nil {
				return runVersion(ctx, binary)
			}
			return version, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fetcher
}

func TestResolveAndStage(t *testing.T) {
	root := t.TempDir()
	binary := []byte("#!/bin/sh\necho rigscope 1.1.0\n")
	server := newReleaseServer(t, "1.1.0", binary, "")
	fetcher := newTestFetcher(t, root, server, "1.1.0", nil)

	resolved, err := fetcher.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Version != "1.1.0" || resolved.ArchiveName != "rigscope_1.1.0_linux_amd64.tar.gz" {
		t.Fatalf("resolved = %+v", resolved)
	}
	staged, err := fetcher.Stage(context.Background(), resolved)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	want := filepath.Join(root, "staging", "1.1.0", "rigscope")
	if staged.Path != want {
		t.Fatalf("path = %s, want %s", staged.Path, want)
	}
	data, err := os.ReadFile(staged.Path)
	if err != nil || !bytes.Equal(data, binary) {
		t.Fatalf("staged data = %q, %v", data, err)
	}
	info, err := os.Stat(staged.Path)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("staged mode = %v, %v", info.Mode().Perm(), err)
	}
}

func TestStageExecutesStagedBinaryVersion(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is unavailable")
	}
	root := t.TempDir()
	binary := []byte("#!/bin/sh\necho rigscope 1.1.0\n")
	server := newReleaseServer(t, "1.1.0", binary, "")
	fetcher, err := NewFetcher(Options{
		APIBase:           server.server.URL + "/releases",
		Client:            server.server.Client(),
		GOARCH:            "amd64",
		StateRoot:         root,
		AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := fetcher.Resolve(context.Background(), "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetcher.Stage(context.Background(), resolved); err != nil {
		t.Fatalf("stage: %v", err)
	}
}

func TestStageRejectsWrongVersion(t *testing.T) {
	root := t.TempDir()
	server := newReleaseServer(t, "1.1.0", []byte("binary"), "")
	fetcher := newTestFetcher(t, root, server, "9.9.9", nil)
	resolved, err := fetcher.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetcher.Stage(context.Background(), resolved)
	if err == nil || !strings.Contains(err.Error(), "reports version") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "staging", "1.1.0", "rigscope")); err == nil {
		t.Fatal("wrong-version binary was staged as usable")
	}
}

func TestStageRefusesToClearStagingContainingBindMount(t *testing.T) {
	runReleaseMountTest(t, "TestStageRefusesToClearStagingContainingBindMount", "RIGSCOPE_RELEASE_MOUNT_HELPER", runStageOverBindMountHelper)
}

func TestStageRefusesToClearStagingRootThatIsAMount(t *testing.T) {
	runReleaseMountTest(t, "TestStageRefusesToClearStagingRootThatIsAMount", "RIGSCOPE_RELEASE_ROOT_MOUNT_HELPER", runStageOverRootMountHelper)
}

// runReleaseMountTest re-runs one test inside a disposable mount namespace.
// unshare availability is probed first; once it is known to work, a helper
// failure is a real setup, cleanup or safety regression and must fail the test
// instead of silently skipping the only coverage of the mount guard.
func runReleaseMountTest(t *testing.T, name, envName string, helper func(*testing.T)) {
	t.Helper()
	if os.Getenv(envName) == "1" {
		helper(t)
		return
	}
	if !releaseUnshareAvailable() {
		t.Skip("unshare with mount namespace support is unavailable")
	}
	command := exec.Command("unshare", "-rm", os.Args[0], "-test.run", "^"+name+"$")
	command.Env = append(os.Environ(), envName+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("mount namespace helper failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "PASS") {
		t.Fatalf("mount namespace probe did not pass:\n%s", output)
	}
}

// stagingMountRoot creates a staging tree with a foreign mount source and
// returns a cleanup that detaches any test mount before removing the tree. A
// live bind mount would otherwise make the test's own cleanup fail (or delete
// the foreign contents) and the helper would exit nonzero for the wrong reason.
func stagingMountRoot(t *testing.T, version string) (root, staging, foreign, inside, keep string) {
	t.Helper()
	root, err := os.MkdirTemp("", "rigscope-release-mount-")
	if err != nil {
		t.Fatal(err)
	}
	staging = filepath.Join(root, "staging", version)
	foreign = filepath.Join(root, "foreign")
	inside = filepath.Join(staging, "sub", "foreign")
	for _, dir := range []string{inside, foreign} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			_ = os.RemoveAll(root)
			t.Fatal(err)
		}
	}
	keep = filepath.Join(foreign, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		_ = os.RemoveAll(root)
		t.Fatal(err)
	}
	return root, staging, foreign, inside, keep
}

func detachAndRemove(root string, mounts ...string) {
	for _, mount := range mounts {
		_ = syscall.Unmount(mount, syscall.MNT_DETACH)
	}
	_ = os.RemoveAll(root)
}

func runStageOverBindMountHelper(t *testing.T) {
	root, _, foreign, inside, keep := stagingMountRoot(t, "1.1.0")
	defer detachAndRemove(root, inside)
	server := newReleaseServer(t, "1.1.0", []byte("binary"), "")
	fetcher := newTestFetcher(t, root, server, "1.1.0", nil)
	resolved, err := fetcher.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount(foreign, inside, "", syscall.MS_BIND, ""); err != nil {
		t.Fatalf("bind mount: %v", err)
	}
	_, err = fetcher.Stage(context.Background(), resolved)
	if err == nil || !strings.Contains(err.Error(), "mount point") {
		t.Fatalf("Stage cleared a staging tree with a foreign mount: %v", err)
	}
	if _, statErr := os.Stat(keep); statErr != nil {
		t.Fatalf("foreign file under the bind mount was deleted: %v", statErr)
	}
}

func runStageOverRootMountHelper(t *testing.T) {
	root, staging, _, _, _ := stagingMountRoot(t, "1.1.0")
	defer detachAndRemove(root, staging)
	marker := filepath.Join(staging, "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := newReleaseServer(t, "1.1.0", []byte("binary"), "")
	fetcher := newTestFetcher(t, root, server, "1.1.0", nil)
	resolved, err := fetcher.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	// A bind mount of the staging root onto itself is a same-device mount that
	// st_dev cannot detect; Stage must refuse it before deleting anything.
	if err := syscall.Mount(staging, staging, "", syscall.MS_BIND, ""); err != nil {
		t.Fatalf("bind mount: %v", err)
	}
	_, err = fetcher.Stage(context.Background(), resolved)
	if err == nil || !strings.Contains(err.Error(), "mount point") {
		t.Fatalf("Stage cleared a staging root that is itself a mount: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("the mounted staging tree was deleted: %v", statErr)
	}
}

func releaseUnshareAvailable() bool {
	if _, err := exec.LookPath("unshare"); err != nil {
		return false
	}
	return exec.Command("unshare", "-rm", "true").Run() == nil
}

func TestStageRejectsChecksumMismatch(t *testing.T) {
	root := t.TempDir()
	server := newReleaseServer(t, "1.1.0", []byte("binary"), strings.Repeat("0", 64))
	fetcher := newTestFetcher(t, root, server, "1.1.0", nil)
	resolved, err := fetcher.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetcher.Stage(context.Background(), resolved); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolvePinnedVersion(t *testing.T) {
	root := t.TempDir()
	server := newReleaseServer(t, "0.9.0", []byte("binary"), "")
	fetcher := newTestFetcher(t, root, server, "0.9.0", nil)
	resolved, err := fetcher.Resolve(context.Background(), "0.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Version != "0.9.0" {
		t.Fatalf("version = %s", resolved.Version)
	}
}

func TestHTTPSDowngradeRedirectRefused(t *testing.T) {
	var downgraded atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downgraded.Add(1)
		_, _ = w.Write([]byte("{}"))
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/releases/latest", http.StatusFound)
	}))
	defer secure.Close()

	fetcher, err := NewFetcher(Options{
		APIBase:   secure.URL + "/releases",
		Client:    secure.Client(),
		GOARCH:    "amd64",
		StateRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetcher.Resolve(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "plaintext HTTP") {
		t.Fatalf("err = %v", err)
	}
	if downgraded.Load() != 0 {
		t.Fatal("fetcher followed the redirect to plaintext HTTP")
	}
}

func TestHTTPSRedirectChainAllowed(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			http.Redirect(w, r, "/releases/latest", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"v1.0.0","assets":[]}`))
	}))
	defer server.Close()
	fetcher, err := NewFetcher(Options{
		Client:    server.Client(),
		GOARCH:    "amd64",
		StateRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := fetcher.get(context.Background(), server.URL+"/first", 1<<20)
	if err != nil {
		t.Fatalf("https redirect chain: %v", err)
	}
	if !strings.Contains(string(body), "tag_name") {
		t.Fatalf("body = %s", body)
	}
}

func TestContextCancellation(t *testing.T) {
	server := newReleaseServer(t, "1.1.0", []byte("binary"), "")
	fetcher := newTestFetcher(t, t.TempDir(), server, "1.1.0", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetcher.Resolve(ctx, ""); err == nil {
		t.Fatal("cancelled context was ignored")
	}
}

func TestVerifyChecksumStrictFormat(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	digest := hex.EncodeToString(sum[:])
	cases := []struct {
		name string
		sums string
		err  string
	}{
		{"good two spaces", digest + "  asset.tar.gz\n", ""},
		{"good asterisk", digest + " *asset.tar.gz\n", ""},
		{"single space", digest + " asset.tar.gz\n", "expected two spaces"},
		{"short digest", "abc  asset.tar.gz\n", "too short"},
		{"non hex", strings.Repeat("z", 64) + "  asset.tar.gz\n", "not hexadecimal"},
		{"slash in name", digest + "  ../asset\n", "separator"},
		{"missing", digest + "  other.tar.gz\n", "no entry"},
		{"malformed other line", digest + "  asset.tar.gz\nnot a line\n", "line 2"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := VerifyChecksum([]byte(test.sums), "asset.tar.gz", []byte("x"))
			if test.err == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Fatalf("err = %v, want %q", err, test.err)
			}
		})
	}
}

func TestVerifyChecksumConflictingDuplicate(t *testing.T) {
	one := strings.Repeat("a", 64)
	two := strings.Repeat("b", 64)
	sums := one + "  asset.tar.gz\n" + two + "  asset.tar.gz\n"
	if err := VerifyChecksum([]byte(sums), "asset.tar.gz", []byte("x")); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractBinaryRejectsTarBombs(t *testing.T) {
	defaults := ExtractOptions{MaxBinary: 1024, MaxDecompressed: 32, MaxEntries: 10}

	t.Run("oversized skipped entry", func(t *testing.T) {
		archive := testArchive(t, map[string][]byte{"padding": make([]byte, 1<<20)})
		if _, err := ExtractBinary(archive, defaults); err == nil || !strings.Contains(err.Error(), "decompressed limit") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("total across skipped entries", func(t *testing.T) {
		archive := testArchive(t, map[string][]byte{"one": bytes.Repeat([]byte("a"), 20), "two": bytes.Repeat([]byte("b"), 20)})
		if _, err := ExtractBinary(archive, ExtractOptions{MaxBinary: 1024, MaxDecompressed: 30, MaxEntries: 10}); err == nil || !strings.Contains(err.Error(), "decompressed limit") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("too many entries", func(t *testing.T) {
		archive := testArchive(t, map[string][]byte{"one": []byte("a"), "two": []byte("b"), "rigscope": []byte("x")})
		if _, err := ExtractBinary(archive, ExtractOptions{MaxBinary: 1024, MaxDecompressed: 1024, MaxEntries: 1}); err == nil || !strings.Contains(err.Error(), "more than") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("path traversal", func(t *testing.T) {
		archive := testArchive(t, map[string][]byte{"../rigscope": []byte("evil")})
		if _, err := ExtractBinary(archive, defaults); err == nil || !strings.Contains(err.Error(), "escapes the archive root") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("absolute path", func(t *testing.T) {
		archive := testArchive(t, map[string][]byte{"/etc/rigscope": []byte("evil")})
		if _, err := ExtractBinary(archive, defaults); err == nil || !strings.Contains(err.Error(), "escapes the archive root") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("symlink rigscope", func(t *testing.T) {
		archive := symlinkArchive(t, "rigscope")
		if _, err := ExtractBinary(archive, defaults); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("oversized binary", func(t *testing.T) {
		archive := testArchive(t, map[string][]byte{"rigscope": bytes.Repeat([]byte("x"), 64)})
		if _, err := ExtractBinary(archive, ExtractOptions{MaxBinary: 8, MaxDecompressed: 1024, MaxEntries: 10}); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("nested rigscope ignored", func(t *testing.T) {
		archive := testArchive(t, map[string][]byte{"dir/rigscope": []byte("x")})
		if _, err := ExtractBinary(archive, defaults); err == nil || !strings.Contains(err.Error(), "no \"rigscope\" binary") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("duplicate rigscope", func(t *testing.T) {
		archive := duplicateArchive(t)
		if _, err := ExtractBinary(archive, defaults); err == nil {
			t.Fatal("duplicate rigscope accepted")
		}
	})
}

func TestExtractBinaryCountsHiddenPAXMetadata(t *testing.T) {
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gz)
	header := &tar.Header{
		Name:       "rigscope",
		Mode:       0o755,
		Size:       1,
		Typeflag:   tar.TypeReg,
		PAXRecords: map[string]string{"comment": strings.Repeat("h", 8192)},
	}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := ExtractBinary(buffer.Bytes(), ExtractOptions{
		MaxBinary:       1024,
		MaxDecompressed: 1 << 20,
		MaxEntries:      10,
		MaxRawBytes:     4096,
	})
	if err == nil || !strings.Contains(err.Error(), "decompressed") {
		t.Fatalf("hidden PAX metadata bypassed the raw limit: %v", err)
	}
}

func TestExtractBinaryCapsRawTarBlocks(t *testing.T) {
	archive := testArchive(t, map[string][]byte{"rigscope": []byte("x")})
	_, err := ExtractBinary(archive, ExtractOptions{
		MaxBinary:       1024,
		MaxDecompressed: 1 << 20,
		MaxEntries:      10,
		MaxRawBytes:     1 << 20,
		MaxBlocks:       2,
	})
	if err == nil || !strings.Contains(err.Error(), "raw tar blocks") {
		t.Fatalf("raw block cap was not enforced: %v", err)
	}
}

func TestRunVersionHasDeadlineAndBoundedOutput(t *testing.T) {
	dir := t.TempDir()
	slow := filepath.Join(dir, "slow")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := runVersion(context.Background(), slow, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("slow version err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("version command was not killed promptly: %s", elapsed)
	}

	noisy := filepath.Join(dir, "noisy")
	script := "#!/bin/sh\nhead -c 70000 /dev/zero | tr '\\0' x\necho\n"
	if err := os.WriteFile(noisy, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = runVersion(context.Background(), noisy, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("noisy version err = %v", err)
	}
}

func TestNewFetcherRejectsRelativeStateRoot(t *testing.T) {
	if _, err := NewFetcher(Options{StateRoot: "relative"}); err == nil {
		t.Fatal("relative state root accepted")
	}
}

func TestResolveRejectsUnsafeTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v../../etc", "assets": []any{}})
	}))
	defer server.Close()
	fetcher, err := NewFetcher(Options{APIBase: server.URL, Client: server.Client(), GOARCH: "amd64", StateRoot: t.TempDir(), AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetcher.Resolve(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "not a safe version") {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadBound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 128))
	}))
	defer server.Close()
	fetcher, err := NewFetcher(Options{APIBase: server.URL, Client: server.Client(), GOARCH: "amd64", StateRoot: t.TempDir(), AllowInsecureHTTP: true, MaxMetadata: 16})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetcher.Resolve(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("err = %v", err)
	}
}

func TestStageDoesNotFollowHTTPWithoutException(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	fetcher, err := NewFetcher(Options{APIBase: server.URL, Client: server.Client(), GOARCH: "amd64", StateRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := fetcher.Resolve(ctx, ""); err == nil || !strings.Contains(err.Error(), "refusing plaintext HTTP") {
		t.Fatalf("err = %v", err)
	}
}

func testArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gz)
	for name, content := range files {
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
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

func symlinkArchive(t *testing.T, name string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gz)
	header := &tar.Header{Name: name, Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func duplicateArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gz)
	for _, content := range []string{"a", "b"} {
		header := &tar.Header{Name: "rigscope", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
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

// (header-only archives are not used: the regular test archive can still make
// the extractor reject a large declared size before it skips the body.)

func TestTestAPIBaseAcceptsOnlyHTTPSLoopback(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
		ok    bool
	}{
		{"empty", "", "", false},
		{"loopback ipv4", "https://127.0.0.1:8443/releases", "https://127.0.0.1:8443/releases", true},
		{"loopback ipv6", "https://[::1]:8443/releases/", "https://[::1]:8443/releases", true},
		{"localhost", "https://localhost/releases", "https://localhost/releases", true},
		{"remote host", "https://example.com/releases", "", false},
		{"plain http loopback", "http://127.0.0.1:8443/releases", "", false},
		{"credentials", "https://user:pass@127.0.0.1/releases", "", false},
		{"query", "https://127.0.0.1/releases?x=1", "", false},
		{"fragment", "https://127.0.0.1/releases#x", "", false},
		{"not a url", "127.0.0.1", "", false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, ok := testAPIBase(func(string) string { return test.value })
			if ok != test.ok || got != test.want {
				t.Fatalf("testAPIBase(%q) = %q, %v; want %q, %v", test.value, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestNewFetcherUsesLoopbackTestOverrideForDefaultAPIBase(t *testing.T) {
	t.Setenv(TestAPIBaseEnv, "https://127.0.0.1:9443/releases/")
	fetcher, err := NewFetcher(Options{StateRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.opts.APIBase != "https://127.0.0.1:9443/releases" {
		t.Fatalf("APIBase = %q", fetcher.opts.APIBase)
	}
}

func TestNewFetcherKeepsExplicitAPIBaseOverTestOverride(t *testing.T) {
	t.Setenv(TestAPIBaseEnv, "https://127.0.0.1:9443/releases")
	fetcher, err := NewFetcher(Options{
		APIBase:   "https://releases.example.test/custom",
		StateRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.opts.APIBase != "https://releases.example.test/custom" {
		t.Fatalf("APIBase = %q", fetcher.opts.APIBase)
	}
}

func TestNewFetcherIgnoresNonLoopbackTestOverride(t *testing.T) {
	t.Setenv(TestAPIBaseEnv, "https://releases.example.test/github")
	fetcher, err := NewFetcher(Options{StateRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.opts.APIBase != DefaultAPIBase {
		t.Fatalf("APIBase = %q, want %q", fetcher.opts.APIBase, DefaultAPIBase)
	}
}
