package activation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/platform/fsutil"
	"github.com/Triet1402/rigscope/internal/platform/lock"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
	"github.com/Triet1402/rigscope/internal/release"
)

// testLocker is a small lock-backed activation.Locker for tests. Production
// CLI wiring uses the shared installation lock adapter instead.
type testLocker struct{ path string }

func (l testLocker) Lock(ctx context.Context, _ string) (func(), error) {
	for {
		held, err := lock.Acquire(l.path)
		if err == nil {
			return func() { _ = held.Release() }, nil
		}
		if !errors.Is(err, lock.ErrBusy) {
			return nil, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrLocked, ctxErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Fakes

type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	tick time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC), tick: time.Second}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	value := c.now
	c.now = c.now.Add(c.tick)
	return value
}

func (c *fakeClock) Sleep(time.Duration) {}

type fakeFetcher struct {
	stateRoot string
	version   string
	staged    []byte

	resolveErr    error
	stageErr      error
	missingStaged bool

	resolveStarted chan struct{}
	resolveRelease chan struct{}
	once           sync.Once
}

func (f *fakeFetcher) Resolve(ctx context.Context, version string) (release.Release, error) {
	if f.resolveStarted != nil {
		f.once.Do(func() { close(f.resolveStarted) })
		<-f.resolveRelease
	}
	if f.resolveErr != nil {
		return release.Release{}, f.resolveErr
	}
	resolved := f.version
	if version != "" {
		resolved = strings.TrimPrefix(version, "v")
	}
	return release.Release{
		Version:     resolved,
		TagName:     "v" + resolved,
		ArchiveName: fmt.Sprintf("rigscope_%s_linux_amd64.tar.gz", resolved),
	}, nil
}

func (f *fakeFetcher) Stage(_ context.Context, rel release.Release) (release.Staged, error) {
	if f.stageErr != nil {
		return release.Staged{}, f.stageErr
	}
	dir := filepath.Join(f.stateRoot, "staging", rel.Version)
	path := filepath.Join(dir, "rigscope")
	if !f.missingStaged {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return release.Staged{}, err
		}
		if err := os.WriteFile(path, f.staged, 0o755); err != nil {
			return release.Staged{}, err
		}
	}
	return release.Staged{Version: rel.Version, Path: path, Dir: dir}, nil
}

type fakeMigrator struct {
	result    MigrateResult
	err       error
	onMigrate func() error
	calls     int
}

func (m *fakeMigrator) Migrate(_ context.Context, _ MigrateRequest) (MigrateResult, error) {
	m.calls++
	if m.onMigrate != nil {
		if err := m.onMigrate(); err != nil {
			return MigrateResult{}, err
		}
	}
	if m.err != nil {
		return MigrateResult{}, m.err
	}
	return m.result, nil
}

type fakeReadiness struct {
	err       error
	byVersion map[string]error
	calls     []string
}

func (r *fakeReadiness) Check(_ context.Context, version string) error {
	r.calls = append(r.calls, version)
	if err, ok := r.byVersion[version]; ok {
		return err
	}
	return r.err
}

type staticSettings struct{ value Settings }

func (s staticSettings) Settings() (Settings, error) { return s.value, nil }

type countingBackup struct {
	real       *FileBackup
	createErr  error
	restoreErr error
	creates    int
	restores   int
	mu         sync.Mutex
}

func (b *countingBackup) Create(ctx context.Context, req BackupRequest) (BackupResult, error) {
	b.mu.Lock()
	b.creates++
	b.mu.Unlock()
	if b.createErr != nil {
		return BackupResult{}, b.createErr
	}
	return b.real.Create(ctx, req)
}

func (b *countingBackup) Restore(ctx context.Context, dir string) error {
	b.mu.Lock()
	b.restores++
	b.mu.Unlock()
	if b.restoreErr != nil {
		return b.restoreErr
	}
	return b.real.Restore(ctx, dir)
}

// verifyingOwnership mimics the installation manifest: it refuses to claim a
// regular file whose digest no longer matches the requested one. The old
// recovery path recorded .prev with the new digest even when publication never
// happened, which fails here exactly as it fails against a genuine manifest.
type verifyingOwnership struct {
	mu      sync.Mutex
	records map[string]string
}

func newVerifyingOwnership() *verifyingOwnership {
	return &verifyingOwnership{records: map[string]string{}}
}

func (o *verifyingOwnership) Record(path string, _ ArtifactKind, sha256 string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if sha256 != "" {
		sum, err := fsutil.HashFile(path)
		if err != nil {
			return err
		}
		if sum != sha256 {
			return fmt.Errorf("refusing to record %s: content changed while it was being recorded", path)
		}
	}
	o.records[path] = sha256
	return nil
}

func (o *verifyingOwnership) Forget(path string, _ ArtifactKind) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.records, path)
	return nil
}

func (o *verifyingOwnership) recorded(path string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	value, ok := o.records[path]
	return value, ok
}

// ---------------------------------------------------------------------------
// Harness

type harness struct {
	t       *testing.T
	root    string
	state   string
	binary  string
	prev    string
	config  string
	collect string
	hubDB   string

	journalPath string
	clock       *fakeClock
	services    *systemd.Fake
	fetcher     *fakeFetcher
	migrator    *fakeMigrator
	readiness   *fakeReadiness
	backup      *countingBackup
	settings    Settings
	fs          FS
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{
		t:           t,
		root:        root,
		state:       filepath.Join(root, "state"),
		binary:      filepath.Join(root, "bin", "rigscope"),
		prev:        filepath.Join(root, "bin", "rigscope.prev"),
		config:      filepath.Join(root, "config.json"),
		collect:     filepath.Join(root, "state", "collector"),
		hubDB:       filepath.Join(root, "state", "hub.db"),
		journalPath: filepath.Join(root, "state", "upgrade-journal.json"),
		clock:       newFakeClock(),
		settings: Settings{
			HubEnabled:       true,
			CollectorEnabled: true,
			HubListen:        "127.0.0.1",
			HubPort:          8081,
		},
		fs: OSFS{},
	}
	h.fetcher = &fakeFetcher{stateRoot: h.state, version: "1.1.0", staged: []byte("new-binary")}
	h.migrator = &fakeMigrator{result: MigrateResult{Schema: 1, CompatibleDown: 1}}
	h.readiness = &fakeReadiness{}
	h.services = systemd.NewFake()
	h.services.ActiveByDefault = true
	h.backup = &countingBackup{real: NewFileBackup(nil)}

	if err := os.MkdirAll(filepath.Dir(h.binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.collect, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.binary, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.prev, []byte("older-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.config, []byte(`{"version":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.collect, "identity.json"), []byte(`{"id":"abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.hubDB, []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) layout() Layout {
	return Layout{
		StateDir:          h.state,
		Binary:            h.binary,
		PreviousBinary:    h.prev,
		ConfigFile:        h.config,
		CollectorStateDir: h.collect,
		HubDBFile:         h.hubDB,
		JournalPath:       h.journalPath,
	}
}

func (h *harness) build(mutate func(*Options)) *Coordinator {
	h.t.Helper()
	opts := Options{
		Layout:    h.layout(),
		FS:        h.fs,
		Clock:     h.clock,
		Services:  h.services,
		Releases:  h.fetcher,
		Backup:    h.backup,
		Migrator:  h.migrator,
		Readiness: h.readiness,
		Settings:  staticSettings{value: h.settings},
		Lock:      testLocker{path: filepath.Join(h.state, "install.lock")},
		Log:       io.Discard,
	}
	if mutate != nil {
		mutate(&opts)
	}
	coordinator, err := New(opts)
	if err != nil {
		h.t.Fatalf("new coordinator: %v", err)
	}
	return coordinator
}

func (h *harness) upgrade(mutate func(*Options), current string) (Result, error) {
	h.t.Helper()
	return h.build(mutate).Upgrade(context.Background(), UpgradeRequest{Version: "", CurrentVersion: current})
}

func (h *harness) journal() *Journal {
	h.t.Helper()
	journal, err := JournalStore{Path: h.journalPath, FS: OSFS{}}.Load()
	if err != nil {
		h.t.Fatalf("load journal: %v", err)
	}
	return journal
}

func (h *harness) content(path string) string {
	h.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func (h *harness) mode(path string) os.FileMode {
	h.t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		h.t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// ---------------------------------------------------------------------------
// Success paths

func TestUpgradeSuccess(t *testing.T) {
	h := newHarness(t)
	result, err := h.upgrade(nil, "1.0.0")
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if result.Phase != PhaseDone || result.To != "1.1.0" {
		t.Fatalf("result = %+v", result)
	}
	if got := h.content(h.binary); got != "new-binary" {
		t.Fatalf("binary = %q", got)
	}
	if got := h.content(h.prev); got != "old-binary" {
		t.Fatalf("previous = %q", got)
	}
	if h.mode(h.binary) != 0o755 || h.mode(h.prev) != 0o755 {
		t.Fatalf("modes = %04o/%04o", h.mode(h.binary), h.mode(h.prev))
	}
	journal := h.journal()
	if journal.Phase != PhaseDone || !journal.AcceptedWrites || journal.CompatibleDown != 1 || journal.PreviousSchema != 1 {
		t.Fatalf("journal = %+v", journal)
	}
	if journal.BackupDir == "" {
		t.Fatal("journal has no backup directory")
	}
	for _, name := range []string{"hub.db", "config.json", "collector/identity.json"} {
		if _, err := os.Stat(filepath.Join(journal.BackupDir, filepath.FromSlash(name))); err != nil {
			t.Fatalf("backup missing %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(h.state, "staging", "1.1.0")); !os.IsNotExist(err) {
		t.Fatalf("staging directory survived success: %v", err)
	}
	methods := strings.Join(h.services.Methods(), "\n")
	var order []string
	for _, method := range h.services.Methods() {
		if strings.HasPrefix(method, "stop ") || strings.HasPrefix(method, "restart ") {
			order = append(order, method)
		}
	}
	want := []string{"stop " + CollectorUnit, "stop " + HubUnit, "restart " + HubUnit, "restart " + CollectorUnit}
	if strings.Join(order, "\n") != strings.Join(want, "\n") {
		t.Fatalf("service order:\n%s\nwant:\n%s", methods, strings.Join(want, "\n"))
	}
	if h.backup.creates != 1 {
		t.Fatalf("backup creates = %d", h.backup.creates)
	}
}

func TestUpgradeMigrationBearing(t *testing.T) {
	h := newHarness(t)
	h.migrator.result = MigrateResult{Schema: 2, CompatibleDown: 1}
	h.migrator.onMigrate = func() error {
		return os.WriteFile(h.config, []byte(`{"version":2}`+"\n"), 0o600)
	}
	result, err := h.upgrade(nil, "1.0.0")
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	journal := h.journal()
	if journal.Schema != 2 || journal.PreviousSchema != 1 || journal.CompatibleDown != 1 {
		t.Fatalf("journal migration fields = %+v", journal)
	}
	if result.Phase != PhaseDone {
		t.Fatalf("phase = %s", result.Phase)
	}

	rollback, err := h.build(nil).Rollback(context.Background())
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !rollback.RolledBack || rollback.From != "1.0.0" {
		t.Fatalf("rollback result = %+v", rollback)
	}
	if got := h.content(h.binary); got != "old-binary" {
		t.Fatalf("binary after rollback = %q", got)
	}
	if got := h.content(h.prev); got != "new-binary" {
		t.Fatalf("previous after rollback = %q", got)
	}
	if got := h.content(h.config); got != `{"version":2}`+"\n" {
		// Rollback after accepted writes is compatibility-based: data is not
		// discarded, only the binary changes.
		t.Fatalf("config after compatible rollback = %q", got)
	}
	journal = h.journal()
	if journal.Phase != PhaseRolledBack {
		t.Fatalf("journal phase = %s", journal.Phase)
	}
}

func TestManualRollbackRefusedWithoutCompatibility(t *testing.T) {
	h := newHarness(t)
	h.migrator.result = MigrateResult{Schema: 2, CompatibleDown: 2}
	h.migrator.onMigrate = func() error {
		return os.WriteFile(h.config, []byte(`{"version":2}`+"\n"), 0o600)
	}
	if _, err := h.upgrade(nil, "1.0.0"); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	_, err := h.build(nil).Rollback(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("rollback err = %v", err)
	}
	if got := h.content(h.binary); got != "new-binary" {
		t.Fatalf("binary changed: %q", got)
	}
	if journal := h.journal(); journal.Phase != PhaseDone {
		t.Fatalf("journal phase = %s", journal.Phase)
	}
}

// ---------------------------------------------------------------------------
// Failure in each phase

func TestUpgradeFailureInEachPhase(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*harness)
		check  func(*testing.T, *harness)
	}{
		{
			name: "staging",
			mutate: func(h *harness) {
				h.fetcher.stageErr = errors.New("stage failed")
			},
			check: func(t *testing.T, h *harness) {
				if h.services.HasCall("stop", CollectorUnit) || h.services.HasCall("stop", HubUnit) {
					t.Fatal("services were stopped for a staging failure")
				}
			},
		},
		{
			name: "quiesce",
			mutate: func(h *harness) {
				h.services.Fail["stop "+CollectorUnit] = errors.New("stop failed")
			},
			check: func(t *testing.T, h *harness) {
				if !h.services.HasCall("restart", HubUnit) {
					t.Fatalf("services not restarted: %v", h.services.Methods())
				}
			},
		},
		{
			name: "backup",
			mutate: func(h *harness) {
				h.backup.createErr = errors.New("backup failed")
			},
			check: func(t *testing.T, h *harness) {
				if !h.services.HasCall("restart", HubUnit) || !h.services.HasCall("restart", CollectorUnit) {
					t.Fatalf("services not restarted: %v", h.services.Methods())
				}
			},
		},
		{
			name: "migrate",
			mutate: func(h *harness) {
				h.migrator.err = errors.New("migration failed")
			},
			check: func(t *testing.T, h *harness) {
				if h.backup.restores != 1 {
					t.Fatalf("backup restores = %d", h.backup.restores)
				}
				if got := h.content(h.config); got != `{"version":1}`+"\n" {
					t.Fatalf("config = %q", got)
				}
			},
		},
		{
			name: "activation",
			mutate: func(h *harness) {
				h.fetcher.missingStaged = true
			},
			check: func(t *testing.T, h *harness) {
				if _, err := os.Stat(h.prev); err == nil && h.content(h.prev) == "new-binary" {
					t.Fatal("failed activation retained the new binary")
				}
			},
		},
		{
			name: "verification",
			mutate: func(h *harness) {
				h.readiness.byVersion = map[string]error{"1.1.0": errors.New("health failed")}
			},
			check: func(t *testing.T, h *harness) {
				if got := h.content(h.binary); got != "old-binary" {
					t.Fatalf("binary = %q", got)
				}
				if got := h.content(h.prev); got != "new-binary" {
					t.Fatalf("previous = %q", got)
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			test.mutate(h)
			_, err := h.upgrade(nil, "1.0.0")
			if err == nil {
				t.Fatal("upgrade succeeded")
			}
			if !strings.Contains(err.Error(), "rolled back") && !strings.Contains(err.Error(), "rollback") {
				t.Fatalf("err = %v", err)
			}
			if journal := h.journal(); journal.Phase != PhaseRolledBack {
				t.Fatalf("journal phase = %s", journal.Phase)
			}
			if got := h.content(h.binary); got != "old-binary" {
				t.Fatalf("binary = %q", got)
			}
			test.check(t, h)
			if test.name != "staging" && test.name != "activation" {
				if !h.services.HasCall("restart", HubUnit) {
					t.Fatalf("hub not restarted: %v", h.services.Methods())
				}
			}
		})
	}
}

func TestVerificationFailureWithoutCompatibilityNeedsAttention(t *testing.T) {
	h := newHarness(t)
	h.migrator.result = MigrateResult{Schema: 2, CompatibleDown: 2}
	h.migrator.onMigrate = func() error {
		return os.WriteFile(h.config, []byte(`{"version":2}`+"\n"), 0o600)
	}
	h.readiness.byVersion = map[string]error{"1.1.0": errors.New("health failed")}
	_, err := h.upgrade(nil, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
	if got := h.content(h.binary); got != "new-binary" {
		t.Fatalf("binary was discarded: %q", got)
	}
	if got := h.content(h.config); got != `{"version":2}`+"\n" {
		t.Fatalf("data was discarded: %q", got)
	}
	journal := h.journal()
	if journal.Phase != PhaseFailedNeedsAttention {
		t.Fatalf("phase = %s", journal.Phase)
	}
	if h.backup.restores != 0 {
		t.Fatalf("backup restored despite incompatibility")
	}
}

// ---------------------------------------------------------------------------
// Crash recovery between phases

func TestRecoveryAfterCrashBetweenPhases(t *testing.T) {
	phases := []Phase{PhaseLocked, PhaseStaged, PhaseQuiesced, PhaseBackedUp, PhaseMigrated, PhasePrepared, PhasePreviousPublished, PhaseActivated, PhaseVerified}
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			h := newHarness(t)
			h.migrator.onMigrate = func() error {
				return os.WriteFile(h.config, []byte(`{"version":1,"migrated":true}`+"\n"), 0o600)
			}
			crash := phase
			_, err := h.upgrade(func(opts *Options) {
				opts.AfterPhase = func(current Phase) error {
					if current == crash {
						return errors.New("simulated process death")
					}
					return nil
				}
			}, "1.0.0")
			if err == nil || !strings.Contains(err.Error(), "simulated crash") {
				t.Fatalf("upgrade err = %v", err)
			}
			if journal := h.journal(); journal.Phase != phase {
				t.Fatalf("journal phase after crash = %s, want %s", journal.Phase, phase)
			}

			// A new process constructs a fresh coordinator from the journal.
			if _, err := h.build(nil).Recover(context.Background()); err != nil {
				t.Fatalf("recover: %v", err)
			}
			journal := h.journal()
			switch phase {
			case PhaseVerified:
				if journal.Phase != PhaseDone {
					t.Fatalf("verified recovery phase = %s", journal.Phase)
				}
				if got := h.content(h.binary); got != "new-binary" {
					t.Fatalf("binary = %q", got)
				}
			default:
				if journal.Phase != PhaseRolledBack {
					t.Fatalf("recovery phase = %s", journal.Phase)
				}
				if got := h.content(h.binary); got != "old-binary" {
					t.Fatalf("binary = %q", got)
				}
				if phase == PhaseMigrated || phase == PhasePrepared || phase == PhaseBackedUp {
					if got := h.content(h.config); got != `{"version":1}`+"\n" {
						t.Fatalf("config = %q", got)
					}
				}
				if phase == PhaseQuiesced || phase == PhaseBackedUp || phase == PhaseMigrated || phase == PhasePrepared || phase == PhasePreviousPublished || phase == PhaseActivated {
					if !h.services.HasCall("restart", HubUnit) || !h.services.HasCall("restart", CollectorUnit) {
						t.Fatalf("services not restarted after recovery: %v", h.services.Methods())
					}
				}
			}
			if _, err := os.Stat(filepath.Join(h.state, "staging", "1.1.0")); !os.IsNotExist(err) {
				t.Fatalf("staging directory survived recovery: %v", err)
			}
		})
	}
}

func TestRecoveryOfFailedNeedsAttentionRefuses(t *testing.T) {
	h := newHarness(t)
	h.migrator.result = MigrateResult{Schema: 2, CompatibleDown: 2}
	h.migrator.onMigrate = func() error {
		return os.WriteFile(h.config, []byte(`{"version":2}`+"\n"), 0o600)
	}
	h.readiness.byVersion = map[string]error{"1.1.0": errors.New("health failed")}
	if _, err := h.upgrade(nil, "1.0.0"); err == nil {
		t.Fatal("upgrade succeeded")
	}
	_, err := h.build(nil).Recover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "needs attention") {
		t.Fatalf("recover err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reviewer regressions

type partialBackup struct {
	dir string
}

func (b *partialBackup) Create(ctx context.Context, req BackupRequest) (BackupResult, error) {
	b.dir = req.Dir
	if err := os.MkdirAll(req.Dir, 0o700); err != nil {
		return BackupResult{}, err
	}
	if err := os.WriteFile(filepath.Join(req.Dir, "hub.db"), []byte("partial"), 0o600); err != nil {
		return BackupResult{}, err
	}
	return BackupResult{}, errors.New("disk full before the manifest was durable")
}

func (b *partialBackup) Restore(context.Context, string) error { return nil }

type failingSettings struct{}

func (failingSettings) Settings() (Settings, error) {
	return Settings{}, errors.New("configuration is invalid")
}

// committingServices returns an error from Restart after the unit was actually
// started and wrote data, reproducing a committed restart that reports failure.
type committingServices struct {
	*systemd.Fake
	failAfter func(string) error
}

func (s *committingServices) Restart(ctx context.Context, unit string) error {
	if err := s.Fake.Restart(ctx, unit); err != nil {
		return err
	}
	if s.failAfter != nil {
		return s.failAfter(unit)
	}
	return nil
}

func TestRestartThatCommitsThenErrorsKeepsNewWrites(t *testing.T) {
	h := newHarness(t)
	services := &committingServices{Fake: h.services}
	var once sync.Once
	services.failAfter = func(unit string) error {
		if unit != HubUnit {
			return nil
		}
		failed := false
		once.Do(func() {
			if err := os.WriteFile(h.hubDB, []byte("written-by-new-version"), 0o600); err != nil {
				t.Fatal(err)
			}
			failed = true
		})
		if failed {
			return errors.New("restart failed after the unit started")
		}
		return nil
	}
	_, err := h.upgrade(func(opts *Options) { opts.Services = services }, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("upgrade err = %v", err)
	}
	journal := h.journal()
	if journal.Phase != PhaseRolledBack {
		t.Fatalf("phase = %s", journal.Phase)
	}
	if !journal.AcceptedWrites {
		t.Fatal("acceptedWrites was not journaled before the first start")
	}
	if got := h.content(h.hubDB); got != "written-by-new-version" {
		t.Fatalf("rollback restored the snapshot over committed writes: %q", got)
	}
	if got := h.content(h.binary); got != "old-binary" {
		t.Fatalf("binary = %q", got)
	}
}

func TestPartialBackupIsNeverJournaledAndIsDiscarded(t *testing.T) {
	h := newHarness(t)
	backup := &partialBackup{}
	_, err := h.upgrade(func(opts *Options) { opts.Backup = backup }, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("upgrade err = %v", err)
	}
	journal := h.journal()
	if journal.BackupComplete {
		t.Fatal("an incomplete backup was journaled as complete")
	}
	if journal.Phase != PhaseRolledBack {
		t.Fatalf("phase = %s", journal.Phase)
	}
	if _, err := os.Stat(backup.dir); !os.IsNotExist(err) {
		t.Fatalf("partial backup was kept: %v", err)
	}
}

func TestFailedMigrationAndFailedRestoreNeedsAttention(t *testing.T) {
	h := newHarness(t)
	h.migrator.onMigrate = func() error {
		if err := os.WriteFile(h.config, []byte(`{"version":2}`+"\n"), 0o600); err != nil {
			return err
		}
		return errors.New("migration failed after rewriting the config")
	}
	h.backup.restoreErr = errors.New("restore failed")
	_, err := h.upgrade(nil, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "artifacts kept") {
		t.Fatalf("err = %v", err)
	}
	journal := h.journal()
	if journal.Phase != PhaseFailedNeedsAttention {
		t.Fatalf("phase = %s", journal.Phase)
	}
	if _, err := os.Stat(filepath.Join(journal.BackupDir, backupManifestName)); err != nil {
		t.Fatalf("backup artifacts were discarded: %v", err)
	}
	if got := h.content(h.config); !strings.Contains(got, `"version":2`) {
		t.Fatalf("migrated data was discarded: %q", got)
	}
	if got := h.content(h.binary); got != "old-binary" {
		t.Fatalf("binary was changed after data restore failure: %q", got)
	}
}

func TestCrashAfterRestorationNeverReplaysSnapshot(t *testing.T) {
	h := newHarness(t)
	h.migrator.onMigrate = func() error {
		if err := os.WriteFile(h.config, []byte(`{"version":2}`+"\n"), 0o600); err != nil {
			return err
		}
		return errors.New("migration failed")
	}
	_, err := h.upgrade(func(opts *Options) {
		opts.AfterPhase = func(phase Phase) error {
			if phase == PhaseRollbackWritersStarted {
				return errors.New("simulated process death")
			}
			return nil
		}
	}, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "simulated crash") {
		t.Fatalf("upgrade err = %v", err)
	}
	if got := h.content(h.config); got != `{"version":1}`+"\n" {
		t.Fatalf("data was not restored before the crash: %q", got)
	}
	// The restored old version may write at any moment now.
	if err := os.WriteFile(h.config, []byte(`{"version":1,"oldWrote":true}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.build(nil).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if journal := h.journal(); journal.Phase != PhaseRolledBack {
		t.Fatalf("phase = %s", journal.Phase)
	}
	if got := h.content(h.config); got != `{"version":1,"oldWrote":true}`+"\n" {
		t.Fatalf("recovery replayed the snapshot over old-version writes: %q", got)
	}
}

func TestRecoveryDoesNotLoadMigratedConfig(t *testing.T) {
	h := newHarness(t)
	h.migrator.onMigrate = func() error {
		return os.WriteFile(h.config, []byte(`{"version":2}`+"\n"), 0o600)
	}
	_, err := h.upgrade(func(opts *Options) {
		opts.AfterPhase = func(phase Phase) error {
			if phase == PhasePrepared {
				return errors.New("simulated process death")
			}
			return nil
		}
	}, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "simulated crash") {
		t.Fatalf("upgrade err = %v", err)
	}
	if err := os.WriteFile(h.config, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A coordinator whose settings source always fails still has to recover:
	// the journal carries the layout and readiness settings.
	if _, err := h.build(func(opts *Options) { opts.Settings = failingSettings{} }).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if journal := h.journal(); journal.Phase != PhaseRolledBack {
		t.Fatalf("phase = %s", journal.Phase)
	}
	if got := h.content(h.config); got != `{"version":1}`+"\n" {
		t.Fatalf("config was not restored from the journal snapshot: %q", got)
	}
	if got := h.content(h.binary); got != "old-binary" {
		t.Fatalf("binary = %q", got)
	}
}

func TestRecoveryDiscardsIncompletePreparedBinaryAndStalePrevious(t *testing.T) {
	h := newHarness(t)
	_, err := h.upgrade(func(opts *Options) {
		opts.AfterPhase = func(phase Phase) error {
			if phase == PhasePrepared {
				return errors.New("simulated process death")
			}
			return nil
		}
	}, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "simulated crash") {
		t.Fatalf("upgrade err = %v", err)
	}
	journal := h.journal()
	newSum, _ := fsutil.HashFile(journal.StagedBinary)
	previousSum, _ := fsutil.HashFile(h.binary)
	preparedPath := tempNameFor(h.binary, journal.ID, "new")
	if err := os.WriteFile(preparedPath, []byte("truncated-copy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.prev, []byte("stale-previous"), 0o755); err != nil {
		t.Fatal(err)
	}
	journal.PublishStarted = true
	journal.PreparedBinary = preparedPath
	journal.NewBinarySHA256 = newSum
	journal.PreviousBinaryPath = h.prev
	journal.PreviousBinarySHA256 = previousSum
	if err := (JournalStore{Path: h.journalPath, FS: OSFS{}}).Save(journal); err != nil {
		t.Fatal(err)
	}

	if _, err := h.build(nil).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := h.content(h.binary); got != "old-binary" {
		t.Fatalf("incomplete prepared binary was published: %q", got)
	}
	if _, err := os.Stat(preparedPath); !os.IsNotExist(err) {
		t.Fatalf("transaction temp survived recovery: %v", err)
	}
	if journal := h.journal(); journal.Phase != PhaseRolledBack {
		t.Fatalf("phase = %s", journal.Phase)
	}
}

func TestRecoveryRollsBackWhenPublicationNeverReachedTheSwap(t *testing.T) {
	h := newHarness(t)
	if _, err := h.upgrade(nil, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	journal := h.journal()
	// Reconstruct the state after the journal recorded publication intent but
	// the process died before PublishPrepared touched anything: the target is
	// still the old release and .prev still holds the release before it. The
	// backup from the completed upgrade is genuine.
	newSum, err := fsutil.HashFile(h.binary)
	if err != nil {
		t.Fatal(err)
	}
	oldSum, err := fsutil.HashFile(h.prev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.binary, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.prev, []byte("older-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	journal.Phase = PhasePrepared
	journal.PublishStarted = true
	journal.NewBinarySHA256 = newSum
	journal.PreviousBinarySHA256 = oldSum
	journal.PreparedBinary = tempNameFor(h.binary, journal.ID, "new")
	journal.AcceptedWrites = false
	if err := (JournalStore{Path: h.journalPath, FS: OSFS{}}).Save(journal); err != nil {
		t.Fatal(err)
	}
	ownership := newVerifyingOwnership()
	if _, err := h.build(func(opts *Options) { opts.Ownership = ownership }).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := h.content(h.binary); got != "old-binary" {
		t.Fatalf("binary = %q", got)
	}
	if got := h.content(h.prev); got != "older-binary" {
		t.Fatalf("previous = %q; recovery claimed a swap that never happened", got)
	}
	if journal := h.journal(); journal.Phase != PhaseRolledBack {
		t.Fatalf("phase = %s", journal.Phase)
	}
	if sum, ok := ownership.recorded(h.prev); ok && sum == newSum {
		t.Fatalf("manifest claimed .prev as the failed release without a swap (recorded %s)", sum)
	}
	if !h.services.HasCall("restart", HubUnit) || !h.services.HasCall("restart", CollectorUnit) {
		t.Fatalf("services were not restarted: %v", h.services.Methods())
	}
}

func TestRecoveryRefusesUnverifiedRestoreTemps(t *testing.T) {
	h := newHarness(t)
	if _, err := h.upgrade(nil, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	journal := h.journal()
	// Rewind to a rollback interrupted between staging the two candidates and
	// renaming them. The journal records both digests.
	journal.Phase = PhaseRollingBack
	journal.ServicesStopped = true
	journal.AcceptedWrites = true
	journal.PublishStarted = true
	journal.NewBinarySHA256, _ = fsutil.HashFile(h.binary)
	journal.PreviousBinarySHA256, _ = fsutil.HashFile(h.prev)
	if err := (JournalStore{Path: h.journalPath, FS: OSFS{}}).Save(journal); err != nil {
		t.Fatal(err)
	}
	bogus := tempNameFor(h.binary, journal.ID, "restore-new")
	if err := os.WriteFile(bogus, []byte("unverified-foreign-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := h.build(nil).Recover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("recover err = %v", err)
	}
	if got := h.content(h.binary); got != "new-binary" {
		t.Fatalf("unverified candidate was published over the installed binary: %q", got)
	}
	if got := h.content(h.prev); got != "old-binary" {
		t.Fatalf("previous binary changed: %q", got)
	}
	if got := h.content(bogus); got != "unverified-foreign-binary" {
		t.Fatalf("unverified artifact was not preserved: %q", got)
	}
	if got := h.journal().Phase; got != PhaseFailedNeedsAttention {
		t.Fatalf("phase = %s", got)
	}
}

func TestRecoveryDiscardsIncompletePreparedBinaryAfterPublish(t *testing.T) {
	h := newHarness(t)
	if _, err := h.upgrade(nil, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	// Turn the completed transaction back into an interrupted one: target is
	// the new release, previous holds the old release, and a truncated .new
	// file is left behind by a crash. Recovery must ignore the temp file and
	// restore from the journal digest.
	journal := h.journal()
	journal.Phase = PhasePrepared
	journal.PublishStarted = true
	journal.PreparedBinary = tempNameFor(h.binary, journal.ID, "new")
	journal.NewBinarySHA256, _ = fsutil.HashFile(h.binary)
	journal.PreviousBinarySHA256, _ = fsutil.HashFile(h.prev)
	if err := os.WriteFile(journal.PreparedBinary, []byte("truncated-copy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (JournalStore{Path: h.journalPath, FS: OSFS{}}).Save(journal); err != nil {
		t.Fatal(err)
	}
	if _, err := h.build(nil).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := h.content(h.binary); got != "old-binary" {
		t.Fatalf("incomplete prepared binary replaced the target: %q", got)
	}
	if got := h.content(h.prev); got != "new-binary" {
		t.Fatalf("previous = %q", got)
	}
	if _, err := os.Stat(journal.PreparedBinary); !os.IsNotExist(err) {
		t.Fatalf("transaction temp survived recovery: %v", err)
	}
}

func TestManualRollbackDoesNotDependOnStaging(t *testing.T) {
	h := newHarness(t)
	if _, err := h.upgrade(nil, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	journal := h.journal()
	// A manual rollback after the upgrade has no staging files left; binary
	// identities must come from the journal alone.
	if err := os.RemoveAll(journal.StagedDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(journal.BackupDir); err != nil {
		t.Fatal(err)
	}
	result, err := h.build(nil).Rollback(context.Background())
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !result.RolledBack {
		t.Fatalf("result = %+v", result)
	}
	if got := h.content(h.binary); got != "old-binary" {
		t.Fatalf("binary = %q", got)
	}
	if got := h.content(h.prev); got != "new-binary" {
		t.Fatalf("previous = %q", got)
	}
}

func TestManualRollbackRefusesMissingPreviousBinary(t *testing.T) {
	h := newHarness(t)
	if _, err := h.upgrade(nil, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.prev); err != nil {
		t.Fatal(err)
	}
	_, err := h.build(nil).Rollback(context.Background())
	if err == nil {
		t.Fatal("rollback succeeded without a previous binary")
	}
	if journal := h.journal(); journal.Phase != PhaseFailedNeedsAttention {
		t.Fatalf("phase = %s", journal.Phase)
	}
	if got := h.content(h.binary); got != "new-binary" {
		t.Fatalf("binary was treated as already rolled back: %q", got)
	}
}

func TestBackupRestoresWALSet(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, "hub.db")
	wal := db + "-wal"
	shm := db + "-shm"
	for path, content := range map[string]string{db: "db", wal: "wal-data", shm: "shm-data"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backup := NewFileBackup(nil)
	dir := filepath.Join(root, "backup")
	if _, err := backup.Create(context.Background(), BackupRequest{Dir: dir, HubDBFile: db}); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{db: "new-db", wal: "new-wal", shm: "new-shm"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := backup.Restore(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{db: "db", wal: "wal-data", shm: "shm-data"} {
		if got := readFile(t, path); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}

	// A sidecar that did not exist at backup time must be removed, not kept.
	root2 := t.TempDir()
	db2 := filepath.Join(root2, "hub.db")
	if err := os.WriteFile(db2, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir2 := filepath.Join(root2, "backup")
	if _, err := backup.Create(context.Background(), BackupRequest{Dir: dir2, HubDBFile: db2}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db2+"-wal", []byte("new-wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db2+"-shm", []byte("new-shm"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(context.Background(), dir2); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{db2 + "-wal", db2 + "-shm"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s was not removed by restore: %v", path, err)
		}
	}
	if got := readFile(t, db2); got != "db" {
		t.Fatalf("db = %q", got)
	}
}

func TestBackupRetentionKeepsNewestThreeAndProtected(t *testing.T) {
	h := newHarness(t)
	root := filepath.Join(h.state, "backups")
	var dirs []string
	for _, name := range []string{"20260101T000000Z-1", "20260102T000000Z-2", "20260103T000000Z-3", "20260104T000000Z-4", "20260105T000000Z-5"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, backupManifestName), []byte(`{"format":2}`), 0o600); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	coordinator := h.build(nil)
	journal := &Journal{AcceptedWrites: true, Layout: h.layout()}
	coordinator.cleanupBackups(journal)
	for index, dir := range dirs {
		_, err := os.Stat(dir)
		if index < 2 && !os.IsNotExist(err) {
			t.Fatalf("old backup %s survived retention: %v", dir, err)
		}
		if index >= 2 && err != nil {
			t.Fatalf("new backup %s was removed: %v", dir, err)
		}
	}

	// A snapshot needed by a supported rollback is protected even when older
	// than the three newest directories.
	for _, name := range []string{"20260106T000000Z-6", "20260107T000000Z-7"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, backupManifestName), []byte(`{"format":2}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	journal = &Journal{AcceptedWrites: false, Layout: h.layout(), BackupDir: filepath.Join(root, "20260103T000000Z-3")}
	coordinator.cleanupBackups(journal)
	if _, err := os.Stat(journal.BackupDir); err != nil {
		t.Fatalf("protected rollback snapshot was removed: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func tempNameFor(target, token, suffix string) string {
	name := filepath.Base(target) + "." + token + "." + suffix
	return filepath.Join(filepath.Dir(target), "."+name)
}

// ---------------------------------------------------------------------------
// Locking

func TestLockRejectsConcurrentInvocation(t *testing.T) {
	h := newHarness(t)
	locker := testLocker{path: filepath.Join(h.state, "install.lock")}
	unlock, err := locker.Lock(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = h.build(nil).Upgrade(ctx, UpgradeRequest{CurrentVersion: "1.0.0"})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(h.journalPath); !os.IsNotExist(statErr) {
		t.Fatal("concurrent invocation wrote a journal")
	}
}

func TestTwoConcurrentUpgradesOneWins(t *testing.T) {
	h := newHarness(t)
	h.fetcher.resolveStarted = make(chan struct{})
	h.fetcher.resolveRelease = make(chan struct{})

	type outcome struct {
		result Result
		err    error
	}
	first := make(chan outcome, 1)
	go func() {
		result, err := h.build(nil).Upgrade(context.Background(), UpgradeRequest{CurrentVersion: "1.0.0"})
		first <- outcome{result: result, err: err}
	}()
	<-h.fetcher.resolveStarted

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := h.build(nil).Upgrade(ctx, UpgradeRequest{CurrentVersion: "1.0.0"}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second upgrade err = %v", err)
	}
	close(h.fetcher.resolveRelease)
	got := <-first
	if got.err != nil || got.result.Phase != PhaseDone {
		t.Fatalf("first outcome = %+v, %v", got.result, got.err)
	}
}

// ---------------------------------------------------------------------------
// Backup and readiness units

func TestBackupRestoresFilesAndModes(t *testing.T) {
	root := t.TempDir()
	collector := filepath.Join(root, "collector")
	if err := os.MkdirAll(filepath.Join(collector, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "config.json")
	db := filepath.Join(root, "hub.db")
	if err := os.WriteFile(config, []byte("config"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(collector, "nested", "id")
	if err := os.WriteFile(identity, []byte("identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := NewFileBackup(nil)
	dir := filepath.Join(root, "backup")
	if _, err := backup.Create(context.Background(), BackupRequest{Dir: dir, From: "1.0.0", To: "1.1.0", ConfigFile: config, HubDBFile: db, CollectorDir: collector}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(config, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(context.Background(), dir); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if data, _ := os.ReadFile(config); string(data) != "config" {
		t.Fatalf("config = %q", data)
	}
	if info, _ := os.Stat(config); info.Mode().Perm() != 0o640 {
		t.Fatalf("config mode = %04o", info.Mode().Perm())
	}
	if data, _ := os.ReadFile(db); string(data) != "db" {
		t.Fatalf("db = %q", data)
	}
	if data, _ := os.ReadFile(identity); string(data) != "identity" {
		t.Fatalf("identity = %q", data)
	}
	if info, _ := os.Stat(filepath.Join(collector, "nested")); info.Mode().Perm() != 0o750 {
		t.Fatalf("directory mode = %04o", info.Mode().Perm())
	}
}

func TestBackupRestoreRemovesStateThatDidNotExist(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config.json")
	db := filepath.Join(root, "hub.db")
	collector := filepath.Join(root, "collector")
	if err := os.WriteFile(config, []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := NewFileBackup(nil)
	dir := filepath.Join(root, "backup")
	if _, err := backup.Create(context.Background(), BackupRequest{Dir: dir, ConfigFile: config, HubDBFile: db, CollectorDir: collector}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("new database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(collector, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(collector, "id"), []byte("new identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("hub database created by the new version survived restore: %v", err)
	}
	if _, err := os.Stat(collector); !os.IsNotExist(err) {
		t.Fatalf("collector directory created by the new version survived restore: %v", err)
	}
}

func TestBackupRefusesRecursiveCollectorDirectory(t *testing.T) {
	root := t.TempDir()
	collector := filepath.Join(root, "collector")
	if err := os.MkdirAll(collector, 0o700); err != nil {
		t.Fatal(err)
	}
	backup := NewFileBackup(nil)
	if _, err := backup.Create(context.Background(), BackupRequest{Dir: filepath.Join(collector, "backup"), CollectorDir: collector}); err == nil {
		t.Fatal("recursive backup accepted")
	}
}

func TestBackupRestoreReportsEveryError(t *testing.T) {
	root := t.TempDir()
	backup := NewFileBackup(nil)
	dir := filepath.Join(root, "backup")
	if _, err := backup.Create(context.Background(), BackupRequest{Dir: dir, From: "1", To: "2"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupManifestName), []byte(`{"format":2,"items":[{"source":"`+filepath.Join(root, "a")+`","relative":"a","kind":"file","existed":true},{"source":"`+filepath.Join(root, "b")+`","relative":"b","kind":"file","existed":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := backup.Restore(context.Background(), dir)
	if err == nil {
		t.Fatal("restore reported no errors")
	}
	if strings.Count(err.Error(), "restore") < 2 {
		t.Fatalf("errors were not joined: %v", err)
	}
}

func TestReadinessChecksHubVersionTLSAndHost(t *testing.T) {
	var gotHost string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		_, _ = w.Write([]byte(`{"version":"1.1.0"}`))
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	portNumber := 0
	if _, err := fmt.Sscanf(port, "%d", &portNumber); err != nil {
		t.Fatal(err)
	}
	// httptest's certificate is valid for example.com (and 127.0.0.1). The
	// canonical origin must drive certificate verification even though the
	// probe dials the loopback listen address.
	settings := staticSettings{value: Settings{
		HubEnabled: true,
		HubListen:  "127.0.0.1",
		HubPort:    portNumber,
		HubOrigin:  "https://example.com",
		HubTLSCert: "/etc/cert.pem",
		HubTLSKey:  "/etc/key.pem",
	}}
	services := systemd.NewFake()
	services.Active["rigscope-hub.service"] = true
	readiness := NewComponentReadiness(ReadinessOptions{
		Services: services,
		Settings: settings,
		Client:   server.Client(),
		Attempts: 1,
		Sleep:    func(time.Duration) {},
	})
	if err := readiness.Check(context.Background(), "1.1.0"); err != nil {
		t.Fatalf("check: %v", err)
	}
	if gotHost != "example.com" {
		t.Fatalf("Host header = %q", gotHost)
	}
	if err := readiness.Check(context.Background(), "1.0.0"); err == nil || !strings.Contains(err.Error(), "expected 1.0.0") {
		t.Fatalf("wrong version err = %v", err)
	}

	wrongName := NewComponentReadiness(ReadinessOptions{
		Services: services,
		Settings: staticSettings{value: Settings{
			HubEnabled: true,
			HubListen:  "127.0.0.1",
			HubPort:    portNumber,
			HubOrigin:  "https://wrong.invalid",
			HubTLSCert: "/etc/cert.pem",
			HubTLSKey:  "/etc/key.pem",
		}},
		Client:   server.Client(),
		Attempts: 1,
		Sleep:    func(time.Duration) {},
	})
	if err := wrongName.Check(context.Background(), "1.1.0"); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("certificate was not verified against the public origin: %v", err)
	}
}

func TestReadinessCollectorCheck(t *testing.T) {
	services := systemd.NewFake()
	services.ActiveByDefault = true
	settings := staticSettings{value: Settings{CollectorEnabled: true}}
	checker := collectorCheckFunc(func(context.Context) error { return errors.New("collector check failed") })
	readiness := NewComponentReadiness(ReadinessOptions{
		Services:  services,
		Settings:  settings,
		Collector: checker,
		Attempts:  1,
		Sleep:     func(time.Duration) {},
	})
	if err := readiness.Check(context.Background(), "1.1.0"); err == nil || !strings.Contains(err.Error(), "collector check failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecCollectorCheckRunsInstalledBinary(t *testing.T) {
	var got *exec.Cmd
	checker := NewExecCollectorCheck(ExecCollectorOptions{
		Binary:     "/usr/local/bin/rigscope",
		ConfigFile: "/etc/rigscope/config.json",
		StateDir:   "/var/lib/rigscope",
		Scope:      "system",
		Euid:       func() int { return 0 },
		Lookup:     func(string) (int, int, error) { return 4242, 4243, nil },
		GroupID:    func(string) (int, error) { return 4244, nil },
		Run: func(_ context.Context, command *exec.Cmd) error {
			got = command
			return nil
		},
	})
	if err := checker.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"collector", "--check", "--config", "/etc/rigscope/config.json", "--state-dir", "/var/lib/rigscope", "--system"}
	if strings.Join(got.Args[1:], " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v", got.Args)
	}
	if got.SysProcAttr == nil || got.SysProcAttr.Credential == nil ||
		got.SysProcAttr.Credential.Uid != 4242 || got.SysProcAttr.Credential.Gid != 4244 ||
		len(got.SysProcAttr.Credential.Groups) != 1 || got.SysProcAttr.Credential.Groups[0] != 4244 {
		t.Fatalf("collector check did not drop to the service account with the shared group: %+v", got.SysProcAttr)
	}
}

func TestExecCollectorCheckReportsFailureAndSkipsWithoutRoot(t *testing.T) {
	checker := NewExecCollectorCheck(ExecCollectorOptions{
		Binary:     "/usr/local/bin/rigscope",
		ConfigFile: "/etc/rigscope/config.json",
		StateDir:   "/var/lib/rigscope",
		Scope:      "system",
		Euid:       func() int { return 0 },
		Lookup:     func(string) (int, int, error) { return 1, 1, nil },
		GroupID:    func(string) (int, error) { return 2, nil },
		Run: func(_ context.Context, command *exec.Cmd) error {
			_, _ = command.Stderr.Write([]byte("collector lease is not writable\n"))
			return errors.New("exit 1")
		},
	})
	if err := checker.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "lease is not writable") {
		t.Fatalf("err = %v", err)
	}

	var notes strings.Builder
	skipped := NewExecCollectorCheck(ExecCollectorOptions{
		Binary:     "/usr/local/bin/rigscope",
		ConfigFile: "/etc/rigscope/config.json",
		StateDir:   "/var/lib/rigscope",
		Scope:      "system",
		Euid:       func() int { return 1000 },
		Log:        &notes,
		Run: func(context.Context, *exec.Cmd) error {
			t.Fatal("a non-root process must not run the collector-account probe")
			return nil
		},
	})
	if err := skipped.Ready(context.Background()); err != nil {
		t.Fatalf("skip returned an error: %v", err)
	}
	if !strings.Contains(notes.String(), "skipping the collector-account collector check") {
		t.Fatalf("skip was not recorded: %q", notes.String())
	}
}

type collectorCheckFunc func(context.Context) error

func (f collectorCheckFunc) Ready(ctx context.Context) error { return f(ctx) }

func TestExecMigratorParsesOutput(t *testing.T) {
	migrator := &ExecMigrator{Run: func(context.Context, *exec.Cmd) ([]byte, error) {
		return []byte(`{"schema":2,"compatibleDown":1}` + "\n"), nil
	}}
	result, err := migrator.Migrate(context.Background(), MigrateRequest{Binary: "/staged/rigscope", From: "1.0.0", To: "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Schema != 2 || result.CompatibleDown != 1 {
		t.Fatalf("result = %+v", result)
	}

	migrator.Run = func(context.Context, *exec.Cmd) ([]byte, error) {
		return []byte("not json"), nil
	}
	if _, err := migrator.Migrate(context.Background(), MigrateRequest{Binary: "/staged/rigscope"}); err == nil {
		t.Fatal("malformed output accepted")
	}
	migrator.Run = func(context.Context, *exec.Cmd) ([]byte, error) {
		return []byte(`{"schema":1,"compatibleDown":1}{"extra":1}`), nil
	}
	if _, err := migrator.Migrate(context.Background(), MigrateRequest{Binary: "/staged/rigscope"}); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestExecMigratorHasDeadlineAndBoundedOutput(t *testing.T) {
	dir := t.TempDir()
	slow := filepath.Join(dir, "slow")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := (&ExecMigrator{Timeout: 100 * time.Millisecond}).Migrate(context.Background(), MigrateRequest{Binary: slow})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("slow migrate err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("migrate was not killed promptly: %s", elapsed)
	}

	noisy := filepath.Join(dir, "noisy")
	if err := os.WriteFile(noisy, []byte("#!/bin/sh\nhead -c 100000 /dev/zero | tr '\\0' x\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = (&ExecMigrator{Timeout: 5 * time.Second}).Migrate(context.Background(), MigrateRequest{Binary: noisy})
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("noisy migrate err = %v", err)
	}
}

func TestExecMigratorInheritsTheInstallationLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "install.lock")
	held, err := lock.Acquire(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	var got *exec.Cmd
	migrator := &ExecMigrator{Run: func(_ context.Context, command *exec.Cmd) ([]byte, error) {
		got = command
		return []byte(`{"schema":1,"compatibleDown":1}`), nil
	}}
	if _, err := migrator.Migrate(context.Background(), MigrateRequest{
		Binary:   "/staged/rigscope",
		System:   true,
		LockFile: held.File(),
	}); err != nil {
		t.Fatal(err)
	}
	if len(got.ExtraFiles) != 1 || got.ExtraFiles[0] != held.File() {
		t.Fatalf("the locked descriptor was not inherited: %+v", got.ExtraFiles)
	}
	want := lock.InheritedLockFDEnv + "=3"
	found := false
	for _, entry := range got.Env {
		if entry == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("environment does not name the inherited descriptor: %v", got.Env)
	}
	args := strings.Join(got.Args, " ")
	if strings.Contains(args, "--lock-held") {
		t.Fatalf("a bypass flag was passed: %s", args)
	}
	if !strings.HasSuffix(args, "migrate --from  --to --config --state --system") && !strings.Contains(args, "--system") {
		t.Fatalf("system scope was not passed: %s", args)
	}
}

func TestExecMigratorInheritsTheHubRuntimeLease(t *testing.T) {
	dir := t.TempDir()
	installPath := filepath.Join(dir, "install.lock")
	hubPath := filepath.Join(dir, "hub.lock")
	installLock, err := lock.Acquire(installPath)
	if err != nil {
		t.Fatal(err)
	}
	defer installLock.Release()
	hubLock, err := lock.Acquire(hubPath)
	if err != nil {
		t.Fatal(err)
	}
	defer hubLock.Release()

	var got *exec.Cmd
	migrator := &ExecMigrator{Run: func(_ context.Context, command *exec.Cmd) ([]byte, error) {
		got = command
		return []byte(`{"schema":1,"compatibleDown":1}`), nil
	}}
	if _, err := migrator.Migrate(context.Background(), MigrateRequest{
		Binary:      "/staged/rigscope",
		LockFile:    installLock.File(),
		HubLockFile: hubLock.File(),
	}); err != nil {
		t.Fatal(err)
	}
	if len(got.ExtraFiles) != 2 || got.ExtraFiles[0] != installLock.File() || got.ExtraFiles[1] != hubLock.File() {
		t.Fatalf("both locked descriptors were not inherited: %+v", got.ExtraFiles)
	}
	want := map[string]bool{
		lock.InheritedLockFDEnv + "=3":    false,
		lock.InheritedHubLockFDEnv + "=4": false,
	}
	for _, entry := range got.Env {
		if _, ok := want[entry]; ok {
			want[entry] = true
		}
	}
	for entry, found := range want {
		if !found {
			t.Fatalf("environment does not name %s: %v", entry, got.Env)
		}
	}
}

// testHubLease is an in-memory activation.HubLease that records whether the
// transaction held it at a given moment.
type testHubLease struct {
	mu       sync.Mutex
	busy     bool
	held     bool
	acquires int
	releases int
}

func (l *testHubLease) Acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.busy {
		return nil, errors.New("the hub is running")
	}
	if l.held {
		return nil, errors.New("the hub runtime lease is already held")
	}
	l.held = true
	l.acquires++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.held = false
		l.releases++
	}, nil
}

func (l *testHubLease) File() *os.File { return nil }

func (l *testHubLease) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

type leaseObservingBackup struct {
	*countingBackup
	lease    *testHubLease
	atCreate bool
}

func (b *leaseObservingBackup) Create(ctx context.Context, req BackupRequest) (BackupResult, error) {
	b.atCreate = b.lease.Held()
	return b.countingBackup.Create(ctx, req)
}

type leaseObservingServices struct {
	*systemd.Fake
	lease         *testHubLease
	heldAtRestart bool
}

func (s *leaseObservingServices) Restart(ctx context.Context, unit string) error {
	if s.lease.Held() {
		s.heldAtRestart = true
	}
	return s.Fake.Restart(ctx, unit)
}

func TestUpgradeHoldsTheHubLeaseAcrossBackupAndMigration(t *testing.T) {
	h := newHarness(t)
	lease := &testHubLease{}
	backup := &leaseObservingBackup{countingBackup: h.backup, lease: lease}
	services := &leaseObservingServices{Fake: h.services, lease: lease}
	heldAtMigrate := false
	h.migrator.onMigrate = func() error {
		heldAtMigrate = lease.Held()
		return nil
	}
	if _, err := h.upgrade(func(opts *Options) {
		opts.HubLease = lease
		opts.Backup = backup
		opts.Services = services
	}, "1.0.0"); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !backup.atCreate {
		t.Fatal("the backup ran without the hub runtime lease")
	}
	if !heldAtMigrate {
		t.Fatal("the migration ran without the hub runtime lease")
	}
	if services.heldAtRestart {
		t.Fatal("a unit was restarted while the transaction still held the hub lease")
	}
	if lease.Held() {
		t.Fatal("the hub runtime lease outlived the transaction")
	}
	if lease.acquires != 1 || lease.releases != 1 {
		t.Fatalf("lease acquires/releases = %d/%d", lease.acquires, lease.releases)
	}
}

func TestUpgradeRefusesWhenTheHubLeaseIsBusy(t *testing.T) {
	h := newHarness(t)
	lease := &testHubLease{busy: true}
	backup := &leaseObservingBackup{countingBackup: h.backup, lease: lease}
	_, err := h.upgrade(func(opts *Options) {
		opts.HubLease = lease
		opts.Backup = backup
	}, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "the hub is running") {
		t.Fatalf("upgrade err = %v", err)
	}
	if backup.atCreate {
		t.Fatal("the backup ran while another process held the hub lease")
	}
	if got := h.content(h.binary); got != "old-binary" {
		t.Fatalf("binary = %q", got)
	}
	if got := h.content(h.hubDB); got != "database" {
		t.Fatalf("database = %q", got)
	}
}

type recordingOwnership struct {
	mu      sync.Mutex
	records map[string]ArtifactKind
	forgot  map[string]bool
}

func newRecordingOwnership() *recordingOwnership {
	return &recordingOwnership{records: map[string]ArtifactKind{}, forgot: map[string]bool{}}
}

func (o *recordingOwnership) Record(path string, kind ArtifactKind, _ string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.records[path] = kind
	return nil
}

func (o *recordingOwnership) Forget(path string, _ ArtifactKind) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.records, path)
	o.forgot[path] = true
	return nil
}

func (o *recordingOwnership) has(path string, kind ArtifactKind) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.records[path] == kind
}

func TestUpgradeRecordsEveryOwnedArtifact(t *testing.T) {
	h := newHarness(t)
	ownership := newRecordingOwnership()
	if _, err := h.upgrade(func(opts *Options) { opts.Ownership = ownership }, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	for path, kind := range map[string]ArtifactKind{
		h.journalPath:                     ArtifactUpgradeJournal,
		filepath.Join(h.state, "staging"): ArtifactStagingDir,
		filepath.Join(h.state, "backups"): ArtifactBackupsDir,
		h.prev:                            ArtifactPreviousBinary,
	} {
		if !ownership.has(path, kind) {
			t.Fatalf("%s was not recorded as %s: %v", path, kind, ownership.records)
		}
	}
}

func TestPreparedBinaryIsRecordedBeforeThePhaseBoundary(t *testing.T) {
	h := newHarness(t)
	ownership := newRecordingOwnership()
	_, err := h.upgrade(func(opts *Options) {
		opts.Ownership = ownership
		opts.AfterPhase = func(phase Phase) error {
			if phase == PhasePrepared {
				return errors.New("simulated process death")
			}
			return nil
		}
	}, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "simulated crash") {
		t.Fatalf("upgrade err = %v", err)
	}
	journal := h.journal()
	prepared := tempNameFor(h.binary, journal.ID, "new")
	if !ownership.has(prepared, ArtifactBinaryTemp) {
		t.Fatalf("prepared temp %s was not recorded before the phase boundary: %v", prepared, ownership.records)
	}
	if _, err := os.Stat(prepared); err != nil {
		t.Fatalf("prepared temp is missing: %v", err)
	}
}

func TestPreviousPublicationIsJournaledAndOwnedBeforeTheSwap(t *testing.T) {
	h := newHarness(t)
	ownership := newRecordingOwnership()
	_, err := h.upgrade(func(opts *Options) {
		opts.Ownership = ownership
		opts.AfterPhase = func(phase Phase) error {
			if phase == PhasePreviousPublished {
				return errors.New("simulated process death before the target swap")
			}
			return nil
		}
	}, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "simulated crash") {
		t.Fatalf("upgrade err = %v", err)
	}
	journal := h.journal()
	if journal.Phase != PhasePreviousPublished {
		t.Fatalf("journal phase after the crash = %s", journal.Phase)
	}
	if !journal.PreviousPublished {
		t.Fatal("the previous-publication step was not journaled")
	}
	// The crash landed before the target swap: the old release is installed.
	if got := h.content(h.binary); got != "old-binary" {
		t.Fatalf("binary = %q", got)
	}
	sum, err := fsutil.HashFile(h.prev)
	if err != nil {
		t.Fatal(err)
	}
	if sum != journal.PreviousBinarySHA256 {
		t.Fatalf(".prev = %s, journal records %s", sum, journal.PreviousBinarySHA256)
	}
	if !ownership.has(h.prev, ArtifactPreviousBinary) {
		t.Fatalf(".prev was not owned before the swap: %v", ownership.records)
	}
}

func TestRecoveryClaimsAPreviousBinaryPublishedBeforeThePhaseSave(t *testing.T) {
	h := newHarness(t)
	// Crash after preparation but before publication.
	_, err := h.upgrade(func(opts *Options) {
		opts.AfterPhase = func(phase Phase) error {
			if phase == PhasePrepared {
				return errors.New("simulated process death")
			}
			return nil
		}
	}, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "simulated crash") {
		t.Fatalf("upgrade err = %v", err)
	}
	journal := h.journal()
	// Simulate the narrower crash: .prev was durably published, but the process
	// died before the previous-publication step and its ownership were saved.
	prepared := fsutil.BinaryFile{Path: journal.PreparedBinary, SHA256: journal.NewBinarySHA256}
	if _, err := fsutil.PublishPrevious(prepared, h.binary, h.prev, journal.ID); err != nil {
		t.Fatalf("publish previous: %v", err)
	}
	journal.PublishStarted = true
	journal.PreviousPublished = false
	store := JournalStore{Path: h.journalPath, FS: OSFS{}}
	if err := store.Save(journal); err != nil {
		t.Fatal(err)
	}

	ownership := newRecordingOwnership()
	if _, err := h.build(func(opts *Options) { opts.Ownership = ownership }).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	sum, err := fsutil.HashFile(h.prev)
	if err != nil {
		t.Fatal(err)
	}
	if sum != journal.PreviousBinarySHA256 {
		t.Fatalf(".prev = %s, journal records %s", sum, journal.PreviousBinarySHA256)
	}
	if !ownership.has(h.prev, ArtifactPreviousBinary) {
		t.Fatalf("recovery did not claim the .prev it published: %v", ownership.records)
	}
	if got := h.content(h.binary); got != "old-binary" {
		t.Fatalf("binary = %q", got)
	}
	if recovered := h.journal(); recovered.Phase != PhaseRolledBack {
		t.Fatalf("phase = %s", recovered.Phase)
	}
}

func TestRollbackRestoreRecordsCollectorDirectory(t *testing.T) {
	h := newHarness(t)
	_, err := h.upgrade(func(opts *Options) {
		opts.AfterPhase = func(phase Phase) error {
			if phase == PhaseActivated {
				return errors.New("simulated process death before writes")
			}
			return nil
		}
	}, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "simulated crash") {
		t.Fatalf("upgrade err = %v", err)
	}
	ownership := newRecordingOwnership()
	if _, err := h.build(func(opts *Options) { opts.Ownership = ownership }).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !ownership.has(h.collect, ArtifactCollectorDir) {
		t.Fatalf("restored collector directory was not re-recorded: %v", ownership.records)
	}
	if got := h.content(h.binary); got != "old-binary" {
		t.Fatalf("binary = %q", got)
	}
}

func TestJournalFormatIsStable(t *testing.T) {
	h := newHarness(t)
	if _, err := h.upgrade(nil, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"format", "id", "phase", "fromVersion", "toVersion", "startedAt", "updatedAt", "backupDir", "acceptedWrites", "layout", "settings", "units", "newBinarySha256", "previousBinarySha256"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("journal is missing %q:\n%s", key, data)
		}
	}
	if decoded["format"].(float64) != 2 || decoded["phase"] != "done" {
		t.Fatalf("journal = %s", data)
	}
	layout := decoded["layout"].(map[string]any)
	if layout["binary"] != h.binary || layout["stateDir"] != h.state {
		t.Fatalf("journal layout = %v", layout)
	}
	settings := decoded["settings"].(map[string]any)
	if settings["hubPort"].(float64) != 8081 {
		t.Fatalf("journal settings = %v", settings)
	}
}

func TestNewValidatesRequiredPaths(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("empty options accepted")
	}
}
