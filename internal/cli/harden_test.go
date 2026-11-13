package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Triet1402/rigscope/internal/activation"
	"github.com/Triet1402/rigscope/internal/collector/runtime"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/fsutil"
	"github.com/Triet1402/rigscope/internal/platform/lock"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

func TestManifestFormatV2RecordsActivationArtifacts(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.FormatVersion != 2 {
		t.Fatalf("format version = %d", loaded.FormatVersion)
	}
	for _, entry := range []struct {
		path string
		kind string
	}{
		{paths.PreviousBinary, kindPreviousBinary},
		{paths.UpgradeJournalFile(), kindUpgradeJournal},
		{paths.StagingDir(), kindStagingDir},
		{paths.BackupsDir(), kindBackupsDir},
		{paths.BinaryDir, kindBinaryTemps},
	} {
		if !loaded.owns(entry.path, entry.kind) {
			t.Fatalf("%s (%s) is not recorded in the manifest", entry.path, entry.kind)
		}
	}
	// Activation writes the journal and staging content after install. The
	// state directory owns those paths, so uninstall removes them.
	if err := os.WriteFile(paths.UpgradeJournalFile(), []byte(`{"format":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.StagingDir(), "1.1.0"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	for _, path := range []string{paths.UpgradeJournalFile(), paths.StagingDir(), paths.BackupsDir()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived uninstall: %v", path, err)
		}
	}
}

func TestUninstallRemovesOnlyRecordedBinaryTransactionTemps(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	recorded := filepath.Join(paths.BinaryDir, ".rigscope.abc123.new")
	unrecorded := filepath.Join(paths.BinaryDir, "rigscope.prev.tmp")
	foreign := filepath.Join(paths.BinaryDir, "unrelated")
	for _, path := range []string{recorded, unrecorded, foreign} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Activation records the transaction temp it created. A reserved-looking
	// name that no manifest entry claims is foreign and must survive.
	current, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	current.Paths = append(current.Paths, manifestEntry{Path: recorded, Kind: kindBinaryTemp, State: manifestPlanned})
	if err := recordPathOwned(current, paths, &current.Paths[len(current.Paths)-1], recorded); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(recorded); !os.IsNotExist(err) {
		t.Fatalf("recorded transaction temp %s survived: %v", recorded, err)
	}
	for _, path := range []string{unrecorded, foreign} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unrecorded file %s was deleted: %v", path, err)
		}
	}
}

func TestInterruptedInstallDoesNotClaimForeignBinary(t *testing.T) {
	f := newFixture(t)
	paths := f.paths(t, config.ScopeUser)
	f.deps.InstallStep = func(step string) error {
		if step == installStepBinary {
			return errors.New("simulated interruption before binary creation")
		}
		return nil
	}
	if code := f.run(t, "install"); code != 1 {
		t.Fatalf("interrupted install exit = %d, want 1", code)
	}
	// The binary entry must be planned, not owned.
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	entry := loaded.entry(paths.Binary, kindBinary)
	if entry == nil || entry.State != manifestPlanned {
		t.Fatalf("binary entry = %+v, want planned", entry)
	}
	foreign := []byte("#!/bin/sh\necho foreign\n")
	if err := os.MkdirAll(paths.BinaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Binary, foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	// A retry must refuse to adopt or overwrite the foreign launcher.
	f.err.Reset()
	if code := f.run(t, "install"); code != 1 {
		t.Fatalf("retry install exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "not recorded in a Rigscope 1.x installation manifest") {
		t.Fatalf("retry stderr = %s", f.err.String())
	}
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	data, err := os.ReadFile(paths.Binary)
	if err != nil {
		t.Fatalf("foreign launcher was deleted: %v", err)
	}
	if string(data) != string(foreign) {
		t.Fatalf("foreign launcher changed: %q", data)
	}
}

func TestPlannedReservedPathNeverAuthorizesDeletion(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	// `.prev` is recorded as planned because install created no previous
	// binary. A foreign file that merely appears at that reserved name must
	// never be deleted.
	foreign := filepath.Join(paths.BinaryDir, "rigscope.prev")
	if err := os.WriteFile(foreign, []byte("foreign"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("uninstall deleted a foreign file at a planned reserved path: %v", err)
	}
}

func TestInterruptedPublicationCanBeAdoptedOnRetry(t *testing.T) {
	f := newFixture(t)
	paths := f.paths(t, config.ScopeUser)
	f.deps.PublishStep = func(path, phase string) error {
		if path == paths.ConfigFile && phase == "published" {
			return errors.New("simulated death after publication")
		}
		return nil
	}
	if code := f.run(t, "install"); code != 1 {
		t.Fatalf("interrupted install exit = %d, want 1", code)
	}
	// The file is durable but the entry was not promoted. A retry adopts it
	// only because its digest still matches the staged record.
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	entry := loaded.entry(paths.ConfigFile, kindConfig)
	if entry == nil || entry.StagedSHA256 == "" {
		t.Fatalf("staged publication was not recorded: %+v", entry)
	}
	f.deps.PublishStep = nil
	f.err.Reset()
	if code := f.run(t, "install"); code != 0 {
		t.Fatalf("retry install exit %d: %s", code, f.err.String())
	}
	if entry := f.manifest(t, paths).entry(paths.ConfigFile, kindConfig); entry.State != manifestOwned || entry.StagedSHA256 != "" {
		t.Fatalf("retry did not promote the published configuration: %+v", entry)
	}
}

func TestInterruptedPublicationNeverAdoptsAChangedFile(t *testing.T) {
	f := newFixture(t)
	paths := f.paths(t, config.ScopeUser)
	f.deps.PublishStep = func(path, phase string) error {
		if path == paths.ConfigFile && phase == "published" {
			return errors.New("simulated death after publication")
		}
		return nil
	}
	if code := f.run(t, "install"); code != 1 {
		t.Fatalf("interrupted install exit = %d, want 1", code)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`{"foreign":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f.deps.PublishStep = nil
	f.err.Reset()
	if code := f.run(t, "install"); code != 1 {
		t.Fatalf("retry install exit = %d, want 1", code)
	}
	data, err := os.ReadFile(paths.ConfigFile)
	if err != nil || string(data) != `{"foreign":true}` {
		t.Fatalf("foreign configuration was adopted: %q, %v", data, err)
	}
}

func TestInterruptedBeforePublicationRetries(t *testing.T) {
	f := newFixture(t)
	paths := f.paths(t, config.ScopeUser)
	f.deps.PublishStep = func(path, phase string) error {
		if path == paths.ConfigFile && phase == "staged" {
			return errors.New("simulated death before publication")
		}
		return nil
	}
	if code := f.run(t, "install"); code != 1 {
		t.Fatalf("interrupted install exit = %d, want 1", code)
	}
	if _, err := os.Stat(paths.ConfigFile); !os.IsNotExist(err) {
		t.Fatalf("configuration was published after a pre-publication crash: %v", err)
	}
	f.deps.PublishStep = nil
	f.err.Reset()
	if code := f.run(t, "install"); code != 0 {
		t.Fatalf("retry install exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.ConfigFile); err != nil {
		t.Fatalf("retry did not create the configuration: %v", err)
	}
}

func TestInterruptedBinaryPublicationRetries(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	// Force a binary replacement so install retains the old binary and
	// publishes the running one.
	if err := os.WriteFile(paths.Binary, []byte("stale-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.deps.PublishStep = func(path, phase string) error {
		if path == paths.Binary && phase == "published" {
			return errors.New("simulated death after binary publication")
		}
		return nil
	}
	if code := f.run(t, "install"); code != 1 {
		t.Fatalf("interrupted install exit = %d, want 1", code)
	}
	f.deps.PublishStep = nil
	f.err.Reset()
	if code := f.run(t, "install"); code != 0 {
		t.Fatalf("retry install exit %d: %s", code, f.err.String())
	}
	checkContent(t, paths.Binary, "binary-v1")
	checkContent(t, paths.PreviousBinary, "stale-binary")
}

func TestRuntimeCollectorFilesFollowDirectoryOwnership(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "collector", "--hub", "http://127.0.0.1:8081", "--token", "grant")
	for name, content := range map[string]string{
		"credentials.json": `{"version":1,"credentials":[]}`,
		"identity.json":    `{"version":1}`,
	} {
		if err := os.WriteFile(filepath.Join(paths.CollectorDir(), name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.err.Reset()
	if code := f.run(t, "install", "--role", "collector"); code != 0 {
		t.Fatalf("install after enrollment exit %d: %s", code, f.err.String())
	}
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.CollectorDir()); !os.IsNotExist(err) {
		t.Fatalf("collector directory survived uninstall: %v", err)
	}
}

func TestFailedUpgradeRestoreRefreshesCollectorOwnership(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	next := upgradeScript("1.1.0")
	release := newReleaseFixture(t, "1.1.0", next, false)
	f.useRelease(release)
	calls := 0
	f.deps.ProbeHealth = func(context.Context, string) error {
		calls++
		if calls <= 20 {
			return errors.New("hub health failed")
		}
		return nil
	}
	if code := f.run(t, "upgrade"); code != 1 {
		t.Fatalf("upgrade exit = %d, want 1", code)
	}
	// The rollback restored the collector directory with a new inode. Install
	// would refuse a stale identity, so the ownership refresh must have run.
	f.err.Reset()
	if code := f.run(t, "install"); code != 0 {
		t.Fatalf("install after rollback exit %d: %s", code, f.err.String())
	}
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.CollectorDir()); !os.IsNotExist(err) {
		t.Fatalf("collector directory survived uninstall: %v", err)
	}
}

func TestUpgradeUsesTheInstalledBinaryVersion(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	// The running process reports 1.0.0, but the installed binary is 0.9.0.
	f.deps.Version = "1.0.0"
	var probed string
	f.deps.BinaryVersion = func(path string) (string, error) {
		probed = path
		return "v0.9.0", nil
	}
	release := newReleaseFixture(t, "0.9.0", upgradeScript("0.9.0"), false)
	f.useRelease(release)
	f.out.Reset()
	if code := f.run(t, "upgrade"); code != 0 {
		t.Fatalf("upgrade exit %d: %s", code, f.err.String())
	}
	if probed != f.paths(t, config.ScopeUser).Binary {
		t.Fatalf("probed %q, want the installed binary", probed)
	}
	if !strings.Contains(f.out.String(), "Rigscope 0.9.0 is already installed") {
		t.Fatalf("output = %s", f.out.String())
	}
}

func TestUpgradeReexecsTheRestoredBinaryAfterRecovery(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	// Recovery rolled back to a binary that differs from the process running
	// this command. The pathname now names the restored file; only
	// /proc/self/exe (the running image) still identifies what is executing.
	if err := os.WriteFile(paths.Binary, []byte("restored-release"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.deps.Executable = func() (string, error) { return paths.Binary, nil }
	journal := activation.Journal{
		ID:             "restored1",
		Phase:          activation.PhaseRolledBack,
		From:           "0.9.0",
		To:             "1.1.0",
		PublishStarted: true,
		Layout: activation.Layout{
			Scope:             string(config.ScopeUser),
			StateDir:          paths.StateDir,
			Binary:            paths.Binary,
			PreviousBinary:    paths.PreviousBinary,
			ConfigFile:        paths.ConfigFile,
			CollectorStateDir: filepath.Join(paths.StateDir, "collector"),
			HubDBFile:         filepath.Join(paths.StateDir, "hub.db"),
			JournalPath:       filepath.Join(paths.StateDir, "upgrade-journal.json"),
		},
	}
	store := activation.JournalStore{Path: filepath.Join(paths.StateDir, "upgrade-journal.json"), FS: activation.OSFS{}}
	if err := store.Save(&journal); err != nil {
		t.Fatal(err)
	}
	f.deps.BinaryVersion = func(string) (string, error) { return "0.9.0", nil }
	var executedPath string
	var executedArgs []string
	f.deps.ExecInstalled = func(path string, args []string) error {
		executedPath = path
		executedArgs = append([]string(nil), args...)
		return nil
	}
	f.err.Reset()
	if code := f.run(t, "upgrade"); code != 0 {
		t.Fatalf("upgrade exit %d: %s", code, f.err.String())
	}
	if executedPath != paths.Binary {
		t.Fatalf("executed %q, want the restored binary", executedPath)
	}
	if len(executedArgs) == 0 || executedArgs[0] != "upgrade" {
		t.Fatalf("args = %v", executedArgs)
	}
}

func TestReexecUsesRunningImageAndASingleGuardVariable(t *testing.T) {
	root := t.TempDir()
	running := filepath.Join(root, "running-image")
	installed := filepath.Join(root, "rigscope")
	if err := os.WriteFile(running, []byte("running-image"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("restored-release"), 0o755); err != nil {
		t.Fatal(err)
	}
	runningSum, err := fsutil.HashFile(running)
	if err != nil {
		t.Fatal(err)
	}
	paths := config.Paths{Binary: installed}
	deps := Deps{
		Getenv:            func(string) string { return "" },
		RunningExecutable: func() (string, error) { return running, nil },
	}
	var (
		executedPath string
		executedArgs []string
		executedEnv  []string
	)
	deps.ExecInstalledEnv = func(path string, args, env []string) error {
		executedPath = path
		executedArgs = append([]string(nil), args...)
		executedEnv = append([]string(nil), env...)
		return nil
	}
	executed, code := reexecRestoredBinary(deps, paths, activation.Result{RolledBack: true}, []string{"--version", "1.2.0"}, runningSum)
	if !executed || code != 0 {
		t.Fatalf("executed = %v, code = %d", executed, code)
	}
	if executedPath != installed {
		t.Fatalf("executed %q, want %q", executedPath, installed)
	}
	if strings.Join(executedArgs, " ") != "upgrade --version 1.2.0" {
		t.Fatalf("args = %v", executedArgs)
	}
	if len(executedEnv) != len(os.Environ())+1 || executedEnv[len(executedEnv)-1] != reexecGuardEnv+"=1" {
		t.Fatalf("environment carried more than the loop guard: %v", executedEnv)
	}

	// The guard prevents a second replacement if the installed file still
	// cannot be identified as the running image.
	deps.Getenv = func(key string) string {
		if key == reexecGuardEnv {
			return "1"
		}
		return ""
	}
	executed, _ = reexecRestoredBinary(deps, paths, activation.Result{RolledBack: true}, nil, runningSum)
	if executed {
		t.Fatal("guarded upgrade re-executed the installed binary again")
	}
}

func TestMigrateInheritedLockIsVerifiedAndRootIsChecked(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	f.deps.Getenv = func(key string) string {
		if key == lock.InheritedLockFDEnv {
			return "not-a-descriptor"
		}
		return ""
	}
	if code := f.run(t, "migrate", "--config", paths.ConfigFile); code != 1 {
		t.Fatalf("migrate exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "invalid inherited installation lock descriptor") {
		t.Fatalf("stderr = %s", f.err.String())
	}

	// A real inherited descriptor is accepted, and releasing the child's copy
	// must not release the parent's lock.
	f = newFixture(t)
	paths = f.install(t)
	parent, err := lock.Acquire(paths.LockFile())
	if err != nil {
		t.Fatal(err)
	}
	dupFD, err := syscall.Dup(int(parent.File().Fd()))
	if err != nil {
		t.Fatal(err)
	}
	f.deps.Getenv = func(key string) string {
		if key == lock.InheritedLockFDEnv {
			return strconv.Itoa(dupFD)
		}
		if key == "XDG_RUNTIME_DIR" {
			return f.runtimeDir
		}
		return ""
	}
	f.out.Reset()
	if code := f.run(t, "migrate", "--config", paths.ConfigFile); code != 0 {
		t.Fatalf("migrate with an inherited lock exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.LockFile()); err != nil {
		t.Fatal(err)
	}
	// The parent's lock is still held.
	other, err := lock.Acquire(paths.LockFile())
	if !errors.Is(err, lock.ErrBusy) {
		if other != nil {
			other.Release()
		}
		t.Fatalf("the child released the parent's lock: %v", err)
	}
	if err := parent.Release(); err != nil {
		t.Fatal(err)
	}

	// Root authorization is enforced independently of the inherited lock.
	f = newFixture(t)
	paths = f.install(t)
	f.deps.Getenv = func(key string) string {
		if key == lock.InheritedLockFDEnv {
			return "3"
		}
		return ""
	}
	f.err.Reset()
	if code := f.run(t, "migrate", "--system", "--config", paths.ConfigFile); code != 1 {
		t.Fatalf("migrate --system exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "requires root") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestInstallationLockerIsOwnerScoped(t *testing.T) {
	f := newFixture(t)
	paths := f.paths(t, config.ScopeUser)
	locker := newInstallationLocker(paths)
	unlock, err := locker.Lock(context.Background(), "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	reentrant, err := locker.Lock(context.Background(), "owner-a")
	if err != nil {
		t.Fatalf("same owner could not re-enter: %v", err)
	}
	reentrant()
	if _, err := locker.Lock(context.Background(), "owner-b"); !errors.Is(err, activation.ErrLocked) {
		t.Fatalf("a different owner borrowed the lock: %v", err)
	}
}

func TestSuccessfulUpgradeRecordsPreviousBinaryOwnership(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	next := upgradeScript("1.1.0")
	release := newReleaseFixture(t, "1.1.0", next, false)
	f.useRelease(release)
	if code := f.run(t, "upgrade"); code != 0 {
		t.Fatalf("upgrade exit %d: %s", code, f.err.String())
	}
	entry := f.manifest(t, paths).entry(paths.PreviousBinary, kindPreviousBinary)
	if entry == nil || entry.State != manifestOwned || entry.SHA256 == "" {
		t.Fatalf("previous binary ownership = %+v", entry)
	}
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.PreviousBinary); !os.IsNotExist(err) {
		t.Fatalf("owned previous binary survived uninstall: %v", err)
	}
}

func (f *fixture) manifest(t *testing.T, paths config.Paths) *manifest {
	t.Helper()
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestUninstallAbortsWhenSystemdCannotBeReached(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	f.fake.Fail["unit-known "+systemd.Hub.UnitName()] = errors.New("Failed to connect to bus: No such file or directory")
	f.err.Reset()
	if code := f.run(t, "uninstall"); code != 1 {
		t.Fatalf("uninstall exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "Failed to connect to bus") || !strings.Contains(f.err.String(), "nothing was deleted") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	for _, path := range []string{paths.Binary, paths.ConfigFile, paths.StateDir} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("uninstall deleted %s after a systemd communication failure: %v", path, err)
		}
	}
}

func TestUninstallRefusesSymlinkedParentBeforeStop(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	foreignDir := t.TempDir()
	foreignConfig := filepath.Join(foreignDir, "config.json")
	if err := os.WriteFile(foreignConfig, []byte(`{"foreign":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	realDir := paths.ConfigDir + ".real"
	if err := os.Rename(paths.ConfigDir, realDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreignDir, paths.ConfigDir); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "uninstall"); code != 1 {
		t.Fatalf("uninstall exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "nothing was deleted") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	if _, err := os.Stat(foreignConfig); err != nil {
		t.Fatalf("foreign config through symlinked parent was deleted: %v", err)
	}
	if _, err := os.Stat(paths.Binary); err != nil {
		t.Fatalf("uninstall deleted the binary despite refusing: %v", err)
	}
	if _, err := os.Stat(paths.UnitPath(systemd.Hub.UnitName())); err != nil {
		t.Fatalf("uninstall deleted a unit despite refusing: %v", err)
	}
}

func TestUninstallRefusesParentSwappedDuringStop(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	foreignDir := t.TempDir()
	foreignUnit := filepath.Join(foreignDir, systemd.Hub.UnitName())
	if err := os.WriteFile(foreignUnit, []byte("# Managed by rigscope. Do not edit\n[Unit]\nDescription=foreign\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unitDir := paths.UnitDir
	realDir := unitDir + ".real"
	f.deps.AfterStop = func() error {
		if err := os.Rename(unitDir, realDir); err != nil {
			return err
		}
		return os.Symlink(foreignDir, unitDir)
	}
	if code := f.run(t, "uninstall"); code != 1 {
		t.Fatalf("uninstall exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "nothing was deleted") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	if _, err := os.Stat(foreignUnit); err != nil {
		t.Fatalf("foreign unit through symlinked parent was deleted: %v", err)
	}
	if _, err := os.Stat(paths.Binary); err != nil {
		t.Fatalf("uninstall deleted the binary despite refusing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(realDir, systemd.Hub.UnitName())); err != nil {
		t.Fatalf("owned unit was deleted despite refusing: %v", err)
	}
}

func TestUninstallStopsRecordedUnitWhoseFileWasDeleted(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	if err := os.Remove(paths.UnitPath(systemd.Hub.UnitName())); err != nil {
		t.Fatal(err)
	}
	f.fake.Reset()
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if !f.fake.HasCall("stop", systemd.Hub.UnitName()) {
		t.Fatalf("deleted unit file bypassed the stop: %v", f.fake.Methods())
	}
	if !f.fake.HasCall("disable", systemd.Hub.UnitName()) {
		t.Fatalf("deleted unit file bypassed disable: %v", f.fake.Methods())
	}
}

func TestUninstallSkipsUnitTheManagerDoesNotKnow(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	if err := os.Remove(paths.UnitPath(systemd.Hub.UnitName())); err != nil {
		t.Fatal(err)
	}
	f.fake.Known[systemd.Hub.UnitName()] = false
	f.fake.Active[systemd.Hub.UnitName()] = false
	f.fake.Reset()
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if f.fake.HasCall("stop", systemd.Hub.UnitName()) {
		t.Fatalf("unknown unit was stopped: %v", f.fake.Methods())
	}
	if !f.fake.HasCall("stop", systemd.Collector.UnitName()) {
		t.Fatalf("known collector unit was not stopped: %v", f.fake.Methods())
	}
}

func TestSystemInstallRunsSysusersBeforeAnyChown(t *testing.T) {
	f := newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Euid = func() int { return 0 }
	var events []string
	f.deps.RunCommand = func(_ context.Context, name string, args ...string) error {
		events = append(events, "command:"+name+" "+strings.Join(args, " "))
		return nil
	}
	f.deps.Chown = func(path, userName, groupName string) error {
		events = append(events, "chown:"+path)
		return nil
	}
	if code := f.run(t, "install", "--system", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	sysusersIndex, chownIndex := -1, -1
	for i, event := range events {
		if strings.HasPrefix(event, "command:systemd-sysusers") && sysusersIndex == -1 {
			sysusersIndex = i
		}
		if strings.HasPrefix(event, "chown:") && chownIndex == -1 {
			chownIndex = i
		}
	}
	if sysusersIndex == -1 {
		t.Fatalf("systemd-sysusers was not run: %v", events)
	}
	if chownIndex != -1 && chownIndex < sysusersIndex {
		t.Fatalf("a chown ran before systemd-sysusers: %v", events)
	}
}

func TestCollectorLeaseMatchesTheLayoutForBothScopes(t *testing.T) {
	for _, scope := range []config.Scope{config.ScopeUser, config.ScopeSystem} {
		scope := scope
		t.Run(string(scope), func(t *testing.T) {
			f := newFixture(t)
			if scope == config.ScopeSystem {
				f.deps.Root = t.TempDir()
				f.deps.Euid = func() int { return 0 }
				f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
				if code := f.run(t, "install", "--system", "--no-start"); code != 0 {
					t.Fatalf("install exit %d: %s", code, f.err.String())
				}
			} else {
				f.install(t)
			}
			paths := f.paths(t, scope)
			if got, want := runtime.LeasePath(paths.StateDir), paths.CollectorRuntimeLock(); got != want {
				t.Fatalf("runtime lease %s does not match the layout lock %s", got, want)
			}
			info, err := os.Stat(paths.CollectorDir())
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o700 {
				t.Fatalf("collector directory mode = %04o, want 0700", info.Mode().Perm())
			}
			if scope == config.ScopeSystem {
				root, err := os.Stat(paths.StateDir)
				if err != nil {
					t.Fatal(err)
				}
				if root.Mode().Perm() != 0o755 {
					t.Fatalf("system state root mode = %04o, want 0755", root.Mode().Perm())
				}
				unit, err := os.ReadFile(paths.UnitPath(systemd.Collector.UnitName()))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(unit), `ReadWritePaths="`+paths.ComponentStateDir("collector")+`"`) {
					t.Fatalf("collector unit does not grant its own state directory:\n%s", unit)
				}
			}
		})
	}
}

func TestSystemUninstallReloadsUdevAndReportsFailure(t *testing.T) {
	f := newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Euid = func() int { return 0 }
	var commands []string
	f.deps.RunCommand = func(_ context.Context, name string, args ...string) error {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil
	}
	if code := f.run(t, "install", "--system", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	commands = nil
	if code := f.run(t, "uninstall", "--system"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if !strings.Contains(strings.Join(commands, "\n"), "udevadm control --reload") {
		t.Fatalf("udev was not reloaded after removing rules: %v", commands)
	}

	f = newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Euid = func() int { return 0 }
	f.deps.RunCommand = func(_ context.Context, name string, args ...string) error {
		if name == "udevadm" && strings.Join(args, " ") == "control --reload" {
			return errors.New("udev daemon unavailable")
		}
		return nil
	}
	if code := f.run(t, "install", "--system", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	f.err.Reset()
	if code := f.run(t, "uninstall", "--system"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if !strings.Contains(f.err.String(), "could not reload udev rules") {
		t.Fatalf("udev reload failure was not reported: %s", f.err.String())
	}
}

func TestFreshSystemInstallRetriesAfterInterruptionAtEveryStep(t *testing.T) {
	for _, interrupted := range installStepNames {
		interrupted := interrupted
		t.Run(interrupted, func(t *testing.T) {
			f := newFixture(t)
			f.deps.Root = t.TempDir()
			f.deps.Euid = func() int { return 0 }
			f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
			f.deps.InstallStep = func(step string) error {
				if step == interrupted {
					return fmt.Errorf("simulated failure before %s", step)
				}
				return nil
			}
			args := []string{"install", "--system", "--role", "both", "--hub", "https://hub.example.com", "--token", "grant", "--no-start"}
			if code := f.run(t, args...); code != 1 {
				t.Fatalf("interrupted install exit = %d, want 1: %s", code, f.err.String())
			}
			f.deps.InstallStep = nil
			f.out.Reset()
			f.err.Reset()
			if code := f.run(t, args...); code != 0 {
				t.Fatalf("retry after %s failed: exit %d: %s", interrupted, code, f.err.String())
			}
			paths := f.paths(t, config.ScopeSystem)
			if _, err := os.Stat(paths.ConfigFile); err != nil {
				t.Fatalf("retry did not create the configuration: %v", err)
			}
			if _, err := os.Stat(paths.Binary); err != nil {
				t.Fatalf("retry did not install the binary: %v", err)
			}
			f.out.Reset()
			f.err.Reset()
			if code := f.run(t, "uninstall", "--system"); code != 0 {
				t.Fatalf("uninstall after retry failed: exit %d: %s", code, f.err.String())
			}
		})
	}
}

func TestLockFileLivesOutsideStateDirectory(t *testing.T) {
	f := newFixture(t)
	paths := f.paths(t, config.ScopeUser)
	if paths.LockFile() == filepath.Join(paths.StateDir, ".install.lock") {
		t.Fatalf("lock file still lives in the deleted state directory: %s", paths.LockFile())
	}
	if !strings.HasPrefix(paths.LockFile(), f.runtimeDir) {
		t.Fatalf("lock file = %s, want below %s", paths.LockFile(), f.runtimeDir)
	}
	if code := f.run(t, "install"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir, ".install.lock")); !os.IsNotExist(err) {
		t.Fatalf("install created a lock inside the state directory: %v", err)
	}
	if _, err := os.Stat(paths.LockFile()); err != nil {
		t.Fatalf("lock file was not created: %v", err)
	}
}

func TestInstallRejectsOriginWithoutHostname(t *testing.T) {
	f := newFixture(t)
	if code := f.run(t, "install", "--origin", "https://:443"); code != 1 {
		t.Fatalf("install exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "no hostname") && !strings.Contains(f.err.String(), "has no host") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	if _, err := os.Stat(f.paths(t, config.ScopeUser).ConfigFile); !os.IsNotExist(err) {
		t.Fatal("install wrote a configuration with an invalid origin")
	}
}

func TestFirstUpgradeCrashBeforeTheSwapRecoversAndUninstallsCleanly(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	next := upgradeScript("1.1.0")
	// The installed release was published to .prev, then the first upgrade
	// died before swapping the target and before the previous-publication step
	// was journaled. This is the narrow window that used to leave an unowned
	// .prev behind.
	if err := os.WriteFile(paths.PreviousBinary, []byte("binary-v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	previousSum, err := fsutil.SHA256File(paths.PreviousBinary)
	if err != nil {
		t.Fatal(err)
	}
	journal := activation.Journal{
		ID:    "crash-prev-1",
		Phase: activation.PhasePrepared,
		From:  "1.0.0",
		To:    "1.1.0",
		Layout: activation.Layout{
			Scope:             string(config.ScopeUser),
			StateDir:          paths.StateDir,
			Binary:            paths.Binary,
			PreviousBinary:    paths.PreviousBinary,
			ConfigFile:        paths.ConfigFile,
			CollectorStateDir: filepath.Join(paths.StateDir, "collector"),
			HubDBFile:         filepath.Join(paths.StateDir, "hub.db"),
			JournalPath:       filepath.Join(paths.StateDir, "upgrade-journal.json"),
		},
		Settings:             activation.Settings{},
		ServicesStopped:      true,
		PublishStarted:       true,
		PreviousPublished:    false,
		PreviousBinaryPath:   paths.PreviousBinary,
		PreviousBinarySHA256: previousSum,
		NewBinarySHA256:      fsutil.SHA256Bytes(next),
		PreparedBinary:       filepath.Join(paths.BinaryDir, ".rigscope.crash-prev-1.new"),
	}
	store := activation.JournalStore{Path: filepath.Join(paths.StateDir, "upgrade-journal.json"), FS: activation.OSFS{}}
	if err := store.Save(&journal); err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if entry := manifestBefore.entry(paths.PreviousBinary, kindPreviousBinary); entry == nil || entry.State == manifestOwned {
		t.Fatalf(".prev entry before recovery = %+v, want planned", entry)
	}

	// `upgrade` repairs the interrupted transaction before anything else. The
	// release is the installed 1.0.0, so the command then reports it is current.
	release := newReleaseFixture(t, "1.0.0", []byte("binary-v1"), false)
	f.useRelease(release)
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "upgrade"); code != 0 {
		t.Fatalf("recovery upgrade exit %d: %s", code, f.err.String())
	}
	recovered, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Phase != activation.PhaseRolledBack {
		t.Fatalf("journal phase = %s, want rolled-back", recovered.Phase)
	}
	manifestAfter, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if entry := manifestAfter.entry(paths.PreviousBinary, kindPreviousBinary); entry == nil || entry.State != manifestOwned {
		t.Fatalf("recovery did not claim the .prev it published: %+v", entry)
	}

	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	for _, path := range []string{paths.Binary, paths.PreviousBinary, paths.ManifestFile(), paths.StateDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived uninstall: %v", path, err)
		}
	}
	remaining, err := os.ReadDir(paths.BinaryDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, entry := range remaining {
		if strings.HasPrefix(entry.Name(), "rigscope") {
			t.Fatalf("uninstall left %s behind", filepath.Join(paths.BinaryDir, entry.Name()))
		}
	}
}

func TestUninstallAfterInterruptionCleansOwnedState(t *testing.T) {
	f := newFixture(t)
	f.deps.InstallStep = func(step string) error {
		if step == installStepDirs {
			return errors.New("simulated interruption after the manifest")
		}
		return nil
	}
	if code := f.run(t, "install"); code != 1 {
		t.Fatalf("interrupted install exit = %d, want 1", code)
	}
	paths := f.paths(t, config.ScopeUser)
	f.deps.InstallStep = nil
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.StateDir); !os.IsNotExist(err) {
		t.Fatalf("owned state directory survived uninstall: %v", err)
	}
}
