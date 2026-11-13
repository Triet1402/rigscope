package alerts

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
)

// Limits mirrors src/alerts/types.ts ALERT_LIMITS and internal/api.AlertLimits.
const (
	MaxRules              = 200
	MaxIncidents          = 1000
	MaxJobs               = 1000
	MaxRuleName           = 128
	MaxMachineID          = 128
	MaxResourceID         = 256
	MaxDurationMs         = 86_400_000
	MaxThreshold          = 1e13
	MaxWebhookURL         = 2048
	MaxWebhookHeaders     = 16
	MaxWebhookHeaderKey   = 128
	MaxWebhookHeaderValue = 1024
	MaxWebhookSecret      = 256
	MaxListLimit          = 500

	// DefaultListLimit is the incident page size GET /api/alerts uses when the
	// browser sends no limit.
	DefaultListLimit = 200
	// DefaultMaxAttempts is the delivery attempt bound for one job.
	DefaultMaxAttempts = 6
	// DefaultDeliveryTimeout bounds one webhook request.
	DefaultDeliveryTimeout = 5 * time.Second
)

const (
	retryBaseMs           = 5_000
	retryCapMs            = 300_000
	missingWebhookRetryMs = 60_000
	maxPayloadBytes       = 64 * 1024
	maxErrorText          = 256
	maxResponseBodyBytes  = 8 * 1024
)

// DeliveryDisclosure is the fixed at-least-once disclosure GET /api/alerts
// returns verbatim.
const DeliveryDisclosure = "Webhook delivery is at-least-once. After a network ambiguity the receiver may see the same deterministic job id more than once; retain it as an idempotency key."

// Limits is the response value shared by every alert list.
var Limits = api.AlertLimits{
	MaxRules:              MaxRules,
	MaxIncidents:          MaxIncidents,
	MaxJobs:               MaxJobs,
	MaxRuleName:           MaxRuleName,
	MaxMachineID:          MaxMachineID,
	MaxResourceID:         MaxResourceID,
	MaxDurationMs:         MaxDurationMs,
	MaxThreshold:          MaxThreshold,
	MaxWebhookURL:         MaxWebhookURL,
	MaxWebhookHeaders:     MaxWebhookHeaders,
	MaxWebhookHeaderKey:   MaxWebhookHeaderKey,
	MaxWebhookHeaderValue: MaxWebhookHeaderValue,
	MaxWebhookSecret:      MaxWebhookSecret,
	MaxListLimit:          MaxListLimit,
}

// metricSpec is the label, unit and resource requirement for one alert metric.
type metricSpec struct {
	label         string
	unit          storage.MetricUnit
	needsResource bool
}

// alertMetrics mirrors ALERT_METRICS from src/alerts/types.ts. Percent values
// are on the metric's native 0-100 scale.
var alertMetrics = map[api.AlertMetric]metricSpec{
	api.AlertMetricCPUUsage:               {"CPU usage", storage.UnitPercent, false},
	api.AlertMetricCPUCore:                {"CPU core usage", storage.UnitPercent, true},
	api.AlertMetricCPUTemperature:         {"CPU temperature", storage.UnitCelsius, false},
	api.AlertMetricCPUPower:               {"CPU package power", storage.UnitWatts, false},
	api.AlertMetricCPUIOWait:              {"CPU I/O wait", storage.UnitPercent, false},
	api.AlertMetricCPUPressure:            {"CPU pressure", storage.UnitPercent, false},
	api.AlertMetricMemoryUsed:             {"Memory used", storage.UnitBytes, false},
	api.AlertMetricMemoryTotal:            {"Memory total", storage.UnitBytes, false},
	api.AlertMetricMemoryUsage:            {"Memory usage", storage.UnitPercent, false},
	api.AlertMetricMemoryPressureSome:     {"Memory pressure (some)", storage.UnitPercent, false},
	api.AlertMetricMemoryPressureFull:     {"Memory pressure (full)", storage.UnitPercent, false},
	api.AlertMetricSwapUsed:               {"Swap used", storage.UnitBytes, false},
	api.AlertMetricSwapTotal:              {"Swap total", storage.UnitBytes, false},
	api.AlertMetricGPUUtilization:         {"GPU utilization", storage.UnitPercent, true},
	api.AlertMetricGPUMemoryUsed:          {"GPU memory used", storage.UnitBytes, true},
	api.AlertMetricGPUMemoryTotal:         {"GPU memory total", storage.UnitBytes, true},
	api.AlertMetricGPUTemperature:         {"GPU temperature", storage.UnitCelsius, true},
	api.AlertMetricGPUJunctionTemperature: {"GPU junction temperature", storage.UnitCelsius, true},
	api.AlertMetricGPUVRAMTemperature:     {"GPU VRAM temperature", storage.UnitCelsius, true},
	api.AlertMetricGPUPower:               {"GPU power", storage.UnitWatts, true},
	api.AlertMetricIOPressureSome:         {"I/O pressure (some)", storage.UnitPercent, false},
	api.AlertMetricIOPressureFull:         {"I/O pressure (full)", storage.UnitPercent, false},
	api.AlertMetricFSUsed:                 {"Filesystem used", storage.UnitBytes, true},
	api.AlertMetricFSTotal:                {"Filesystem total", storage.UnitBytes, true},
	api.AlertMetricFSUsage:                {"Filesystem usage", storage.UnitPercent, true},
	api.AlertMetricFSInodesUsage:          {"Filesystem inode usage", storage.UnitPercent, true},
	api.AlertMetricDiskRead:               {"Disk read", storage.UnitBytesPerSecond, true},
	api.AlertMetricDiskWrite:              {"Disk write", storage.UnitBytesPerSecond, true},
	api.AlertMetricDiskReadLatency:        {"Disk read latency", storage.UnitMilliseconds, true},
	api.AlertMetricDiskWriteLatency:       {"Disk write latency", storage.UnitMilliseconds, true},
	api.AlertMetricDiskUtilization:        {"Disk utilization", storage.UnitPercent, true},
	api.AlertMetricNetRx:                  {"Network receive", storage.UnitBytesPerSecond, true},
	api.AlertMetricNetTx:                  {"Network transmit", storage.UnitBytesPerSecond, true},
}

// MetricNames returns the alert metric catalog in stable order.
func MetricNames() []string {
	names := make([]string, 0, len(alertMetrics))
	for metric := range alertMetrics {
		names = append(names, string(metric))
	}
	sort.Strings(names)
	return names
}

func metricSpecFor(metric api.AlertMetric) (metricSpec, bool) {
	spec, ok := alertMetrics[metric]
	return spec, ok
}

// metricFamily is the payload family that supplies a metric. I/O pressure
// belongs to the disks family.
func metricFamily(metric api.AlertMetric) string {
	name := string(metric)
	switch {
	case strings.HasPrefix(name, "cpu."):
		return "cpu"
	case strings.HasPrefix(name, "memory."):
		return "memory"
	case strings.HasPrefix(name, "swap."):
		return "swap"
	case strings.HasPrefix(name, "gpu."):
		return "gpus"
	case strings.HasPrefix(name, "io."), strings.HasPrefix(name, "fs."), strings.HasPrefix(name, "disk."):
		return "disks"
	default:
		return "network"
	}
}

// Mode distinguishes a newly committed LIVE record from replayed history.
type Mode uint8

const (
	// Live is a new durable live metrics commit. Only live records are
	// evaluated; a replay must never reopen a condition.
	Live Mode = iota
	// Replay is historical data. Observe ignores it.
	Replay
)

// Observer is the ingestion seam the hub composition root wires to live
// record commits. The record must be a newly committed LIVE metrics record;
// Replay never changes state. Observe uses the engine clock as the receipt
// time; Engine.ObserveAt accepts the hub's exact receipt time when ingestion
// records it separately.
type Observer interface {
	Observe(machineID string, record protocol.MetricsRecord, mode Mode)
}

var _ Observer = (*Engine)(nil)

// MachineMetrics is the last live payload the hub holds for a machine.
type MachineMetrics struct {
	Payload    protocol.MetricsPayload
	ReceivedAt time.Time
}

// MachineState identifies one machine for a stale or unavailable tick.
// Machines absent from a Tick snapshot are untouched. ExpectedIntervalMs
// supplies the stale threshold. LastReceivedAt and Metrics are retained for
// callers that build a fleet snapshot, but a tick ignores them as evaluation
// evidence: evidence must come from observations the engine itself evaluated,
// never from live state that may still be queued. The one exception is a
// LastReceivedAt older than the engine's start, which a stale rule may use as a
// durable restart fallback because no receipt from before the start can still
// be queued.
type MachineState struct {
	MachineID string
	// LastReceivedAt is the last receipt time, or nil before the first record.
	LastReceivedAt *time.Time
	// ExpectedIntervalMs is the collector's advertised cadence, or nil when
	// unknown.
	ExpectedIntervalMs *int64
	// Metrics is the last known live payload, or nil before the first record.
	Metrics *MachineMetrics
}

// staleThreshold is max(15s, 3 x the advertised cadence).
func staleThreshold(expectedIntervalMs *int64) time.Duration {
	const floor = 15 * time.Second
	if expectedIntervalMs == nil || *expectedIntervalMs <= 0 {
		return floor
	}
	cadence := time.Duration(*expectedIntervalMs) * time.Millisecond
	if cadence <= 0 {
		return floor
	}
	threshold := 3 * cadence
	if threshold < floor {
		return floor
	}
	return threshold
}

// Scope is a session's machine visibility.
type Scope interface {
	Contains(machineID string) bool
}

type allMachinesScope struct{}

func (allMachinesScope) Contains(string) bool { return true }

// AllMachines returns a wildcard scope.
func AllMachines() Scope { return allMachinesScope{} }

// ValidationError is a bounded, user-facing rule or webhook validation
// failure. The HTTP layer maps it to 400 with its message.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func validationf(format string, args ...any) *ValidationError {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

// NotFoundError reports an unknown rule or incident.
type NotFoundError struct {
	Resource string
}

func (e *NotFoundError) Error() string {
	if e.Resource == "" {
		return "alert rule not found"
	}
	return e.Resource + " not found"
}

func notFound(resource string) *NotFoundError { return &NotFoundError{Resource: resource} }

// IsValidation reports whether err is a validation failure.
func IsValidation(err error) bool {
	var target *ValidationError
	return errors.As(err, &target)
}

// IsNotFound reports whether err is a not-found failure.
func IsNotFound(err error) bool {
	var target *NotFoundError
	return errors.As(err, &target)
}

// isoLayout renders the fixed ISO-8601 form the browser contract expects:
// millisecond precision in UTC.
const isoLayout = "2006-01-02T15:04:05.000Z"

func formatTime(value time.Time) string {
	return value.UTC().Format(isoLayout)
}

func formatTimePtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	text := formatTime(*value)
	return &text
}

func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	text := *value
	return &text
}

func cloneFloatPtr(value *float64) *float64 {
	if value == nil {
		return nil
	}
	number := *value
	return &number
}

// utf16Len is the JavaScript string length unit the reference bounds use.
func utf16Len(value string) int {
	n := 0
	for _, r := range value {
		if r > 0xffff {
			n += 2
			continue
		}
		n++
	}
	return n
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func hasSpace(value string) bool {
	for _, r := range value {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' {
			return true
		}
	}
	return false
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

// formatBytes mirrors the reference's human-readable byte formatting.
func formatBytes(bytes float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	value := math.Max(0, bytes)
	index := 0
	for value >= 1024 && index < len(units)-1 {
		value /= 1024
		index++
	}
	if index == 0 {
		return fmt.Sprintf("%.0f %s", math.Round(value), units[index])
	}
	return fmt.Sprintf("%.1f %s", value, units[index])
}

func jsRound(value float64) float64 {
	return math.Floor(value + 0.5)
}

// formatMetricValue mirrors formatValue in the reference engine.
func formatMetricValue(unit storage.MetricUnit, value *float64) string {
	if value == nil || !finite(*value) {
		return "unavailable"
	}
	switch unit {
	case storage.UnitPercent:
		return trimNumber(jsRound(*value*100)/100) + "%"
	case storage.UnitBytes:
		return formatBytes(*value)
	case storage.UnitBytesPerSecond:
		return formatBytes(*value) + "/s"
	case storage.UnitCelsius:
		return trimNumber(jsRound(*value)) + " °C"
	case storage.UnitWatts:
		return trimNumber(jsRound(*value)) + " W"
	case storage.UnitMilliseconds:
		return trimNumber(jsRound(*value*10)/10) + " ms"
	default:
		return trimNumber(*value)
	}
}

func trimNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func formatDuration(ms int64) string {
	switch {
	case ms < 60_000:
		return fmt.Sprintf("%.0f s", jsRound(float64(ms)/1000))
	case ms < 3_600_000:
		return fmt.Sprintf("%.0f min", jsRound(float64(ms)/60_000))
	default:
		return fmt.Sprintf("%.0f h", jsRound(float64(ms)/3_600_000))
	}
}

func conditionPhrase(condition api.AlertCondition) string {
	switch condition {
	case api.AlertConditionAbove:
		return "at or above"
	case api.AlertConditionBelow:
		return "at or below"
	case api.AlertConditionStale:
		return "stale"
	default:
		return "unavailable"
	}
}

func metricLabel(metric api.AlertMetric, resourceID string) string {
	spec := alertMetrics[metric]
	if resourceID == "" {
		return spec.label
	}
	return spec.label + " (" + resourceID + ")"
}

// RuleInput is one rule create/update request. ID is present to update and
// absent (empty) to create. Threshold is nil for a JSON null or an absent
// field; a non-nil numeric threshold is rejected for stale/unavailable.
type RuleInput struct {
	ID               string
	Name             string
	MachineID        string
	Metric           api.AlertMetric
	ResourceID       string
	Condition        api.AlertCondition
	Threshold        *float64
	DurationMs       int64
	Enabled          *bool
	MaintenanceUntil *time.Time
}

// RuleInputFromAPI maps a decoded browser body onto the engine input. The
// browser decoder has already enforced shape, key set and field bounds.
func RuleInputFromAPI(input api.AlertRuleInput) RuleInput {
	out := RuleInput{
		Name:      input.Name,
		MachineID: input.MachineID,
		Metric:    input.Metric,
		Condition: input.Condition,
	}
	if input.ID != nil {
		out.ID = *input.ID
	}
	if input.ResourceID != nil {
		out.ResourceID = *input.ResourceID
	}
	if input.Threshold != nil && input.Threshold.Valid {
		value := input.Threshold.Value
		out.Threshold = &value
	}
	out.DurationMs = input.DurationMs
	out.Enabled = input.Enabled
	if input.MaintenanceUntil != nil && input.MaintenanceUntil.Valid {
		if parsed, err := time.Parse(time.RFC3339Nano, input.MaintenanceUntil.Value); err == nil {
			out.MaintenanceUntil = &parsed
		}
	}
	return out
}
