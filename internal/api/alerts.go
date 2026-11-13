package api

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// ---------------------------------------------------------------------------
// Alert domain
// ---------------------------------------------------------------------------

// AlertMetric is a storage metric that an alert rule can watch. The set
// matches KnownMetrics.
type AlertMetric string

// Alert metrics.
const (
	AlertMetricCPUUsage               AlertMetric = "cpu.usage"
	AlertMetricCPUCore                AlertMetric = "cpu.core"
	AlertMetricCPUTemperature         AlertMetric = "cpu.temperature"
	AlertMetricCPUPower               AlertMetric = "cpu.power"
	AlertMetricCPUIOWait              AlertMetric = "cpu.iowait"
	AlertMetricCPUPressure            AlertMetric = "cpu.pressure"
	AlertMetricMemoryUsed             AlertMetric = "memory.used"
	AlertMetricMemoryTotal            AlertMetric = "memory.total"
	AlertMetricMemoryUsage            AlertMetric = "memory.usage"
	AlertMetricMemoryPressureSome     AlertMetric = "memory.pressure.some"
	AlertMetricMemoryPressureFull     AlertMetric = "memory.pressure.full"
	AlertMetricSwapUsed               AlertMetric = "swap.used"
	AlertMetricSwapTotal              AlertMetric = "swap.total"
	AlertMetricGPUUtilization         AlertMetric = "gpu.utilization"
	AlertMetricGPUMemoryUsed          AlertMetric = "gpu.memory.used"
	AlertMetricGPUMemoryTotal         AlertMetric = "gpu.memory.total"
	AlertMetricGPUTemperature         AlertMetric = "gpu.temperature"
	AlertMetricGPUJunctionTemperature AlertMetric = "gpu.junction.temperature"
	AlertMetricGPUVRAMTemperature     AlertMetric = "gpu.vram.temperature"
	AlertMetricGPUPower               AlertMetric = "gpu.power"
	AlertMetricFSUsed                 AlertMetric = "fs.used"
	AlertMetricFSTotal                AlertMetric = "fs.total"
	AlertMetricFSUsage                AlertMetric = "fs.usage"
	AlertMetricFSInodesUsage          AlertMetric = "fs.inodes.usage"
	AlertMetricIOPressureSome         AlertMetric = "io.pressure.some"
	AlertMetricIOPressureFull         AlertMetric = "io.pressure.full"
	AlertMetricDiskRead               AlertMetric = "disk.read"
	AlertMetricDiskWrite              AlertMetric = "disk.write"
	AlertMetricDiskReadLatency        AlertMetric = "disk.read.latency"
	AlertMetricDiskWriteLatency       AlertMetric = "disk.write.latency"
	AlertMetricDiskUtilization        AlertMetric = "disk.utilization"
	AlertMetricNetRx                  AlertMetric = "net.rx"
	AlertMetricNetTx                  AlertMetric = "net.tx"
)

func (m AlertMetric) validate() error {
	if !KnownMetric(string(m)) {
		return fmt.Errorf("unknown alert metric %q", m)
	}
	return nil
}

// AlertCondition is the comparison a rule applies.
type AlertCondition string

// Alert conditions.
const (
	AlertConditionAbove       AlertCondition = "above"
	AlertConditionBelow       AlertCondition = "below"
	AlertConditionStale       AlertCondition = "stale"
	AlertConditionUnavailable AlertCondition = "unavailable"
)

func (c AlertCondition) validate() error {
	switch c {
	case AlertConditionAbove, AlertConditionBelow, AlertConditionStale, AlertConditionUnavailable:
		return nil
	default:
		return fmt.Errorf("unknown alert condition %q", c)
	}
}

// AlertRuleState is the rule's current evaluation state.
type AlertRuleState string

// Alert rule states.
const (
	AlertRuleStateOK      AlertRuleState = "ok"
	AlertRuleStatePending AlertRuleState = "pending"
	AlertRuleStateFiring  AlertRuleState = "firing"
)

// AlertEventKind is the transition a notification reports.
type AlertEventKind string

// Alert event kinds.
const (
	AlertEventFired     AlertEventKind = "fired"
	AlertEventRecovered AlertEventKind = "recovered"
)

// AlertJobStatus is one webhook delivery job's state.
type AlertJobStatus string

// Alert job states.
const (
	AlertJobPending   AlertJobStatus = "pending"
	AlertJobDelivered AlertJobStatus = "delivered"
	AlertJobFailed    AlertJobStatus = "failed"
)

// AlertWebhookTransport is the only transport a configured webhook may use.
type AlertWebhookTransport string

// Alert webhook transports.
const (
	AlertWebhookTransportHTTPS        AlertWebhookTransport = "https"
	AlertWebhookTransportLoopbackHTTP AlertWebhookTransport = "loopback-http"
)

// DeliveryAtLeastOnce is the fixed delivery guarantee string.
type DeliveryAtLeastOnce string

// DeliveryAtLeastOnceValue is the only valid DeliveryAtLeastOnce value.
const DeliveryAtLeastOnceValue DeliveryAtLeastOnce = "at-least-once"

// AlertIncidentStatus is one incident's lifecycle state.
type AlertIncidentStatus string

// Incident states.
const (
	AlertIncidentOpen      AlertIncidentStatus = "open"
	AlertIncidentRecovered AlertIncidentStatus = "recovered"
)

// AlertWebhookAction is the discriminated request action.
type AlertWebhookAction string

// Webhook actions.
const (
	AlertWebhookActionConfigure AlertWebhookAction = "configure"
	AlertWebhookActionClear     AlertWebhookAction = "clear"
	AlertWebhookActionTest      AlertWebhookAction = "test"
)

// WebhookChannel is the only notification channel.
type WebhookChannel string

// WebhookChannelValue is the only webhook channel value.
const WebhookChannelValue WebhookChannel = "webhook"

// AlertLimits mirrors ALERT_LIMITS from src/alerts/types.ts.
//
//api:response
type AlertLimits struct {
	MaxRules              int64   `json:"maxRules"`
	MaxIncidents          int64   `json:"maxIncidents"`
	MaxJobs               int64   `json:"maxJobs"`
	MaxRuleName           int64   `json:"maxRuleName"`
	MaxMachineID          int64   `json:"maxMachineId"`
	MaxResourceID         int64   `json:"maxResourceId"`
	MaxDurationMs         int64   `json:"maxDurationMs"`
	MaxThreshold          float64 `json:"maxThreshold"` //api:range=0..Number.MAX_VALUE
	MaxWebhookURL         int64   `json:"maxWebhookUrl"`
	MaxWebhookHeaders     int64   `json:"maxWebhookHeaders"`
	MaxWebhookHeaderKey   int64   `json:"maxWebhookHeaderKey"`
	MaxWebhookHeaderValue int64   `json:"maxWebhookHeaderValue"`
	MaxWebhookSecret      int64   `json:"maxWebhookSecret"`
	MaxListLimit          int64   `json:"maxListLimit"`
}

// AlertRule is one configured rule plus its live evaluation state.
//
//api:response
type AlertRule struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`      //api:length=1..128
	MachineID        string         `json:"machineId"` //api:length=1..128
	Metric           AlertMetric    `json:"metric"`
	ResourceID       string         `json:"resourceId"` //api:length=0..256
	Condition        AlertCondition `json:"condition"`
	Threshold        *float64       `json:"threshold"`
	DurationMs       int64          `json:"durationMs"` //api:range=0..86400000
	Enabled          bool           `json:"enabled"`
	MaintenanceUntil *string        `json:"maintenanceUntil"` //api:timestamp
	State            AlertRuleState `json:"state"`
	PendingSince     *string        `json:"pendingSince"` //api:timestamp
	ActiveIncidentID *string        `json:"activeIncidentId"`
	LastValue        *float64       `json:"lastValue"`
	LastObservedAt   *string        `json:"lastObservedAt"`
	CreatedAt        string         `json:"createdAt"`
	UpdatedAt        string         `json:"updatedAt"`
}

func (r AlertRule) validate() error {
	if err := validateID(r.ID, "rule id"); err != nil {
		return err
	}
	if err := validateText(r.Name, "name", 1, 128); err != nil {
		return err
	}
	if err := validateText(r.MachineID, "machineId", 1, MaxMachineIDQueryLength); err != nil {
		return err
	}
	if err := r.Metric.validate(); err != nil {
		return err
	}
	if err := validateText(r.ResourceID, "resourceId", 0, MaxResourceIDLength); err != nil {
		return err
	}
	if err := r.Condition.validate(); err != nil {
		return err
	}
	if err := validateFinitePtr(r.Threshold, "threshold"); err != nil {
		return err
	}
	if r.DurationMs < 0 || r.DurationMs > 86_400_000 {
		return fmt.Errorf("durationMs must be 0..86400000")
	}
	switch r.State {
	case AlertRuleStateOK, AlertRuleStatePending, AlertRuleStateFiring:
	default:
		return fmt.Errorf("unknown rule state %q", r.State)
	}
	if r.MaintenanceUntil != nil {
		if err := validateTimestamp(*r.MaintenanceUntil, "maintenanceUntil"); err != nil {
			return err
		}
	}
	if r.PendingSince != nil {
		if err := validateTimestamp(*r.PendingSince, "pendingSince"); err != nil {
			return err
		}
	}
	if r.LastObservedAt != nil {
		if err := validateTimestamp(*r.LastObservedAt, "lastObservedAt"); err != nil {
			return err
		}
	}
	if err := validateFinitePtr(r.LastValue, "lastValue"); err != nil {
		return err
	}
	if err := validateTimestamp(r.CreatedAt, "createdAt"); err != nil {
		return err
	}
	return validateTimestamp(r.UpdatedAt, "updatedAt")
}

// AlertIncident is one open or recovered incident.
//
//api:response
type AlertIncident struct {
	ID             string              `json:"id"`
	RuleID         string              `json:"ruleId"`
	RuleName       string              `json:"ruleName"`
	MachineID      string              `json:"machineId"`
	Metric         AlertMetric         `json:"metric"`
	ResourceID     string              `json:"resourceId"`
	Condition      AlertCondition      `json:"condition"`
	Status         AlertIncidentStatus `json:"status"`
	StartedAt      string              `json:"startedAt"`
	ResolvedAt     *string             `json:"resolvedAt"`
	StartedValue   *float64            `json:"startedValue"`
	ResolvedValue  *float64            `json:"resolvedValue"`
	LastObservedAt string              `json:"lastObservedAt"`
	Message        string              `json:"message"`
}

func (i AlertIncident) validate() error {
	if err := validateID(i.ID, "incident id"); err != nil {
		return err
	}
	if err := validateID(i.RuleID, "ruleId"); err != nil {
		return err
	}
	if i.Metric.validate() != nil || i.Condition.validate() != nil {
		return fmt.Errorf("incident carries an unknown metric or condition")
	}
	if i.Status != AlertIncidentOpen && i.Status != AlertIncidentRecovered {
		return fmt.Errorf("unknown incident status %q", i.Status)
	}
	if err := validateTimestamp(i.StartedAt, "startedAt"); err != nil {
		return err
	}
	if i.ResolvedAt != nil {
		if err := validateTimestamp(*i.ResolvedAt, "resolvedAt"); err != nil {
			return err
		}
	}
	if err := validateFinitePtr(i.StartedValue, "startedValue"); err != nil {
		return err
	}
	if err := validateFinitePtr(i.ResolvedValue, "resolvedValue"); err != nil {
		return err
	}
	return validateTimestamp(i.LastObservedAt, "lastObservedAt")
}

// AlertNotification is one webhook delivery job. id is the deterministic
// idempotency key sent to the receiver.
//
//api:response
type AlertNotification struct {
	ID            string         `json:"id"`
	RuleID        string         `json:"ruleId"`
	IncidentID    string         `json:"incidentId"`
	Event         AlertEventKind `json:"event"`
	Status        AlertJobStatus `json:"status"`
	Attempts      int64          `json:"attempts"`    //api:range=0..Number.MAX_SAFE_INTEGER
	MaxAttempts   int64          `json:"maxAttempts"` //api:range=0..Number.MAX_SAFE_INTEGER
	NextAttemptAt *string        `json:"nextAttemptAt"`
	LastAttemptAt *string        `json:"lastAttemptAt"`
	LastError     *string        `json:"lastError"`
	CreatedAt     string         `json:"createdAt"`
	DeliveredAt   *string        `json:"deliveredAt"`
}

func (n AlertNotification) validate() error {
	if err := validateID(n.ID, "notification id"); err != nil {
		return err
	}
	switch n.Event {
	case AlertEventFired, AlertEventRecovered:
	default:
		return fmt.Errorf("unknown alert event %q", n.Event)
	}
	switch n.Status {
	case AlertJobPending, AlertJobDelivered, AlertJobFailed:
	default:
		return fmt.Errorf("unknown alert job status %q", n.Status)
	}
	for name, value := range map[string]*string{
		"nextAttemptAt": n.NextAttemptAt,
		"lastAttemptAt": n.LastAttemptAt,
		"deliveredAt":   n.DeliveredAt,
	} {
		if value != nil {
			if err := validateTimestamp(*value, name); err != nil {
				return err
			}
		}
	}
	if err := validateText(derefOr(n.LastError, ""), "lastError", 0, 1024); err != nil {
		return err
	}
	return validateTimestamp(n.CreatedAt, "createdAt")
}

func derefOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

// AlertNotificationSummary is the bounded notification queue summary.
//
//api:response
type AlertNotificationSummary struct {
	Pending        int64               `json:"pending"`   //api:range=0..Number.MAX_SAFE_INTEGER
	Delivered      int64               `json:"delivered"` //api:range=0..Number.MAX_SAFE_INTEGER
	Failed         int64               `json:"failed"`    //api:range=0..Number.MAX_SAFE_INTEGER
	Recent         []AlertNotification `json:"recent"`
	LastDeliveryAt *string             `json:"lastDeliveryAt"`
	LastError      *string             `json:"lastError"`
	Delivery       DeliveryAtLeastOnce `json:"delivery"`
}

func (s AlertNotificationSummary) validate() error {
	if s.Delivery != DeliveryAtLeastOnceValue {
		return fmt.Errorf("delivery must be %q", DeliveryAtLeastOnceValue)
	}
	if s.Recent == nil {
		return fmt.Errorf("recent must be an array")
	}
	if s.LastDeliveryAt != nil {
		if err := validateTimestamp(*s.LastDeliveryAt, "lastDeliveryAt"); err != nil {
			return err
		}
	}
	for _, event := range s.Recent {
		if err := event.validate(); err != nil {
			return err
		}
	}
	return nil
}

// AlertWebhookStatus is the webhook configuration state. It never includes the
// URL, headers or secret.
//
//api:response
type AlertWebhookStatus struct {
	Channel    WebhookChannel         `json:"channel"`
	Configured bool                   `json:"configured"`
	Enabled    bool                   `json:"enabled"`
	Transport  *AlertWebhookTransport `json:"transport"`
	LastTestAt *string                `json:"lastTestAt"`
	LastTestOK *bool                  `json:"lastTestOk"`
	LastError  *string                `json:"lastError"`
	Delivery   DeliveryAtLeastOnce    `json:"delivery"`
}

func (s AlertWebhookStatus) validate() error {
	if s.Channel != WebhookChannelValue {
		return fmt.Errorf("channel must be %q", WebhookChannelValue)
	}
	if s.Delivery != DeliveryAtLeastOnceValue {
		return fmt.Errorf("delivery must be %q", DeliveryAtLeastOnceValue)
	}
	if s.Transport != nil {
		switch *s.Transport {
		case AlertWebhookTransportHTTPS, AlertWebhookTransportLoopbackHTTP:
		default:
			return fmt.Errorf("unknown webhook transport %q", *s.Transport)
		}
	}
	if s.LastTestAt != nil {
		if err := validateTimestamp(*s.LastTestAt, "lastTestAt"); err != nil {
			return err
		}
	}
	return nil
}

// AlertListResponse is GET /api/alerts.
//
//api:response
type AlertListResponse struct {
	GeneratedAt        string                   `json:"generatedAt"`
	CanAdmin           bool                     `json:"canAdmin"`
	Rules              []AlertRule              `json:"rules"`
	Incidents          []AlertIncident          `json:"incidents"`
	Notifications      AlertNotificationSummary `json:"notifications"`
	Webhook            AlertWebhookStatus       `json:"webhook"`
	Limits             AlertLimits              `json:"limits"`
	DeliveryDisclosure string                   `json:"deliveryDisclosure"`
}

func (r AlertListResponse) validate() error {
	if r.Rules == nil || r.Incidents == nil {
		return fmt.Errorf("rules and incidents must be arrays")
	}
	if err := validateTimestamp(r.GeneratedAt, "generatedAt"); err != nil {
		return err
	}
	for _, rule := range r.Rules {
		if err := rule.validate(); err != nil {
			return err
		}
	}
	for _, incident := range r.Incidents {
		if err := incident.validate(); err != nil {
			return err
		}
	}
	if err := r.Notifications.validate(); err != nil {
		return err
	}
	if err := r.Webhook.validate(); err != nil {
		return err
	}
	if r.Limits.MaxRules < 1 || r.Limits.MaxListLimit < 1 {
		return fmt.Errorf("alert limits must be positive")
	}
	if r.DeliveryDisclosure == "" {
		return fmt.Errorf("deliveryDisclosure must not be empty")
	}
	return nil
}

// AlertDiagnostics is GET /api/admin/diagnostics/alerts. It reports alert
// evaluation losses that can change rule semantics.
//
//api:response
type AlertDiagnostics struct {
	GeneratedAt             string   `json:"generatedAt"`
	DroppedObservations     uint64   `json:"droppedObservations"`
	ObservationsAbandoned   uint64   `json:"observationsAbandoned"`
	ContinuityResets        uint64   `json:"continuityResets"`
	ContinuityResetFailures uint64   `json:"continuityResetFailures"`
	UnresolvedBreaks        []string `json:"unresolvedBreaks"`
	QueueDepth              int64    `json:"queueDepth"`
	QueueCapacity           int64    `json:"queueCapacity"`
	LastTickAt              *string  `json:"lastTickAt"` //api:timestamp
}

func (d AlertDiagnostics) validate() error {
	if err := validateTimestamp(d.GeneratedAt, "generatedAt"); err != nil {
		return err
	}
	if d.UnresolvedBreaks == nil {
		return fmt.Errorf("unresolvedBreaks must be an array")
	}
	for _, machineID := range d.UnresolvedBreaks {
		if err := validateText(machineID, "unresolved break machine id", 1, MaxMachineIDQueryLength); err != nil {
			return err
		}
	}
	if d.QueueDepth < 0 || d.QueueCapacity < 0 || d.QueueDepth > d.QueueCapacity {
		return fmt.Errorf("alert queue depth must be between 0 and capacity")
	}
	if d.LastTickAt != nil {
		if err := validateTimestamp(*d.LastTickAt, "lastTickAt"); err != nil {
			return err
		}
	}
	return nil
}

// AlertRuleInput is the POST /api/alerts/rules body. id is present to update
// an existing rule and omitted to create one. Resource-scoped metrics must
// carry resourceId.
type AlertRuleInput struct {
	ID               *string                     `json:"id,omitempty"`
	Name             string                      `json:"name"`
	MachineID        string                      `json:"machineId"`
	Metric           AlertMetric                 `json:"metric"`
	ResourceID       *string                     `json:"resourceId,omitempty"`
	Condition        AlertCondition              `json:"condition"`
	Threshold        *protocol.Optional[float64] `json:"threshold,omitempty"`
	DurationMs       int64                       `json:"durationMs"`
	Enabled          *bool                       `json:"enabled,omitempty"`
	MaintenanceUntil *protocol.Optional[string]  `json:"maintenanceUntil,omitempty"`
}

// UnmarshalJSON enforces the reference body's exact key set: name, machineId,
// metric, condition and durationMs are required; id, resourceId, threshold,
// enabled and maintenanceUntil are optional.
func (r *AlertRuleInput) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data, "alert rule input",
		[]string{"name", "machineId", "metric", "condition", "durationMs"},
		[]string{"id", "resourceId", "threshold", "enabled", "maintenanceUntil"})
	if err != nil {
		return err
	}
	var out AlertRuleInput
	if out.Name, err = rawString(fields["name"], "name"); err != nil {
		return err
	}
	if out.MachineID, err = rawString(fields["machineId"], "machineId"); err != nil {
		return err
	}
	var metric string
	if metric, err = rawString(fields["metric"], "metric"); err != nil {
		return err
	}
	out.Metric = AlertMetric(metric)
	var condition string
	if condition, err = rawString(fields["condition"], "condition"); err != nil {
		return err
	}
	out.Condition = AlertCondition(condition)
	if out.DurationMs, err = rawInt64(fields["durationMs"], "durationMs"); err != nil {
		return err
	}
	if raw, ok := fields["id"]; ok {
		value, err := rawString(raw, "id")
		if err != nil {
			return err
		}
		out.ID = &value
	}
	if raw, ok := fields["resourceId"]; ok {
		value, err := rawString(raw, "resourceId")
		if err != nil {
			return err
		}
		out.ResourceID = &value
	}
	if raw, ok := fields["threshold"]; ok {
		var value protocol.Optional[float64]
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("threshold: %w", err)
		}
		out.Threshold = &value
	}
	if raw, ok := fields["enabled"]; ok {
		value, err := rawBool(raw, "enabled")
		if err != nil {
			return err
		}
		out.Enabled = &value
	}
	if raw, ok := fields["maintenanceUntil"]; ok {
		var value protocol.Optional[string]
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("maintenanceUntil: %w", err)
		}
		out.MaintenanceUntil = &value
	}
	*r = out
	return nil
}

func (r AlertRuleInput) validate() error {
	if r.ID != nil {
		if err := validateID(*r.ID, "rule id"); err != nil {
			return err
		}
	}
	if err := validateText(r.Name, "name", 1, 128); err != nil {
		return err
	}
	if err := validateText(r.MachineID, "machineId", 1, MaxMachineIDQueryLength); err != nil {
		return err
	}
	if err := r.Metric.validate(); err != nil {
		return err
	}
	resourceID := ""
	if r.ResourceID != nil {
		res := *r.ResourceID
		if err := validateText(res, "resourceId", 0, MaxResourceIDLength); err != nil {
			return err
		}
		resourceID = res
	}
	if alertMetricRequiresResource(r.Metric) && resourceID == "" {
		return fmt.Errorf("this metric requires a resource id")
	}
	if !alertMetricRequiresResource(r.Metric) && resourceID != "" {
		return fmt.Errorf("this metric does not accept a resource id")
	}
	if err := r.Condition.validate(); err != nil {
		return err
	}
	switch r.Condition {
	case AlertConditionAbove, AlertConditionBelow:
		if r.Threshold == nil || !r.Threshold.Valid {
			return fmt.Errorf("threshold is required and must be a finite bounded number")
		}
		if err := validateFinitePtr(&r.Threshold.Value, "threshold"); err != nil {
			return err
		}
		if math.Abs(r.Threshold.Value) > 1e13 {
			return fmt.Errorf("threshold exceeds the supported magnitude")
		}
	default:
		if r.Threshold != nil && r.Threshold.Valid {
			return fmt.Errorf("this condition does not accept a threshold")
		}
	}
	if r.DurationMs < 0 || r.DurationMs > 86_400_000 {
		return fmt.Errorf("durationMs must be 0..86400000")
	}
	if r.MaintenanceUntil != nil && r.MaintenanceUntil.Valid {
		if err := validateTimestamp(r.MaintenanceUntil.Value, "maintenanceUntil"); err != nil {
			return err
		}
	}
	return nil
}

// alertResourceFree lists alert metrics that describe the whole machine and
// must not carry a resource id. Every other alert metric requires one.
var alertResourceFree = map[AlertMetric]struct{}{
	AlertMetricCPUUsage:           {},
	AlertMetricCPUTemperature:     {},
	AlertMetricCPUPower:           {},
	AlertMetricCPUIOWait:          {},
	AlertMetricCPUPressure:        {},
	AlertMetricMemoryUsed:         {},
	AlertMetricMemoryTotal:        {},
	AlertMetricMemoryUsage:        {},
	AlertMetricMemoryPressureSome: {},
	AlertMetricMemoryPressureFull: {},
	AlertMetricSwapUsed:           {},
	AlertMetricSwapTotal:          {},
	AlertMetricIOPressureSome:     {},
	AlertMetricIOPressureFull:     {},
}

func alertMetricRequiresResource(metric AlertMetric) bool {
	_, free := alertResourceFree[metric]
	return !free
}

// AlertRuleResponse is the POST /api/alerts/rules response (200 update,
// 201 create).
//
//api:response
type AlertRuleResponse struct {
	Rule AlertRule `json:"rule"`
}

func (r AlertRuleResponse) validate() error { return r.Rule.validate() }

// AlertWebhookRequest is the POST /api/alerts/webhook body. action selects the
// variant: "configure" requires url and accepts headers/secret/enabled;
// "clear" and "test" accept no other fields.
type AlertWebhookRequest struct {
	Action  AlertWebhookAction         `json:"action"`
	URL     *string                    `json:"url,omitempty"`
	Headers *map[string]string         `json:"headers,omitempty"`
	Secret  *protocol.Optional[string] `json:"secret,omitempty"`
	Enabled *bool                      `json:"enabled,omitempty"`
}

// UnmarshalJSON requires action and tracks optional key presence, because
// "clear"/"test" must carry no other field while "configure" accepts a URL,
// headers, a secret and an enabled flag.
func (r *AlertWebhookRequest) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data, "alert webhook request", []string{"action"},
		[]string{"url", "headers", "secret", "enabled"})
	if err != nil {
		return err
	}
	var out AlertWebhookRequest
	var action string
	if action, err = rawString(fields["action"], "action"); err != nil {
		return err
	}
	out.Action = AlertWebhookAction(action)
	if raw, ok := fields["url"]; ok {
		value, err := rawString(raw, "url")
		if err != nil {
			return err
		}
		out.URL = &value
	}
	if raw, ok := fields["headers"]; ok {
		if isNull(raw) {
			return fmt.Errorf("headers must be an object, not null")
		}
		headers, err := decodeWebhookHeaders(raw)
		if err != nil {
			return err
		}
		out.Headers = &headers
	}
	if raw, ok := fields["secret"]; ok {
		var value protocol.Optional[string]
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("secret: %w", err)
		}
		out.Secret = &value
	}
	if raw, ok := fields["enabled"]; ok {
		value, err := rawBool(raw, "enabled")
		if err != nil {
			return err
		}
		out.Enabled = &value
	}
	*r = out
	return nil
}

var webhookHeaderName = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")

var webhookReservedHeaders = map[string]struct{}{
	"host": {}, "content-length": {}, "content-type": {}, "idempotency-key": {},
}

// decodeWebhookHeaders inspects the raw JSON values (a null is not a string),
// trims header names the way the Node parser does, and rejects a collision
// after trimming so output ordering is deterministic.
func decodeWebhookHeaders(data json.RawMessage) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("headers: %w", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("headers must be an object, not null")
	}
	if len(raw) > 16 {
		return nil, fmt.Errorf("headers exceeds 16 entries")
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		trimmed := strings.TrimSpace(key)
		if isNull(value) {
			return nil, fmt.Errorf("header %q value must be a string, not null", trimmed)
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return nil, fmt.Errorf("header %q value: %w", trimmed, err)
		}
		if _, duplicate := out[trimmed]; duplicate {
			return nil, fmt.Errorf("duplicate header name %q", trimmed)
		}
		out[trimmed] = text
	}
	return out, nil
}

func (r AlertWebhookRequest) validate() error {
	switch r.Action {
	case AlertWebhookActionConfigure:
		if r.URL == nil {
			return fmt.Errorf("configure requires url")
		}
		if err := validateText(*r.URL, "url", 1, 2048); err != nil {
			return err
		}
		if r.Headers != nil {
			if len(*r.Headers) > 16 {
				return fmt.Errorf("headers exceeds 16 entries")
			}
			for key, value := range *r.Headers {
				if err := validateText(key, "header name", 1, 128); err != nil {
					return err
				}
				if !webhookHeaderName.MatchString(key) {
					return fmt.Errorf("header name %q is not an HTTP token", key)
				}
				if _, reserved := webhookReservedHeaders[strings.ToLower(key)]; reserved {
					return fmt.Errorf("header name %q is reserved", key)
				}
				if err := validateText(value, "header value", 0, 1024); err != nil {
					return err
				}
			}
		}
		if r.Secret != nil && r.Secret.Valid {
			if err := validateText(r.Secret.Value, "secret", 0, 256); err != nil {
				return err
			}
		}
	case AlertWebhookActionClear, AlertWebhookActionTest:
		if r.URL != nil || r.Headers != nil || r.Secret != nil || r.Enabled != nil {
			return fmt.Errorf("%s accepts only action", r.Action)
		}
	default:
		return fmt.Errorf("unknown webhook action %q", r.Action)
	}
	return nil
}

// AlertWebhookResponse is the configure/clear response.
//
//api:response
type AlertWebhookResponse struct {
	Status AlertWebhookStatus `json:"status"`
}

func (r AlertWebhookResponse) validate() error { return r.Status.validate() }

// AlertWebhookTestResult is one synchronous webhook test attempt.
//
//api:response
type AlertWebhookTestResult struct {
	OK       bool    `json:"ok"`
	Status   *int64  `json:"status"` //api:range=100..599
	Error    *string `json:"error"`
	TestedAt string  `json:"testedAt"`
}

func (r AlertWebhookTestResult) validate() error {
	return validateTimestamp(r.TestedAt, "testedAt")
}

// AlertWebhookTestResponse is the test action's response.
//
//api:response
type AlertWebhookTestResponse struct {
	Result AlertWebhookTestResult `json:"result"`
	Status AlertWebhookStatus     `json:"status"`
}

func (r AlertWebhookTestResponse) validate() error {
	if err := r.Result.validate(); err != nil {
		return err
	}
	return r.Status.validate()
}
