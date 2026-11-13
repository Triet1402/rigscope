package api

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// knownMetrics is this build's storage metric catalog, mirroring
// internal/hub/storage. It is the allowlist for /api/history queries and
// the alert rule metric set. It is never exposed directly: callers receive a
// defensive copy so package state cannot be mutated from outside.
var knownMetrics = map[string]MetricUnit{
	"cpu.usage":                MetricUnitPercent,
	"cpu.core":                 MetricUnitPercent,
	"cpu.temperature":          MetricUnitCelsius,
	"cpu.power":                MetricUnitWatts,
	"cpu.iowait":               MetricUnitPercent,
	"cpu.pressure":             MetricUnitPercent,
	"memory.used":              MetricUnitBytes,
	"memory.total":             MetricUnitBytes,
	"memory.usage":             MetricUnitPercent,
	"memory.pressure.some":     MetricUnitPercent,
	"memory.pressure.full":     MetricUnitPercent,
	"swap.used":                MetricUnitBytes,
	"swap.total":               MetricUnitBytes,
	"gpu.utilization":          MetricUnitPercent,
	"gpu.memory.used":          MetricUnitBytes,
	"gpu.memory.total":         MetricUnitBytes,
	"gpu.temperature":          MetricUnitCelsius,
	"gpu.junction.temperature": MetricUnitCelsius,
	"gpu.vram.temperature":     MetricUnitCelsius,
	"gpu.power":                MetricUnitWatts,
	"fs.used":                  MetricUnitBytes,
	"fs.total":                 MetricUnitBytes,
	"fs.usage":                 MetricUnitPercent,
	"fs.inodes.usage":          MetricUnitPercent,
	"io.pressure.some":         MetricUnitPercent,
	"io.pressure.full":         MetricUnitPercent,
	"disk.read":                MetricUnitBytesPerSecond,
	"disk.write":               MetricUnitBytesPerSecond,
	"disk.read.latency":        MetricUnitMilliseconds,
	"disk.write.latency":       MetricUnitMilliseconds,
	"disk.utilization":         MetricUnitPercent,
	"net.rx":                   MetricUnitBytesPerSecond,
	"net.tx":                   MetricUnitBytesPerSecond,
}

// KnownMetrics returns a defensive copy of this build's storage metric
// catalog, keyed by metric name.
func KnownMetrics() map[string]MetricUnit {
	out := make(map[string]MetricUnit, len(knownMetrics))
	for name, unit := range knownMetrics {
		out[name] = unit
	}
	return out
}

// KnownMetric reports whether name is a storage metric this build understands.
func KnownMetric(name string) bool {
	_, ok := knownMetrics[name]
	return ok
}

// ---------------------------------------------------------------------------
// Machine metadata
// ---------------------------------------------------------------------------

// MachineMeta is the machine display name and tags. displayName is required
// and nullable; tags is required and deduplicated by the hub.
//
//api:response
type MachineMeta struct {
	DisplayName *string  `json:"displayName"` //api:length=0..128
	Tags        []string `json:"tags"`        //api:items=0..32
}

func (m MachineMeta) validate() error {
	if m.DisplayName != nil {
		if err := validateText(*m.DisplayName, "displayName", 0, MaxDisplayNameLength); err != nil {
			return err
		}
	}
	return validateTags(m.Tags)
}

// MachineMetaInput is the POST /api/machines/meta body. At least one of the
// keys must be present. An explicit null displayName clears the name; an
// absent key leaves the stored value alone.
type MachineMetaInput struct {
	DisplayName *protocol.Optional[string] `json:"displayName,omitempty"`
	Tags        *[]string                  `json:"tags,omitempty"`
}

// UnmarshalJSON tracks key presence separately from value: an absent key
// leaves the stored value alone, while an explicit null clears displayName.
// Request tags are trimmed and deduplicated the way the Node handler stores
// them ([" lab ", "lab"] becomes ["lab"]); stored responses remain strict.
func (m *MachineMetaInput) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data, "machine metadata input", nil, []string{"displayName", "tags"})
	if err != nil {
		return err
	}
	var out MachineMetaInput
	if raw, ok := fields["displayName"]; ok {
		var value protocol.Optional[string]
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("displayName: %w", err)
		}
		out.DisplayName = &value
	}
	if raw, ok := fields["tags"]; ok {
		if isNull(raw) {
			return fmt.Errorf("tags must be an array, not null")
		}
		var tags []string
		if err := json.Unmarshal(raw, &tags); err != nil {
			return fmt.Errorf("tags: %w", err)
		}
		normalized, err := normalizeInputTags(tags)
		if err != nil {
			return err
		}
		out.Tags = &normalized
	}
	*m = out
	return nil
}

// normalizeInputTags applies the Node request semantics: each raw tag is
// validated before trimming, then trimmed values are deduplicated in first-seen
// order. Empty tags are rejected.
func normalizeInputTags(tags []string) ([]string, error) {
	if len(tags) > MaxTags {
		return nil, fmt.Errorf("tags exceeds %d entries", MaxTags)
	}
	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" {
			return nil, fmt.Errorf("tags must be non-empty")
		}
		if err := validateText(tag, "tag", 1, MaxTagLength); err != nil {
			return nil, err
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out, nil
}

func (m MachineMetaInput) validate() error {
	if m.DisplayName == nil && m.Tags == nil {
		return fmt.Errorf("machine metadata requires displayName or tags")
	}
	if m.DisplayName != nil && m.DisplayName.Valid {
		if err := validateText(m.DisplayName.Value, "displayName", 0, MaxDisplayNameLength); err != nil {
			return err
		}
	}
	if m.Tags != nil {
		return validateTags(*m.Tags)
	}
	return nil
}

func validateTags(tags []string) error {
	if len(tags) > MaxTags {
		return fmt.Errorf("tags exceeds %d entries", MaxTags)
	}
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" || trimmed != tag {
			return fmt.Errorf("tags must be non-empty and trimmed")
		}
		if err := validateText(tag, "tag", 1, MaxTagLength); err != nil {
			return err
		}
		if _, duplicate := seen[tag]; duplicate {
			return fmt.Errorf("duplicate tag %q", tag)
		}
		seen[tag] = struct{}{}
	}
	return nil
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

// HistoryMode selects the hub's history backend.
type HistoryMode string

// History modes.
const (
	HistoryModeMemory     HistoryMode = "memory"
	HistoryModePersistent HistoryMode = "persistent"
)

// HistoryTier is one storage resolution.
type HistoryTier string

// History storage tiers. "1s" is the in-RAM ring.
const (
	HistoryTierOneSecond HistoryTier = "1s"
	HistoryTierRaw       HistoryTier = "raw"
	HistoryTierTenSecond HistoryTier = "10s"
	HistoryTierMinute    HistoryTier = "1m"
	HistoryTierHour      HistoryTier = "1h"
)

// StorageTier is the persistent history backend's stored resolution.
type StorageTier string

// Persistent storage tiers.
const (
	StorageTierRaw     StorageTier = "raw"
	StorageTierSummary StorageTier = "summary"
)

// ProcessState is one /api/processes viewer state.
type ProcessState string

// Process viewer states.
const (
	ProcessStateLoading      ProcessState = "loading"
	ProcessStateReady        ProcessState = "ready"
	ProcessStateExpired      ProcessState = "expired"
	ProcessStateCancelled    ProcessState = "cancelled"
	ProcessStateWithheld     ProcessState = "withheld"
	ProcessStateUnsupported  ProcessState = "unsupported"
	ProcessStateBusy         ProcessState = "busy"
	ProcessStateFailed       ProcessState = "failed"
	ProcessStateDisconnected ProcessState = "disconnected"
)

func validateProcessState(state ProcessState) error {
	switch state {
	case ProcessStateLoading, ProcessStateReady, ProcessStateExpired,
		ProcessStateCancelled, ProcessStateWithheld, ProcessStateUnsupported,
		ProcessStateBusy, ProcessStateFailed, ProcessStateDisconnected:
		return nil
	default:
		return fmt.Errorf("unknown process state %q", state)
	}
}

// MetricUnit is a storage unit from the shared metric catalog.
type MetricUnit string

// Metric units.
const (
	MetricUnitPercent        MetricUnit = "percent"
	MetricUnitBytes          MetricUnit = "bytes"
	MetricUnitBytesPerSecond MetricUnit = "bytes_per_second"
	MetricUnitCelsius        MetricUnit = "celsius"
	MetricUnitWatts          MetricUnit = "watts"
	MetricUnitMilliseconds   MetricUnit = "milliseconds"
)

// GapKind distinguishes a missing interval from an explicitly reported drop.
type GapKind string

// Gap kinds.
const (
	GapKindMissing GapKind = "missing"
	GapKindDropped GapKind = "dropped"
)

// HistoryTierStatus describes one tier's retention and memory footprint.
//
//api:response
type HistoryTierStatus struct {
	Tier        HistoryTier `json:"tier"`
	StepMs      int64       `json:"stepMs"`      //api:range=0..Number.MAX_SAFE_INTEGER
	RetentionMs int64       `json:"retentionMs"` //api:range=0..Number.MAX_SAFE_INTEGER
	Bytes       int64       `json:"bytes"`       //api:range=0..Number.MAX_SAFE_INTEGER
}

// MemoryHistoryStatus is the in-RAM history backend's state.
//
//api:response
type MemoryHistoryStatus struct {
	BudgetBytes    int64               `json:"budgetBytes"` //api:range=0..Number.MAX_SAFE_INTEGER
	UsedBytes      int64               `json:"usedBytes"`   //api:range=0..Number.MAX_SAFE_INTEGER
	Pressure       bool                `json:"pressure"`
	SeriesCount    int64               `json:"seriesCount"`    //api:range=0..Number.MAX_SAFE_INTEGER
	MachineCount   int64               `json:"machineCount"`   //api:range=0..Number.MAX_SAFE_INTEGER
	EvictedSeries  int64               `json:"evictedSeries"`  //api:range=0..Number.MAX_SAFE_INTEGER
	RejectedSeries int64               `json:"rejectedSeries"` //api:range=0..Number.MAX_SAFE_INTEGER
	OldestMs       *int64              `json:"oldestMs"`       //api:range=0..Number.MAX_SAFE_INTEGER
	NewestMs       *int64              `json:"newestMs"`       //api:range=0..Number.MAX_SAFE_INTEGER
	Tiers          []HistoryTierStatus `json:"tiers"`
}

// PersistentHistoryStatus is the SQLite summary backend's state.
//
//api:response
type PersistentHistoryStatus struct {
	Enabled             bool                `json:"enabled"`
	StorageTier         StorageTier         `json:"storageTier"`
	FlushIntervalMs     int64               `json:"flushIntervalMs"`     //api:range=0..Number.MAX_SAFE_INTEGER
	QueuedRecords       int64               `json:"queuedRecords"`       //api:range=0..Number.MAX_SAFE_INTEGER
	QueuedBatches       int64               `json:"queuedBatches"`       //api:range=0..Number.MAX_SAFE_INTEGER
	LastFlushMs         *int64              `json:"lastFlushMs"`         //api:range=0..Number.MAX_SAFE_INTEGER
	LastFlushDurationMs *int64              `json:"lastFlushDurationMs"` //api:range=0..Number.MAX_SAFE_INTEGER
	WriteFailures       int64               `json:"writeFailures"`       //api:range=0..Number.MAX_SAFE_INTEGER
	LastWriteError      *string             `json:"lastWriteError"`
	DBBytes             int64               `json:"dbBytes"`      //api:range=0..Number.MAX_SAFE_INTEGER
	DBWalBytes          int64               `json:"dbWalBytes"`   //api:range=0..Number.MAX_SAFE_INTEGER
	DBBytesLimit        *int64              `json:"dbBytesLimit"` //api:range=0..Number.MAX_SAFE_INTEGER
	Tiers               []HistoryTierStatus `json:"tiers"`
}

// HistoryStatus is GET /api/history/status. Exactly one of memory and
// persistence is non-null, matching mode.
//
//api:response
type HistoryStatus struct {
	Mode              HistoryMode              `json:"mode"`
	PersistentHistory bool                     `json:"persistentHistory"`
	AvailableFromMs   *int64                   `json:"availableFromMs"`
	AvailableToMs     *int64                   `json:"availableToMs"`
	Memory            *MemoryHistoryStatus     `json:"memory"`
	Persistence       *PersistentHistoryStatus `json:"persistence"`
	Gaps              int64                    `json:"gaps"`            //api:range=0..Number.MAX_SAFE_INTEGER
	AcceptedRecords   int64                    `json:"acceptedRecords"` //api:range=0..Number.MAX_SAFE_INTEGER
	GeneratedAt       string                   `json:"generatedAt"`
}

func (s HistoryStatus) validate() error {
	switch s.Mode {
	case HistoryModeMemory:
		if s.PersistentHistory || s.Memory == nil || s.Persistence != nil {
			return fmt.Errorf("memory history status must have memory state and no persistence")
		}
		if err := validateTiers(s.Memory.Tiers); err != nil {
			return err
		}
	case HistoryModePersistent:
		if !s.PersistentHistory || s.Memory != nil || s.Persistence == nil {
			return fmt.Errorf("persistent history status must have persistence state and no memory")
		}
		if err := validateTiers(s.Persistence.Tiers); err != nil {
			return err
		}
		if err := validateStorageTier(s.Persistence.StorageTier); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown history mode %q", s.Mode)
	}
	return validateTimestamp(s.GeneratedAt, "generatedAt")
}

func validateTiers(tiers []HistoryTierStatus) error {
	if tiers == nil {
		return fmt.Errorf("tiers must be an array")
	}
	for _, tier := range tiers {
		if err := validateTier(tier.Tier); err != nil {
			return err
		}
		if tier.StepMs < 0 || tier.RetentionMs < 0 || tier.Bytes < 0 {
			return fmt.Errorf("history tier values must not be negative")
		}
	}
	return nil
}

func validateTier(tier HistoryTier) error {
	switch tier {
	case HistoryTierOneSecond, HistoryTierRaw, HistoryTierTenSecond, HistoryTierMinute, HistoryTierHour:
		return nil
	default:
		return fmt.Errorf("unknown history tier %q", tier)
	}
}

func validateStorageTier(tier StorageTier) error {
	switch tier {
	case StorageTierRaw, StorageTierSummary:
		return nil
	default:
		return fmt.Errorf("unknown storage tier %q", tier)
	}
}

// MetricDefinition is one metric catalog entry.
//
//api:response
type MetricDefinition struct {
	Unit MetricUnit `json:"unit"`
}

// MetricCatalog is the metric name → definition map.
type MetricCatalog map[string]MetricDefinition

// MetricCatalogResponse is GET /api/history/metrics.
//
//api:response
type MetricCatalogResponse struct {
	Metrics MetricCatalog `json:"metrics"`
}

func (r MetricCatalogResponse) validate() error {
	if r.Metrics == nil {
		return fmt.Errorf("metrics must be an object")
	}
	for name, definition := range r.Metrics {
		if name == "" {
			return fmt.Errorf("metric name must not be empty")
		}
		if err := validateMetricUnit(definition.Unit); err != nil {
			return fmt.Errorf("metric %q: %w", name, err)
		}
	}
	return nil
}

func validateMetricUnit(unit MetricUnit) error {
	switch unit {
	case MetricUnitPercent, MetricUnitBytes, MetricUnitBytesPerSecond,
		MetricUnitCelsius, MetricUnitWatts, MetricUnitMilliseconds:
		return nil
	default:
		return fmt.Errorf("unknown metric unit %q", unit)
	}
}

// HistoryQuery is the bounded GET /api/history query after parsing. The hub
// rejects an empty metrics list, a non-positive range, a range wider than 400
// days, too many metrics/resources and an out-of-range maxPoints.
//
//api:skip
type HistoryQuery struct {
	MachineID   string
	Metrics     []string
	ResourceIDs []string
	// CoreIDs distinguishes an absent coreIds parameter (nil, no restriction)
	// from an explicit empty list (non-nil empty, which excludes every
	// cpu.core series), matching the Node hub's URLSearchParams semantics.
	CoreIDs   *[]string
	FromMs    int64
	ToMs      int64
	MaxPoints int
}

func (q HistoryQuery) validate() error {
	if err := validateText(q.MachineID, "machineId", 1, MaxHistoryMachineIDLength); err != nil {
		return err
	}
	if len(q.Metrics) == 0 || len(q.Metrics) > MaxHistoryMetrics {
		return fmt.Errorf("metrics must contain 1..%d metric names", MaxHistoryMetrics)
	}
	seenMetrics := make(map[string]struct{}, len(q.Metrics))
	for _, metric := range q.Metrics {
		if !KnownMetric(metric) {
			return fmt.Errorf("unknown metric %q", metric)
		}
		if _, duplicate := seenMetrics[metric]; duplicate {
			return fmt.Errorf("duplicate metric %q", metric)
		}
		seenMetrics[metric] = struct{}{}
	}
	if len(q.ResourceIDs) > MaxHistoryResourceIDs {
		return fmt.Errorf("resourceIds must contain at most %d ids", MaxHistoryResourceIDs)
	}
	seenResources := make(map[string]struct{}, len(q.ResourceIDs))
	for _, id := range q.ResourceIDs {
		if err := validateText(id, "resourceId", 1, MaxResourceIDLength); err != nil {
			return err
		}
		if _, duplicate := seenResources[id]; duplicate {
			return fmt.Errorf("duplicate resource id %q", id)
		}
		seenResources[id] = struct{}{}
	}
	if q.CoreIDs != nil {
		if len(*q.CoreIDs) > protocol.MaxCores {
			return fmt.Errorf("coreIds must contain at most %d ids", protocol.MaxCores)
		}
		seenCores := make(map[string]struct{}, len(*q.CoreIDs))
		for _, id := range *q.CoreIDs {
			if !coreIDPattern.MatchString(id) {
				return fmt.Errorf("invalid core id %q", id)
			}
			index, err := strconv.Atoi(strings.TrimPrefix(id, "core:"))
			if err != nil || index < 0 || index >= protocol.MaxCores {
				return fmt.Errorf("invalid core id %q", id)
			}
			if _, duplicate := seenCores[id]; duplicate {
				return fmt.Errorf("duplicate core id %q", id)
			}
			seenCores[id] = struct{}{}
		}
	}
	if !isSafeInt64(q.FromMs) || !isSafeInt64(q.ToMs) {
		return fmt.Errorf("from and to must be safe integer epoch milliseconds")
	}
	if q.FromMs >= q.ToMs {
		return fmt.Errorf("fromMs must be before toMs")
	}
	if q.ToMs-q.FromMs > 400*24*60*60*1000 {
		return fmt.Errorf("range exceeds 400 days")
	}
	// Node clamps maxPoints to 2000 instead of rejecting a larger value; a
	// non-positive or non-integral value is a client error.
	if q.MaxPoints < 1 {
		return fmt.Errorf("maxPoints must be a positive integer")
	}
	return nil
}

// coreIDPattern matches the shared cpu.core resource identifier.
var coreIDPattern = regexp.MustCompile(`^core:(?:0|[1-9][0-9]{0,3})$`)

// HistoryPoint is one downsampled bucket. All three values are null when the
// bucket only saw explicit unavailable readings.
//
//api:response
type HistoryPoint struct {
	T   int64    `json:"t"`
	Min *float64 `json:"min"`
	Max *float64 `json:"max"`
	Avg *float64 `json:"avg"`
}

// HistorySeries is one metric/resource series. Empty buckets are omitted and
// never zero-filled.
//
//api:response
type HistorySeries struct {
	Metric     string         `json:"metric"`
	ResourceID string         `json:"resourceId"`
	Unit       MetricUnit     `json:"unit"`
	Points     []HistoryPoint `json:"points"`
}

// HistoryGap is one missing or explicitly dropped interval.
//
//api:response
type HistoryGap struct {
	FromMs         int64   `json:"fromMs"`
	ToMs           int64   `json:"toMs"`
	Kind           GapKind `json:"kind"`
	Reason         *string `json:"reason,omitempty"`
	DroppedRecords *int64  `json:"droppedRecords,omitempty"`
}

// HistoryResult is the GET /api/history response.
//
//api:response
type HistoryResult struct {
	MachineID string          `json:"machineId"` //api:length=1..128
	FromMs    int64           `json:"fromMs"`
	ToMs      int64           `json:"toMs"`
	StepMs    int64           `json:"stepMs"` //api:range=1..Number.MAX_SAFE_INTEGER
	Tier      HistoryTier     `json:"tier"`
	Series    []HistorySeries `json:"series"`
	Gaps      []HistoryGap    `json:"gaps"`
	Truncated bool            `json:"truncated"`
	ElapsedMs float64         `json:"elapsedMs"` //api:range=0..Number.MAX_VALUE
}

func (r HistoryResult) validate() error {
	if err := validateText(r.MachineID, "machineId", 1, MaxMachineIDQueryLength); err != nil {
		return err
	}
	if err := validateTier(r.Tier); err != nil {
		return err
	}
	if r.StepMs <= 0 {
		return fmt.Errorf("stepMs must be positive")
	}
	if r.Series == nil || r.Gaps == nil {
		return fmt.Errorf("series and gaps must be arrays")
	}
	if r.ElapsedMs < 0 || math.IsNaN(r.ElapsedMs) || math.IsInf(r.ElapsedMs, 0) {
		return fmt.Errorf("elapsedMs must be a non-negative number")
	}
	for _, series := range r.Series {
		if series.Metric == "" {
			return fmt.Errorf("series requires a metric")
		}
		if err := validateText(series.ResourceID, "resourceId", 0, MaxResourceIDLength); err != nil {
			return err
		}
		if err := validateMetricUnit(series.Unit); err != nil {
			return err
		}
		if series.Points == nil {
			return fmt.Errorf("series points must be an array")
		}
		for _, point := range series.Points {
			if err := validateFinitePtr(point.Min, "min"); err != nil {
				return err
			}
			if err := validateFinitePtr(point.Max, "max"); err != nil {
				return err
			}
			if err := validateFinitePtr(point.Avg, "avg"); err != nil {
				return err
			}
		}
	}
	for _, gap := range r.Gaps {
		if gap.Kind != GapKindMissing && gap.Kind != GapKindDropped {
			return fmt.Errorf("unknown gap kind %q", gap.Kind)
		}
		if gap.ToMs < gap.FromMs {
			return fmt.Errorf("gap range is inverted")
		}
	}
	return nil
}

func validateFinitePtr(value *float64, what string) error {
	if value == nil {
		return nil
	}
	if math.IsNaN(*value) || math.IsInf(*value, 0) {
		return fmt.Errorf("%s must be finite", what)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Process detail
// ---------------------------------------------------------------------------

// ProcessAcquireResponse is the POST /api/processes response: the viewer lease
// terms the browser must renew within.
//
//api:response
type ProcessAcquireResponse struct {
	LeaseMs    int64 `json:"leaseMs"`    //api:range=1..Number.MAX_SAFE_INTEGER
	IntervalMs int64 `json:"intervalMs"` //api:range=1..Number.MAX_SAFE_INTEGER
}

func (r ProcessAcquireResponse) validate() error {
	if r.LeaseMs <= 0 || r.IntervalMs <= 0 {
		return fmt.Errorf("process lease terms must be positive")
	}
	return nil
}

// ProcessReadResponse is the GET /api/processes response. "state" is one of
// loading, ready, expired, cancelled, withheld, unsupported, busy, failed or
// disconnected; "result" is the latest collector snapshot, or null while
// loading or after the interest ended.
//
//api:response
type ProcessReadResponse struct {
	State  ProcessState           `json:"state"`
	Result *protocol.DetailResult `json:"result"`
}

func (r ProcessReadResponse) validate() error {
	return validateProcessState(r.State)
}
