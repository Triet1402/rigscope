package storage

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

const (
	maxHistoryRange      = 400 * 24 * time.Hour
	maxResourceIDs       = 64
	defaultMaxPoints     = 600
	maxHistoryPoints     = 2000
	maxHistoryGaps       = 500
	defaultMaxSeries     = 64
	defaultMaxRows       = 250_000
	defaultMaxElapsed    = 5 * time.Second
	maxReceiptScanRows   = 20_000
	history10sStepMS     = int64(10_000)
	history1mStepMS      = int64(60_000)
	history1hStepMS      = int64(3_600_000)
	collectorRestartText = "collector restarted"
)

// GapReasonCollectorRestarted marks the unaccounted interval between the last
// record of one collector run and the first record of the next. The collector
// could not observe that interval, so the gap carries no dropped count.
const GapReasonCollectorRestarted = collectorRestartText

var coreIDPattern = regexp.MustCompile(`^core:(0|[1-9][0-9]{0,3})$`)

// Tier is one history storage resolution.
type Tier string

// History tiers. "1s" comes from the bounded RAM ring; the rest are persistent
// summary tiers.
const (
	Tier1s  Tier = "1s"
	Tier10s Tier = "10s"
	Tier1m  Tier = "1m"
	Tier1h  Tier = "1h"
)

// HistoryQuery is a bounded chart/export request.
type HistoryQuery struct {
	MachineID   string
	Metrics     []string
	ResourceIDs []string
	// CoreIDs restricts only cpu.core; other families keep their resources.
	CoreIDs []string
	FromMs  int64
	ToMs    int64
	// MaxPoints is the target bucket count per series. Default 600, max 2000.
	MaxPoints int
}

// HistoryQueryBudget bounds the work a single query may do.
type HistoryQueryBudget struct {
	MaxSeries      int
	MaxRowsScanned int
	MaxElapsed     time.Duration
	// Now is the clock used for tier retention selection. Defaults to the
	// store clock.
	Now time.Time
	// Tiers restricts which backends may answer. Empty means every tier.
	Tiers []Tier
}

// HistoryPoint is one aggregated bucket. A nil Min/Max/Avg means the bucket
// contained only explicit unavailable readings.
type HistoryPoint struct {
	T   int64
	Min *float64
	Max *float64
	Avg *float64
}

// HistorySeries is one metric/resource line.
type HistorySeries struct {
	Metric     string
	ResourceID string
	Unit       MetricUnit
	Points     []HistoryPoint
}

// GapKind distinguishes a dropped interval reported by the collector from an
// interval the hub can prove was missed by comparing receipts.
type GapKind string

// Gap kinds.
const (
	GapMissing GapKind = "missing"
	GapDropped GapKind = "dropped"
)

// HistoryGap is one missing or dropped interval.
type HistoryGap struct {
	FromMs         int64
	ToMs           int64
	Kind           GapKind
	Reason         string
	DroppedRecords int64
}

// HistoryResult is a complete bounded answer.
type HistoryResult struct {
	MachineID string
	FromMs    int64
	ToMs      int64
	StepMs    int64
	Tier      Tier
	Series    []HistorySeries
	Gaps      []HistoryGap
	// Truncated means the answer omitted rows or series to stay inside a
	// bound. BudgetExceeded additionally means a scan or elapsed-time budget
	// was spent, so the caller should ask for a narrower range or lower
	// resolution rather than treat the answer as complete.
	Truncated      bool
	BudgetExceeded bool
	Elapsed        time.Duration
}

// Query answers a bounded history request by choosing the finest tier whose
// actual indexed rows fit the scan budget. The 1-second RAM ring is the finest
// tier when the entire range is inside its retained window; otherwise the
// persistent summary tiers answer the query. Ring and summary buckets are
// never spliced into one result, because the two grids differ and mixing them
// would double count the overlap.
func (t *Telemetry) Query(ctx context.Context, query HistoryQuery, budget HistoryQueryBudget) (HistoryResult, error) {
	if budget.Now.IsZero() {
		budget.Now = t.now()
	}
	return queryHistory(ctx, t.db, t.ring, t.retention.policy, query, budget)
}

func queryHistory(ctx context.Context, db *Database, ring *Ring, retention RetentionPolicy, query HistoryQuery, budget HistoryQueryBudget) (HistoryResult, error) {
	started := time.Now()
	maxPoints, err := validateMaxPoints(query.MaxPoints)
	if err != nil {
		return HistoryResult{}, err
	}
	maxSeries, err := positiveBudget(budget.MaxSeries, defaultMaxSeries, "maxSeries")
	if err != nil {
		return HistoryResult{}, err
	}
	maxRows, err := positiveBudget(budget.MaxRowsScanned, defaultMaxRows, "maxRowsScanned")
	if err != nil {
		return HistoryResult{}, err
	}
	maxElapsed := budget.MaxElapsed
	if maxElapsed == 0 {
		maxElapsed = defaultMaxElapsed
	}
	if maxElapsed < 0 {
		return HistoryResult{}, invalidError("maxElapsed must be positive")
	}
	if err := validateHistoryQuery(query); err != nil {
		return HistoryResult{}, err
	}
	nowMs := budget.Now.UnixMilli()

	// One deadline covers catalog discovery, tier probes, data reads and gap
	// derivation. SQLite cancels the running statement when it expires, so a
	// pathological query cannot hold a reader connection indefinitely.
	queryCtx, cancel := context.WithTimeout(ctx, maxElapsed)
	defer cancel()
	scan := &historyScanBudget{remaining: maxRows}

	seriesRows, err := loadHistorySeries(queryCtx, db, query, scan, maxSeries+1)
	if err != nil {
		if ctx.Err() != nil {
			return HistoryResult{}, ctx.Err()
		}
		if queryCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			scan.exceeded = true
		} else {
			return HistoryResult{}, err
		}
	}
	truncated := scan.exceeded
	if len(seriesRows) > maxSeries {
		seriesRows = seriesRows[:maxSeries]
		truncated = true
	}
	seriesIDs := make([]int64, len(seriesRows))
	seriesKeys := make([]KnownSeries, len(seriesRows))
	for index, row := range seriesRows {
		seriesIDs[index] = row.SeriesID
		seriesKeys[index] = KnownSeries{Metric: row.Metric, ResourceID: row.ResourceID}
	}

	chosen, err := chooseHistoryTier(queryCtx, db, ring, retention, seriesIDs, seriesKeys, query, nowMs, budget, scan, started)
	if err != nil {
		if ctx.Err() != nil {
			return HistoryResult{}, ctx.Err()
		}
		if queryCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			scan.exceeded = true
		} else {
			return HistoryResult{}, err
		}
	}
	if chosen.TimeExceeded || chosen.OverBudget {
		truncated = true
	}
	if chosen.CutSeries < len(seriesRows) {
		seriesRows = seriesRows[:chosen.CutSeries]
		truncated = true
	}
	if chosen.ResolutionMs <= 0 {
		chosen = tierChoice{Tier: Tier10s, ResolutionMs: history10sStepMS, CutSeries: 0}
	}

	rangeMs := query.ToMs - query.FromMs
	step := (rangeMs + int64(maxPoints) - 1) / int64(maxPoints)
	resolution := chosen.ResolutionMs
	step = ((step + resolution - 1) / resolution) * resolution
	if step < resolution {
		step = resolution
	}
	origin := query.FromMs / step * step

	series := make([]HistorySeries, 0, len(seriesRows))
	for _, row := range seriesRows {
		if ctx.Err() != nil {
			return HistoryResult{}, ctx.Err()
		}
		if queryCtx.Err() != nil || time.Since(started) > maxElapsed {
			scan.exceeded = true
			break
		}
		unit, ok := MetricUnitFor(row.Metric)
		if !ok {
			continue // A future metric this build does not know.
		}
		var points []HistoryPoint
		if chosen.Tier == Tier1s {
			points = readRingSeries(ring, query.MachineID, row.Metric, row.ResourceID, query.FromMs, query.ToMs, origin, step)
			if !scan.spend(len(points)) {
				scan.exceeded = true
				break
			}
		} else {
			points, err = readDiskSeries(queryCtx, db, chosen.Tier, row.SeriesID, fromBucketMs(chosen.Tier, query.FromMs), query.ToMs, origin, step, scan)
			if err != nil {
				if ctx.Err() != nil {
					return HistoryResult{}, ctx.Err()
				}
				if queryCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
					scan.exceeded = true
					break
				}
				return HistoryResult{}, err
			}
		}
		if len(points) == 0 {
			continue
		}
		series = append(series, HistorySeries{
			Metric:     row.Metric,
			ResourceID: row.ResourceID,
			Unit:       unit,
			Points:     points,
		})
	}

	gaps, err := readHistoryGaps(queryCtx, db, query.MachineID, query.FromMs, query.ToMs, scan)
	if err != nil {
		if ctx.Err() != nil {
			return HistoryResult{}, ctx.Err()
		}
		if queryCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			scan.exceeded = true
			gaps = nil
		} else {
			return HistoryResult{}, err
		}
	}
	truncated = truncated || scan.exceeded

	return HistoryResult{
		MachineID:      query.MachineID,
		FromMs:         query.FromMs,
		ToMs:           query.ToMs,
		StepMs:         step,
		Tier:           chosen.Tier,
		Series:         series,
		Gaps:           gaps,
		Truncated:      truncated,
		BudgetExceeded: scan.exceeded,
		Elapsed:        time.Since(started),
	}, nil
}

// historyScanBudget is the shared row and work allowance for one query.
type historyScanBudget struct {
	remaining int
	exceeded  bool
}

func (b *historyScanBudget) available() int {
	if b.remaining < 0 {
		return 0
	}
	return b.remaining
}

func (b *historyScanBudget) spend(rows int) bool {
	if rows <= 0 {
		return true
	}
	if rows > b.remaining {
		b.remaining = 0
		b.exceeded = true
		return false
	}
	b.remaining -= rows
	return true
}

type historySeriesRow struct {
	SeriesID   int64
	Metric     string
	ResourceID string
}

func loadHistorySeries(ctx context.Context, db *Database, query HistoryQuery, scan *historyScanBudget, want int) ([]historySeriesRow, error) {
	limit := want
	if limit > scan.available() {
		limit = scan.available()
	}
	if limit <= 0 {
		scan.exceeded = true
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(query.Metrics)), ", ")
	args := make([]any, 0, 1+len(query.Metrics)+len(query.ResourceIDs)+len(query.CoreIDs))
	args = append(args, query.MachineID)
	for _, metric := range query.Metrics {
		args = append(args, metric)
	}
	sql := "SELECT series_id, metric, resource_id FROM series WHERE machine_id = ? AND metric IN (" + placeholders + ")"
	if len(query.ResourceIDs) > 0 {
		sql += " AND resource_id IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(query.ResourceIDs)), ", ") + ")"
		for _, id := range query.ResourceIDs {
			args = append(args, id)
		}
	}
	if query.CoreIDs != nil {
		if len(query.CoreIDs) == 0 {
			sql += " AND metric <> 'cpu.core'"
		} else {
			sql += " AND (metric <> 'cpu.core' OR resource_id IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(query.CoreIDs)), ", ") + "))"
			for _, id := range query.CoreIDs {
				args = append(args, id)
			}
		}
	}
	sql += " ORDER BY metric, resource_id LIMIT ?"
	args = append(args, limit)
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []historySeriesRow
	for rows.Next() {
		var row historySeriesRow
		if err := rows.Scan(&row.SeriesID, &row.Metric, &row.ResourceID); err != nil {
			return nil, mapError(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	if len(result) >= limit && limit < want {
		// The catalog scan hit the remaining budget, so there may be more
		// matching series.
		scan.exceeded = true
	}
	if !scan.spend(len(result)) {
		scan.exceeded = true
	}
	return result, nil
}

type tierCandidate struct {
	Tier         Tier
	ResolutionMs int64
	RetentionMs  int64
}

type tierChoice struct {
	Tier         Tier
	ResolutionMs int64
	CutSeries    int
	TimeExceeded bool
	OverBudget   bool
}

func chooseHistoryTier(
	ctx context.Context,
	db *Database,
	ring *Ring,
	retention RetentionPolicy,
	seriesIDs []int64,
	seriesKeys []KnownSeries,
	query HistoryQuery,
	nowMs int64,
	budget HistoryQueryBudget,
	scan *historyScanBudget,
	started time.Time,
) (tierChoice, error) {
	allowed := map[Tier]bool{}
	if len(budget.Tiers) == 0 {
		for _, tier := range []Tier{Tier1s, Tier10s, Tier1m, Tier1h} {
			allowed[tier] = true
		}
	} else {
		for _, tier := range budget.Tiers {
			switch tier {
			case Tier1s, Tier10s, Tier1m, Tier1h:
				allowed[tier] = true
			default:
				return tierChoice{}, invalidError("unknown history tier %q", tier)
			}
		}
	}
	candidates := make([]tierCandidate, 0, 4)
	// The ring may answer an automatic tier choice only when it covers the
	// whole requested range for every requested series. A process that started
	// ten minutes ago, or an evicted machine, holds only part of the range;
	// the durable tiers still have the rest. A caller that explicitly asks for
	// only the ring gets the partial window it asked for.
	if allowed[Tier1s] && ring != nil && ring.Enabled() {
		ringCovered := ring.covers(query.MachineID, seriesKeys, query.FromMs)
		onlyRing := len(allowed) == 1
		if ringCovered || onlyRing {
			candidates = append(candidates, tierCandidate{Tier1s, ringStepMS, ring.Retention().Milliseconds()})
		}
	}
	for _, candidate := range []tierCandidate{
		{Tier10s, history10sStepMS, retention.TenSecondMs},
		{Tier1m, history1mStepMS, retention.OneMinuteMs},
		{Tier1h, history1hStepMS, retention.OneHourMs},
	} {
		if allowed[candidate.Tier] {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return tierChoice{}, invalidError("no history storage tiers are available")
	}
	available := make([]tierCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.RetentionMs > 0 && query.FromMs >= nowMs-candidate.RetentionMs {
			available = append(available, candidate)
		}
	}
	pool := available
	if len(pool) == 0 {
		// The coarsest allowed tier is always a usable fallback even if a custom
		// retention policy is shorter than the requested range.
		pool = candidates[len(candidates)-1:]
	}

	timeExceeded := false
	for index, candidate := range pool {
		if ctx.Err() != nil {
			timeExceeded = true
			scan.exceeded = true
			break
		}
		// Each probe gets its own allowance equal to what the read could
		// spend. Rows counted while rejecting a finer tier are not charged to
		// the read budget; otherwise a rejected 10s probe would leave nothing
		// for the coarser tier that does fit, and the answer would be empty.
		available := scan.available()
		probe := &historyScanBudget{remaining: available}
		rows, err := countHistoryRows(ctx, db, ring, candidate.Tier, seriesIDs, seriesKeys, query, probe)
		if err != nil {
			return tierChoice{}, err
		}
		fits := rows <= available && !probe.exceeded
		if fits {
			// An empty finer tier (for example after a restart, before the ring
			// refills) falls through to the next tier instead of hiding older
			// data that is already available.
			if rows == 0 && index < len(pool)-1 {
				continue
			}
			return tierChoice{Tier: candidate.Tier, ResolutionMs: candidate.ResolutionMs, CutSeries: len(seriesIDs)}, nil
		}
	}
	if ctx.Err() != nil {
		timeExceeded = true
		scan.exceeded = true
	}

	fallback := pool[len(pool)-1]
	cut := len(seriesIDs)
	fits := len(seriesIDs) == 0
	if timeExceeded {
		cut = maxInt(1, cut/2)
	}
	for !fits && cut >= 1 {
		if ctx.Err() != nil {
			timeExceeded = true
			scan.exceeded = true
			break
		}
		available := scan.available()
		if available <= 0 {
			scan.exceeded = true
			break
		}
		probe := &historyScanBudget{remaining: available}
		rows, err := countHistoryRows(ctx, db, ring, fallback.Tier, seriesIDs[:cut], seriesKeys[:cut], query, probe)
		if err != nil {
			return tierChoice{}, err
		}
		if rows <= available && !probe.exceeded {
			fits = true
			break
		}
		if cut == 1 {
			break
		}
		cut = maxInt(1, cut/2)
	}
	if !fits {
		scan.exceeded = true
	}
	return tierChoice{
		Tier:         fallback.Tier,
		ResolutionMs: fallback.ResolutionMs,
		CutSeries:    maxInt(1, minInt(cut, len(seriesIDs))),
		TimeExceeded: timeExceeded,
		OverBudget:   !fits,
	}, nil
}

// countHistoryRows probes the actual indexed rows for one tier and charges
// them to the shared query budget. It stops as soon as the budget cannot cover
// the probe, so a pathological tier cannot scan the whole database.
func countHistoryRows(ctx context.Context, db *Database, ring *Ring, tier Tier, seriesIDs []int64, seriesKeys []KnownSeries, query HistoryQuery, scan *historyScanBudget) (int, error) {
	if len(seriesIDs) == 0 {
		return 0, nil
	}
	if tier == Tier1s {
		if ring == nil {
			return 0, nil
		}
		available := scan.available()
		rows := ring.countRows(query.MachineID, seriesKeys, query.FromMs, query.ToMs, available)
		if rows > available {
			scan.spend(available)
			scan.exceeded = true
		} else {
			scan.spend(rows)
		}
		return rows, nil
	}
	table, column := historyTable(tier)
	if table == "" {
		return 0, invalidError("unknown history tier %q", tier)
	}
	statement := "SELECT COUNT(*) AS n FROM (SELECT 1 FROM " + table +
		" WHERE series_id = ? AND " + column + " >= ? AND " + column + " <= ? LIMIT ?)"
	total := 0
	for _, seriesID := range seriesIDs {
		remaining := scan.available()
		if remaining <= 0 {
			scan.exceeded = true
			return total, nil
		}
		var count int64
		if err := db.QueryRow(ctx, statement, seriesID, fromBucketMs(tier, query.FromMs), query.ToMs, remaining+1).Scan(&count); err != nil {
			return 0, mapError(err)
		}
		if count > int64(remaining) {
			scan.spend(remaining)
			scan.exceeded = true
			return total + remaining + 1, nil
		}
		total += int(count)
		scan.spend(int(count))
	}
	return total, nil
}

type historyAggregateRow struct {
	Bucket   int64
	Min      *float64
	Max      *float64
	Sum      *float64
	Count    int64
	RowCount int64
}

func readDiskSeries(ctx context.Context, db *Database, tier Tier, seriesID, fromBucket, toMs, originMs, stepMs int64, scan *historyScanBudget) ([]HistoryPoint, error) {
	table, _ := historyTable(tier)
	if table == "" || tier == Tier1s {
		return nil, invalidError("unknown disk history tier %q", tier)
	}
	rowLimit := scan.available()
	if rowLimit <= 0 {
		scan.exceeded = true
		return nil, nil
	}
	statement := "SELECT ? + CAST((bucket_ms - ?) / ? AS INTEGER) * ? AS bucket, " +
		"MIN(min) AS min_value, MAX(max) AS max_value, " +
		"SUM(sum) AS sum_value, SUM(count) AS count_value, COUNT(*) AS row_count " +
		"FROM (SELECT bucket_ms, min, max, sum, count FROM " + table +
		" WHERE series_id = ? AND bucket_ms >= ? AND bucket_ms <= ? ORDER BY bucket_ms LIMIT ?) " +
		"GROUP BY bucket ORDER BY bucket"
	rows, err := db.Query(ctx, statement, originMs, originMs, stepMs, stepMs, seriesID, fromBucket, toMs, rowLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := make([]HistoryPoint, 0, 64)
	for rows.Next() {
		var row historyAggregateRow
		if err := rows.Scan(&row.Bucket, &row.Min, &row.Max, &row.Sum, &row.Count, &row.RowCount); err != nil {
			return nil, mapError(err)
		}
		point := HistoryPoint{T: row.Bucket, Min: row.Min, Max: row.Max}
		if row.Count > 0 && row.Sum != nil {
			average := *row.Sum / float64(row.Count)
			point.Avg = &average
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	if len(points) >= rowLimit {
		// The data read stopped at the remaining budget; there may be more
		// buckets in the requested range.
		scan.exceeded = true
	}
	scan.spend(minInt(len(points), rowLimit))
	return points, nil
}

func readRingSeries(ring *Ring, machineID, metric, resourceID string, fromMs, toMs, originMs, stepMs int64) []HistoryPoint {
	buckets := ring.read(machineID, metric, resourceID, fromMs, toMs)
	if len(buckets) == 0 {
		return nil
	}
	type aggregate struct {
		t     int64
		min   float64
		max   float64
		sum   float64
		count uint32
		nulls uint32
	}
	byBucket := make(map[int64]*aggregate, len(buckets))
	order := make([]int64, 0, len(buckets))
	for _, bucket := range buckets {
		at := (bucket.stamp - 1) * ringStepMS
		target := originMs + (at-originMs)/stepMs*stepMs
		entry := byBucket[target]
		if entry == nil {
			entry = &aggregate{t: target}
			byBucket[target] = entry
			order = append(order, target)
		}
		if bucket.count > 0 {
			if entry.count == 0 {
				entry.min = bucket.min
				entry.max = bucket.max
			} else {
				if bucket.min < entry.min {
					entry.min = bucket.min
				}
				if bucket.max > entry.max {
					entry.max = bucket.max
				}
			}
			entry.sum += bucket.sum
			entry.count += bucket.count
		}
		entry.nulls += bucket.nulls
	}
	sortInt64s(order)
	points := make([]HistoryPoint, 0, len(order))
	for _, at := range order {
		entry := byBucket[at]
		point := HistoryPoint{T: entry.t}
		if entry.count > 0 {
			minimum, maximum := entry.min, entry.max
			average := entry.sum / float64(entry.count)
			point.Min = &minimum
			point.Max = &maximum
			point.Avg = &average
		}
		points = append(points, point)
	}
	return points
}

func readHistoryGaps(ctx context.Context, db *Database, machineID string, fromMs, toMs int64, scan *historyScanBudget) ([]HistoryGap, error) {
	gaps := make([]HistoryGap, 0, 8)
	if scan.available() <= 0 {
		scan.exceeded = true
		return gaps, nil
	}
	rows, err := db.Query(ctx,
		"SELECT from_ms, to_ms, dropped, reason FROM gaps "+
			"WHERE machine_id = ? AND to_ms >= ? AND from_ms <= ? ORDER BY from_ms LIMIT ?",
		machineID, fromMs, toMs, maxHistoryGaps,
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var (
			gap     HistoryGap
			dropped int64
		)
		if err := rows.Scan(&gap.FromMs, &gap.ToMs, &dropped, &gap.Reason); err != nil {
			rows.Close()
			return nil, mapError(err)
		}
		gap.Kind = GapDropped
		gap.DroppedRecords = dropped
		gaps = append(gaps, gap)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, mapError(err)
	}
	rows.Close()
	if len(gaps) >= maxHistoryGaps {
		scan.exceeded = true
	}
	if !scan.spend(len(gaps)) {
		scan.exceeded = true
		return gaps, nil
	}

	// Missing gaps come from receipts: one receipt before the window proves an
	// interval that started earlier, and the threshold uses the row's own
	// advertised cadence. No leading or trailing gap is invented without real
	// stored receipts. A run change is an unknown interval with no dropped
	// count even when it is shorter than the cadence threshold: the collector
	// could not observe it.
	receiptLimit := scan.available()
	if receiptLimit <= 0 {
		scan.exceeded = true
		return gaps, nil
	}
	if receiptLimit > maxReceiptScanRows {
		receiptLimit = maxReceiptScanRows
	}
	// Count the window's receipts up to one past the limit. The derivation is
	// partial only when the window really holds more receipts than it may
	// scan, not merely because the remaining budget is below the cap.
	var receiptCount int64
	if err := db.QueryRow(ctx,
		"SELECT COUNT(*) FROM (SELECT 1 FROM sample_receipts "+
			"WHERE machine_id = ? AND captured_at_ms >= ? AND captured_at_ms <= ? LIMIT ?)",
		machineID, fromMs, toMs, receiptLimit+1,
	).Scan(&receiptCount); err != nil {
		return nil, mapError(err)
	}
	if receiptCount > int64(receiptLimit) {
		scan.exceeded = true
	}
	missingRows, err := db.Query(ctx,
		"WITH before_window AS ( "+
			"SELECT captured_at_ms, expected_interval_ms, run_id FROM sample_receipts "+
			"WHERE machine_id = ? AND captured_at_ms < ? ORDER BY captured_at_ms DESC LIMIT 1 "+
			"), in_window AS ( "+
			"SELECT captured_at_ms, expected_interval_ms, run_id FROM sample_receipts "+
			"WHERE machine_id = ? AND captured_at_ms >= ? AND captured_at_ms <= ? "+
			"ORDER BY captured_at_ms LIMIT ? "+
			"), ordered AS ( "+
			"SELECT captured_at_ms, expected_interval_ms, run_id, "+
			"LAG(captured_at_ms) OVER (ORDER BY captured_at_ms) AS prev_ms, "+
			"LAG(run_id) OVER (ORDER BY captured_at_ms) AS prev_run "+
			"FROM (SELECT * FROM before_window UNION ALL SELECT * FROM in_window) "+
			") "+
			"SELECT MAX(prev_ms, ?) AS from_ms, captured_at_ms AS to_ms, "+
			"CASE WHEN run_id <> prev_run THEN 1 ELSE 0 END AS restarted "+
			"FROM ordered "+
			"WHERE prev_ms IS NOT NULL AND captured_at_ms > ? "+
			"AND (run_id <> prev_run OR captured_at_ms - prev_ms > MAX(15000, 3 * COALESCE(expected_interval_ms, 5000))) "+
			"ORDER BY captured_at_ms LIMIT ?",
		machineID, fromMs, machineID, fromMs, toMs, receiptLimit, fromMs, fromMs, maxHistoryGaps,
	)
	if err != nil {
		return nil, err
	}
	defer missingRows.Close()
	for missingRows.Next() {
		var (
			gap       HistoryGap
			restarted int64
		)
		if err := missingRows.Scan(&gap.FromMs, &gap.ToMs, &restarted); err != nil {
			return nil, mapError(err)
		}
		gap.Kind = GapMissing
		if restarted != 0 {
			gap.Reason = GapReasonCollectorRestarted
		} else {
			gap.Reason = "expected interval missed"
		}
		gaps = append(gaps, gap)
	}
	if err := missingRows.Err(); err != nil {
		return nil, mapError(err)
	}
	scan.spend(minInt(int(receiptCount), receiptLimit))
	sortHistoryGaps(gaps)
	if len(gaps) > maxHistoryGaps {
		gaps = gaps[:maxHistoryGaps]
		scan.exceeded = true
	}
	return gaps, nil
}

func historyTable(tier Tier) (table, column string) {
	switch tier {
	case Tier10s:
		return "points_10s", "bucket_ms"
	case Tier1m:
		return "points_1m", "bucket_ms"
	case Tier1h:
		return "points_1h", "bucket_ms"
	default:
		return "", ""
	}
}

func fromBucketMs(tier Tier, fromMs int64) int64 {
	switch tier {
	case Tier10s:
		return fromMs / history10sStepMS * history10sStepMS
	case Tier1m:
		return fromMs / history1mStepMS * history1mStepMS
	case Tier1h:
		return fromMs / history1hStepMS * history1hStepMS
	default:
		return fromMs
	}
}

func validateHistoryQuery(query HistoryQuery) error {
	if query.MachineID == "" || len(query.MachineID) > 256 {
		return invalidError("machineId must be a nonempty string of at most 256 characters")
	}
	if len(query.Metrics) == 0 || len(query.Metrics) > len(metricCatalog) {
		return invalidError("metrics must contain 1..%d metric names", len(metricCatalog))
	}
	seenMetrics := make(map[string]struct{}, len(query.Metrics))
	for _, metric := range query.Metrics {
		if _, ok := MetricUnitFor(metric); !ok {
			return invalidError("unknown metric %q", metric)
		}
		if _, duplicate := seenMetrics[metric]; duplicate {
			return invalidError("duplicate metric %q", metric)
		}
		seenMetrics[metric] = struct{}{}
	}
	if query.ResourceIDs != nil {
		if len(query.ResourceIDs) > maxResourceIDs {
			return invalidError("resourceIds must contain at most %d ids", maxResourceIDs)
		}
		seenResources := make(map[string]struct{}, len(query.ResourceIDs))
		for _, id := range query.ResourceIDs {
			if id == "" || len(id) > 256 {
				return invalidError("resourceIds must be nonempty strings of at most 256 bytes")
			}
			if _, duplicate := seenResources[id]; duplicate {
				return invalidError("duplicate resource id %q", id)
			}
			seenResources[id] = struct{}{}
		}
	}
	if query.CoreIDs != nil {
		if len(query.CoreIDs) > protocol.MaxCores {
			return invalidError("coreIds must contain at most %d unique core identifiers", protocol.MaxCores)
		}
		seenCores := make(map[string]struct{}, len(query.CoreIDs))
		for _, id := range query.CoreIDs {
			if !coreIDPattern.MatchString(id) {
				return invalidError("coreIds must use the form core:<index>")
			}
			index := 0
			for _, digit := range id[len("core:"):] {
				index = index*10 + int(digit-'0')
			}
			if index >= protocol.MaxCores {
				return invalidError("coreIds contains an out-of-range core index")
			}
			if _, duplicate := seenCores[id]; duplicate {
				return invalidError("coreIds must contain unique core identifiers")
			}
			seenCores[id] = struct{}{}
		}
	}
	if query.FromMs >= query.ToMs {
		return invalidError("fromMs must be before toMs")
	}
	if query.ToMs-query.FromMs > maxHistoryRange.Milliseconds() {
		return invalidError("range exceeds 400 days")
	}
	return nil
}

func validateMaxPoints(value int) (int, error) {
	if value == 0 {
		return defaultMaxPoints, nil
	}
	if value < 1 {
		return 0, invalidError("maxPoints must be a positive integer")
	}
	if value > maxHistoryPoints {
		return maxHistoryPoints, nil
	}
	return value, nil
}

func positiveBudget(value, fallback int, name string) (int, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < 1 {
		return 0, invalidError("%s must be a positive integer", name)
	}
	return value, nil
}

func sortInt64s(values []int64) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func sortHistoryGaps(gaps []HistoryGap) {
	for i := 1; i < len(gaps); i++ {
		for j := i; j > 0; j-- {
			left, right := gaps[j-1], gaps[j]
			if left.FromMs < right.FromMs || (left.FromMs == right.FromMs && left.ToMs <= right.ToMs) {
				break
			}
			gaps[j-1], gaps[j] = right, left
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
