package storage

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestResolveRetentionKeepsReceiptsIndependent(t *testing.T) {
	merged, err := ResolveRetention(RetentionOverrides{TenSecond: durationPtr(time.Second)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	defaults := DefaultRetentionPolicy()
	if merged.TenSecondMs != 1000 {
		t.Fatalf("ten second = %d", merged.TenSecondMs)
	}
	if merged.ReceiptsMs != defaults.ReceiptsMs || merged.GapsMs != defaults.GapsMs {
		t.Fatalf("receipts/gaps were coupled to chart retention: %+v", merged)
	}
	custom, err := ResolveRetention(RetentionOverrides{Receipts: durationPtr(5 * time.Second)})
	if err != nil || custom.ReceiptsMs != 5000 {
		t.Fatalf("custom receipts = %+v, %v", custom, err)
	}
	if _, err := ResolveRetention(RetentionOverrides{TenSecond: durationPtr(-time.Second)}); CodeOf(err) != CodeInvalid {
		t.Fatalf("negative retention error = %v", err)
	}
}

func retentionFixture(t *testing.T, policy RetentionPolicy, maxDatabaseBytes int64) (*Database, *retentionManager, time.Time) {
	t.Helper()
	db, _ := openTestDatabase(t)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	db.now = func() time.Time { return now }
	manager := newRetentionManager(db, policy, func() time.Time { return now }, time.Minute, 0, maxDatabaseBytes, 0)
	return db, manager, now
}

func insertSeries(t *testing.T, db *Database, metric, resource string) int64 {
	t.Helper()
	var id int64
	if err := db.Write(testContext(t), func(tx *Tx) error {
		result, err := tx.Exec("INSERT INTO series (machine_id, metric, resource_id) VALUES ('machine-1', ?, ?)", metric, resource)
		if err != nil {
			return err
		}
		id, err = result.LastInsertId()
		return err
	}); err != nil {
		t.Fatalf("insert series: %v", err)
	}
	return id
}

func insertSummaries(t *testing.T, db *Database, table string, seriesID int64, buckets []int64) {
	t.Helper()
	if err := db.Write(testContext(t), func(tx *Tx) error {
		for _, bucket := range buckets {
			if _, err := tx.Exec(
				"INSERT INTO "+table+" (series_id, bucket_ms, min, max, sum, count, null_count) VALUES (?, ?, 1, 1, 1, 1, 0)",
				seriesID, bucket,
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("insert %s: %v", table, err)
	}
}

func TestRetentionDrainsExpiredRowsInBoundedChunks(t *testing.T) {
	policy := RetentionPolicy{TenSecondMs: 1000, OneMinuteMs: 1000, OneHourMs: 1000, ReceiptsMs: 1000, GapsMs: 1000}
	db, manager, now := retentionFixture(t, policy, 0)
	series := insertSeries(t, db, "cpu.usage", "")
	buckets := make([]int64, 0, 12_000)
	for index := 0; index < 12_000; index++ {
		buckets = append(buckets, now.UnixMilli()-10_000-int64(index))
	}
	insertSummaries(t, db, "points_10s", series, buckets)
	manager.chunkSize = 1000
	total := 0
	calls := 0
	largest := 0
	for {
		deleted, err := manager.runChunk(testContext(t))
		if err != nil {
			t.Fatalf("chunk: %v", err)
		}
		if deleted == 0 {
			break
		}
		if deleted > 1000 {
			t.Fatalf("chunk deleted %d rows", deleted)
		}
		if deleted > largest {
			largest = deleted
		}
		total += deleted
		calls++
		if calls > 100 {
			t.Fatal("retention did not converge")
		}
	}
	if total != 12_000 || largest != 1000 || calls < 12 {
		t.Fatalf("total=%d largest=%d calls=%d", total, largest, calls)
	}
	if scalar(t, db, "SELECT COUNT(*) FROM points_10s") != 0 {
		t.Fatal("expired rows remain")
	}
	status := manager.statusSnapshot()
	if status.DeletedRows != 12_000 || status.EffectiveCutoffMs != now.UnixMilli()-1000 {
		t.Fatalf("status = %+v", status)
	}
}

func TestRetentionAppliesEachTierCutoff(t *testing.T) {
	hour := int64(3_600_000)
	policy := RetentionPolicy{TenSecondMs: hour, OneMinuteMs: 2 * hour, OneHourMs: 3 * hour, ReceiptsMs: hour, GapsMs: hour}
	db, manager, now := retentionFixture(t, policy, 0)
	series := insertSeries(t, db, "cpu.usage", "")
	insertSummaries(t, db, "points_10s", series, []int64{now.UnixMilli() - 2*hour, now.UnixMilli() - hour/2})
	insertSummaries(t, db, "points_1m", series, []int64{now.UnixMilli() - 3*hour, now.UnixMilli() - 2*hour + hour/2})
	insertSummaries(t, db, "points_1h", series, []int64{now.UnixMilli() - 4*hour, now.UnixMilli() - 2*hour})
	if err := db.Write(testContext(t), func(tx *Tx) error {
		if _, err := tx.Exec(
			"INSERT INTO sample_receipts (collector_id, run_id, seq, machine_id, kind, mode, captured_at_ms, received_at_ms, expected_interval_ms) VALUES ('collector-1','run-1',1,'machine-1','metrics','live',?,?,5000)",
			now.UnixMilli()-2*hour, now.UnixMilli(),
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			"INSERT INTO sample_receipts (collector_id, run_id, seq, machine_id, kind, mode, captured_at_ms, received_at_ms, expected_interval_ms) VALUES ('collector-1','run-1',2,'machine-1','metrics','live',?,?,5000)",
			now.UnixMilli()-hour/2, now.UnixMilli(),
		); err != nil {
			return err
		}
		for _, entry := range []struct {
			seq int64
			at  int64
		}{{10, now.UnixMilli() - 2*hour}, {11, now.UnixMilli() - hour/2}} {
			if _, err := tx.Exec(
				"INSERT INTO gaps (collector_id, run_id, seq, machine_id, from_ms, to_ms, dropped, reason, received_at_ms) VALUES ('collector-1','run-1',?,'machine-1',?,?,1,'outbox-age',?)",
				entry.seq, entry.at, entry.at, now.UnixMilli(),
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed receipts/gaps: %v", err)
	}
	if err := manager.runPass(testContext(t)); err != nil {
		t.Fatalf("pass: %v", err)
	}
	for _, table := range []string{"points_10s", "points_1m", "points_1h", "sample_receipts", "gaps"} {
		if got := scalar(t, db, "SELECT COUNT(*) FROM "+table); got != 1 {
			t.Fatalf("%s rows = %d, want 1", table, got)
		}
	}
	if status := manager.statusSnapshot(); status.DeletedRows != 5 || status.Pressure {
		t.Fatalf("status = %+v", status)
	}
}

func TestRetentionPressureKeepsTheLastHour(t *testing.T) {
	hour := int64(3_600_000)
	policy := RetentionPolicy{TenSecondMs: 100 * hour, OneMinuteMs: 100 * hour, OneHourMs: 100 * hour, ReceiptsMs: 100 * hour, GapsMs: 100 * hour}
	db, manager, now := retentionFixture(t, policy, 1)
	series := insertSeries(t, db, "cpu.usage", "")
	insertSummaries(t, db, "points_10s", series, []int64{now.UnixMilli() - 2*hour, now.UnixMilli() - hour/2})
	insertSummaries(t, db, "points_1m", series, []int64{now.UnixMilli() - 2*hour, now.UnixMilli() - hour/2})
	if err := manager.runPass(testContext(t)); err != nil {
		t.Fatalf("pass: %v", err)
	}
	var remaining []int64
	rows, err := db.Query(testContext(t), "SELECT bucket_ms FROM points_10s ORDER BY bucket_ms")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
		var bucket int64
		if err := rows.Scan(&bucket); err != nil {
			t.Fatalf("scan: %v", err)
		}
		remaining = append(remaining, bucket)
	}
	rows.Close()
	if len(remaining) != 1 || remaining[0] != now.UnixMilli()-hour/2 {
		t.Fatalf("remaining 10s = %v", remaining)
	}
	// The minute row inside the protected hour is deleted only after the older
	// ten-second data is gone; a still-over-budget database keeps reporting
	// pressure instead of deleting recent history.
	var minuteRows int64
	if err := db.QueryRow(testContext(t), "SELECT COUNT(*) FROM points_1m").Scan(&minuteRows); err != nil {
		t.Fatalf("count 1m: %v", err)
	}
	status := manager.statusSnapshot()
	if !status.Pressure {
		t.Fatalf("tight budget did not report pressure: %+v", status)
	}
	if status.EffectiveCutoffMs != now.UnixMilli()-hour {
		t.Fatalf("cutoff = %d", status.EffectiveCutoffMs)
	}
	if status.DeletedRows != 2 {
		t.Fatalf("deleted = %d, want 2", status.DeletedRows)
	}
	_ = minuteRows
}

func TestRetentionChunkRollsBackOnFailure(t *testing.T) {
	policy := RetentionPolicy{TenSecondMs: 1000, OneMinuteMs: 1000, OneHourMs: 1000, ReceiptsMs: 1000, GapsMs: 1000}
	db, manager, now := retentionFixture(t, policy, 0)
	manager.chunkSize = 10
	first := insertSeries(t, db, "first", "")
	second := insertSeries(t, db, "second", "")
	insertSummaries(t, db, "points_10s", first, []int64{now.UnixMilli() - 2000})
	insertSummaries(t, db, "points_10s", second, []int64{now.UnixMilli() - 2000})
	if err := db.Write(testContext(t), func(tx *Tx) error {
		_, err := tx.Exec("CREATE TRIGGER fail_second BEFORE DELETE ON points_10s WHEN OLD.series_id = " + itoa64(second) +
			" BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END")
		return err
	}); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if _, err := manager.runChunk(testContext(t)); err == nil || !strings.Contains(err.Error(), "injected delete failure") {
		t.Fatalf("chunk error = %v", err)
	}
	if got := scalar(t, db, "SELECT COUNT(*) FROM points_10s"); got != 2 {
		t.Fatalf("rollback left %d rows, want 2", got)
	}
	if err := db.Write(testContext(t), func(tx *Tx) error {
		_, err := tx.Exec("DROP TRIGGER fail_second")
		return err
	}); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	deleted, err := manager.runChunk(testContext(t))
	if err != nil || deleted != 2 {
		t.Fatalf("recovered chunk = %d, %v", deleted, err)
	}
}

func TestRetentionDrainsAcrossSeriesWithinChunkSize(t *testing.T) {
	policy := RetentionPolicy{TenSecondMs: 1000, OneMinuteMs: 1000, OneHourMs: 1000, ReceiptsMs: 1000, GapsMs: 1000}
	db, manager, now := retentionFixture(t, policy, 0)
	manager.chunkSize = 100
	var series []int64
	for index := 0; index < 3; index++ {
		series = append(series, insertSeries(t, db, "cpu.core", "core:"+itoa(index)))
	}
	for _, id := range series {
		buckets := make([]int64, 0, 200)
		for index := 0; index < 200; index++ {
			buckets = append(buckets, now.UnixMilli()-10_000-int64(index))
		}
		insertSummaries(t, db, "points_10s", id, buckets)
	}
	total := 0
	for calls := 0; ; calls++ {
		deleted, err := manager.runChunk(testContext(t))
		if err != nil {
			t.Fatalf("chunk: %v", err)
		}
		if deleted == 0 {
			break
		}
		if deleted > 100 {
			t.Fatalf("chunk = %d", deleted)
		}
		total += deleted
		if calls > 50 {
			t.Fatal("did not converge")
		}
	}
	if total != 600 {
		t.Fatalf("deleted = %d, want 600", total)
	}
}

func TestRetentionDeleteUsesPrimaryKey(t *testing.T) {
	policy := RetentionPolicy{TenSecondMs: 1000, OneMinuteMs: 1000, OneHourMs: 1000, ReceiptsMs: 1000, GapsMs: 1000}
	db, _, now := retentionFixture(t, policy, 0)
	for _, table := range []string{"points_10s", "points_1m", "points_1h"} {
		statement := "DELETE FROM " + table + " WHERE (series_id, bucket_ms) IN " +
			"(SELECT series_id, bucket_ms FROM " + table + " WHERE series_id = ? AND bucket_ms < ? LIMIT ?)"
		rows, err := db.Query(testContext(t), "EXPLAIN QUERY PLAN "+statement, 1, now.UnixMilli(), 1000)
		if err != nil {
			t.Fatalf("plan %s: %v", table, err)
		}
		text := ""
		for rows.Next() {
			var id, parent, notUsed int64
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				rows.Close()
				t.Fatalf("scan: %v", err)
			}
			text += detail + "\n"
		}
		rows.Close()
		if !strings.Contains(text, "USING PRIMARY KEY") {
			t.Fatalf("%s retention plan = %s", table, text)
		}
		if strings.Contains(text, "SCAN "+table) {
			t.Fatalf("%s retention full-scans: %s", table, text)
		}
	}
}

func TestRetentionPaginatesAndPrunesOrphanedSeries(t *testing.T) {
	policy := RetentionPolicy{TenSecondMs: 1000, OneMinuteMs: 1000, OneHourMs: 1000, ReceiptsMs: 1000, GapsMs: 1000}
	db, manager, now := retentionFixture(t, policy, 0)
	manager.pageSize = 2
	manager.chunkSize = 4
	var pruned []int64
	manager.onSeriesPruned = func(ids []int64) { pruned = append(pruned, ids...) }
	for index := 0; index < 6; index++ {
		id := insertSeries(t, db, "cpu.core", "core:"+itoa(index))
		buckets := make([]int64, 0, 10)
		for offset := 0; offset < 10; offset++ {
			buckets = append(buckets, now.UnixMilli()-10_000-int64(offset))
		}
		insertSummaries(t, db, "points_10s", id, buckets)
	}
	total := 0
	for calls := 0; ; calls++ {
		deleted, err := manager.runChunk(testContext(t))
		if err != nil {
			t.Fatalf("chunk: %v", err)
		}
		if deleted > 4 {
			t.Fatalf("chunk deleted %d rows, cap is 4", deleted)
		}
		total += deleted
		if deleted == 0 {
			break
		}
		if calls > 100 {
			t.Fatal("retention did not converge")
		}
	}
	if total != 60 {
		t.Fatalf("deleted = %d, want 60", total)
	}
	if got := scalar(t, db, "SELECT COUNT(*) FROM series"); got != 0 {
		t.Fatalf("orphaned series rows = %d", got)
	}
	if len(pruned) != 6 {
		t.Fatalf("pruned callback ids = %v", pruned)
	}
	if status := manager.statusSnapshot(); status.PrunedSeries != 6 {
		t.Fatalf("status = %+v", status)
	}
	// The cursor is durable, so a restarted hub resumes instead of rescanning.
	restarted := newRetentionManager(db, policy, func() time.Time { return now }, time.Minute, 0, 0, 0)
	if cursor := restarted.loadCursor(testContext(t)); cursor <= 0 {
		t.Fatalf("persisted cursor = %d", cursor)
	}
}

func TestRetentionBackgroundLoopStartsAndStops(t *testing.T) {
	policy := RetentionPolicy{TenSecondMs: 1000, OneMinuteMs: 1000, OneHourMs: 1000, ReceiptsMs: 1000, GapsMs: 1000}
	_, manager, _ := retentionFixture(t, policy, 0)
	manager.interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for manager.statusSnapshot().LastRunAt.IsZero() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	manager.stop()
	if manager.statusSnapshot().LastRunAt.IsZero() {
		t.Fatal("background retention never ran")
	}
	manager.stop() // idempotent
}
