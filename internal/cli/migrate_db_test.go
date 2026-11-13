package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/activation"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/storage"

	_ "modernc.org/sqlite"
)

func TestRollbackComparesThePreviousDatabaseSchema(t *testing.T) {
	cases := []struct {
		name       string
		floor      int
		wantExit   int
		wantBinary string
	}{
		{name: "breaking migration refuses rollback", floor: 5, wantExit: 1},
		{name: "additive migration allows rollback", floor: 4, wantExit: 0, wantBinary: "binary-v1"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t)
			paths := f.install(t)
			// The previous release left the database at schema 4 while the
			// configuration was still at version 1.
			seedDatabaseAtSchema(t, filepath.Join(paths.StateDir, "hub.db"), 4)
			next := upgradeScriptWithMigration("1.1.0", 5, testCase.floor)
			release := newReleaseFixture(t, "1.1.0", next, false)
			f.useRelease(release)
			f.out.Reset()
			if code := f.run(t, "upgrade"); code != 0 {
				t.Fatalf("upgrade exit %d: %s", code, f.err.String())
			}
			journal, err := (activation.JournalStore{
				Path: filepath.Join(paths.StateDir, "upgrade-journal.json"),
				FS:   activation.OSFS{},
			}).Load()
			if err != nil {
				t.Fatal(err)
			}
			if journal.PreviousSchema != 4 {
				t.Fatalf("PreviousSchema = %d, want the database ledger version 4", journal.PreviousSchema)
			}
			f.err.Reset()
			code := f.run(t, "rollback")
			if code != testCase.wantExit {
				t.Fatalf("rollback exit = %d, want %d: %s", code, testCase.wantExit, f.err.String())
			}
			if testCase.wantExit != 0 {
				if !strings.Contains(f.err.String(), "not allowed") {
					t.Fatalf("stderr = %s", f.err.String())
				}
				checkContent(t, paths.Binary, string(next))
				return
			}
			checkContent(t, paths.Binary, testCase.wantBinary)
		})
	}
}

// seedDatabaseAtSchema writes a hub database whose migration ledger stops at
// version. It is enough for the upgrade transaction, which only reads the
// ledger before the staged binary migrates it.
func seedDatabaseAtSchema(t *testing.T, path string, version int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		compatible_down INTEGER NOT NULL,
		applied_at_ms INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	ledger := storage.MigrationLedger()
	if version > len(ledger) {
		t.Fatalf("schema version %d exceeds the migration ledger", version)
	}
	for _, migration := range ledger[:version] {
		if _, err := database.Exec(
			"INSERT INTO schema_migrations VALUES (?, ?, ?, 0)",
			migration.Version, migration.Name, migration.CompatibleDown,
		); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateCommandMigratesAndReportsTheDatabase(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	dbPath := filepath.Join(paths.StateDir, "hub.db")
	database, err := storage.Open(context.Background(), dbPath, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if code := f.run(t, "migrate", "--config", paths.ConfigFile, "--state", paths.StateDir); code != 0 {
		t.Fatalf("migrate exit %d: %s", code, f.err.String())
	}
	var report struct {
		Schema         int `json:"schema"`
		CompatibleDown int `json:"compatibleDown"`
		Config         struct {
			Schema         int `json:"schema"`
			CompatibleDown int `json:"compatibleDown"`
		} `json:"config"`
		Database *struct {
			Schema         int `json:"schema"`
			CompatibleDown int `json:"compatibleDown"`
		} `json:"database"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &report); err != nil {
		t.Fatalf("json: %v\n%s", err, f.out.String())
	}
	latest := storage.LatestSchemaVersion()
	if report.Database == nil {
		t.Fatalf("report has no database detail: %s", f.out.String())
	}
	if report.Database.Schema != latest || report.Schema != latest {
		t.Fatalf("database schema = %d / top-level %d, want %d", report.Database.Schema, report.Schema, latest)
	}
	if report.Config.Schema != config.CurrentVersion {
		t.Fatalf("config schema = %d", report.Config.Schema)
	}
	wantFloor := report.Config.CompatibleDown
	if report.Database.CompatibleDown > wantFloor {
		wantFloor = report.Database.CompatibleDown
	}
	if report.CompatibleDown != wantFloor {
		t.Fatalf("compatibleDown = %d, want max(config, database) = %d", report.CompatibleDown, wantFloor)
	}
}
