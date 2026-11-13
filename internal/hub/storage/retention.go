package storage

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	hourMS        = int64(3_600_000)
	dayMS         = int64(24) * hourMS
	defaultChunk  = 5_000
	maxChunksPass = 10_000

	defaultSeriesPageSize = 64
	defaultPassBudget     = 250 * time.Millisecond
	defaultPruneLimit     = 500
	seriesCursorKey       = "retention.series_cursor"
)

// RetentionPolicy is the age window for every stored history tier. Dedup
// receipts have an independent, longer window so shortening chart history can
// never let a replayed collector record count twice. There is no raw tier in
// 1.0: the 1-second ring is memory-only.
type RetentionPolicy struct {
	TenSecondMs int64
	OneMinuteMs int64
	OneHourMs   int64
	ReceiptsMs  int64
	GapsMs      int64
}

// DefaultRetentionPolicy is deliberately conservative.
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		TenSecondMs: 2 * hourMS,
		OneMinuteMs: 30 * dayMS,
		OneHourMs:   400 * dayMS,
		ReceiptsMs:  7 * dayMS,
		GapsMs:      400 * dayMS,
	}
}

// RetentionOverrides replaces individual windows; nil fields keep defaults.
type RetentionOverrides struct {
	TenSecond *time.Duration
	OneMinute *time.Duration
	OneHour   *time.Duration
	Receipts  *time.Duration
	Gaps      *time.Duration
}

// ResolveRetention merges overrides over the defaults.
func ResolveRetention(overrides RetentionOverrides) (RetentionPolicy, error) {
	policy := DefaultRetentionPolicy()
	set := func(target *int64, value *time.Duration, name string) error {
		if value == nil {
			return nil
		}
		if *value < 0 || *value > 400*24*time.Hour {
			return invalidError("retention %s must be between 0 and 400 days", name)
		}
		*target = value.Milliseconds()
		return nil
	}
	if err := set(&policy.TenSecondMs, overrides.TenSecond, "tenSecond"); err != nil {
		return RetentionPolicy{}, err
	}
	if err := set(&policy.OneMinuteMs, overrides.OneMinute, "oneMinute"); err != nil {
		return RetentionPolicy{}, err
	}
	if err := set(&policy.OneHourMs, overrides.OneHour, "oneHour"); err != nil {
		return RetentionPolicy{}, err
	}
	if err := set(&policy.ReceiptsMs, overrides.Receipts, "receipts"); err != nil {
		return RetentionPolicy{}, err
	}
	if err := set(&policy.GapsMs, overrides.Gaps, "gaps"); err != nil {
		return RetentionPolicy{}, err
	}
	return policy, nil
}

// RetentionStatus is a diagnostic snapshot of cleanup and physical pressure.
type RetentionStatus struct {
	LastRunAt          time.Time
	LastRunDuration    time.Duration
	DeletedRows        int64
	PrunedSeries       int64
	Pressure           bool
	EffectiveCutoffMs  int64
	PhysicalBytes      int64
	WALBytes           int64
	PhysicalLimitBytes int64
	PhysicalPressure   bool
}

type retentionManager struct {
	db               *Database
	policy           RetentionPolicy
	now              func() time.Time
	interval         time.Duration
	chunkSize        int
	maxChunksPerPass int
	maxDatabaseBytes int64
	maxPhysicalBytes int64

	pageSize   int
	passBudget time.Duration
	pruneLimit int

	// onSeriesPruned invalidates the in-memory catalog after orphaned series
	// rows are deleted, so a cached id can never point at a missing row.
	onSeriesPruned func([]int64)

	runMu        sync.Mutex
	cursor       int64
	cursorLoaded bool

	mu     sync.Mutex
	status RetentionStatus

	cancel context.CancelFunc
	done   chan struct{}
}

func newRetentionManager(db *Database, policy RetentionPolicy, now func() time.Time, interval time.Duration, chunkSize int, maxDatabaseBytes, maxPhysicalBytes int64) *retentionManager {
	if interval <= 0 {
		interval = time.Minute
	}
	if chunkSize <= 0 {
		chunkSize = defaultChunk
	}
	manager := &retentionManager{
		db:               db,
		policy:           policy,
		now:              now,
		interval:         interval,
		chunkSize:        chunkSize,
		maxChunksPerPass: maxChunksPass,
		maxDatabaseBytes: maxDatabaseBytes,
		maxPhysicalBytes: maxPhysicalBytes,
		pageSize:         defaultSeriesPageSize,
		passBudget:       defaultPassBudget,
		pruneLimit:       defaultPruneLimit,
	}
	manager.refreshPhysicalState()
	return manager
}

func (m *retentionManager) start(ctx context.Context) {
	if m.done != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_ = m.runPass(runCtx)
			}
		}
	}()
}

func (m *retentionManager) stop() {
	m.stopContext(context.Background())
}

// stopContext stops the loop and waits for it no longer than ctx allows. A
// caller whose deadline already expired is not made to wait for a blocked
// database operation.
func (m *retentionManager) stopContext(ctx context.Context) {
	if m.cancel == nil {
		return
	}
	m.cancel()
	select {
	case <-m.done:
		m.cancel = nil
		m.done = nil
	case <-ctx.Done():
	}
}

func (m *retentionManager) statusSnapshot() RetentionStatus {
	m.refreshPhysicalState()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *retentionManager) overBudget() bool {
	bytes := m.db.SizeBytes()
	if m.maxDatabaseBytes > 0 && bytes > m.maxDatabaseBytes {
		return true
	}
	if m.maxPhysicalBytes > 0 {
		physical := m.db.PhysicalLimit()
		if physical != nil {
			return physical.Pressure
		}
		return bytes > m.maxPhysicalBytes
	}
	return false
}

// relievePressure performs bounded, synchronous cleanup for the write path. It
// deletes only ten-second and one-minute rows older than the most recent hour,
// so a too-small budget degrades old history rather than the live view. If the
// last hour alone exceeds the cap, ingestion reports busy instead.
func (m *retentionManager) relievePressure(ctx context.Context, maxChunks int) int {
	deleted := 0
	for chunk := 0; chunk < maxChunks; chunk++ {
		if !m.overBudget() {
			break
		}
		count, err := m.runChunk(ctx)
		if err != nil {
			break
		}
		deleted += count
		if count == 0 {
			break
		}
	}
	m.db.ReclaimPages(128)
	_ = m.db.Checkpoint(ctx)
	m.refreshPhysicalState()
	return deleted
}

// runChunk deletes at most chunkSize rows for one page of series, one tier at a
// time. The series page comes from a persistent cursor so cleanup never scans
// every series row to find work, and the writer transaction holds only that
// page.
func (m *retentionManager) runChunk(ctx context.Context) (int, error) {
	deleted, _, err := m.runChunkCycle(ctx)
	return deleted, err
}

func (m *retentionManager) runChunkCycle(ctx context.Context) (int, bool, error) {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	started := time.Now()
	nowMs := m.now().UnixMilli()
	page, nextCursor, wrapped, err := m.seriesPage(ctx)
	if err != nil {
		return 0, wrapped, err
	}
	cutoffs := []struct {
		table  string
		column string
		cutoff int64
	}{
		{"points_10s", "bucket_ms", nowMs - m.policy.TenSecondMs},
		{"points_1m", "bucket_ms", nowMs - m.policy.OneMinuteMs},
		{"points_1h", "bucket_ms", nowMs - m.policy.OneHourMs},
	}

	deleted := 0
	for _, tier := range cutoffs {
		if ctx.Err() != nil {
			return 0, wrapped, ctx.Err()
		}
		count, err := m.deleteTierChunk(ctx, tier.table, tier.column, page, tier.cutoff, nextCursor, nowMs, deleted > 0)
		if err != nil {
			return 0, wrapped, err
		}
		deleted += count
		if count > 0 {
			break
		}
	}
	if deleted == 0 {
		deleted, err = m.deleteReceiptsChunk(ctx, nowMs-m.policy.ReceiptsMs, nextCursor, nowMs)
		if err != nil {
			return 0, wrapped, err
		}
	}
	if deleted == 0 {
		deleted, err = m.deleteGapsChunk(ctx, nowMs-m.policy.GapsMs, nextCursor, nowMs)
		if err != nil {
			return 0, wrapped, err
		}
	}

	effectiveCutoff := nowMs - m.policy.TenSecondMs
	if deleted == 0 && m.overBudget() {
		floor := nowMs - hourMS
		if floor > effectiveCutoff {
			effectiveCutoff = floor
		}
		deleted, err = m.deleteTierChunk(ctx, "points_10s", "bucket_ms", page, effectiveCutoff, nextCursor, nowMs, false)
		if err != nil {
			return 0, wrapped, err
		}
		if deleted == 0 {
			deleted, err = m.deleteTierChunk(ctx, "points_1m", "bucket_ms", page, floor, nextCursor, nowMs, false)
			if err != nil {
				return 0, wrapped, err
			}
		}
		if deleted > 0 {
			m.db.ReclaimPages(64)
			_ = m.db.Checkpoint(ctx)
		}
	}

	pruned := 0
	if len(page) > 0 {
		pruned, err = m.pruneOrphanSeries(ctx, page)
		if err != nil {
			return 0, wrapped, err
		}
	}

	m.cursor = nextCursor
	m.refreshPhysicalState()
	m.mu.Lock()
	m.status.EffectiveCutoffMs = effectiveCutoff
	m.status.DeletedRows += int64(deleted)
	m.status.PrunedSeries += int64(pruned)
	m.status.LastRunAt = msToTime(nowMs)
	m.status.LastRunDuration = time.Since(started)
	m.status.Pressure = m.overBudget()
	m.mu.Unlock()
	return deleted, wrapped, nil
}

// runPass drains expired rows in bounded chunks until a full cursor cycle
// found nothing, the per-pass work budget is spent, or the caller stops. The
// next tick resumes from the persisted cursor.
func (m *retentionManager) runPass(ctx context.Context) error {
	deadline := time.Now().Add(m.passBudget)
	deletedSinceWrap := 0
	for chunk := 0; chunk < m.maxChunksPerPass; chunk++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return nil
		}
		deleted, wrapped, err := m.runChunkCycle(ctx)
		if err != nil {
			return err
		}
		deletedSinceWrap += deleted
		if wrapped {
			if deletedSinceWrap == 0 {
				return nil
			}
			deletedSinceWrap = 0
		}
	}
	return nil
}

// seriesPage reads the next bounded page of series ids from the persistent
// cleanup cursor. A page is read through the reader pool, so the writer lock is
// only held for the deletions themselves.
func (m *retentionManager) seriesPage(ctx context.Context) ([]int64, int64, bool, error) {
	if !m.cursorLoaded {
		m.cursor = m.loadCursor(ctx)
		m.cursorLoaded = true
	}
	page, more, err := m.readSeriesPage(ctx, m.cursor)
	if err != nil {
		return nil, m.cursor, false, err
	}
	if len(page) == 0 && m.cursor != 0 {
		// Wrap once per full cycle; the next pass continues from the start.
		page, more, err = m.readSeriesPage(ctx, 0)
		if err != nil {
			return nil, 0, false, err
		}
		m.cursor = 0
		if len(page) == 0 {
			return nil, 0, true, nil
		}
		next := page[len(page)-1]
		if more {
			next = page[len(page)-1]
		}
		return page, next, true, nil
	}
	if len(page) == 0 {
		return nil, 0, true, nil
	}
	return page, page[len(page)-1], false, nil
}

func (m *retentionManager) readSeriesPage(ctx context.Context, after int64) ([]int64, bool, error) {
	rows, err := m.db.Query(ctx,
		"SELECT series_id FROM series WHERE series_id > ? ORDER BY series_id LIMIT ?",
		after, m.pageSize+1,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	ids := make([]int64, 0, m.pageSize)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, false, mapError(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, mapError(err)
	}
	more := len(ids) > m.pageSize
	if more {
		ids = ids[:m.pageSize]
	}
	return ids, more, nil
}

func (m *retentionManager) loadCursor(ctx context.Context) int64 {
	var value string
	err := m.db.QueryRow(ctx, "SELECT value FROM settings WHERE key = ?", seriesCursorKey).Scan(&value)
	if err != nil {
		return 0
	}
	cursor, err := strconv.ParseInt(value, 10, 64)
	if err != nil || cursor < 0 {
		return 0
	}
	return cursor
}

func persistSeriesCursor(tx *Tx, cursor, nowMs int64) error {
	_, err := tx.Exec(
		"INSERT INTO settings (key, value, updated_at_ms) VALUES (?, ?, ?) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at_ms = excluded.updated_at_ms",
		seriesCursorKey, strconv.FormatInt(cursor, 10), nowMs,
	)
	return err
}

func (m *retentionManager) deleteTierChunk(ctx context.Context, table, column string, seriesIDs []int64, cutoffMs, nextCursor, nowMs int64, persistOnly bool) (int, error) {
	if len(seriesIDs) == 0 {
		return 0, nil
	}
	statement := "DELETE FROM " + table + " WHERE (series_id, " + column + ") IN " +
		"(SELECT series_id, " + column + " FROM " + table + " WHERE series_id = ? AND " + column + " < ? LIMIT ?)"
	deleted := 0
	err := m.db.writeMaintenance(ctx, func(tx *Tx) error {
		for _, seriesID := range seriesIDs {
			remaining := m.chunkSize - deleted
			if remaining <= 0 {
				break
			}
			result, err := tx.Exec(statement, seriesID, cutoffMs, remaining)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return mapError(err)
			}
			deleted += int(affected)
		}
		if deleted > 0 || persistOnly {
			// Best effort: losing the cursor only repeats a bounded scan, while
			// letting a full-page-cap metadata write abort the deletion would
			// keep the pressure from ever being relieved.
			_ = persistSeriesCursor(tx, nextCursor, nowMs)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

func (m *retentionManager) deleteReceiptsChunk(ctx context.Context, cutoffMs, nextCursor, nowMs int64) (int, error) {
	return m.deleteKeyedChunk(ctx,
		"DELETE FROM sample_receipts WHERE (collector_id, run_id, seq) IN "+
			"(SELECT collector_id, run_id, seq FROM sample_receipts WHERE captured_at_ms < ? LIMIT ?)",
		cutoffMs, nextCursor, nowMs,
	)
}

func (m *retentionManager) deleteGapsChunk(ctx context.Context, cutoffMs, nextCursor, nowMs int64) (int, error) {
	return m.deleteKeyedChunk(ctx,
		"DELETE FROM gaps WHERE (collector_id, run_id, seq) IN "+
			"(SELECT collector_id, run_id, seq FROM gaps WHERE to_ms < ? LIMIT ?)",
		cutoffMs, nextCursor, nowMs,
	)
}

func (m *retentionManager) deleteKeyedChunk(ctx context.Context, statement string, cutoffMs, nextCursor, nowMs int64) (int, error) {
	deleted := 0
	err := m.db.writeMaintenance(ctx, func(tx *Tx) error {
		result, err := tx.Exec(statement, cutoffMs, m.chunkSize)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return mapError(err)
		}
		deleted = int(affected)
		if deleted > 0 {
			_ = persistSeriesCursor(tx, nextCursor, nowMs)
		}
		return nil
	})
	return deleted, err
}

// pruneOrphanSeries deletes series catalog rows that have no points in any
// tier. It checks only the current page, so the writer never scans the whole
// series table, and it caps deletions per call.
func (m *retentionManager) pruneOrphanSeries(ctx context.Context, page []int64) (int, error) {
	if len(page) == 0 {
		return 0, nil
	}
	limit := m.pruneLimit
	if limit <= 0 {
		limit = defaultPruneLimit
	}
	ids := page
	if len(ids) > limit {
		ids = ids[:limit]
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")
	selectStatement := "SELECT s.series_id FROM series s WHERE s.series_id IN (" + placeholders + ") " +
		"AND NOT EXISTS (SELECT 1 FROM points_10s p WHERE p.series_id = s.series_id) " +
		"AND NOT EXISTS (SELECT 1 FROM points_1m p WHERE p.series_id = s.series_id) " +
		"AND NOT EXISTS (SELECT 1 FROM points_1h p WHERE p.series_id = s.series_id) " +
		"LIMIT ?"
	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, limit)

	var removed []int64
	err := m.db.writeMaintenance(ctx, func(tx *Tx) error {
		rows, err := tx.Query(selectStatement, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return mapError(err)
			}
			removed = append(removed, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return mapError(err)
		}
		rows.Close()
		if len(removed) == 0 {
			return nil
		}
		deletePlaceholders := strings.TrimSuffix(strings.Repeat("?, ", len(removed)), ", ")
		deleteArgs := make([]any, 0, len(removed))
		for _, id := range removed {
			deleteArgs = append(deleteArgs, id)
		}
		_, err = tx.Exec("DELETE FROM series WHERE series_id IN ("+deletePlaceholders+")", deleteArgs...)
		return err
	})
	if err != nil {
		return 0, err
	}
	if len(removed) > 0 && m.onSeriesPruned != nil {
		m.onSeriesPruned(removed)
	}
	return len(removed), nil
}

func (m *retentionManager) refreshPhysicalState() {
	bytes := m.db.SizeBytes()
	wal := m.db.WALBytes()
	physical := m.db.PhysicalLimit()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.PhysicalBytes = bytes
	m.status.WALBytes = wal
	if physical != nil {
		m.status.PhysicalLimitBytes = physical.LimitBytes
		m.status.PhysicalPressure = physical.Pressure
	} else if m.maxPhysicalBytes > 0 {
		m.status.PhysicalLimitBytes = m.maxPhysicalBytes
		m.status.PhysicalPressure = bytes > m.maxPhysicalBytes
	}
	m.status.Pressure = (m.maxDatabaseBytes > 0 && bytes > m.maxDatabaseBytes) || m.status.PhysicalPressure
}
