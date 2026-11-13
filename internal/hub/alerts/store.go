package alerts

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

// This file owns the SQL over the alert tables. One observation must commit its
// progress row, rule transitions, incident and delivery job atomically, which
// the per-call storage repositories cannot express. Reads for browser
// responses also go through storage.Database's bounded reader pool.

const (
	jobPending   = "pending"
	jobDelivered = "delivered"
	jobFailed    = "failed"
)

const alertRuleColumns = "id, name, machine_id, metric, resource_id, condition, threshold, duration_ms, enabled, maintenance_until_ms, state, pending_since_ms, active_incident_id, last_value, last_observed_at_ms, created_at_ms, updated_at_ms"

const alertIncidentColumns = "id, rule_id, rule_name, machine_id, metric, resource_id, condition, status, started_at_ms, resolved_at_ms, started_value, resolved_value, last_observed_at_ms, message"

const alertJobColumns = "id, rule_id, incident_id, machine_id, event, status, attempts, max_attempts, next_attempt_ms, last_attempt_ms, last_error, created_at_ms, delivered_at_ms, payload_json"

type rowScanner interface {
	Scan(dest ...any) error
}

type ruleRow struct {
	id               string
	name             string
	machineID        string
	metric           string
	resourceID       string
	condition        string
	threshold        *float64
	durationMs       int64
	enabled          bool
	maintenanceUntil *time.Time
	state            string
	pendingSince     *time.Time
	activeIncidentID *string
	lastValue        *float64
	lastObservedAt   *time.Time
	createdAt        time.Time
	updatedAt        time.Time
}

func scanRule(row rowScanner) (ruleRow, error) {
	var (
		out                                          ruleRow
		threshold                                    sql.NullFloat64
		enabled                                      int64
		maintenanceUntil, pendingSince, lastObserved sql.NullInt64
		activeIncident                               sql.NullString
		lastValue                                    sql.NullFloat64
		createdAt, updatedAt                         int64
	)
	if err := row.Scan(
		&out.id, &out.name, &out.machineID, &out.metric, &out.resourceID, &out.condition,
		&threshold, &out.durationMs, &enabled, &maintenanceUntil, &out.state, &pendingSince,
		&activeIncident, &lastValue, &lastObserved, &createdAt, &updatedAt,
	); err != nil {
		return ruleRow{}, err
	}
	if threshold.Valid {
		value := threshold.Float64
		out.threshold = &value
	}
	out.enabled = enabled == 1
	out.maintenanceUntil = nullTime(maintenanceUntil)
	out.pendingSince = nullTime(pendingSince)
	if activeIncident.Valid {
		value := activeIncident.String
		out.activeIncidentID = &value
	}
	if lastValue.Valid {
		value := lastValue.Float64
		out.lastValue = &value
	}
	out.lastObservedAt = nullTime(lastObserved)
	out.createdAt = msTime(createdAt)
	out.updatedAt = msTime(updatedAt)
	return out, nil
}

func nullTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	parsed := msTime(value.Int64)
	return &parsed
}

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func (r ruleRow) api() api.AlertRule {
	metric := api.AlertMetric(r.metric)
	if _, ok := metricSpecFor(metric); !ok {
		metric = api.AlertMetricCPUUsage
	}
	condition := api.AlertCondition(r.condition)
	switch condition {
	case api.AlertConditionAbove, api.AlertConditionBelow, api.AlertConditionStale, api.AlertConditionUnavailable:
	default:
		condition = api.AlertConditionAbove
	}
	state := api.AlertRuleState(r.state)
	switch state {
	case api.AlertRuleStateOK, api.AlertRuleStatePending, api.AlertRuleStateFiring:
	default:
		state = api.AlertRuleStateOK
	}
	return api.AlertRule{
		ID:               r.id,
		Name:             r.name,
		MachineID:        r.machineID,
		Metric:           metric,
		ResourceID:       r.resourceID,
		Condition:        condition,
		Threshold:        cloneFloatPtr(r.threshold),
		DurationMs:       r.durationMs,
		Enabled:          r.enabled,
		MaintenanceUntil: formatTimePtr(r.maintenanceUntil),
		State:            state,
		PendingSince:     formatTimePtr(r.pendingSince),
		ActiveIncidentID: cloneStringPtr(r.activeIncidentID),
		LastValue:        cloneFloatPtr(r.lastValue),
		LastObservedAt:   formatTimePtr(r.lastObservedAt),
		CreatedAt:        formatTime(r.createdAt),
		UpdatedAt:        formatTime(r.updatedAt),
	}
}

type incidentRow struct {
	id             string
	ruleID         string
	ruleName       string
	machineID      string
	metric         string
	resourceID     string
	condition      string
	status         string
	startedAt      time.Time
	resolvedAt     *time.Time
	startedValue   *float64
	resolvedValue  *float64
	lastObservedAt time.Time
	message        string
}

func scanIncident(row rowScanner) (incidentRow, error) {
	var (
		out                         incidentRow
		resolvedAt, lastObserved    sql.NullInt64
		startedValue, resolvedValue sql.NullFloat64
		startedAt                   int64
	)
	if err := row.Scan(
		&out.id, &out.ruleID, &out.ruleName, &out.machineID, &out.metric, &out.resourceID,
		&out.condition, &out.status, &startedAt, &resolvedAt, &startedValue, &resolvedValue,
		&lastObserved, &out.message,
	); err != nil {
		return incidentRow{}, err
	}
	out.startedAt = msTime(startedAt)
	out.resolvedAt = nullTime(resolvedAt)
	if startedValue.Valid {
		value := startedValue.Float64
		out.startedValue = &value
	}
	if resolvedValue.Valid {
		value := resolvedValue.Float64
		out.resolvedValue = &value
	}
	if lastObserved.Valid {
		out.lastObservedAt = msTime(lastObserved.Int64)
	}
	return out, nil
}

func (i incidentRow) api() api.AlertIncident {
	metric := api.AlertMetric(i.metric)
	if _, ok := metricSpecFor(metric); !ok {
		metric = api.AlertMetricCPUUsage
	}
	status := api.AlertIncidentOpen
	if i.status == storage.IncidentRecovered {
		status = api.AlertIncidentRecovered
	}
	return api.AlertIncident{
		ID:             i.id,
		RuleID:         i.ruleID,
		RuleName:       i.ruleName,
		MachineID:      i.machineID,
		Metric:         metric,
		ResourceID:     i.resourceID,
		Condition:      api.AlertCondition(i.condition),
		Status:         status,
		StartedAt:      formatTime(i.startedAt),
		ResolvedAt:     formatTimePtr(i.resolvedAt),
		StartedValue:   cloneFloatPtr(i.startedValue),
		ResolvedValue:  cloneFloatPtr(i.resolvedValue),
		LastObservedAt: formatTime(i.lastObservedAt),
		Message:        i.message,
	}
}

type jobRow struct {
	id          string
	ruleID      string
	incidentID  string
	machineID   string
	event       string
	status      string
	attempts    int64
	maxAttempts int64
	nextAttempt *time.Time
	lastAttempt *time.Time
	lastError   *string
	createdAt   time.Time
	deliveredAt *time.Time
	payloadJSON string
}

func scanJob(row rowScanner) (jobRow, error) {
	var (
		out                                   jobRow
		nextAttempt, lastAttempt, deliveredAt sql.NullInt64
		lastError                             sql.NullString
		createdAt                             int64
	)
	if err := row.Scan(
		&out.id, &out.ruleID, &out.incidentID, &out.machineID, &out.event, &out.status,
		&out.attempts, &out.maxAttempts, &nextAttempt, &lastAttempt, &lastError, &createdAt,
		&deliveredAt, &out.payloadJSON,
	); err != nil {
		return jobRow{}, err
	}
	out.nextAttempt = nullTime(nextAttempt)
	out.lastAttempt = nullTime(lastAttempt)
	if lastError.Valid {
		value := lastError.String
		out.lastError = &value
	}
	out.createdAt = msTime(createdAt)
	out.deliveredAt = nullTime(deliveredAt)
	return out, nil
}

func (j jobRow) api() api.AlertNotification {
	status := api.AlertJobPending
	switch j.status {
	case "delivered":
		status = api.AlertJobDelivered
	case "failed":
		status = api.AlertJobFailed
	}
	event := api.AlertEventFired
	if j.event == "recovered" {
		event = api.AlertEventRecovered
	}
	return api.AlertNotification{
		ID:            j.id,
		RuleID:        j.ruleID,
		IncidentID:    j.incidentID,
		Event:         event,
		Status:        status,
		Attempts:      j.attempts,
		MaxAttempts:   j.maxAttempts,
		NextAttemptAt: formatTimePtr(j.nextAttempt),
		LastAttemptAt: formatTimePtr(j.lastAttempt),
		LastError:     cloneStringPtr(j.lastError),
		CreatedAt:     formatTime(j.createdAt),
		DeliveredAt:   formatTimePtr(j.deliveredAt),
	}
}

type webhookRow struct {
	url         string
	headersJSON string
	secret      *string
	enabled     bool
	updatedAt   time.Time
	lastTestAt  *time.Time
	lastTestOK  *bool
	lastError   *string
}

// -- rules ---------------------------------------------------------------

func listRulesDB(ctx context.Context, database *storage.Database, machineID string) ([]ruleRow, error) {
	query := "SELECT " + alertRuleColumns + " FROM alert_rules"
	var args []any
	if machineID != "" {
		query += " WHERE machine_id = ?"
		args = append(args, machineID)
	}
	query += " ORDER BY created_at_ms ASC"
	rows, err := database.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ruleRow
	for rows.Next() {
		rule, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func listRulesTx(tx *storage.Tx, machineID string) ([]ruleRow, error) {
	query := "SELECT " + alertRuleColumns + " FROM alert_rules"
	var args []any
	if machineID != "" {
		query += " WHERE machine_id = ?"
		args = append(args, machineID)
	}
	query += " ORDER BY created_at_ms ASC"
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ruleRow
	for rows.Next() {
		rule, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func ruleByIDDB(ctx context.Context, database *storage.Database, id string) (ruleRow, error) {
	rule, err := scanRule(database.QueryRow(ctx, "SELECT "+alertRuleColumns+" FROM alert_rules WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return ruleRow{}, notFound("alert rule")
	}
	return rule, err
}

func ruleByIDTx(tx *storage.Tx, id string) (ruleRow, error) {
	rule, err := scanRule(tx.QueryRow("SELECT "+alertRuleColumns+" FROM alert_rules WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return ruleRow{}, notFound("alert rule")
	}
	return rule, err
}

func countRulesDB(ctx context.Context, database *storage.Database) (int, error) {
	var count int
	if err := database.QueryRow(ctx, "SELECT COUNT(*) FROM alert_rules").Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func countRulesTx(tx *storage.Tx) (int, error) {
	var count int
	if err := tx.QueryRow("SELECT COUNT(*) FROM alert_rules").Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func insertRuleTx(tx *storage.Tx, rule ruleRow) error {
	_, err := tx.Exec(
		"INSERT INTO alert_rules ("+alertRuleColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		rule.id, rule.name, rule.machineID, rule.metric, rule.resourceID, rule.condition,
		nullableFloat(rule.threshold), rule.durationMs, boolInt(rule.enabled),
		nullableTime(rule.maintenanceUntil), storage.AlertStateOK, nil, nil, nil, nil,
		rule.createdAt.UnixMilli(), rule.updatedAt.UnixMilli(),
	)
	return err
}

func updateRuleConfigTx(tx *storage.Tx, rule ruleRow) error {
	_, err := tx.Exec(
		"UPDATE alert_rules SET name = ?, machine_id = ?, metric = ?, resource_id = ?, condition = ?, "+
			"threshold = ?, duration_ms = ?, enabled = ?, maintenance_until_ms = ?, updated_at_ms = ? WHERE id = ?",
		rule.name, rule.machineID, rule.metric, rule.resourceID, rule.condition,
		nullableFloat(rule.threshold), rule.durationMs, boolInt(rule.enabled),
		nullableTime(rule.maintenanceUntil), rule.updatedAt.UnixMilli(), rule.id,
	)
	return err
}

func updateRuleStateTx(tx *storage.Tx, id, state string, pendingSince *time.Time, activeIncidentID *string, lastValue *float64, observedAt time.Time) error {
	_, err := tx.Exec(
		"UPDATE alert_rules SET state = ?, pending_since_ms = ?, active_incident_id = ?, "+
			"last_value = ?, last_observed_at_ms = ? WHERE id = ?",
		state, nullableTime(pendingSince), nullableString(activeIncidentID),
		nullableFloat(lastValue), observedAt.UnixMilli(), id,
	)
	return err
}

func deleteRuleTx(tx *storage.Tx, id string) error {
	_, err := tx.Exec("DELETE FROM alert_rules WHERE id = ?", id)
	return err
}

// -- incidents -----------------------------------------------------------

func openIncidentTx(tx *storage.Tx, ruleID string) (*incidentRow, error) {
	incident, err := scanIncident(tx.QueryRow(
		"SELECT "+alertIncidentColumns+" FROM alert_incidents WHERE rule_id = ? AND status = ? "+
			"ORDER BY started_at_ms DESC LIMIT 1",
		ruleID, storage.IncidentOpen,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &incident, nil
}

func incidentByIDTx(tx *storage.Tx, id string) (*incidentRow, error) {
	incident, err := scanIncident(tx.QueryRow("SELECT "+alertIncidentColumns+" FROM alert_incidents WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &incident, nil
}

func insertIncidentTx(tx *storage.Tx, incident incidentRow) error {
	_, err := tx.Exec(
		"INSERT INTO alert_incidents ("+alertIncidentColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		incident.id, incident.ruleID, incident.ruleName, incident.machineID, incident.metric,
		incident.resourceID, incident.condition, incident.status, incident.startedAt.UnixMilli(),
		nullableTime(incident.resolvedAt), nullableFloat(incident.startedValue),
		nullableFloat(incident.resolvedValue), incident.lastObservedAt.UnixMilli(), incident.message,
	)
	return err
}

func resolveIncidentTx(tx *storage.Tx, id string, at time.Time, value *float64) error {
	_, err := tx.Exec(
		"UPDATE alert_incidents SET status = ?, resolved_at_ms = ?, resolved_value = ?, "+
			"last_observed_at_ms = ? WHERE id = ? AND status = ?",
		storage.IncidentRecovered, at.UnixMilli(), nullableFloat(value), at.UnixMilli(),
		id, storage.IncidentOpen,
	)
	return err
}

func recentIncidentsDB(ctx context.Context, database *storage.Database, limit int) ([]incidentRow, error) {
	rows, err := database.Query(ctx,
		"SELECT "+alertIncidentColumns+" FROM alert_incidents ORDER BY started_at_ms DESC, id LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incidentRow
	for rows.Next() {
		incident, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, incident)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func trimIncidentsTx(tx *storage.Tx, keep int) error {
	_, err := tx.Exec(
		"DELETE FROM alert_incidents WHERE id NOT IN ("+
			"SELECT id FROM alert_incidents ORDER BY (status = 'open') DESC, started_at_ms DESC LIMIT ?)",
		keep,
	)
	return err
}

// -- delivery jobs -------------------------------------------------------

func insertJobTx(tx *storage.Tx, job jobRow) error {
	_, err := tx.Exec(
		"INSERT OR IGNORE INTO alert_jobs ("+alertJobColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		job.id, job.ruleID, job.incidentID, job.machineID, job.event, job.status,
		job.attempts, job.maxAttempts, nullableTime(job.nextAttempt), nullableTime(job.lastAttempt),
		nullableString(job.lastError), job.createdAt.UnixMilli(), nullableTime(job.deliveredAt),
		job.payloadJSON,
	)
	return err
}

func trimJobsTx(tx *storage.Tx, keep int) error {
	_, err := tx.Exec(
		"DELETE FROM alert_jobs WHERE id NOT IN ("+
			"SELECT id FROM alert_jobs ORDER BY (status = 'pending') DESC, created_at_ms DESC LIMIT ?)",
		keep,
	)
	return err
}

func dueJobsDB(ctx context.Context, database *storage.Database, at time.Time) ([]jobRow, error) {
	rows, err := database.Query(ctx,
		"SELECT "+alertJobColumns+" FROM alert_jobs WHERE status = ? AND (next_attempt_ms IS NULL OR next_attempt_ms <= ?) "+
			"ORDER BY created_at_ms ASC LIMIT 8",
		jobPending, at.UnixMilli(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []jobRow
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func recentJobsDB(ctx context.Context, database *storage.Database, limit int) ([]jobRow, error) {
	rows, err := database.Query(ctx,
		"SELECT "+alertJobColumns+" FROM alert_jobs ORDER BY created_at_ms DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []jobRow
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func updateJobTx(tx *storage.Tx, job jobRow) error {
	_, err := tx.Exec(
		"UPDATE alert_jobs SET status = ?, attempts = ?, next_attempt_ms = ?, last_attempt_ms = ?, "+
			"last_error = ?, delivered_at_ms = ? WHERE id = ?",
		job.status, job.attempts, nullableTime(job.nextAttempt), nullableTime(job.lastAttempt),
		nullableString(job.lastError), nullableTime(job.deliveredAt), job.id,
	)
	return err
}

func postponeJobTx(tx *storage.Tx, id string, next time.Time, message string) error {
	_, err := tx.Exec(
		"UPDATE alert_jobs SET next_attempt_ms = ?, last_error = ? WHERE id = ?",
		next.UnixMilli(), message, id,
	)
	return err
}

// -- webhook -------------------------------------------------------------

func loadWebhookDB(ctx context.Context, database *storage.Database) (*webhookRow, error) {
	var (
		out                    webhookRow
		secret                 sql.NullString
		enabled                int64
		updatedAt              int64
		lastTestAt, lastTestOK sql.NullInt64
		lastError              sql.NullString
	)
	err := database.QueryRow(ctx,
		"SELECT url, headers_json, secret, enabled, updated_at_ms, last_test_at_ms, last_test_ok, last_error "+
			"FROM alert_webhook WHERE id = 1",
	).Scan(&out.url, &out.headersJSON, &secret, &enabled, &updatedAt, &lastTestAt, &lastTestOK, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if secret.Valid {
		value := secret.String
		out.secret = &value
	}
	out.enabled = enabled == 1
	out.updatedAt = msTime(updatedAt)
	out.lastTestAt = nullTime(lastTestAt)
	if lastTestOK.Valid {
		value := lastTestOK.Int64 == 1
		out.lastTestOK = &value
	}
	if lastError.Valid {
		value := lastError.String
		out.lastError = &value
	}
	return &out, nil
}

func upsertWebhookDB(ctx context.Context, database *storage.Database, webhook webhookRow) error {
	return database.Write(ctx, func(tx *storage.Tx) error {
		return upsertWebhookTx(tx, webhook)
	})
}

func upsertWebhookTx(tx *storage.Tx, webhook webhookRow) error {
	_, err := tx.Exec(
		"INSERT INTO alert_webhook (id, url, headers_json, secret, enabled, updated_at_ms, "+
			"last_test_at_ms, last_test_ok, last_error) VALUES (1, ?, ?, ?, ?, ?, NULL, NULL, NULL) "+
			"ON CONFLICT(id) DO UPDATE SET url = excluded.url, headers_json = excluded.headers_json, "+
			"secret = excluded.secret, enabled = excluded.enabled, updated_at_ms = excluded.updated_at_ms, "+
			"last_test_at_ms = NULL, last_test_ok = NULL, last_error = NULL",
		webhook.url, webhook.headersJSON, nullableString(webhook.secret),
		boolInt(webhook.enabled), webhook.updatedAt.UnixMilli(),
	)
	return err
}

func deleteWebhookDB(ctx context.Context, database *storage.Database) error {
	return database.Write(ctx, func(tx *storage.Tx) error {
		return deleteWebhookTx(tx)
	})
}

func deleteWebhookTx(tx *storage.Tx) error {
	_, err := tx.Exec("DELETE FROM alert_webhook WHERE id = 1")
	return err
}

func recordWebhookTestDB(ctx context.Context, database *storage.Database, at time.Time, ok bool, message *string) error {
	return database.Write(ctx, func(tx *storage.Tx) error {
		_, err := tx.Exec(
			"UPDATE alert_webhook SET last_test_at_ms = ?, last_test_ok = ?, last_error = ? WHERE id = 1",
			at.UnixMilli(), boolInt(ok), nullableString(message),
		)
		return err
	})
}

// -- machine progress and observed resources -----------------------------

func progressTx(tx *storage.Tx, machineID string) (*machineProgress, error) {
	var (
		out                   machineProgress
		seq                   int64
		receivedAt, updatedAt int64
	)
	err := tx.QueryRow(
		"SELECT collector_id, run_id, seq, received_at_ms, updated_at_ms FROM alert_machine_progress WHERE machine_id = ?",
		machineID,
	).Scan(&out.collectorID, &out.runID, &seq, &receivedAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.machineID = machineID
	out.seq = seq
	out.receivedAt = msTime(receivedAt)
	out.updatedAt = msTime(updatedAt)
	return &out, nil
}

func putProgressTx(tx *storage.Tx, progress machineProgress) error {
	_, err := tx.Exec(
		"INSERT INTO alert_machine_progress (machine_id, collector_id, run_id, seq, received_at_ms, updated_at_ms) "+
			"VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(machine_id) DO UPDATE SET collector_id = excluded.collector_id, "+
			"run_id = excluded.run_id, seq = excluded.seq, received_at_ms = excluded.received_at_ms, "+
			"updated_at_ms = excluded.updated_at_ms",
		progress.machineID, progress.collectorID, progress.runID, progress.seq,
		progress.receivedAt.UnixMilli(), progress.updatedAt.UnixMilli(),
	)
	return err
}

func observedExistsTx(tx *storage.Tx, ruleID, metric, resourceID string) (bool, error) {
	var seen int
	err := tx.QueryRow(
		"SELECT 1 FROM alert_observed_resources WHERE rule_id = ? AND metric = ? AND resource_id = ? LIMIT 1",
		ruleID, metric, resourceID,
	).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func seriesExistsTx(tx *storage.Tx, machineID, metric, resourceID string) (bool, error) {
	var seen int
	err := tx.QueryRow(
		"SELECT 1 FROM series WHERE machine_id = ? AND metric = ? AND resource_id = ? LIMIT 1",
		machineID, metric, resourceID,
	).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func markObservedTx(tx *storage.Tx, ruleID, metric, resourceID string, at time.Time) error {
	_, err := tx.Exec(
		"INSERT INTO alert_observed_resources (rule_id, metric, resource_id, first_seen_ms, last_seen_ms) "+
			"VALUES (?, ?, ?, ?, ?) ON CONFLICT(rule_id, metric, resource_id) DO UPDATE SET "+
			"last_seen_ms = excluded.last_seen_ms",
		ruleID, metric, resourceID, at.UnixMilli(), at.UnixMilli(),
	)
	return err
}

func deleteObservedTx(tx *storage.Tx, ruleID string) error {
	_, err := tx.Exec("DELETE FROM alert_observed_resources WHERE rule_id = ?", ruleID)
	return err
}

type machineProgress struct {
	machineID   string
	collectorID string
	runID       string
	seq         int64
	receivedAt  time.Time
	updatedAt   time.Time
}

// -- small value helpers -------------------------------------------------

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

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
