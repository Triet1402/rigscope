package storage

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

func (f *telemetryFixture) setClock(at time.Time) {
	f.clock.set(at)
}

func diskHistoryFixture(t *testing.T, options TelemetryOptions) *telemetryFixture {
	t.Helper()
	options.Ring.Retention = time.Second
	return setupTelemetry(t, options)
}

func TestSummaryWritesOneCompactRowPerSeriesBucket(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	records := make([]protocol.TelemetryRecord, 0, 10)
	for index := 0; index < 10; index++ {
		records = append(records, makeMetricsRecord(recordOptions{
			sequence:   int64(index + 1),
			capturedAt: testBase.Add(time.Duration(index) * time.Second).UnixMilli(),
			payload:    payloadWithUsage(float64(10 + index*10)),
		}))
	}
	fixture.ingest(t, liveBatch(records...))
	summaries := scalar(t, fixture.db, "SELECT COUNT(*) FROM points_10s")
	if summaries <= 1 {
		t.Fatalf("summary rows = %d", summaries)
	}
	if scalar(t, fixture.db, "SELECT COUNT(*) FROM points_1m") != summaries {
		t.Fatal("one-minute rollup is not one row per 10s bucket")
	}
	if scalar(t, fixture.db, "SELECT COUNT(*) FROM points_1h") != summaries {
		t.Fatal("one-hour rollup is not one row per 10s bucket")
	}
	result, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: testBase.UnixMilli(), ToMs: testBase.Add(10 * time.Second).UnixMilli(),
	}, HistoryQueryBudget{Tiers: []Tier{Tier10s}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Tier != Tier10s {
		t.Fatalf("tier = %q", result.Tier)
	}
	if len(result.Series) != 1 || len(result.Series[0].Points) != 1 {
		t.Fatalf("series = %+v", result.Series)
	}
	point := result.Series[0].Points[0]
	if point.T != testBase.UnixMilli() || point.Min == nil || *point.Min != 10 || point.Max == nil || *point.Max != 100 || point.Avg == nil || *point.Avg != 55 {
		t.Fatalf("point = %+v", point)
	}
}

func TestHistoryKeepsMinuteRollupsOnAStableWeightedGrid(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	start := testBase.Truncate(3 * time.Minute).Add(-6 * time.Minute)
	fixture.setClock(start.Add(15 * time.Minute))
	offsets := []time.Duration{60 * time.Second, 70 * time.Second, 120 * time.Second, 180 * time.Second, 190 * time.Second}
	values := []float64{10, 30, 90, 40, 60}
	for index, offset := range offsets {
		fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{
			sequence:   int64(index + 1),
			capturedAt: start.Add(offset + time.Second).UnixMilli(),
			payload:    payloadWithUsage(values[index]),
		})))
	}
	query := func(offset time.Duration) HistoryResult {
		result, err := fixture.store.Query(testContext(t), HistoryQuery{
			MachineID: "machine-1", Metrics: []string{"cpu.usage"},
			FromMs: start.Add(offset).UnixMilli(), ToMs: start.Add(5*time.Minute + offset).UnixMilli(),
			MaxPoints: 2,
		}, HistoryQueryBudget{Tiers: []Tier{Tier1m}, MaxRowsScanned: 500})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		return result
	}
	before := query(15 * time.Second)
	after := query(16 * time.Second)
	if before.Tier != Tier1m || before.StepMs != 3*60_000 {
		t.Fatalf("tier/step = %q/%d", before.Tier, before.StepMs)
	}
	if len(before.Series) != 1 || len(before.Series[0].Points) != 2 {
		t.Fatalf("points = %+v", before.Series)
	}
	first, second := before.Series[0].Points[0], before.Series[0].Points[1]
	if first.T != start.UnixMilli() || !floatPtrEqual(first.Min, 10) || !floatPtrEqual(first.Max, 90) || !floatPtrEqual(first.Avg, 130.0/3) {
		t.Fatalf("first = %+v", first)
	}
	if second.T != start.Add(3*time.Minute).UnixMilli() || !floatPtrEqual(second.Min, 40) || !floatPtrEqual(second.Max, 60) || !floatPtrEqual(second.Avg, 50) {
		t.Fatalf("second = %+v", second)
	}
	if len(after.Series[0].Points) != 2 || !pointsEqual(after.Series[0].Points, before.Series[0].Points) {
		t.Fatalf("moving grid changed completed buckets: before=%+v after=%+v", before.Series[0].Points, after.Series[0].Points)
	}
}

func TestHistoryBoundsDenseBucketsToMaxPointsPlusOneEdge(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	start := testBase
	fixture.setClock(start.Add(2 * time.Hour))
	records := make([]protocol.TelemetryRecord, 0, 601)
	for index := 0; index < 601; index++ {
		value := 10.0
		if index%2 == 1 {
			value = 90
		}
		records = append(records, makeMetricsRecord(recordOptions{
			sequence:   int64(index + 1),
			capturedAt: start.Add(time.Duration(index)*10*time.Second + 500*time.Millisecond).UnixMilli(),
			payload:    payloadWithUsage(value),
		}))
	}
	fixture.ingest(t, liveBatch(records...))
	query := func(offset time.Duration) HistoryResult {
		result, err := fixture.store.Query(testContext(t), HistoryQuery{
			MachineID: "machine-1", Metrics: []string{"cpu.usage"},
			FromMs:    start.Add(500*time.Millisecond + offset).UnixMilli(),
			ToMs:      start.Add(6000*time.Second + 500*time.Millisecond + offset).UnixMilli(),
			MaxPoints: 600,
		}, HistoryQueryBudget{Tiers: []Tier{Tier10s}})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		return result
	}
	before := query(0)
	after := query(10 * time.Second)
	if len(before.Series[0].Points) != 601 {
		t.Fatalf("before points = %d", len(before.Series[0].Points))
	}
	if len(after.Series[0].Points) != 600 {
		t.Fatalf("after points = %d", len(after.Series[0].Points))
	}
	if !pointsEqual(after.Series[0].Points, before.Series[0].Points[1:]) {
		t.Fatal("advancing the window did not drop exactly the oldest bucket")
	}
}

func TestHistoryChoosesTierFromActualRows(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	from := testBase
	records := make([]protocol.TelemetryRecord, 0, 180)
	for index := 0; index < 180; index++ {
		records = append(records, makeMetricsRecord(recordOptions{
			sequence:   int64(index + 1),
			capturedAt: from.Add(time.Duration(index) * 20 * time.Second).UnixMilli(),
			payload:    payloadWithUsage(10),
		}))
	}
	fixture.ingest(t, liveBatch(records...))
	fixture.setClock(from.Add(time.Hour))
	// With enough budget the finest durable tier whose actual rows fit is
	// chosen.
	result, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: from.UnixMilli(), ToMs: from.Add(time.Hour).UnixMilli(),
	}, HistoryQueryBudget{MaxRowsScanned: 10_000})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Tier != Tier10s || result.StepMs != 10_000 {
		t.Fatalf("tier/step = %q/%d", result.Tier, result.StepMs)
	}
	if len(result.Series) == 0 || len(result.Series[0].Points) == 0 {
		t.Fatal("no points")
	}

	// A budget too small to even probe the finest tier falls back to a coarser
	// tier, reports the exceeded budget and does not scan the whole range.
	result, err = fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: from.UnixMilli(), ToMs: from.Add(time.Hour).UnixMilli(),
	}, HistoryQueryBudget{MaxRowsScanned: 100})
	if err != nil {
		t.Fatalf("budgeted query: %v", err)
	}
	if result.Tier == Tier1s || result.Tier == Tier10s {
		t.Fatalf("budgeted tier = %q, want a coarser fallback", result.Tier)
	}
	if !result.BudgetExceeded || !result.Truncated {
		t.Fatalf("budget marker = %+v", result)
	}

	// Dense data is measured from its actual receipt/point rows, not the
	// requested time range.
	denseFrom := from.Add(2 * time.Hour)
	dense := make([]protocol.TelemetryRecord, 0, 300)
	for index := 0; index < 300; index++ {
		dense = append(dense, makeMetricsRecord(recordOptions{
			runID:      "run-dense",
			sequence:   int64(index + 1),
			capturedAt: denseFrom.Add(time.Duration(index) * 100 * time.Millisecond).UnixMilli(),
			payload:    payloadWithUsage(10),
		}))
	}
	fixture.setClock(denseFrom.Add(time.Minute))
	fixture.ingest(t, liveBatch(dense...))
	result, err = fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: denseFrom.UnixMilli(), ToMs: denseFrom.Add(30 * time.Second).UnixMilli(),
	}, HistoryQueryBudget{MaxRowsScanned: 2})
	if err != nil {
		t.Fatalf("dense query: %v", err)
	}
	if !result.BudgetExceeded {
		t.Fatalf("dense budget marker = %+v", result)
	}
}

func TestHistoryUnavailableBucketAndResourceFilters(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	from := testBase
	unavailable := makePayload(payloadOptions{unavailableCPU: true})
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 1, capturedAt: from.UnixMilli(), payload: &unavailable})))
	result, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: from.UnixMilli(), ToMs: from.Add(10 * time.Second).UnixMilli(),
	}, HistoryQueryBudget{Tiers: []Tier{Tier10s}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	point := result.Series[0].Points[0]
	if point.Min != nil || point.Max != nil || point.Avg != nil {
		t.Fatalf("unavailable point = %+v", point)
	}

	minuteStart := from.Add(20 * time.Minute)
	fixture.setClock(minuteStart.Add(2 * time.Hour))
	payload := makePayload(payloadOptions{
		cpuUsage: f64(10),
		gpus: []gpuFixture{
			{id: "gpu-a", utilization: f64(1)},
			{id: "gpu-b", utilization: f64(80)},
		},
	})
	fixture.ingest(t, liveBatch(
		makeMetricsRecord(recordOptions{sequence: 2, capturedAt: minuteStart.UnixMilli(), payload: &payload}),
		makeMetricsRecord(recordOptions{sequence: 3, capturedAt: minuteStart.Add(10 * time.Second).UnixMilli(), payload: payloadWithUsage(90)}),
		makeMetricsRecord(recordOptions{sequence: 4, capturedAt: minuteStart.Add(20 * time.Second).UnixMilli(), payload: payloadWithUsage(50)}),
		makeMetricsRecord(recordOptions{sequence: 5, capturedAt: minuteStart.Add(70 * time.Second).UnixMilli(), payload: payloadWithUsage(70)}),
	))
	filtered, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"gpu.utilization"}, ResourceIDs: []string{"gpu-b"},
		FromMs: minuteStart.UnixMilli(), ToMs: minuteStart.Add(time.Minute).UnixMilli(),
	}, HistoryQueryBudget{Tiers: []Tier{Tier10s}})
	if err != nil {
		t.Fatalf("filtered query: %v", err)
	}
	if len(filtered.Series) != 1 || filtered.Series[0].ResourceID != "gpu-b" || filtered.Series[0].Unit != UnitPercent {
		t.Fatalf("filtered = %+v", filtered.Series)
	}
	oneMinute, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: minuteStart.UnixMilli(), ToMs: minuteStart.Add(2 * time.Minute).UnixMilli(),
	}, HistoryQueryBudget{Tiers: []Tier{Tier1m}, MaxRowsScanned: 1000})
	if err != nil {
		t.Fatalf("1m query: %v", err)
	}
	if oneMinute.Tier != Tier1m || len(oneMinute.Series[0].Points) != 2 {
		t.Fatalf("1m = %+v", oneMinute.Series)
	}
	if !floatPtrEqual(oneMinute.Series[0].Points[0].Avg, 50) {
		t.Fatalf("1m first avg = %v", oneMinute.Series[0].Points[0].Avg)
	}
	oneHour, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: minuteStart.UnixMilli(), ToMs: minuteStart.Add(2 * time.Minute).UnixMilli(),
	}, HistoryQueryBudget{Tiers: []Tier{Tier1h}, MaxRowsScanned: 1000})
	if err != nil {
		t.Fatalf("1h query: %v", err)
	}
	if oneHour.Tier != Tier1h || len(oneHour.Series[0].Points) != 1 || !floatPtrEqual(oneHour.Series[0].Points[0].Avg, 55) {
		t.Fatalf("1h = %+v", oneHour.Series)
	}
}

func TestHistoryCoreFilterAndTruncation(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	from := testBase
	fixture.setClock(from.Add(time.Minute))
	payload := makePayload(payloadOptions{cores: 128, coreValue: f64(35), memoryUsed: 200, memoryTotal: 1000})
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 1, capturedAt: from.UnixMilli(), payload: &payload})))
	query := HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.core", "memory.usage"},
		CoreIDs: []string{"core:127"},
		FromMs:  from.UnixMilli(), ToMs: from.Add(30 * time.Second).UnixMilli(),
	}
	selected, err := fixture.store.Query(testContext(t), query, HistoryQueryBudget{Tiers: []Tier{Tier10s}})
	if err != nil {
		t.Fatalf("selected: %v", err)
	}
	if len(selected.Series) != 2 || selected.Series[0].Metric != "cpu.core" || selected.Series[0].ResourceID != "core:127" ||
		selected.Series[1].Metric != "memory.usage" {
		t.Fatalf("selected = %+v", selected.Series)
	}
	if selected.Truncated {
		t.Fatal("selected query truncated")
	}
	query.CoreIDs = []string{}
	none, err := fixture.store.Query(testContext(t), query, HistoryQueryBudget{Tiers: []Tier{Tier10s}})
	if err != nil || len(none.Series) != 1 || none.Series[0].Metric != "memory.usage" {
		t.Fatalf("no cores = %+v, %v", none.Series, err)
	}
	query.CoreIDs = nil
	query.Metrics = MetricNames()
	all, err := fixture.store.Query(testContext(t), query, HistoryQueryBudget{Tiers: []Tier{Tier10s}, MaxSeries: 200})
	if err != nil {
		t.Fatalf("all metrics: %v", err)
	}
	found := false
	for _, series := range all.Series {
		if series.Metric == "cpu.core" && series.ResourceID == "core:127" {
			found = true
		}
	}
	if !found {
		t.Fatal("core:127 missing from all-metric query")
	}
	if _, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.core"}, CoreIDs: []string{"core:1024"},
		FromMs: from.UnixMilli(), ToMs: from.Add(time.Second).UnixMilli(),
	}, HistoryQueryBudget{}); CodeOf(err) != CodeInvalid {
		t.Fatalf("out of range core error = %v", err)
	}
	if _, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.core"}, CoreIDs: []string{"core:1", "core:1"},
		FromMs: from.UnixMilli(), ToMs: from.Add(time.Second).UnixMilli(),
	}, HistoryQueryBudget{}); CodeOf(err) != CodeInvalid {
		t.Fatalf("duplicate core error = %v", err)
	}

	// maxSeries truncates before reading points.
	truncated, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.core"},
		FromMs: from.UnixMilli(), ToMs: from.Add(10 * time.Second).UnixMilli(),
	}, HistoryQueryBudget{Tiers: []Tier{Tier10s}, MaxSeries: 2})
	if err != nil || !truncated.Truncated || len(truncated.Series) != 2 {
		t.Fatalf("truncated = %+v, %v", truncated, err)
	}
}

func TestHistoryReportsDroppedAndMissingGaps(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	from := testBase
	fixture.setClock(from.Add(10 * time.Minute))
	fixture.ingest(t, liveBatch(makeGapRecord(gapOptions{
		sequence: 1, capturedAt: from.Add(10 * time.Second).UnixMilli(),
		fromMs: from.Add(time.Second).UnixMilli(), toMs: from.Add(9 * time.Second).UnixMilli(),
		dropped: 4, reason: protocol.GapOutboxBytes,
	})))
	dropped, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: from.UnixMilli(), ToMs: from.Add(time.Minute).UnixMilli(),
	}, HistoryQueryBudget{Tiers: []Tier{Tier10s}})
	if err != nil {
		t.Fatalf("dropped query: %v", err)
	}
	if len(dropped.Gaps) != 1 {
		t.Fatalf("gaps = %+v", dropped.Gaps)
	}
	gap := dropped.Gaps[0]
	if gap.FromMs != from.Add(time.Second).UnixMilli() || gap.ToMs != from.Add(9*time.Second).UnixMilli() ||
		gap.Kind != GapDropped || gap.Reason != string(protocol.GapOutboxBytes) || gap.DroppedRecords != 4 {
		t.Fatalf("gap = %+v", gap)
	}

	// Two receipts a minute apart with a 5s cadence are a missing interval.
	other := from.Add(20 * time.Minute)
	fixture.setClock(other.Add(10 * time.Minute))
	batch := liveBatch(
		makeMetricsRecord(recordOptions{collectorID: "collector-2", runID: "run-2", sequence: 1, capturedAt: other.UnixMilli()}),
		makeMetricsRecord(recordOptions{collectorID: "collector-2", runID: "run-2", sequence: 2, capturedAt: other.Add(time.Minute).UnixMilli()}),
	)
	batch.MachineID = "machine-2"
	batch.CollectorID = "collector-2"
	batch.ExpectedIntervalMs = i64(5000)
	fixture.ingest(t, batch)
	missing, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-2", Metrics: []string{"cpu.usage"},
		FromMs: other.Add(-time.Minute).UnixMilli(), ToMs: other.Add(2 * time.Minute).UnixMilli(),
	}, HistoryQueryBudget{Tiers: []Tier{Tier10s}})
	if err != nil {
		t.Fatalf("missing query: %v", err)
	}
	if len(missing.Gaps) != 1 {
		t.Fatalf("missing gaps = %+v", missing.Gaps)
	}
	if missing.Gaps[0].FromMs != other.UnixMilli() || missing.Gaps[0].ToMs != other.Add(time.Minute).UnixMilli() || missing.Gaps[0].Kind != GapMissing {
		t.Fatalf("missing gap = %+v", missing.Gaps[0])
	}
}

func TestHistoryDetectsOutageCrossingQueryStartWithoutInventingEdges(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	start := testBase
	fixture.setClock(start.Add(10 * time.Minute))
	batch := liveBatch(
		makeMetricsRecord(recordOptions{sequence: 1, capturedAt: start.UnixMilli()}),
		makeMetricsRecord(recordOptions{sequence: 2, capturedAt: start.Add(time.Minute).UnixMilli()}),
	)
	fixture.ingest(t, batch)
	query := func(fromMs, toMs int64) []HistoryGap {
		result, err := fixture.store.Query(testContext(t), HistoryQuery{
			MachineID: "machine-1", Metrics: []string{"cpu.usage"}, FromMs: fromMs, ToMs: toMs,
		}, HistoryQueryBudget{Tiers: []Tier{Tier10s}})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		return result.Gaps
	}
	gaps := query(start.Add(20*time.Second).UnixMilli(), start.Add(90*time.Second).UnixMilli())
	if len(gaps) != 1 || gaps[0].FromMs != start.Add(20*time.Second).UnixMilli() || gaps[0].ToMs != start.Add(time.Minute).UnixMilli() {
		t.Fatalf("crossing gaps = %+v", gaps)
	}
	if gaps := query(start.Add(20*time.Second).UnixMilli(), start.Add(40*time.Second).UnixMilli()); len(gaps) != 0 {
		t.Fatalf("invented trailing gap: %+v", gaps)
	}
	if gaps := query(start.Add(60*time.Second).UnixMilli(), start.Add(90*time.Second).UnixMilli()); len(gaps) != 0 {
		t.Fatalf("invented leading gap: %+v", gaps)
	}
}

func TestPartiallyPopulatedRingFallsBackToDurableHistory(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	from := testBase
	fixture.setClock(from.Add(time.Minute))
	fixture.ingest(t, liveBatch(
		makeMetricsRecord(recordOptions{sequence: 1, capturedAt: from.Add(10 * time.Minute).UnixMilli(), payload: payloadWithUsage(10)}),
		makeMetricsRecord(recordOptions{sequence: 2, capturedAt: from.Add(11 * time.Minute).UnixMilli(), payload: payloadWithUsage(20)}),
	))
	// The ring only knows the last two minutes. A wider query must not answer
	// from it and silently hide the earlier part of the range.
	wider, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: from.UnixMilli(), ToMs: from.Add(12 * time.Minute).UnixMilli(),
	}, HistoryQueryBudget{})
	if err != nil {
		t.Fatalf("wider query: %v", err)
	}
	if wider.Tier != Tier10s {
		t.Fatalf("tier = %q, want durable 10s history", wider.Tier)
	}
	if len(wider.Series) != 1 || len(wider.Series[0].Points) == 0 {
		t.Fatalf("durable points = %+v", wider.Series)
	}
	// A range entirely inside the ring's coverage may still use 1s detail.
	inside, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: from.Add(10 * time.Minute).UnixMilli(), ToMs: from.Add(12 * time.Minute).UnixMilli(),
	}, HistoryQueryBudget{})
	if err != nil {
		t.Fatalf("inside query: %v", err)
	}
	if inside.Tier != Tier1s || len(inside.Series) != 1 {
		t.Fatalf("inside tier/series = %q/%+v", inside.Tier, inside.Series)
	}
}

func TestHistoryReportsCollectorRestartAsUnknownGap(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	start := testBase
	fixture.setClock(start.Add(10 * time.Minute))
	fixture.ingest(t, liveBatch(
		makeMetricsRecord(recordOptions{runID: "run-before", sequence: 7, capturedAt: start.UnixMilli()}),
		makeMetricsRecord(recordOptions{runID: "run-after", sequence: 1, capturedAt: start.Add(time.Second).UnixMilli()}),
	))
	result, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: start.Add(-time.Second).UnixMilli(), ToMs: start.Add(10 * time.Second).UnixMilli(),
	}, HistoryQueryBudget{Tiers: []Tier{Tier10s}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Gaps) != 1 {
		t.Fatalf("gaps = %+v", result.Gaps)
	}
	gap := result.Gaps[0]
	if gap.Kind != GapMissing || gap.Reason != GapReasonCollectorRestarted || gap.DroppedRecords != 0 {
		t.Fatalf("restart gap = %+v", gap)
	}
	if gap.FromMs != start.UnixMilli() || gap.ToMs != start.Add(time.Second).UnixMilli() {
		t.Fatalf("restart interval = %d..%d", gap.FromMs, gap.ToMs)
	}
}

func TestHistoryBoundsTheReceiptScanForGaps(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	base := testBase.UnixMilli()
	const receipts = 100_000
	if err := fixture.db.Write(testContext(t), func(tx *Tx) error {
		statement, err := tx.Prepare(
			"INSERT INTO sample_receipts (collector_id, run_id, seq, machine_id, kind, mode, captured_at_ms, received_at_ms, expected_interval_ms) " +
				"VALUES ('collector-1', 'run-bulk', ?, 'machine-1', 'metrics', 'live', ?, ?, 5000)",
		)
		if err != nil {
			return err
		}
		defer statement.Close()
		for index := 0; index < receipts; index++ {
			at := base + int64(index)*1000
			if _, err := statement.Exec(index+1, at, at); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed receipts: %v", err)
	}
	started := time.Now()
	result, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: base, ToMs: base + receipts*1000,
	}, HistoryQueryBudget{MaxRowsScanned: 1, MaxElapsed: time.Millisecond, Tiers: []Tier{Tier10s}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !result.BudgetExceeded {
		t.Fatalf("budget marker missing: %+v", result)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("bounded query took %v", elapsed)
	}
}

func TestHistoryPointTablesUsePrimaryKey(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 1, capturedAt: testBase.UnixMilli()})))
	for _, table := range []string{"points_10s", "points_1m", "points_1h"} {
		rows, err := fixture.db.Query(testContext(t),
			"EXPLAIN QUERY PLAN SELECT * FROM "+table+" WHERE series_id = ? AND bucket_ms >= ? AND bucket_ms <= ?",
			1, testBase.UnixMilli(), testBase.Add(time.Minute).UnixMilli(),
		)
		if err != nil {
			t.Fatalf("plan %s: %v", table, err)
		}
		text := ""
		for rows.Next() {
			var id, parent, notUsed int64
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				rows.Close()
				t.Fatalf("plan scan: %v", err)
			}
			text += detail + "\n"
		}
		rows.Close()
		if !strings.Contains(text, "USING PRIMARY KEY") {
			t.Fatalf("%s plan does not use its primary key: %s", table, text)
		}
		if strings.Contains(text, "SCAN "+table) {
			t.Fatalf("%s plan full-scans: %s", table, text)
		}
	}
}

func TestHistoryValidatesQueries(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	base := HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: 0, ToMs: 1000,
	}
	check := func(mutate func(*HistoryQuery)) error {
		query := base
		mutate(&query)
		_, err := fixture.store.Query(testContext(t), query, HistoryQueryBudget{})
		return err
	}
	cores := make([]string, 1024)
	for index := range cores {
		cores[index] = "core:" + itoa(index)
	}
	if err := check(func(query *HistoryQuery) { query.Metrics = []string{"cpu.core"}; query.CoreIDs = cores }); err != nil {
		t.Fatalf("max cores should be accepted: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*HistoryQuery)
	}{
		{"unknown metric", func(query *HistoryQuery) { query.Metrics = []string{"not.a.metric"} }},
		{"duplicate metric", func(query *HistoryQuery) { query.Metrics = []string{"cpu.usage", "cpu.usage"} }},
		{"too many metrics", func(query *HistoryQuery) { query.Metrics = append(MetricNames(), "cpu.usage") }},
		{"empty range", func(query *HistoryQuery) { query.FromMs = 1000 }},
		{"too long", func(query *HistoryQuery) { query.ToMs = 401 * 24 * 3_600_000 }},
		{"empty machine", func(query *HistoryQuery) { query.MachineID = "" }},
		{"too many resources", func(query *HistoryQuery) {
			ids := make([]string, 65)
			for index := range ids {
				ids[index] = "r" + itoa(index)
			}
			query.ResourceIDs = ids
		}},
	}
	for _, testCase := range cases {
		if err := check(testCase.mutate); CodeOf(err) != CodeInvalid {
			t.Fatalf("%s error = %v", testCase.name, err)
		}
	}
}

func TestRingAnswersRecentQueriesAndFoldsLateSamples(t *testing.T) {
	fixture := setupTelemetry(t, TelemetryOptions{})
	from := testBase
	fixture.ingest(t, liveBatch(
		makeMetricsRecord(recordOptions{sequence: 1, capturedAt: from.UnixMilli(), payload: payloadWithUsage(10)}),
		makeMetricsRecord(recordOptions{sequence: 2, capturedAt: from.Add(5 * time.Second).UnixMilli(), payload: payloadWithUsage(20)}),
	))
	duplicate := fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{
		sequence: 2, capturedAt: from.Add(5 * time.Second).UnixMilli(), payload: payloadWithUsage(999),
	})))
	if len(duplicate.Accepted) != 1 || fixture.store.Stats().DuplicateRecords != 1 {
		t.Fatalf("duplicate = %+v", duplicate)
	}
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 3, capturedAt: from.Add(time.Second).UnixMilli(), payload: payloadWithUsage(30)})))
	result, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: from.UnixMilli(), ToMs: from.Add(20 * time.Second).UnixMilli(),
	}, HistoryQueryBudget{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Tier != Tier1s {
		t.Fatalf("tier = %q, want 1s", result.Tier)
	}
	if len(result.Series[0].Points) != 3 {
		t.Fatalf("points = %+v", result.Series[0].Points)
	}
	if !floatPtrEqual(result.Series[0].Points[0].Min, 10) || !floatPtrEqual(result.Series[0].Points[0].Max, 10) ||
		!floatPtrEqual(result.Series[0].Points[0].Avg, 10) {
		t.Fatalf("first ring bucket = all=%s", formatPoints(result.Series[0].Points))
	}
	if !floatPtrEqual(result.Series[0].Points[1].Avg, 30) || !floatPtrEqual(result.Series[0].Points[2].Avg, 20) {
		t.Fatalf("late sample or duplicate handling = %s", formatPoints(result.Series[0].Points))
	}
	stats := fixture.store.RingStats()
	if stats.UsedBytes <= 0 || stats.Series == 0 || stats.Machines != 1 {
		t.Fatalf("ring stats = %+v", stats)
	}
}

func TestRestartKeepsSummariesAndDropsTheRing(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	from := testBase
	fixture.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 1, capturedAt: from.UnixMilli(), payload: payloadWithUsage(42)})))
	before, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: from.UnixMilli(), ToMs: from.Add(10 * time.Second).UnixMilli(),
	}, HistoryQueryBudget{})
	if err != nil || len(before.Series) != 1 {
		t.Fatalf("before = %+v, %v", before, err)
	}
	if err := fixture.store.Close(testContext(t)); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := fixture.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	db, err := Open(testContext(t), fixture.path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	clock := fixture.clock
	db.now = clock.now
	store, err := NewTelemetry(testContext(t), db, TelemetryOptions{Now: clock.now, FlushInterval: time.Millisecond, Ring: RingOptions{Retention: time.Second}})
	if err != nil {
		t.Fatalf("new telemetry: %v", err)
	}
	defer store.Close(testContext(t))
	after, err := store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: from.UnixMilli(), ToMs: from.Add(10 * time.Second).UnixMilli(),
	}, HistoryQueryBudget{Tiers: []Tier{Tier10s}})
	if err != nil || len(after.Series) != 1 {
		t.Fatalf("after = %+v, %v", after, err)
	}
	if !floatPtrEqual(after.Series[0].Points[0].Avg, 42) {
		t.Fatalf("restored point = %+v", after.Series[0].Points[0])
	}
	ringOnly, err := store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: from.UnixMilli(), ToMs: from.Add(10 * time.Second).UnixMilli(),
	}, HistoryQueryBudget{Tiers: []Tier{Tier1s}})
	if err != nil {
		t.Fatalf("ring query: %v", err)
	}
	if ringOnly.Tier != Tier1s || len(ringOnly.Series) != 0 {
		t.Fatalf("ring should be empty after restart: %+v", ringOnly)
	}
}

func deref(pointer *float64) any {
	if pointer == nil {
		return nil
	}
	return *pointer
}

func formatPoints(points []HistoryPoint) string {
	var builder strings.Builder
	for _, point := range points {
		builder.WriteString("{")
		builder.WriteString(itoa64(point.T))
		builder.WriteString(" ")
		builder.WriteString(fmt.Sprint(deref(point.Min)))
		builder.WriteString("/")
		builder.WriteString(fmt.Sprint(deref(point.Max)))
		builder.WriteString("/")
		builder.WriteString(fmt.Sprint(deref(point.Avg)))
		builder.WriteString("}")
	}
	return builder.String()
}

func itoa64(value int64) string { return fmt.Sprintf("%d", value) }

func floatPtrEqual(pointer *float64, want float64) bool {
	return pointer != nil && *pointer == want
}

func pointsEqual(left, right []HistoryPoint) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].T != right[index].T || !pointerEqual(left[index].Min, right[index].Min) ||
			!pointerEqual(left[index].Max, right[index].Max) || !pointerEqual(left[index].Avg, right[index].Avg) {
			return false
		}
	}
	return true
}

func pointerEqual(left, right *float64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// A finer tier that is too large to read must not use up the budget the
// coarser tier needs: with one hour of 10-second rows over the budget, the
// minute tier still answers with every bucket instead of an empty hour tier.
func TestRejectedFinerTierLeavesBudgetForTheCoarserTier(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	from := testBase
	records := make([]protocol.TelemetryRecord, 0, 360)
	for index := 0; index < 360; index++ {
		records = append(records, makeMetricsRecord(recordOptions{
			sequence:   int64(index + 1),
			capturedAt: from.Add(time.Duration(index) * 10 * time.Second).UnixMilli(),
			payload:    payloadWithUsage(10),
		}))
	}
	fixture.ingest(t, liveBatch(records...))
	fixture.setClock(from.Add(time.Hour))
	result, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: from.UnixMilli(), ToMs: from.Add(time.Hour).UnixMilli(), MaxPoints: 600,
	}, HistoryQueryBudget{MaxRowsScanned: 300})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Tier != Tier1m || result.StepMs != 60_000 {
		t.Fatalf("tier/step = %q/%d, want 1m/60000", result.Tier, result.StepMs)
	}
	if len(result.Series) != 1 || len(result.Series[0].Points) != 60 {
		t.Fatalf("series = %+v, want one series with 60 minute buckets", result.Series)
	}
}

// The gap derivation reports a partial answer only when the window holds more
// receipts than it may scan, not whenever the remaining budget is below the
// receipt cap.
func TestGapScanIsCompleteWhenEveryReceiptFits(t *testing.T) {
	fixture := diskHistoryFixture(t, TelemetryOptions{})
	from := testBase
	records := make([]protocol.TelemetryRecord, 0, 60)
	for index := 0; index < 60; index++ {
		records = append(records, makeMetricsRecord(recordOptions{
			sequence:   int64(index + 1),
			capturedAt: from.Add(time.Duration(index) * 10 * time.Second).UnixMilli(),
			payload:    payloadWithUsage(10),
		}))
	}
	fixture.ingest(t, liveBatch(records...))
	fixture.setClock(from.Add(10 * time.Minute))
	result, err := fixture.store.Query(testContext(t), HistoryQuery{
		MachineID: "machine-1", Metrics: []string{"cpu.usage"},
		FromMs: from.UnixMilli(), ToMs: from.Add(10 * time.Minute).UnixMilli(), MaxPoints: 600,
	}, HistoryQueryBudget{MaxRowsScanned: 1000})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Truncated || result.BudgetExceeded {
		t.Fatalf("complete answer marked partial: truncated=%v exceeded=%v", result.Truncated, result.BudgetExceeded)
	}
	if len(result.Series) != 1 || len(result.Series[0].Points) == 0 {
		t.Fatalf("series = %+v", result.Series)
	}
}
