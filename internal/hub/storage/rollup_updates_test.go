package storage

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// rollupRow is one stored summary row. Min and max are NULL for a bucket that
// has seen only unavailable readings.
type rollupRow struct {
	seriesID, bucketMS int64
	min, max           sql.NullFloat64
	sum                float64
	count, nulls       int64
}

var rollupTables = []string{"points_10s", "points_1m", "points_1h"}

func readRollups(t testing.TB, db *Database, table string) []rollupRow {
	t.Helper()
	rows, err := db.Query(context.Background(),
		"SELECT series_id, bucket_ms, min, max, sum, count, null_count FROM "+table+" ORDER BY series_id, bucket_ms")
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	var out []rollupRow
	for rows.Next() {
		var row rollupRow
		if err := rows.Scan(&row.seriesID, &row.bucketMS, &row.min, &row.max, &row.sum, &row.count, &row.nulls); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", table, err)
	}
	return out
}

func seedRollups(t testing.TB, db *Database, table string, rows []rollupRow) {
	t.Helper()
	err := db.Write(context.Background(), func(tx *Tx) error {
		for _, row := range rows {
			if _, err := tx.Exec("INSERT INTO "+table+" (series_id, bucket_ms, min, max, sum, count, null_count) VALUES (?, ?, ?, ?, ?, ?, ?)",
				row.seriesID, row.bucketMS, row.min, row.max, row.sum, row.count, row.nulls); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed %s: %v", table, err)
	}
}

// writeSummariesCountingChanges runs writeSummaries in one real write
// transaction and returns the number of rows SQLite reports as inserted or
// updated by it. Each rollup UPSERT changes exactly one row, so this is the
// number of rollup row writes the transaction performed.
func writeSummariesCountingChanges(t testing.TB, db *Database, write func(*Tx, map[int64]map[int64]*summaryAggregate) error, summaries map[int64]map[int64]*summaryAggregate) int64 {
	t.Helper()
	var changes int64
	err := db.Write(context.Background(), func(tx *Tx) error {
		var before, after int64
		if err := tx.QueryRow("SELECT total_changes()").Scan(&before); err != nil {
			return err
		}
		if err := write(tx, summaries); err != nil {
			return err
		}
		if err := tx.QueryRow("SELECT total_changes()").Scan(&after); err != nil {
			return err
		}
		changes = after - before
		return nil
	})
	if err != nil {
		t.Fatalf("write summaries: %v", err)
	}
	return changes
}

// perSourceWriteSummaries is the previous behaviour, kept only as a reference
// oracle: one UPSERT into every tier for every 10-second source bucket.
func perSourceWriteSummaries(tx *Tx, summaries map[int64]map[int64]*summaryAggregate) error {
	steps := [3]int64{summaryStepMS, history1mStepMS, history1hStepMS}
	statements := make([]*sql.Stmt, 0, len(rollupTables))
	defer func() {
		for _, statement := range statements {
			_ = statement.Close()
		}
	}()
	for _, table := range rollupTables {
		statement, err := tx.Prepare(fmt.Sprintf(rollupUpsertTemplate, table))
		if err != nil {
			return err
		}
		statements = append(statements, statement)
	}
	for seriesID, buckets := range summaries {
		for bucketMS, aggregate := range buckets {
			var minimum, maximum any
			if aggregate.count > 0 {
				minimum, maximum = aggregate.min, aggregate.max
			}
			for index, statement := range statements {
				target := bucketMS / steps[index] * steps[index]
				if _, err := statement.Exec(seriesID, target, minimum, maximum, aggregate.sum, aggregate.count, aggregate.nulls); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func cloneSummaries(in map[int64]map[int64]*summaryAggregate) map[int64]map[int64]summaryAggregate {
	out := make(map[int64]map[int64]summaryAggregate, len(in))
	for seriesID, buckets := range in {
		out[seriesID] = make(map[int64]summaryAggregate, len(buckets))
		for bucketMS, aggregate := range buckets {
			out[seriesID][bucketMS] = *aggregate
		}
	}
	return out
}

func nullFloat(value float64) sql.NullFloat64 { return sql.NullFloat64{Float64: value, Valid: true} }

// An hour boundary that is also a minute boundary, so one batch spans two
// minute and two hour targets.
var rollupHour = testBase.Truncate(time.Hour).UnixMilli()

func TestWriteSummariesMergesTargetsWithStoredRowsAndNulls(t *testing.T) {
	db, _ := openTestDatabase(t)
	minuteBefore := rollupHour - history1mStepMS
	hourBefore := rollupHour - history1hStepMS

	// Stored rows from earlier transactions: a valued minute, a null-only
	// minute and a valued hour.
	seedRollups(t, db, "points_1m", []rollupRow{
		{seriesID: 1, bucketMS: minuteBefore, min: nullFloat(4), max: nullFloat(6), sum: 10, count: 2, nulls: 1},
		{seriesID: 2, bucketMS: rollupHour, nulls: 3},
	})
	seedRollups(t, db, "points_1h", []rollupRow{
		{seriesID: 1, bucketMS: hourBefore, min: nullFloat(1), max: nullFloat(9), sum: 50, count: 5},
	})

	summaries := map[int64]map[int64]*summaryAggregate{
		1: {
			// Three source buckets in the last minute of the previous hour: one
			// null-only, then values that lower min and raise max.
			rollupHour - 30_000: {nulls: 2},
			rollupHour - 20_000: {min: 3, max: 3, sum: 3, count: 1},
			rollupHour - 10_000: {min: 2, max: 8, sum: 15, count: 3, nulls: 1},
			// Two in the first minute of the new hour.
			rollupHour:          {min: 7, max: 7, sum: 7, count: 1},
			rollupHour + 50_000: {min: 5, max: 6, sum: 11, count: 2},
		},
		2: {
			// Null-only into a null-only stored minute, then a value in the
			// same minute: min/max must come from the value alone.
			rollupHour + 10_000: {nulls: 1},
			rollupHour + 20_000: {min: -1, max: -1, sum: -1, count: 1},
		},
		3: {
			// Only nulls everywhere: min/max stay NULL in every tier.
			rollupHour + 10_000: {nulls: 4},
			rollupHour + 40_000: {nulls: 1},
		},
	}
	before := cloneSummaries(summaries)
	changes := writeSummariesCountingChanges(t, db, writeSummaries, summaries)
	if !reflect.DeepEqual(cloneSummaries(summaries), before) {
		t.Fatal("writeSummaries modified its input aggregates")
	}
	// 9 source buckets; minute targets: s1 {prev, cur}, s2 {cur}, s3 {cur};
	// hour targets: s1 {prev, cur}, s2 {cur}, s3 {cur}.
	if want := int64(9 + 4 + 4); changes != want {
		t.Fatalf("rollup row writes = %d, want %d", changes, want)
	}

	wantMinutes := []rollupRow{
		{seriesID: 1, bucketMS: minuteBefore, min: nullFloat(2), max: nullFloat(8), sum: 28, count: 6, nulls: 4},
		{seriesID: 1, bucketMS: rollupHour, min: nullFloat(5), max: nullFloat(7), sum: 18, count: 3},
		{seriesID: 2, bucketMS: rollupHour, min: nullFloat(-1), max: nullFloat(-1), sum: -1, count: 1, nulls: 4},
		{seriesID: 3, bucketMS: rollupHour, nulls: 5},
	}
	if got := readRollups(t, db, "points_1m"); !reflect.DeepEqual(got, wantMinutes) {
		t.Fatalf("points_1m =\n%+v\nwant\n%+v", got, wantMinutes)
	}
	wantHours := []rollupRow{
		{seriesID: 1, bucketMS: hourBefore, min: nullFloat(1), max: nullFloat(9), sum: 68, count: 9, nulls: 3},
		{seriesID: 1, bucketMS: rollupHour, min: nullFloat(5), max: nullFloat(7), sum: 18, count: 3},
		{seriesID: 2, bucketMS: rollupHour, min: nullFloat(-1), max: nullFloat(-1), sum: -1, count: 1, nulls: 1},
		{seriesID: 3, bucketMS: rollupHour, nulls: 5},
	}
	if got := readRollups(t, db, "points_1h"); !reflect.DeepEqual(got, wantHours) {
		t.Fatalf("points_1h =\n%+v\nwant\n%+v", got, wantHours)
	}
	if got := len(readRollups(t, db, "points_10s")); got != 9 {
		t.Fatalf("points_10s rows = %d, want 9", got)
	}

	// A second transaction with the same aggregates accumulates (receipts,
	// not writeSummaries, are what deduplicate replays).
	writeSummariesCountingChanges(t, db, writeSummaries, summaries)
	got := readRollups(t, db, "points_1m")
	if got[1].count != 6 || got[1].sum != 36 || got[3].nulls != 10 {
		t.Fatalf("second write did not accumulate into stored minutes: %+v", got)
	}
}

// randomRollupInput builds a seeded mix of valued, partly-null and null-only
// source buckets across minute and hour boundaries, with values that are
// multiples of 0.25 so float sums are exact regardless of addition order.
func randomRollupInput(rng *rand.Rand, series, spanBuckets int) map[int64]map[int64]*summaryAggregate {
	start := rollupHour - int64(spanBuckets/2)*summaryStepMS
	summaries := make(map[int64]map[int64]*summaryAggregate, series)
	for seriesID := int64(1); seriesID <= int64(series); seriesID++ {
		buckets := make(map[int64]*summaryAggregate)
		for index := 0; index < spanBuckets; index++ {
			if rng.Intn(4) == 0 {
				continue // sparse: not every source bucket is present
			}
			aggregate := &summaryAggregate{}
			readings := 1 + rng.Intn(3)
			for range readings {
				if rng.Intn(3) == 0 {
					aggregate.nulls++
					continue
				}
				value := float64(rng.Intn(400)-200) / 4
				if aggregate.count == 0 || value < aggregate.min {
					aggregate.min = value
				}
				if aggregate.count == 0 || value > aggregate.max {
					aggregate.max = value
				}
				aggregate.sum += value
				aggregate.count++
			}
			buckets[start+int64(index)*summaryStepMS] = aggregate
		}
		summaries[seriesID] = buckets
	}
	return summaries
}

func TestWriteSummariesMatchesPerSourceUpsertsWithFewerRowWrites(t *testing.T) {
	rng := rand.New(rand.NewSource(20261113))
	merged, _ := openTestDatabase(t)
	reference, _ := openTestDatabase(t)

	// Identical pre-existing rows in both databases, including null-only ones
	// and rows for series the batch never touches.
	for _, db := range []*Database{merged, reference} {
		seedRollups(t, db, "points_1m", []rollupRow{
			{seriesID: 1, bucketMS: rollupHour - history1mStepMS, min: nullFloat(-80), max: nullFloat(90), sum: 12.5, count: 7, nulls: 2},
			{seriesID: 2, bucketMS: rollupHour, nulls: 6},
			{seriesID: 99, bucketMS: rollupHour, min: nullFloat(1), max: nullFloat(1), sum: 1, count: 1},
		})
		seedRollups(t, db, "points_1h", []rollupRow{
			{seriesID: 1, bucketMS: rollupHour - history1hStepMS, min: nullFloat(-100), max: nullFloat(100), sum: 3.75, count: 40, nulls: 9},
			{seriesID: 3, bucketMS: rollupHour, nulls: 11},
		})
	}

	var mergedChanges, referenceChanges int64
	for round := range 3 {
		input := randomRollupInput(rng, 12, 900) // 75 minutes either side of an hour boundary
		mergedChanges += writeSummariesCountingChanges(t, merged, writeSummaries, input)
		referenceChanges += writeSummariesCountingChanges(t, reference, perSourceWriteSummaries, input)
		for _, table := range rollupTables {
			got, want := readRollups(t, merged, table), readRollups(t, reference, table)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round %d: %s differs from per-source upserts", round, table)
			}
		}
	}
	if mergedChanges*2 >= referenceChanges {
		t.Fatalf("merged rollups wrote %d rows, per-source wrote %d; expected well under half", mergedChanges, referenceChanges)
	}
	t.Logf("rollup row writes: merged %d, per-source %d", mergedChanges, referenceChanges)
}

// Through the real ingest path: a replay batch spanning minute and hour
// boundaries keeps every coarse tier equal to the fold of its 10-second rows,
// and redelivering it changes nothing.
func TestReplayAcrossBoundariesKeepsTiersConsistentAndIdempotent(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	var records []protocol.TelemetryRecord
	start := rollupHour - 2*history1mStepMS - 5_000
	for sequence := int64(1); sequence <= 40; sequence++ {
		options := payloadOptions{cpuUsage: f64(float64(sequence%7) / 2), coreValue: f64(float64(sequence % 5))}
		if sequence%3 == 0 {
			options.coreNullIndex = new(int)
		}
		// Temperature unavailable for a whole minute, so some buckets in
		// every tier are null-only.
		if sequence >= 12 && sequence <= 37 {
			options.cpuTemperature = protocol.Null[float64]()
		} else {
			options.cpuTemperature = protocol.Some(40 + float64(sequence%4))
		}
		payload := makePayload(options)
		records = append(records, makeMetricsRecord(recordOptions{
			sequence: sequence, capturedAt: start + (sequence-1)*5_000, payload: &payload,
		}))
	}
	fixture.ingest(t, replayBatch(records...))

	snapshot := func() [][]rollupRow {
		out := make([][]rollupRow, len(rollupTables))
		for index, table := range rollupTables {
			out[index] = readRollups(t, fixture.db, table)
		}
		return out
	}
	first := snapshot()

	steps := []int64{history1mStepMS, history1hStepMS}
	for index, table := range rollupTables[1:] {
		step := steps[index]
		query := "SELECT series_id, bucket_ms / ? * ?, MIN(min), MAX(max), SUM(sum), SUM(count), SUM(null_count) " +
			"FROM points_10s GROUP BY series_id, bucket_ms / ? ORDER BY series_id, bucket_ms / ?"
		rows, err := fixture.db.Query(testContext(t), query, step, step, step, step)
		if err != nil {
			t.Fatal(err)
		}
		var want []rollupRow
		for rows.Next() {
			var row rollupRow
			if err := rows.Scan(&row.seriesID, &row.bucketMS, &row.min, &row.max, &row.sum, &row.count, &row.nulls); err != nil {
				t.Fatal(err)
			}
			want = append(want, row)
		}
		rows.Close()
		if !reflect.DeepEqual(first[index+1], want) {
			t.Fatalf("%s is not the fold of points_10s", table)
		}
	}
	var nullOnly, hours int
	for _, row := range first[1] {
		if !row.min.Valid && row.nulls > 0 {
			nullOnly++
		}
	}
	seenHours := map[int64]bool{}
	for _, row := range first[2] {
		seenHours[row.bucketMS] = true
	}
	hours = len(seenHours)
	if nullOnly == 0 || hours != 2 || len(first[1]) <= len(first[2]) {
		t.Fatalf("fixture did not cover null-only minutes (%d) and two hours (%d)", nullOnly, hours)
	}

	fixture.ingest(t, replayBatch(records...))
	if !reflect.DeepEqual(snapshot(), first) {
		t.Fatal("redelivered replay changed stored rollups")
	}
}

// BenchmarkWriteSummaries measures one rollup transaction for a backfill-shaped
// flush (many 10-second buckets per series) and a live-shaped one (one bucket
// per series). rows/op is the SQLite total_changes() delta: the number of
// rollup UPSERTs that touched a row.
func BenchmarkWriteSummaries(b *testing.B) {
	shapes := []struct {
		name            string
		series, buckets int
	}{
		{"backfill_100x360", 100, 360},
		{"live_2000x1", 2000, 1},
	}
	writers := []struct {
		name  string
		write func(*Tx, map[int64]map[int64]*summaryAggregate) error
	}{
		{"merged", writeSummaries},
		{"per_source", perSourceWriteSummaries},
	}
	for _, shape := range shapes {
		input := make(map[int64]map[int64]*summaryAggregate, shape.series)
		for seriesID := int64(1); seriesID <= int64(shape.series); seriesID++ {
			buckets := make(map[int64]*summaryAggregate, shape.buckets)
			for index := 0; index < shape.buckets; index++ {
				value := float64(index % 17)
				buckets[rollupHour+int64(index)*summaryStepMS] = &summaryAggregate{min: value, max: value, sum: value, count: 1}
			}
			input[seriesID] = buckets
		}
		for _, writer := range writers {
			b.Run(shape.name+"/"+writer.name, func(b *testing.B) {
				dir := privateTempDir(b)
				db, err := Open(context.Background(), dir+"/hub.db", Options{})
				if err != nil {
					b.Fatal(err)
				}
				defer db.Close()
				var changes int64
				b.ResetTimer()
				for range b.N {
					changes += writeSummariesCountingChanges(b, db, writer.write, input)
				}
				b.StopTimer()
				b.ReportMetric(float64(changes)/float64(b.N), "rows/op")
			})
		}
	}
}
