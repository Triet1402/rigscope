package delivery

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

var queueTestT0 = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

var queueTestIdentity = QueueIdentity{CollectorID: "collector-1", RunID: "run-a", BootID: "boot-a"}

func testPayload() protocol.MetricsPayload {
	return protocol.MetricsPayload{
		CPU:     protocol.CpuAvailable{UsagePercent: 12.5, PerCore: []*float64{protocol.Ptr(10.0)}},
		Memory:  protocol.MemoryAvailable{UsedBytes: 1_000, TotalBytes: 8_000},
		Swap:    protocol.Unavailable{Reason: "no swap"},
		GPUs:    protocol.GpusAvailable{},
		Disks:   protocol.DisksAvailable{},
		Network: protocol.NetworkAvailable{},
	}
}

func metricsRecord(sequence int64, capturedAt time.Time) protocol.MetricsRecord {
	return protocol.MetricsRecord{
		RecordEnvelope: protocol.RecordEnvelope{
			CollectorID: "collector-1",
			RunID:       "run-a",
			BootID:      "boot-a",
			Sequence:    sequence,
			CapturedAt:  formatTimestamp(capturedAt),
		},
		Payload: testPayload(),
	}
}

func largeRecord(sequence int64, capturedAt time.Time) protocol.MetricsRecord {
	record := metricsRecord(sequence, capturedAt)
	perCore := make([]*float64, 1024)
	for index := range perCore {
		perCore[index] = protocol.Ptr(0.0)
	}
	cpu := record.Payload.CPU.(protocol.CpuAvailable)
	cpu.PerCore = perCore
	record.Payload.CPU = cpu
	return record
}

type queueFixture struct {
	queue *queue
	now   *time.Time
	seq   int64
}

func newQueueFixture(t *testing.T, limits QueueLimits) *queueFixture {
	t.Helper()
	fixture := &queueFixture{now: &queueTestT0, seq: 1000}
	fixture.queue = newQueue(queueOptions{
		limits:   limits,
		identity: queueTestIdentity,
		allocate: func() int64 { fixture.seq++; return fixture.seq },
		clock:    func() time.Time { return *fixture.now },
	})
	t.Cleanup(fixture.queue.close)
	return fixture
}

func (f *queueFixture) append(t *testing.T, record protocol.TelemetryRecord) {
	t.Helper()
	if err := f.queue.append(record); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func allRecords(queue *queue) []protocol.TelemetryRecord {
	live := queue.takeLive()
	rest := queue.takeReplay(10_000, 1<<30)
	queue.releaseAll()
	var records []protocol.TelemetryRecord
	if live != nil {
		records = append(records, live.records...)
	}
	if rest != nil {
		records = append(records, rest.records...)
	}
	return records
}

func gapRecords(records []protocol.TelemetryRecord) []protocol.GapRecord {
	var gaps []protocol.GapRecord
	for _, record := range records {
		if gap, ok := record.(protocol.GapRecord); ok {
			gaps = append(gaps, gap)
		}
	}
	return gaps
}

func metricSequences(records []protocol.TelemetryRecord) []int64 {
	var sequences []int64
	for _, record := range records {
		if metrics, ok := record.(protocol.MetricsRecord); ok {
			sequences = append(sequences, metrics.Sequence)
		}
	}
	return sequences
}

func jsonBytes(t *testing.T, value any) int {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return len(data)
}

func TestLiveSlotTakesNewestAndKeepsItReplayable(t *testing.T) {
	fixture := newQueueFixture(t, DefaultQueueLimits())
	fixture.append(t, metricsRecord(1, queueTestT0))
	fixture.append(t, metricsRecord(2, queueTestT0))

	live := fixture.queue.takeLive()
	if live == nil {
		t.Fatal("no live record")
	}
	if metrics := live.records[0].(protocol.MetricsRecord); metrics.Sequence != 2 {
		t.Fatalf("live sequence = %d, want the newest (2)", metrics.Sequence)
	}
	if fixture.queue.takeLive() != nil {
		t.Fatal("the live slot must drain after one take")
	}
	// While the live send is outstanding its entry is reserved, so replay
	// cannot hand out a second copy of sequence 2.
	if replay := fixture.queue.takeReplay(10, 1<<30); replay == nil || len(replay.records) != 1 {
		t.Fatalf("replay during the live send = %v, want only sequence 1", replay)
	} else if got := metricSequences(replay.records); len(got) != 1 || got[0] != 1 {
		t.Fatalf("replay sequences during the live send = %v, want [1]", got)
	}
	fixture.queue.releaseAll()
	// A lost ack releases the live entry and the next replay includes it again.
	fixture.queue.release(live.refs())
	replay := fixture.queue.takeReplay(10, 1<<30)
	if got := metricSequences(replay.records); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("replay sequences = %v, want [1 2]: an unacknowledged live send is replayed", got)
	}
}

func TestReplayIsBoundedByCountAndBytes(t *testing.T) {
	fixture := newQueueFixture(t, DefaultQueueLimits())
	for sequence := int64(1); sequence <= 5; sequence++ {
		fixture.append(t, metricsRecord(sequence, queueTestT0))
	}
	if got := fixture.queue.takeReplay(2, 1<<30); got == nil || len(got.records) != 2 {
		t.Fatalf("count bound returned %v records, want 2", got)
	}
	fixture.queue.releaseAll()
	if got := fixture.queue.takeReplay(10, 1); got == nil || len(got.records) != 1 {
		t.Fatalf("byte bound returned %v records, want at least one", got)
	}
	fixture.queue.releaseAll()
}

func TestInFlightRecordsAreNeverEvicted(t *testing.T) {
	one := jsonBytes(t, metricsRecord(1, queueTestT0))
	limits := DefaultQueueLimits()
	limits.MaxAge = time.Second
	limits.MaxBytes = one
	fixture := newQueueFixture(t, limits)
	fixture.append(t, metricsRecord(1, queueTestT0))
	reserved := fixture.queue.takeLive()
	if reserved == nil {
		t.Fatal("no record reserved")
	}

	*fixture.now = queueTestT0.Add(time.Minute)
	fixture.queue.enforceLimits()
	stats := fixture.queue.status()
	if stats.InFlightRecords != 1 || stats.PendingRecords != 0 {
		t.Fatalf("in-flight record was evicted: %+v", stats)
	}
	fixture.queue.release(reserved.refs())
	if stats := fixture.queue.status(); stats.PendingRecords != 1 {
		t.Fatalf("released record did not return to pending: %+v", stats)
	}
}

func TestAcceptedAndRejectedRecordsLeaveTheQueue(t *testing.T) {
	fixture := newQueueFixture(t, DefaultQueueLimits())
	fixture.append(t, metricsRecord(1, queueTestT0))
	fixture.append(t, metricsRecord(2, queueTestT0))
	// Reserve both records, mirroring a batch selection.
	fixture.queue.takeReplay(10, 1<<30)
	fixture.queue.acknowledge(
		[]protocol.RecordRef{{RunID: "run-a", Sequence: 1}},
		[]protocol.RecordRef{{RunID: "run-a", Sequence: 2}},
	)
	stats := fixture.queue.status()
	if stats.PendingRecords != 0 || stats.InFlightRecords != 0 || stats.Replaying {
		t.Fatalf("queue not empty after ack: %+v", stats)
	}
	if stats.LastAckAt == nil {
		t.Fatal("lastAckAt was not recorded")
	}
	// A duplicate acknowledgement is harmless.
	fixture.queue.acknowledge([]protocol.RecordRef{{RunID: "run-a", Sequence: 1}}, nil)
	if fixture.queue.status().LastAckAt == nil {
		t.Fatal("duplicate ack should still record an ack time")
	}
}

func TestAgeEvictionDropsOldestAndFoldsOneGap(t *testing.T) {
	limits := DefaultQueueLimits()
	limits.MaxAge = 5 * time.Second
	fixture := newQueueFixture(t, limits)
	fixture.append(t, metricsRecord(1, queueTestT0))
	fixture.append(t, metricsRecord(2, queueTestT0.Add(time.Second)))
	fixture.append(t, metricsRecord(3, queueTestT0.Add(40*time.Second)))
	*fixture.now = queueTestT0.Add(41 * time.Second)
	fixture.queue.enforceLimits()

	records := allRecords(fixture.queue)
	if got := metricSequences(records); len(got) != 1 || got[0] != 3 {
		t.Fatalf("retained metrics = %v, want [3]", got)
	}
	gaps := gapRecords(records)
	if len(gaps) != 1 || gaps[0].Payload.Reason != protocol.GapOutboxAge {
		t.Fatalf("age gaps = %+v", gaps)
	}
	if gaps[0].Payload.DroppedRecords != 2 {
		t.Fatalf("droppedRecords = %d, want 2", gaps[0].Payload.DroppedRecords)
	}
	if stats := fixture.queue.status(); stats.DroppedRecords != 2 {
		t.Fatalf("droppedRecords = %d, want 2", stats.DroppedRecords)
	}
}

func TestByteEvictionKeepsTheNewestAndReportsAGap(t *testing.T) {
	one := jsonBytes(t, metricsRecord(1, queueTestT0))
	limits := DefaultQueueLimits()
	limits.MaxBytes = one * 3
	fixture := newQueueFixture(t, limits)
	for sequence := int64(1); sequence <= 8; sequence++ {
		fixture.append(t, metricsRecord(sequence, queueTestT0))
	}
	records := allRecords(fixture.queue)
	sequences := metricSequences(records)
	foundNewest := false
	for _, sequence := range sequences {
		if sequence == 8 {
			foundNewest = true
		}
	}
	if !foundNewest {
		t.Fatalf("the newest sample was evicted: %v", sequences)
	}
	gaps := gapRecords(records)
	if len(gaps) != 1 || gaps[0].Payload.Reason != protocol.GapOutboxBytes {
		t.Fatalf("byte gaps = %+v", gaps)
	}
}

func TestOversizedRecordIsDroppedWithAGap(t *testing.T) {
	limits := DefaultQueueLimits()
	limits.MaxBytes = 2048
	fixture := newQueueFixture(t, limits)
	fixture.append(t, largeRecord(1, queueTestT0))
	records := allRecords(fixture.queue)
	if len(metricSequences(records)) != 0 {
		t.Fatal("an oversized record was retained")
	}
	gaps := gapRecords(records)
	if len(gaps) != 1 || gaps[0].Payload.Reason != protocol.GapOutboxBytes || gaps[0].Payload.DroppedRecords != 1 {
		t.Fatalf("oversized gaps = %+v", gaps)
	}
}

func TestGapsCoalescePerReasonAndMergeUnderTheCap(t *testing.T) {
	one := jsonBytes(t, metricsRecord(1, queueTestT0))
	limits := DefaultQueueLimits()
	limits.MaxAge = time.Second
	limits.MaxBytes = one * 4
	limits.MaxGapRecords = 2
	fixture := newQueueFixture(t, limits)

	// Alternate age eviction and oversized-record rejection to create gaps
	// with different reasons.
	for index := 0; index < 6; index++ {
		*fixture.now = queueTestT0.Add(time.Duration(index) * 5 * time.Second)
		fixture.append(t, metricsRecord(int64(100+index), *fixture.now))
		*fixture.now = fixture.now.Add(6 * time.Second)
		if index%2 == 0 {
			fixture.append(t, largeRecord(int64(200+index), *fixture.now))
		}
		fixture.queue.enforceLimits()
	}
	gaps := gapRecords(allRecords(fixture.queue))
	if len(gaps) > limits.MaxGapRecords {
		t.Fatalf("retained %d gaps, cap is %d", len(gaps), limits.MaxGapRecords)
	}
	var dropped int64
	for _, gap := range gaps {
		dropped += gap.Payload.DroppedRecords
	}
	if dropped < 3 {
		t.Fatalf("merged gaps lost the dropped count: %d", dropped)
	}
}

func TestRecordsHandedOutAreDetachedCopies(t *testing.T) {
	fixture := newQueueFixture(t, DefaultQueueLimits())
	fixture.append(t, metricsRecord(1, queueTestT0))
	live := fixture.queue.takeLive()
	taken, ok := live.records[0].(protocol.MetricsRecord)
	if !ok {
		t.Fatal("expected a metrics record")
	}
	taken.CapturedAt = "mutated"
	fixture.queue.release(live.refs())
	again := fixture.queue.takeReplay(1, 1<<30).records[0].(protocol.MetricsRecord)
	if again.CapturedAt == "mutated" {
		t.Fatal("a caller mutated what is still owed")
	}
}

func TestRestartLosesTheQueueWithoutReportingLocalDrops(t *testing.T) {
	first := newQueueFixture(t, DefaultQueueLimits())
	first.append(t, metricsRecord(1, queueTestT0))
	first.append(t, metricsRecord(2, queueTestT0))
	first.queue.close()

	second := newQueueFixture(t, DefaultQueueLimits())
	if stats := second.queue.status(); stats.PendingRecords != 0 || stats.DroppedRecords != 0 {
		t.Fatalf("restart state = %+v, want empty and no local drops", stats)
	}
	if records := allRecords(second.queue); len(records) != 0 {
		t.Fatalf("records survived a restart: %d", len(records))
	}
}

func TestForeignRecordIdentityIsRefused(t *testing.T) {
	fixture := newQueueFixture(t, DefaultQueueLimits())
	foreign := metricsRecord(1, queueTestT0)
	foreign.RunID = "other-run"
	if err := fixture.queue.append(foreign); err == nil {
		t.Fatal("a record from another run was accepted")
	}
	foreign = metricsRecord(1, queueTestT0)
	foreign.BootID = "other-boot"
	if err := fixture.queue.append(foreign); err == nil {
		t.Fatal("a record from another boot was accepted")
	}
	if stats := fixture.queue.status(); stats.PendingRecords != 0 || stats.DroppedRecords != 0 {
		t.Fatalf("refused records changed the queue: %+v", stats)
	}
}

func TestQueueStatusReportsMemoryModeAndGapCount(t *testing.T) {
	limits := DefaultQueueLimits()
	limits.MaxAge = time.Second
	fixture := newQueueFixture(t, limits)
	fixture.append(t, metricsRecord(1, queueTestT0))
	*fixture.now = queueTestT0.Add(time.Minute)
	fixture.queue.enforceLimits()
	stats := fixture.queue.status()
	if stats.GapRecords != 1 {
		t.Fatalf("gap records = %d, want 1", stats.GapRecords)
	}
	delivery := deliveryStatusOf(stats)
	if delivery.QueueMode == nil || *delivery.QueueMode != protocol.QueueModeMemory {
		t.Fatalf("queue mode = %v, want memory", delivery.QueueMode)
	}
}

func TestNewRequiresOneSharedSequenceAllocator(t *testing.T) {
	_, err := New(Config{
		HubURL:     "ws://127.0.0.1:1/api/collector/connect",
		Credential: "token",
		Identity:   Identity{CollectorID: "c", RunID: "r"},
	})
	if err == nil || !strings.Contains(err.Error(), "AllocateSequence") {
		t.Fatalf("New without an allocator = %v, want a refusal", err)
	}
}

func TestGapsAndMetricsShareOneSequenceSpace(t *testing.T) {
	limits := DefaultQueueLimits()
	limits.MaxAge = time.Second
	next := int64(0)
	fixture := &queueFixture{now: &queueTestT0}
	fixture.queue = newQueue(queueOptions{
		limits:   limits,
		identity: queueTestIdentity,
		allocate: func() int64 { next++; return next },
		clock:    func() time.Time { return *fixture.now },
	})
	t.Cleanup(fixture.queue.close)

	// The sampler takes sequence 1 from the shared allocator.
	first := metricsRecord(next+1, queueTestT0)
	next++
	fixture.append(t, first)
	*fixture.now = queueTestT0.Add(time.Minute)
	fixture.queue.enforceLimits()

	records := allRecords(fixture.queue)
	gaps := gapRecords(records)
	if len(gaps) != 1 {
		t.Fatalf("gaps = %d, want 1", len(gaps))
	}
	if gaps[0].Sequence != 2 {
		t.Fatalf("gap sequence = %d, want the shared allocator's next value 2", gaps[0].Sequence)
	}
	seen := map[int64]bool{}
	for _, record := range records {
		ref, _ := refOf(record)
		if seen[ref.Sequence] {
			t.Fatalf("sequence %d appears in two records", ref.Sequence)
		}
		seen[ref.Sequence] = true
	}
}

func TestDuplicateRecordRefIsRejectedLocally(t *testing.T) {
	fixture := newQueueFixture(t, DefaultQueueLimits())
	fixture.append(t, metricsRecord(1, queueTestT0))
	if err := fixture.queue.append(metricsRecord(1, queueTestT0)); err == nil ||
		!strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate metrics ref = %v, want a refusal", err)
	}
	gap := protocol.GapRecord{
		RecordEnvelope: protocol.RecordEnvelope{
			CollectorID: "collector-1",
			RunID:       "run-a",
			BootID:      "boot-a",
			Sequence:    1,
			CapturedAt:  formatTimestamp(queueTestT0),
		},
		Payload: protocol.GapPayload{
			FromCapturedAt: formatTimestamp(queueTestT0),
			ToCapturedAt:   formatTimestamp(queueTestT0),
			DroppedRecords: 1,
			Reason:         protocol.GapOutboxBytes,
		},
	}
	if err := fixture.queue.append(gap); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("gap colliding with a metric ref = %v, want a refusal", err)
	}
}

// TestReservationSurvivesEnqueueBetweenSelectionAndSend uses a hook (not a
// sleep) to run an Enqueue after takeReplay selected its batch. The reserved
// entry must not be evicted or folded into the gap the new record triggers.
func TestReservationSurvivesEnqueueBetweenSelectionAndSend(t *testing.T) {
	one := jsonBytes(t, metricsRecord(1, queueTestT0))
	limits := DefaultQueueLimits()
	limits.MaxBytes = one * 2
	fixture := newQueueFixture(t, limits)
	fixture.append(t, metricsRecord(1, queueTestT0))
	fixture.append(t, metricsRecord(2, queueTestT0.Add(time.Second)))

	var reserved *reservation
	fixture.queue.afterReserve = func() {
		// This runs after selection and before the caller sends. It forces
		// byte eviction of every pending entry it can take.
		for sequence := int64(3); sequence <= 5; sequence++ {
			fixture.append(t, metricsRecord(sequence, queueTestT0.Add(time.Duration(sequence)*time.Second)))
		}
	}
	reserved = fixture.queue.takeReplay(1, 1<<30)
	if reserved == nil || len(reserved.records) != 1 {
		t.Fatalf("reservation = %v", reserved)
	}
	ref, _ := refOf(reserved.records[0])
	if ref.Sequence != 1 {
		t.Fatalf("reserved sequence = %d, want 1", ref.Sequence)
	}
	fixture.queue.mu.Lock()
	entry := fixture.queue.byRef[refKey(ref.RunID, ref.Sequence)]
	stillReserved := entry != nil && entry.state == stateInFlight
	fixture.queue.mu.Unlock()
	if !stillReserved {
		t.Fatal("an Enqueue between selection and send evicted the reserved record")
	}
	if stats := fixture.queue.status(); stats.InFlightRecords != 1 {
		t.Fatalf("in-flight records = %d, want 1", stats.InFlightRecords)
	}
	fixture.queue.release(reserved.refs())
}

func TestTrimmedReservationEntriesAreReleased(t *testing.T) {
	fixture := newQueueFixture(t, DefaultQueueLimits())
	fixture.append(t, metricsRecord(1, queueTestT0))
	fixture.append(t, metricsRecord(2, queueTestT0))
	live := fixture.queue.takeLive()
	rest := fixture.queue.takeReplay(1, 1<<30)
	if live == nil || rest == nil || len(rest.entries) != 1 {
		t.Fatalf("reservations = %v, %v", live, rest)
	}
	// The sender keeps the live record and trims the replay entry because the
	// encoded batch is too big.
	fixture.queue.releaseEntries(rest.entries)
	stats := fixture.queue.status()
	if stats.InFlightRecords != 1 || stats.PendingRecords != 1 {
		t.Fatalf("after trimming = %+v, want one in flight and one pending", stats)
	}
}

func TestOfferedGapIdentityIsImmutableUnderTheCap(t *testing.T) {
	limits := DefaultQueueLimits()
	limits.MaxAge = time.Second
	limits.MaxGapRecords = 1
	fixture := newQueueFixture(t, limits)

	// Create the first gap and offer it to the hub.
	fixture.append(t, metricsRecord(1, queueTestT0))
	*fixture.now = queueTestT0.Add(10 * time.Second)
	fixture.queue.enforceLimits()
	reserved := fixture.queue.takeReplay(100, 1<<30)
	if reserved == nil {
		t.Fatal("no gap reserved")
	}
	var offered protocol.GapRecord
	for _, record := range reserved.records {
		if gap, ok := record.(protocol.GapRecord); ok {
			offered = gap
		}
	}
	if offered.Sequence == 0 {
		t.Fatal("the first gap was not reserved")
	}
	fixture.queue.mu.Lock()
	offeredRaw := append([]byte(nil), fixture.queue.byRef[refKey("run-a", offered.Sequence)].raw...)
	fixture.queue.mu.Unlock()

	// Create more gaps while the first is offered. The cap cannot rewrite the
	// offered gap, so it may stay over the cap until the ack arrives.
	fixture.append(t, metricsRecord(2, *fixture.now))
	*fixture.now = fixture.now.Add(10 * time.Second)
	fixture.queue.enforceLimits()
	fixture.append(t, metricsRecord(3, *fixture.now))
	*fixture.now = fixture.now.Add(10 * time.Second)
	fixture.queue.enforceLimits()

	fixture.queue.mu.Lock()
	entry := fixture.queue.byRef[refKey("run-a", offered.Sequence)]
	if entry == nil {
		fixture.queue.mu.Unlock()
		t.Fatal("the offered gap disappeared")
	}
	unchanged := string(entry.raw) == string(offeredRaw)
	fixture.queue.mu.Unlock()
	if !unchanged {
		t.Fatal("gap-cap compaction rewrote an offered gap; its identity and payload must be immutable")
	}
}

func TestDropUnsendableAccountsAndEmitsAGap(t *testing.T) {
	limits := DefaultQueueLimits()
	limits.MaxBytes = 1 << 20
	fixture := newQueueFixture(t, limits)
	fixture.append(t, largeRecord(1, queueTestT0))
	live := fixture.queue.takeLive()
	if live == nil {
		t.Fatal("no record reserved")
	}
	ref, _ := refOf(live.records[0])
	fixture.queue.dropUnsendable(ref)
	stats := fixture.queue.status()
	if stats.DroppedRecords != 1 || stats.PendingRecords != 1 {
		t.Fatalf("after dropping an unsendable record = %+v", stats)
	}
	gaps := gapRecords(allRecords(fixture.queue))
	if len(gaps) != 1 || gaps[0].Payload.Reason != protocol.GapOutboxBytes || gaps[0].Payload.DroppedRecords != 1 {
		t.Fatalf("unsendable gap = %+v", gaps)
	}
}

func TestEmptyCaptureTimesDoNotBreakEviction(t *testing.T) {
	fixture := newQueueFixture(t, DefaultQueueLimits())
	record := metricsRecord(1, queueTestT0)
	record.CapturedAt = formatTimestamp(queueTestT0)
	fixture.append(t, record)
	// A gap with a wide interval remains valid; the queue must not panic on a
	// zero timestamp during coalescing.
	fixture.queue.mu.Lock()
	fixture.queue.writeGapLocked(protocol.GapWriteFailure, time.Time{}, time.Time{}, 1)
	fixture.queue.mu.Unlock()
	if len(gapRecords(allRecords(fixture.queue))) != 1 {
		t.Fatal("expected one gap")
	}
}
