package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// privateTempDir is a temporary directory with the 0700 mode the hub database
// directory policy requires. t.TempDir honours the process umask and may be
// group-writable in some environments.
func privateTempDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod temp dir: %v", err)
	}
	return dir
}

func openTestDatabase(t *testing.T) (*Database, string) {
	t.Helper()
	dir := privateTempDir(t)
	path := filepath.Join(dir, "hub.db")
	db, err := Open(testContext(t), path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func rawDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

func TestOpenFreshDatabase(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "nested", "hub.db")
	ctx := testContext(t)
	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if got := db.SchemaVersion(); got != LatestSchemaVersion() {
		t.Fatalf("schema version = %d, want %d", got, LatestSchemaVersion())
	}
	want := []struct {
		version int
		name    string
		down    int
	}{
		{1, "foundation", 1},
		{2, "collector_enrollment", 1},
		{3, "alerts", 1},
		{4, "telemetry", 1},
		{5, "collector_credential_hashing", 5},
		{6, "account_version", 5},
		{7, "pairing_grant_binding", 5},
	}
	info := db.Schema()
	if len(info.Applied) != len(want) {
		t.Fatalf("applied migrations = %d, want %d", len(info.Applied), len(want))
	}
	for i, expect := range want {
		got := info.Applied[i]
		if got.Version != expect.version || got.Name != expect.name || got.CompatibleDown != expect.down {
			t.Fatalf("migration %d = %+v, want version %d name %q down %d", i, got, expect.version, expect.name, expect.down)
		}
	}
	// The floor is the maximum compatible_down over the applied ledger. The
	// credential-hashing migration is breaking, so the floor is 5.
	if info.CompatibleDown != 5 {
		t.Fatalf("CompatibleDown = %d, want 5", info.CompatibleDown)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %o, want 600", fileInfo.Mode().Perm())
	}
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if parentInfo.Mode().Perm() != 0o700 {
		t.Fatalf("parent mode = %o, want 700", parentInfo.Mode().Perm())
	}

	// Reopening must not rerun a migration or change the recorded rows.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got := reopened.SchemaVersion(); got != LatestSchemaVersion() {
		t.Fatalf("reopened schema version = %d", got)
	}
	var count int
	if err := reopened.QueryRow(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != len(want) {
		t.Fatalf("schema_migrations rows = %d, want %d", count, len(want))
	}
}

func TestFreshDatabaseEnablesIncrementalVacuum(t *testing.T) {
	db, _ := openTestDatabase(t)
	ctx := testContext(t)
	if got := db.pragmaInt64(ctx, "auto_vacuum", -1); got != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (INCREMENTAL) on a fresh database", got)
	}
	if status := db.PhysicalLimit(); status != nil {
		t.Fatalf("unexpected physical limit: %+v", status)
	}
	// The pragma survives a reopen because it lives in the file header.
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got := reopened.pragmaInt64(ctx, "auto_vacuum", -1); got != 2 {
		t.Fatalf("auto_vacuum after reopen = %d, want 2", got)
	}
	if _, err := reopened.SetPhysicalLimit(16 * 1024 * 1024); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	if status := reopened.PhysicalLimit(); status == nil || status.AutoVacuum != 2 {
		t.Fatalf("physical status = %+v", status)
	}
}

func TestOpenMemoryDatabase(t *testing.T) {
	ctx := testContext(t)
	db, err := Open(ctx, ":memory:", Options{})
	if err != nil {
		t.Fatalf("open memory: %v", err)
	}
	defer db.Close()
	if got := db.SchemaVersion(); got != LatestSchemaVersion() {
		t.Fatalf("schema version = %d", got)
	}
	if db.SizeBytes() <= 0 {
		t.Fatalf("memory database reports no size")
	}
	if err := db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO settings (key, value, updated_at_ms) VALUES ('a', 'b', 1)")
		return err
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var value string
	if err := db.QueryRow(ctx, "SELECT value FROM settings WHERE key = 'a'").Scan(&value); err != nil {
		t.Fatalf("read from pool: %v", err)
	}
	if value != "b" {
		t.Fatalf("value = %q", value)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "future.db")
	db, err := Open(testContext(t), path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw := rawDatabase(t, path)
	latest := LatestSchemaVersion()
	if _, err := raw.Exec("INSERT INTO schema_migrations VALUES (?, 'from-the-future', ?, 0)", latest+1, latest+1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = raw.Close()
	_, err = Open(testContext(t), path, Options{})
	if err == nil {
		t.Fatal("expected a newer-schema failure")
	}
	if !contains(err.Error(), "requires a hub that writes at least schema version") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenAllowsAFutureAdditiveMigration(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "future-additive.db")
	ctx := testContext(t)
	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	latest := LatestSchemaVersion()
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw := rawDatabase(t, path)
	if _, err := raw.Exec(
		"INSERT INTO schema_migrations VALUES (?, 'future-additive', ?, 0)", latest+1, latest,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = raw.Close()

	// The unknown migration is additive, so this binary may still operate on
	// the schema subset it knows.
	reopened, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("additive future open: %v", err)
	}
	defer reopened.Close()
	info := reopened.Schema()
	if info.Version != latest+1 || info.CompatibleDown != latest {
		t.Fatalf("schema = %+v, want version %d floor %d", info, latest+1, latest)
	}
	if err := reopened.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO settings (key, value, updated_at_ms) VALUES ('future', '1', 1)")
		return err
	}); err != nil {
		t.Fatalf("write on known subset: %v", err)
	}
}

func TestOpenRefusesNonContiguousOrRenamedHistory(t *testing.T) {
	dir := privateTempDir(t)
	ctx := testContext(t)

	holePath := filepath.Join(dir, "hole.db")
	raw := rawDatabase(t, holePath)
	_, err := raw.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, compatible_down INTEGER NOT NULL, applied_at_ms INTEGER NOT NULL
	);
	INSERT INTO schema_migrations VALUES (1, 'foundation', 1, 0);
	INSERT INTO schema_migrations VALUES (3, 'alerts', 2, 0);`)
	if err != nil {
		t.Fatalf("seed hole: %v", err)
	}
	_ = raw.Close()
	if err := os.Chmod(holePath, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := Open(ctx, holePath, Options{}); err == nil || !contains(err.Error(), "not contiguous") {
		t.Fatalf("hole error = %v", err)
	}

	renamedPath := filepath.Join(dir, "renamed.db")
	raw = rawDatabase(t, renamedPath)
	_, err = raw.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, compatible_down INTEGER NOT NULL, applied_at_ms INTEGER NOT NULL
	);
	INSERT INTO schema_migrations VALUES (1, 'something-else', 1, 0);`)
	if err != nil {
		t.Fatalf("seed renamed: %v", err)
	}
	_ = raw.Close()
	if err := os.Chmod(renamedPath, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := Open(ctx, renamedPath, Options{}); err == nil || !contains(err.Error(), "named") {
		t.Fatalf("renamed error = %v", err)
	}
}

func TestOpenUpgradesAnOlderLedger(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "older.db")
	raw := rawDatabase(t, path)
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, compatible_down INTEGER NOT NULL, applied_at_ms INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	for _, statement := range migrations[0].Statements {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("apply baseline: %v", err)
		}
	}
	if _, err := raw.Exec("INSERT INTO schema_migrations VALUES (1, 'foundation', 1, 0)"); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	if _, err := raw.Exec("INSERT INTO users (username, password_hash, scope_json, is_admin, disabled, created_at_ms, updated_at_ms) VALUES ('owner', 'hash', '*', 1, 0, 1, 1)"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	_ = raw.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	db, err := Open(testContext(t), path, Options{})
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer db.Close()
	if got := db.SchemaVersion(); got != LatestSchemaVersion() {
		t.Fatalf("schema version = %d", got)
	}
	var username string
	if err := db.QueryRow(testContext(t), "SELECT username FROM users").Scan(&username); err != nil {
		t.Fatalf("read preserved user: %v", err)
	}
	if username != "owner" {
		t.Fatalf("username = %q", username)
	}
}

// TestMigrationSixAdoptsOnlyMatchingAccountSessions starts from schema 5, seeds
// sessions whose stored snapshots differ from the account's current privileges,
// and upgrades. Only an exact enabled snapshot may adopt the account's version
// and incarnation; stale, disabled and orphaned rows must be deleted.
func TestMigrationSixAdoptsOnlyMatchingAccountSessions(t *testing.T) {
	ctx := testContext(t)
	dir := privateTempDir(t)
	path := filepath.Join(dir, "adopt-sessions.db")
	saved := migrations
	defer func() { migrations = saved }()

	migrations = saved[:5]
	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("open schema 5: %v", err)
	}
	now := time.Now().UTC()
	expires := now.Add(time.Hour).UnixMilli()
	if err := db.Write(ctx, func(tx *Tx) error {
		for _, user := range []struct {
			username string
			scope    string
			admin    int64
			disabled int64
		}{
			{"kept", `["m-2"]`, 0, 0},
			{"changed", `["m-2"]`, 0, 0},
			{"disabled", "*", 0, 1},
		} {
			if _, err := tx.Exec(
				"INSERT INTO users (username, password_hash, scope_json, is_admin, disabled, created_at_ms, updated_at_ms) VALUES (?, 'hash', ?, ?, ?, ?, ?)",
				user.username, user.scope, user.admin, user.disabled, now.UnixMilli(), now.UnixMilli(),
			); err != nil {
				return err
			}
		}
		for _, session := range []struct {
			tokenHash string
			username  string
			scope     string
			admin     int64
		}{
			{"s-kept", "kept", `["m-2"]`, 0},
			{"s-wild", "changed", "*", 1},
			{"s-disabled", "disabled", "*", 0},
			{"s-orphan", "ghost", "*", 0},
		} {
			if _, err := tx.Exec(
				"INSERT INTO browser_sessions (token_hash, username, csrf_token, scope_json, is_admin, created_at_ms, expires_at_ms) VALUES (?, ?, 'csrf', ?, ?, ?, ?)",
				session.tokenHash, session.username, session.scope, session.admin, now.UnixMilli(), expires,
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed schema 5: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close schema 5: %v", err)
	}

	migrations = saved
	upgraded, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer upgraded.Close()

	var version int64
	var incarnation string
	if err := upgraded.QueryRow(ctx, "SELECT version, incarnation FROM users WHERE username = 'changed'").Scan(&version, &incarnation); err != nil {
		t.Fatalf("read changed account: %v", err)
	}
	if version < 1 || incarnation == "" {
		t.Fatalf("migrated account = version %d incarnation %q", version, incarnation)
	}
	var (
		sessionVersion     int64
		sessionIncarnation string
	)
	if err := upgraded.QueryRow(ctx, "SELECT user_version, user_incarnation FROM browser_sessions WHERE token_hash = 's-kept'").
		Scan(&sessionVersion, &sessionIncarnation); err != nil {
		t.Fatalf("read kept session: %v", err)
	}
	var keptVersion int64
	var keptIncarnation string
	if err := upgraded.QueryRow(ctx, "SELECT version, incarnation FROM users WHERE username = 'kept'").Scan(&keptVersion, &keptIncarnation); err != nil {
		t.Fatalf("read kept account: %v", err)
	}
	if sessionVersion != keptVersion || sessionIncarnation != keptIncarnation {
		t.Fatalf("kept session adopted version %d incarnation %q, account has %d %q",
			sessionVersion, sessionIncarnation, keptVersion, keptIncarnation)
	}
	var count int
	if err := upgraded.QueryRow(ctx, "SELECT COUNT(*) FROM browser_sessions").Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 1 {
		t.Fatalf("adopted sessions = %d, want only the exact snapshot", count)
	}
	sessions := NewSessions(upgraded)
	if _, err := sessions.Find(ctx, "s-kept"); err != nil {
		t.Fatalf("exact snapshot session was not adopted: %v", err)
	}
	if _, err := sessions.Find(ctx, "s-wild"); CodeOf(err) != CodeNotFound {
		t.Fatalf("stale wildcard session survived upgrade: %v", err)
	}
}

// TestOpenRepairsEmptyAccountIncarnations simulates a schema-5 writer that
// legally wrote this migrated file after a rollback. Its account rows carry the
// migration-6 default empty incarnation and its sessions the same default. A
// reopen must assign a fresh incarnation and drop the unbound sessions before
// any login can compare an empty snapshot.
func TestOpenRepairsEmptyAccountIncarnations(t *testing.T) {
	ctx := testContext(t)
	dir := privateTempDir(t)
	path := filepath.Join(dir, "rolled-back.db")

	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().UTC()
	if err := db.Write(ctx, func(tx *Tx) error {
		if _, err := tx.Exec(
			"INSERT INTO users (username, password_hash, scope_json, is_admin, disabled, created_at_ms, updated_at_ms) "+
				"VALUES ('rolled-back', 'hash', '*', 0, 0, ?, ?)",
			now.UnixMilli(), now.UnixMilli(),
		); err != nil {
			return err
		}
		// The schema-5 writer knows neither user_version nor user_incarnation;
		// both take the migration default.
		_, err := tx.Exec(
			"INSERT INTO browser_sessions (token_hash, username, csrf_token, scope_json, is_admin, created_at_ms, expires_at_ms) "+
				"VALUES ('schema-5-session', 'rolled-back', 'csrf', '*', 0, ?, ?)",
			now.UnixMilli(), now.Add(time.Hour).UnixMilli(),
		)
		return err
	}); err != nil {
		t.Fatalf("simulate schema-5 write: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	var incarnation string
	if err := reopened.QueryRow(ctx, "SELECT incarnation FROM users WHERE username = 'rolled-back'").Scan(&incarnation); err != nil {
		t.Fatalf("read repaired account: %v", err)
	}
	if incarnation == "" {
		t.Fatal("reopen left an account with an empty incarnation")
	}
	var sessions int
	if err := reopened.QueryRow(ctx, "SELECT COUNT(*) FROM browser_sessions").Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Fatalf("unbound sessions survived repair: %d", sessions)
	}
	if _, err := NewSessions(reopened).Find(ctx, "schema-5-session"); CodeOf(err) != CodeNotFound {
		t.Fatalf("schema-5 session lookup = %v, want not found", err)
	}
}

func TestPreviousBinaryRunsAnAdditiveMigration(t *testing.T) {
	ctx := testContext(t)
	dir := privateTempDir(t)
	path := filepath.Join(dir, "additive.db")
	saved := migrations
	defer func() { migrations = saved }()

	// The newer binary migrates to N and accepts writes.
	migrations = saved[:4]
	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("open newer: %v", err)
	}
	store, err := NewTelemetry(ctx, db, TelemetryOptions{
		FlushInterval:    time.Millisecond,
		MaxPhysicalBytes: negativeInt64(),
	})
	if err != nil {
		t.Fatalf("telemetry: %v", err)
	}
	now := time.Now().UTC()
	result, err := store.Enqueue(ctx, liveBatch(makeMetricsRecord(recordOptions{
		sequence: 1, capturedAt: now.UnixMilli(), payload: payloadWithUsage(42),
	})))
	if err != nil || len(result.Accepted) != 1 {
		t.Fatalf("accepted write = %+v, %v", result, err)
	}
	if err := store.Close(ctx); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// The previous binary knows only N-1 migrations. Migration N was additive
	// (it keeps the floor of 1), so it may open the file and keep writing the
	// subset it knows without running the unknown migration.
	migrations = saved[:3]
	previous, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("previous binary open: %v", err)
	}
	defer previous.Close()
	info := previous.Schema()
	if info.Version != 4 || info.CompatibleDown != 1 {
		t.Fatalf("previous binary schema = %+v, want version 4 floor 1", info)
	}
	if err := previous.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO settings (key, value, updated_at_ms) VALUES ('previous', 'ok', 1)")
		return err
	}); err != nil {
		t.Fatalf("previous binary write: %v", err)
	}
	if got := scalar(t, previous, "SELECT COUNT(*) FROM sample_receipts"); got != 1 {
		t.Fatalf("accepted history = %d rows", got)
	}
}

func TestPreviousBinaryRefusesABreakingMigration(t *testing.T) {
	ctx := testContext(t)
	dir := privateTempDir(t)
	path := filepath.Join(dir, "breaking.db")
	saved := migrations
	defer func() { migrations = saved }()

	next := len(saved) + 1
	withBreaking := append([]Migration(nil), saved...)
	withBreaking = append(withBreaking, Migration{
		Version: next, Name: "breaking-simulation", CompatibleDown: next,
		Statements: []string{"CREATE TABLE breaking_simulation (id INTEGER PRIMARY KEY)"},
	})
	migrations = withBreaking
	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("open breaking: %v", err)
	}
	if info := db.Schema(); info.CompatibleDown != next {
		t.Fatalf("breaking floor = %d, want %d", info.CompatibleDown, next)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	migrations = saved
	_, err = Open(ctx, path, Options{})
	if err == nil {
		t.Fatal("expected the previous binary to refuse a breaking migration")
	}
	if !contains(err.Error(), "requires a hub that writes at least schema version") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenFailsClosedOnUnsafePaths(t *testing.T) {
	dir := privateTempDir(t)
	ctx := testContext(t)

	loose := filepath.Join(dir, "loose.db")
	if err := os.WriteFile(loose, []byte("not a database"), 0o644); err != nil {
		t.Fatalf("write loose: %v", err)
	}
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatalf("chmod loose: %v", err)
	}
	if _, err := Open(ctx, loose, Options{}); err == nil || !contains(err.Error(), "must be private") {
		t.Fatalf("loose error = %v", err)
	}

	directoryPath := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := Open(ctx, directoryPath, Options{}); err == nil || !contains(err.Error(), "regular file") {
		t.Fatalf("directory error = %v", err)
	}

	// A 0600 file in a group- or world-accessible directory can still be
	// renamed or replaced by another local account.
	looseDir := filepath.Join(dir, "loose-dir")
	if err := os.Mkdir(looseDir, 0o700); err != nil {
		t.Fatalf("mkdir loose: %v", err)
	}
	if err := os.Chmod(looseDir, 0o755); err != nil {
		t.Fatalf("chmod loose: %v", err)
	}
	if _, err := Open(ctx, filepath.Join(looseDir, "hub.db"), Options{}); err == nil || !contains(err.Error(), "private (0700)") {
		t.Fatalf("loose directory error = %v", err)
	}
}

func TestBackupIsConsistentPrivateAndReplaceable(t *testing.T) {
	ctx := testContext(t)
	db, path := openTestDatabase(t)
	backupPath := filepath.Join(t.TempDir(), "backups", "copy.db")
	if err := db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES ('machine-1', 'Workstation', '[\"desk\"]', 123)")
		return err
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if db.SizeBytes() <= 0 {
		t.Fatal("database reports no size")
	}
	if err := db.Backup(ctx, backupPath); err != nil {
		t.Fatalf("backup: %v", err)
	}
	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %o", info.Mode().Perm())
	}
	copyDB := rawDatabase(t, backupPath)
	var name string
	if err := copyDB.QueryRow("SELECT display_name FROM machine_meta WHERE machine_id = 'machine-1'").Scan(&name); err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if name != "Workstation" {
		t.Fatalf("backup name = %q", name)
	}
	_ = copyDB.Close()

	// A later backup replaces the previous copy.
	if err := db.Backup(ctx, backupPath); err != nil {
		t.Fatalf("replace backup: %v", err)
	}

	// Aliases of the live file are refused: syntactic, hard link and symlink.
	if err := db.Backup(ctx, filepath.Join(filepath.Dir(path), ".", "hub.db")); err == nil || !contains(err.Error(), "aliases") {
		t.Fatalf("syntactic alias error = %v", err)
	}
	hardLink := filepath.Join(filepath.Dir(path), "hard-link.db")
	if err := os.Link(path, hardLink); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	if err := db.Backup(ctx, hardLink); err == nil || !contains(err.Error(), "aliases") {
		t.Fatalf("hard link error = %v", err)
	}
	symlink := filepath.Join(filepath.Dir(path), "symlink.db")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := db.Backup(ctx, symlink); err == nil || !contains(err.Error(), "regular file") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestBackupRefusesSidecarsAndIsPrivateUnderAnyUmask(t *testing.T) {
	ctx := testContext(t)
	db, path := openTestDatabase(t)
	if err := db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO settings (key, value, updated_at_ms) VALUES ('backup', 'live', 1)")
		return err
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Force a WAL and SHM into existence so hard-link aliases are real files.
	var value string
	if err := db.QueryRow(ctx, "SELECT value FROM settings WHERE key = 'backup'").Scan(&value); err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, sidecar := range []string{"-wal", "-shm", "-journal"} {
		destination := path + sidecar
		if err := db.Backup(ctx, destination); err == nil || !contains(err.Error(), "aliases") {
			t.Fatalf("%s alias error = %v", sidecar, err)
		}
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		hardLink := filepath.Join(filepath.Dir(path), "wal-hard-link")
		if err := os.Link(path+"-wal", hardLink); err != nil {
			t.Fatalf("hard link WAL: %v", err)
		}
		if err := db.Backup(ctx, hardLink); err == nil || !contains(err.Error(), "aliases") {
			t.Fatalf("hard-linked WAL error = %v", err)
		}
	}
	symlink := filepath.Join(filepath.Dir(path), "wal-symlink")
	if err := os.Symlink(path+"-wal", symlink); err != nil {
		t.Fatalf("symlink WAL: %v", err)
	}
	if err := db.Backup(ctx, symlink); err == nil || !contains(err.Error(), "regular file") {
		t.Fatalf("symlinked WAL error = %v", err)
	}

	// The output is private even with a fully permissive umask, and the
	// temporary workspace is removed.
	previous := syscall.Umask(0)
	backupPath := filepath.Join(t.TempDir(), "private.db")
	err := db.Backup(ctx, backupPath)
	syscall.Umask(previous)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".rigscope-backup-") {
			t.Fatalf("backup workspace left behind: %s", entry.Name())
		}
	}
}

func TestWriteAdmissionReturnsTypedStoragePressure(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	if err := db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO settings (key, value, updated_at_ms) VALUES ('seed', '1', 1)")
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A page cap below the current file size leaves no room for a new page.
	if _, err := db.SetPhysicalLimit(16 * 1024); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	err := db.Write(ctx, func(tx *Tx) error {
		_, execErr := tx.Exec("INSERT INTO settings (key, value, updated_at_ms) VALUES ('blocked', '1', 1)")
		return execErr
	})
	if CodeOf(err) != CodeStoragePressure {
		t.Fatalf("write error = %v (code %q), want storage-pressure", err, CodeOf(err))
	}
	if !IsBusy(err) {
		t.Fatal("storage pressure must be a retryable busy condition")
	}
	if got := scalar(t, db, "SELECT COUNT(*) FROM settings WHERE key = 'blocked'"); got != 0 {
		t.Fatalf("pressure write inserted %d rows", got)
	}
	// Maintenance writes (retention) must still be able to make room.
	if err := db.writeMaintenance(ctx, func(tx *Tx) error {
		_, execErr := tx.Exec("DELETE FROM settings WHERE key = 'seed'")
		return execErr
	}); err != nil {
		t.Fatalf("maintenance write under pressure: %v", err)
	}
}

func TestReadsAreBoundedByReadTimeout(t *testing.T) {
	ctx := testContext(t)
	dir := privateTempDir(t)
	path := filepath.Join(dir, "bounded-read.db")
	db, err := Open(ctx, path, Options{ReadTimeout: 25 * time.Millisecond})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	started := time.Now()
	rows, err := db.Query(ctx,
		"WITH RECURSIVE counter(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM counter WHERE x < 1000000000) "+
			"SELECT COUNT(*) FROM counter",
	)
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("bounded read error = %v, want a deadline/interrupt error", err)
		}
	} else {
		for rows.Next() {
		}
		err = rows.Err()
		_ = rows.Close()
		if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("bounded read error = %v, want a deadline/interrupt error", err)
		}
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("bounded read took %v", elapsed)
	}
}

func TestSecondHandleReportsBusyUnderAHeldWrite(t *testing.T) {
	ctx := testContext(t)
	_, path := openTestDatabase(t)
	other, err := Open(ctx, path, Options{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	defer other.Close()

	first, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("open first handle: %v", err)
	}
	defer first.Close()

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- first.Write(ctx, func(tx *Tx) error {
			_, err := tx.Exec("INSERT INTO settings (key, value, updated_at_ms) VALUES ('held', '1', 1)")
			if err != nil {
				return err
			}
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	err = other.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO settings (key, value, updated_at_ms) VALUES ('blocked', '1', 1)")
		return err
	})
	close(release)
	if werr := <-done; werr != nil {
		t.Fatalf("held write: %v", werr)
	}
	if CodeOf(err) != CodeBusy {
		t.Fatalf("second write error = %v (code %q), want busy", err, CodeOf(err))
	}
}

func TestPhysicalLimitReportsPressure(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	status, err := db.SetPhysicalLimit(16 * 1024 * 1024)
	if err != nil {
		t.Fatalf("set physical limit: %v", err)
	}
	if status.LimitBytes != 16*1024*1024 {
		t.Fatalf("limit = %d", status.LimitBytes)
	}
	if status.MaxPageCount <= 0 || status.PageSize <= 0 {
		t.Fatalf("status = %+v", status)
	}
	if status.Pressure {
		t.Fatalf("a fresh database must not report pressure: %+v", status)
	}
	if db.PhysicalLimit() == nil {
		t.Fatal("PhysicalLimit returned nil after configuration")
	}
	if _, err := db.SetPhysicalLimit(1024); CodeOf(err) != CodeInvalid {
		t.Fatalf("tiny limit error = %v", err)
	}
	// A page cap below the current file size reports pressure without deleting
	// rows or failing to open.
	err = db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO settings (key, value, updated_at_ms) VALUES ('x', 'y', 1)")
		return err
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestZeroSchemaVersionIsZero(t *testing.T) {
	var db Database
	if db.SchemaVersion() != 0 {
		t.Fatal("zero database should report schema 0")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestStorageErrorClassification(t *testing.T) {
	_ = errors.Is(context.Canceled, context.Canceled)
	if CodeOf(invalidError("x")) != CodeInvalid {
		t.Fatal("invalidError code")
	}
	if !IsStorageFull(&Error{Code: CodeStorageFull}) {
		t.Fatal("IsStorageFull")
	}
	if !IsBusy(&Error{Code: CodeBusy}) {
		t.Fatal("IsBusy")
	}
	if RetryAfter(&Error{Code: CodeBusy, RetryAfter: time.Second}) != time.Second {
		t.Fatal("RetryAfter")
	}
}

func TestPhysicalPageCapSurfacesStorageFull(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	baseline := db.SizeBytes()
	limit := baseline + 64*1024
	if _, err := db.SetPhysicalLimit(limit); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	large := make([]byte, 512*1024)
	for index := range large {
		large[index] = 'x'
	}
	err := db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO settings (key, value, updated_at_ms) VALUES ('too-large', ?, 1)", string(large))
		return err
	})
	if err == nil {
		t.Fatal("oversized write unexpectedly succeeded")
	}
	if !IsStorageFull(err) {
		t.Fatalf("error = %v (code %q), want storage-full", err, CodeOf(err))
	}
}
