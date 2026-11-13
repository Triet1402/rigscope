package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/platform/lock"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// runMigrate is the migration entry point used by the upgrade transaction. It
// is intentionally absent from the short help: the staged binary is invoked as
// `rigscope migrate --from <v> --to <v> --state <dir>` and must print one JSON
// object on stdout:
//
//	{"schema": N, "compatibleDown": M,
//	 "config": {"schema": C, "compatibleDown": CF},
//	 "database": {"schema": D, "compatibleDown": DF}}
//
// schema/compatibleDown stay top-level for the coordinator. The nested objects
// expose each store; compatibleDown is the larger of the configuration and
// database floors. A fresh hub has no database, so only the configuration
// floor is reported and "database" is omitted.
//
// The upgrade transaction passes its held installation lock as an inherited
// descriptor (RIGSCOPE_INSTALL_LOCK_FD). The child verifies the descriptor is
// the documented lock inode and re-locks the same open file description before
// skipping acquisition. There is no flag that claims "the lock is held".
func runMigrate(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("migrate", deps.Stderr)
	from := flags.String("from", "", "release version being upgraded from")
	to := flags.String("to", "", "release version being upgraded to")
	configPath := flags.String("config", "", "configuration file to migrate")
	stateDir := flags.String("state", "", "installation state directory")
	system := flags.Bool("system", false, "migrate the root-owned system installation")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		return deps.errorf("migrate takes no positional arguments, got %q", flags.Arg(0))
	}
	if *configPath == "" {
		return deps.errorf("migrate requires --config")
	}
	_ = from
	_ = to
	// Scope authorization is independent of the lock: inheriting the parent's
	// descriptor never turns a non-root process into a system-scope operation.
	scope := scopeFrom(*system)
	if code, done := deps.ensureRoot(scope); done {
		return code
	}
	paths, err := deps.paths(scope)
	if err != nil {
		return deps.errorf("%v", err)
	}
	held, err := migrationLock(paths, deps.env(lock.InheritedLockFDEnv))
	if err != nil {
		return deps.errorf("%v", err)
	}
	defer held.Release()

	stateRoot := *stateDir
	if stateRoot == "" {
		stateRoot = paths.StateDir
	}
	// The hub database must not be backed up or migrated while the hub (or an
	// offline recovery command) owns the same database. The upgrade transaction
	// passes its held lease as an inherited descriptor; a standalone migrate
	// refuses with "the hub is running" when the lease is busy.
	hubHeld, err := migrationHubLease(hubLeasePathForStateRoot(paths.Scope, stateRoot), deps.env(lock.InheritedHubLockFDEnv))
	if err != nil {
		return deps.errorf("%v", err)
	}
	defer hubHeld.Release()
	if prepare := hubLeasePrepare(deps, paths.Scope); prepare != nil {
		if err := prepare(hubHeld.File()); err != nil {
			return deps.errorf("prepare the hub runtime lease: %v", err)
		}
	}

	data, err := os.ReadFile(*configPath)
	if err != nil {
		return deps.errorf("read configuration: %v", err)
	}
	var header struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil || header.Version == nil {
		return deps.errorf("parse configuration: missing or invalid \"version\"")
	}
	previous := *header.Version
	// LoadWithMigrations migrates the raw document, then strictly decodes and
	// validates the current schema.
	cfg, err := config.LoadWithMigrations(*configPath, config.DefaultMigrations())
	if err != nil {
		return deps.errorf("%v", err)
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(*configPath); err == nil {
		mode = info.Mode().Perm()
	}
	if cfg.Version != previous {
		if err := config.SaveWithMode(*configPath, cfg, mode); err != nil {
			return deps.errorf("%v", err)
		}
	}
	// CompatibleDown is the floor (lowest schema whose binaries can use the
	// migrated data). No breaking configuration migration exists yet, so the
	// configuration floor stays at the schema read before migrating; a breaking
	// migration to version N must raise it to N.
	configReport := migrateDetail{Schema: cfg.Version, CompatibleDown: previous}
	report := migrateReport{
		Schema:         configReport.Schema,
		CompatibleDown: configReport.CompatibleDown,
		Config:         configReport,
	}

	// Open the hub database through the storage layer, which applies its
	// migrations. Only an existing database is opened: a fresh hub has no file
	// yet and must not gain one from a migration probe.
	dbPath := migrateDatabasePath(paths.Scope, stateRoot)
	if info, statErr := os.Stat(dbPath); statErr == nil {
		if !info.Mode().IsRegular() {
			return deps.errorf("hub database %s is not a regular file", dbPath)
		}
		database, openErr := storage.Open(ctx, dbPath, storage.Options{})
		if openErr != nil {
			return deps.errorf("migrate hub database: %v", openErr)
		}
		schema := database.Schema()
		closeErr := database.Close()
		if closeErr != nil {
			return deps.errorf("close hub database: %v", closeErr)
		}
		dbReport := migrateDetail{Schema: schema.Version, CompatibleDown: schema.CompatibleDown}
		report.Database = &dbReport
		// The database advances independently of configuration. The transaction
		// compares the previous binary's actual schema, so the reported floor is
		// the larger of the two stores.
		report.Schema = dbReport.Schema
		if dbReport.CompatibleDown > report.CompatibleDown {
			report.CompatibleDown = dbReport.CompatibleDown
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return deps.errorf("inspect hub database: %v", statErr)
	}

	encoder := json.NewEncoder(deps.Stdout)
	if err := encoder.Encode(report); err != nil {
		return deps.errorf("encode migrate report: %v", err)
	}
	return 0
}

// migrateDetail is the per-store part of the migrate report.
type migrateDetail struct {
	Schema         int `json:"schema"`
	CompatibleDown int `json:"compatibleDown"`
}

// migrateReport is the JSON contract printed by "rigscope migrate". The
// top-level fields are what the coordinator consumes; Config and Database
// expose each store for diagnostics.
type migrateReport struct {
	Schema         int           `json:"schema"`
	CompatibleDown int           `json:"compatibleDown"`
	Config         migrateDetail `json:"config"`
	// Database is omitted for a fresh hub that has no database file yet.
	Database *migrateDetail `json:"database,omitempty"`
}

// migrateDatabasePath locates the hub database below a state root exactly as
// the hub does: config.Paths.ComponentStateDir("hub") joined with
// hubDatabaseFile (user installs keep it in the state directory, system
// installs in the hub account's private directory).
func migrateDatabasePath(scope config.Scope, stateRoot string) string {
	paths := config.Paths{Scope: scope, StateDir: stateRoot}
	return filepath.Join(paths.ComponentStateDir(systemd.Hub.String()), hubDatabaseFile)
}

// migrationLock uses the parent's inherited descriptor when one was supplied,
// and otherwise acquires the lock normally. A malformed inherited value is an
// error; silently falling back would let a caller claim the lock is held.
func migrationLock(paths config.Paths, value string) (*lock.Lock, error) {
	if value == "" {
		return acquireLock(paths)
	}
	fd, err := strconv.Atoi(value)
	if err != nil {
		return nil, fmt.Errorf("invalid inherited installation lock descriptor %q", value)
	}
	return lock.AcquireInherited(paths.LockFile(), fd)
}
