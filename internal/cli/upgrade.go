package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/activation"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/fsutil"
	"github.com/Triet1402/rigscope/internal/platform/lock"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
	"github.com/Triet1402/rigscope/internal/release"
)

var (
	_ activation.ReleaseFetcher = (*release.Fetcher)(nil)
	_ activation.Services       = (*systemd.Exec)(nil)
)

// runUpgrade starts the activation transaction from internal/activation.
func runUpgrade(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("upgrade", deps.Stderr)
	requested := flags.String("version", "", "release version to install (default: latest)")
	system := flags.Bool("system", false, "upgrade the root-owned system installation")
	yes := addYesFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		return deps.errorf("upgrade takes no positional arguments, got %q", flags.Arg(0))
	}
	scope := deps.installedScope(*system)
	if code, done := deps.ensureRoot(scope); done {
		return code
	}
	paths, err := deps.paths(scope)
	if err != nil {
		return deps.errorf("%v", err)
	}
	owner := activation.NewOwnerToken()
	locker := newInstallationLocker(paths)
	unlock, err := locker.Lock(ctx, owner)
	if err != nil {
		return deps.errorf("%v", err)
	}
	defer unlock()

	// Hash the running image before recovery can replace the file at the
	// installed path. /proc/self/exe refers to the executing inode even after
	// that path now names a different binary.
	runningSum, _ := hashRunningExecutable(deps)
	// Repair an interrupted transaction before touching configuration: the
	// journal carries every path and readiness setting recovery needs, so a
	// migrated or invalid config file cannot block it.
	recovery, err := newCoordinator(deps, scope, paths, nil, nil, locker, owner)
	if err != nil {
		return deps.errorf("%v", err)
	}
	recovered, err := recovery.RecoverPending(ctx)
	if err != nil {
		return deps.errorf("%v", err)
	}
	// After a rollback the process that is running may no longer be the
	// installed release. Continue in the restored binary instead of measuring
	// this process's version.
	executed, code := reexecRestoredBinary(deps, paths, recovered, args, runningSum)
	if executed {
		return code
	}
	installedVersion, err := resolveInstalledVersion(deps, paths, recovered)
	if err != nil {
		return deps.errorf("%v", err)
	}

	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return deps.errorf("%v; run rigscope install first", err)
	}
	if _, err := assetArch(deps.architecture()); err != nil {
		return deps.errorf("%v", err)
	}

	fetcher, err := release.NewFetcher(release.Options{
		APIBase:           deps.apiBase(),
		Client:            deps.httpClient(),
		GOARCH:            assetArchValue(deps.architecture()),
		StateRoot:         paths.StateDir,
		UserAgent:         "rigscope/" + deps.buildVersion(),
		AllowInsecureHTTP: deps.AllowInsecureHTTP,
	})
	if err != nil {
		return deps.errorf("%v", err)
	}
	coordinator, err := newCoordinator(deps, scope, paths, cfg, fetcher, locker, owner, transactionApproval(deps, *yes))
	if err != nil {
		return deps.errorf("%v", err)
	}

	fmt.Fprintf(deps.Stdout, "%s %s\n", deps.out(styleBold, "Upgrading Rigscope"), installedVersion)
	result, err := coordinator.Upgrade(ctx, activation.UpgradeRequest{
		Version:        *requested,
		CurrentVersion: installedVersion,
	})
	if errors.Is(err, errCancelled) {
		return deps.errorf("upgrade: %v", err)
	}
	if errors.Is(err, activation.ErrAlreadyCurrent) {
		fmt.Fprintf(deps.Stdout, "Rigscope %s is already installed\n", result.To)
		return 0
	}
	if err != nil {
		return deps.errorf("%v", err)
	}
	fmt.Fprintf(deps.Stdout, "%s %s\n", deps.out(styleGreen+";"+styleBold, "Upgraded to Rigscope"), result.To)
	fmt.Fprintf(deps.Stdout, "  previous: %s\n", paths.PreviousBinary)
	if result.BackupDir != "" {
		fmt.Fprintf(deps.Stdout, "  backup:   %s\n", result.BackupDir)
	}
	return 0
}

func runRollback(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("rollback", deps.Stderr)
	system := flags.Bool("system", false, "roll back the root-owned system installation")
	yes := addYesFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		return deps.errorf("rollback takes no positional arguments, got %q", flags.Arg(0))
	}
	scope := deps.installedScope(*system)
	if code, done := deps.ensureRoot(scope); done {
		return code
	}
	paths, err := deps.paths(scope)
	if err != nil {
		return deps.errorf("%v", err)
	}
	owner := activation.NewOwnerToken()
	locker := newInstallationLocker(paths)
	unlock, err := locker.Lock(ctx, owner)
	if err != nil {
		return deps.errorf("%v", err)
	}
	defer unlock()

	recovery, err := newCoordinator(deps, scope, paths, nil, nil, locker, owner)
	if err != nil {
		return deps.errorf("%v", err)
	}
	if _, err := recovery.RecoverPending(ctx); err != nil {
		return deps.errorf("%v", err)
	}

	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		// A rollback can proceed from the journal alone; only refuse when there
		// is no journal to supply the layout and readiness settings.
		if _, statErr := os.Stat(journalPath(paths)); statErr != nil {
			return deps.errorf("%v", err)
		}
		cfg = nil
	}
	coordinator, err := newCoordinator(deps, scope, paths, cfg, nil, locker, owner, transactionApproval(deps, *yes))
	if err != nil {
		return deps.errorf("%v", err)
	}
	result, err := coordinator.Rollback(ctx)
	if err != nil {
		return deps.errorf("%v", err)
	}
	fmt.Fprintf(deps.Stdout, "%s %s\n", deps.out(styleGreen+";"+styleBold, "Rolled back to Rigscope"), result.From)
	fmt.Fprintf(deps.Stdout, "  previous: %s\n", paths.PreviousBinary)
	return 0
}

// newCoordinator wires the activation transaction to the CLI's injectable
// environment. A nil fetcher is valid for rollback. A nil config is valid when
// the coordinator is only used to recover a journal before configuration is
// loaded; recovery reads its settings from the journal.
func newCoordinator(deps Deps, scope config.Scope, paths config.Paths, cfg *config.Config, fetcher activation.ReleaseFetcher, locker activation.Locker, owner string, confirm ...func(context.Context, string, string, []string) error) (*activation.Coordinator, error) {
	var confirmStop func(context.Context, string, string, []string) error
	if len(confirm) > 0 {
		confirmStop = confirm[0]
	}
	client := deps.httpClient()
	var settings activation.SettingsSource
	if cfg != nil {
		settings = configSettings{value: settingsFromConfig(cfg)}
	} else {
		settings = journalSettings{path: journalPath(paths)}
	}
	var collector activation.CollectorCheck
	if deps.CollectorReady != nil {
		collector = readinessFunc(deps.CollectorReady)
	}
	readiness := activation.NewComponentReadiness(activation.ReadinessOptions{
		Services:   deps.manager(scope),
		Settings:   settings,
		Binary:     paths.Binary,
		ConfigFile: paths.ConfigFile,
		StateDir:   paths.StateDir,
		Scope:      string(scope),
		Client:     client,
		Sleep:      deps.sleep,
		Log:        deps.Stdout,
		Collector:  collector,
		Probe: func(ctx context.Context, target, host, serverName, expected string) error {
			if deps.ProbeHealth != nil {
				return deps.ProbeHealth(ctx, target)
			}
			return activation.ProbeHubHealth(ctx, client, target, host, serverName, expected)
		},
	})
	return activation.New(activation.Options{
		Layout: activation.Layout{
			Scope:             string(scope),
			StateDir:          paths.StateDir,
			Binary:            paths.Binary,
			PreviousBinary:    paths.PreviousBinary,
			ConfigFile:        paths.ConfigFile,
			CollectorStateDir: filepath.Join(paths.StateDir, "collector"),
			HubDBFile:         filepath.Join(paths.ComponentStateDir(systemd.Hub.String()), "hub.db"),
		},
		Services:    deps.manager(scope),
		Releases:    fetcher,
		Backup:      activation.NewFileBackup(nil),
		Migrator:    activation.NewExecMigrator(),
		Readiness:   readiness,
		Settings:    settings,
		Lock:        locker,
		HubLease:    newHubRuntimeLease(paths.HubRuntimeLock(), hubLeasePrepare(deps, scope)),
		Owner:       owner,
		Ownership:   manifestOwnership{paths: paths},
		Log:         deps.Stdout,
		ConfirmStop: confirmStop,
	})
}

// transactionApproval asks before an upgrade or rollback stops the running
// services. They start again once the transaction's health checks run.
func transactionApproval(deps Deps, yes bool) func(context.Context, string, string, []string) error {
	return func(_ context.Context, action, version string, units []string) error {
		var running []systemd.Component
		for _, component := range installComponents {
			for _, unit := range units {
				if unit == component.UnitName() {
					running = append(running, component)
				}
			}
		}
		target := strings.TrimPrefix(version, "v")
		effect := fmt.Sprintf("stop for the %s to %s and start again once it is verified", action, target)
		if action == "rollback" {
			effect = fmt.Sprintf("stop while Rigscope rolls back to %s and start again once it is verified", target)
		}
		return deps.approveServiceStop(running, effect, yes)
	}
}

type readinessFunc func(context.Context) error

func (f readinessFunc) Ready(ctx context.Context) error { return f(ctx) }

type configSettings struct{ value activation.Settings }

func (s configSettings) Settings() (activation.Settings, error) { return s.value, nil }

// journalSettings supplies readiness settings from the pending journal during
// pre-config recovery, so an invalid or migrated config cannot block it.
type journalSettings struct{ path string }

func (s journalSettings) Settings() (activation.Settings, error) {
	journal, err := (activation.JournalStore{Path: s.path, FS: activation.OSFS{}}).Load()
	if err != nil {
		return activation.Settings{}, err
	}
	return journal.Settings, nil
}

func journalPath(paths config.Paths) string {
	return filepath.Join(paths.StateDir, "upgrade-journal.json")
}

// installationLocker adapts the shared installation flock used by install and
// uninstall to the activation transaction. Re-entry is scoped to one owner
// token, so an unrelated caller that happens to share the object cannot borrow
// a lock held by the operation that owns it.
type installationLocker struct {
	paths config.Paths
	mu    sync.Mutex
	held  *lock.Lock
	owner string
	depth int
}

func newInstallationLocker(paths config.Paths) *installationLocker {
	return &installationLocker{paths: paths}
}

func (l *installationLocker) Lock(ctx context.Context, owner string) (func(), error) {
	if owner == "" {
		return nil, errors.New("installation lock owner is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.depth > 0 {
		if l.owner != owner {
			return nil, fmt.Errorf("%w: another operation in this process owns the lock", activation.ErrLocked)
		}
		l.depth++
		return l.unlock, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	held, err := acquireLock(l.paths)
	if err != nil {
		if errors.Is(err, lock.ErrBusy) {
			return nil, fmt.Errorf("%w: %v", activation.ErrLocked, err)
		}
		return nil, err
	}
	l.held = held
	l.owner = owner
	l.depth = 1
	return l.unlock, nil
}

// File exposes the held lock descriptor to the staged migration child.
func (l *installationLocker) File() *os.File {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		return nil
	}
	return l.held.File()
}

func (l *installationLocker) unlock() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.depth == 0 {
		return
	}
	l.depth--
	if l.depth > 0 {
		return
	}
	if l.held != nil {
		_ = l.held.Release()
		l.held = nil
	}
	l.owner = ""
}

// installedVersionTimeout bounds the installed binary's "version" probe.
const installedVersionTimeout = 10 * time.Second

// resolveInstalledVersion asks the installed binary what version it is. The
// running process may be an older or newer file than paths.Binary, especially
// after recovery, so its own build version is not authoritative. The journal's
// recorded fromVersion is the fallback when the binary cannot be executed.
func resolveInstalledVersion(deps Deps, paths config.Paths, recovery activation.Result) (string, error) {
	probe := deps.BinaryVersion
	if probe != nil {
		type outcome struct {
			version string
			err     error
		}
		result := make(chan outcome, 1)
		go func() {
			version, err := probe(paths.Binary)
			result <- outcome{version: version, err: err}
		}()
		select {
		case got := <-result:
			if got.err == nil && strings.TrimSpace(got.version) != "" {
				return strings.TrimPrefix(strings.TrimSpace(got.version), "v"), nil
			}
		case <-time.After(installedVersionTimeout):
		}
	}
	if recovery.From != "" {
		return strings.TrimPrefix(strings.TrimSpace(recovery.From), "v"), nil
	}
	return "", fmt.Errorf("could not determine the installed version of %s; run rigscope install first", paths.Binary)
}

// reexecGuardEnv prevents an endless re-exec loop if the restored binary still
// cannot be identified as the running image.
const reexecGuardEnv = "RIGSCOPE_UPGRADE_REEXEC"

// hashRunningExecutable hashes the running process image. The path is normally
// /proc/self/exe, whose open file still refers to the executing inode after a
// binary swap has replaced the installed path.
func hashRunningExecutable(deps Deps) (string, error) {
	path, err := deps.runningExecutable()
	if err != nil {
		return "", err
	}
	return fsutil.HashFile(path)
}

// reexecRestoredBinary replaces this process with the restored installed
// binary when recovery rolled back to a different file than the one running.
// runningSum is the digest of the running image captured before recovery. It
// returns whether it handled the request and the exit code to use.
func reexecRestoredBinary(deps Deps, paths config.Paths, recovery activation.Result, args []string, runningSum string) (bool, int) {
	if !recovery.RolledBack || runningSum == "" {
		return false, 0
	}
	installedSum, err := fsutil.HashFile(paths.Binary)
	if err != nil || installedSum == "" || installedSum == runningSum {
		return false, 0
	}
	if deps.env(reexecGuardEnv) == "1" {
		// The installed binary is already executing; do not replace it again.
		return false, 0
	}
	next := append([]string{"upgrade"}, args...)
	// Only the loop guard changes; everything else is the inherited
	// environment, never values parsed from this invocation. Drop any
	// pre-existing guard so the child sees exactly one value.
	guard := reexecGuardEnv + "=1"
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, reexecGuardEnv+"=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, guard)
	if err := deps.execInstalledWithEnv(paths.Binary, next, env); err != nil {
		return true, deps.errorf("recovery restored a different binary at %s; run %s upgrade to continue: %v", paths.Binary, paths.Binary, err)
	}
	return true, 0
}

func settingsFromConfig(cfg *config.Config) activation.Settings {
	hub := cfg.Components.Hub
	return activation.Settings{
		HubEnabled:       hub.Enabled,
		CollectorEnabled: cfg.Components.Collector.Enabled,
		HubListen:        hub.Listen,
		HubPort:          hub.Port,
		HubOrigin:        hub.Origin,
		HubTLSCert:       hub.TLSCert,
		HubTLSKey:        hub.TLSKey,
	}
}

// httpClient returns the *http.Client release downloads and health probes need.
// A non-*http.Client test doer is wrapped in a transport so the fetcher can
// still install its redirect policy.
func (d Deps) httpClient() *http.Client {
	if client, ok := d.HTTPClient.(*http.Client); ok && client != nil {
		return client
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	if d.HTTPClient != nil {
		client.Transport = doerTransport{doer: d.HTTPClient}
	}
	return client
}

type doerTransport struct{ doer HTTPDoer }

func (t doerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return t.doer.Do(request)
}

// assetArch maps Go architectures to published release names.
func assetArch(arch string) (string, error) {
	switch arch {
	case "amd64", "arm64", "armv7":
		return arch, nil
	case "arm":
		return "armv7", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q; Rigscope publishes linux/amd64, linux/arm64 and linux/armv7", arch)
	}
}

func assetArchValue(arch string) string {
	value, err := assetArch(arch)
	if err != nil {
		return arch
	}
	return value
}

// hubHealthURL is retained for status/doctor and tests.
func hubHealthURL(cfg *config.Config) string {
	target, _, _, err := activation.HubHealthURL(settingsFromConfig(cfg))
	if err != nil {
		return ""
	}
	return target
}

// extractBinary and verifyChecksum keep small wrappers over internal/release so
// older callers and tests share one implementation.
func extractBinary(archive []byte, limit int64) ([]byte, error) {
	return release.ExtractBinary(archive, release.ExtractOptions{MaxBinary: limit})
}

func verifyChecksum(sums []byte, asset string, data []byte) error {
	return release.VerifyChecksum(sums, asset, data)
}
