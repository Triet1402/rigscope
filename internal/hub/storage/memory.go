package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// DefaultMemoryDatabaseBytes bounds RAM used by summary tables, receipts and
// inventory. The separately bounded one-second ring adds at most 64 MiB.
const DefaultMemoryDatabaseBytes int64 = 128 * 1024 * 1024

// newMemoryTelemetry reuses the bounded summary/query machinery on a private
// in-memory SQLite database. Control state and collector credentials stay in
// the supplied durable database; telemetry never writes to that database.
func newMemoryTelemetry(ctx context.Context, registryDB *Database, opts TelemetryOptions) (*Telemetry, error) {
	if opts.Now == nil {
		opts.Now = registryDB.Now
	}
	db, err := Open(ctx, ":memory:", Options{Now: opts.Now})
	if err != nil {
		return nil, err
	}
	// Status polls must not scan the whole history table. This index exists
	// only in the volatile database and needs no durable schema migration.
	if err := db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec("CREATE INDEX memory_history_bounds ON points_1h(bucket_ms)")
		return err
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	opts.Memory = false
	opts.credentialDB = registryDB
	if opts.FlushInterval <= 0 {
		// RAM acceptance incurs no disk commit; keep its shorter latency.
		opts.FlushInterval = time.Second
	}
	if opts.MaxPhysicalBytes == nil {
		limit := DefaultMemoryDatabaseBytes
		opts.MaxPhysicalBytes = &limit
	}
	if opts.MaxDatabaseBytes == 0 {
		opts.MaxDatabaseBytes = DefaultMemoryDatabaseBytes * 3 / 4
	}
	store, err := NewTelemetry(ctx, db, opts)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Persistent reports whether acknowledged history survives a hub restart.
func (t *Telemetry) Persistent() bool { return t.credentialDB == nil }

// HistoryBounds returns the actual extent of retained summaries, including
// replayed data, rather than promising a full retention window after startup.
func (t *Telemetry) HistoryBounds() (*int64, *int64) {
	var oldest, newest sql.NullInt64
	if err := t.db.QueryRow(t.ctx, "SELECT (SELECT MIN(bucket_ms) FROM points_1h), (SELECT MAX(bucket_ms) + 3600000 FROM points_1h)").Scan(&oldest, &newest); err != nil || !oldest.Valid {
		return nil, nil
	}
	end := t.now().UnixMilli()
	if newest.Int64 < end {
		end = newest.Int64
	}
	return &oldest.Int64, &end
}

// writeBatch fences credential replacement/revocation until RAM acceptance
// finishes. Every durable registry writer takes this same mutex. No durable
// transaction is opened, so ordinary RAM ingestion causes no disk writes.
// Lock order is always durable database, then memory database.
func (t *Telemetry) writeBatch(fn func(*Tx) error) error {
	if t.credentialDB != nil {
		t.credentialDB.writeMu.Lock()
		defer t.credentialDB.writeMu.Unlock()
	}
	return t.db.Write(t.ctx, fn)
}

func (t *Telemetry) batchCredentialCurrent(tx *Tx, batch *Batch) (bool, error) {
	if t.credentialDB == nil || batch.CredentialHash == "" {
		return batchCredentialCurrentTx(tx, batch)
	}
	var stored string
	err := t.credentialDB.QueryRow(t.ctx,
		"SELECT token_hash FROM collector_registry WHERE collector_id = ? AND revoked = 0",
		batch.CollectorID,
	).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapError(err)
	}
	return stored == batch.CredentialHash, nil
}
