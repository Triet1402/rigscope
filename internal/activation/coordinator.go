package activation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/platform/fsutil"
	"github.com/Triet1402/rigscope/internal/release"
)

// ErrAlreadyCurrent is returned when the resolved release equals the installed
// version (and the installed version is a real release, not "dev").
var ErrAlreadyCurrent = errors.New("the requested release is already installed")

// Layout is the exact set of paths the transaction touches. It is persisted in
// the upgrade journal so recovery does not need to decode configuration.
type Layout struct {
	// Scope is "user" or "system".
	Scope string `json:"scope,omitempty"`
	// StateDir is ~/.local/state/rigscope or /var/lib/rigscope.
	StateDir string `json:"stateDir"`
	// Binary is the installed executable; PreviousBinary is its .prev sibling.
	Binary         string `json:"binary"`
	PreviousBinary string `json:"previousBinary"`
	// ConfigFile is the shared configuration file.
	ConfigFile string `json:"configFile"`
	// CollectorStateDir holds the collector identity and credentials.
	CollectorStateDir string `json:"collectorStateDir,omitempty"`
	// HubDBFile is the hub's SQLite database, when it exists.
	HubDBFile string `json:"hubDBFile,omitempty"`
	// JournalPath is the upgrade journal itself.
	JournalPath string `json:"journalPath,omitempty"`
}

// ReleaseFetcher resolves and stages releases. *release.Fetcher implements it.
type ReleaseFetcher interface {
	Resolve(ctx context.Context, version string) (release.Release, error)
	Stage(ctx context.Context, rel release.Release) (release.Staged, error)
}

// HubLease is the running hub's process lease (hub.lock). The transaction
// takes it after quiescing the hub and holds it while it backs up and migrates
// the database, so a standalone `migrate` or a racing hub start cannot open the
// same database concurrently. A nil lease disables acquisition (unit tests).
type HubLease interface {
	// Acquire takes the lease without blocking. A busy lease is an error.
	Acquire(ctx context.Context) (release func(), err error)
	// File returns the held descriptor so a staged child can inherit it.
	File() *os.File
}

// Options injects every side effect the coordinator performs.
type Options struct {
	Layout   Layout
	FS       FS
	Clock    Clock
	Services Services
	Releases ReleaseFetcher
	Backup   BackupManager
	Migrator Migrator
	// Readiness verifies the activated install.
	Readiness Readiness
	// Settings supplies the enabled components and hub health settings.
	Settings SettingsSource
	// Lock is the shared installation lock. CLI wiring supplies an adapter over
	// the same helper install/uninstall use.
	Lock Locker
	// HubLease is the hub's runtime lease. It is taken after quiescing and
	// held across the backup and migration so those steps are exclusive against
	// the hub. Optional; tests that do not exercise it leave it nil.
	HubLease HubLease
	// Owner scopes re-entrant locking to this coordinator instance. Empty
	// generates a token. The CLI passes the token of the lock it already holds
	// so recovery and the upgrade share it.
	Owner string
	// Ownership records every artifact this transaction creates in the
	// installation manifest. Without it those paths are never authorized for
	// deletion, so an interrupted transaction errs on the side of keeping
	// files.
	Ownership Ownership
	Log       io.Writer
	// RecoveryTimeout bounds crash recovery. It defaults to five minutes and
	// never uses the caller's (possibly cancelled) context.
	RecoveryTimeout time.Duration

	// ConfirmStop is asked before a transaction stops running services. It
	// receives the action ("upgrade" or "rollback"), the target version and the
	// active units, and runs before any journal is written, so an error (the
	// operator declining) leaves nothing to recover. Nil approves.
	ConfirmStop func(ctx context.Context, action, version string, running []string) error

	// AfterPhase is a test-only crash seam. A non-nil return stops the
	// transaction at that phase boundary without rolling back, so a test can
	// construct a new coordinator from the journal exactly as a process restart
	// would.
	AfterPhase func(Phase) error
}

// Coordinator runs upgrade, rollback and recovery transactions.
type Coordinator struct {
	opts   Options
	store  JournalStore
	clock  Clock
	layout Layout
	fs     FS
	owner  string
	// hubLeaseRelease is non-nil while this coordinator holds the hub runtime
	// lease. It is released before any unit is started again.
	hubLeaseRelease func()
}

// UpgradeRequest names the version to activate. CurrentVersion is the version
// reported by the running binary.
type UpgradeRequest struct {
	Version        string
	CurrentVersion string
}

// Result describes a completed or rolled-back transaction.
type Result struct {
	From       string
	To         string
	BackupDir  string
	Phase      Phase
	RolledBack bool
}

// New validates options and fills defaults.
func New(opts Options) (*Coordinator, error) {
	if opts.Layout.StateDir == "" {
		return nil, errors.New("activation: state directory is required")
	}
	if opts.Layout.Binary == "" || opts.Layout.PreviousBinary == "" {
		return nil, errors.New("activation: binary and previous binary paths are required")
	}
	if opts.Layout.ConfigFile == "" {
		return nil, errors.New("activation: config file is required")
	}
	if !filepath.IsAbs(opts.Layout.StateDir) {
		return nil, errors.New("activation: state directory must be absolute")
	}
	if opts.FS == nil {
		opts.FS = OSFS{}
	}
	if opts.Clock == nil {
		opts.Clock = RealClock{}
	}
	if opts.Services == nil {
		return nil, errors.New("activation: services are required")
	}
	if opts.Backup == nil {
		opts.Backup = NewFileBackup(nil)
	}
	if opts.Migrator == nil {
		opts.Migrator = NewExecMigrator()
	}
	if opts.Readiness == nil {
		return nil, errors.New("activation: readiness checker is required")
	}
	if opts.Settings == nil {
		return nil, errors.New("activation: settings source is required")
	}
	if opts.Lock == nil {
		return nil, errors.New("activation: installation locker is required")
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.RecoveryTimeout <= 0 {
		opts.RecoveryTimeout = 5 * time.Minute
	}
	owner := opts.Owner
	if owner == "" {
		owner = NewOwnerToken()
	}
	journalPath := opts.Layout.JournalPath
	if journalPath == "" {
		journalPath = filepath.Join(opts.Layout.StateDir, "upgrade-journal.json")
	}
	return &Coordinator{
		opts:   opts,
		store:  JournalStore{Path: journalPath, FS: opts.FS},
		clock:  opts.Clock,
		layout: opts.Layout,
		fs:     opts.FS,
		owner:  owner,
	}, nil
}

// Upgrade runs the transaction. An interrupted previous transaction is
// recovered (resumed or rolled back) before this one starts.
func (c *Coordinator) Upgrade(ctx context.Context, req UpgradeRequest) (Result, error) {
	if c.opts.Releases == nil {
		return Result{}, errors.New("activation: release fetcher is required to upgrade")
	}
	unlock, err := c.opts.Lock.Lock(ctx, c.owner)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	defer c.releaseHubLease()

	if err := c.recoverLocked(ctx); err != nil {
		return Result{}, err
	}
	resolved, err := c.opts.Releases.Resolve(ctx, req.Version)
	if err != nil {
		return Result{}, err
	}
	current := strings.TrimPrefix(strings.TrimSpace(req.CurrentVersion), "v")
	if current != "" && current != "dev" && strings.TrimPrefix(resolved.Version, "v") == current {
		return Result{From: current, To: resolved.Version, Phase: PhaseDone}, ErrAlreadyCurrent
	}
	settings, err := c.opts.Settings.Settings()
	if err != nil {
		return Result{}, err
	}
	if err := c.confirmStop(ctx, "upgrade", resolved.Version, settings.Components()); err != nil {
		return Result{}, err
	}

	layout := c.layout
	layout.JournalPath = c.store.Path
	journal := &Journal{
		ID:        newID(),
		Phase:     PhaseLocked,
		From:      req.CurrentVersion,
		To:        resolved.Version,
		StartedAt: c.clock.Now(),
		UpdatedAt: c.clock.Now(),
		Layout:    layout,
		Settings:  settings,
		Units:     settings.Components(),
	}
	if err := c.savePhase(journal, PhaseLocked, "transaction started"); err != nil {
		return Result{}, err
	}

	if err := c.run(ctx, journal, resolved, settings); err != nil {
		if isCrash(err) {
			return Result{}, err
		}
		return Result{}, c.failed(journal, err)
	}
	result := Result{
		From:      journal.From,
		To:        journal.To,
		BackupDir: journal.BackupDir,
		Phase:     journal.Phase,
	}
	c.cleanup(journal)
	return result, nil
}

// Recover resumes or rolls back an interrupted transaction. It is also called
// by Upgrade before a new transaction starts.
func (c *Coordinator) Recover(ctx context.Context) (Result, error) {
	unlock, err := c.opts.Lock.Lock(ctx, c.owner)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	defer c.releaseHubLease()
	if err := c.recoverLocked(ctx); err != nil {
		return Result{}, err
	}
	journal, err := c.store.Load()
	if err != nil {
		return Result{}, err
	}
	return Result{From: journal.From, To: journal.To, BackupDir: journal.BackupDir, Phase: journal.Phase, RolledBack: journal.Phase == PhaseRolledBack}, nil
}

// RecoverPending recovers an interrupted transaction if one exists. It is used
// by commands that must repair an installation before loading configuration.
func (c *Coordinator) RecoverPending(ctx context.Context) (Result, error) {
	unlock, err := c.opts.Lock.Lock(ctx, c.owner)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	defer c.releaseHubLease()
	if err := c.recoverLocked(ctx); err != nil {
		return Result{}, err
	}
	journal, err := c.store.Load()
	if err != nil {
		if errors.Is(err, ErrNoJournal) {
			return Result{}, nil
		}
		return Result{}, err
	}
	return Result{From: journal.From, To: journal.To, BackupDir: journal.BackupDir, Phase: journal.Phase, RolledBack: journal.Phase == PhaseRolledBack}, nil
}

func (c *Coordinator) run(ctx context.Context, journal *Journal, resolved release.Release, settings Settings) error {
	staged, err := c.opts.Releases.Stage(ctx, resolved)
	if err != nil {
		return err
	}
	journal.To = staged.Version
	journal.StagedBinary = staged.Path
	journal.StagedDir = staged.Dir
	// Record ownership before the phase is journaled: a process death at this
	// boundary must still leave the staged tree recognizable to uninstall.
	if staged.Dir != "" {
		// The manifest owns the staging root; the per-release directory and
		// verified binary inside it are covered by directory ownership.
		root := filepath.Join(c.layout.StateDir, "staging")
		if pathWithin(staged.Dir, root) {
			if err := c.record(root, ArtifactStagingDir, ""); err != nil {
				return err
			}
		} else if err := c.record(staged.Dir, ArtifactStagingDir, ""); err != nil {
			return err
		}
	}
	if err := c.savePhase(journal, PhaseStaged, "release verified and staged"); err != nil {
		return err
	}

	// Quiesce: collector first, then hub, and verify both are inactive.
	journal.ServicesStopped = true
	if err := c.save(journal); err != nil {
		return err
	}
	if err := c.quiesce(ctx, settings.StopOrder()); err != nil {
		return err
	}
	if err := c.savePhase(journal, PhaseQuiesced, "components stopped"); err != nil {
		return err
	}
	// The hub is stopped. Take its runtime lease and hold it across the backup
	// and migration, so a standalone `rigscope migrate` or a racing hub start
	// cannot open the database the transaction is about to copy and rewrite.
	if err := c.acquireHubLease(ctx); err != nil {
		return err
	}

	backupDir, err := c.backupDir(journal)
	if err != nil {
		return err
	}
	journal.BackupDir = backupDir
	if err := c.save(journal); err != nil {
		return err
	}
	if _, err := c.opts.Backup.Create(ctx, BackupRequest{
		Dir:          backupDir,
		From:         journal.From,
		To:           journal.To,
		HubDBFile:    c.layout.HubDBFile,
		ConfigFile:   c.layout.ConfigFile,
		CollectorDir: c.layout.CollectorStateDir,
	}); err != nil {
		return err
	}
	journal.BackupComplete = true
	if err := c.record(filepath.Join(c.layout.StateDir, "backups"), ArtifactBackupsDir, ""); err != nil {
		return err
	}
	if err := c.savePhase(journal, PhaseBackedUp, "database, configuration and identity backed up"); err != nil {
		return err
	}

	previousSchema, err := c.readPreviousSchema(ctx)
	if err != nil {
		return err
	}
	migrated, err := c.opts.Migrator.Migrate(ctx, MigrateRequest{
		Binary:     journal.StagedBinary,
		ConfigFile: c.layout.ConfigFile,
		StateDir:   c.layout.StateDir,
		From:       journal.From,
		To:         journal.To,
		System:     c.layout.Scope == "system",
		LockFile:   c.lockedFile(),
		// The child inherits the hub lease this process already holds instead
		// of trying to acquire it again and refusing its own parent.
		HubLockFile: c.hubLockFile(),
	})
	if err != nil {
		return fmt.Errorf("migrate with %s: %w", journal.To, err)
	}
	journal.PreviousSchema = previousSchema
	journal.Schema = migrated.Schema
	journal.CompatibleDown = migrated.CompatibleDown
	if err := c.savePhase(journal, PhaseMigrated, "migrations applied by the staged binary"); err != nil {
		return err
	}

	prepared, err := fsutil.PrepareBinary(journal.StagedBinary, c.layout.Binary, journal.ID)
	if err != nil {
		return fmt.Errorf("prepare %s: %w", journal.To, err)
	}
	previousSum, err := fsutil.HashFile(c.layout.Binary)
	if err != nil {
		_ = fsutil.CleanupTransaction(c.layout.Binary, journal.ID)
		return fmt.Errorf("hash installed binary: %w", err)
	}
	journal.PreparedBinary = prepared.Path
	journal.NewBinarySHA256 = prepared.SHA256
	journal.PreviousBinaryPath = c.layout.PreviousBinary
	journal.PreviousBinarySHA256 = previousSum
	if err := c.record(prepared.Path, ArtifactBinaryTemp, prepared.SHA256); err != nil {
		return err
	}
	if err := c.savePhase(journal, PhasePrepared, "new binary prepared and fsynced"); err != nil {
		return err
	}

	journal.PublishStarted = true
	if err := c.save(journal); err != nil {
		return err
	}
	// Publish .prev as its own step: record its verified digest and ownership
	// before the target swap, so a crash in the window between them leaves an
	// owned previous binary instead of an orphan uninstall must not touch.
	previous, err := fsutil.PublishPrevious(prepared, c.layout.Binary, c.layout.PreviousBinary, journal.ID)
	if err != nil {
		return fmt.Errorf("retain previous %s: %w", journal.From, err)
	}
	if previous.SHA256 != journal.PreviousBinarySHA256 {
		return fmt.Errorf("retained previous binary does not match the journal")
	}
	journal.PreviousPublished = true
	if err := c.record(c.layout.PreviousBinary, ArtifactPreviousBinary, previous.SHA256); err != nil {
		return err
	}
	if err := c.savePhase(journal, PhasePreviousPublished, "previous binary published and verified"); err != nil {
		return err
	}
	if err := fsutil.SwapPrepared(prepared, c.layout.Binary, journal.ID); err != nil {
		return fmt.Errorf("activate %s: %w", journal.To, err)
	}
	c.forget(prepared.Path, ArtifactBinaryTemp)
	if err := c.savePhase(journal, PhaseActivated, "binary activated"); err != nil {
		return err
	}

	// Writers may run from the first start onward. Record that before issuing
	// the first start so recovery never restores the pre-upgrade snapshot over
	// data written by the new version.
	if len(settings.Components()) > 0 {
		journal.AcceptedWrites = true
		if err := c.save(journal); err != nil {
			return err
		}
	}
	// The hub is about to start under its own lease; do not keep it held.
	c.releaseHubLease()
	for _, unit := range journal.startOrder() {
		if err := c.opts.Services.Restart(ctx, unit); err != nil {
			return fmt.Errorf("start %s: %w", unit, err)
		}
	}

	if err := c.opts.Readiness.Check(ctx, journal.To); err != nil {
		return fmt.Errorf("health check after activation: %w", err)
	}
	if err := c.savePhase(journal, PhaseVerified, "readiness contract met"); err != nil {
		return err
	}
	if err := c.savePhase(journal, PhaseDone, "upgrade complete"); err != nil {
		return err
	}
	c.cleanupBackups(journal)
	return nil
}

// failed rolls back a failed transaction with a bounded recovery context and
// returns an error that distinguishes rolled-back from needs-attention.
func (c *Coordinator) failed(journal *Journal, cause error) error {
	recoveryCtx, cancel := context.WithTimeout(context.Background(), c.opts.RecoveryTimeout)
	defer cancel()
	original := journal.Phase
	refused := !rollbackAllowed(journal)
	rollbackErr := c.rollback(recoveryCtx, journal, cause)
	switch journal.Phase {
	case PhaseRolledBack:
		if rollbackErr != nil {
			return fmt.Errorf("upgrade to %s failed and was rolled back (%s), with recovery problems: %v: %w", journal.To, original, rollbackErr, cause)
		}
		return fmt.Errorf("upgrade to %s failed and was rolled back: %w", journal.To, cause)
	case PhaseFailedNeedsAttention:
		if refused {
			return fmt.Errorf("upgrade to %s failed: %v; automatic rollback was refused: %w", journal.To, cause, rollbackErr)
		}
		return fmt.Errorf("upgrade to %s failed: %v; automatic rollback stopped with artifacts kept: %w", journal.To, cause, rollbackErr)
	default:
		return fmt.Errorf("upgrade to %s failed: %v; rollback failed: %w", journal.To, cause, rollbackErr)
	}
}

// rollback restores the previous release. It resumes from any recorded
// rollback substate. It returns an error when the rollback could not finish;
// the journal phase always records the outcome.
func (c *Coordinator) rollback(ctx context.Context, journal *Journal, cause error) error {
	layout := c.journalLayout(journal)
	if !rollbackAllowed(journal) {
		return c.needsAttention(journal, cause, errors.New(rollbackRefusal(journal)))
	}

	if rollbackRank(journal.Phase) == 0 {
		journal.Phase = PhaseRollingBack
		journal.Error = cause.Error()
		journal.UpdatedAt = c.clock.Now()
		journal.History = append(journal.History, Event{At: journal.UpdatedAt, Phase: PhaseRollingBack, Note: cause.Error()})
		if err := c.saveNow(journal); err != nil {
			return err
		}
	}
	fail := func(err error) error {
		return c.needsAttention(journal, cause, err)
	}

	restoreNeedsQuiesce := c.needsQuiesce(journal)
	if !rollbackAtLeast(journal.Phase, PhaseRollbackQuiesced) {
		if journal.ServicesStopped && restoreNeedsQuiesce {
			if err := c.quiesce(ctx, journal.stopOrder()); err != nil {
				return fail(fmt.Errorf("quiesce before restore: %w", err))
			}
		}
		if err := c.savePhase(journal, PhaseRollbackQuiesced, "components verified inactive"); err != nil {
			return err
		}
	} else if journal.ServicesStopped && restoreNeedsQuiesce && !rollbackAtLeast(journal.Phase, PhaseRollbackWritersStarted) {
		// Before restoring anything, prove the writers are still inactive.
		if err := c.quiesce(ctx, journal.stopOrder()); err != nil {
			return fail(fmt.Errorf("quiesce before restore: %w", err))
		}
	}

	if !rollbackAtLeast(journal.Phase, PhaseRollbackDataRestored) {
		// Restoring the snapshot rewrites the hub database. Hold the hub lease
		// across it unless this process already took it before the backup.
		if journal.BackupDir != "" && !journal.AcceptedWrites {
			if err := c.acquireHubLease(ctx); err != nil {
				return fail(err)
			}
		}
		if err := c.restoreData(ctx, journal); err != nil {
			return fail(err)
		}
		if err := c.savePhase(journal, PhaseRollbackDataRestored, "data snapshot restored"); err != nil {
			return err
		}
	}

	if !rollbackAtLeast(journal.Phase, PhaseRollbackBinaryRestored) {
		if journal.PublishStarted {
			// publishStarted is only the intent: a crash may land after the
			// flag was journaled but before PublishPrepared changed anything.
			// Finish any interrupted rollback swap first (both candidates are
			// verified against the journal), then observe what is installed.
			if _, err := fsutil.RecoverSwap(layout.Binary, layout.PreviousBinary, journal.ID, journal.PreviousBinarySHA256, journal.NewBinarySHA256); err != nil {
				return fail(fmt.Errorf("recover binary swap: %w", err))
			}
			swapped, err := c.publicationApplied(journal, layout)
			if err != nil {
				return fail(err)
			}
			if swapped {
				if err := c.restoreBinary(journal, layout); err != nil {
					return fail(err)
				}
			}
			// When swapped is false the previous release is still installed:
			// there is nothing to restore and .prev may be absent (or still
			// hold an older release). Record only what the transaction
			// actually created.
		}
		if err := c.refreshPreviousAfterRollback(journal, layout); err != nil {
			return fail(err)
		}
		if err := c.savePhase(journal, PhaseRollbackBinaryRestored, "previous binary restored"); err != nil {
			return err
		}
	} else if err := c.refreshPreviousAfterRollback(journal, layout); err != nil {
		// Resuming at or after this substate: refresh the record as well.
		return fail(err)
	}

	if !rollbackAtLeast(journal.Phase, PhaseRollbackWritersStarted) {
		// Record that writers may run before starting them: a later crash must
		// never replay the data snapshot over writes made by the old version.
		if err := c.savePhase(journal, PhaseRollbackWritersStarted, "writers may run after restoration"); err != nil {
			return err
		}
	}
	// No data or binary restore is pending; the hub may start and must be able
	// to take its own runtime lease.
	c.releaseHubLease()
	if journal.ServicesStopped {
		for _, unit := range journal.startOrder() {
			if err := c.opts.Services.Restart(ctx, unit); err != nil {
				return fail(fmt.Errorf("restart %s: %w", unit, err))
			}
		}
		if journal.From != "" {
			if err := c.opts.Readiness.Check(ctx, journal.From); err != nil {
				return fail(fmt.Errorf("restored install health check: %w", err))
			}
		}
	}

	journal.Phase = PhaseRolledBack
	journal.UpdatedAt = c.clock.Now()
	journal.History = append(journal.History, Event{At: journal.UpdatedAt, Phase: PhaseRolledBack, Note: "restoration complete"})
	if err := c.saveNow(journal); err != nil {
		return err
	}
	c.cleanup(journal)
	c.cleanupBackups(journal)
	return nil
}

// needsAttention stops a rollback without discarding artifacts and records the
// failure. A later run refuses to continue automatically.
func (c *Coordinator) needsAttention(journal *Journal, cause, failure error) error {
	journal.Phase = PhaseFailedNeedsAttention
	journal.Error = fmt.Sprintf("%v; %v", cause, failure)
	journal.UpdatedAt = c.clock.Now()
	journal.History = append(journal.History, Event{At: journal.UpdatedAt, Phase: PhaseFailedNeedsAttention, Note: journal.Error})
	saveErr := c.saveNow(journal)
	return errors.Join(failure, saveErr)
}

// Rollback performs a manual rollback of a completed upgrade. It is allowed
// only when the new version never accepted writes, or when the release declared
// compatibility down to the previous schema.
func (c *Coordinator) Rollback(ctx context.Context) (Result, error) {
	unlock, err := c.opts.Lock.Lock(ctx, c.owner)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	defer c.releaseHubLease()
	if err := c.recoverLocked(ctx); err != nil {
		return Result{}, err
	}
	journal, err := c.store.Load()
	if err != nil {
		if errors.Is(err, ErrNoJournal) {
			if _, statErr := os.Stat(c.layout.PreviousBinary); statErr != nil {
				return Result{}, fmt.Errorf("no previous binary at %s; nothing to roll back", c.layout.PreviousBinary)
			}
			return Result{}, fmt.Errorf("no upgrade journal; there is nothing to roll back")
		}
		return Result{}, err
	}
	switch journal.Phase {
	case PhaseDone:
	case PhaseRolledBack:
		return Result{From: journal.From, To: journal.To, Phase: journal.Phase, RolledBack: true}, errors.New("this upgrade has already been rolled back")
	case PhaseFailedNeedsAttention:
		return Result{}, errors.New(rollbackRefusal(journal))
	default:
		return Result{}, fmt.Errorf("upgrade journal is at phase %q; finish or recover it with rigscope upgrade first", journal.Phase)
	}
	if !rollbackAllowed(journal) {
		return Result{}, errors.New(rollbackRefusal(journal))
	}
	if journal.PreviousBinarySHA256 == "" {
		return Result{}, fmt.Errorf("upgrade journal has no recorded previous binary; refusing to guess a rollback")
	}

	if err := c.confirmStop(ctx, "rollback", journal.From, journal.Settings.Components()); err != nil {
		return Result{}, err
	}

	journal.Phase = PhaseRollingBack
	journal.ServicesStopped = true
	journal.UpdatedAt = c.clock.Now()
	journal.History = append(journal.History, Event{At: journal.UpdatedAt, Phase: PhaseRollingBack, Note: "manual rollback"})
	if err := c.saveNow(journal); err != nil {
		return Result{}, err
	}
	if err := c.rollback(ctx, journal, errors.New("manual rollback")); err != nil {
		return Result{}, err
	}
	if journal.Phase != PhaseRolledBack {
		return Result{}, fmt.Errorf("manual rollback did not complete; the upgrade journal is at %q", journal.Phase)
	}
	return Result{From: journal.From, To: journal.To, BackupDir: journal.BackupDir, Phase: journal.Phase, RolledBack: true}, nil
}

// needsQuiesce reports whether the rollback is about to restore data or a
// binary. Only then must the units be proven inactive first.
func (c *Coordinator) needsQuiesce(journal *Journal) bool {
	if journal.PublishStarted {
		return true
	}
	if journal.AcceptedWrites || journal.BackupDir == "" {
		return false
	}
	if journal.BackupComplete {
		return true
	}
	_, err := os.Stat(filepath.Join(journal.BackupDir, backupManifestName))
	return err == nil
}

// restoreData restores the snapshot only when the new version never accepted
// writes. A restore failure stops the rollback.
func (c *Coordinator) restoreData(ctx context.Context, journal *Journal) error {
	if journal.AcceptedWrites || journal.BackupDir == "" {
		return nil
	}
	manifest := filepath.Join(journal.BackupDir, backupManifestName)
	if _, err := os.Stat(manifest); err == nil {
		if err := c.opts.Backup.Restore(ctx, journal.BackupDir); err != nil {
			return fmt.Errorf("restore backup: %w", err)
		}
		// Restoring the collector identity tree replaces its inode. Refresh the
		// manifest's directory identity under the same lock so a later
		// uninstall still recognises it as Rigscope's own directory. A snapshot
		// that had no collector directory removes it, so stop claiming it.
		if c.layout.CollectorStateDir != "" {
			if _, err := os.Stat(c.layout.CollectorStateDir); err == nil {
				if err := c.record(c.layout.CollectorStateDir, ArtifactCollectorDir, ""); err != nil {
					return err
				}
			} else if errors.Is(err, os.ErrNotExist) {
				c.forget(c.layout.CollectorStateDir, ArtifactCollectorDir)
			} else {
				return fmt.Errorf("inspect restored collector directory: %w", err)
			}
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect backup: %w", err)
	}
	if journal.BackupComplete {
		return fmt.Errorf("backup manifest %s is missing; refusing to continue", manifest)
	}
	if err := c.fs.RemoveAll(journal.BackupDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("discard partial backup: %w", err)
	}
	return nil
}

// restoreBinary restores the recorded previous release. It works from journal
// digests and never depends on the staging directory.
func (c *Coordinator) restoreBinary(journal *Journal, layout Layout) error {
	if journal.PreviousBinarySHA256 == "" {
		return errors.New("journal has no previous binary SHA-256; refusing to guess a rollback")
	}
	// Finish any interrupted RestorePrevious first. If the target is already the
	// recorded previous release, the swap happened and must not be replayed in
	// a way that overwrites .prev (which now holds the newer release). Both
	// pending candidates must match the journal digests; an unverified file is
	// never renamed into place.
	if journal.PublishStarted {
		if _, err := fsutil.RecoverSwap(layout.Binary, layout.PreviousBinary, journal.ID, journal.PreviousBinarySHA256, journal.NewBinarySHA256); err != nil {
			return fmt.Errorf("recover binary swap: %w", err)
		}
	}
	targetSum, err := fsutil.HashFile(layout.Binary)
	if err != nil {
		return fmt.Errorf("hash installed binary: %w", err)
	}
	if targetSum == journal.PreviousBinarySHA256 {
		return nil
	}
	if journal.PublishStarted {
		if _, err := fsutil.RecoverPublish(layout.Binary, layout.PreviousBinary, journal.ID, journal.NewBinarySHA256, journal.PreviousBinarySHA256); err != nil {
			return fmt.Errorf("recover binary publication: %w", err)
		}
		targetSum, err = fsutil.HashFile(layout.Binary)
		if err != nil {
			return fmt.Errorf("hash installed binary: %w", err)
		}
		if targetSum == journal.PreviousBinarySHA256 {
			return nil
		}
	}
	previousSum, err := fsutil.HashFile(layout.PreviousBinary)
	if err != nil {
		return fmt.Errorf("previous binary %s does not match the journal: %w", layout.PreviousBinary, err)
	}
	if previousSum != journal.PreviousBinarySHA256 {
		return fmt.Errorf("previous binary %s has SHA-256 %s, journal records %s", layout.PreviousBinary, previousSum, journal.PreviousBinarySHA256)
	}
	if err := fsutil.RestorePrevious(layout.Binary, layout.PreviousBinary, journal.ID); err != nil {
		return fmt.Errorf("restore previous binary: %w", err)
	}
	restored, err := fsutil.HashFile(layout.Binary)
	if err != nil {
		return fmt.Errorf("verify restored binary: %w", err)
	}
	if restored != journal.PreviousBinarySHA256 {
		return fmt.Errorf("restored binary SHA-256 %s does not match journal %s", restored, journal.PreviousBinarySHA256)
	}
	return nil
}

func (c *Coordinator) recoverLocked(ctx context.Context) error {
	journal, err := c.store.Load()
	if err != nil {
		if errors.Is(err, ErrNoJournal) {
			return nil
		}
		return err
	}
	switch journal.Phase {
	case PhaseDone, PhaseRolledBack:
		c.cleanup(journal)
		return nil
	case PhaseFailedNeedsAttention:
		return fmt.Errorf("the previous upgrade to %s needs attention: %s", journal.To, journal.Error)
	case PhaseVerified:
		if err := c.savePhase(journal, PhaseDone, "recovered after successful verification"); err != nil {
			return err
		}
		if err := c.refreshPreviousBinaryOwnership(journal, true); err != nil {
			return err
		}
		c.cleanup(journal)
		c.cleanupBackups(journal)
		return nil
	}
	recoveryCtx, cancel := context.WithTimeout(context.Background(), c.opts.RecoveryTimeout)
	defer cancel()
	_ = ctx
	cause := fmt.Errorf("interrupted upgrade to %s at phase %s", journal.To, journal.Phase)
	if err := c.rollback(recoveryCtx, journal, cause); err != nil {
		if journal.Phase == PhaseFailedNeedsAttention {
			return fmt.Errorf("recover interrupted upgrade: %w", err)
		}
		return fmt.Errorf("recover interrupted upgrade: %w", err)
	}
	if journal.Phase == PhaseFailedNeedsAttention {
		return errors.New(rollbackRefusal(journal))
	}
	if journal.Phase == PhaseRolledBack {
		if err := c.refreshPreviousBinaryOwnership(journal, false); err != nil {
			return err
		}
	}
	return nil
}

// refreshPreviousBinaryOwnership records the current .prev identity after a
// recovery renamed transaction temp files into place. completed selects the
// digest semantics: a completed upgrade keeps the old release in .prev, while
// a rollback keeps the failed new release there.
func (c *Coordinator) refreshPreviousBinaryOwnership(journal *Journal, completed bool) error {
	if !journal.PublishStarted {
		return nil
	}
	layout := c.journalLayout(journal)
	if layout.PreviousBinary == "" {
		return nil
	}
	if _, err := os.Stat(layout.PreviousBinary); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect previous binary: %w", err)
	}
	if !completed {
		// A rollback only creates the failed release in .prev when publication
		// actually swapped the target. When the crash landed before the swap,
		// .prev is untouched and must keep its existing ownership record.
		return c.refreshPreviousAfterRollback(journal, layout)
	}
	return c.record(layout.PreviousBinary, ArtifactPreviousBinary, journal.PreviousBinarySHA256)
}

// confirmStop asks ConfirmStop about the units that are active right now. No
// active unit needs no approval.
func (c *Coordinator) confirmStop(ctx context.Context, action, version string, units []string) error {
	if c.opts.ConfirmStop == nil {
		return nil
	}
	var running []string
	for _, unit := range units {
		active, err := c.opts.Services.IsActive(ctx, unit)
		if err == nil && active {
			running = append(running, unit)
		}
	}
	if len(running) == 0 {
		return nil
	}
	return c.opts.ConfirmStop(ctx, action, version, running)
}

func (c *Coordinator) quiesce(ctx context.Context, units []string) error {
	for _, unit := range units {
		if err := c.opts.Services.Stop(ctx, unit); err != nil {
			return fmt.Errorf("stop %s: %w", unit, err)
		}
		active, err := c.opts.Services.IsActive(ctx, unit)
		if err != nil {
			return fmt.Errorf("verify %s stopped: %w", unit, err)
		}
		if active {
			return fmt.Errorf("%s is still active after stop", unit)
		}
	}
	return nil
}

func (c *Coordinator) backupDir(journal *Journal) (string, error) {
	stamp := c.clock.Now().UTC().Format("20060102T150405Z")
	name := fmt.Sprintf("%s-%s-to-%s", stamp, safeName(journal.From), safeName(journal.To))
	dir := filepath.Join(c.layout.StateDir, "backups", name)
	if _, err := os.Stat(dir); err == nil {
		dir = fmt.Sprintf("%s-%s", dir, journal.ID[:8])
		if _, err := os.Stat(dir); err == nil {
			return "", fmt.Errorf("backup directory %s already exists", dir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect backup directory: %w", err)
	}
	return dir, nil
}

// readPreviousSchema records the schema the previous binary actually wrote,
// before the staged binary migrates it. Configuration and the hub database are
// independent stores, so the effective previous schema is the larger of the two
// versions the previous binary left behind. A fresh hub has no database yet.
func (c *Coordinator) readPreviousSchema(ctx context.Context) (int, error) {
	configVersion, err := c.readConfigSchema()
	if err != nil {
		return 0, err
	}
	previous := configVersion
	if c.layout.HubDBFile != "" {
		info, err := storage.ReadSchemaLedger(ctx, c.layout.HubDBFile)
		switch {
		case err == nil:
			if info.Version > previous {
				previous = info.Version
			}
		case errors.Is(err, storage.ErrNoSchemaLedger):
			// No database yet (fresh hub) or a placeholder file that is not a
			// SQLite ledger; the configuration version is the whole schema.
		default:
			return 0, fmt.Errorf("read previous hub schema: %w", err)
		}
	}
	return previous, nil
}

func (c *Coordinator) readConfigSchema() (int, error) {
	data, err := c.fs.ReadFile(c.layout.ConfigFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read configuration schema: %w", err)
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return 0, fmt.Errorf("parse configuration schema: %w", err)
	}
	return header.Version, nil
}

// publicationApplied reports whether PublishPrepared actually swapped the
// target. The journal records both digests before publication starts, so the
// installed file distinguishes "the intent was journaled" from "the swap
// happened" without requiring .prev.
func (c *Coordinator) publicationApplied(journal *Journal, layout Layout) (bool, error) {
	targetSum, err := fsutil.HashFile(layout.Binary)
	if err != nil {
		return false, fmt.Errorf("hash installed binary: %w", err)
	}
	switch targetSum {
	case journal.NewBinarySHA256:
		return true, nil
	case journal.PreviousBinarySHA256:
		return false, nil
	default:
		return false, fmt.Errorf(
			"installed binary %s has SHA-256 %s, which matches neither the previous release (%s) nor the prepared release (%s)",
			layout.Binary, targetSum, journal.PreviousBinarySHA256, journal.NewBinarySHA256)
	}
}

// refreshPreviousAfterRollback records the failed new release in .prev only
// when the transaction actually put it there. When publication never reached
// the swap, .prev holds the release that was just retained; if the process died
// before the previous-publication step was journaled, that file is still owned
// by this transaction and is recorded here. An unrecognised .prev is left for
// an operator rather than claimed.
func (c *Coordinator) refreshPreviousAfterRollback(journal *Journal, layout Layout) error {
	if !journal.PublishStarted || layout.PreviousBinary == "" {
		return nil
	}
	sum, err := fsutil.HashFile(layout.PreviousBinary)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect previous binary: %w", err)
	}
	switch sum {
	case journal.NewBinarySHA256, journal.PreviousBinarySHA256:
		return c.record(layout.PreviousBinary, ArtifactPreviousBinary, sum)
	default:
		return nil
	}
}

func (c *Coordinator) savePhase(journal *Journal, phase Phase, note string) error {
	journal.Phase = phase
	journal.UpdatedAt = c.clock.Now()
	journal.History = append(journal.History, Event{At: journal.UpdatedAt, Phase: phase, Note: note})
	if err := c.store.Save(journal); err != nil {
		return err
	}
	if err := c.recordJournal(); err != nil {
		return err
	}
	if c.opts.AfterPhase != nil {
		if err := c.opts.AfterPhase(phase); err != nil {
			return crashSignal{err: err}
		}
	}
	return nil
}

func (c *Coordinator) save(journal *Journal) error {
	journal.UpdatedAt = c.clock.Now()
	return c.saveNow(journal)
}

// saveNow persists a journal snapshot and refreshes its manifest ownership.
func (c *Coordinator) saveNow(journal *Journal) error {
	if err := c.store.Save(journal); err != nil {
		return err
	}
	return c.recordJournal()
}

// lockedFile returns the open descriptor of the installation lock this
// coordinator holds, when the injected Locker can provide it. The staged
// migration child inherits it instead of acquiring the lock again.
func (c *Coordinator) lockedFile() *os.File {
	provider, ok := c.opts.Lock.(LockFileProvider)
	if !ok {
		return nil
	}
	return provider.File()
}

// acquireHubLease takes the hub's runtime lease unless it is already held. It
// is called after the hub has been stopped and before the database is backed
// up or migrated; a busy lease means another hub or offline command won the
// race and the transaction must not touch the database.
func (c *Coordinator) acquireHubLease(ctx context.Context) error {
	if c.opts.HubLease == nil || c.hubLeaseRelease != nil {
		return nil
	}
	release, err := c.opts.HubLease.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire the hub runtime lease: %w", err)
	}
	c.hubLeaseRelease = release
	return nil
}

// releaseHubLease releases the hub runtime lease before any unit is started
// again. It is idempotent; public entry points defer it as a safety net.
func (c *Coordinator) releaseHubLease() {
	if c.hubLeaseRelease == nil {
		return
	}
	release := c.hubLeaseRelease
	c.hubLeaseRelease = nil
	release()
}

// hubLockFile returns the held hub lease descriptor for the staged migration
// child, when the injected lease can provide it.
func (c *Coordinator) hubLockFile() *os.File {
	if c.opts.HubLease == nil {
		return nil
	}
	return c.opts.HubLease.File()
}

// recordJournal refreshes the manifest's record of the journal, whose atomic
// rewrite gives it a new inode on every save.
func (c *Coordinator) recordJournal() error {
	if c.opts.Ownership == nil {
		return nil
	}
	data, err := c.fs.ReadFile(c.store.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read upgrade journal: %w", err)
	}
	return c.record(c.store.Path, ArtifactUpgradeJournal, fsutil.SHA256Bytes(data))
}

// record promotes one activation-created path to owned in the manifest.
func (c *Coordinator) record(path string, kind ArtifactKind, sha256 string) error {
	if c.opts.Ownership == nil || path == "" {
		return nil
	}
	if err := c.opts.Ownership.Record(path, kind, sha256); err != nil {
		return fmt.Errorf("record ownership of %s: %w", path, err)
	}
	return nil
}

// forget stops claiming a transaction temp file that was removed. A failure is
// reported but never turned into a rollback: the safe outcome is to keep
// claiming nothing when the file is gone.
func (c *Coordinator) forget(path string, kind ArtifactKind) {
	if c.opts.Ownership == nil || path == "" {
		return
	}
	if err := c.opts.Ownership.Forget(path, kind); err != nil {
		fmt.Fprintf(c.opts.Log, "warning: could not forget %s in the installation manifest: %v\n", path, err)
	}
}

// cleanup removes staging and transaction temp files after a terminal outcome.
func (c *Coordinator) cleanup(journal *Journal) {
	c.cleanupStaged(journal)
	layout := c.journalLayout(journal)
	for _, suffix := range []string{"new", "prev", "restore-new", "restore-old"} {
		c.forget(fsutil.TransactionTempPath(layout.Binary, journal.ID, suffix), ArtifactBinaryTemp)
	}
	if err := fsutil.CleanupTransaction(layout.Binary, journal.ID); err != nil {
		fmt.Fprintf(c.opts.Log, "warning: could not remove transaction temporary files: %v\n", err)
	}
}

func (c *Coordinator) cleanupStaged(journal *Journal) {
	if journal.StagedDir == "" {
		return
	}
	if err := c.fs.RemoveAll(journal.StagedDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(c.opts.Log, "warning: could not remove staging directory %s: %v\n", journal.StagedDir, err)
	}
	journal.StagedDir = ""
}

// cleanupBackups applies the retention policy: keep the newest three completed
// backups plus the snapshot of the current terminal transaction when a
// supported rollback could still need it. Older directories are removed
// without following symlinks.
func (c *Coordinator) cleanupBackups(journal *Journal) {
	layout := c.journalLayout(journal)
	root := filepath.Join(layout.StateDir, "backups")
	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(c.opts.Log, "warning: could not list backups: %v\n", err)
		}
		return
	}
	type candidate struct {
		name string
		path string
	}
	var completed []candidate
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if _, err := os.Lstat(filepath.Join(path, backupManifestName)); err != nil {
			continue
		}
		completed = append(completed, candidate{name: entry.Name(), path: path})
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].name < completed[j].name })
	protected := ""
	if journal.BackupDir != "" && !journal.AcceptedWrites && rollbackAllowed(journal) {
		protected = filepath.Clean(journal.BackupDir)
	}
	keep := len(completed) - 3
	for index, item := range completed {
		if index >= keep {
			break
		}
		if protected != "" && filepath.Clean(item.path) == protected {
			continue
		}
		if err := fsutil.RemoveAllPath(item.path); err != nil {
			fmt.Fprintf(c.opts.Log, "warning: could not remove old backup %s: %v\n", item.path, err)
		}
	}
}

func (c *Coordinator) journalLayout(journal *Journal) Layout {
	layout := journal.Layout
	if layout.StateDir == "" && layout.Binary == "" {
		return c.layout
	}
	return layout
}

func rollbackAllowed(journal *Journal) bool {
	if !journal.AcceptedWrites {
		return true
	}
	// CompatibleDown is the floor: the lowest schema whose binaries can use the
	// migrated data. Rolling back to PreviousSchema is safe only at or above it.
	return journal.PreviousSchema >= journal.CompatibleDown
}

func rollbackRefusal(journal *Journal) string {
	return fmt.Sprintf(
		"rollback from %s to %s is not allowed: the new release migrated data from schema %d to schema %d and requires at least schema %d, newer than the previous schema. The new release is still installed and neither data nor binaries were discarded; reinstall the previous release only with its paired data.",
		journal.To, journal.From, journal.PreviousSchema, journal.Schema, journal.CompatibleDown)
}

func safeName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(value, "_")
}

func newID() string {
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer[:])
}

// crashSignal marks a test-injected process death. It must skip rollback.
type crashSignal struct{ err error }

func (c crashSignal) Error() string { return "simulated crash: " + c.err.Error() }
func (c crashSignal) Unwrap() error { return c.err }

func isCrash(err error) bool {
	var signal crashSignal
	return errors.As(err, &signal)
}
