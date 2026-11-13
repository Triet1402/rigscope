package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

var testBase = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

type testClock struct {
	value atomic.Int64
}

func (c *testClock) now() time.Time   { return time.UnixMilli(c.value.Load()) }
func (c *testClock) set(at time.Time) { c.value.Store(at.UnixMilli()) }

type telemetryFixture struct {
	db    *Database
	store *Telemetry
	path  string
	clock *testClock
}

func setupTelemetry(t *testing.T, options TelemetryOptions) *telemetryFixture {
	t.Helper()
	dir := privateTempDir(t)
	path := dir + "/hub.db"
	db, err := Open(testContext(t), path, Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	clock := &testClock{}
	clock.set(testBase.Add(time.Minute))
	db.now = clock.now
	if options.Now == nil {
		options.Now = clock.now
	}
	if options.FlushInterval == 0 {
		options.FlushInterval = time.Millisecond
	}
	if options.Ring.Retention == 0 {
		options.Ring.Retention = 15 * time.Minute
	}
	store, err := NewTelemetry(testContext(t), db, options)
	if err != nil {
		t.Fatalf("new telemetry: %v", err)
	}
	fixture := &telemetryFixture{db: db, store: store, path: path, clock: clock}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = store.Close(closeCtx)
		_ = db.Close()
	})
	return fixture
}

func (f *telemetryFixture) ingest(t *testing.T, batch Batch) IngestResult {
	t.Helper()
	result, err := f.store.Enqueue(testContext(t), batch)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return result
}

func scalar(t *testing.T, db *Database, query string, args ...any) int64 {
	t.Helper()
	var value int64
	if err := db.QueryRow(testContext(t), query, args...).Scan(&value); err != nil {
		t.Fatalf("scalar %q: %v", query, err)
	}
	return value
}

func TestIngestCommitsBeforeAckAndIsVisibleToSecondConnection(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	result := fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 1, capturedAt: testBase.UnixMilli()})))
	if len(result.Accepted) != 1 || len(result.Rejected) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if scalar(t, fixture.db, "SELECT COUNT(*) FROM sample_receipts") != 1 {
		t.Fatal("receipt missing after commit")
	}
	if scalar(t, fixture.db, "SELECT COUNT(*) FROM points_10s") == 0 {
		t.Fatal("summary rows missing after commit")
	}
	var tables int64
	if err := fixture.db.QueryRow(testContext(t), "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='points_raw'").Scan(&tables); err != nil {
		t.Fatalf("check raw table: %v", err)
	}
	if tables != 0 {
		t.Fatal("summary-only storage must not create points_raw")
	}
}

func TestIngestDeduplicatesAndKeepsRollupExtrema(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	records := []protocol.TelemetryRecord{
		makeMetricsRecord(recordOptions{sequence: 1, capturedAt: testBase.UnixMilli(), payload: payloadWithUsage(90)}),
		makeMetricsRecord(recordOptions{sequence: 2, capturedAt: testBase.Add(10 * time.Second).UnixMilli(), payload: payloadWithUsage(5)}),
	}
	first := fixture.ingest(t, liveBatch(records...))
	if len(first.Accepted) != 2 {
		t.Fatalf("accepted = %+v", first.Accepted)
	}
	seriesID := scalar(t, fixture.db, "SELECT series_id FROM series WHERE machine_id = 'machine-1' AND metric = 'cpu.usage' AND resource_id = ''")
	var (
		min, max float64
		count    int64
	)
	if err := fixture.db.QueryRow(testContext(t),
		"SELECT MIN(min), MAX(max), SUM(count) FROM points_1m WHERE series_id = ?", seriesID,
	).Scan(&min, &max, &count); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if min != 5 || max != 90 || count != 2 {
		t.Fatalf("rollup min=%v max=%v count=%d", min, max, count)
	}

	replayed := fixture.ingest(t, replayBatch(records[1], records[0]))
	if len(replayed.Accepted) != 2 || len(replayed.Rejected) != 0 {
		t.Fatalf("retry result = %+v", replayed)
	}
	if scalar(t, fixture.db, "SELECT COUNT(*) FROM points_10s") == 0 {
		t.Fatal("summaries disappeared")
	}
	var sum int64
	if err := fixture.db.QueryRow(testContext(t), "SELECT SUM(count) FROM points_1m WHERE series_id = ?", seriesID).Scan(&sum); err != nil {
		t.Fatalf("rollup after retry: %v", err)
	}
	if sum != 2 {
		t.Fatalf("rollup count after retry = %d, want 2", sum)
	}
	stats := fixture.store.Stats()
	if stats.DuplicateRecords != 2 || stats.CommittedRecords != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestIngestReplayNeverUpdatesLatestLive(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	live := fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 1, capturedAt: testBase.UnixMilli()})))
	if live.NewLive == nil || live.NewLive.Sequence != 1 {
		t.Fatalf("live = %+v", live.NewLive)
	}
	replay := fixture.ingest(t, replayBatch(makeMetricsRecord(recordOptions{sequence: 2, capturedAt: testBase.Add(2 * time.Minute).UnixMilli()})))
	if replay.NewLive != nil {
		t.Fatalf("replay produced live state: %+v", replay.NewLive)
	}
	latest, err := fixture.store.LatestLive(testContext(t))
	if err != nil || len(latest) != 1 || latest[0].Sequence != 1 {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	newer := fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 3, capturedAt: testBase.Add(4 * time.Minute).UnixMilli()})))
	if newer.NewLive == nil || newer.NewLive.Sequence != 3 {
		t.Fatalf("newer live = %+v", newer.NewLive)
	}
}

func TestIngestLiveCursorDoesNotRegress(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 5, capturedAt: testBase.UnixMilli()})))
	older := fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 4, capturedAt: testBase.Add(time.Second).UnixMilli()})))
	if older.NewLive != nil {
		t.Fatal("older sequence regressed live state")
	}
	olderRun := fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{
		runID: "run-2", sequence: 1, capturedAt: testBase.Add(-time.Minute).UnixMilli(),
	})))
	if olderRun.NewLive != nil {
		t.Fatal("older run regressed live state")
	}
	latest, _ := fixture.store.LatestLive(testContext(t))
	if latest[0].RunID != "run-1" || latest[0].Sequence != 5 {
		t.Fatalf("latest = %+v", latest[0])
	}
	newerRun := fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{
		runID: "run-3", sequence: 1, capturedAt: testBase.Add(time.Minute).UnixMilli(),
	})))
	if newerRun.NewLive == nil || newerRun.NewLive.RunID != "run-3" {
		t.Fatalf("newer run = %+v", newerRun.NewLive)
	}
}

func TestIngestRejectsMismatchedCollectorAndFutureTimes(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	result := fixture.ingest(t, liveBatch(
		makeMetricsRecord(recordOptions{collectorID: "collector-other", sequence: 1, capturedAt: testBase.UnixMilli()}),
		makeMetricsRecord(recordOptions{sequence: 2, capturedAt: fixture.clock.now().Add(25 * time.Hour).UnixMilli()}),
	))
	if len(result.Accepted) != 0 || len(result.Rejected) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Rejected[0].Reason, "collectorId") {
		t.Fatalf("reason = %q", result.Rejected[0].Reason)
	}
	if !strings.Contains(result.Rejected[1].Reason, "future") {
		t.Fatalf("reason = %q", result.Rejected[1].Reason)
	}
	if scalar(t, fixture.db, "SELECT COUNT(*) FROM sample_receipts") != 0 {
		t.Fatal("rejected records produced receipts")
	}
	if fixture.store.Stats().RejectedRecords != 2 {
		t.Fatalf("stats = %+v", fixture.store.Stats())
	}
}

func TestIngestRejectsSamplesOutsideTheReplayWindow(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{
		MaxReplayAge: 6 * time.Hour,
		ClockSkew:    5 * time.Minute,
	})
	now := fixture.clock.now()
	result := fixture.ingest(t, liveBatch(
		makeMetricsRecord(recordOptions{sequence: 1, capturedAt: now.Add(-7 * time.Hour).UnixMilli()}),
		makeMetricsRecord(recordOptions{sequence: 2, capturedAt: now.Add(10 * time.Minute).UnixMilli()}),
		makeMetricsRecord(recordOptions{sequence: 3, capturedAt: now.Add(-time.Hour).UnixMilli()}),
	))
	if len(result.Accepted) != 1 || len(result.Rejected) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if result.Rejected[0].Reason != RejectionCapturedTooOld || !IsPermanentRejection(result.Rejected[0].Reason) {
		t.Fatalf("old reason = %q", result.Rejected[0].Reason)
	}
	if result.Rejected[1].Reason != RejectionCapturedInFuture || !IsPermanentRejection(result.Rejected[1].Reason) {
		t.Fatalf("future reason = %q", result.Rejected[1].Reason)
	}
	// Rejections produce no receipt, so a retry cannot resurrect them later.
	if got := scalar(t, fixture.db, "SELECT COUNT(*) FROM sample_receipts"); got != 1 {
		t.Fatalf("receipts = %d, want 1", got)
	}
}

func TestReceiptRetentionMustOutliveTheReplayWindow(t *testing.T) {
	db, _ := openTestDatabase(t)
	_, err := NewTelemetry(testContext(t), db, TelemetryOptions{
		Retention: RetentionOverrides{Receipts: durationPtr(time.Hour)},
	})
	if CodeOf(err) != CodeInvalid || !strings.Contains(err.Error(), "receipt retention") {
		t.Fatalf("short receipts error = %v", err)
	}
	_, err = NewTelemetry(testContext(t), db, TelemetryOptions{
		Retention:    RetentionOverrides{Receipts: durationPtr(48 * time.Hour)},
		MaxReplayAge: 47 * time.Hour,
		ClockSkew:    2 * time.Hour,
	})
	if CodeOf(err) != CodeInvalid || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("non-covering receipts error = %v", err)
	}
	defaults := DefaultRetentionPolicy()
	if defaults.ReceiptsMs <= defaultMaxReplayAge.Milliseconds()+defaultClockSkew.Milliseconds() {
		t.Fatalf("default receipts %d do not outlive the default replay window", defaults.ReceiptsMs)
	}
}

func TestIngestUnavailableSemantics(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	unavailable := makePayload(payloadOptions{
		unavailableCPU: true, unavailableMemory: true, unavailableSwap: true,
		unavailableGPUs: true, unavailableDisks: true, unavailableNetwork: true,
	})
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 1, capturedAt: testBase.UnixMilli(), payload: &unavailable})))

	if got := scalar(t, fixture.db, "SELECT COUNT(*) FROM points_10s"); got != 4 {
		t.Fatalf("summary rows = %d, want 4", got)
	}
	for _, metric := range []string{"cpu.usage", "memory.used", "memory.usage", "swap.used"} {
		seriesID := scalar(t, fixture.db, "SELECT series_id FROM series WHERE metric = ? AND resource_id = ''", metric)
		var count, nulls int64
		if err := fixture.db.QueryRow(testContext(t),
			"SELECT SUM(count), SUM(null_count) FROM points_10s WHERE series_id = ?", seriesID,
		).Scan(&count, &nulls); err != nil {
			t.Fatalf("read %s: %v", metric, err)
		}
		if count != 0 || nulls != 1 {
			t.Fatalf("%s count=%d nulls=%d", metric, count, nulls)
		}
	}
	for _, prefix := range []string{"gpu.%", "fs.%", "disk.%", "net.%"} {
		if got := scalar(t, fixture.db, "SELECT COUNT(*) FROM series WHERE metric LIKE ?", prefix); got != 0 {
			t.Fatalf("%s series created on a fresh unavailable machine: %d", prefix, got)
		}
	}

	// An unavailable per-resource reading gives NULL primary metrics only.
	perResource := makePayload(payloadOptions{
		cores: 1,
		gpus: []gpuFixture{
			{id: "gpu-ok", utilization: f64(50), memoryUsed: i64(100), memoryTotal: i64(200), temperature: nil, power: f64(75)},
			{id: "gpu-bad", unavailable: true},
		},
		filesystems: []filesystemFixture{
			{id: "fs-ok", used: 10, total: 20},
			{id: "fs-bad", unavailable: true},
		},
		disks: []diskFixture{
			{id: "disk-ok", read: 1, write: 2},
			{id: "disk-bad", unavailable: true},
		},
		interfaces: []interfaceFixture{
			{id: "eth-ok", rx: 3, tx: 4},
			{id: "eth-bad", unavailable: true},
		},
	})
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 2, capturedAt: testBase.Add(10 * time.Second).UnixMilli(), payload: &perResource})))
	nullCount := func(metric, resourceID string) int64 {
		return scalar(t, fixture.db,
			"SELECT SUM(s.null_count) FROM points_10s s JOIN series c USING (series_id) WHERE c.metric = ? AND c.resource_id = ?",
			metric, resourceID)
	}
	for _, check := range [][2]string{
		{"gpu.utilization", "gpu-bad"}, {"gpu.memory.used", "gpu-bad"},
		{"gpu.temperature", "gpu-ok"}, {"fs.used", "fs-bad"}, {"fs.usage", "fs-bad"},
		{"fs.inodes.usage", "fs-bad"}, {"disk.read", "disk-bad"}, {"disk.write", "disk-bad"},
		{"disk.read.latency", "disk-bad"}, {"disk.write.latency", "disk-bad"}, {"disk.utilization", "disk-bad"},
		{"net.rx", "eth-bad"}, {"net.tx", "eth-bad"},
	} {
		if got := nullCount(check[0], check[1]); got != 1 {
			t.Fatalf("%s/%s null count = %d, want 1", check[0], check[1], got)
		}
	}
}

func TestIngestBreaksKnownFamiliesWhenTheyBecomeUnavailable(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	healthy := makePayload(payloadOptions{
		gpus:        []gpuFixture{{id: "gpu-a", utilization: f64(50), memoryUsed: i64(100), memoryTotal: i64(200), temperature: f64(60), power: f64(70)}},
		filesystems: []filesystemFixture{{id: "fs-a", used: 10, total: 20}},
		disks:       []diskFixture{{id: "disk-a", read: 1, write: 2}},
		interfaces:  []interfaceFixture{{id: "eth-a", rx: 3, tx: 4}},
	})
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 1, capturedAt: testBase.UnixMilli(), payload: &healthy})))
	down := makePayload(payloadOptions{unavailableGPUs: true, unavailableDisks: true, unavailableNetwork: true})
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 2, capturedAt: testBase.Add(5 * time.Second).UnixMilli(), payload: &down})))

	readings := func(metric, resourceID string) (int64, int64) {
		var count, nulls int64
		if err := fixture.db.QueryRow(testContext(t),
			"SELECT SUM(s.count), SUM(s.null_count) FROM points_10s s JOIN series c USING (series_id) "+
				"WHERE c.metric = ? AND c.resource_id = ?", metric, resourceID,
		).Scan(&count, &nulls); err != nil {
			t.Fatalf("read %s/%s: %v", metric, resourceID, err)
		}
		return count, nulls
	}
	for _, metric := range []string{"gpu.utilization", "gpu.memory.used", "gpu.memory.total", "gpu.temperature", "gpu.power"} {
		count, nulls := readings(metric, "gpu-a")
		if count != 1 || nulls != 1 {
			t.Fatalf("%s count=%d nulls=%d", metric, count, nulls)
		}
	}
	for _, check := range [][2]string{{"fs.total", "fs-a"}, {"disk.write", "disk-a"}, {"net.rx", "eth-a"}} {
		count, nulls := readings(check[0], check[1])
		if count != 1 || nulls != 1 {
			t.Fatalf("%s count=%d nulls=%d", check[0], count, nulls)
		}
	}
}

func TestIngestBreaksDisappearingOptionalMetrics(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	healthy := makePayload(payloadOptions{
		gpus: []gpuFixture{{
			id: "gpu-a", utilization: f64(50),
			junction: protocol.Some(72.0), vram: protocol.Some(70.0),
		}},
		diskPressureSome: protocol.Some(5.0),
		diskPressureFull: protocol.Some(1.0),
	})
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{
		sequence: 1, capturedAt: testBase.UnixMilli(), payload: &healthy,
	})))
	// The GPU stays available but stops reporting its optional temperatures,
	// and PSI pressure becomes unsupported while the disk family is healthy.
	sparse := makePayload(payloadOptions{
		gpus: []gpuFixture{{id: "gpu-a", utilization: f64(60)}},
	})
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{
		sequence: 2, capturedAt: testBase.Add(10 * time.Second).UnixMilli(), payload: &sparse,
	})))
	for _, check := range []struct{ metric, resource string }{
		{"gpu.junction.temperature", "gpu-a"},
		{"gpu.vram.temperature", "gpu-a"},
		{"io.pressure.some", ""},
		{"io.pressure.full", ""},
	} {
		var nulls int64
		if err := fixture.db.QueryRow(testContext(t),
			"SELECT SUM(s.null_count) FROM points_10s s JOIN series c USING (series_id) WHERE c.metric = ? AND c.resource_id = ?",
			check.metric, check.resource,
		).Scan(&nulls); err != nil {
			t.Fatalf("read %s: %v", check.metric, err)
		}
		if nulls != 1 {
			t.Fatalf("%s/%s null count = %d, want 1 explicit unavailable point", check.metric, check.resource, nulls)
		}
	}
}

func TestIngestLiveSelectionUsesCursorOrdering(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	// Same run: the higher sequence wins even when its capture time is earlier
	// than another record in the same batch.
	result := fixture.ingest(t, liveBatch(
		makeMetricsRecord(recordOptions{sequence: 5, capturedAt: testBase.Add(10 * time.Second).UnixMilli()}),
		makeMetricsRecord(recordOptions{sequence: 6, capturedAt: testBase.Add(5 * time.Second).UnixMilli()}),
	))
	if result.NewLive == nil || result.NewLive.Sequence != 6 {
		t.Fatalf("new live = %+v, want sequence 6", result.NewLive)
	}
	var sequence int64
	if err := fixture.db.QueryRow(testContext(t), "SELECT seq FROM machine_latest WHERE machine_id = 'machine-1'").Scan(&sequence); err != nil {
		t.Fatalf("latest: %v", err)
	}
	if sequence != 6 {
		t.Fatalf("persisted live sequence = %d", sequence)
	}
}

func TestSeriesCacheIsBounded(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{MaxCachedSeries: 3})
	payload := makePayload(payloadOptions{cores: 8})
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{
		sequence: 1, capturedAt: testBase.UnixMilli(), payload: &payload,
	})))
	if size := fixture.store.catalog.size(); size > 3 {
		t.Fatalf("catalog size = %d, want at most 3", size)
	}
	var seriesRows int64
	if err := fixture.db.QueryRow(testContext(t), "SELECT COUNT(*) FROM series").Scan(&seriesRows); err != nil {
		t.Fatalf("count series: %v", err)
	}
	// A second sample for the same evicted series must reuse the durable row.
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{
		runID: "run-2", sequence: 1, capturedAt: testBase.Add(10 * time.Second).UnixMilli(), payload: &payload,
	})))
	var after int64
	if err := fixture.db.QueryRow(testContext(t), "SELECT COUNT(*) FROM series").Scan(&after); err != nil {
		t.Fatalf("count series after: %v", err)
	}
	if after != seriesRows {
		t.Fatalf("series rows grew from %d to %d on cache misses", seriesRows, after)
	}
	if size := fixture.store.catalog.size(); size > 3 {
		t.Fatalf("catalog size after = %d", size)
	}
}

func TestIngestStoresGapsOnce(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	gap := makeGapRecord(gapOptions{
		sequence: 1, capturedAt: testBase.Add(10 * time.Second).UnixMilli(),
		fromMs: testBase.UnixMilli(), toMs: testBase.Add(9 * time.Second).UnixMilli(), dropped: 3,
	})
	fixture.ingest(t, liveBatch(gap))
	fixture.ingest(t, replayBatch(gap))
	if got := scalar(t, fixture.db, "SELECT COUNT(*) FROM gaps"); got != 1 {
		t.Fatalf("gaps = %d", got)
	}
	if fixture.store.Stats().DuplicateRecords != 1 {
		t.Fatalf("stats = %+v", fixture.store.Stats())
	}
}

func TestIngestQueueBoundIsImmediateBusy(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{MaxQueuedRecords: 5, FlushInterval: 5 * time.Second})
	records := make([]protocol.TelemetryRecord, 6)
	for index := range records {
		records[index] = makeMetricsRecord(recordOptions{sequence: int64(index + 1), capturedAt: testBase.Add(time.Duration(index) * time.Second).UnixMilli()})
	}
	_, err := fixture.store.Enqueue(testContext(t), liveBatch(records...))
	if CodeOf(err) != CodeBusy {
		t.Fatalf("queue bound error = %v", err)
	}
	if retry := RetryAfter(err); retry < time.Second || retry > 5*time.Second {
		t.Fatalf("retryAfter = %v", retry)
	}
	if fixture.store.Stats().QueuedRecords != 0 {
		t.Fatalf("stats = %+v", fixture.store.Stats())
	}
	if scalar(t, fixture.db, "SELECT COUNT(*) FROM sample_receipts") != 0 {
		t.Fatal("rejected queue batch wrote receipts")
	}
}

func TestIngestFailureRollsBackAndRecovers(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	if err := fixture.db.Write(testContext(t), func(tx *Tx) error {
		_, err := tx.Exec("CREATE TRIGGER injected_failure BEFORE INSERT ON points_10s BEGIN SELECT RAISE(ABORT, 'injected write failure'); END")
		return err
	}); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	batch := liveBatch(
		makeMetricsRecord(recordOptions{sequence: 1, capturedAt: testBase.UnixMilli()}),
		makeMetricsRecord(recordOptions{sequence: 2, capturedAt: testBase.Add(5 * time.Second).UnixMilli()}),
	)
	_, err := fixture.store.Enqueue(testContext(t), batch)
	if CodeOf(err) != CodeBusy {
		t.Fatalf("failure error = %v", err)
	}
	if !strings.Contains(err.Error(), "storage write failed") {
		t.Fatalf("error = %v", err)
	}
	stats := fixture.store.Stats()
	if stats.WriteFailures != 1 || !strings.Contains(stats.LastWriteError, "injected write failure") {
		t.Fatalf("stats = %+v", stats)
	}
	if scalar(t, fixture.db, "SELECT COUNT(*) FROM sample_receipts") != 0 {
		t.Fatal("failed transaction left receipts")
	}
	if scalar(t, fixture.db, "SELECT COUNT(*) FROM series") != 0 {
		t.Fatal("failed transaction left catalog rows")
	}
	if err := fixture.db.Write(testContext(t), func(tx *Tx) error {
		_, err := tx.Exec("DROP TRIGGER injected_failure")
		return err
	}); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	recovered := fixture.ingest(t, batch)
	if len(recovered.Accepted) != 2 {
		t.Fatalf("recovered = %+v", recovered)
	}
	if scalar(t, fixture.db, "SELECT COUNT(*) FROM sample_receipts") != 2 {
		t.Fatal("recovered receipts missing")
	}
	if fixture.store.Stats().WriteFailures != 1 {
		t.Fatal("historical failure count was lost")
	}
	if scalar(t, fixture.db, "SELECT COUNT(*) FROM series") == 0 {
		t.Fatal("series were not cached after recovery")
	}
}

func TestIngestRestoresLatestAndRejectsReceiptDuplicatesAcrossRestart(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	record := makeMetricsRecord(recordOptions{bootID: "boot-9", sequence: 7, capturedAt: testBase.UnixMilli()})
	batch := liveBatch(record)
	batch.Hostname = stringPtr("workstation-1")
	batch.ExpectedIntervalMs = i64(5000)
	fixture.ingest(t, batch)
	before, err := fixture.store.LatestLive(testContext(t))
	if err != nil || len(before) != 1 {
		t.Fatalf("latest = %+v, %v", before, err)
	}
	if before[0].BootID != "boot-9" || before[0].Sequence != 7 || before[0].Hostname == nil || *before[0].Hostname != "workstation-1" ||
		before[0].ExpectedIntervalMs == nil || *before[0].ExpectedIntervalMs != 5000 {
		t.Fatalf("latest = %+v", before[0])
	}
	if err := fixture.store.Close(testContext(t)); err != nil {
		t.Fatalf("close telemetry: %v", err)
	}
	if err := fixture.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	db, err := Open(testContext(t), fixture.path, Options{})
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()
	clock := fixture.clock
	db.now = clock.now
	store, err := NewTelemetry(testContext(t), db, TelemetryOptions{
		Now: clock.now, FlushInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("reopen telemetry: %v", err)
	}
	defer store.Close(testContext(t))
	restored, err := store.LatestLive(testContext(t))
	if err != nil || len(restored) != 1 {
		t.Fatalf("restored = %+v, %v", restored, err)
	}
	if restored[0].BootID != "boot-9" || restored[0].RunID != "run-1" || restored[0].Sequence != 7 ||
		!restored[0].ReceivedAt.Equal(before[0].ReceivedAt) || !restored[0].CapturedAt.Equal(before[0].CapturedAt) {
		t.Fatalf("restored = %+v, before = %+v", restored[0], before[0])
	}
	replayed, err := store.Enqueue(testContext(t), batch)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if replayed.NewLive != nil {
		t.Fatal("lost-ack retry regressed live state")
	}
	after, _ := store.LatestLive(testContext(t))
	if !after[0].ReceivedAt.Equal(before[0].ReceivedAt) {
		t.Fatalf("retry moved receivedAt: %v -> %v", before[0].ReceivedAt, after[0].ReceivedAt)
	}
}

func TestInventoryPersistsAcrossRestart(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	inventory := makeInventoryFixture()
	if err := fixture.store.SaveInventory(testContext(t), "machine-1", testBase, inventory); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	stored, err := fixture.store.LoadInventory(testContext(t), "machine-1")
	if err != nil || stored == nil {
		t.Fatalf("load inventory = %+v, %v", stored, err)
	}
	if stored.Inventory.CPU.LogicalCores != 2 || !stored.CapturedAt.Equal(testBase) {
		t.Fatalf("inventory = %+v", stored)
	}
	missing, err := fixture.store.LoadInventory(testContext(t), "missing")
	if err != nil || missing != nil {
		t.Fatalf("missing inventory = %+v, %v", missing, err)
	}
}

func TestCloseDrainsQueuedWork(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{FlushInterval: 5 * time.Second})
	done := make(chan error, 1)
	go func() {
		_, err := fixture.store.Enqueue(testContext(t), liveBatch(makeMetricsRecord(recordOptions{sequence: 1, capturedAt: testBase.UnixMilli()})))
		done <- err
	}()
	// Give Enqueue time to hand the batch to the writer, then close.
	time.Sleep(20 * time.Millisecond)
	if err := fixture.store.Close(testContext(t)); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("enqueue during close: %v", err)
	}
	if scalar(t, fixture.db, "SELECT COUNT(*) FROM sample_receipts") != 1 {
		t.Fatal("queued work was not drained on close")
	}
}

func TestCloseHonoursItsContext(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{FlushInterval: 5 * time.Second})
	// Hold the writer so the flush cannot commit and Close has to wait.
	fixture.db.writeMu.Lock()
	enqueued := make(chan error, 1)
	go func() {
		_, err := fixture.store.Enqueue(testContext(t), liveBatch(
			makeMetricsRecord(recordOptions{sequence: 1, capturedAt: testBase.UnixMilli()}),
		))
		enqueued <- err
	}()
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := fixture.store.Close(ctx)
	elapsed := time.Since(started)
	fixture.db.writeMu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close error = %v, want deadline exceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("close blocked for %v", elapsed)
	}
	select {
	case <-enqueued:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue did not return after close")
	}
}

func TestDiskBudgetRejectsWithBusyAndDegradedState(t *testing.T) {
	dir := privateTempDir(t)
	path := dir + "/hub.db"
	db, err := Open(testContext(t), path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	clock := &testClock{}
	clock.set(testBase.Add(time.Minute))
	db.now = clock.now
	baseline := db.SizeBytes()
	capBytes := baseline + 512*1024
	store, err := NewTelemetry(testContext(t), db, TelemetryOptions{
		Now:                      clock.now,
		FlushInterval:            time.Millisecond,
		MaxPhysicalBytes:         &capBytes,
		MaxRecordsPerTransaction: 4,
		Retention: RetentionOverrides{
			TenSecond: durationPtr(100 * time.Hour),
			OneMinute: durationPtr(100 * time.Hour),
			OneHour:   durationPtr(100 * time.Hour),
			Receipts:  durationPtr(100 * time.Hour),
			Gaps:      durationPtr(100 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("new telemetry: %v", err)
	}
	defer store.Close(testContext(t))
	payload := largePayload()
	committed, busy := 0, 0
	for index := 0; index < 400; index++ {
		batch := liveBatch()
		for offset := 0; offset < 8; offset++ {
			sequence := int64(index*8 + offset + 1)
			batch.Records = append(batch.Records, makeMetricsRecord(recordOptions{
				sequence: sequence, capturedAt: testBase.Add(time.Duration(sequence) * time.Second).UnixMilli(), payload: &payload,
			}))
		}
		_, err := store.Enqueue(testContext(t), batch)
		if err == nil {
			committed += len(batch.Records)
		} else if CodeOf(err) == CodeBusy {
			busy++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if committed <= 20 || busy == 0 {
		t.Fatalf("committed=%d busy=%d", committed, busy)
	}
	stats := store.Stats()
	if stats.DBBytes > capBytes {
		t.Fatalf("db bytes %d > cap %d", stats.DBBytes, capBytes)
	}
	if !stats.Degraded || stats.DiskFullFailures == 0 {
		t.Fatalf("degraded state = %+v", stats)
	}
	if !strings.Contains(strings.ToLower(stats.LastWriteError), "disk budget") {
		t.Fatalf("last write error = %q", stats.LastWriteError)
	}
	if physical := db.PhysicalLimit(); physical != nil && stats.DBWALBytes > physical.WALBudgetBytes {
		t.Fatalf("WAL %d > budget %d", stats.DBWALBytes, physical.WALBudgetBytes)
	}
	if got := scalar(t, db, "SELECT COUNT(*) FROM sample_receipts"); got != int64(committed) {
		t.Fatalf("receipts = %d, committed = %d", got, committed)
	}
}

func TestConcurrentIngestQueryAndReads(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{
		FlushInterval:    2 * time.Millisecond,
		MaxQueuedRecords: 10_000,
	})
	users := NewUsers(fixture.db)
	if err := users.Create(testContext(t), "owner", User{Username: "owner", PasswordHash: "hash", Scope: AllMachines(), Admin: true}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	var wait sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := 0; index < 40; index++ {
				record := makeMetricsRecord(recordOptions{
					runID:      fmt.Sprintf("run-%d", worker),
					sequence:   int64(index + 1),
					capturedAt: testBase.Add(time.Duration(index) * time.Second).UnixMilli(),
					payload:    payloadWithUsage(float64(index % 100)),
				})
				_, _ = fixture.store.Enqueue(context.Background(), liveBatch(record))
			}
		}(worker)
	}
	for worker := 0; worker < 4; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := 0; index < 20; index++ {
				_, _ = fixture.store.Query(context.Background(), HistoryQuery{
					MachineID: "machine-1",
					Metrics:   []string{"cpu.usage"},
					FromMs:    testBase.UnixMilli(),
					ToMs:      testBase.Add(10 * time.Minute).UnixMilli(),
				}, HistoryQueryBudget{})
				_, _ = users.List(context.Background())
				_ = fixture.store.Stats()
			}
		}()
	}
	wait.Wait()
	if err := fixture.store.Flush(testContext(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := scalar(t, fixture.db, "SELECT COUNT(*) FROM sample_receipts"); got != 160 {
		t.Fatalf("receipts = %d, want 160", got)
	}
}

func payloadWithUsage(value float64) *protocol.MetricsPayload {
	payload := makePayload(payloadOptions{cpuUsage: f64(value)})
	return &payload
}

func largePayload() protocol.MetricsPayload {
	return makePayload(payloadOptions{
		cores: 32,
		gpus: func() []gpuFixture {
			out := make([]gpuFixture, 8)
			for index := range out {
				out[index] = gpuFixture{
					id: fmt.Sprintf("GPU-%d", index), utilization: f64(float64(index * 10)),
					memoryUsed: i64(1000), memoryTotal: i64(2000),
				}
			}
			return out
		}(),
		filesystems: func() []filesystemFixture {
			out := make([]filesystemFixture, 16)
			for index := range out {
				out[index] = filesystemFixture{id: fmt.Sprintf("fs-%d", index), used: 1, total: 2}
			}
			return out
		}(),
		disks: func() []diskFixture {
			out := make([]diskFixture, 4)
			for index := range out {
				out[index] = diskFixture{id: fmt.Sprintf("disk-%d", index)}
			}
			return out
		}(),
		interfaces: func() []interfaceFixture {
			out := make([]interfaceFixture, 8)
			for index := range out {
				out[index] = interfaceFixture{id: fmt.Sprintf("eth-%d", index)}
			}
			return out
		}(),
	})
}

func durationPtr(value time.Duration) *time.Duration { return &value }

// TestCaptureRejectionMatchesIngestPolicy pins the exported eligibility helper
// to the same replay-age and clock-skew policy the commit path applies.
func TestCaptureRejectionMatchesIngestPolicy(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{
		MaxReplayAge: 2 * time.Hour,
		ClockSkew:    30 * time.Minute,
	})
	now := fixture.clock.now()
	format := func(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }
	cases := []struct {
		name     string
		captured string
		want     string
	}{
		{name: "inside the horizon", captured: format(now.Add(-time.Hour)), want: ""},
		{name: "at the horizon", captured: format(now.Add(-2 * time.Hour)), want: ""},
		{name: "older than the horizon", captured: format(now.Add(-2*time.Hour - time.Second)), want: RejectionCapturedTooOld},
		{name: "inside the skew allowance", captured: format(now.Add(30 * time.Minute)), want: ""},
		{name: "ahead of the skew allowance", captured: format(now.Add(31 * time.Minute)), want: RejectionCapturedInFuture},
		{name: "not a timestamp", captured: "yesterday", want: "capturedAt is not a parseable timestamp"},
	}
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) {
			if got := fixture.store.CaptureRejection("c-1", scenario.captured, now); got != scenario.want {
				t.Fatalf("CaptureRejection = %q, want %q", got, scenario.want)
			}
		})
	}
}
