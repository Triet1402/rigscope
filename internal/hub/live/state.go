package live

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/protocol"
)

// State errors classify a rejected collector session so the transport maps
// them to the documented WebSocket close codes.
var (
	ErrUnknownCollector = errors.New("unconfigured collector")
	ErrIdentityMismatch = errors.New("collector identity mismatch")
	ErrSessionConflict  = errors.New("session conflict")
)

// StartSessionInput is one authenticated hello.
type StartSessionInput struct {
	SessionID   string
	CollectorID string
	// CredentialHash is the credential that authenticated this session. A
	// delayed replacement invalidation compares it so only the old credential's
	// sockets are fenced.
	CredentialHash     string
	RunID              string
	BootID             string
	Hostname           string
	ExpectedIntervalMS int64
}

// StartSessionResult is the outcome of one authenticated hello.
//
// ReplacedSessionID names the exact predecessor session the transition
// superseded, or "" when no session was open. The transport closes that one
// socket and nothing else, so two concurrent hellos for one collector can
// never terminate the newest session they just created.
type StartSessionResult struct {
	Session           SessionInfo
	ReplacedSessionID string
}

// SessionInfo is the authenticated session the transport and ingest use.
type SessionInfo struct {
	SessionID          string
	CollectorID        string
	MachineID          string
	RunID              string
	BootID             string
	Hostname           string
	ExpectedIntervalMS int64
}

// MachineView is the read-only machine summary the process-detail handler
// needs. Inventory is nil before the collector has sent one.
type MachineView struct {
	MachineID   string
	CollectorID string
	Hostname    string
	Connected   bool
	Inventory   *protocol.Inventory
}

// MachineTelemetry is the liveness view of one machine. The alert engine uses
// the last receipt time, the advertised cadence and the latest live payload for
// stale and unavailable rules. It is deliberately independent of the browser
// snapshot type so the composition root can adapt it without importing live
// types into alerts.
type MachineTelemetry struct {
	MachineID string
	// LastReceivedAt is the last live receipt, or nil before the first record.
	LastReceivedAt *time.Time
	// ExpectedIntervalMS is the collector's advertised cadence, or nil when
	// unknown.
	ExpectedIntervalMS *int64
	// Metrics is the last known live payload, or nil before the first record.
	Metrics *protocol.MetricsPayload
	// MetricsReceivedAt is the hub receipt time of Metrics.
	MetricsReceivedAt *time.Time
}

type machineRow struct {
	machineID   string
	collectorID string
	// credentialHash names the collector credential that currently owns this
	// mapping. It is empty when the mapping was restored without a credential,
	// for example at process start before the collector reconnects.
	credentialHash string
	hostname       string

	sessionID string
	runID     string
	bootID    string

	lastSequence int64
	hasSequence  bool
	lastReceived *string

	expectedIntervalMS *int64

	metrics   *api.ReceivedMetrics
	inventory *protocol.Inventory
	delivery  *protocol.StatusMessage

	displayName *protocol.Optional[string]
	tags        *[]string
}

// Change is one state invalidation. Consumers coalesce reads of Snapshot; it
// carries no data so a burst of mutations needs one delivery.
type Change struct{}

// CredentialInvalidation names the exact collector credential that is no longer
// current and the machine mapping it owned. Collector ids can be reused (a
// replacement enrollment commonly keeps the id, and a later enrollment may map
// it to a different machine), so a collector-id-only message would fence a
// legitimate new credential. TokenHash is the invalidated credential and
// MachineID is the mapping observed when it was superseded.
type CredentialInvalidation struct {
	CollectorID string
	MachineID   string
	TokenHash   string
}

// State is the authoritative in-memory fleet state of one hub process run. It
// is safe for concurrent use.
type State struct {
	hubRunID string
	now      func() time.Time

	mu          sync.RWMutex
	rows        map[string]*machineRow // by machineID
	byCollector map[string]*machineRow
	bySession   map[string]*machineRow
	listeners   map[chan Change]struct{}
	// credentialListeners observe collector credential invalidation. The
	// gateway subscribes so a replacement enrollment or revocation can fence
	// and close only the sessions authenticated by the old credential.
	credentialListeners    map[uint64]func(CredentialInvalidation)
	nextCredentialListener uint64
	// droppedSessions remembers active sessions whose row a replacement
	// enrollment forgot, keyed by session id. The superseded socket still
	// owns the disconnect: when it ends, EndSession reports the machine once,
	// unless a successor session is already live for it or a removal already
	// reported it. Entries are bounded by open collector connections.
	droppedSessions map[string]string
}

// NewState creates an empty fleet state.
func NewState(hubRunID string, now func() time.Time) *State {
	if now == nil {
		now = time.Now
	}
	return &State{
		hubRunID:            hubRunID,
		now:                 now,
		rows:                make(map[string]*machineRow),
		byCollector:         make(map[string]*machineRow),
		bySession:           make(map[string]*machineRow),
		listeners:           make(map[chan Change]struct{}),
		credentialListeners: make(map[uint64]func(CredentialInvalidation)),
		droppedSessions:     make(map[string]string),
	}
}

// HubRunID is the per-process identifier carried by snapshots and event ids.
func (s *State) HubRunID() string { return s.hubRunID }

// RegisterMachine makes an enrolled collector visible before it connects.
// Re-registering the same mapping is a no-op so restart recovery and a fresh
// enrollment can both call it.
func (s *State) RegisterMachine(collectorID, machineID string) error {
	return s.RegisterMachineWithCredential(collectorID, machineID, "")
}

// RegisterMachineWithCredential is RegisterMachine plus the credential hash
// that currently owns the mapping. A replacement enrollment and the gateway
// admission both call it so a delayed invalidation can tell whether the mapping
// still belongs to the credential it names.
func (s *State) RegisterMachineWithCredential(collectorID, machineID, credentialHash string) error {
	if collectorID == "" || machineID == "" {
		return fmt.Errorf("live: collector and machine ids are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[machineID]; ok {
		if row.collectorID != collectorID {
			return fmt.Errorf("live: machine %q already belongs to collector %q", machineID, row.collectorID)
		}
		row.credentialHash = credentialHash
		return nil
	}
	if row, ok := s.byCollector[collectorID]; ok {
		if row.machineID != machineID {
			return fmt.Errorf("live: collector %q already maps to machine %q", collectorID, row.machineID)
		}
		row.credentialHash = credentialHash
		return nil
	}
	row := &machineRow{machineID: machineID, collectorID: collectorID, credentialHash: credentialHash}
	s.rows[machineID] = row
	s.byCollector[collectorID] = row
	s.emitLocked()
	return nil
}

// ReplaceMachine removes a machine mapping (a replacement enrollment changed
// the hub-local machine id) and drops its live state.
func (s *State) ReplaceMachine(collectorID, machineID string) {
	s.ReplaceMachineWithCredential(collectorID, machineID, "")
}

// ReplaceMachineWithCredential is ReplaceMachine plus the credential hash that
// now owns the mapping.
func (s *State) ReplaceMachineWithCredential(collectorID, machineID, credentialHash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A replacement changes both sides of the mapping: the collector id may
	// name the previous row and the machine id may still be owned by the
	// superseded collector. Both indexes must forget their old row. Leaving a
	// stale byCollector entry behind would make a later delayed invalidation
	// follow it and delete the fresh row through s.rows[machineID].
	if row, ok := s.byCollector[collectorID]; ok {
		s.dropSessionLocked(row)
		s.forgetLocked(row)
	}
	if row, ok := s.rows[machineID]; ok {
		s.dropSessionLocked(row)
		s.forgetLocked(row)
	}
	row := &machineRow{machineID: machineID, collectorID: collectorID, credentialHash: credentialHash}
	s.rows[machineID] = row
	s.byCollector[collectorID] = row
	s.emitLocked()
}

// RemoveCollector drops a revoked collector's live mapping together with its
// readings, inventory and delivery status. Revocation uses it so a machine
// whose credential is gone disappears from the fleet snapshot instead of
// lingering as an offline row with data the hub no longer trusts. The removal
// is a normal state change, so SSE subscribers publish it.
func (s *State) RemoveCollector(collectorID string) (machineID string, removed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.byCollector[collectorID]
	if !ok {
		return "", false
	}
	machineID = row.machineID
	// The collector index can outlive the row it names when a replacement
	// re-registered the machine under another collector. Only the row the
	// machine index still names may disappear, and only then does the caller
	// fire a disconnect callback for it.
	removed = s.rows[machineID] == row
	s.forgetLocked(row)
	if removed {
		// The caller reports this machine's disconnect; a superseded socket
		// ending later must not report it again.
		s.forgetDroppedLocked(machineID)
	}
	s.emitLocked()
	return machineID, removed
}

// RemoveCollectorIf removes a collector's mapping only when it still points to
// machineID and still belongs to credentialHash (or either is unknown). A
// delayed replacement invalidation must not remove a mapping that a newer
// credential has already claimed, nor one where the reused collector id now
// serves a different machine.
func (s *State) RemoveCollectorIf(collectorID, machineID, credentialHash string) (removedMachineID string, removed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.byCollector[collectorID]
	if !ok || row.machineID != machineID {
		return "", false
	}
	if credentialHash != "" && row.credentialHash != "" && row.credentialHash != credentialHash {
		return "", false
	}
	if s.rows[row.machineID] != row {
		// The collector index is stale: the machine now belongs to a newer
		// row. Drop only the stale index entry. Deleting s.rows[machineID]
		// here would remove the successor's live state and publish a
		// disconnect for a machine that is still present.
		s.forgetLocked(row)
		return "", false
	}
	s.forgetLocked(row)
	s.forgetDroppedLocked(row.machineID)
	s.emitLocked()
	return row.machineID, true
}

// StartSession opens the authenticated session for one collector. A different
// run supersedes the previous session only here, after an authenticated hello.
// A connection claiming the identity's current run while another session holds
// it is a conflicting simultaneous session and is rejected.
func (s *State) StartSession(input StartSessionInput) (StartSessionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.byCollector[input.CollectorID]
	if !ok {
		return StartSessionResult{}, ErrUnknownCollector
	}
	if row.runID == input.RunID && row.bootID != input.BootID {
		return StartSessionResult{}, ErrIdentityMismatch
	}
	predecessor := ""
	if row.sessionID != "" {
		if row.runID == input.RunID {
			return StartSessionResult{}, ErrSessionConflict
		}
		predecessor = row.sessionID
		if current, ok := s.bySession[row.sessionID]; ok && current == row {
			delete(s.bySession, row.sessionID)
		}
		row.sessionID = ""
	}
	if row.runID != input.RunID {
		row.lastSequence = 0
		row.hasSequence = false
		row.lastReceived = nil
		row.metrics = nil
		row.delivery = nil
	}
	expected := input.ExpectedIntervalMS
	row.sessionID = input.SessionID
	row.credentialHash = input.CredentialHash
	row.runID = input.RunID
	row.bootID = input.BootID
	row.hostname = input.Hostname
	row.expectedIntervalMS = &expected
	s.bySession[input.SessionID] = row
	s.emitLocked()
	return StartSessionResult{Session: row.session(), ReplacedSessionID: predecessor}, nil
}

// EndSession marks the session closed. Last known readings, hostname and the
// hello-advertised cadence are retained while the machine is offline.
func (s *State) EndSession(sessionID string) (machineID string, ended bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.bySession[sessionID]
	if !ok || row.sessionID != sessionID {
		if machineID, dropped := s.droppedSessions[sessionID]; dropped {
			// A replacement enrollment forgot this session's row while the
			// socket was still open. Its viewers learn the machine went away
			// now, exactly once, unless a successor session already serves it.
			delete(s.droppedSessions, sessionID)
			if current, ok := s.rows[machineID]; ok && current.sessionID != "" {
				return "", false
			}
			return machineID, true
		}
		return "", false
	}
	row.sessionID = ""
	delete(s.bySession, sessionID)
	s.emitLocked()
	return row.machineID, true
}

// CurrentSession resolves a session id to its live session, or ok=false when
// the session was replaced or ended.
func (s *State) CurrentSession(sessionID string) (SessionInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.bySession[sessionID]
	if !ok || row.sessionID != sessionID {
		return SessionInfo{}, false
	}
	return row.session(), true
}

// ApplyMetrics publishes one LIVE record while its session is current and the
// record is newer than the last accepted sequence. It reports whether the
// record was applied, so callers do not repeat live-only side effects.
func (s *State) ApplyMetrics(sessionID string, record protocol.MetricsRecord, receivedAt time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.bySession[sessionID]
	if !ok || row.sessionID != sessionID {
		return false
	}
	if row.runID != record.RunID || row.bootID != record.BootID || row.collectorID != record.CollectorID {
		return false
	}
	if row.hasSequence && record.Sequence <= row.lastSequence {
		return false
	}
	received := formatTimestamp(receivedAt)
	row.lastSequence = record.Sequence
	row.hasSequence = true
	row.lastReceived = &received
	row.metrics = &api.ReceivedMetrics{
		CollectorID: record.CollectorID,
		RunID:       record.RunID,
		BootID:      record.BootID,
		Sequence:    record.Sequence,
		CapturedAt:  record.CapturedAt,
		Kind:        string(protocol.RecordKindMetrics),
		Payload:     record.Payload,
		ReceivedAt:  received,
	}
	s.emitLocked()
	return true
}

// RestoreMetrics restores a persisted latest record without implying that the
// collector is connected or manufacturing a new receipt.
func (s *State) RestoreMetrics(machineID string, record protocol.MetricsRecord, receivedAt time.Time, hostname *string, expectedIntervalMS *int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[machineID]
	if !ok || row.collectorID != record.CollectorID || row.sessionID != "" {
		return
	}
	row.runID = record.RunID
	row.bootID = record.BootID
	row.lastSequence = record.Sequence
	row.hasSequence = true
	if hostname != nil {
		row.hostname = *hostname
	}
	if expectedIntervalMS != nil {
		value := *expectedIntervalMS
		row.expectedIntervalMS = &value
	}
	received := formatTimestamp(receivedAt)
	row.lastReceived = &received
	row.metrics = &api.ReceivedMetrics{
		CollectorID: record.CollectorID,
		RunID:       record.RunID,
		BootID:      record.BootID,
		Sequence:    record.Sequence,
		CapturedAt:  record.CapturedAt,
		Kind:        string(protocol.RecordKindMetrics),
		Payload:     record.Payload,
		ReceivedAt:  received,
	}
	s.emitLocked()
}

// SetInventory stores a machine's inventory. Restart recovery and resync use
// it because they have no authenticated session to bind to.
func (s *State) SetInventory(machineID string, inventory protocol.Inventory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[machineID]
	if !ok {
		return
	}
	value := inventory
	row.inventory = &value
	s.emitLocked()
}

// SetSessionInventory stores inventory for a session's machine only while that
// session is still the current authenticated one. A collector message that
// raced a session replacement therefore cannot overwrite the newer session's
// inventory. It reports whether the value was applied.
func (s *State) SetSessionInventory(sessionID string, inventory protocol.Inventory) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.bySession[sessionID]
	if !ok || row.sessionID != sessionID {
		return false
	}
	value := inventory
	row.inventory = &value
	s.emitLocked()
	return true
}

// SetDelivery stores the latest delivery status for the session's machine.
func (s *State) SetDelivery(sessionID string, status protocol.StatusMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.bySession[sessionID]
	if !ok || row.sessionID != sessionID {
		return
	}
	value := status
	row.delivery = &value
	s.emitLocked()
}

// SetMetadata stores presentation metadata and publishes a change.
func (s *State) SetMetadata(machineID string, displayName *string, tags []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[machineID]
	if !ok {
		return
	}
	if displayName == nil {
		row.displayName = &protocol.Optional[string]{Valid: false}
	} else {
		row.displayName = &protocol.Optional[string]{Valid: true, Value: *displayName}
	}
	copied := append([]string{}, tags...)
	row.tags = &copied
	s.emitLocked()
}

// Machine returns the summary for one machine.
func (s *State) Machine(machineID string) (MachineView, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.rows[machineID]
	if !ok {
		return MachineView{}, false
	}
	return row.view(), true
}

// Machines returns every machine summary ordered by machine id.
func (s *State) Machines() []MachineView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]MachineView, 0, len(s.rows))
	for _, row := range s.rows {
		result = append(result, row.view())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].MachineID < result[j].MachineID })
	return result
}

// MachineTelemetry returns every registered machine's liveness view, ordered
// by machine id. A machine without a live record has nil receipt and metrics,
// so the alert engine can distinguish "no value yet" from an unavailable
// payload.
func (s *State) MachineTelemetry() []MachineTelemetry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]MachineTelemetry, 0, len(s.rows))
	for _, row := range s.rows {
		view := MachineTelemetry{MachineID: row.machineID}
		if row.lastReceived != nil {
			if parsed, err := time.Parse(time.RFC3339Nano, *row.lastReceived); err == nil {
				view.LastReceivedAt = &parsed
			}
		}
		if row.expectedIntervalMS != nil {
			expected := *row.expectedIntervalMS
			view.ExpectedIntervalMS = &expected
		}
		if row.metrics != nil {
			payload := row.metrics.Payload
			view.Metrics = &payload
			if parsed, err := time.Parse(time.RFC3339Nano, row.metrics.ReceivedAt); err == nil {
				view.MetricsReceivedAt = &parsed
			}
		}
		result = append(result, view)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].MachineID < result[j].MachineID })
	return result
}

// Snapshot builds the authoritative full snapshot, ordered by machine id.
func (s *State) Snapshot() api.HubSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	machines := make([]api.MachineState, 0, len(s.rows))
	for _, row := range s.rows {
		machines = append(machines, row.apiState())
	}
	sort.Slice(machines, func(i, j int) bool { return machines[i].MachineID < machines[j].MachineID })
	return api.HubSnapshot{HubRunID: s.hubRunID, Machines: machines}
}

// ConnectedCount reports how many collectors currently hold a session.
func (s *State) ConnectedCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.bySession)
}

// ConfiguredCount reports how many machines are registered.
func (s *State) ConfiguredCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.rows)
}

// SubscribeChanges returns a channel notified after every state mutation and
// the function that releases it. The channel is buffered to one invalidation:
// consumers read Snapshot and cannot lose a change by being slow.
func (s *State) SubscribeChanges() (<-chan Change, func()) {
	channel := make(chan Change, 1)
	s.mu.Lock()
	s.listeners[channel] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	return channel, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.listeners, channel)
			s.mu.Unlock()
		})
	}
}

// SubscribeCredentialInvalidation registers a listener for collector
// credentials that were replaced or revoked. It returns the function that
// releases the subscription. Listeners run synchronously outside the state
// lock.
func (s *State) SubscribeCredentialInvalidation(listener func(CredentialInvalidation)) func() {
	s.mu.Lock()
	id := s.nextCredentialListener
	s.nextCredentialListener++
	s.credentialListeners[id] = listener
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.credentialListeners, id)
			s.mu.Unlock()
		})
	}
}

// InvalidateCredential tells every subscriber that one exact collector
// credential is no longer current. The gateway uses it to fence pending
// upgrades and close live sessions authenticated by the old credential without
// touching a newer credential that reused the same collector id.
func (s *State) InvalidateCredential(invalidation CredentialInvalidation) {
	if invalidation.CollectorID == "" {
		return
	}
	s.mu.RLock()
	listeners := make([]func(CredentialInvalidation), 0, len(s.credentialListeners))
	for _, listener := range s.credentialListeners {
		listeners = append(listeners, listener)
	}
	s.mu.RUnlock()
	for _, listener := range listeners {
		listener(invalidation)
	}
}

func (s *State) emitLocked() {
	for listener := range s.listeners {
		select {
		case listener <- Change{}:
		default:
		}
	}
}

// dropSessionLocked records that a replacement is about to forget a row with an
// active session, so that session's end still reports the disconnect.
func (s *State) dropSessionLocked(row *machineRow) {
	if row.sessionID != "" {
		s.droppedSessions[row.sessionID] = row.machineID
	}
}

// forgetDroppedLocked discards dropped sessions for a machine whose disconnect
// another path has just reported.
func (s *State) forgetDroppedLocked(machineID string) {
	for sessionID, dropped := range s.droppedSessions {
		if dropped == machineID {
			delete(s.droppedSessions, sessionID)
		}
	}
}

// forgetLocked drops one row from every index that still names it. A stale
// index entry can point at a row another mapping has already superseded;
// deleting by key alone would then remove the successor's row. It reports
// whether the machine row was still current and therefore disappeared.
func (s *State) forgetLocked(row *machineRow) (removed bool) {
	if current, ok := s.rows[row.machineID]; ok && current == row {
		delete(s.rows, row.machineID)
		removed = true
	}
	if current, ok := s.byCollector[row.collectorID]; ok && current == row {
		delete(s.byCollector, row.collectorID)
	}
	if row.sessionID != "" {
		if current, ok := s.bySession[row.sessionID]; ok && current == row {
			delete(s.bySession, row.sessionID)
		}
	}
	return removed
}

func (r *machineRow) session() SessionInfo {
	expected := int64(0)
	if r.expectedIntervalMS != nil {
		expected = *r.expectedIntervalMS
	}
	return SessionInfo{
		SessionID:          r.sessionID,
		CollectorID:        r.collectorID,
		MachineID:          r.machineID,
		RunID:              r.runID,
		BootID:             r.bootID,
		Hostname:           r.hostname,
		ExpectedIntervalMS: expected,
	}
}

func (r *machineRow) view() MachineView {
	var inventory *protocol.Inventory
	if r.inventory != nil {
		copied := *r.inventory
		inventory = &copied
	}
	return MachineView{
		MachineID:   r.machineID,
		CollectorID: r.collectorID,
		Hostname:    r.hostname,
		Connected:   r.sessionID != "",
		Inventory:   inventory,
	}
}

func (r *machineRow) apiState() api.MachineState {
	state := api.MachineState{
		MachineID:      r.machineID,
		CollectorID:    r.collectorID,
		Hostname:       r.hostname,
		Connected:      r.sessionID != "",
		LastReceivedAt: r.lastReceived,
	}
	// The browser contract always carries the key: null means the cadence is
	// unknown before a hello. A non-nil pointer keeps omitempty from dropping
	// it, and an invalid Optional encodes as null.
	state.ExpectedIntervalMs = &protocol.Optional[int64]{Valid: false}
	if r.expectedIntervalMS != nil {
		value := *r.expectedIntervalMS
		state.ExpectedIntervalMs = &protocol.Optional[int64]{Valid: true, Value: value}
	}
	if r.metrics != nil {
		copied := *r.metrics
		state.Metrics = &copied
	}
	if r.inventory != nil {
		copied := *r.inventory
		state.Inventory = &copied
	}
	if r.delivery != nil {
		copied := *r.delivery
		state.Delivery = &copied
	}
	if r.displayName != nil {
		copied := *r.displayName
		state.DisplayName = &copied
	}
	if r.tags != nil {
		copied := append([]string{}, (*r.tags)...)
		state.Tags = &copied
	}
	return state
}

// formatTimestamp renders the canonical ISO-8601 millisecond form the browser
// contract uses (Date.toISOString()).
func formatTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}
