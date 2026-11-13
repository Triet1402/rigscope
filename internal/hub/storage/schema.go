package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ErrNoSchemaLedger reports that a hub database file does not exist or does not
// carry a Rigscope migration ledger. A fresh hub has no database yet, and an
// unrelated placeholder file is not one either.
var ErrNoSchemaLedger = errors.New("hub database has no schema ledger")

// sqliteHeader is the fixed 16-byte header of a SQLite 3 database file.
const sqliteHeader = "SQLite format 3\x00"

// ReadSchemaLedger reads the applied migration ledger from an existing hub
// database without creating the database, running migrations or writing
// application data. The upgrade transaction uses it to record the previous
// binary's actual schema before the staged binary migrates the file: the staged
// schema floor can then be compared with the version the old binary actually
// wrote.
//
// The SQLite connection is opened read-only, but SQLite can still create or
// update its sidecar files beside the database (notably the -shm file for a WAL
// database) to take a read lock. Callers must therefore expect that opening a
// database through this function may create a missing -shm; the database file
// itself and the schema are never modified.
func ReadSchemaLedger(ctx context.Context, path string) (SchemaInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SchemaInfo{}, ErrNoSchemaLedger
		}
		return SchemaInfo{}, fmt.Errorf("inspect hub database: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return SchemaInfo{}, fmt.Errorf("hub database %s is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return SchemaInfo{}, fmt.Errorf("open hub database: %w", err)
	}
	header := make([]byte, len(sqliteHeader))
	_, readErr := io.ReadFull(file, header)
	closeErr := file.Close()
	if readErr != nil {
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			return SchemaInfo{}, ErrNoSchemaLedger
		}
		return SchemaInfo{}, fmt.Errorf("read hub database header: %w", readErr)
	}
	if closeErr != nil {
		return SchemaInfo{}, fmt.Errorf("close hub database: %w", closeErr)
	}
	if string(header) != sqliteHeader {
		return SchemaInfo{}, ErrNoSchemaLedger
	}

	dsn, err := databaseDSN(path, true, false, defaultBusyTimeout)
	if err != nil {
		return SchemaInfo{}, err
	}
	// A read-only connection cannot create the file or apply migrations.
	dsn += "&mode=ro"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return SchemaInfo{}, wrapStorageError(CodeUnavailable, err, "open hub database read-only")
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.PingContext(ctx); err != nil {
		return SchemaInfo{}, wrapStorageError(CodeUnavailable, mapError(err), "open hub database read-only")
	}
	var ledgers int
	if err := database.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'",
	).Scan(&ledgers); err != nil {
		return SchemaInfo{}, wrapStorageError(CodeUnavailable, mapError(err), "inspect hub migration ledger")
	}
	if ledgers == 0 {
		return SchemaInfo{}, ErrNoSchemaLedger
	}
	rows, err := database.QueryContext(ctx,
		"SELECT version, name, compatible_down, applied_at_ms FROM schema_migrations ORDER BY version")
	if err != nil {
		return SchemaInfo{}, wrapStorageError(CodeUnavailable, mapError(err), "read hub migration ledger")
	}
	defer rows.Close()
	var applied []AppliedMigration
	floor := 1
	for rows.Next() {
		var (
			version        int
			name           string
			compatibleDown int
			appliedAtMS    int64
		)
		if err := rows.Scan(&version, &name, &compatibleDown, &appliedAtMS); err != nil {
			return SchemaInfo{}, wrapStorageError(CodeUnavailable, mapError(err), "read hub migration ledger")
		}
		if version != len(applied)+1 {
			return SchemaInfo{}, &Error{
				Code: CodeUnavailable,
				Message: fmt.Sprintf(
					"hub database migration history is not contiguous at version %d; it was written by a hub this build does not recognize",
					version,
				),
			}
		}
		if compatibleDown < 1 || compatibleDown > version {
			return SchemaInfo{}, &Error{
				Code: CodeUnavailable,
				Message: fmt.Sprintf(
					"hub database migration %d declares compatible_down %d outside 1..%d",
					version, compatibleDown, version,
				),
			}
		}
		if compatibleDown > floor {
			floor = compatibleDown
		}
		applied = append(applied, AppliedMigration{
			Version:        version,
			Name:           name,
			CompatibleDown: compatibleDown,
			AppliedAt:      time.UnixMilli(appliedAtMS).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return SchemaInfo{}, wrapStorageError(CodeUnavailable, mapError(err), "read hub migration ledger")
	}
	if len(applied) == 0 {
		return SchemaInfo{}, ErrNoSchemaLedger
	}
	return SchemaInfo{
		Version:        applied[len(applied)-1].Version,
		CompatibleDown: floor,
		Applied:        applied,
	}, nil
}
