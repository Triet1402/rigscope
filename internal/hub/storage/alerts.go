package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"time"
)

const (
	// MaxAlertNameBytes bounds a rule name.
	MaxAlertNameBytes = 200
	// MaxAlertMessageBytes bounds an incident message.
	MaxAlertMessageBytes = 1000
	// MaxIncidentPage is the largest incident page List returns.
	MaxIncidentPage = 1000
)

// Alert rule conditions.
const (
	ConditionAbove       = "above"
	ConditionBelow       = "below"
	ConditionUnavailable = "unavailable"
)

// Alert rule and incident states.
const (
	AlertStateOK      = "ok"
	AlertStatePending = "pending"
	AlertStateFiring  = "firing"
	IncidentOpen      = "open"
	IncidentRecovered = "recovered"
)

// Rule is one durable alert rule plus the engine's continuation state. Storage
// validates and persists it; evaluation belongs to the alerts package.
type Rule struct {
	ID               string
	Name             string
	MachineID        string
	Metric           string
	ResourceID       string
	Condition        string
	Threshold        *float64
	DurationMs       int64
	Enabled          bool
	MaintenanceUntil *time.Time
	State            string
	PendingSince     *time.Time
	ActiveIncidentID *string
	LastValue        *float64
	LastObservedAt   *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Rules is the alert-rule repository.
type Rules struct {
	db  *Database
	now func() time.Time
}

// NewRules returns the rule repository over db.
func NewRules(db *Database) *Rules { return &Rules{db: db, now: db.Now} }

const ruleColumns = "id, name, machine_id, metric, resource_id, condition, threshold, duration_ms, enabled, maintenance_until_ms, state, pending_since_ms, active_incident_id, last_value, last_observed_at_ms, created_at_ms, updated_at_ms"

type alertRowScanner interface {
	Scan(dest ...any) error
}

func scanRule(row alertRowScanner) (Rule, error) {
	var (
		rule                           Rule
		threshold                      sql.NullFloat64
		enabled                        int64
		maintenanceUntil, pendingSince sql.NullInt64
		activeIncident                 sql.NullString
		lastValue                      sql.NullFloat64
		lastObserved                   sql.NullInt64
		createdAt, updatedAt           int64
	)
	if err := row.Scan(
		&rule.ID, &rule.Name, &rule.MachineID, &rule.Metric, &rule.ResourceID, &rule.Condition,
		&threshold, &rule.DurationMs, &enabled, &maintenanceUntil, &rule.State, &pendingSince,
		&activeIncident, &lastValue, &lastObserved, &createdAt, &updatedAt,
	); err != nil {
		return Rule{}, err
	}
	if threshold.Valid {
		value := threshold.Float64
		rule.Threshold = &value
	}
	rule.Enabled = intToBool(enabled)
	if maintenanceUntil.Valid {
		value := msToTime(maintenanceUntil.Int64)
		rule.MaintenanceUntil = &value
	}
	if pendingSince.Valid {
		value := msToTime(pendingSince.Int64)
		rule.PendingSince = &value
	}
	if activeIncident.Valid {
		value := activeIncident.String
		rule.ActiveIncidentID = &value
	}
	if lastValue.Valid {
		value := lastValue.Float64
		rule.LastValue = &value
	}
	if lastObserved.Valid {
		value := msToTime(lastObserved.Int64)
		rule.LastObservedAt = &value
	}
	rule.CreatedAt = msToTime(createdAt)
	rule.UpdatedAt = msToTime(updatedAt)
	return rule, nil
}

func validateRule(rule Rule) error {
	if rule.ID != "" && !identifierPattern.MatchString(rule.ID) {
		return invalidError("rule id must be printable ASCII of at most 128 characters")
	}
	if rule.Name == "" || len(rule.Name) > MaxAlertNameBytes || containsNUL(rule.Name) {
		return invalidError("rule name must be 1..%d bytes without NUL", MaxAlertNameBytes)
	}
	if !identifierPattern.MatchString(rule.MachineID) {
		return invalidError("rule machine id is invalid")
	}
	if _, ok := MetricUnitFor(rule.Metric); !ok {
		return invalidError("unknown alert metric %q", rule.Metric)
	}
	if len(rule.ResourceID) > 256 || containsNUL(rule.ResourceID) {
		return invalidError("rule resource id must be at most 256 bytes")
	}
	switch rule.Condition {
	case ConditionAbove, ConditionBelow:
		if rule.Threshold == nil {
			return invalidError("condition %q requires a threshold", rule.Condition)
		}
	case ConditionUnavailable:
		if rule.Threshold != nil {
			return invalidError("condition %q does not accept a threshold", rule.Condition)
		}
	default:
		return invalidError("unknown alert condition %q", rule.Condition)
	}
	if rule.DurationMs < 0 || rule.DurationMs > 30*24*60*60*1000 {
		return invalidError("rule duration must be between 0 and 30 days")
	}
	switch rule.State {
	case "", AlertStateOK, AlertStatePending, AlertStateFiring:
	default:
		return invalidError("unknown rule state %q", rule.State)
	}
	if rule.State == "" {
		rule.State = AlertStateOK
	}
	return nil
}

// Create stores a new rule, generating an ID when the caller leaves it empty.
func (s *Rules) Create(ctx context.Context, rule Rule) (Rule, error) {
	if err := validateRule(rule); err != nil {
		return Rule{}, err
	}
	if rule.ID == "" {
		id, err := newUUID()
		if err != nil {
			return Rule{}, wrapStorageError(CodeUnavailable, err, "generate rule id")
		}
		rule.ID = id
	}
	if rule.State == "" {
		rule.State = AlertStateOK
	}
	now := s.now()
	if rule.CreatedAt.IsZero() {
		rule.CreatedAt = now
	}
	rule.UpdatedAt = now
	err := s.db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(
			"INSERT INTO alert_rules ("+ruleColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			rule.ID, rule.Name, rule.MachineID, rule.Metric, rule.ResourceID, rule.Condition,
			nullableFloat(rule.Threshold), rule.DurationMs, boolToInt(rule.Enabled),
			nullableTime(rule.MaintenanceUntil), rule.State, nullableTime(rule.PendingSince),
			nullableString(rule.ActiveIncidentID), nullableFloat(rule.LastValue),
			nullableTime(rule.LastObservedAt), rule.CreatedAt.UnixMilli(), rule.UpdatedAt.UnixMilli(),
		)
		if err != nil {
			if CodeOf(err) == CodeConflict {
				return conflictError("alert rule %q already exists", rule.ID)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return Rule{}, err
	}
	return rule, nil
}

// Get returns one rule.
func (s *Rules) Get(ctx context.Context, id string) (Rule, error) {
	if !identifierPattern.MatchString(id) {
		return Rule{}, invalidError("rule id is invalid")
	}
	rule, err := scanRule(s.db.QueryRow(ctx, "SELECT "+ruleColumns+" FROM alert_rules WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, notFoundError("alert rule %q not found", id)
	}
	if err != nil {
		return Rule{}, mapError(err)
	}
	return rule, nil
}

// List returns rules ordered by name; an empty machineID lists every machine.
func (s *Rules) List(ctx context.Context, machineID string) ([]Rule, error) {
	var (
		rows *readRows
		err  error
	)
	if machineID == "" {
		rows, err = s.db.Query(ctx, "SELECT "+ruleColumns+" FROM alert_rules ORDER BY name, id")
	} else {
		if !identifierPattern.MatchString(machineID) {
			return nil, invalidError("machine id is invalid")
		}
		rows, err = s.db.Query(ctx, "SELECT "+ruleColumns+" FROM alert_rules WHERE machine_id = ? ORDER BY name, id", machineID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []Rule
	for rows.Next() {
		rule, err := scanRule(rows)
		if err != nil {
			return nil, mapError(err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return rules, nil
}

// Update replaces one rule's state and configuration, preserving its ID and
// creation time.
func (s *Rules) Update(ctx context.Context, rule Rule) error {
	if err := validateRule(rule); err != nil {
		return err
	}
	if rule.ID == "" {
		return invalidError("rule id is required for update")
	}
	now := s.now()
	return s.db.Write(ctx, func(tx *Tx) error {
		result, err := tx.Exec(
			"UPDATE alert_rules SET name = ?, machine_id = ?, metric = ?, resource_id = ?, condition = ?, threshold = ?, duration_ms = ?, enabled = ?, maintenance_until_ms = ?, state = ?, pending_since_ms = ?, active_incident_id = ?, last_value = ?, last_observed_at_ms = ?, updated_at_ms = ? WHERE id = ?",
			rule.Name, rule.MachineID, rule.Metric, rule.ResourceID, rule.Condition,
			nullableFloat(rule.Threshold), rule.DurationMs, boolToInt(rule.Enabled),
			nullableTime(rule.MaintenanceUntil), rule.State, nullableTime(rule.PendingSince),
			nullableString(rule.ActiveIncidentID), nullableFloat(rule.LastValue),
			nullableTime(rule.LastObservedAt), now.UnixMilli(), rule.ID,
		)
		if err != nil {
			return err
		}
		return ensureAffected(result, CodeNotFound, "alert rule %q not found", rule.ID)
	})
}

// Delete removes one rule. Its incident history stays.
func (s *Rules) Delete(ctx context.Context, id string) error {
	if !identifierPattern.MatchString(id) {
		return invalidError("rule id is invalid")
	}
	return s.db.Write(ctx, func(tx *Tx) error {
		result, err := tx.Exec("DELETE FROM alert_rules WHERE id = ?", id)
		if err != nil {
			return err
		}
		return ensureAffected(result, CodeNotFound, "alert rule %q not found", id)
	})
}

// Incident is one alert incident. The engine owns transitions; storage keeps
// the history and the one-open-incident-per-rule invariant.
type Incident struct {
	ID             string
	RuleID         string
	RuleName       string
	MachineID      string
	Metric         string
	ResourceID     string
	Condition      string
	Status         string
	StartedAt      time.Time
	ResolvedAt     *time.Time
	StartedValue   *float64
	ResolvedValue  *float64
	LastObservedAt time.Time
	Message        string
}

// Incidents is the alert-incident repository.
type Incidents struct {
	db  *Database
	now func() time.Time
}

// NewIncidents returns the incident repository over db.
func NewIncidents(db *Database) *Incidents { return &Incidents{db: db, now: db.Now} }

const incidentColumns = "id, rule_id, rule_name, machine_id, metric, resource_id, condition, status, started_at_ms, resolved_at_ms, started_value, resolved_value, last_observed_at_ms, message"

func scanIncident(row alertRowScanner) (Incident, error) {
	var (
		incident                    Incident
		resolvedAt                  sql.NullInt64
		startedValue, resolvedValue sql.NullFloat64
		startedAt, lastObserved     int64
	)
	if err := row.Scan(
		&incident.ID, &incident.RuleID, &incident.RuleName, &incident.MachineID, &incident.Metric,
		&incident.ResourceID, &incident.Condition, &incident.Status, &startedAt, &resolvedAt,
		&startedValue, &resolvedValue, &lastObserved, &incident.Message,
	); err != nil {
		return Incident{}, err
	}
	incident.StartedAt = msToTime(startedAt)
	if resolvedAt.Valid {
		value := msToTime(resolvedAt.Int64)
		incident.ResolvedAt = &value
	}
	if startedValue.Valid {
		value := startedValue.Float64
		incident.StartedValue = &value
	}
	if resolvedValue.Valid {
		value := resolvedValue.Float64
		incident.ResolvedValue = &value
	}
	incident.LastObservedAt = msToTime(lastObserved)
	return incident, nil
}

func validateIncident(incident Incident) error {
	if incident.ID != "" && !identifierPattern.MatchString(incident.ID) {
		return invalidError("incident id is invalid")
	}
	if !identifierPattern.MatchString(incident.RuleID) {
		return invalidError("incident rule id is invalid")
	}
	if incident.RuleName == "" || len(incident.RuleName) > MaxAlertNameBytes || containsNUL(incident.RuleName) {
		return invalidError("incident rule name is invalid")
	}
	if !identifierPattern.MatchString(incident.MachineID) {
		return invalidError("incident machine id is invalid")
	}
	if _, ok := MetricUnitFor(incident.Metric); !ok {
		return invalidError("unknown incident metric %q", incident.Metric)
	}
	if incident.Condition == "" || len(incident.Condition) > 64 {
		return invalidError("incident condition is invalid")
	}
	switch incident.Status {
	case IncidentOpen, IncidentRecovered:
	default:
		return invalidError("unknown incident status %q", incident.Status)
	}
	if incident.Message == "" || len(incident.Message) > MaxAlertMessageBytes || containsNUL(incident.Message) {
		return invalidError("incident message must be 1..%d bytes without NUL", MaxAlertMessageBytes)
	}
	if incident.StartedAt.IsZero() {
		return invalidError("incident start time is required")
	}
	if incident.Status == IncidentRecovered && incident.ResolvedAt == nil {
		return invalidError("a recovered incident requires a resolution time")
	}
	return nil
}

// Create stores one incident. The unique partial index rejects a second open
// incident for the same rule as a conflict.
func (s *Incidents) Create(ctx context.Context, incident Incident) (Incident, error) {
	if err := validateIncident(incident); err != nil {
		return Incident{}, err
	}
	if incident.ID == "" {
		id, err := newUUID()
		if err != nil {
			return Incident{}, wrapStorageError(CodeUnavailable, err, "generate incident id")
		}
		incident.ID = id
	}
	if incident.LastObservedAt.IsZero() {
		incident.LastObservedAt = incident.StartedAt
	}
	err := s.db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(
			"INSERT INTO alert_incidents ("+incidentColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			incident.ID, incident.RuleID, incident.RuleName, incident.MachineID, incident.Metric,
			incident.ResourceID, incident.Condition, incident.Status, incident.StartedAt.UnixMilli(),
			nullableTime(incident.ResolvedAt), nullableFloat(incident.StartedValue),
			nullableFloat(incident.ResolvedValue), incident.LastObservedAt.UnixMilli(), incident.Message,
		)
		if err != nil {
			if CodeOf(err) == CodeConflict {
				return conflictError("an open incident already exists for rule %q", incident.RuleID)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return Incident{}, err
	}
	return incident, nil
}

// Get returns one incident.
func (s *Incidents) Get(ctx context.Context, id string) (Incident, error) {
	if !identifierPattern.MatchString(id) {
		return Incident{}, invalidError("incident id is invalid")
	}
	incident, err := scanIncident(s.db.QueryRow(ctx, "SELECT "+incidentColumns+" FROM alert_incidents WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Incident{}, notFoundError("incident %q not found", id)
	}
	if err != nil {
		return Incident{}, mapError(err)
	}
	return incident, nil
}

// IncidentFilter narrows List.
type IncidentFilter struct {
	MachineID string
	RuleID    string
	Status    string
	Limit     int
}

// List returns incidents newest-first. An empty filter lists every machine.
func (s *Incidents) List(ctx context.Context, filter IncidentFilter) ([]Incident, error) {
	limit := filter.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > MaxIncidentPage {
		return nil, invalidError("incident limit must be between 1 and %d", MaxIncidentPage)
	}
	query := "SELECT " + incidentColumns + " FROM alert_incidents WHERE 1 = 1"
	args := make([]any, 0, 4)
	if filter.MachineID != "" {
		if !identifierPattern.MatchString(filter.MachineID) {
			return nil, invalidError("machine id is invalid")
		}
		query += " AND machine_id = ?"
		args = append(args, filter.MachineID)
	}
	if filter.RuleID != "" {
		if !identifierPattern.MatchString(filter.RuleID) {
			return nil, invalidError("rule id is invalid")
		}
		query += " AND rule_id = ?"
		args = append(args, filter.RuleID)
	}
	if filter.Status != "" {
		if filter.Status != IncidentOpen && filter.Status != IncidentRecovered {
			return nil, invalidError("incident status is invalid")
		}
		query += " AND status = ?"
		args = append(args, filter.Status)
	}
	query += " ORDER BY started_at_ms DESC, id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var incidents []Incident
	for rows.Next() {
		incident, err := scanIncident(rows)
		if err != nil {
			return nil, mapError(err)
		}
		incidents = append(incidents, incident)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return incidents, nil
}

// Update replaces one incident's mutable state.
func (s *Incidents) Update(ctx context.Context, incident Incident) error {
	if err := validateIncident(incident); err != nil {
		return err
	}
	if incident.ID == "" {
		return invalidError("incident id is required for update")
	}
	return s.db.Write(ctx, func(tx *Tx) error {
		result, err := tx.Exec(
			"UPDATE alert_incidents SET status = ?, resolved_at_ms = ?, resolved_value = ?, last_observed_at_ms = ?, message = ? WHERE id = ?",
			incident.Status, nullableTime(incident.ResolvedAt), nullableFloat(incident.ResolvedValue),
			incident.LastObservedAt.UnixMilli(), incident.Message, incident.ID,
		)
		if err != nil {
			if CodeOf(err) == CodeConflict {
				return conflictError("an open incident already exists for rule %q", incident.RuleID)
			}
			return err
		}
		return ensureAffected(result, CodeNotFound, "incident %q not found", incident.ID)
	})
}

// Delete removes one incident row.
func (s *Incidents) Delete(ctx context.Context, id string) error {
	if !identifierPattern.MatchString(id) {
		return invalidError("incident id is invalid")
	}
	return s.db.Write(ctx, func(tx *Tx) error {
		result, err := tx.Exec("DELETE FROM alert_incidents WHERE id = ?", id)
		if err != nil {
			return err
		}
		return ensureAffected(result, CodeNotFound, "incident %q not found", id)
	})
}

// Webhook is the single alert delivery channel. Headers and the optional HMAC
// secret are private database state; the browser API must not return them.
type Webhook struct {
	URL        string
	Headers    map[string]string
	Secret     *string
	Enabled    bool
	UpdatedAt  time.Time
	LastTestAt *time.Time
	LastTestOK *bool
	LastError  *string
}

// Webhooks is the single-row webhook settings repository.
type Webhooks struct {
	db  *Database
	now func() time.Time
}

// NewWebhooks returns the webhook repository over db.
func NewWebhooks(db *Database) *Webhooks { return &Webhooks{db: db, now: db.Now} }

// Get returns the configured webhook, or nil when none is configured.
func (s *Webhooks) Get(ctx context.Context) (*Webhook, error) {
	var (
		webhook     Webhook
		headersJSON string
		secret      sql.NullString
		enabled     int64
		updatedAt   int64
		lastTestAt  sql.NullInt64
		lastTestOK  sql.NullInt64
		lastError   sql.NullString
	)
	err := s.db.QueryRow(ctx,
		"SELECT url, headers_json, secret, enabled, updated_at_ms, last_test_at_ms, last_test_ok, last_error FROM alert_webhook WHERE id = 1",
	).Scan(&webhook.URL, &headersJSON, &secret, &enabled, &updatedAt, &lastTestAt, &lastTestOK, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapError(err)
	}
	webhook.Headers = map[string]string{}
	if err := json.Unmarshal([]byte(headersJSON), &webhook.Headers); err != nil {
		return nil, wrapStorageError(CodeUnavailable, err, "parse stored webhook headers")
	}
	if secret.Valid {
		value := secret.String
		webhook.Secret = &value
	}
	webhook.Enabled = intToBool(enabled)
	webhook.UpdatedAt = msToTime(updatedAt)
	if lastTestAt.Valid {
		value := msToTime(lastTestAt.Int64)
		webhook.LastTestAt = &value
	}
	if lastTestOK.Valid {
		value := intToBool(lastTestOK.Int64)
		webhook.LastTestOK = &value
	}
	if lastError.Valid {
		value := lastError.String
		webhook.LastError = &value
	}
	return &webhook, nil
}

// Set creates or replaces the single webhook channel.
func (s *Webhooks) Set(ctx context.Context, webhook Webhook) error {
	if webhook.URL == "" || len(webhook.URL) > 2048 {
		return invalidError("webhook URL must be 1..2048 bytes")
	}
	parsed, err := url.Parse(webhook.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return invalidError("webhook URL must be an absolute http(s) URL")
	}
	if len(webhook.Headers) > 32 {
		return invalidError("webhook headers are limited to 32 entries")
	}
	headers := make(map[string]string, len(webhook.Headers))
	for name, value := range webhook.Headers {
		if name == "" || len(name) > 128 || len(value) > 1024 || containsNUL(name) || containsNUL(value) {
			return invalidError("webhook header name or value is invalid")
		}
		headers[name] = value
	}
	headersJSON, err := json.Marshal(headers)
	if err != nil {
		return wrapStorageError(CodeInvalid, err, "encode webhook headers")
	}
	if webhook.Secret != nil && (len(*webhook.Secret) == 0 || len(*webhook.Secret) > 1024 || containsNUL(*webhook.Secret)) {
		return invalidError("webhook secret must be 1..1024 bytes without NUL")
	}
	if webhook.LastError != nil && len(*webhook.LastError) > 512 {
		return invalidError("webhook error text is too long")
	}
	now := s.now()
	if webhook.UpdatedAt.IsZero() {
		webhook.UpdatedAt = now
	}
	return s.db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(
			"INSERT INTO alert_webhook (id, url, headers_json, secret, enabled, updated_at_ms, last_test_at_ms, last_test_ok, last_error) "+
				"VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?) "+
				"ON CONFLICT(id) DO UPDATE SET url = excluded.url, headers_json = excluded.headers_json, "+
				"secret = excluded.secret, enabled = excluded.enabled, updated_at_ms = excluded.updated_at_ms, "+
				"last_test_at_ms = excluded.last_test_at_ms, last_test_ok = excluded.last_test_ok, last_error = excluded.last_error",
			webhook.URL, string(headersJSON), nullableString(webhook.Secret), boolToInt(webhook.Enabled),
			webhook.UpdatedAt.UnixMilli(), nullableTime(webhook.LastTestAt),
			nullableBool(webhook.LastTestOK), nullableString(webhook.LastError),
		)
		return err
	})
}

// Delete removes the webhook channel; deleting an absent channel is not an
// error.
func (s *Webhooks) Delete(ctx context.Context) error {
	return s.db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec("DELETE FROM alert_webhook WHERE id = 1")
		return err
	})
}

func nullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableTime(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return value.UnixMilli()
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableBool(value *bool) any {
	if value == nil {
		return nil
	}
	return boolToInt(*value)
}
