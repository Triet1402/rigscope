package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sqlite "modernc.org/sqlite"
)

const (
	// DefaultMaxDatabaseBytes is the finite default budget for the main
	// database file plus its WAL.
	DefaultMaxDatabaseBytes int64 = 2 * 1024 * 1024 * 1024
	// WALReserveBytes caps the write-ahead log reserve inside that budget.
	WALReserveBytes int64 = 128 * 1024 * 1024

	defaultBusyTimeout = 5 * time.Second
	defaultReadConns   = 4
	defaultReadTimeout = 30 * time.Second
	fileMode           = 0o600
	dirMode            = 0o700
)

// Options configures one hub database handle.
type Options struct {
	// BusyTimeout bounds how long a statement waits for a lock. Default 5s.
	BusyTimeout time.Duration
	// ReadConnections is the size of the read-only pool. Default 4.
	ReadConnections int
	// ReadTimeout bounds one read statement or row iteration. A read
	// transaction that never finishes pins WAL frames and blocks checkpointing,
	// so queries get a deadline even when the caller passes Background.
	// Default 30s.
	ReadTimeout time.Duration
	// Now is the clock used for migration timestamps. Defaults to time.Now.
	Now func() time.Time
}

// Database is one open hub database: a single serialized writer and a
// read-only pool over the same file.
type Database struct {
	path        string
	now         func() time.Time
	readTimeout time.Duration

	write *sql.DB
	read  *sql.DB

	writeMu sync.Mutex
	closed  atomic.Bool

	physicalMu sync.Mutex
	physical   *PhysicalStatus

	schema SchemaInfo
}

var memoryDatabaseCounter atomic.Uint64

// Open opens (and if needed creates and migrates) a hub database.
//
// The parent directory is created 0700 and the database file 0600. An existing
// path must already be a regular private file, otherwise Open fails before
// reading it. ":memory:" is accepted for tests; it uses a shared-cache
// in-memory database so the read pool sees the writer's rows.
func Open(ctx context.Context, path string, opts Options) (*Database, error) {
	if path == "" || len(path) > 4096 {
		return nil, invalidError("hub database path must be nonempty and at most 4096 bytes")
	}
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = defaultBusyTimeout
	}
	if opts.ReadConnections <= 0 {
		opts.ReadConnections = defaultReadConns
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = defaultReadTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	memory := path == ":memory:"
	dsnPath := path
	if memory {
		dsnPath = fmt.Sprintf("rigscope-memory-%d-%d", os.Getpid(), memoryDatabaseCounter.Add(1))
	} else {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, wrapStorageError(CodeInvalid, err, "resolve hub database path")
		}
		if err := preparePrivateFile(abs); err != nil {
			return nil, err
		}
		dsnPath = abs
		path = abs
	}

	writeDSN, err := databaseDSN(dsnPath, false, memory, opts.BusyTimeout)
	if err != nil {
		return nil, err
	}
	writeDB, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, wrapStorageError(CodeUnavailable, err, "open hub database")
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)

	db := &Database{path: path, now: opts.Now, readTimeout: opts.ReadTimeout, write: writeDB}
	cleanup := func() {
		_ = writeDB.Close()
	}
	if err := writeDB.PingContext(ctx); err != nil {
		cleanup()
		return nil, wrapStorageError(CodeUnavailable, mapError(err), "open hub database")
	}

	// A new database can return freed pages incrementally. auto_vacuum is a
	// file-level setting that must be chosen before the first page is written,
	// so it runs before journal_mode (which writes page 1) and before any
	// migration. On an existing database whose auto_vacuum is already NONE this
	// is a no-op (changing it would require a full VACUUM, which startup never
	// runs).
	if _, err := writeDB.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		cleanup()
		return nil, wrapStorageError(CodeUnavailable, mapError(err), "configure hub database")
	}
	if _, err := writeDB.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		cleanup()
		return nil, wrapStorageError(CodeUnavailable, mapError(err), "enable WAL")
	}
	// synchronous=FULL makes a WAL COMMIT durable before it returns. The hub
	// acknowledges samples only after COMMIT, so this is part of the delivery
	// contract, not a performance preference.
	if _, err := writeDB.ExecContext(ctx, "PRAGMA synchronous = FULL"); err != nil {
		cleanup()
		return nil, wrapStorageError(CodeUnavailable, mapError(err), "configure synchronous mode")
	}
	if !memory {
		if err := normalizeSidecarModes(path); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := db.migrate(ctx); err != nil {
		cleanup()
		return nil, err
	}
	// A schema-5 binary may legitimately write this file after a rollback
	// (compatible_down is 5). It creates accounts with the migration-6 default
	// empty incarnation, which the session insert must never treat as a
	// wildcard. Repair those rows before serving authentication.
	if err := db.repairAccountIncarnations(ctx); err != nil {
		cleanup()
		return nil, err
	}
	if !memory {
		if err := normalizeSidecarModes(path); err != nil {
			cleanup()
			return nil, err
		}
	}

	readDSN, err := databaseDSN(dsnPath, true, memory, opts.BusyTimeout)
	if err != nil {
		cleanup()
		return nil, err
	}
	readDB, err := sql.Open("sqlite", readDSN)
	if err != nil {
		cleanup()
		return nil, wrapStorageError(CodeUnavailable, err, "open hub database readers")
	}
	readDB.SetMaxOpenConns(opts.ReadConnections)
	readDB.SetMaxIdleConns(opts.ReadConnections)
	if err := readDB.PingContext(ctx); err != nil {
		_ = readDB.Close()
		cleanup()
		return nil, wrapStorageError(CodeUnavailable, mapError(err), "open hub database readers")
	}
	db.read = readDB
	return db, nil
}

// Path is the database file path, or ":memory:".
func (d *Database) Path() string { return d.path }

// repairAccountIncarnations gives a fresh never-reused incarnation to every
// account that has none and drops sessions that could not be bound to one.
// Migration 6 defaults the column to empty, and a schema-5 binary that legally
// writes this file after a rollback cannot fill it. The repair runs at open,
// before the read pool starts, so authentication never observes an account
// whose empty incarnation would compare equal to another empty snapshot.
func (d *Database) repairAccountIncarnations(ctx context.Context) error {
	if d.schema.Version < 6 {
		return nil
	}
	return d.Write(ctx, func(tx *Tx) error {
		rows, err := tx.Query("SELECT username FROM users WHERE incarnation = '' ORDER BY username")
		if err != nil {
			return mapError(err)
		}
		var usernames []string
		for rows.Next() {
			var username string
			if err := rows.Scan(&username); err != nil {
				rows.Close()
				return mapError(err)
			}
			usernames = append(usernames, username)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return mapError(err)
		}
		rows.Close()
		for _, username := range usernames {
			incarnation, err := newAccountIncarnation()
			if err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE users SET incarnation = ? WHERE username = ?", incarnation, username); err != nil {
				return err
			}
		}
		// A schema-5 writer stores the migration default in every session it
		// mints. Those rows can never match the now-repaired account, and a
		// session issued before the account had an incarnation must not be
		// adopted after one is assigned.
		if _, err := tx.Exec("DELETE FROM browser_sessions WHERE user_incarnation = ''"); err != nil {
			return err
		}
		return nil
	})
}

// SchemaVersion is the highest applied migration version.
func (d *Database) SchemaVersion() int { return d.schema.Version }

// Schema reports the applied migration ledger and the compatibility floor: the
// lowest schema version whose binary can still open and write this database.
// The upgrade transaction allows a rollback to a binary that writes schema P
// exactly when P >= CompatibleDown. Version can be newer than this build when
// an older binary opened a file migrated by a newer one.
func (d *Database) Schema() SchemaInfo { return d.schema }

// Now reports the database clock.
func (d *Database) Now() time.Time { return d.now() }

// Write runs fn inside one BEGIN IMMEDIATE ... COMMIT transaction on the
// single writer connection. A returned error rolls back; fn must not retain
// the transaction after returning.
//
// Every ordinary writer passes admission first: when the configured main-file
// plus WAL budget is already spent, the write attempts one bounded checkpoint
// and returns a typed storage-pressure error instead of growing the WAL until
// the filesystem fails. Control-state writes therefore cannot bypass the
// budget that telemetry already honours.
func (d *Database) Write(ctx context.Context, fn func(*Tx) error) error {
	return d.writeTransaction(ctx, fn, true)
}

// writeMaintenance is a writer that may make room under a physical budget:
// retention deletes, incremental vacuum and cursor bookkeeping. Running them
// through admission would reject the transaction that relieves the pressure.
func (d *Database) writeMaintenance(ctx context.Context, fn func(*Tx) error) error {
	return d.writeTransaction(ctx, fn, false)
}

func (d *Database) writeTransaction(ctx context.Context, fn func(*Tx) error, admit bool) error {
	if d.closed.Load() {
		return storageError(CodeClosed, "hub database is closed")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if d.closed.Load() {
		return storageError(CodeClosed, "hub database is closed")
	}
	if admit {
		if err := d.admitWriteLocked(ctx); err != nil {
			return err
		}
	}
	conn, err := d.write.Conn(ctx)
	if err != nil {
		return wrapStorageError(CodeUnavailable, mapError(err), "acquire writer connection")
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return wrapStorageError(CodeBusy, mapError(err), "begin write transaction")
	}
	tx := &Tx{ctx: ctx, conn: conn}
	if err := fn(tx); err != nil {
		// The caller's context may have been cancelled; ROLLBACK must still run
		// so the single writer connection is usable for the next caller.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return wrapStorageError(CodeUnavailable, mapError(err), "commit write transaction")
	}
	return nil
}

// admitWriteLocked checks the configured physical budget while writeMu is
// held. A single checkpoint and incremental-vacuum attempt runs before the
// write is refused, so transient WAL growth does not fail an otherwise healthy
// write.
func (d *Database) admitWriteLocked(ctx context.Context) error {
	d.physicalMu.Lock()
	configured := d.physical
	d.physicalMu.Unlock()
	if configured == nil {
		return nil
	}
	// Fast path: two file stats are enough to prove the write is well inside
	// the budget. A page-cap check below that headroom would require the
	// expensive PRAGMA reads and can only trigger when the files are already
	// near the cap.
	headroom := physicalHeadroom(configured.LimitBytes)
	if d.SizeBytes()+headroom < configured.LimitBytes {
		return nil
	}
	status := d.readPhysicalStatus(ctx, configured.LimitBytes, configured.WALBudgetBytes, configured.CheckpointPages, configured.MaxPageCount)
	if !writeAtPressure(status) {
		d.storePhysical(status)
		return nil
	}
	_ = d.checkpointLocked(ctx)
	if status.AutoVacuum == 2 {
		d.reclaimPagesLocked(ctx, 128)
	}
	status = d.readPhysicalStatus(ctx, configured.LimitBytes, configured.WALBudgetBytes, configured.CheckpointPages, configured.MaxPageCount)
	d.storePhysical(status)
	if writeAtPressure(status) {
		return &Error{
			Code:       CodeStoragePressure,
			Message:    "hub storage is over its size budget",
			RetryAfter: 2 * time.Second,
		}
	}
	return nil
}

// writeAtPressure reports whether a write needs its reserved headroom but the
// measured files plus that headroom already exceed the budget, or the page cap
// has no free page left.
func physicalHeadroom(limitBytes int64) int64 {
	headroom := limitBytes / 8
	if headroom < 64*1024 {
		headroom = 64 * 1024
	}
	if headroom > 1024*1024 {
		headroom = 1024 * 1024
	}
	return headroom
}

func writeAtPressure(status PhysicalStatus) bool {
	pageFull := status.MaxPageCount > 0 && status.PageCount >= status.MaxPageCount && status.FreePages == 0
	return status.Bytes+physicalHeadroom(status.LimitBytes) >= status.LimitBytes || pageFull
}

func (d *Database) storePhysical(status PhysicalStatus) {
	d.physicalMu.Lock()
	d.physical = &status
	d.physicalMu.Unlock()
}

// readRows wraps a read-pool result so closing it releases the bounded read
// context. Without the wrapper a caller that forgets Close would pin WAL
// frames until its own context expired (possibly never).
type readRows struct {
	*sql.Rows
	cancel context.CancelFunc
}

// Close closes the rows and releases the read deadline.
func (r *readRows) Close() error {
	err := r.Rows.Close()
	r.cancel()
	return err
}

// readRow wraps a single-row read with the same bounded lifetime.
type readRow struct {
	*sql.Row
	cancel context.CancelFunc
}

// Scan reads the row and releases the read deadline.
func (r *readRow) Scan(dest ...any) error {
	defer r.cancel()
	return r.Row.Scan(dest...)
}

// Err reports any error from the row and releases the read deadline.
func (r *readRow) Err() error {
	defer r.cancel()
	return r.Row.Err()
}

// Query runs a read-only query through the reader pool. The statement and its
// iteration are bounded by Options.ReadTimeout in addition to the caller's
// context.
func (d *Database) Query(ctx context.Context, query string, args ...any) (*readRows, error) {
	if d.closed.Load() {
		return nil, storageError(CodeClosed, "hub database is closed")
	}
	readCtx, cancel := context.WithTimeout(ctx, d.readTimeout)
	rows, err := d.read.QueryContext(readCtx, query, args...)
	if err != nil {
		cancel()
		return nil, mapError(err)
	}
	return &readRows{Rows: rows, cancel: cancel}, nil
}

// QueryRow runs a read-only query through the reader pool. The row lookup is
// bounded by Options.ReadTimeout in addition to the caller's context.
func (d *Database) QueryRow(ctx context.Context, query string, args ...any) *readRow {
	readCtx, cancel := context.WithTimeout(ctx, d.readTimeout)
	return &readRow{Row: d.read.QueryRowContext(readCtx, query, args...), cancel: cancel}
}

// Tx is an in-flight write transaction. Every method is context-aware and
// returns classified storage errors.
type Tx struct {
	ctx  context.Context
	conn *sql.Conn
}

// Exec runs a statement inside the transaction.
func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	res, err := t.conn.ExecContext(t.ctx, query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	return res, nil
}

// Query runs a statement that returns rows.
func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	rows, err := t.conn.QueryContext(t.ctx, query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	return rows, nil
}

// QueryRow runs a statement expected to return at most one row.
func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(t.ctx, query, args...)
}

// Prepare compiles a statement on the writer connection so a bounded batch can
// reuse it for thousands of rows without re-preparing each time.
func (t *Tx) Prepare(query string) (*sql.Stmt, error) {
	statement, err := t.conn.PrepareContext(t.ctx, query)
	if err != nil {
		return nil, mapError(err)
	}
	return statement, nil
}

// SizeBytes is the main database file plus the WAL. The shm index is excluded.
func (d *Database) SizeBytes() int64 {
	if d.path == ":memory:" {
		pageSize := d.pragmaInt64(context.Background(), "page_size", 4096)
		pageCount := d.pragmaInt64(context.Background(), "page_count", 0)
		return pageSize * pageCount
	}
	return fileSize(d.path) + d.WALBytes()
}

// WALBytes is the size of the write-ahead log sidecar.
func (d *Database) WALBytes() int64 {
	if d.path == ":memory:" {
		return 0
	}
	return fileSize(d.path + "-wal")
}

// SetPhysicalLimit applies a finite main-file page cap plus a bounded WAL
// reserve. Existing databases over the cap report pressure; this never deletes
// rows or runs a destructive startup VACUUM.
func (d *Database) SetPhysicalLimit(limitBytes int64) (PhysicalStatus, error) {
	if d.closed.Load() {
		return PhysicalStatus{}, storageError(CodeClosed, "hub database is closed")
	}
	if limitBytes < 16*1024 {
		return PhysicalStatus{}, invalidError("physical limit must be at least 16 KiB")
	}
	ctx := context.Background()
	pageSize := d.pragmaInt64(ctx, "page_size", 4096)
	if pageSize <= 0 {
		pageSize = 4096
	}
	walBudget := limitBytes / 16
	if walBudget < 64*1024 {
		walBudget = 64 * 1024
	}
	if walBudget > WALReserveBytes {
		walBudget = WALReserveBytes
	}
	mainBudget := limitBytes - walBudget
	if mainBudget < pageSize {
		mainBudget = pageSize
	}
	pages := mainBudget / pageSize
	checkpointPages := walBudget / pageSize
	if checkpointPages < 16 {
		checkpointPages = 16
	}
	if checkpointPages > 16_384 {
		checkpointPages = 16_384
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if _, err := d.write.ExecContext(ctx, fmt.Sprintf("PRAGMA journal_size_limit = %d", walBudget)); err != nil {
		return PhysicalStatus{}, wrapStorageError(CodeUnavailable, mapError(err), "configure WAL journal limit")
	}
	if _, err := d.write.ExecContext(ctx, fmt.Sprintf("PRAGMA wal_autocheckpoint = %d", checkpointPages)); err != nil {
		return PhysicalStatus{}, wrapStorageError(CodeUnavailable, mapError(err), "configure WAL autocheckpoint")
	}
	maxPages := pages
	var readBack int64
	if err := d.write.QueryRowContext(ctx, fmt.Sprintf("PRAGMA max_page_count = %d", pages)).Scan(&readBack); err == nil && readBack > 0 {
		maxPages = readBack
	} else if _, execErr := d.write.ExecContext(ctx, fmt.Sprintf("PRAGMA max_page_count = %d", pages)); execErr != nil {
		return PhysicalStatus{}, wrapStorageError(CodeUnavailable, mapError(execErr), "configure database page cap")
	} else {
		maxPages = d.pragmaInt64(ctx, "max_page_count", pages)
	}

	status := d.readPhysicalStatus(ctx, limitBytes, walBudget, checkpointPages, maxPages)
	d.physicalMu.Lock()
	d.physical = &status
	d.physicalMu.Unlock()
	// Shrink an existing WAL when no reader blocks it. A busy checkpoint must
	// not make opening the hub fail.
	_, _ = d.write.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	status = d.readPhysicalStatus(ctx, limitBytes, walBudget, checkpointPages, maxPages)
	d.physicalMu.Lock()
	d.physical = &status
	d.physicalMu.Unlock()
	return status, nil
}

// PhysicalLimit reports the current physical budget and measured files, or
// nil when no limit has been configured.
func (d *Database) PhysicalLimit() *PhysicalStatus {
	d.physicalMu.Lock()
	configured := d.physical
	d.physicalMu.Unlock()
	if configured == nil {
		return nil
	}
	status := d.readPhysicalStatus(
		context.Background(),
		configured.LimitBytes,
		configured.WALBudgetBytes,
		configured.CheckpointPages,
		configured.MaxPageCount,
	)
	d.physicalMu.Lock()
	d.physical = &status
	d.physicalMu.Unlock()
	return &status
}

// ReclaimPages returns up to maxPages free pages to the filesystem when
// auto_vacuum permits it, and reports how many were reclaimed.
func (d *Database) ReclaimPages(maxPages int) int {
	if maxPages <= 0 {
		maxPages = 64
	}
	if maxPages > 100_000 {
		maxPages = 100_000
	}
	ctx := context.Background()
	if d.pragmaInt64(ctx, "auto_vacuum", 0) != 2 {
		return 0
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return d.reclaimPagesLocked(ctx, maxPages)
}

// reclaimPagesLocked runs incremental_vacuum with writeMu held.
func (d *Database) reclaimPagesLocked(ctx context.Context, maxPages int) int {
	if maxPages <= 0 {
		maxPages = 64
	}
	if maxPages > 100_000 {
		maxPages = 100_000
	}
	before := d.pragmaInt64(ctx, "freelist_count", 0)
	if _, err := d.write.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", maxPages)); err != nil {
		return 0
	}
	after := d.pragmaInt64(ctx, "freelist_count", 0)
	if after >= before {
		return 0
	}
	return int(before - after)
}

// Checkpoint truncates the WAL when no reader blocks it. A busy checkpoint is
// not an error: the page cap still bounds the file until the next pass.
func (d *Database) Checkpoint(ctx context.Context) error {
	if d.closed.Load() {
		return storageError(CodeClosed, "hub database is closed")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return d.checkpointLocked(ctx)
}

// checkpointLocked truncates the WAL with writeMu held.
func (d *Database) checkpointLocked(ctx context.Context) error {
	rows, err := d.write.QueryContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	if err != nil {
		return mapError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var busy, logFrames, checkpointed int64
		if err := rows.Scan(&busy, &logFrames, &checkpointed); err != nil {
			return mapError(err)
		}
	}
	return mapError(rows.Err())
}

// Backup writes a consistent online copy of the live database to dstPath using
// SQLite's VACUUM INTO. The destination is built beside its final name,
// chmodded 0600, verified as a regular file and atomically renamed, so a
// failed backup leaves the previous good copy and the live database untouched.
//
// This method satisfies the upgrade transaction's DatabaseBackup contract:
//
//	Backup(ctx context.Context, dstPath string) error
func (d *Database) Backup(ctx context.Context, dstPath string) error {
	if d.closed.Load() {
		return storageError(CodeClosed, "hub database is closed")
	}
	if dstPath == "" {
		return invalidError("backup destination must be a nonempty path")
	}
	dst, err := filepath.Abs(dstPath)
	if err != nil {
		return wrapStorageError(CodeInvalid, err, "resolve backup destination")
	}
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, dirMode); err != nil {
		return wrapStorageError(CodeUnavailable, err, "create backup directory")
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return wrapStorageError(CodeUnavailable, err, "resolve backup directory")
	}
	destination := filepath.Join(realParent, filepath.Base(dst))

	existing, err := os.Lstat(destination)
	switch {
	case err == nil:
		if !existing.Mode().IsRegular() {
			return invalidError("backup destination must be a regular file path")
		}
	case errors.Is(err, fs.ErrNotExist):
		// Fine: a new copy.
	default:
		return wrapStorageError(CodeUnavailable, err, "inspect backup destination")
	}
	if d.path != ":memory:" && d.destinationAliasesLive(destination) {
		// The live file, its WAL, SHM and rollback journal are all part of the
		// open database. Replacing any of them would corrupt or silently drop
		// committed state.
		return conflictError("backup destination aliases the live database")
	}

	// VACUUM cannot run inside a transaction. Serialize against the writer so
	// the snapshot cannot race a flush that is between statements. The output
	// is created inside a fresh 0700 directory so it is unreachable by other
	// accounts from its first byte, independent of the process umask; it is
	// renamed only after it is verified.
	temporaryDir, err := os.MkdirTemp(realParent, ".rigscope-backup-")
	if err != nil {
		return wrapStorageError(CodeUnavailable, err, "create backup workspace")
	}
	defer os.RemoveAll(temporaryDir)
	if err := os.Chmod(temporaryDir, dirMode); err != nil {
		return wrapStorageError(CodeUnavailable, err, "protect backup workspace")
	}
	temporary := filepath.Join(temporaryDir, "copy.db")

	d.writeMu.Lock()
	_, vacErr := d.write.ExecContext(ctx, "VACUUM INTO ?", temporary)
	d.writeMu.Unlock()
	if vacErr != nil {
		return wrapStorageError(CodeUnavailable, mapError(vacErr), "database backup failed")
	}
	if err := os.Chmod(temporary, fileMode); err != nil {
		return wrapStorageError(CodeUnavailable, err, "protect database backup")
	}
	if info, err := os.Lstat(temporary); err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("backup is not a regular file")
		}
		return wrapStorageError(CodeUnavailable, err, "verify database backup")
	}
	if err := os.Rename(temporary, destination); err != nil {
		return wrapStorageError(CodeUnavailable, err, "replace database backup")
	}
	syncDirectory(realParent)
	return nil
}

// destinationAliasesLive reports whether dst canonically names the open
// database file or one of its SQLite sidecars, including through symlinks,
// hard links and syntactic aliases.
func (d *Database) destinationAliasesLive(dst string) bool {
	live := []string{d.path, d.path + "-wal", d.path + "-shm", d.path + "-journal"}
	canonicalDst := canonicalFilePath(dst)
	destInfo, destErr := os.Stat(dst)
	for _, path := range live {
		if canonicalDst == canonicalFilePath(path) {
			return true
		}
		if destErr != nil {
			continue
		}
		liveInfo, err := os.Stat(path)
		if err != nil {
			continue
		}
		if os.SameFile(liveInfo, destInfo) {
			return true
		}
	}
	return false
}

// canonicalFilePath resolves symlinks in path when it exists, and otherwise
// resolves the directory that would contain it and joins the base name.
func canonicalFilePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	parent := filepath.Dir(path)
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Join(filepath.Clean(resolved), filepath.Base(path))
	}
	return filepath.Clean(path)
}

// Close flushes the WAL where possible and closes both pools. It is
// idempotent.
func (d *Database) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.writeMu.Lock()
	_, _ = d.write.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	d.writeMu.Unlock()
	var firstErr error
	if d.read != nil {
		if err := d.read.Close(); err != nil {
			firstErr = err
		}
	}
	if err := d.write.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (d *Database) readPhysicalStatus(ctx context.Context, limitBytes, walBudget, checkpointPages, maxPageCount int64) PhysicalStatus {
	mainBytes := d.SizeBytes() - d.WALBytes()
	walBytes := d.WALBytes()
	pageSize := d.pragmaInt64(ctx, "page_size", 4096)
	pageCount := d.pragmaInt64(ctx, "page_count", 0)
	freePages := d.pragmaInt64(ctx, "freelist_count", 0)
	if d.path == ":memory:" {
		mainBytes = pageSize * pageCount
	}
	return PhysicalStatus{
		LimitBytes:      limitBytes,
		MainBytes:       mainBytes,
		WALBytes:        walBytes,
		Bytes:           mainBytes + walBytes,
		PageSize:        pageSize,
		PageCount:       pageCount,
		MaxPageCount:    maxPageCount,
		FreePages:       freePages,
		WALBudgetBytes:  walBudget,
		CheckpointPages: checkpointPages,
		AutoVacuum:      d.pragmaInt64(ctx, "auto_vacuum", 0),
		Pressure:        mainBytes+walBytes > limitBytes || (maxPageCount > 0 && pageCount >= maxPageCount && freePages == 0),
	}
}

// PhysicalStatus is a measured snapshot of the database files and budget.
type PhysicalStatus struct {
	LimitBytes      int64
	MainBytes       int64
	WALBytes        int64
	Bytes           int64
	PageSize        int64
	PageCount       int64
	MaxPageCount    int64
	FreePages       int64
	WALBudgetBytes  int64
	CheckpointPages int64
	AutoVacuum      int64
	Pressure        bool
}

func (d *Database) pragmaInt64(ctx context.Context, name string, fallback int64) int64 {
	rows, err := d.write.QueryContext(ctx, "PRAGMA "+name)
	if err != nil {
		return fallback
	}
	defer rows.Close()
	if !rows.Next() {
		return fallback
	}
	var value int64
	if err := rows.Scan(&value); err != nil {
		return fallback
	}
	return value
}

func databaseDSN(path string, readOnly, memory bool, busyTimeout time.Duration) (string, error) {
	busyMS := busyTimeout.Milliseconds()
	if busyMS < 1 {
		busyMS = 1
	}
	pragmas := []string{
		fmt.Sprintf("busy_timeout(%d)", busyMS),
		"foreign_keys(1)",
	}
	if readOnly {
		pragmas = append(pragmas, "query_only(1)")
	} else {
		// journal_mode and synchronous are set by Open with ExecContext after
		// PRAGMA auto_vacuum: a DSN pragma runs at connection open, before Open
		// can choose auto_vacuum, and initializing WAL writes page 1.
		pragmas = append(pragmas, "temp_store(MEMORY)")
	}
	if memory && readOnly {
		pragmas = append(pragmas, "temp_store(MEMORY)")
	}
	values := url.Values{}
	for _, pragma := range pragmas {
		values.Add("_pragma", pragma)
	}
	if !readOnly {
		values.Set("_txlock", "immediate")
	}
	if memory {
		values.Set("mode", "memory")
		values.Set("cache", "shared")
		return "file:" + path + "?" + values.Encode(), nil
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: values.Encode()}
	return u.String(), nil
}

func preparePrivateFile(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, dirMode); err != nil {
		return wrapStorageError(CodeUnavailable, err, "create hub database directory")
	}
	// The file is 0600, but a group- or world-accessible directory still lets
	// another local account rename, replace or unlink it. Refuse to open in a
	// directory that is not private.
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return wrapStorageError(CodeUnavailable, err, "inspect hub database directory")
	}
	if !parentInfo.IsDir() {
		return invalidError("hub database parent must be a directory")
	}
	if parentInfo.Mode().Perm()&0o077 != 0 {
		return invalidError("hub database directory must be private (0700)")
	}
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return invalidError("hub database path must be a regular file")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return invalidError("hub database file must be private (0600)")
		}
	case errors.Is(err, fs.ErrNotExist):
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
		if err != nil {
			return wrapStorageError(CodeUnavailable, err, "create hub database file")
		}
		if err := file.Close(); err != nil {
			return wrapStorageError(CodeUnavailable, err, "create hub database file")
		}
	default:
		return wrapStorageError(CodeUnavailable, err, "inspect hub database file")
	}
	return nil
}

func normalizeSidecarModes(path string) error {
	for _, sidecar := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(sidecar, fileMode); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return wrapStorageError(CodeUnavailable, err, "protect hub database file")
		}
	}
	return nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func syncDirectory(path string) {
	dir, err := os.Open(path)
	if err != nil {
		return
	}
	defer dir.Close()
	_ = dir.Sync()
}

// SQLite primary result codes used for classification.
const (
	sqliteBusy       = 5
	sqliteLocked     = 6
	sqliteReadOnly   = 8
	sqliteIOErr      = 10
	sqliteCorrupt    = 11
	sqliteFull       = 13
	sqliteCantOpen   = 14
	sqliteConstraint = 19
)

// mapError classifies driver and context errors. Unknown errors pass through
// unchanged so callers still see the original text.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case sqliteBusy, sqliteLocked:
			return &Error{
				Code:       CodeBusy,
				Message:    "hub storage is busy",
				RetryAfter: time.Second,
				Err:        err,
			}
		case sqliteFull:
			return &Error{
				Code:       CodeStorageFull,
				Message:    "storage disk budget reached",
				RetryAfter: 5 * time.Second,
				Err:        err,
			}
		case sqliteIOErr, sqliteCantOpen:
			return &Error{
				Code:       CodeStorageFull,
				Message:    "storage write failed: disk I/O error",
				RetryAfter: 5 * time.Second,
				Err:        err,
			}
		case sqliteCorrupt:
			return &Error{
				Code:       CodeUnavailable,
				Message:    "hub database is corrupt",
				RetryAfter: 5 * time.Second,
				Err:        err,
			}
		case sqliteReadOnly:
			return &Error{Code: CodeUnavailable, Message: "hub database is read-only", Err: err}
		case sqliteConstraint:
			return &Error{Code: CodeConflict, Message: err.Error(), Err: err}
		}
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "database or disk is full"):
		return &Error{Code: CodeStorageFull, Message: "storage disk budget reached", RetryAfter: 5 * time.Second, Err: err}
	case strings.Contains(message, "database is locked"):
		return &Error{Code: CodeBusy, Message: "hub storage is busy", RetryAfter: time.Second, Err: err}
	}
	return err
}

// IsStorageFull reports whether err means the database hit its size budget or
// the filesystem is out of space. Callers treat it as a visible degraded
// state, never as a successful write.
func IsStorageFull(err error) bool {
	return CodeOf(err) == CodeStorageFull
}

// IsStoragePressure reports whether err means the configured physical budget
// was already spent when a write was admitted. It is a retryable degraded
// state: retention may free space, and the write did not run.
func IsStoragePressure(err error) bool {
	return CodeOf(err) == CodeStoragePressure
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func intToBool(value int64) bool { return value != 0 }
