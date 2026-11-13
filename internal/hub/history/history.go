// Package history serves the hub's bounded telemetry history queries.
//
// It owns GET /api/history/status, GET /api/history/metrics and GET /api/history
// as documented in docs/rewrite/api-v1.md: session and machine-scope checks,
// query parsing through internal/api, the per-session query token bucket, and
// mapping the storage package's bounded HistoryResult into the browser
// contract. Buckets are never zero-filled.
//
// It does not own retention, rollups, the RAM ring or authentication; those
// live in internal/hub/storage and the auth package.
package history

import (
	"context"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

const (
	// QueryBudgetTokens and QueryBudgetRefill throttle history scans per
	// session: a burst of 8 requests, one recovered every 250 ms.
	QueryBudgetTokens = 8.0
	QueryBudgetRefill = 250 * time.Millisecond
	// maxBudgetSessions bounds the per-session budget map.
	maxBudgetSessions = 256
	// scan budgets mirror the reference hub.
	maxRowsScanned = 25_000
	maxScanElapsed = 100 * time.Millisecond
	maxSeries      = 64
	maxSeriesCores = 1024 + 64
	defaultFlushMS = int64(1000)
)

// Options configures the history endpoints.
type Options struct {
	Auth      live.Authenticator
	State     *live.State
	Telemetry *storage.Telemetry
	// CheckOrigin, when non-nil, rejects an untrusted Origin on a GET.
	CheckOrigin func(*http.Request) bool
	Now         func() time.Time
	// FlushIntervalMS is the telemetry store's configured flush interval for
	// the status document.
	FlushIntervalMS int64
}

type budget struct {
	at     time.Time
	tokens float64
}

// Handler serves the three history routes.
type Handler struct {
	opts Options

	mu      sync.Mutex
	budgets map[string]*budget
}

// New creates the history handler.
func New(opts Options) *Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.FlushIntervalMS <= 0 {
		if opts.Telemetry != nil {
			opts.FlushIntervalMS = opts.Telemetry.FlushInterval().Milliseconds()
		} else {
			opts.FlushIntervalMS = defaultFlushMS
		}
	}
	return &Handler{opts: opts, budgets: make(map[string]*budget)}
}

// ServeHTTP routes the three history endpoints.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.opts.CheckOrigin != nil && r.Header.Get("Origin") != "" && !h.opts.CheckOrigin(r) {
		writeError(w, http.StatusForbidden, "origin not allowed")
		return
	}
	session, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	switch r.URL.Path {
	case "/api/history/status":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET required")
			return
		}
		h.serveStatus(w)
	case "/api/history/metrics":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET required")
			return
		}
		machine, ok := h.machine(w, r, session)
		if !ok {
			return
		}
		_ = machine
		known := api.KnownMetrics()
		catalog := make(api.MetricCatalog, len(known))
		for name, unit := range known {
			catalog[name] = api.MetricDefinition{Unit: unit}
		}
		body, err := api.EncodeMetricCatalogResponse(api.MetricCatalogResponse{Metrics: catalog})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "response unavailable")
			return
		}
		writeBody(w, http.StatusOK, body)
	case "/api/history":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET required")
			return
		}
		if _, ok := h.machine(w, r, session); !ok {
			return
		}
		h.serveHistory(w, r, session)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (h *Handler) serveStatus(w http.ResponseWriter) {
	if h.opts.Telemetry == nil {
		writeError(w, http.StatusServiceUnavailable, "monitoring request temporarily unavailable")
		return
	}
	stats := h.opts.Telemetry.Stats()
	ring := h.opts.Telemetry.RingStats()
	policy := h.opts.Telemetry.RetentionPolicy()
	now := h.opts.Now()
	nowMS := now.UnixMilli()

	retention := policy.TenSecondMs
	if policy.OneMinuteMs > retention {
		retention = policy.OneMinuteMs
	}
	if policy.OneHourMs > retention {
		retention = policy.OneHourMs
	}
	availableFrom := nowMS - retention
	availableTo := nowMS

	tiers := make([]api.HistoryTierStatus, 0, 4)
	if ring.Retention > 0 {
		tiers = append(tiers, api.HistoryTierStatus{
			Tier:        api.HistoryTierOneSecond,
			StepMs:      ring.StepMS,
			RetentionMs: ring.Retention.Milliseconds(),
			Bytes:       ring.UsedBytes,
		})
	}
	tiers = append(tiers,
		api.HistoryTierStatus{Tier: api.HistoryTierTenSecond, StepMs: 10_000, RetentionMs: policy.TenSecondMs, Bytes: stats.DBBytes},
		api.HistoryTierStatus{Tier: api.HistoryTierMinute, StepMs: 60_000, RetentionMs: policy.OneMinuteMs},
		api.HistoryTierStatus{Tier: api.HistoryTierHour, StepMs: 3_600_000, RetentionMs: policy.OneHourMs},
	)

	persistence := &api.PersistentHistoryStatus{
		Enabled:         true,
		StorageTier:     "summary",
		FlushIntervalMs: h.opts.FlushIntervalMS,
		QueuedRecords:   int64(stats.QueuedRecords),
		QueuedBatches:   int64(stats.QueuedBatches),
		WriteFailures:   stats.WriteFailures,
		DBBytes:         stats.DBBytes,
		DBWalBytes:      stats.DBWALBytes,
		Tiers:           tiers,
	}
	if !stats.LastFlush.IsZero() {
		value := stats.LastFlush.UnixMilli()
		persistence.LastFlushMs = &value
	}
	if stats.LastFlushDuration > 0 {
		value := stats.LastFlushDuration.Milliseconds()
		persistence.LastFlushDurationMs = &value
	}
	if stats.LastWriteError != "" {
		value := stats.LastWriteError
		persistence.LastWriteError = &value
	}
	if stats.DBLimitBytes > 0 {
		value := stats.DBLimitBytes
		persistence.DBBytesLimit = &value
	}
	status := api.HistoryStatus{
		Mode:              api.HistoryModePersistent,
		PersistentHistory: true,
		AvailableFromMs:   &availableFrom,
		AvailableToMs:     &availableTo,
		Persistence:       persistence,
		AcceptedRecords:   stats.CommittedRecords,
		GeneratedAt:       formatTimestamp(now),
	}
	if !h.opts.Telemetry.Persistent() {
		status.Mode = api.HistoryModeMemory
		status.PersistentHistory = false
		status.Persistence = nil
		status.Memory = &api.MemoryHistoryStatus{
			BudgetBytes:    stats.DBLimitBytes + ring.BudgetBytes,
			UsedBytes:      stats.DBBytes + ring.UsedBytes,
			Pressure:       stats.Degraded || stats.Retention.Pressure,
			SeriesCount:    int64(ring.Series),
			MachineCount:   int64(ring.Machines),
			EvictedSeries:  ring.EvictedSeries,
			RejectedSeries: ring.RejectedSeries,
			Tiers:          tiers,
		}
		oldest, newest := h.opts.Telemetry.HistoryBounds()
		status.AvailableFromMs, status.AvailableToMs = oldest, newest
		status.Memory.OldestMs, status.Memory.NewestMs = oldest, newest
	}
	body, err := api.EncodeHistoryStatus(status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "response unavailable")
		return
	}
	writeBody(w, http.StatusOK, body)
}

func (h *Handler) serveHistory(w http.ResponseWriter, r *http.Request, session live.Session) {
	if h.opts.Telemetry == nil {
		writeError(w, http.StatusServiceUnavailable, "monitoring request temporarily unavailable")
		return
	}
	if !h.takeToken(session) {
		writeError(w, http.StatusTooManyRequests, "Too many history requests. Pause another History tab, then refresh.")
		return
	}
	query, err := api.ParseHistoryQuery(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	seriesBudget := maxSeries
	for _, metric := range query.Metrics {
		if metric == "cpu.core" {
			seriesBudget = maxSeriesCores
			break
		}
	}
	var coreIDs []string
	if query.CoreIDs != nil {
		coreIDs = *query.CoreIDs
	}
	result, err := h.opts.Telemetry.Query(r.Context(), storage.HistoryQuery{
		MachineID:   query.MachineID,
		Metrics:     query.Metrics,
		ResourceIDs: query.ResourceIDs,
		CoreIDs:     coreIDs,
		FromMs:      query.FromMs,
		ToMs:        query.ToMs,
		MaxPoints:   query.MaxPoints,
	}, storage.HistoryQueryBudget{
		MaxSeries:      seriesBudget,
		MaxRowsScanned: maxRowsScanned,
		MaxElapsed:     maxScanElapsed,
	})
	if err != nil {
		if code := storage.CodeOf(err); code == storage.CodeInvalid {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusServiceUnavailable, "monitoring request temporarily unavailable")
		return
	}
	response := api.HistoryResult{
		MachineID: result.MachineID,
		FromMs:    result.FromMs,
		ToMs:      result.ToMs,
		StepMs:    result.StepMs,
		Tier:      api.HistoryTier(result.Tier),
		Series:    make([]api.HistorySeries, 0, len(result.Series)),
		Gaps:      make([]api.HistoryGap, 0, len(result.Gaps)),
		Truncated: result.Truncated,
		ElapsedMs: math.Round(result.Elapsed.Seconds()*1000*100) / 100,
	}
	for _, series := range result.Series {
		points := make([]api.HistoryPoint, 0, len(series.Points))
		for _, point := range series.Points {
			points = append(points, api.HistoryPoint{T: point.T, Min: point.Min, Max: point.Max, Avg: point.Avg})
		}
		response.Series = append(response.Series, api.HistorySeries{
			Metric:     series.Metric,
			ResourceID: series.ResourceID,
			Unit:       api.MetricUnit(series.Unit),
			Points:     points,
		})
	}
	for _, gap := range result.Gaps {
		converted := api.HistoryGap{
			FromMs: gap.FromMs,
			ToMs:   gap.ToMs,
			Kind:   api.GapKind(gap.Kind),
		}
		if gap.Reason != "" {
			reason := gap.Reason
			converted.Reason = &reason
		}
		if gap.Kind == storage.GapDropped {
			dropped := gap.DroppedRecords
			converted.DroppedRecords = &dropped
		}
		response.Gaps = append(response.Gaps, converted)
	}
	body, err := api.EncodeHistoryResult(response)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "response unavailable")
		return
	}
	writeBody(w, http.StatusOK, body)
}

// machine applies the shared machineId scope and existence checks.
func (h *Handler) machine(w http.ResponseWriter, r *http.Request, session live.Session) (live.MachineView, bool) {
	machineID := r.URL.Query().Get("machineId")
	if machineID == "" || len(machineID) > api.MaxHistoryMachineIDLength || !session.Scope.Includes(machineID) {
		writeError(w, http.StatusForbidden, "machine access denied")
		return live.MachineView{}, false
	}
	machine, ok := h.opts.State.Machine(machineID)
	if !ok {
		writeError(w, http.StatusNotFound, "machine not found")
		return live.MachineView{}, false
	}
	return machine, true
}

func (h *Handler) takeToken(session live.Session) bool {
	key := session.ID
	if key == "" {
		key = session.Username
	}
	now := h.opts.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, ok := h.budgets[key]
	if !ok {
		if len(h.budgets) >= maxBudgetSessions {
			for oldestKey := range h.budgets {
				delete(h.budgets, oldestKey)
				break
			}
		}
		entry = &budget{at: now, tokens: QueryBudgetTokens}
		h.budgets[key] = entry
	}
	elapsed := now.Sub(entry.at)
	if elapsed > 0 {
		entry.tokens += float64(elapsed) / float64(QueryBudgetRefill)
		if entry.tokens > QueryBudgetTokens {
			entry.tokens = QueryBudgetTokens
		}
		entry.at = now
	}
	if entry.tokens < 1 {
		return false
	}
	entry.tokens--
	return true
}

func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (live.Session, bool) {
	if h.opts.Auth == nil {
		writeError(w, http.StatusServiceUnavailable, "browser access is not configured")
		return live.Session{}, false
	}
	session, err := h.opts.Auth.Authenticate(r)
	if err != nil {
		if err == live.ErrBrowserDisabled {
			writeError(w, http.StatusServiceUnavailable, "browser access is not configured")
		} else {
			writeError(w, http.StatusUnauthorized, "authentication required")
		}
		return live.Session{}, false
	}
	return session, true
}

func writeError(w http.ResponseWriter, status int, message string) {
	body, err := api.EncodeAPIError(api.APIError{Error: message})
	if err != nil {
		body = []byte(`{"error":"internal error"}`)
	}
	writeBody(w, status, body)
}

func writeBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func formatTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

var _ = context.Background
