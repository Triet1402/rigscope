package delivery

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// QueueLimits bounds the RAM queue. The defaults match the documented 1.0
// collector bounds: ten minutes or 64 MiB, whichever comes first, plus a hard
// ceiling on retained gap records.
type QueueLimits struct {
	MaxAge        time.Duration
	MaxBytes      int
	MaxGapRecords int
}

// DefaultQueueLimits returns the production queue bounds.
func DefaultQueueLimits() QueueLimits {
	return QueueLimits{
		MaxAge:        10 * time.Minute,
		MaxBytes:      64 * 1024 * 1024,
		MaxGapRecords: 64,
	}
}

func (l QueueLimits) validate() error {
	if l.MaxAge <= 0 {
		return errors.New("queue: MaxAge must be positive")
	}
	if l.MaxBytes < 1024 {
		return errors.New("queue: MaxBytes must be at least 1024")
	}
	if l.MaxGapRecords < 1 || l.MaxGapRecords > 1024 {
		return errors.New("queue: MaxGapRecords must be 1..1024")
	}
	return nil
}

// QueueIdentity is the record envelope identity written into locally created
// gap records.
type QueueIdentity struct {
	CollectorID string
	RunID       string
	BootID      string
}

type queueOptions struct {
	limits   QueueLimits
	identity QueueIdentity
	allocate func() int64
	clock    func() time.Time
	// afterReserve runs after a selection has reserved its entries and
	// released the queue mutex. Tests use it to interleave an Enqueue between
	// selection and the send that follows.
	afterReserve func()
}

type entryState int

const (
	statePending entryState = iota
	stateInFlight
)

type entry struct {
	id         uint64
	runID      string
	sequence   int64
	capturedAt time.Time
	kind       protocol.RecordKind
	gapReason  protocol.GapReason
	state      entryState
	offered    bool
	bytes      int
	raw        []byte
}

func (e *entry) key() string {
	return refKey(e.runID, e.sequence)
}

type queueStats struct {
	PendingRecords          int64
	PendingBytes            int64
	InFlightRecords         int64
	DroppedRecords          int64
	GapRecords              int64
	Replaying               bool
	OldestPendingCapturedAt *time.Time
	LastAckAt               *time.Time
}

// queue is the RAM-only delivery queue. It mirrors the prototype's
// DestinationQueue semantics without the durable SQLite variant: a newest-
// sample live slot, count/byte-bounded replay, in-flight records that are never
// evicted, oldest-first age/byte eviction, and coalesced gap records.
//
// Records are kept in their serialized wire form and re-parsed on every read,
// exactly like a durable row. That gives identical byte accounting and hands
// callers a detached copy that cannot mutate what is still owed.
type queue struct {
	mu sync.Mutex

	limits   QueueLimits
	identity QueueIdentity
	allocate func() int64
	clock    func() time.Time

	entries      []*entry
	byRef        map[string]*entry
	nextID       uint64
	totalBytes   int
	liveID       uint64
	dropped      int64
	lastAckAt    *time.Time
	closed       bool
	afterReserve func()
}

// reservation is a set of entries selected atomically and already marked
// in-flight. A reserved entry cannot be evicted or folded into a gap until it
// is released or acknowledged.
type reservation struct {
	records []protocol.TelemetryRecord
	entries []*entry
}

func (r *reservation) refs() []protocol.RecordRef {
	if r == nil {
		return nil
	}
	return refsOf(r.records)
}

func newQueue(options queueOptions) *queue {
	if options.clock == nil {
		options.clock = time.Now
	}
	return &queue{
		limits:       options.limits,
		identity:     options.identity,
		allocate:     options.allocate,
		clock:        options.clock,
		byRef:        make(map[string]*entry),
		afterReserve: options.afterReserve,
	}
}

// append admits one already-filtered record. Age and byte caps are applied
// first so the newest sample keeps its slot, then the gap cap is settled so
// admission itself keeps retained data bounded. It never panics and reports a
// contract violation only for a record that cannot be on the wire at all.
func (q *queue) append(record protocol.TelemetryRecord) error {
	if record == nil || isNilInterface(record) {
		return errors.New("queue: cannot append a nil record")
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("queue: encode record: %w", err)
	}
	parsed, err := protocol.ParseTelemetryRecord(raw)
	if err != nil {
		return fmt.Errorf("queue: invalid record: %w", err)
	}
	bytes := len(raw)

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return errors.New("queue: closed")
	}
	meta := metadataOf(parsed)
	// The hub authenticates record identity against the session. A record from
	// another collector or run would make the hub close the connection, so it
	// is refused here instead of becoming a poison batch.
	if meta.collectorID != q.identity.CollectorID || meta.runID != q.identity.RunID || meta.bootID != q.identity.BootID {
		return errors.New("queue: record identity does not match this collector run")
	}
	// One run may never hold two records with the same sequence, whatever
	// their kind. A collision means a metric and a gap allocator diverged (or
	// the sampler reused a sequence); accepting it would corrupt hub dedup.
	if _, duplicate := q.byRef[refKey(meta.runID, meta.sequence)]; duplicate {
		return fmt.Errorf("queue: duplicate record ref %s#%d", meta.runID, meta.sequence)
	}
	if bytes > q.limits.MaxBytes {
		// A single record larger than the whole queue can never be admitted.
		q.dropped++
		q.writeGapLocked(protocol.GapOutboxBytes, meta.capturedAt, meta.capturedAt, 1)
		q.enforceAdmissionLocked()
		return nil
	}
	q.enforceAgeLocked()
	q.enforceBytesLocked(q.limits.MaxBytes - bytes)
	q.insertLocked(meta, raw, bytes)
	q.enforceAdmissionLocked()
	return nil
}

type recordMeta struct {
	collectorID string
	runID       string
	bootID      string
	sequence    int64
	capturedAt  time.Time
	kind        protocol.RecordKind
	gapReason   protocol.GapReason
}

func metadataOf(record protocol.TelemetryRecord) recordMeta {
	switch typed := record.(type) {
	case protocol.MetricsRecord:
		return recordMeta{
			collectorID: typed.CollectorID,
			runID:       typed.RunID,
			bootID:      typed.BootID,
			sequence:    typed.Sequence,
			capturedAt:  parseTimestamp(typed.CapturedAt),
			kind:        protocol.RecordKindMetrics,
		}
	case protocol.GapRecord:
		return recordMeta{
			collectorID: typed.CollectorID,
			runID:       typed.RunID,
			bootID:      typed.BootID,
			sequence:    typed.Sequence,
			capturedAt:  parseTimestamp(typed.CapturedAt),
			kind:        protocol.RecordKindGap,
			gapReason:   typed.Payload.Reason,
		}
	default:
		return recordMeta{kind: protocol.RecordKindMetrics}
	}
}

// takeLive reserves the newest metrics record that has not been offered to the
// live slot yet, atomically with its snapshot. An unacknowledged live send is
// released after its ack timeout and replayed like any other pending record.
func (q *queue) takeLive() *reservation {
	q.mu.Lock()
	if q.closed || q.liveID == 0 {
		q.mu.Unlock()
		return nil
	}
	id := q.liveID
	q.liveID = 0
	var found *entry
	for _, candidate := range q.entries {
		if candidate.id == id {
			found = candidate
			break
		}
	}
	if found == nil || found.state != statePending || found.kind != protocol.RecordKindMetrics {
		q.mu.Unlock()
		return nil
	}
	record := q.parseLocked(found)
	if record == nil {
		q.mu.Unlock()
		return nil
	}
	found.state = stateInFlight
	found.offered = true
	hook := q.afterReserve
	q.mu.Unlock()
	if hook != nil {
		hook()
	}
	return &reservation{records: []protocol.TelemetryRecord{record}, entries: []*entry{found}}
}

// takeReplay reserves the oldest pending records, bounded by count and bytes.
// Selection and the in-flight mark happen under one lock, so a concurrent
// Enqueue cannot evict or coalesce a record between selection and sending. At
// least one record is returned when one fits, so a single large sample cannot
// stall the queue.
func (q *queue) takeReplay(maxRecords, maxBytes int) *reservation {
	q.mu.Lock()
	if q.closed || maxRecords < 1 {
		q.mu.Unlock()
		return nil
	}
	reserved := &reservation{}
	bytes := 0
	for _, candidate := range q.entries {
		if len(reserved.records) >= maxRecords {
			break
		}
		if candidate.state != statePending || candidate.id == q.liveID {
			continue
		}
		if len(reserved.records) > 0 && bytes+candidate.bytes > maxBytes {
			break
		}
		record := q.parseLocked(candidate)
		if record == nil {
			continue
		}
		candidate.state = stateInFlight
		candidate.offered = true
		reserved.records = append(reserved.records, record)
		reserved.entries = append(reserved.entries, candidate)
		bytes += candidate.bytes
	}
	hook := q.afterReserve
	q.mu.Unlock()
	if len(reserved.records) == 0 {
		return nil
	}
	if hook != nil {
		hook()
	}
	return reserved
}

func (q *queue) release(refs []protocol.RecordRef) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, ref := range refs {
		if candidate, ok := q.byRef[refKey(ref.RunID, ref.Sequence)]; ok && candidate.state == stateInFlight {
			candidate.state = statePending
		}
	}
}

func (q *queue) releaseAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, candidate := range q.entries {
		if candidate.state == stateInFlight {
			candidate.state = statePending
		}
	}
}

// releaseEntries returns entries trimmed from a reserved batch to the pending
// set so they are retried in a later, smaller batch.
func (q *queue) releaseEntries(entries []*entry) {
	if len(entries) == 0 {
		return
	}
	refs := make([]protocol.RecordRef, 0, len(entries))
	for _, candidate := range entries {
		refs = append(refs, protocol.RecordRef{RunID: candidate.runID, Sequence: candidate.sequence})
	}
	q.release(refs)
}

// dropUnsendable removes a reserved record that can never fit MaxBatchBytes.
// The drop is real loss, so it is counted and covered by a gap describing the
// record's capture interval; it is never acknowledged as if it had reached the
// hub.
func (q *queue) dropUnsendable(ref protocol.RecordRef) {
	q.mu.Lock()
	defer q.mu.Unlock()
	candidate, ok := q.byRef[refKey(ref.RunID, ref.Sequence)]
	if !ok {
		return
	}
	capturedAt := candidate.capturedAt
	kind := candidate.kind
	q.removeLocked(candidate)
	q.dropped++
	if kind == protocol.RecordKindMetrics {
		q.writeGapLocked(protocol.GapOutboxBytes, capturedAt, capturedAt, 1)
		q.enforceAdmissionLocked()
	}
}

// acknowledge removes accepted and rejected records. Unknown refs are ignored,
// which makes duplicate or delayed acknowledgements harmless.
func (q *queue) acknowledge(accepted, rejected []protocol.RecordRef) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if len(accepted) == 0 && len(rejected) == 0 {
		return
	}
	for _, ref := range append(append([]protocol.RecordRef{}, accepted...), rejected...) {
		if candidate, ok := q.byRef[refKey(ref.RunID, ref.Sequence)]; ok {
			q.removeLocked(candidate)
		}
	}
	now := q.clock()
	q.lastAckAt = &now
}

func (q *queue) enforceLimits() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.enforceAgeLocked()
	q.enforceBytesLocked(q.limits.MaxBytes)
	q.enforceAdmissionLocked()
}

func (q *queue) status() queueStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	stats := queueStats{DroppedRecords: q.dropped}
	for _, candidate := range q.entries {
		if candidate.state == stateInFlight {
			stats.InFlightRecords++
			continue
		}
		stats.PendingRecords++
		stats.PendingBytes += int64(candidate.bytes)
		if candidate.capturedAt.IsZero() {
			continue
		}
		if stats.OldestPendingCapturedAt == nil || candidate.capturedAt.Before(*stats.OldestPendingCapturedAt) {
			captured := candidate.capturedAt
			stats.OldestPendingCapturedAt = &captured
		}
	}
	stats.Replaying = stats.PendingRecords > 0
	stats.GapRecords = int64(q.gapCountLocked())
	if q.lastAckAt != nil {
		ack := *q.lastAckAt
		stats.LastAckAt = &ack
	}
	return stats
}

func (q *queue) hasPending() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries) > 0
}

func (q *queue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.entries = nil
	q.byRef = make(map[string]*entry)
	q.totalBytes = 0
	q.liveID = 0
}

// allocateGapSequenceLocked asks the run's shared allocator for a sequence and
// skips any value already present. A correct allocator never collides; the
// guard turns an allocator bug into a skipped number instead of a corrupted
// byRef map and a duplicate record the hub would reject.
func (q *queue) allocateGapSequenceLocked() (int64, bool) {
	for attempt := 0; attempt < 64; attempt++ {
		sequence := q.allocate()
		if _, exists := q.byRef[refKey(q.identity.RunID, sequence)]; !exists {
			return sequence, true
		}
	}
	return 0, false
}

func (q *queue) insertLocked(meta recordMeta, raw []byte, bytes int) *entry {
	q.nextID++
	candidate := &entry{
		id:         q.nextID,
		runID:      meta.runID,
		sequence:   meta.sequence,
		capturedAt: meta.capturedAt,
		kind:       meta.kind,
		gapReason:  meta.gapReason,
		state:      statePending,
		bytes:      bytes,
		raw:        raw,
	}
	q.entries = append(q.entries, candidate)
	q.byRef[candidate.key()] = candidate
	q.totalBytes += bytes
	if candidate.kind == protocol.RecordKindMetrics {
		q.liveID = candidate.id
	}
	return candidate
}

func (q *queue) removeLocked(candidate *entry) {
	delete(q.byRef, candidate.key())
	for index, existing := range q.entries {
		if existing == candidate {
			q.entries = append(q.entries[:index], q.entries[index+1:]...)
			break
		}
	}
	if q.liveID == candidate.id {
		q.liveID = 0
	}
	q.totalBytes -= candidate.bytes
	if q.totalBytes < 0 {
		q.totalBytes = 0
	}
}

// parseLocked re-parses one entry. Ours is always self-written, so a parse
// failure means an internal bug: count it as dropped and forget the row rather
// than stalling every later record.
func (q *queue) parseLocked(candidate *entry) protocol.TelemetryRecord {
	record, err := protocol.ParseTelemetryRecord(candidate.raw)
	if err != nil {
		q.removeLocked(candidate)
		q.dropped++
		return nil
	}
	return record
}

func (q *queue) pendingMetricsLocked() []*entry {
	var pending []*entry
	for _, candidate := range q.entries {
		if candidate.state == statePending && candidate.kind == protocol.RecordKindMetrics {
			pending = append(pending, candidate)
		}
	}
	return pending
}

func (q *queue) enforceAgeLocked() {
	cutoff := q.clock().Add(-q.limits.MaxAge)
	var expired []*entry
	for _, candidate := range q.pendingMetricsLocked() {
		if !candidate.capturedAt.IsZero() && candidate.capturedAt.Before(cutoff) {
			expired = append(expired, candidate)
		}
	}
	if len(expired) > 0 {
		q.evictLocked(expired, protocol.GapOutboxAge)
	}
}

func (q *queue) enforceBytesLocked(targetBytes int) {
	if q.totalBytes <= targetBytes {
		return
	}
	pending := q.pendingMetricsLocked()
	var victims []*entry
	freed := 0
	for _, candidate := range pending {
		if q.totalBytes-freed <= targetBytes {
			break
		}
		victims = append(victims, candidate)
		freed += candidate.bytes
	}
	if len(victims) == 0 && len(pending) > 0 {
		victims = append(victims, pending[0])
	}
	if len(victims) > 0 {
		q.evictLocked(victims, protocol.GapOutboxBytes)
	}
}

// evictLocked removes victims and folds them into one coalesced gap so the hub
// sees the hole instead of a silent absence.
func (q *queue) evictLocked(victims []*entry, reason protocol.GapReason) int {
	if len(victims) == 0 {
		return 0
	}
	from := victims[0].capturedAt
	to := victims[0].capturedAt
	for _, candidate := range victims {
		q.removeLocked(candidate)
		if !candidate.capturedAt.IsZero() {
			if candidate.capturedAt.Before(from) || from.IsZero() {
				from = candidate.capturedAt
			}
			if candidate.capturedAt.After(to) || to.IsZero() {
				to = candidate.capturedAt
			}
		}
	}
	q.dropped += int64(len(victims))
	q.writeGapLocked(reason, from, to, int64(len(victims)))
	return len(victims)
}

// writeGapLocked coalesces into the newest pending gap of the same run and
// reason that has not been offered yet, so per-reason evidence stays one
// record while gaps queue.
func (q *queue) writeGapLocked(reason protocol.GapReason, from, to time.Time, dropped int64) {
	if dropped < 1 {
		dropped = 1
	}
	if from.IsZero() {
		from = q.clock()
	}
	if to.IsZero() {
		to = from
	}
	if to.Before(from) {
		from, to = to, from
	}
	for index := len(q.entries) - 1; index >= 0; index-- {
		candidate := q.entries[index]
		if candidate.kind != protocol.RecordKindGap || candidate.state != statePending || candidate.offered {
			continue
		}
		if candidate.runID != q.identity.RunID || candidate.gapReason != reason {
			continue
		}
		record, ok := q.parseLocked(candidate).(protocol.GapRecord)
		if !ok {
			break
		}
		extendGap(&record.Payload, from, to, dropped)
		raw, err := json.Marshal(record)
		if err != nil {
			break
		}
		candidate.raw = raw
		q.totalBytes += len(raw) - candidate.bytes
		candidate.bytes = len(raw)
		return
	}
	now := q.clock()
	sequence, ok := q.allocateGapSequenceLocked()
	if !ok {
		return
	}
	record := protocol.GapRecord{
		RecordEnvelope: protocol.RecordEnvelope{
			CollectorID: q.identity.CollectorID,
			RunID:       q.identity.RunID,
			BootID:      q.identity.BootID,
			Sequence:    sequence,
			CapturedAt:  formatTimestamp(now),
		},
		Payload: protocol.GapPayload{
			FromCapturedAt: formatTimestamp(from),
			ToCapturedAt:   formatTimestamp(to),
			DroppedRecords: dropped,
			Reason:         reason,
		},
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return
	}
	q.insertLocked(recordMeta{
		runID:      record.RunID,
		sequence:   record.Sequence,
		capturedAt: now,
		kind:       protocol.RecordKindGap,
		gapReason:  reason,
	}, raw, len(raw))
}

func extendGap(payload *protocol.GapPayload, from, to time.Time, dropped int64) {
	from = from.Truncate(time.Millisecond)
	to = to.Truncate(time.Millisecond)
	if payload.FromCapturedAt == "" || formatTimestamp(from) < payload.FromCapturedAt {
		payload.FromCapturedAt = formatTimestamp(from)
	}
	if payload.ToCapturedAt == "" || formatTimestamp(to) > payload.ToCapturedAt {
		payload.ToCapturedAt = formatTimestamp(to)
	}
	payload.DroppedRecords += dropped
}

func (q *queue) enforceAdmissionLocked() {
	q.enforceGapCapLocked()
}

// enforceGapCap bounds retained gap records by compacting pairs that have
// never been offered to the hub. A gap that was offered (or is in flight) has
// an identity the hub may already have committed; changing its sequence or
// payload would defeat deduplication when an ack was lost, so it is immutable.
// If every remaining gap has been offered the queue stays above the cap until
// those gaps are acknowledged; no offered gap is ever rewritten.
func (q *queue) enforceGapCapLocked() {
	for guard := 0; q.gapCountLocked() > q.limits.MaxGapRecords && guard < 10_000; guard++ {
		if !q.compactOneGapPairLocked() {
			return
		}
	}
}

// compactOneGapPairLocked merges the two oldest never-offered gaps that share a
// reason. The older entry keeps its identity: nothing has seen it yet, so
// rewriting it cannot collide with a committed record.
func (q *queue) compactOneGapPairLocked() bool {
	for _, candidate := range q.entries {
		if !mergeableGap(candidate) {
			continue
		}
		for _, other := range q.entries {
			if other == candidate || !mergeableGap(other) || other.gapReason != candidate.gapReason {
				continue
			}
			olderRecord, ok := q.parseLocked(candidate).(protocol.GapRecord)
			if !ok {
				return false
			}
			otherRecord, ok := q.parseLocked(other).(protocol.GapRecord)
			if !ok {
				return false
			}
			extendGap(&olderRecord.Payload,
				parseTimestamp(otherRecord.Payload.FromCapturedAt),
				parseTimestamp(otherRecord.Payload.ToCapturedAt),
				otherRecord.Payload.DroppedRecords,
			)
			raw, err := json.Marshal(olderRecord)
			if err != nil {
				return false
			}
			candidate.raw = raw
			q.totalBytes += len(raw) - candidate.bytes
			candidate.bytes = len(raw)
			q.removeLocked(other)
			return true
		}
	}
	return false
}

func mergeableGap(candidate *entry) bool {
	return candidate.kind == protocol.RecordKindGap &&
		candidate.state == statePending &&
		!candidate.offered
}

func (q *queue) gapCountLocked() int {
	count := 0
	for _, candidate := range q.entries {
		if candidate.kind == protocol.RecordKindGap {
			count++
		}
	}
	return count
}

func refKey(runID string, sequence int64) string {
	return runID + "\x00" + strconv.FormatInt(sequence, 10)
}

func refOf(record protocol.TelemetryRecord) (protocol.RecordRef, bool) {
	switch typed := record.(type) {
	case protocol.MetricsRecord:
		return protocol.RecordRef{RunID: typed.RunID, Sequence: typed.Sequence}, true
	case protocol.GapRecord:
		return protocol.RecordRef{RunID: typed.RunID, Sequence: typed.Sequence}, true
	default:
		return protocol.RecordRef{}, false
	}
}

func refsOf(records []protocol.TelemetryRecord) []protocol.RecordRef {
	refs := make([]protocol.RecordRef, 0, len(records))
	for _, record := range records {
		if ref, ok := refOf(record); ok {
			refs = append(refs, ref)
		}
	}
	return refs
}

// formatTimestamp emits the canonical millisecond ISO form the wire contract
// prescribes. Values outside the protocol's sanity window cannot come from a
// sane clock; they are still formatted rather than invented.
func formatTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func parseTimestamp(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.Truncate(time.Millisecond)
}

// isNilInterface reports a nil interface or a typed nil pointer stored in one.
func isNilInterface(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
