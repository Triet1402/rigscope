package alerts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
)

// Options configures one alert engine.
type Options struct {
	// Database owns the alert tables. Required.
	Database *storage.Database
	// Now is the clock. Defaults to time.Now.
	Now func() time.Time
	// Sender delivers webhook requests. Nil uses NewWebhookSender with the
	// configured SSRF policy.
	Sender WebhookSender
	// MaxIncidents bounds retained incident history. Default 1000.
	MaxIncidents int
	// MaxJobs bounds retained delivery jobs. Default 1000.
	MaxJobs int
	// MaxAttempts bounds one job's delivery attempts. Default 6.
	MaxAttempts int
	// DeliveryTimeout bounds one webhook request. Default 5 s (100 ms..30 s).
	DeliveryTimeout time.Duration
	// AllowPrivateWebhookTargets permits HTTPS delivery to loopback, private
	// and link-local addresses. Off by default.
	AllowPrivateWebhookTargets bool
	// Resolver overrides DNS for the default sender. Nil uses the system
	// resolver.
	Resolver IPResolver
	Logger   *slog.Logger
}

// Engine evaluates rules, owns the incident lifecycle and delivers webhook
// jobs.
type Engine struct {
	db              *storage.Database
	now             func() time.Time
	sender          WebhookSender
	maxIncidents    int
	maxJobs         int
	maxAttempts     int
	deliveryTimeout time.Duration
	logger          *slog.Logger
	startedAt       time.Time

	baseCtx context.Context
	cancel  context.CancelFunc

	stopped atomic.Bool
	closed  atomic.Bool

	notifyMu      sync.Mutex
	notifyRunning bool
	notifyAgain   bool
	notifyDone    chan struct{}
	background    sync.WaitGroup

	// evalMu serialises evaluation so an observation and a tick can never
	// interleave their read/modify/write of rule state. Everything below is
	// guarded by evalMu except lastTickAt.
	evalMu sync.Mutex
	// barriers holds the receipt-time horizon of the most recent continuity
	// break per machine. A record received before the barrier is pre-gap
	// evidence and must not restart a sustained duration. The entry is removed
	// once a record at or after the barrier has been evaluated, or once it is
	// older than continuityRetention.
	barriers map[string]time.Time
	// unresolvedBreaks holds machines whose continuity reset has not committed,
	// keyed by the break's receipt-time barrier. While an entry is present,
	// evaluations treat a pending rule as fresh instead of inheriting the
	// pre-gap pending interval.
	unresolvedBreaks map[string]time.Time
	// evaluated is the last observation this engine evaluated per machine. A
	// liveness tick judges stale and unavailable rules only from this evidence,
	// never from the live snapshot: a record can be published to live state
	// before the single evaluation worker has drained it.
	evaluated map[string]evaluatedObservation
	// tickHorizon is the latest successfully committed liveness-tick evaluation
	// time. A tick never evaluates behind state it already committed. A failed
	// tick leaves it unchanged so a retry can use its own (earlier) cutoff.
	tickHorizon time.Time
	// lastTickAt is the engine clock of the most recent committed liveness
	// tick. It is updated atomically so the admin diagnostics endpoint can read
	// it without taking evalMu.
	lastTickAt atomic.Int64

	// testRulesLoadedHook is a test seam invoked after ObserveAt loads a
	// machine's rules and before it mutates rule state.
	testRulesLoadedHook func()
}

// continuityRetention bounds in-memory continuity and evidence state. A
// barrier older than the longest rule duration plus the ingest replay horizon
// can no longer matter: any record that could be pre-gap evidence is older
// than the horizon at which ingest permanently rejects it. Pending rule
// intervals older than this bound are also treated as fresh, so dropping a
// pruned barrier can never manufacture a sustained duration.
const continuityRetention = time.Duration(MaxDurationMs)*time.Millisecond + storage.DefaultMaxReplayAge

// evaluatedObservation is the last evidence the engine committed for one
// machine. Tick evaluation is derived only from this, never from live state.
type evaluatedObservation struct {
	receipt time.Time
	payload protocol.MetricsPayload
}

// New validates the dependencies and returns an engine. It does not start a
// goroutine: Observe and Tick schedule delivery, and the composition root calls
// Tick about once a second like the reference hub.
func New(opts Options) (*Engine, error) {
	if opts.Database == nil {
		return nil, errors.New("alerts: database is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	maxAttempts := boundedCount(opts.MaxAttempts, DefaultMaxAttempts, 1, 100)
	deliveryTimeout := opts.DeliveryTimeout
	if deliveryTimeout <= 0 {
		deliveryTimeout = DefaultDeliveryTimeout
	}
	if deliveryTimeout < 100*time.Millisecond {
		deliveryTimeout = 100 * time.Millisecond
	}
	if deliveryTimeout > 30*time.Second {
		deliveryTimeout = 30 * time.Second
	}
	sender := opts.Sender
	if sender == nil {
		sender = NewWebhookSender(WebhookSenderOptions{
			AllowPrivateTargets: opts.AllowPrivateWebhookTargets,
			Resolver:            opts.Resolver,
		})
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	return &Engine{
		db:               opts.Database,
		now:              now,
		sender:           sender,
		maxIncidents:     boundedCount(opts.MaxIncidents, MaxIncidents, 1, MaxIncidents),
		maxJobs:          boundedCount(opts.MaxJobs, MaxJobs, 1, MaxJobs),
		maxAttempts:      maxAttempts,
		deliveryTimeout:  deliveryTimeout,
		logger:           logger,
		startedAt:        now(),
		baseCtx:          baseCtx,
		cancel:           cancel,
		barriers:         make(map[string]time.Time),
		unresolvedBreaks: make(map[string]time.Time),
		evaluated:        make(map[string]evaluatedObservation),
	}, nil
}

func boundedCount(value, fallback, min, max int) int {
	if value == 0 {
		return fallback
	}
	if value < min || value > max {
		return fallback
	}
	return value
}

// Stop aborts in-flight webhook requests and refuses new writes. A late remote
// completion can no longer touch the database.
func (e *Engine) Stop() {
	if e.stopped.Swap(true) {
		return
	}
	e.closed.Store(true)
	e.cancel()
}

// Drain stops the engine and waits up to timeout for in-flight delivery work.
// Callers must Drain before closing the database.
func (e *Engine) Drain(timeout time.Duration) {
	e.Stop()
	if timeout <= 0 {
		timeout = e.deliveryTimeout + time.Second
	}
	done := make(chan struct{})
	go func() {
		e.background.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// Close is Drain with the default bound.
func (e *Engine) Close() { e.Drain(0) }

// nowMs reports the engine clock in milliseconds.
func (e *Engine) nowTime() time.Time { return e.now() }

// SessionAuthority is the browser session that originated an alert mutation.
// It is validated again inside each write transaction, so a session revoked or
// an account demoted while the request body was being read cannot commit.
type SessionAuthority = storage.SessionAuthority

// validateMutationAuthority re-checks the originating session inside tx and
// returns its current machine scope. It is shared by every alert write.
func (e *Engine) validateMutationAuthority(tx *storage.Tx, authority SessionAuthority) (storage.Scope, error) {
	return storage.AuthorizeSessionTx(tx, authority, e.nowTime())
}

// -- rule CRUD -----------------------------------------------------------

type validatedRule struct {
	id               string
	name             string
	machineID        string
	metric           api.AlertMetric
	resourceID       string
	condition        api.AlertCondition
	threshold        *float64
	durationMs       int64
	enabled          bool
	maintenanceUntil *time.Time
}

func validateRuleInput(input RuleInput) (validatedRule, error) {
	name := strings.TrimSpace(input.Name)
	if utf16Len(name) < 1 || utf16Len(name) > MaxRuleName {
		return validatedRule{}, validationf("name length is invalid")
	}
	if hasControl(name) {
		return validatedRule{}, validationf("name contains invalid characters")
	}
	machineID := strings.TrimSpace(input.MachineID)
	if utf16Len(machineID) < 1 || utf16Len(machineID) > MaxMachineID {
		return validatedRule{}, validationf("machineId length is invalid")
	}
	if hasControl(machineID) || hasSpace(machineID) {
		return validatedRule{}, validationf("machineId contains invalid characters")
	}
	spec, ok := metricSpecFor(input.Metric)
	if !ok {
		return validatedRule{}, validationf("unknown metric")
	}
	resourceID := strings.TrimSpace(input.ResourceID)
	if utf16Len(resourceID) > MaxResourceID {
		return validatedRule{}, validationf("resourceId length is invalid")
	}
	if hasControl(resourceID) || hasSpace(resourceID) {
		return validatedRule{}, validationf("resourceId contains invalid characters")
	}
	if spec.needsResource && resourceID == "" {
		return validatedRule{}, validationf("this metric requires a resource id")
	}
	if !spec.needsResource && resourceID != "" {
		return validatedRule{}, validationf("this metric does not accept a resource id")
	}
	switch input.Condition {
	case api.AlertConditionAbove, api.AlertConditionBelow, api.AlertConditionStale, api.AlertConditionUnavailable:
	default:
		return validatedRule{}, validationf("unknown condition")
	}
	if input.DurationMs < 0 || input.DurationMs > MaxDurationMs {
		return validatedRule{}, validationf("durationMs is outside bounds")
	}
	var threshold *float64
	switch input.Condition {
	case api.AlertConditionAbove, api.AlertConditionBelow:
		if input.Threshold == nil || !finite(*input.Threshold) || math.Abs(*input.Threshold) > MaxThreshold {
			return validatedRule{}, validationf("threshold is required and must be a finite bounded number")
		}
		value := *input.Threshold
		threshold = &value
	default:
		if input.Threshold != nil {
			return validatedRule{}, validationf("this condition does not accept a threshold")
		}
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	return validatedRule{
		name:             name,
		machineID:        machineID,
		metric:           input.Metric,
		resourceID:       resourceID,
		condition:        input.Condition,
		threshold:        threshold,
		durationMs:       input.DurationMs,
		enabled:          enabled,
		maintenanceUntil: input.MaintenanceUntil,
	}, nil
}

// CreateRule persists a new rule and returns it with fresh ok state. The
// originating session is re-checked inside the insert transaction.
func (e *Engine) CreateRule(ctx context.Context, authority SessionAuthority, input RuleInput) (api.AlertRule, error) {
	validated, err := validateRuleInput(input)
	if err != nil {
		return api.AlertRule{}, err
	}
	id, err := newUUID()
	if err != nil {
		return api.AlertRule{}, err
	}
	at := e.nowTime()
	row := ruleRow{
		id:               id,
		name:             validated.name,
		machineID:        validated.machineID,
		metric:           string(validated.metric),
		resourceID:       validated.resourceID,
		condition:        string(validated.condition),
		threshold:        validated.threshold,
		durationMs:       validated.durationMs,
		enabled:          validated.enabled,
		maintenanceUntil: validated.maintenanceUntil,
		state:            storage.AlertStateOK,
		createdAt:        at,
		updatedAt:        at,
	}
	if err := e.db.Write(ctx, func(tx *storage.Tx) error {
		scope, err := e.validateMutationAuthority(tx, authority)
		if err != nil {
			return err
		}
		if !scope.Contains(row.machineID) {
			return storage.ErrScopeForbidden
		}
		// Counting inside the write transaction closes the race where two
		// concurrent creators both observe a below-limit count.
		count, err := countRulesTx(tx)
		if err != nil {
			return err
		}
		if count >= MaxRules {
			return validationf("alert rule limit reached")
		}
		return insertRuleTx(tx, row)
	}); err != nil {
		return api.AlertRule{}, err
	}
	return row.api(), nil
}

// UpdateRule replaces a rule's configuration and reconciles its state. Both the
// rule's current machine and the new target must be inside the originating
// session's current scope.
func (e *Engine) UpdateRule(ctx context.Context, authority SessionAuthority, id string, input RuleInput) (api.AlertRule, error) {
	validated, err := validateRuleInput(input)
	if err != nil {
		return api.AlertRule{}, err
	}
	at := e.nowTime()
	maintenanceActive := validated.maintenanceUntil != nil && validated.maintenanceUntil.After(at)
	var updated ruleRow
	err = e.db.Write(ctx, func(tx *storage.Tx) error {
		scope, err := e.validateMutationAuthority(tx, authority)
		if err != nil {
			return err
		}
		// Read the rule inside the same write transaction that reconciles its
		// incident so a concurrent update or delete cannot race the decision.
		row, err := ruleByIDTx(tx, id)
		if err != nil {
			return err
		}
		if !scope.Contains(row.machineID) || !scope.Contains(validated.machineID) {
			return storage.ErrScopeForbidden
		}
		targetChanged := row.machineID != validated.machineID ||
			row.metric != string(validated.metric) ||
			row.resourceID != validated.resourceID ||
			row.condition != string(validated.condition) ||
			!equalFloatPtr(row.threshold, validated.threshold)
		if row.state == storage.AlertStateFiring && (targetChanged || !validated.enabled) {
			if err := e.closeIncidentSilentlyTx(tx, row, at); err != nil {
				return err
			}
		} else if row.state == storage.AlertStatePending && (targetChanged || maintenanceActive || !validated.enabled) {
			if err := updateRuleStateTx(tx, row.id, storage.AlertStateOK, nil, nil, row.lastValue, at); err != nil {
				return err
			}
		}
		if targetChanged {
			if err := deleteObservedTx(tx, id); err != nil {
				return err
			}
		}
		row.name = validated.name
		row.machineID = validated.machineID
		row.metric = string(validated.metric)
		row.resourceID = validated.resourceID
		row.condition = string(validated.condition)
		row.threshold = validated.threshold
		row.durationMs = validated.durationMs
		row.enabled = validated.enabled
		row.maintenanceUntil = validated.maintenanceUntil
		row.updatedAt = at
		if err := updateRuleConfigTx(tx, row); err != nil {
			return err
		}
		updated, err = ruleByIDTx(tx, id)
		return err
	})
	if err != nil {
		return api.AlertRule{}, err
	}
	return updated.api(), nil
}

// DeleteRule removes a rule, silently closes its open incident and forgets its
// observed resources. The originating session and the rule's machine scope are
// re-checked inside the delete transaction.
func (e *Engine) DeleteRule(ctx context.Context, authority SessionAuthority, id string) error {
	return e.db.Write(ctx, func(tx *storage.Tx) error {
		scope, err := e.validateMutationAuthority(tx, authority)
		if err != nil {
			return err
		}
		// The incident reconciliation must use the rule state committed in
		// this transaction, not a row read before it acquired the writer.
		existing, err := ruleByIDTx(tx, id)
		if err != nil {
			return err
		}
		if !scope.Contains(existing.machineID) {
			return storage.ErrScopeForbidden
		}
		if err := e.closeIncidentSilentlyTx(tx, existing, e.nowTime()); err != nil {
			return err
		}
		if err := deleteObservedTx(tx, id); err != nil {
			return err
		}
		return deleteRuleTx(tx, id)
	})
}

// ForgetMachine drops the evaluator's in-memory state for a removed machine.
// Its rules, incidents and jobs are deleted from the database by the caller.
func (e *Engine) ForgetMachine(machineID string) {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	delete(e.barriers, machineID)
	delete(e.unresolvedBreaks, machineID)
	delete(e.evaluated, machineID)
}

// GetRule returns one rule.
func (e *Engine) GetRule(ctx context.Context, id string) (api.AlertRule, error) {
	row, err := ruleByIDDB(ctx, e.db, id)
	if err != nil {
		return api.AlertRule{}, err
	}
	return row.api(), nil
}

// ListRules returns rules within scope, oldest first, bounded by MaxRules.
func (e *Engine) ListRules(ctx context.Context, scope Scope) ([]api.AlertRule, error) {
	rows, err := listRulesDB(ctx, e.db, "")
	if err != nil {
		return nil, err
	}
	if scope == nil {
		scope = AllMachines()
	}
	out := make([]api.AlertRule, 0, len(rows))
	for _, row := range rows {
		if !scope.Contains(row.machineID) {
			continue
		}
		if len(out) >= MaxRules {
			break
		}
		out = append(out, row.api())
	}
	return out, nil
}

// ListIncidents returns the newest incidents within scope.
func (e *Engine) ListIncidents(ctx context.Context, scope Scope, limit int) ([]api.AlertIncident, error) {
	if limit < 1 || limit > MaxListLimit {
		limit = DefaultListLimit
	}
	if scope == nil {
		scope = AllMachines()
	}
	rows, err := recentIncidentsDB(ctx, e.db, e.maxIncidents)
	if err != nil {
		return nil, err
	}
	out := make([]api.AlertIncident, 0, limit)
	for _, row := range rows {
		if !scope.Contains(row.machineID) {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, row.api())
	}
	return out, nil
}

// NotificationSummary returns the bounded delivery queue view for scope.
func (e *Engine) NotificationSummary(ctx context.Context, scope Scope) (api.AlertNotificationSummary, error) {
	if scope == nil {
		scope = AllMachines()
	}
	rows, err := recentJobsDB(ctx, e.db, e.maxJobs)
	if err != nil {
		return api.AlertNotificationSummary{}, err
	}
	summary := api.AlertNotificationSummary{
		Recent:   []api.AlertNotification{},
		Delivery: api.DeliveryAtLeastOnceValue,
	}
	for _, row := range rows {
		if !scope.Contains(row.machineID) {
			continue
		}
		item := row.api()
		switch item.Status {
		case api.AlertJobPending:
			summary.Pending++
		case api.AlertJobDelivered:
			summary.Delivered++
		case api.AlertJobFailed:
			summary.Failed++
		}
		if len(summary.Recent) < 50 {
			summary.Recent = append(summary.Recent, item)
		}
		if item.Status == api.AlertJobDelivered && summary.LastDeliveryAt == nil {
			summary.LastDeliveryAt = cloneStringPtr(item.DeliveredAt)
		}
		if item.LastError != nil && summary.LastError == nil {
			summary.LastError = cloneStringPtr(item.LastError)
		}
	}
	return summary, nil
}

// ListResponse builds GET /api/alerts for one session.
func (e *Engine) ListResponse(ctx context.Context, scope Scope, canAdmin bool, limit int) (api.AlertListResponse, error) {
	rules, err := e.ListRules(ctx, scope)
	if err != nil {
		return api.AlertListResponse{}, err
	}
	incidents, err := e.ListIncidents(ctx, scope, limit)
	if err != nil {
		return api.AlertListResponse{}, err
	}
	notifications, err := e.NotificationSummary(ctx, scope)
	if err != nil {
		return api.AlertListResponse{}, err
	}
	webhook, err := e.WebhookStatus(ctx)
	if err != nil {
		return api.AlertListResponse{}, err
	}
	return api.AlertListResponse{
		GeneratedAt:        formatTime(e.nowTime()),
		CanAdmin:           canAdmin,
		Rules:              rules,
		Incidents:          incidents,
		Notifications:      notifications,
		Webhook:            webhook,
		Limits:             Limits,
		DeliveryDisclosure: DeliveryDisclosure,
	}, nil
}

func equalFloatPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func newUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return hex.EncodeToString(raw[0:4]) + "-" + hex.EncodeToString(raw[4:6]) + "-" +
		hex.EncodeToString(raw[6:8]) + "-" + hex.EncodeToString(raw[8:10]) + "-" +
		hex.EncodeToString(raw[10:16]), nil
}

// -- observe and tick ----------------------------------------------------

// Observe evaluates one newly committed LIVE metrics record using the engine
// clock as its receipt time.
func (e *Engine) Observe(machineID string, record protocol.MetricsRecord, mode Mode) {
	e.ObserveAt(machineID, record, mode, e.nowTime())
}

// ObserveAt is Observe with the hub's exact receipt time for the record. A
// replay or a record received before this engine started is ignored.
//
// Sustained durations are measured from receipt-time evidence only: the
// record's own receipt time is the evaluation time. Nothing clamps it to a
// later wall-clock evaluation, because doing so would manufacture elapsed
// evidence that the hub never received.
func (e *Engine) ObserveAt(machineID string, record protocol.MetricsRecord, mode Mode, receivedAt time.Time) {
	if e.closed.Load() || mode != Live || receivedAt.IsZero() || receivedAt.Before(e.startedAt) {
		return
	}
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	if e.closed.Load() {
		return
	}
	if barrier, ok := e.barriers[machineID]; ok && receivedAt.Before(barrier) {
		// Pre-gap evidence cannot restart a sustained duration after a
		// continuity break.
		return
	}
	unresolved := e.continuityUnresolved(machineID)
	at := receivedAt
	evaluated := false
	err := e.db.Write(context.Background(), func(tx *storage.Tx) error {
		// Load rule state inside the write transaction so a concurrent
		// update, disable or delete either happens entirely before this
		// evaluation or entirely after it.
		rules, err := listRulesTx(tx, machineID)
		if err != nil {
			return err
		}
		// No rules is the normal default. Do not write a progress row for
		// every sample just to remember a sequence number nothing uses.
		if len(rules) == 0 {
			evaluated = true
			return nil
		}
		if e.testRulesLoadedHook != nil {
			e.testRulesLoadedHook()
		}
		progress, err := progressTx(tx, machineID)
		if err != nil {
			return err
		}
		if progress != nil {
			if progress.runID == record.RunID && record.Sequence <= progress.seq {
				return nil
			}
			// A record older than state already applied must not shorten or
			// lengthen a duration.
			if receivedAt.Before(progress.receivedAt) {
				return nil
			}
		}
		if err := putProgressTx(tx, machineProgress{
			machineID:   machineID,
			collectorID: record.CollectorID,
			runID:       record.RunID,
			seq:         record.Sequence,
			receivedAt:  receivedAt,
			updatedAt:   e.nowTime(),
		}); err != nil {
			return err
		}
		evaluated = true
		for _, row := range rules {
			if !row.enabled {
				continue
			}
			if unresolved {
				row = withoutPendingState(row)
			}
			rule := validatedFromRow(row)
			if row.condition == string(api.AlertConditionStale) {
				// A just-received live record proves the machine is not stale.
				if err := e.evaluateTx(tx, row, rule, false, nil, at); err != nil {
					return err
				}
				continue
			}
			observation, err := e.observationForTx(tx, row, record.Payload, at)
			if err != nil {
				return err
			}
			condition := conditionIsTrue(rule, observation)
			value := observation.value
			if err := e.evaluateTx(tx, row, rule, condition, value, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// The live path must never be interrupted by an alerting failure.
		e.logger.Warn("alert evaluation failed", "machineId", machineID, "error", err)
	} else if evaluated {
		// Only observations this engine actually committed are evidence for a
		// later liveness tick. A queued record that has merely been published
		// to live state must never make a tick judge a machine early.
		if previous, ok := e.evaluated[machineID]; !ok || !receivedAt.Before(previous.receipt) {
			e.evaluated[machineID] = evaluatedObservation{receipt: receivedAt, payload: record.Payload}
		}
		if barrier, ok := e.barriers[machineID]; ok && !receivedAt.Before(barrier) {
			// A record at or after the barrier is post-gap evidence; later
			// ordering is handled by the durable machine progress row.
			delete(e.barriers, machineID)
		}
	}
	e.scheduleNotifications()
}

// withoutPendingState treats a pending rule as fresh. It is used while a
// machine's continuity reset has not committed, so evaluations never inherit
// the pre-gap pending interval. An open incident is intentionally preserved.
func withoutPendingState(row ruleRow) ruleRow {
	if row.state == storage.AlertStatePending {
		row.state = storage.AlertStateOK
		row.pendingSince = nil
	}
	return row
}

// continuityUnresolved reports whether a machine's continuity reset still
// needs to commit. Callers hold evalMu.
func (e *Engine) continuityUnresolved(machineID string) bool {
	_, ok := e.unresolvedBreaks[machineID]
	return ok
}

// BreakContinuity forgets sustained-duration progress for one machine after a
// gap in its live observation stream. A pending rule returns to ok so the next
// observation restarts its duration; an incident that is already open stays
// open, because the gap itself must never resolve it.
func (e *Engine) BreakContinuity(machineID string) error {
	return e.BreakContinuityAt(machineID, time.Time{})
}

// BreakContinuityAt is BreakContinuity with the earliest receipt time that may
// start new sustained-duration state. Records older than since are pre-gap
// evidence and are ignored. A zero since uses the engine clock. A failed
// storage reset leaves the break unresolved in memory: evaluations treat
// pending rules as fresh until a retry commits.
func (e *Engine) BreakContinuityAt(machineID string, since time.Time) error {
	if machineID == "" || e.closed.Load() {
		return nil
	}
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	if e.closed.Load() {
		return nil
	}
	if since.IsZero() {
		since = e.nowTime()
	}
	if previous, ok := e.barriers[machineID]; !ok || since.After(previous) {
		e.barriers[machineID] = since
	}
	err := e.db.Write(context.Background(), func(tx *storage.Tx) error {
		rules, err := listRulesTx(tx, machineID)
		if err != nil {
			return err
		}
		for _, row := range rules {
			if row.state != storage.AlertStatePending {
				continue
			}
			observedAt := since
			if row.lastObservedAt != nil {
				observedAt = *row.lastObservedAt
			}
			if err := updateRuleStateTx(tx, row.id, storage.AlertStateOK, nil, nil, row.lastValue, observedAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		e.unresolvedBreaks[machineID] = since
		return err
	}
	delete(e.unresolvedBreaks, machineID)
	return nil
}

// Tick evaluates stale and unavailable rules for the supplied machines.
// Machines absent from the list are untouched.
func (e *Engine) Tick(machines []MachineState) {
	e.TickAsOf(machines, time.Time{})
}

// TickAsOf is Tick with a receipt cutoff. The runtime supplies the newest
// observation receipt it drained for this tick, so a tick can never judge
// staleness or recovery for evidence the hub has received but not yet
// evaluated. A zero cutoff means the engine clock is the horizon.
//
// A machine's stale and unavailable state is derived only from the last
// observation this engine evaluated for it. Two durable fallbacks cover a
// restart: the machine progress receipt for stale rules, and the machine_latest
// last-received time when the previous hub process had no rule to evaluate and
// therefore wrote no progress row. The latter is used only when it predates
// this engine's start; a receipt from after the start is still waiting in the
// evaluation queue, and judging it early would manufacture elapsed evidence.
// The live snapshot contributes identity and cadence only.
func (e *Engine) TickAsOf(machines []MachineState, cutoff time.Time) {
	if e.closed.Load() {
		return
	}
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	if e.closed.Load() {
		return
	}
	now := e.nowTime()
	e.pruneContinuity(now)
	at := now
	if !cutoff.IsZero() && !cutoff.Before(e.startedAt) && cutoff.Before(at) {
		at = cutoff
	}
	if at.Before(e.tickHorizon) {
		at = e.tickHorizon
	}
	count, err := countRulesDB(context.Background(), e.db)
	if err != nil {
		e.logger.Warn("alert tick failed", "error", err)
		e.scheduleNotifications()
		return
	}
	if count == 0 {
		e.tickHorizon = at
		e.lastTickAt.Store(now.UnixNano())
		e.scheduleNotifications()
		return
	}
	err = e.db.Write(context.Background(), func(tx *storage.Tx) error {
		for _, machine := range machines {
			unresolved := e.continuityUnresolved(machine.MachineID)
			rows, err := listRulesTx(tx, machine.MachineID)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				continue
			}
			evidence, hasEvidence := e.evaluated[machine.MachineID]
			receipt := time.Time{}
			if hasEvidence {
				receipt = evidence.receipt
			}
			for _, row := range rows {
				if !row.enabled {
					continue
				}
				if unresolved {
					row = withoutPendingState(row)
				}
				rule := validatedFromRow(row)
				switch api.AlertCondition(row.condition) {
				case api.AlertConditionStale:
					machineReceipt := receipt
					if machineReceipt.IsZero() {
						// The engine keeps no in-memory evidence across a
						// restart, but the durable progress row is still the
						// receipt this engine committed before stopping.
						progress, err := progressTx(tx, machine.MachineID)
						if err != nil {
							return err
						}
						if progress != nil {
							machineReceipt = progress.receivedAt
						}
					}
					if machineReceipt.IsZero() && machine.LastReceivedAt != nil &&
						machine.LastReceivedAt.Before(e.startedAt) {
						// A machine that had no rules before the restart never
						// wrote a progress row, so machine_latest is the only
						// durable receipt. Only a receipt older than this
						// engine can be trusted: anything newer may still be
						// queued for evaluation.
						machineReceipt = *machine.LastReceivedAt
					}
					if machineReceipt.IsZero() || machineReceipt.After(at) {
						continue
					}
					stale := at.Sub(machineReceipt) >= staleThreshold(machine.ExpectedIntervalMs)
					if err := e.evaluateTx(tx, row, rule, stale, nil, at); err != nil {
						return err
					}
				case api.AlertConditionUnavailable:
					if !hasEvidence || evidence.receipt.After(at) {
						continue
					}
					// Pre-gap payload is not evidence of the machine's state
					// after the break. Wait for a post-gap observation instead
					// of accumulating unavailable time from stale evidence.
					if barrier, ok := e.barriers[machine.MachineID]; ok && evidence.receipt.Before(barrier) {
						continue
					}
					observation, err := e.observationForTx(tx, row, evidence.payload, at)
					if err != nil {
						return err
					}
					if err := e.evaluateTx(tx, row, rule, observation.state == observationUnavailable, nil, at); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		// A failed transaction committed nothing. Leave the tick horizon at
		// its previous value so a retry with its own cutoff can evaluate the
		// evidence that attempt was meant to cover.
		e.logger.Warn("alert tick failed", "error", err)
		e.scheduleNotifications()
		return
	}
	e.tickHorizon = at
	e.lastTickAt.Store(now.UnixNano())
	e.scheduleNotifications()
}

// pruneContinuity drops in-memory continuity and evidence state that can no
// longer affect an evaluation. Callers hold evalMu.
//
// A retained record older than the ingest replay horizon is rejected before it
// reaches the engine, and continuityRetention adds the longest rule duration to
// that horizon. Any queued observation that could be pre-gap evidence for a
// pruned barrier is therefore far older than the bounded queue can hold, or is
// already rejected at ingest. Pending intervals older than the same bound are
// treated as fresh in evaluateTx, so pruning an unresolved break cannot make a
// stale pending duration fire.
func (e *Engine) pruneContinuity(now time.Time) {
	for machineID, barrier := range e.barriers {
		if now.Sub(barrier) > continuityRetention {
			delete(e.barriers, machineID)
		}
	}
	for machineID, since := range e.unresolvedBreaks {
		if now.Sub(since) > continuityRetention {
			delete(e.unresolvedBreaks, machineID)
		}
	}
	for machineID, observation := range e.evaluated {
		if now.Sub(observation.receipt) > continuityRetention {
			delete(e.evaluated, machineID)
		}
	}
}

// LastTickAt reports the engine clock of the most recent liveness tick, or the
// zero time when no tick has run.
func (e *Engine) LastTickAt() time.Time {
	nanos := e.lastTickAt.Load()
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos).UTC()
}

func validatedFromRow(row ruleRow) validatedRule {
	enabled := row.enabled
	return validatedRule{
		id:               row.id,
		name:             row.name,
		machineID:        row.machineID,
		metric:           api.AlertMetric(row.metric),
		resourceID:       row.resourceID,
		condition:        api.AlertCondition(row.condition),
		threshold:        row.threshold,
		durationMs:       row.durationMs,
		enabled:          enabled,
		maintenanceUntil: row.maintenanceUntil,
	}
}

// -- evaluation ----------------------------------------------------------

type observationState uint8

const (
	observationAbsent observationState = iota
	observationUnavailable
	observationValue
)

type observation struct {
	state observationState
	value *float64
}

func (e *Engine) evaluateTx(tx *storage.Tx, row ruleRow, rule validatedRule, condition bool, value *float64, at time.Time) error {
	maintenance := rule.maintenanceUntil != nil && rule.maintenanceUntil.After(at)
	if maintenance {
		if row.state == storage.AlertStatePending {
			return updateRuleStateTx(tx, row.id, storage.AlertStateOK, nil, nil, row.lastValue, at)
		}
		return nil
	}
	switch row.state {
	case storage.AlertStateFiring:
		if condition {
			return updateRuleStateTx(tx, row.id, storage.AlertStateFiring, row.pendingSince, row.activeIncidentID, value, at)
		}
		return e.recoverTx(tx, row, rule, value, at)
	}
	if !condition {
		return updateRuleStateTx(tx, row.id, storage.AlertStateOK, nil, nil, value, at)
	}
	since := at
	if row.state == storage.AlertStatePending && row.pendingSince != nil {
		since = *row.pendingSince
		if at.Sub(since) > continuityRetention {
			// The pending interval is older than any evidence ingest can still
			// replay. It cannot be continuous evidence; restart it instead of
			// letting a pruned continuity barrier manufacture elapsed time.
			since = at
		}
	}
	if since.After(at) {
		// A pending interval can only start from evidence at or before this
		// evaluation. Clock skew or an older queued receipt restarts it
		// instead of accumulating fabricated time.
		since = at
	}
	if at.Sub(since) >= time.Duration(rule.durationMs)*time.Millisecond {
		return e.openIncidentTx(tx, row, rule, value, at)
	}
	pendingSince := since
	return updateRuleStateTx(tx, row.id, storage.AlertStatePending, &pendingSince, nil, value, at)
}

func (e *Engine) openIncidentTx(tx *storage.Tx, row ruleRow, rule validatedRule, value *float64, at time.Time) error {
	existing, err := openIncidentTx(tx, row.id)
	if err != nil {
		return err
	}
	if existing != nil {
		return updateRuleStateTx(tx, row.id, storage.AlertStateFiring, nil, &existing.id, value, at)
	}
	incidentID, err := newUUID()
	if err != nil {
		return err
	}
	incident := incidentRow{
		id:             incidentID,
		ruleID:         row.id,
		ruleName:       row.name,
		machineID:      row.machineID,
		metric:         row.metric,
		resourceID:     row.resourceID,
		condition:      row.condition,
		status:         storage.IncidentOpen,
		startedAt:      at,
		startedValue:   value,
		lastObservedAt: at,
		message:        incidentMessage(rule, value),
	}
	if err := insertIncidentTx(tx, incident); err != nil {
		// The unique partial index is the last line of defence for the
		// one-open-incident invariant.
		if storage.CodeOf(err) == storage.CodeConflict {
			open, lookupErr := openIncidentTx(tx, row.id)
			if lookupErr != nil {
				return lookupErr
			}
			if open != nil {
				return updateRuleStateTx(tx, row.id, storage.AlertStateFiring, nil, &open.id, value, at)
			}
		}
		return err
	}
	if err := updateRuleStateTx(tx, row.id, storage.AlertStateFiring, nil, &incidentID, value, at); err != nil {
		return err
	}
	if err := e.enqueueJobTx(tx, rule, incident, "fired", value, at); err != nil {
		return err
	}
	return trimIncidentsTx(tx, e.maxIncidents)
}

func (e *Engine) recoverTx(tx *storage.Tx, row ruleRow, rule validatedRule, value *float64, at time.Time) error {
	if row.activeIncidentID == nil {
		return updateRuleStateTx(tx, row.id, storage.AlertStateOK, nil, nil, value, at)
	}
	if err := resolveIncidentTx(tx, *row.activeIncidentID, at, value); err != nil {
		return err
	}
	incident, err := incidentByIDTx(tx, *row.activeIncidentID)
	if err != nil {
		return err
	}
	if err := updateRuleStateTx(tx, row.id, storage.AlertStateOK, nil, nil, value, at); err != nil {
		return err
	}
	if incident != nil {
		return e.enqueueJobTx(tx, rule, *incident, "recovered", value, at)
	}
	return nil
}

func (e *Engine) closeIncidentSilentlyTx(tx *storage.Tx, row ruleRow, at time.Time) error {
	if row.activeIncidentID != nil {
		if err := resolveIncidentTx(tx, *row.activeIncidentID, at, row.lastValue); err != nil {
			return err
		}
	}
	if row.state != storage.AlertStateOK || row.activeIncidentID != nil {
		return updateRuleStateTx(tx, row.id, storage.AlertStateOK, nil, nil, row.lastValue, at)
	}
	return nil
}

func (e *Engine) observationForTx(tx *storage.Tx, row ruleRow, payload protocol.MetricsPayload, at time.Time) (observation, error) {
	metric := api.AlertMetric(row.metric)
	for _, point := range storage.Flatten(payload, nil) {
		if point.Metric != row.metric || point.ResourceID != row.resourceID {
			continue
		}
		if row.resourceID != "" {
			if err := markObservedTx(tx, row.id, row.metric, row.resourceID, at); err != nil {
				return observation{}, err
			}
		}
		if point.Value == nil {
			return observation{state: observationUnavailable}, nil
		}
		return observation{state: observationValue, value: point.Value}, nil
	}
	if familyUnavailable(metric, payload) {
		// The rule has now observed this resource in the failed family; when
		// the family returns without the resource it must stay unavailable.
		if row.resourceID != "" {
			if err := markObservedTx(tx, row.id, row.metric, row.resourceID, at); err != nil {
				return observation{}, err
			}
		}
		return observation{state: observationUnavailable}, nil
	}
	observed, err := e.wasResourceObservedTx(tx, row)
	if err != nil {
		return observation{}, err
	}
	if observed {
		return observation{state: observationUnavailable}, nil
	}
	return observation{state: observationAbsent}, nil
}

func (e *Engine) wasResourceObservedTx(tx *storage.Tx, row ruleRow) (bool, error) {
	if row.resourceID == "" {
		return false, nil
	}
	seen, err := observedExistsTx(tx, row.id, row.metric, row.resourceID)
	if err != nil || seen {
		return seen, err
	}
	return seriesExistsTx(tx, row.machineID, row.metric, row.resourceID)
}

func (e *Engine) enqueueJobTx(tx *storage.Tx, rule validatedRule, incident incidentRow, event string, value *float64, at time.Time) error {
	jobID := "alert-" + sha256Hex(incident.id + "\x00" + event)[:40]
	payload := struct {
		Event string `json:"event"`
		JobID string `json:"jobId"`
		Alert struct {
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			MachineID  string   `json:"machineId"`
			Metric     string   `json:"metric"`
			ResourceID string   `json:"resourceId"`
			Condition  string   `json:"condition"`
			Threshold  *float64 `json:"threshold"`
			DurationMs int64    `json:"durationMs"`
		} `json:"alert"`
		Incident struct {
			ID         string  `json:"id"`
			Status     string  `json:"status"`
			StartedAt  string  `json:"startedAt"`
			ResolvedAt *string `json:"resolvedAt"`
		} `json:"incident"`
		ObservedValue *float64 `json:"observedValue"`
		OccurredAt    string   `json:"occurredAt"`
		Delivery      string   `json:"delivery"`
	}{
		Event:         event,
		JobID:         jobID,
		ObservedValue: value,
		OccurredAt:    formatTime(at),
		Delivery:      string(api.DeliveryAtLeastOnceValue),
	}
	payload.Alert.ID = rule.id
	payload.Alert.Name = rule.name
	payload.Alert.MachineID = rule.machineID
	payload.Alert.Metric = string(rule.metric)
	payload.Alert.ResourceID = rule.resourceID
	payload.Alert.Condition = string(rule.condition)
	payload.Alert.Threshold = cloneFloatPtr(rule.threshold)
	payload.Alert.DurationMs = rule.durationMs
	payload.Incident.ID = incident.id
	payload.Incident.Status = incident.status
	payload.Incident.StartedAt = formatTime(incident.startedAt)
	payload.Incident.ResolvedAt = formatTimePtr(incident.resolvedAt)
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(body) > maxPayloadBytes {
		return nil
	}
	if err := insertJobTx(tx, jobRow{
		id:          jobID,
		ruleID:      payload.Alert.ID,
		incidentID:  incident.id,
		machineID:   rule.machineID,
		event:       event,
		status:      jobPending,
		maxAttempts: int64(e.maxAttempts),
		createdAt:   at,
		payloadJSON: string(body),
	}); err != nil {
		return err
	}
	return trimJobsTx(tx, e.maxJobs)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func incidentMessage(rule validatedRule, value *float64) string {
	spec := alertMetrics[rule.metric]
	label := metricLabel(rule.metric, rule.resourceID)
	switch rule.condition {
	case api.AlertConditionStale:
		return rule.name + ": no receipt from " + rule.machineID + " within the stale threshold"
	case api.AlertConditionUnavailable:
		return rule.name + ": " + label + " unavailable on " + rule.machineID + " for " + formatDuration(rule.durationMs)
	default:
		observed := formatMetricValue(spec.unit, value)
		threshold := formatMetricValue(spec.unit, rule.threshold)
		return rule.name + ": " + label + " " + observed + " is " + conditionPhrase(rule.condition) + " " + threshold + " on " + rule.machineID
	}
}

func conditionIsTrue(rule validatedRule, obs observation) bool {
	if rule.condition == api.AlertConditionUnavailable {
		return obs.state == observationUnavailable
	}
	if obs.state != observationValue || rule.threshold == nil || obs.value == nil {
		return false
	}
	if rule.condition == api.AlertConditionAbove {
		return *obs.value >= *rule.threshold
	}
	return *obs.value <= *rule.threshold
}

func familyUnavailable(metric api.AlertMetric, payload protocol.MetricsPayload) bool {
	switch metricFamily(metric) {
	case "cpu":
		return unavailableVariant(payload.CPU)
	case "memory":
		return unavailableVariant(payload.Memory)
	case "swap":
		return unavailableVariant(payload.Swap)
	case "gpus":
		return unavailableVariant(payload.GPUs)
	case "disks":
		return unavailableVariant(payload.Disks)
	default:
		return unavailableVariant(payload.Network)
	}
}

func unavailableVariant(value any) bool {
	switch typed := value.(type) {
	case protocol.Unavailable:
		return true
	case *protocol.Unavailable:
		return typed != nil
	default:
		return false
	}
}

// -- notifications -------------------------------------------------------

// ProcessNotifications drains due delivery jobs synchronously. Observe and Tick
// schedule the same work in the background; the composition root can call this
// from a one-second loop like the reference hub's tick.
func (e *Engine) ProcessNotifications(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if e.closed.Load() {
		return nil
	}
	for {
		e.notifyMu.Lock()
		if !e.notifyRunning {
			e.notifyRunning = true
			e.notifyAgain = false
			e.notifyDone = make(chan struct{})
			e.notifyMu.Unlock()
			return e.runNotificationPass(ctx)
		}
		done := e.notifyDone
		e.notifyMu.Unlock()
		select {
		case <-done:
			// The previous owner finished. Loop to become the owner and run
			// one more pass so this synchronous caller observes a completed
			// pass.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// runNotificationPass is the single owner loop for notification delivery. A
// repeat request only sets notifyAgain; the owner re-runs while it is dirty.
// Ownership is always released on return, including an error or panic, so a
// later caller can never wait forever on a pass that already exited.
func (e *Engine) runNotificationPass(ctx context.Context) (err error) {
	released := false
	defer func() {
		if released {
			return
		}
		e.notifyMu.Lock()
		e.notifyRunning = false
		e.notifyAgain = false
		close(e.notifyDone)
		e.notifyMu.Unlock()
	}()
	for {
		if err = e.runNotifications(ctx); err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		e.notifyMu.Lock()
		if e.notifyAgain && !e.closed.Load() {
			e.notifyAgain = false
			e.notifyMu.Unlock()
			continue
		}
		e.notifyRunning = false
		e.notifyAgain = false
		close(e.notifyDone)
		released = true
		e.notifyMu.Unlock()
		return nil
	}
}

func (e *Engine) scheduleNotifications() {
	if e.closed.Load() {
		return
	}
	e.notifyMu.Lock()
	if e.notifyRunning {
		e.notifyAgain = true
		e.notifyMu.Unlock()
		return
	}
	e.notifyRunning = true
	e.notifyAgain = false
	e.notifyDone = make(chan struct{})
	e.notifyMu.Unlock()
	e.background.Add(1)
	go func() {
		defer e.background.Done()
		if err := e.runNotificationPass(context.Background()); err != nil && !e.closed.Load() {
			e.logger.Warn("alert notification run failed", "error", err)
		}
	}()
}

func (e *Engine) runNotifications(ctx context.Context) error {
	if e.closed.Load() {
		return nil
	}
	jobs, err := dueJobsDB(ctx, e.db, e.nowTime())
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	webhook, err := loadWebhookDB(ctx, e.db)
	if err != nil {
		return err
	}
	if webhook == nil || !webhook.enabled {
		retryAt := e.nowTime().Add(missingWebhookRetryMs * time.Millisecond)
		return e.db.Write(ctx, func(tx *storage.Tx) error {
			for _, job := range jobs {
				if err := postponeJobTx(tx, job.id, retryAt, "webhook not configured"); err != nil {
					return err
				}
			}
			return nil
		})
	}
	headers := map[string]string{}
	if webhook.headersJSON != "" {
		var stored map[string]string
		if err := json.Unmarshal([]byte(webhook.headersJSON), &stored); err == nil {
			if parsed, parseErr := parseWebhookHeaders(stored); parseErr == nil {
				headers = parsed
			}
		}
	}
	for _, job := range jobs {
		if e.closed.Load() {
			return nil
		}
		attempts := job.attempts + 1
		result, sendErr := e.sendWebhook(ctx, WebhookRequest{
			URL:     webhook.url,
			Headers: headers,
			Secret:  webhook.secret,
			Body:    []byte(job.payloadJSON),
			JobID:   job.id,
			Timeout: e.deliveryTimeout,
		})
		if e.closed.Load() {
			return nil
		}
		attemptAt := e.nowTime()
		if sendErr == nil && result.OK {
			job.status = jobDelivered
			job.attempts = attempts
			job.nextAttempt = nil
			job.lastAttempt = &attemptAt
			job.lastError = nil
			job.deliveredAt = &attemptAt
		} else {
			message := result.Error
			if sendErr != nil {
				message = sanitizeWebhookError(sendErr)
			}
			if message == "" {
				message = "delivery failed"
			}
			message = redactDeliveryError(message, webhook.url, headers, webhook.secret)
			job.attempts = attempts
			job.lastAttempt = &attemptAt
			job.lastError = &message
			if attempts >= job.maxAttempts {
				job.status = jobFailed
				job.nextAttempt = nil
			} else {
				job.status = jobPending
				backoff := retryBackoff(attempts)
				next := attemptAt.Add(backoff)
				job.nextAttempt = &next
			}
		}
		if err := e.db.Write(ctx, func(tx *storage.Tx) error { return updateJobTx(tx, job) }); err != nil {
			return err
		}
	}
	return nil
}

func retryBackoff(attempts int64) time.Duration {
	power := attempts - 1
	if power < 0 {
		power = 0
	}
	if power > 20 {
		power = 20
	}
	delay := int64(retryBaseMs) << uint(power)
	if delay > retryCapMs {
		delay = retryCapMs
	}
	return time.Duration(delay) * time.Millisecond
}

func (e *Engine) sendWebhook(_ context.Context, request WebhookRequest) (result WebhookResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.New("webhook sender panicked")
			result = WebhookResult{}
		}
	}()
	// Delivery is bounded by the engine's own deadline and aborts on Stop, like
	// the reference sender. A caller context is not allowed to cut a delivery
	// short after the request has been accepted.
	requestCtx, cancel := context.WithTimeout(e.baseCtx, e.deliveryTimeout)
	defer cancel()
	return e.sender.Send(requestCtx, request), nil
}

// -- webhook configuration ----------------------------------------------

// WebhookConfig is one configure request.
type WebhookConfig struct {
	URL     string
	Headers map[string]string
	Secret  *string
	Enabled *bool
}

// WebhookStatus returns the secret-free configured channel state.
func (e *Engine) WebhookStatus(ctx context.Context) (api.AlertWebhookStatus, error) {
	row, err := loadWebhookDB(ctx, e.db)
	if err != nil {
		return api.AlertWebhookStatus{}, err
	}
	status := api.AlertWebhookStatus{
		Channel:  api.WebhookChannelValue,
		Delivery: api.DeliveryAtLeastOnceValue,
	}
	if row == nil {
		return status, nil
	}
	status.Configured = true
	status.Enabled = row.enabled
	if parsed, parseErr := parseWebhookURL(row.url); parseErr == nil {
		transport := parsed.transport
		status.Transport = &transport
	}
	status.LastTestAt = formatTimePtr(row.lastTestAt)
	status.LastTestOK = cloneBoolPtr(row.lastTestOK)
	status.LastError = cloneStringPtr(row.lastError)
	return status, nil
}

// ConfigureWebhook validates and persists the single webhook channel. The
// originating session is re-checked inside the write transaction.
func (e *Engine) ConfigureWebhook(ctx context.Context, authority SessionAuthority, config WebhookConfig) (api.AlertWebhookStatus, error) {
	parsed, err := parseWebhookURL(config.URL)
	if err != nil {
		return api.AlertWebhookStatus{}, err
	}
	headers, err := parseWebhookHeaders(config.Headers)
	if err != nil {
		return api.AlertWebhookStatus{}, err
	}
	secret, err := parseWebhookSecret(config.Secret)
	if err != nil {
		return api.AlertWebhookStatus{}, err
	}
	enabled := true
	if config.Enabled != nil {
		enabled = *config.Enabled
	}
	encoded, err := json.Marshal(headers)
	if err != nil {
		return api.AlertWebhookStatus{}, err
	}
	if err := e.db.Write(ctx, func(tx *storage.Tx) error {
		if _, err := e.validateMutationAuthority(tx, authority); err != nil {
			return err
		}
		return upsertWebhookTx(tx, webhookRow{
			url:         parsed.url,
			headersJSON: string(encoded),
			secret:      secret,
			enabled:     enabled,
			updatedAt:   e.nowTime(),
		})
	}); err != nil {
		return api.AlertWebhookStatus{}, err
	}
	return e.WebhookStatus(ctx)
}

// ClearWebhook removes the channel. Clearing an absent channel is fine. The
// originating session is re-checked inside the delete transaction.
func (e *Engine) ClearWebhook(ctx context.Context, authority SessionAuthority) (api.AlertWebhookStatus, error) {
	if err := e.db.Write(ctx, func(tx *storage.Tx) error {
		if _, err := e.validateMutationAuthority(tx, authority); err != nil {
			return err
		}
		return deleteWebhookTx(tx)
	}); err != nil {
		return api.AlertWebhookStatus{}, err
	}
	return e.WebhookStatus(ctx)
}

// TestWebhook delivers one explicit test to a configured loopback HTTP sink.
// An HTTPS webhook is exercised by the background delivery job instead. The
// originating session is re-checked immediately before the outbound request, so
// a revoked session cannot make the hub send.
func (e *Engine) TestWebhook(ctx context.Context, authority SessionAuthority) (api.AlertWebhookTestResult, error) {
	testedAt := e.nowTime()
	result := api.AlertWebhookTestResult{TestedAt: formatTime(testedAt)}
	if e.closed.Load() {
		message := "alerts are shutting down"
		result.Error = &message
		return result, nil
	}
	row, err := loadWebhookDB(ctx, e.db)
	if err != nil {
		return result, err
	}
	if row == nil {
		message := "webhook is not configured"
		result.Error = &message
		return result, nil
	}
	if !row.enabled {
		message := "webhook is disabled"
		result.Error = &message
		return result, nil
	}
	parsed, err := parseWebhookURL(row.url)
	if err != nil {
		message := "webhook url is invalid"
		result.Error = &message
		return result, nil
	}
	if parsed.transport != api.AlertWebhookTransportLoopbackHTTP {
		message := "test delivery is restricted to a loopback HTTP sink"
		result.Error = &message
		if err := e.recordTest(ctx, testedAt, false, &message); err != nil {
			return result, err
		}
		return result, nil
	}
	body, err := json.Marshal(struct {
		Event    string `json:"event"`
		TestedAt string `json:"testedAt"`
		Text     string `json:"text"`
		Delivery string `json:"delivery"`
	}{
		Event:    "test",
		TestedAt: formatTime(testedAt),
		Text:     "Rigscope webhook test. This is an explicit admin action.",
		Delivery: string(api.DeliveryAtLeastOnceValue),
	})
	if err != nil {
		return result, err
	}
	headers := map[string]string{}
	if row.headersJSON != "" {
		var stored map[string]string
		headersInvalid := false
		if err := json.Unmarshal([]byte(row.headersJSON), &stored); err != nil {
			headersInvalid = true
		} else if parsedHeaders, parseErr := parseWebhookHeaders(stored); parseErr != nil {
			headersInvalid = true
		} else {
			headers = parsedHeaders
		}
		if headersInvalid {
			message := "delivery failed"
			result.Error = &message
			if err := e.recordTest(ctx, testedAt, false, result.Error); err != nil {
				return result, err
			}
			return result, nil
		}
	}
	jobID := "alert-test-" + sha256Hex(row.url + "\x00" + strconv.FormatInt(row.updatedAt.UnixMilli(), 10))[:24]
	// The send is the side effect; validate the session in a transaction
	// immediately before it so logout or revocation that happened while the
	// webhook row and body were being prepared refuses the delivery.
	if err := e.db.Write(ctx, func(tx *storage.Tx) error {
		_, err := e.validateMutationAuthority(tx, authority)
		return err
	}); err != nil {
		return result, err
	}
	delivery, sendErr := e.sendWebhook(ctx, WebhookRequest{
		URL:     row.url,
		Headers: headers,
		Secret:  row.secret,
		Body:    body,
		JobID:   jobID,
		Timeout: e.deliveryTimeout,
	})
	message := delivery.Error
	if sendErr != nil {
		message = sanitizeWebhookError(sendErr)
	}
	if message == "" && !delivery.OK {
		message = "delivery failed"
	}
	if message != "" {
		message = redactDeliveryError(message, row.url, headers, row.secret)
		result.Error = &message
	}
	result.OK = delivery.OK
	if delivery.Status != nil {
		status := int64(*delivery.Status)
		result.Status = &status
	}
	if err := e.recordTest(ctx, testedAt, delivery.OK, result.Error); err != nil {
		return result, err
	}
	return result, nil
}

func (e *Engine) recordTest(ctx context.Context, at time.Time, ok bool, message *string) error {
	if e.closed.Load() {
		return nil
	}
	return recordWebhookTestDB(ctx, e.db, at, ok, message)
}

func cloneBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

// -- error redaction -----------------------------------------------------

var (
	urlPattern      = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
	bearerPattern   = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	authLinePattern = regexp.MustCompile(`(?i)\b(authorization|proxy-authorization)\b[^\n]*`)
)

// redactDeliveryError removes the configured URL, header values and secret
// plus URL-shaped and bearer-shaped fragments before persistence.
func redactDeliveryError(raw, configuredURL string, headers map[string]string, secret *string) string {
	text := raw
	sensitive := make([]string, 0, len(headers)+4)
	sensitive = append(sensitive, configuredURL)
	if secret != nil {
		sensitive = append(sensitive, *secret)
	}
	for _, value := range headers {
		sensitive = append(sensitive, value)
	}
	if parsed, err := url.Parse(configuredURL); err == nil {
		if parsed.Path != "" && parsed.Path != "/" {
			sensitive = append(sensitive, parsed.Path)
		}
		for _, values := range parsed.Query() {
			sensitive = append(sensitive, values...)
		}
	}
	filtered := sensitive[:0]
	for _, value := range sensitive {
		if value != "" {
			filtered = append(filtered, value)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool { return len(filtered[i]) > len(filtered[j]) })
	for _, value := range filtered {
		text = strings.ReplaceAll(text, value, "[redacted]")
	}
	text = urlPattern.ReplaceAllString(text, "[redacted-url]")
	text = bearerPattern.ReplaceAllString(text, "Bearer [redacted]")
	text = authLinePattern.ReplaceAllString(text, "${1} [redacted]")
	return capError(text)
}

func capError(value string) string {
	clean := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value))
	if utf16Len(clean) <= maxErrorText {
		return clean
	}
	var builder strings.Builder
	units := 0
	for _, r := range clean {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > maxErrorText-3 {
			break
		}
		builder.WriteRune(r)
		units += width
	}
	return builder.String() + "..."
}
