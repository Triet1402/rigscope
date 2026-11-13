package gpu

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// testdataDir is the repository's shared GPU fixture root. Go tests run with
// the package directory as the working directory.
const testdataDir = "../../../../testdata/gpu"

const mib = int64(1024 * 1024)

func requireReadingValue(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %v", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %v, want %v", name, *got, want)
	}
}

func requireBytes(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %v", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %v, want %v", name, *got, want)
	}
}

func requireOptionalFloat(t *testing.T, name string, got, want *float64) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("%s = %v, want absent", name, *got)
		}
		return
	}
	requireReadingValue(t, name, got, *want)
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(testdataDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// fakeRunner feeds canned command output and records what was run.
type fakeRunner struct {
	mu       sync.Mutex
	handler  func(ctx context.Context, command string, args []string, timeout time.Duration) (string, string, error)
	calls    [][]string
	timeouts []time.Duration
}

func (f *fakeRunner) Run(ctx context.Context, command string, args []string, timeout time.Duration) (string, string, error) {
	f.mu.Lock()
	copied := append([]string{}, args...)
	f.calls = append(f.calls, copied)
	f.timeouts = append(f.timeouts, timeout)
	handler := f.handler
	f.mu.Unlock()
	if handler == nil {
		return "", "", fmt.Errorf("unexpected command %s %v", command, args)
	}
	return handler(ctx, command, copied, timeout)
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRunner) call(index int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index < 0 || index >= len(f.calls) {
		return nil
	}
	return append([]string{}, f.calls[index]...)
}

func (f *fakeRunner) observedTimeout(index int) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index < 0 || index >= len(f.timeouts) {
		return 0
	}
	return f.timeouts[index]
}

// isGPUQuery reports whether args ask for the GPU CSV.
func isGPUQuery(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--query-gpu=") {
			return true
		}
	}
	return false
}

// isAppQuery reports whether args ask for the compute-apps CSV.
func isAppQuery(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--query-compute-apps=") {
			return true
		}
	}
	return false
}

// clock is an injectable monotonic clock for deterministic tests.
type clock struct {
	mu  sync.Mutex
	now time.Duration
}

func (c *clock) Now() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now += delta
	c.mu.Unlock()
}

// memSysFS is a mutable fixture filesystem implementing [SysFS]. Directory
// listings are derived from the file paths.
type memSysFS struct {
	mu    sync.Mutex
	files map[string]string
}

func newMemSysFS(files map[string]string) *memSysFS {
	copied := make(map[string]string, len(files))
	for name, contents := range files {
		copied[name] = contents
	}
	return &memSysFS{files: copied}
}

func (m *memSysFS) ReadText(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	contents, ok := m.files[name]
	return contents, ok
}

func (m *memSysFS) ReadDir(name string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := strings.TrimSuffix(name, "/") + "/"
	entries := make(map[string]struct{})
	for path := range m.files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := path[len(prefix):]
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			rest = rest[:slash]
		}
		entries[rest] = struct{}{}
	}
	if len(entries) == 0 {
		return nil, fs.ErrNotExist
	}
	names := make([]string, 0, len(entries))
	for entry := range entries {
		names = append(names, entry)
	}
	sort.Strings(names)
	return names, nil
}

func (m *memSysFS) set(name, contents string) {
	m.mu.Lock()
	m.files[name] = contents
	m.mu.Unlock()
}

func (m *memSysFS) delete(name string) {
	m.mu.Lock()
	delete(m.files, name)
	m.mu.Unlock()
}

// loadFixtureTree copies a testdata tree into a mutable [memSysFS] so a test
// can make a GPU suspend, wake or lose permissions.
func loadFixtureTree(t *testing.T, relative string) *memSysFS {
	t.Helper()
	root := filepath.Join(testdataDir, relative)
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		files["/"+filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("load fixture tree %s: %v", relative, err)
	}
	return newMemSysFS(files)
}

// fakeSensor is a [SensorSource] test double. It records the scope of every
// cycle so tests can assert that gated devices were excluded before any read.
type fakeSensor struct {
	mu      sync.Mutex
	sensors SensorMap
	poll    func(ctx context.Context, scope SampleScope) error
	polls   int
	scopes  []SampleScope
}

func (f *fakeSensor) PollOnce(ctx context.Context, scope SampleScope) error {
	f.mu.Lock()
	f.polls++
	f.scopes = append(f.scopes, scope)
	poll := f.poll
	f.mu.Unlock()
	if poll != nil {
		return poll(ctx, scope)
	}
	return nil
}

func (f *fakeSensor) Latest() SensorMap {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copySensorMap(f.sensors)
}

func (f *fakeSensor) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

func (f *fakeSensor) scope(index int) (SampleScope, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index < 0 || index >= len(f.scopes) {
		return SampleScope{}, false
	}
	return f.scopes[index], true
}

// registerCall records one register batch.
type registerCall struct {
	path    string
	offsets []int64
}

// fakeRegisters is a [RegisterSource] backed by a map of BAR words.
type fakeRegisters struct {
	mu         sync.Mutex
	words      map[string]map[int64]uint32
	zeroAll    bool
	unreadable bool
	panics     bool
	calls      []registerCall
}

func newFakeRegisters() *fakeRegisters {
	return &fakeRegisters{words: make(map[string]map[int64]uint32)}
}

func (f *fakeRegisters) set(path string, offset int64, word uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	words, ok := f.words[path]
	if !ok {
		words = make(map[int64]uint32)
		f.words[path] = words
	}
	words[offset] = word
}

func (f *fakeRegisters) clear(path string, offset int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.words[path], offset)
}

func (f *fakeRegisters) ReadRegisters(_ context.Context, resourcePath string, offsets []int64) ([]*uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, registerCall{path: resourcePath, offsets: append([]int64{}, offsets...)})
	if f.panics {
		panic("simulated register read panic")
	}
	if f.unreadable {
		return nil, fmt.Errorf("permission denied")
	}
	values := make([]*uint32, len(offsets))
	for index, offset := range offsets {
		if f.zeroAll {
			values[index] = pointer(uint32(0))
			continue
		}
		if word, ok := f.words[resourcePath][offset]; ok {
			values[index] = pointer(word)
		}
	}
	return values, nil
}

func (f *fakeRegisters) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRegisters) callsSnapshot() []registerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]registerCall{}, f.calls...)
}

// fakeChildProcess stands in for a child the kernel will not reap: wait blocks
// until the test releases it, even after kill.
type fakeChildProcess struct {
	release chan struct{}
	mu      sync.Mutex
	killed  bool
	err     error
}

func (f *fakeChildProcess) wait() error {
	<-f.release
	return f.err
}

func (f *fakeChildProcess) kill() {
	f.mu.Lock()
	f.killed = true
	f.mu.Unlock()
}

func (f *fakeChildProcess) wasKilled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killed
}

// waitFor polls condition until it holds or the test deadline expires. It is
// only used for the poller's background loop, never to sleep past a real
// assertion.
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
