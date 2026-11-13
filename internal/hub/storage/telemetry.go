package storage

import (
	"container/list"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

const (
	// DefaultMaxReplayAge is the default oldest captured time a record may
	// carry. Ingest permanently rejects a record whose capture time is older
	// than this horizon, so no observation earlier than it can still enter the
	// alert engine.
	DefaultMaxReplayAge     = 24 * time.Hour
	defaultFlushInterval    = 5 * time.Second
	defaultMaxQueuedRecords = 4096
	defaultMaxRecordsPerTxn = 512
	failureRetry            = 5 * time.Second
	defaultMaxReplayAge     = DefaultMaxReplayAge
	// defaultClockSkew matches the historical 24-hour future tolerance. It is
	// the allowance for a collector clock running ahead of the hub.
	defaultClockSkew        = 24 * time.Hour
	minReceiptLifetime      = 24 * time.Hour
	minDerivedPhysicalBytes = 8 * 1024 * 1024
	defaultMaxCachedSeries  = 20_000
	summaryStepMS           = int64(10_000)
)

// Permanent ingest rejection reasons. A rejected record is never retried: it
// produced no receipt, so the hub must turn these into ack rejections rather
// than ask the collector to send the batch again.
const (
	// RejectionCapturedTooOld means the capture time is older than the replay
	// horizon, where dedup receipts may already have been pruned.
	RejectionCapturedTooOld = "capturedAt is older than the replay horizon"
	// RejectionCapturedInFuture means the capture time is further ahead of the
	// hub clock than the configured clock-skew allowance.
	RejectionCapturedInFuture = "capturedAt is more than the clock-skew allowance in the future"
)

// IsPermanentRejection reports whether an ingest rejection reason is final.
// Every currently reported rejection is permanent; the helper is the stable
// contract the hub uses when composing an ack.
func IsPermanentRejection(reason string) bool {
	switch reason {
	case RejectionCapturedTooOld, RejectionCapturedInFuture:
		return true
	default:
		return false
	}
}

// Batch is one accepted samples batch.
type Batch struct {
	MachineID   string
	CollectorID string
	// CredentialHash is the collector credential the gateway authenticated
	// when it accepted this batch. The write transaction re-reads the registry
	// and drops the batch when this credential is no longer current, so a
	// replacement that commits while the batch waits in the queue cannot
	// persist history or machine_latest from the old credential. Empty means
	// the caller did not present a credential (internal replay/test paths).
	CredentialHash     string
	Mode               protocol.SampleMode
	ExpectedIntervalMs *int64
	Hostname           *string
	Records            []protocol.TelemetryRecord
	// ReceivedAt is the authoritative hub receipt time captured once at
	// ingest. Live eligibility and persistence both use it, so a batch cannot
	// be published as fresh and then rejected across a replay-horizon
	// boundary. Zero means Enqueue captures its own clock.
	ReceivedAt time.Time
}

// ErrCredentialSuperseded reports that a queued batch's collector credential
// was replaced or revoked before its write transaction ran. Nothing from that
// batch was persisted and no acknowledgement was sent, so the transport must
// not answer with a retry: the credential itself is no longer current.
var ErrCredentialSuperseded = errors.New("storage: collector credential is no longer current")

// IngestResult is the committed result for one batch. It is returned only
// after COMMIT.
type IngestResult struct {
	Accepted   []protocol.RecordRef
	Rejected   []protocol.RejectedRecord
	NewLive    *protocol.MetricsRecord
	ReceivedAt time.Time
}

// TelemetryOptions configures summary history and the RAM ring. The hub
// composition root selects Memory by default; repository callers opt in.
type TelemetryOptions struct {
	// Memory isolates telemetry in a bounded in-memory database. The supplied
	// database remains the durable authority for collector credentials.
	Memory                   bool
	credentialDB             *Database
	Now                      func() time.Time
	FlushInterval            time.Duration
	MaxQueuedRecords         int
	MaxRecordsPerTransaction int
	Retention                RetentionOverrides
	// MaxReplayAge is the oldest captured time a record may carry. Anything
	// older is rejected permanently instead of risking a pruned receipt and
	// duplicate side effects. Default 24h.
	MaxReplayAge time.Duration
	// ClockSkew is how far a capture time may run ahead of the hub clock.
	// Default 24h.
	ClockSkew time.Duration
	// MaxCachedSeries bounds the in-memory series catalog with a least
	// recently used policy. Default 20000; evicted series are looked up (or
	// re-inserted) on their next sample.
	MaxCachedSeries int
	// MaxDatabaseBytes is a soft retention trigger. Zero disables it.
	MaxDatabaseBytes int64
	// MaxPhysicalBytes is the finite main-file page cap plus WAL reserve. When
	// nil it derives from MaxDatabaseBytes (at least 8 MiB) or the 2 GiB
	// default. Negative disables the hard cap (tests and measurement only).
	MaxPhysicalBytes *int64
	Ring             RingOptions
}

// TelemetryStats is a diagnostic snapshot of ingestion and storage pressure.
type TelemetryStats struct {
	QueuedRecords     int
	QueuedBatches     int
	LastFlush         time.Time
	LastFlushDuration time.Duration
	CommittedRecords  int64
	DuplicateRecords  int64
	RejectedRecords   int64
	// SupersededBatches and SupersededRecords count batches dropped because
	// their collector credential was replaced or revoked before the write
	// transaction ran. They were never acknowledged or persisted.
	SupersededBatches int64
	SupersededRecords int64
	WriteFailures     int64
	DiskFullFailures  int64
	LastWriteError    string
	DBBytes           int64
	DBWALBytes        int64
	DBLimitBytes      int64
	Degraded          bool
	Retention         RetentionStatus
}

type summaryAggregate struct {
	min   float64
	max   float64
	sum   float64
	count int64
	nulls int64
}

type liveCursor struct {
	runID        string
	sequence     int64
	capturedAtMs int64
}

type liveUpsert struct {
	record             protocol.MetricsRecord
	receivedAtMs       int64
	expectedIntervalMs *int64
	hostname           *string
}

type flushCounters struct {
	committed  int64
	duplicates int64
	rejected   int64
}

type flushState struct {
	cursors    map[string]*liveCursor
	cursorRead map[string]bool
	upserts    map[string]*liveUpsert
	summaries  map[int64]map[int64]*summaryAggregate
	ringPoints []ringPoint
	counters   flushCounters
	// known caches the known-series view per machine for this flush. Payloads
	// that omit an optional field need it for NULL breaks, but recomputing and
	// sorting it per record would make the common path expensive.
	known        map[string][]KnownSeries
	knownPending map[string]int
}

type catalogEntry struct {
	key        string
	machineID  string
	metric     string
	resourceID string
}

type catalogItem struct {
	key        string
	id         int64
	machineID  string
	metric     string
	resourceID string
}

type pendingSeries struct {
	ids   map[string]int64
	added []catalogEntry
}

func newPendingSeries() *pendingSeries {
	return &pendingSeries{ids: make(map[string]int64)}
}

// seriesCatalog is a bounded LRU view of the durable series catalog. It exists
// to avoid one insert+select per sample, not as the source of truth: an evicted
// series is re-read or re-inserted on its next sample. The known-series view
// used for NULL breaks is derived from the same bounded entries.
type seriesCatalog struct {
	mu      sync.Mutex
	limit   int
	order   *list.List
	entries map[string]*list.Element
	known   map[string]map[string]KnownSeries
}

func newSeriesCatalog(ctx context.Context, db *Database, limit int) (*seriesCatalog, error) {
	if limit <= 0 {
		limit = defaultMaxCachedSeries
	}
	catalog := &seriesCatalog{
		limit:   limit,
		order:   list.New(),
		entries: make(map[string]*list.Element),
		known:   make(map[string]map[string]KnownSeries),
	}
	// Load the newest entries first; older ones are looked up on demand.
	rows, err := db.Query(ctx,
		"SELECT series_id, machine_id, metric, resource_id FROM series ORDER BY series_id DESC LIMIT ?",
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item catalogItem
		if err := rows.Scan(&item.id, &item.machineID, &item.metric, &item.resourceID); err != nil {
			return nil, mapError(err)
		}
		item.key = seriesKey(item.machineID, item.metric, item.resourceID)
		catalog.insertLocked(item)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return catalog, nil
}

func seriesKey(machineID, metric, resourceID string) string {
	return machineID + "\x00" + metric + "\x00" + resourceID
}

func (c *seriesCatalog) rememberLocked(item catalogItem) {
	machine := c.known[item.machineID]
	if machine == nil {
		machine = make(map[string]KnownSeries)
		c.known[item.machineID] = machine
	}
	machine[item.metric+"\x00"+item.resourceID] = KnownSeries{Metric: item.metric, ResourceID: item.resourceID}
}

func (c *seriesCatalog) insertLocked(item catalogItem) {
	if existing, ok := c.entries[item.key]; ok {
		existing.Value.(*catalogItem).id = item.id
		c.order.MoveToFront(existing)
		c.rememberLocked(item)
		return
	}
	c.entries[item.key] = c.order.PushFront(&item)
	c.rememberLocked(item)
	for len(c.entries) > c.limit {
		c.evictLocked()
	}
}

func (c *seriesCatalog) evictLocked() {
	back := c.order.Back()
	if back == nil {
		return
	}
	item := back.Value.(*catalogItem)
	c.order.Remove(back)
	delete(c.entries, item.key)
	machine := c.known[item.machineID]
	if machine == nil {
		return
	}
	delete(machine, item.metric+"\x00"+item.resourceID)
	if len(machine) == 0 {
		delete(c.known, item.machineID)
	}
}

// removeIDs drops cached entries after retention deleted their series rows.
// Without this a stale id could write points for a missing series.
func (c *seriesCatalog) removeIDs(ids []int64) {
	if len(ids) == 0 {
		return
	}
	removed := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		removed[id] = struct{}{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, element := range c.entries {
		item := element.Value.(*catalogItem)
		if _, ok := removed[item.id]; !ok {
			continue
		}
		c.order.Remove(element)
		delete(c.entries, key)
		if machine := c.known[item.machineID]; machine != nil {
			delete(machine, item.metric+"\x00"+item.resourceID)
			if len(machine) == 0 {
				delete(c.known, item.machineID)
			}
		}
	}
}

func (c *seriesCatalog) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *seriesCatalog) idFor(tx *Tx, pending *pendingSeries, machineID, metric, resourceID string) (int64, error) {
	key := seriesKey(machineID, metric, resourceID)
	if id, ok := pending.ids[key]; ok {
		return id, nil
	}
	c.mu.Lock()
	if element, ok := c.entries[key]; ok {
		c.order.MoveToFront(element)
		id := element.Value.(*catalogItem).id
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()
	if _, err := tx.Exec(
		"INSERT INTO series (machine_id, metric, resource_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING",
		machineID, metric, resourceID,
	); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(
		"SELECT series_id FROM series WHERE machine_id = ? AND metric = ? AND resource_id = ?",
		machineID, metric, resourceID,
	).Scan(&id); err != nil {
		return 0, mapError(err)
	}
	pending.ids[key] = id
	pending.added = append(pending.added, catalogEntry{
		key:        key,
		machineID:  machineID,
		metric:     metric,
		resourceID: resourceID,
	})
	return id, nil
}

func (c *seriesCatalog) knownFor(machineID string, pending *pendingSeries) []KnownSeries {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]KnownSeries, 0, len(c.known[machineID])+8)
	seen := make(map[string]struct{}, len(c.known[machineID]))
	for key, item := range c.known[machineID] {
		result = append(result, item)
		seen[key] = struct{}{}
	}
	if pending != nil {
		for _, entry := range pending.added {
			if entry.machineID != machineID {
				continue
			}
			key := entry.metric + "\x00" + entry.resourceID
			if _, ok := seen[key]; ok {
				continue
			}
			result = append(result, KnownSeries{Metric: entry.metric, ResourceID: entry.resourceID})
			seen[key] = struct{}{}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Metric != result[j].Metric {
			return result[i].Metric < result[j].Metric
		}
		return result[i].ResourceID < result[j].ResourceID
	})
	return result
}

func (c *seriesCatalog) commit(pending *pendingSeries) {
	if pending == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range pending.added {
		c.insertLocked(catalogItem{
			key:        entry.key,
			id:         pending.ids[entry.key],
			machineID:  entry.machineID,
			metric:     entry.metric,
			resourceID: entry.resourceID,
		})
	}
}

// Telemetry owns persistent summary history, its bounded write batcher and the
// in-memory one-second ring. It is safe for concurrent Enqueue, Query and
// repository use; one writer goroutine serializes ingestion.
type Telemetry struct {
	credentialDB *Database // non-nil only when db is a separate RAM database
	db           *Database
	ctx          context.Context
	cancel       context.CancelFunc
	now          func() time.Time
	ring         *Ring
	catalog      *seriesCatalog
	retention    *retentionManager

	flushInterval  time.Duration
	maxQueued      int
	maxPerTxn      int
	maxPhysical    int64
	maxDatabase    int64
	maxReplayAgeMS int64
	clockSkewMS    int64

	jobs    chan *ingestJob
	closeCh chan struct{}
	done    chan struct{}
	closed  atomic.Bool

	submitMu sync.RWMutex
	queueMu  sync.Mutex

	queuedRecords int
	queuedBatches int

	statsMu        sync.Mutex
	committed      int64
	duplicates     int64
	rejected       int64
	superseded     int64
	supersededRecs int64
	writeFailures  int64
	diskFull       int64
	lastWriteError string
	lastFlush      time.Time
	lastFlushDur   time.Duration
	degraded       bool
}

type ingestJob struct {
	batch      *Batch
	barrier    chan error
	receivedAt time.Time
	result     chan jobResult
	accepted   []protocol.RecordRef
	rejected   []protocol.RejectedRecord
	newLive    *protocol.MetricsRecord
	// superseded marks a batch whose credential was no longer current inside
	// the write transaction. It is dropped without persisting anything and
	// resolved with ErrCredentialSuperseded.
	superseded bool
}

type jobResult struct {
	result IngestResult
	err    error
}

// NewTelemetry opens history over an already migrated database and starts its
// writer and retention loops. The caller owns the Database and must close the
// Telemetry before closing it.
func NewTelemetry(ctx context.Context, db *Database, opts TelemetryOptions) (*Telemetry, error) {
	if opts.Memory {
		return newMemoryTelemetry(ctx, db, opts)
	}
	if opts.Now == nil {
		opts.Now = db.Now
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = defaultFlushInterval
	}
	if opts.FlushInterval > 5*time.Second {
		return nil, invalidError("flush interval must be at most five seconds")
	}
	if opts.MaxQueuedRecords <= 0 {
		opts.MaxQueuedRecords = defaultMaxQueuedRecords
	}
	if opts.MaxRecordsPerTransaction <= 0 {
		opts.MaxRecordsPerTransaction = defaultMaxRecordsPerTxn
	}
	if opts.MaxDatabaseBytes < 0 {
		return nil, invalidError("maxDatabaseBytes must not be negative")
	}
	policy, err := ResolveRetention(opts.Retention)
	if err != nil {
		return nil, err
	}
	if opts.MaxReplayAge < 0 {
		return nil, invalidError("maxReplayAge must not be negative")
	}
	if opts.MaxReplayAge == 0 {
		opts.MaxReplayAge = defaultMaxReplayAge
	}
	if opts.MaxReplayAge < time.Minute {
		return nil, invalidError("maxReplayAge must be at least one minute")
	}
	if opts.ClockSkew < 0 {
		return nil, invalidError("clockSkew must not be negative")
	}
	if opts.ClockSkew == 0 {
		opts.ClockSkew = defaultClockSkew
	}
	if opts.ClockSkew > 7*24*time.Hour {
		return nil, invalidError("clockSkew must be at most seven days")
	}
	// A receipt is the only thing that makes a retried record idempotent. It
	// must outlive every capture time that is still accepted, plus the
	// allowance for a collector clock behind the hub.
	if policy.ReceiptsMs < minReceiptLifetime.Milliseconds() {
		return nil, invalidError("receipt retention must be at least 24 hours")
	}
	if policy.ReceiptsMs <= opts.MaxReplayAge.Milliseconds()+opts.ClockSkew.Milliseconds() {
		return nil, invalidError("receipt retention must be longer than maxReplayAge plus clockSkew")
	}
	physicalLimit := int64(0)
	switch {
	case opts.MaxPhysicalBytes == nil:
		if opts.MaxDatabaseBytes >= minDerivedPhysicalBytes {
			physicalLimit = opts.MaxDatabaseBytes
		} else {
			physicalLimit = DefaultMaxDatabaseBytes
		}
	case *opts.MaxPhysicalBytes < 0:
		physicalLimit = 0
	case *opts.MaxPhysicalBytes < 16*1024:
		return nil, invalidError("maxPhysicalBytes must be at least 16 KiB or negative to disable")
	default:
		physicalLimit = *opts.MaxPhysicalBytes
	}
	if physicalLimit > 0 {
		if _, err := db.SetPhysicalLimit(physicalLimit); err != nil {
			return nil, err
		}
	}
	catalog, err := newSeriesCatalog(ctx, db, opts.MaxCachedSeries)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	telemetry := &Telemetry{
		credentialDB:   opts.credentialDB,
		db:             db,
		ctx:            runCtx,
		cancel:         cancel,
		now:            opts.Now,
		ring:           NewRing(opts.Ring),
		catalog:        catalog,
		flushInterval:  opts.FlushInterval,
		maxQueued:      opts.MaxQueuedRecords,
		maxPerTxn:      opts.MaxRecordsPerTransaction,
		maxPhysical:    physicalLimit,
		maxDatabase:    opts.MaxDatabaseBytes,
		maxReplayAgeMS: opts.MaxReplayAge.Milliseconds(),
		clockSkewMS:    opts.ClockSkew.Milliseconds(),
		jobs:           make(chan *ingestJob, 64),
		closeCh:        make(chan struct{}),
		done:           make(chan struct{}),
	}
	telemetry.retention = newRetentionManager(db, policy, opts.Now, time.Minute, defaultChunk, opts.MaxDatabaseBytes, physicalLimit)
	telemetry.retention.onSeriesPruned = catalog.removeIDs
	telemetry.retention.start(runCtx)
	go telemetry.run()
	return telemetry, nil
}

// FlushInterval is the effective batching delay. Live publication does not
// wait for this delay; acknowledgements wait for acceptance in the store.
func (t *Telemetry) FlushInterval() time.Duration { return t.flushInterval }

// RetentionPolicy reports the store's active retention windows.
func (t *Telemetry) RetentionPolicy() RetentionPolicy { return t.retention.policy }

// CaptureRejection reports why a record captured at capturedAt would be
// rejected if it were enqueued at receivedAt, or "" when the record is inside
// the replay horizon and the clock-skew allowance in both directions.
//
// Transport code uses it before publishing a record to live state so a record
// the store rejects is never shown as fresh. It applies exactly the policy the
// commit path applies.
func (t *Telemetry) CaptureRejection(collectorID, capturedAt string, receivedAt time.Time) string {
	return rejectionReason(collectorID, capturedAt, receivedAt.UnixMilli(), t.maxReplayAgeMS, t.clockSkewMS)
}

// RingStats reports the bounded RAM ring's state.
func (t *Telemetry) RingStats() RingStats { return t.ring.Stats() }

// Enqueue validates and queues one batch. It returns after the transaction
// that wrote the batch commits, or with a busy/full storage error that tells
// the collector to keep and retry the batch. Records are deduplicated by
// (collectorId, runId, sequence), so retrying after a lost acknowledgement is
// safe.
func (t *Telemetry) Enqueue(ctx context.Context, batch Batch) (IngestResult, error) {
	if err := validateBatch(batch); err != nil {
		return IngestResult{}, err
	}
	t.submitMu.RLock()
	if t.closed.Load() {
		t.submitMu.RUnlock()
		return IngestResult{}, storageError(CodeClosed, "storage is closed")
	}
	receivedAt := batch.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = t.now()
	}
	job := &ingestJob{
		batch:      &batch,
		receivedAt: receivedAt,
		result:     make(chan jobResult, 1),
	}
	t.queueMu.Lock()
	if t.queuedRecords+len(batch.Records) > t.maxQueued {
		queued := t.queuedRecords
		t.queueMu.Unlock()
		t.submitMu.RUnlock()
		ratio := float64(queued) / float64(t.maxQueued)
		retry := time.Duration(1000+int(ratio*4000)) * time.Millisecond
		if retry > 5*time.Second {
			retry = 5 * time.Second
		}
		return IngestResult{}, &Error{Code: CodeBusy, Message: "ingest queue is full", RetryAfter: retry}
	}
	t.queuedRecords += len(batch.Records)
	t.queuedBatches++
	t.queueMu.Unlock()
	// Release the submission lock as soon as the batch is admitted. Waiting for
	// the commit must not hold off Close, which honours its own context.
	t.submitMu.RUnlock()

	select {
	case t.jobs <- job:
	case <-t.closeCh:
		t.discardQueued(len(batch.Records))
		return IngestResult{}, storageError(CodeClosed, "storage is shutting down")
	case <-ctx.Done():
		t.discardQueued(len(batch.Records))
		return IngestResult{}, ctx.Err()
	}
	select {
	case outcome := <-job.result:
		return outcome.result, outcome.err
	case <-ctx.Done():
		// The batch may still commit; the collector's retry is deduplicated by
		// its receipt, so dropping the wait cannot duplicate history.
		return IngestResult{}, ctx.Err()
	}
}

func (t *Telemetry) discardQueued(records int) {
	t.queueMu.Lock()
	t.queuedRecords -= records
	t.queuedBatches--
	t.queueMu.Unlock()
}

// Flush commits every batch queued before the call. It is intended for tests
// and graceful shutdown, not the steady-state path.
func (t *Telemetry) Flush(ctx context.Context) error {
	t.submitMu.RLock()
	if t.closed.Load() {
		t.submitMu.RUnlock()
		return storageError(CodeClosed, "storage is closed")
	}
	barrier := &ingestJob{barrier: make(chan error, 1)}
	select {
	case t.jobs <- barrier:
		t.submitMu.RUnlock()
	case <-t.closeCh:
		t.submitMu.RUnlock()
		return storageError(CodeClosed, "storage is shutting down")
	case <-ctx.Done():
		t.submitMu.RUnlock()
		return ctx.Err()
	}
	select {
	case err := <-barrier.barrier:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
		return storageError(CodeClosed, "storage is closed")
	}
}

// Close flushes queued work for as long as ctx allows, then stops the writer
// and retention loops. It closes an owned RAM database but never the supplied
// durable control database.
func (t *Telemetry) Close(ctx context.Context) error {
	t.submitMu.Lock()
	if t.closed.Load() {
		t.submitMu.Unlock()
		return nil
	}
	t.closed.Store(true)
	close(t.closeCh)
	t.submitMu.Unlock()
	select {
	case <-t.done:
		t.retention.stopContext(ctx)
		t.cancel()
		if t.credentialDB != nil {
			return t.db.Close()
		}
		return nil
	case <-ctx.Done():
		// Cancel the writer and the retention loop, then return without waiting
		// for a transaction that the deadline forbids us to wait for.
		t.cancel()
		t.retention.stopContext(ctx)
		if t.credentialDB != nil {
			go func() {
				<-t.done
				t.retention.stopContext(context.Background())
				_ = t.db.Close()
			}()
		}
		return ctx.Err()
	}
}

func (t *Telemetry) run() {
	defer close(t.done)
	for {
		var pending []*ingestJob
		select {
		case job := <-t.jobs:
			pending = append(pending, job)
		case <-t.closeCh:
			// Drain anything already queued, commit it, and stop.
			for {
				select {
				case job := <-t.jobs:
					pending = append(pending, job)
				default:
					if len(pending) > 0 {
						t.flush(pending)
					}
					return
				}
			}
		}
		timer := time.NewTimer(t.flushInterval)
		records := pendingRecords(pending)
	collecting:
		for {
			select {
			case job := <-t.jobs:
				pending = append(pending, job)
				records += pendingDelta(job)
				if records >= t.maxPerTxn || job.barrier != nil {
					break collecting
				}
			case <-timer.C:
				break collecting
			case <-t.closeCh:
				break collecting
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		t.flush(pending)
	}
}

func pendingRecords(jobs []*ingestJob) int {
	total := 0
	for _, job := range jobs {
		total += pendingDelta(job)
	}
	return total
}

func pendingDelta(job *ingestJob) int {
	if job.batch == nil {
		return 0
	}
	return len(job.batch.Records)
}

func (t *Telemetry) flush(jobs []*ingestJob) {
	started := time.Now()
	if err := t.preflight(); err != nil {
		t.finish(jobs, err)
		return
	}
	state := &flushState{
		cursors:      make(map[string]*liveCursor),
		cursorRead:   make(map[string]bool),
		upserts:      make(map[string]*liveUpsert),
		summaries:    make(map[int64]map[int64]*summaryAggregate),
		known:        make(map[string][]KnownSeries),
		knownPending: make(map[string]int),
	}
	pending := newPendingSeries()
	err := t.writeBatch(func(tx *Tx) error {
		for _, job := range jobs {
			if job.batch == nil {
				continue
			}
			current, err := t.batchCredentialCurrent(tx, job.batch)
			if err != nil {
				return err
			}
			if !current {
				// The collector's credential was replaced or revoked after this
				// batch was accepted. Drop it without writing receipts, points
				// or machine_latest; the transport has not acknowledged it yet.
				job.superseded = true
				continue
			}
			if err := t.processBatch(tx, job, state, pending); err != nil {
				return err
			}
		}
		if err := writeSummaries(tx, state.summaries); err != nil {
			return err
		}
		for machineID, live := range state.upserts {
			payloadJSON, err := json.Marshal(live.record.Payload)
			if err != nil {
				return wrapStorageError(CodeInvalid, err, "encode latest metrics payload")
			}
			if _, err := tx.Exec(
				"INSERT INTO machine_latest "+
					"(machine_id, collector_id, run_id, boot_id, seq, captured_at_ms, received_at_ms, expected_interval_ms, hostname, payload_json) "+
					"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
					"ON CONFLICT(machine_id) DO UPDATE SET collector_id = excluded.collector_id, run_id = excluded.run_id, "+
					"boot_id = excluded.boot_id, seq = excluded.seq, captured_at_ms = excluded.captured_at_ms, "+
					"received_at_ms = excluded.received_at_ms, "+
					"expected_interval_ms = COALESCE(excluded.expected_interval_ms, expected_interval_ms), "+
					"hostname = COALESCE(excluded.hostname, hostname), payload_json = excluded.payload_json",
				machineID, live.record.CollectorID, live.record.RunID, live.record.BootID, live.record.Sequence,
				parseWireTime(live.record.CapturedAt), live.receivedAtMs, nullableInt64(live.expectedIntervalMs),
				nullableString(live.hostname), string(payloadJSON),
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.finish(jobs, err)
		return
	}

	t.catalog.commit(pending)
	t.ring.Apply(state.ringPoints)
	var supersededBatches, supersededRecords int64
	for _, job := range jobs {
		if job.superseded {
			supersededBatches++
			supersededRecords += int64(len(job.batch.Records))
		}
	}
	t.statsMu.Lock()
	t.committed += state.counters.committed
	t.duplicates += state.counters.duplicates
	t.rejected += state.counters.rejected
	t.superseded += supersededBatches
	t.supersededRecs += supersededRecords
	t.lastFlush = t.now()
	t.lastFlushDur = time.Since(started)
	t.lastWriteError = ""
	t.degraded = false
	t.statsMu.Unlock()

	t.resolve(jobs)

	if t.maxPhysical > 0 {
		if t.db.SizeBytes() > t.maxPhysical {
			t.retention.relievePressure(t.ctx, 4)
		}
		physical := t.db.PhysicalLimit()
		if physical != nil && t.db.WALBytes() > physical.WALBudgetBytes {
			_ = t.db.Checkpoint(t.ctx)
		}
	}
}

// batchCredentialCurrentTx reports whether the registry still holds the exact
// credential that accepted this batch. The read shares the write transaction
// with the batch, so a replacement or revocation that commits first is observed
// before any receipt or machine_latest row is written.
func batchCredentialCurrentTx(tx *Tx, batch *Batch) (bool, error) {
	if batch.CredentialHash == "" {
		return true, nil
	}
	var stored string
	err := tx.QueryRow(
		"SELECT token_hash FROM collector_registry WHERE collector_id = ? AND revoked = 0",
		batch.CollectorID,
	).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapError(err)
	}
	return stored == batch.CredentialHash, nil
}

func (t *Telemetry) processBatch(tx *Tx, job *ingestJob, state *flushState, pending *pendingSeries) error {
	batch := job.batch
	receivedAtMS := job.receivedAt.UnixMilli()
	var (
		best   *protocol.MetricsRecord
		bestAt int64
	)
	for _, record := range batch.Records {
		var (
			ref        protocol.RecordRef
			kind       string
			capturedMS int64
			reason     string
			metrics    *protocol.MetricsRecord
			gap        *protocol.GapRecord
		)
		switch value := record.(type) {
		case protocol.MetricsRecord:
			metrics = &value
		case *protocol.MetricsRecord:
			metrics = value
		case protocol.GapRecord:
			gap = &value
		case *protocol.GapRecord:
			gap = value
		}
		if metrics == nil && gap == nil {
			job.rejected = append(job.rejected, protocol.RejectedRecord{Reason: "unsupported record kind"})
			state.counters.rejected++
			continue
		}
		if metrics != nil {
			ref = protocol.RecordRef{RunID: metrics.RunID, Sequence: metrics.Sequence}
			kind = string(protocol.RecordKindMetrics)
			reason = rejectionReason(metrics.CollectorID, metrics.CapturedAt, receivedAtMS, t.maxReplayAgeMS, t.clockSkewMS)
		} else {
			ref = protocol.RecordRef{RunID: gap.RunID, Sequence: gap.Sequence}
			kind = string(protocol.RecordKindGap)
			reason = rejectionReason(gap.CollectorID, gap.CapturedAt, receivedAtMS, t.maxReplayAgeMS, t.clockSkewMS)
			if reason == "" && gap.Payload.DroppedRecords < 0 {
				reason = "droppedRecords must not be negative"
			}
		}
		if metrics != nil && reason == "" {
			if metrics.CollectorID != batch.CollectorID {
				reason = "collectorId does not match the authenticated batch"
			}
		}
		if gap != nil && reason == "" {
			if gap.CollectorID != batch.CollectorID {
				reason = "collectorId does not match the authenticated batch"
			}
		}
		if reason == "" {
			var err error
			if metrics != nil {
				capturedMS, err = parseCapturedAt(metrics.CapturedAt)
			} else {
				capturedMS, err = parseCapturedAt(gap.CapturedAt)
			}
			if err != nil {
				reason = err.Error()
			}
		}
		var gapFromMS, gapToMS int64
		if gap != nil && reason == "" {
			var err error
			gapFromMS, err = parseCapturedAt(gap.Payload.FromCapturedAt)
			if err != nil {
				reason = "gap fromCapturedAt is not a parseable timestamp"
			}
			if reason == "" {
				gapToMS, err = parseCapturedAt(gap.Payload.ToCapturedAt)
				if err != nil {
					reason = "gap toCapturedAt is not a parseable timestamp"
				}
			}
		}
		if reason == "" && metrics != nil && metrics.Sequence < 0 {
			reason = "sequence must not be negative"
		}
		if reason != "" {
			job.rejected = append(job.rejected, protocol.RejectedRecord{RecordRef: ref, Reason: reason})
			state.counters.rejected++
			continue
		}

		result, err := tx.Exec(
			"INSERT OR IGNORE INTO sample_receipts "+
				"(collector_id, run_id, seq, machine_id, kind, mode, captured_at_ms, received_at_ms, expected_interval_ms) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			batch.CollectorID, ref.RunID, ref.Sequence, batch.MachineID, kind, string(batch.Mode),
			capturedMS, receivedAtMS, nullableInt64(batch.ExpectedIntervalMs),
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return mapError(err)
		}
		job.accepted = append(job.accepted, ref)
		if affected == 0 {
			// Lost-ack retry: acknowledge accepted but do not touch points,
			// rollups, gaps or live state.
			state.counters.duplicates++
			continue
		}
		state.counters.committed++

		if metrics != nil {
			var known []KnownSeries
			if NeedsKnownSeries(metrics.Payload) {
				known = t.knownForBatch(state, batch.MachineID, pending)
			}
			for _, point := range Flatten(metrics.Payload, known) {
				seriesID, err := t.catalog.idFor(tx, pending, batch.MachineID, point.Metric, point.ResourceID)
				if err != nil {
					return err
				}
				addSummaryPoint(state.summaries, seriesID, capturedMS, point.Value)
				state.ringPoints = append(state.ringPoints, ringPoint{
					machineID: batch.MachineID, metric: point.Metric, resourceID: point.ResourceID,
					atMs: capturedMS, value: point.Value,
				})
			}
			if batch.Mode == protocol.SampleModeLive {
				// Use exactly the live cursor's ordering. Choosing by capture
				// time first would let a lower-sequence record with a later
				// timestamp win inside one batch and hide a genuinely newer
				// sequence from the persisted cursor.
				candidate := &liveCursor{runID: metrics.RunID, sequence: metrics.Sequence, capturedAtMs: capturedMS}
				if best == nil {
					best = metrics
					bestAt = capturedMS
				} else if isNewer(candidate, &liveCursor{runID: best.RunID, sequence: best.Sequence, capturedAtMs: bestAt}) {
					best = metrics
					bestAt = capturedMS
				}
			}
			continue
		}

		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO gaps "+
				"(collector_id, run_id, seq, machine_id, from_ms, to_ms, dropped, reason, received_at_ms) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			batch.CollectorID, gap.RunID, gap.Sequence, batch.MachineID, gapFromMS, gapToMS,
			gap.Payload.DroppedRecords, string(gap.Payload.Reason), receivedAtMS,
		); err != nil {
			return err
		}
	}

	if batch.Mode != protocol.SampleModeLive || best == nil {
		return nil
	}
	cursor, err := t.latestCursor(tx, batch.MachineID, state)
	if err != nil {
		return err
	}
	candidate := &liveCursor{runID: best.RunID, sequence: best.Sequence, capturedAtMs: bestAt}
	if cursor == nil || isNewer(candidate, cursor) {
		job.newLive = best
		state.cursors[batch.MachineID] = candidate
		state.cursorRead[batch.MachineID] = true
		state.upserts[batch.MachineID] = &liveUpsert{
			record:             *best,
			receivedAtMs:       receivedAtMS,
			expectedIntervalMs: batch.ExpectedIntervalMs,
			hostname:           batch.Hostname,
		}
	}
	return nil
}

// knownForBatch returns the catalog's known series for one machine during a
// flush. The pending additions are part of the view, so the cache is rebuilt
// only when the batch created a new series.
func (t *Telemetry) knownForBatch(state *flushState, machineID string, pending *pendingSeries) []KnownSeries {
	if cached, ok := state.known[machineID]; ok && state.knownPending[machineID] == len(pending.added) {
		return cached
	}
	known := t.catalog.knownFor(machineID, pending)
	state.known[machineID] = known
	state.knownPending[machineID] = len(pending.added)
	return known
}

func (t *Telemetry) latestCursor(tx *Tx, machineID string, state *flushState) (*liveCursor, error) {
	if state.cursorRead[machineID] {
		return state.cursors[machineID], nil
	}
	var (
		runID        string
		sequence     int64
		capturedAtMS int64
	)
	err := tx.QueryRow(
		"SELECT run_id, seq, captured_at_ms FROM machine_latest WHERE machine_id = ?", machineID,
	).Scan(&runID, &sequence, &capturedAtMS)
	state.cursorRead[machineID] = true
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapError(err)
	}
	cursor := &liveCursor{runID: runID, sequence: sequence, capturedAtMs: capturedAtMS}
	state.cursors[machineID] = cursor
	return cursor, nil
}

func addSummaryPoint(summaries map[int64]map[int64]*summaryAggregate, seriesID, tsMS int64, value *float64) {
	bucket := tsMS / summaryStepMS * summaryStepMS
	buckets := summaries[seriesID]
	if buckets == nil {
		buckets = make(map[int64]*summaryAggregate)
		summaries[seriesID] = buckets
	}
	aggregate := buckets[bucket]
	if aggregate == nil {
		aggregate = &summaryAggregate{}
		buckets[bucket] = aggregate
	}
	if value == nil {
		aggregate.nulls++
		return
	}
	if aggregate.count == 0 {
		aggregate.min = *value
		aggregate.max = *value
	} else {
		if *value < aggregate.min {
			aggregate.min = *value
		}
		if *value > aggregate.max {
			aggregate.max = *value
		}
	}
	aggregate.sum += *value
	aggregate.count++
}

const rollupUpsertTemplate = "INSERT INTO %s (series_id, bucket_ms, min, max, sum, count, null_count) " +
	"VALUES (?, ?, ?, ?, ?, ?, ?) " +
	"ON CONFLICT(series_id, bucket_ms) DO UPDATE SET " +
	"min = CASE WHEN excluded.count = 0 THEN min WHEN min IS NULL THEN excluded.min ELSE MIN(min, excluded.min) END, " +
	"max = CASE WHEN excluded.count = 0 THEN max WHEN max IS NULL THEN excluded.max ELSE MAX(max, excluded.max) END, " +
	"sum = sum + excluded.sum, count = count + excluded.count, null_count = null_count + excluded.null_count"

func writeSummaries(tx *Tx, summaries map[int64]map[int64]*summaryAggregate) error {
	if len(summaries) == 0 {
		return nil
	}
	tables := []string{"points_10s", "points_1m", "points_1h"}
	statements := make([]*sql.Stmt, len(tables))
	for index, table := range tables {
		statement, err := tx.Prepare(fmt.Sprintf(rollupUpsertTemplate, table))
		if err != nil {
			for _, opened := range statements[:index] {
				_ = opened.Close()
			}
			return err
		}
		statements[index] = statement
	}
	defer func() {
		for _, statement := range statements {
			_ = statement.Close()
		}
	}()
	exec := func(tier int, seriesID, bucketMS int64, aggregate *summaryAggregate) error {
		var minimum, maximum any
		if aggregate.count > 0 {
			minimum = aggregate.min
			maximum = aggregate.max
		}
		if _, err := statements[tier].Exec(seriesID, bucketMS, minimum, maximum, aggregate.sum, aggregate.count, aggregate.nulls); err != nil {
			return mapError(err)
		}
		return nil
	}

	// Walk 10-second buckets in (series, bucket) order. Within one series the
	// minute and hour targets are then non-decreasing, so each coarser tier
	// can fold consecutive source buckets into one pending aggregate and issue
	// a single UPSERT per (series_id, target bucket) instead of one per source
	// bucket. The pending aggregates are copies; the caller's map is not
	// modified.
	seriesIDs := make([]int64, 0, len(summaries))
	for seriesID := range summaries {
		seriesIDs = append(seriesIDs, seriesID)
	}
	sort.Slice(seriesIDs, func(i, j int) bool { return seriesIDs[i] < seriesIDs[j] })
	steps := [3]int64{summaryStepMS, history1mStepMS, history1hStepMS}
	var buckets []int64
	for _, seriesID := range seriesIDs {
		buckets = buckets[:0]
		for bucketMS := range summaries[seriesID] {
			buckets = append(buckets, bucketMS)
		}
		sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })
		var pending [3]summaryAggregate
		var pendingBucket [3]int64
		var havePending [3]bool
		for _, bucketMS := range buckets {
			aggregate := summaries[seriesID][bucketMS]
			if err := exec(0, seriesID, bucketMS, aggregate); err != nil {
				return err
			}
			for tier := 1; tier < len(steps); tier++ {
				target := bucketMS / steps[tier] * steps[tier]
				if havePending[tier] && pendingBucket[tier] != target {
					if err := exec(tier, seriesID, pendingBucket[tier], &pending[tier]); err != nil {
						return err
					}
					havePending[tier] = false
				}
				if !havePending[tier] {
					pending[tier] = *aggregate
					pendingBucket[tier] = target
					havePending[tier] = true
					continue
				}
				mergeSummaryAggregate(&pending[tier], aggregate)
			}
		}
		for tier := 1; tier < len(steps); tier++ {
			if havePending[tier] {
				if err := exec(tier, seriesID, pendingBucket[tier], &pending[tier]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// mergeSummaryAggregate folds from into into with the same rules the rollup
// UPSERT applies to a stored row: an aggregate with no non-NULL readings
// contributes only its null count and never its zero-valued min/max.
func mergeSummaryAggregate(into *summaryAggregate, from *summaryAggregate) {
	if from.count > 0 {
		if into.count == 0 {
			into.min = from.min
			into.max = from.max
		} else {
			if from.min < into.min {
				into.min = from.min
			}
			if from.max > into.max {
				into.max = from.max
			}
		}
	}
	into.sum += from.sum
	into.count += from.count
	into.nulls += from.nulls
}

func (t *Telemetry) preflight() error {
	if t.maxPhysical <= 0 {
		return nil
	}
	// Reserve headroom for the transaction and its WAL frames. Without it a
	// commit could cross the cap between preflight and COMMIT, and the hub
	// would report a size it promised to keep.
	headroom := t.maxPhysical / 8
	if headroom < 64*1024 {
		headroom = 64 * 1024
	}
	if headroom > 1024*1024 {
		headroom = 1024 * 1024
	}
	bytes := t.db.SizeBytes()
	physical := t.db.PhysicalLimit()
	pageFull := physical != nil && physical.PageCount >= physical.MaxPageCount && physical.FreePages == 0
	if bytes+headroom >= t.maxPhysical || pageFull {
		t.retention.relievePressure(t.ctx, 4)
		bytes = t.db.SizeBytes()
		physical = t.db.PhysicalLimit()
		pageFull = physical != nil && physical.PageCount >= physical.MaxPageCount && physical.FreePages == 0
	}
	if bytes+headroom >= t.maxPhysical || pageFull {
		return &Error{Code: CodeStorageFull, Message: "storage disk budget reached", RetryAfter: failureRetry}
	}
	return nil
}

func (t *Telemetry) finish(jobs []*ingestJob, err error) {
	t.queueMu.Lock()
	for _, job := range jobs {
		if job.batch != nil {
			t.queuedRecords -= len(job.batch.Records)
			t.queuedBatches--
		}
	}
	t.queueMu.Unlock()

	code := CodeOf(err)
	reason := "storage write failed"
	retry := failureRetry
	if code == CodeStorageFull || code == CodeStoragePressure {
		reason = "storage disk budget reached"
	} else if code == CodeBusy {
		reason = "storage is busy"
		retry = time.Second
	}
	t.statsMu.Lock()
	t.writeFailures++
	if code == CodeStorageFull || code == CodeStoragePressure {
		t.diskFull++
		t.degraded = true
	}
	t.lastWriteError = sanitizeWriteError(err)
	t.statsMu.Unlock()

	for _, job := range jobs {
		if job.barrier != nil {
			job.barrier <- err
			continue
		}
		job.result <- jobResult{err: &Error{Code: CodeBusy, Message: reason, RetryAfter: retry, Err: err}}
	}
}

func (t *Telemetry) resolve(jobs []*ingestJob) {
	t.queueMu.Lock()
	for _, job := range jobs {
		if job.batch != nil {
			t.queuedRecords -= len(job.batch.Records)
			t.queuedBatches--
		}
	}
	t.queueMu.Unlock()
	for _, job := range jobs {
		if job.barrier != nil {
			job.barrier <- nil
			continue
		}
		if job.superseded {
			job.result <- jobResult{err: ErrCredentialSuperseded}
			continue
		}
		job.result <- jobResult{result: IngestResult{
			Accepted:   job.accepted,
			Rejected:   job.rejected,
			NewLive:    job.newLive,
			ReceivedAt: job.receivedAt,
		}}
	}
}

// Stats reports ingestion, pressure and retention state.
func (t *Telemetry) Stats() TelemetryStats {
	t.queueMu.Lock()
	queuedRecords := t.queuedRecords
	queuedBatches := t.queuedBatches
	t.queueMu.Unlock()
	t.statsMu.Lock()
	stats := TelemetryStats{
		QueuedRecords:     queuedRecords,
		QueuedBatches:     queuedBatches,
		LastFlush:         t.lastFlush,
		LastFlushDuration: t.lastFlushDur,
		CommittedRecords:  t.committed,
		DuplicateRecords:  t.duplicates,
		RejectedRecords:   t.rejected,
		SupersededBatches: t.superseded,
		SupersededRecords: t.supersededRecs,
		WriteFailures:     t.writeFailures,
		DiskFullFailures:  t.diskFull,
		LastWriteError:    t.lastWriteError,
		Degraded:          t.degraded,
	}
	t.statsMu.Unlock()
	stats.DBBytes = t.db.SizeBytes()
	stats.DBWALBytes = t.db.WALBytes()
	stats.DBLimitBytes = t.maxPhysical
	stats.Retention = t.retention.statusSnapshot()
	if stats.Retention.Pressure {
		stats.Degraded = true
	}
	return stats
}

// LatestLive is the persisted latest LIVE record for one machine.
type LatestLive struct {
	MachineID          string
	CollectorID        string
	RunID              string
	BootID             string
	Sequence           int64
	CapturedAt         time.Time
	ReceivedAt         time.Time
	ExpectedIntervalMs *int64
	Hostname           *string
	Payload            protocol.MetricsPayload
}

// LatestLive returns every persisted latest LIVE record for restart recovery.
func (t *Telemetry) LatestLive(ctx context.Context) ([]LatestLive, error) {
	rows, err := t.db.Query(ctx,
		"SELECT machine_id, collector_id, run_id, boot_id, seq, captured_at_ms, received_at_ms, expected_interval_ms, hostname, payload_json "+
			"FROM machine_latest ORDER BY machine_id",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []LatestLive
	for rows.Next() {
		var (
			live                     LatestLive
			capturedAtMS, receivedMS int64
			expected                 sql.NullInt64
			hostname                 sql.NullString
			payloadJSON              string
		)
		if err := rows.Scan(
			&live.MachineID, &live.CollectorID, &live.RunID, &live.BootID, &live.Sequence,
			&capturedAtMS, &receivedMS, &expected, &hostname, &payloadJSON,
		); err != nil {
			return nil, mapError(err)
		}
		live.CapturedAt = msToTime(capturedAtMS)
		live.ReceivedAt = msToTime(receivedMS)
		if expected.Valid {
			value := expected.Int64
			live.ExpectedIntervalMs = &value
		}
		if hostname.Valid {
			value := hostname.String
			live.Hostname = &value
		}
		payload, err := protocol.ParseMetricsPayload([]byte(payloadJSON))
		if err != nil {
			return nil, wrapStorageError(CodeUnavailable, err, "parse stored metrics payload")
		}
		live.Payload = payload
		result = append(result, live)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return result, nil
}

// StoredInventory is the persisted inventory plus its capture times.
type StoredInventory struct {
	CapturedAt time.Time
	ReceivedAt time.Time
	Inventory  protocol.Inventory
}

// SaveInventory stores the latest inventory for one machine.
func (t *Telemetry) SaveInventory(ctx context.Context, machineID string, capturedAt time.Time, inventory protocol.Inventory) error {
	if !identifierPattern.MatchString(machineID) {
		return invalidError("machine id is invalid")
	}
	if capturedAt.IsZero() {
		return invalidError("inventory capturedAt is required")
	}
	encoded, err := json.Marshal(inventory)
	if err != nil {
		return wrapStorageError(CodeInvalid, err, "encode inventory")
	}
	return t.db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(
			"INSERT INTO inventory (machine_id, captured_at_ms, received_at_ms, inventory_json) VALUES (?, ?, ?, ?) "+
				"ON CONFLICT(machine_id) DO UPDATE SET captured_at_ms = excluded.captured_at_ms, "+
				"received_at_ms = excluded.received_at_ms, inventory_json = excluded.inventory_json",
			machineID, capturedAt.UnixMilli(), t.now().UnixMilli(), string(encoded),
		)
		return err
	})
}

// LoadInventory returns the stored inventory for one machine, or nil.
func (t *Telemetry) LoadInventory(ctx context.Context, machineID string) (*StoredInventory, error) {
	var (
		stored                   StoredInventory
		capturedAtMS, receivedMS int64
		encoded                  string
	)
	err := t.db.QueryRow(ctx,
		"SELECT captured_at_ms, received_at_ms, inventory_json FROM inventory WHERE machine_id = ?", machineID,
	).Scan(&capturedAtMS, &receivedMS, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapError(err)
	}
	stored.CapturedAt = msToTime(capturedAtMS)
	stored.ReceivedAt = msToTime(receivedMS)
	inventory, err := protocol.ParseInventory([]byte(encoded))
	if err != nil {
		return nil, wrapStorageError(CodeUnavailable, err, "parse stored inventory")
	}
	stored.Inventory = inventory
	return &stored, nil
}

func validateBatch(batch Batch) error {
	if !identifierPattern.MatchString(batch.MachineID) {
		return invalidError("ingest batch machineId must be a valid identifier")
	}
	if !identifierPattern.MatchString(batch.CollectorID) {
		return invalidError("ingest batch collectorId must be a valid identifier")
	}
	if batch.Mode != protocol.SampleModeLive && batch.Mode != protocol.SampleModeReplay {
		return invalidError("ingest batch mode must be live or replay")
	}
	if batch.ExpectedIntervalMs != nil && *batch.ExpectedIntervalMs < 0 {
		return invalidError("ingest batch expectedIntervalMs must not be negative")
	}
	if batch.Hostname != nil && len(*batch.Hostname) > 255 {
		return invalidError("ingest batch hostname must be at most 255 characters")
	}
	if len(batch.Records) == 0 {
		return invalidError("ingest batch must contain at least one record")
	}
	return nil
}

func rejectionReason(collectorID, capturedAt string, receivedAtMS, maxReplayAgeMS, clockSkewMS int64) string {
	if collectorID == "" {
		return "collectorId is required"
	}
	capturedMS, err := parseCapturedAt(capturedAt)
	if err != nil {
		return "capturedAt is not a parseable timestamp"
	}
	if capturedMS < receivedAtMS-maxReplayAgeMS {
		return RejectionCapturedTooOld
	}
	if capturedMS > receivedAtMS+clockSkewMS {
		return RejectionCapturedInFuture
	}
	return ""
}

func parseCapturedAt(value string) (int64, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0, fmt.Errorf("timestamp is not RFC 3339")
	}
	return parsed.UnixMilli(), nil
}

func parseWireTime(value string) int64 {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}

func isNewer(candidate, latest *liveCursor) bool {
	if candidate.runID == latest.runID {
		return candidate.sequence > latest.sequence
	}
	return candidate.capturedAtMs > latest.capturedAtMs
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func sanitizeWriteError(err error) string {
	message := err.Error()
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 200 {
		message = message[:200]
	}
	return message
}
