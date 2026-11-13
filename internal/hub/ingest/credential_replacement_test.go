package ingest

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
	"github.com/coder/websocket"
)

// enrollReplacement claims a replacement grant through the real enrollment
// handler, with the same gateway callback the composition root installs. It
// returns the issued credential token.
func enrollReplacement(t *testing.T, harness *gatewayHarness, oldMachineID, newCollectorID string) string {
	t.Helper()
	grant, err := harness.registry.CreateGrant(context.Background(), "owner", storage.GrantOptions{
		ReplaceMachineID: oldMachineID,
	})
	if err != nil {
		t.Fatalf("create replacement grant: %v", err)
	}
	body, err := api.EncodeEnrollmentRequest(api.EnrollmentRequest{CollectorID: newCollectorID})
	if err != nil {
		t.Fatalf("encode enrollment: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/collector/enroll", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+grant.Token)
	handler := NewEnrollmentHandler(EnrollmentOptions{
		Registry: harness.registry,
		State:    harness.state,
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("replacement enrollment = %d, body %s", recorder.Code, recorder.Body.String())
	}
	identity, err := api.DecodeEnrollmentIdentity(recorder.Body.Bytes())
	if err != nil {
		t.Fatalf("decode replacement identity: %v", err)
	}
	return identity.Token
}

// TestReplacementEnrollmentClosesOldCredentialSessions proves that the
// enrollment path notifies the gateway: after a replacement grant issues a new
// credential, the old credential's open socket is fenced and closed instead of
// continuing to publish and receive acks.
func TestReplacementEnrollmentClosesOldCredentialSessions(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-old", MachineID: "m-replace", Token: "token-old"}
	harness := newGatewayHarness(t, identity, nil)

	old := harness.dial(identity.Token)
	defer old.CloseNow()
	internalSend(t, old, internalHello(identity.CollectorID, "run-old"))
	if _, ok := internalRead(t, old).(protocol.WelcomeMessage); !ok {
		t.Fatal("old session was not welcomed")
	}

	closed := make(chan error, 1)
	go func() {
		_, _, err := old.Read(context.Background())
		closed <- err
	}()

	enrollReplacement(t, harness, identity.MachineID, "c-new")

	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("old credential's socket stayed open after replacement")
		}
		if status := websocket.CloseStatus(err); status != ClosePolicy {
			t.Fatalf("old socket close = %v (status %d), want policy close %d", err, status, ClosePolicy)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old credential's socket stayed open after replacement")
	}
	harness.waitFor(2*time.Second, "old gateway session to close", func() bool {
		return harness.gateway.ConnectedCount() == 0 && harness.state.ConnectedCount() == 0
	})
	if _, found, err := harness.registry.Authenticate(context.Background(), identity.Token); err != nil || found {
		t.Fatalf("old credential still authenticates: found=%v err=%v", found, err)
	}
}

// TestReplacementEnrollmentUnderNewCollectorKeepsMachineLive reproduces a
// replacement grant that keeps the hub-local machine id but moves it to a new
// collector id. The enrollment handler registers the successor first and then
// publishes the old credential's invalidation; the invalidation must not follow
// a stale collector index and drop the successor's row.
func TestReplacementEnrollmentUnderNewCollectorKeepsMachineLive(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-old", MachineID: "m-replace", Token: "token-old"}
	harness := newGatewayHarness(t, identity, nil)

	old := harness.dial(identity.Token)
	defer old.CloseNow()
	internalSend(t, old, internalHello(identity.CollectorID, "run-old"))
	if _, ok := internalRead(t, old).(protocol.WelcomeMessage); !ok {
		t.Fatal("old session was not welcomed")
	}
	// Drain the socket so the gateway's close handshake cannot block the
	// enrollment request.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_, _, _ = old.Read(context.Background())
	}()

	token := enrollReplacement(t, harness, identity.MachineID, "c-new")

	if _, ok := harness.state.Machine(identity.MachineID); !ok {
		t.Fatal("replacement invalidated the new collector's machine mapping")
	}
	if got := harness.state.ConfiguredCount(); got != 1 {
		t.Fatalf("configured machines = %d, want 1", got)
	}
	if got := harness.state.MachineTelemetry(); len(got) != 1 || got[0].MachineID != identity.MachineID {
		t.Fatalf("telemetry after replacement = %+v", got)
	}

	// The newest collector connects with the recovered row still present. A
	// stale collector index would make admission return early and the machine
	// would never reappear until the hub restarted.
	connection := harness.dial(token)
	defer connection.CloseNow()
	internalSend(t, connection, internalHello("c-new", "run-new"))
	if _, ok := internalRead(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("new collector was not welcomed")
	}
	harness.waitFor(2*time.Second, "new collector session", func() bool {
		return harness.gateway.ConnectedCount() == 1
	})
	view, ok := harness.state.Machine(identity.MachineID)
	if !ok || !view.Connected || view.CollectorID != "c-new" {
		t.Fatalf("machine after new hello = %+v, present=%v", view, ok)
	}
	// A live batch still round-trips, so the mapping serves the new session.
	internalSend(t, connection, protocol.SamplesMessage{
		Mode:    protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{internalRecord(t, "c-new", "run-new", 1, time.Now())},
	})
	if _, ok := internalRead(t, connection).(protocol.AckMessage); !ok {
		t.Fatal("new collector did not receive an ack")
	}
	<-closed
}

// TestReplacementEnrollmentFencesInFlightBatchCommit pauses a batch after live
// publication, replaces the collector's credential, then releases the commit.
// The old session must not persist the already-accepted batch.
func TestReplacementEnrollmentFencesInFlightBatchCommit(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-commit", MachineID: "m-commit", Token: "token-commit"}
	paused := make(chan struct{})
	release := make(chan struct{})
	fenced := make(chan struct{})
	var pauseOnce sync.Once
	var fenceOnce sync.Once
	harness := newGatewayHarness(t, identity, func(opts *GatewayOptions) {
		opts.testBeforeCommit = func() {
			pauseOnce.Do(func() {
				close(paused)
				<-release
			})
		}
		opts.testFencedCommit = func() {
			fenceOnce.Do(func() { close(fenced) })
		}
	})

	old := harness.dial(identity.Token)
	defer old.CloseNow()
	internalSend(t, old, internalHello(identity.CollectorID, "run-old"))
	if _, ok := internalRead(t, old).(protocol.WelcomeMessage); !ok {
		t.Fatal("old session was not welcomed")
	}
	// Drain the socket so the server's close handshake cannot block the
	// enrollment request.
	closed := make(chan error, 1)
	go func() {
		_, _, err := old.Read(context.Background())
		closed <- err
	}()
	internalSend(t, old, protocol.SamplesMessage{
		Mode:    protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{internalRecord(t, identity.CollectorID, "run-old", 1, time.Now())},
	})
	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatal("batch commit never paused")
	}

	enrollReplacement(t, harness, identity.MachineID, "c-commit-new")
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("old credential's socket stayed open after replacement")
	}
	close(release)
	select {
	case <-fenced:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight commit was not fenced by the credential replacement")
	}
	var receipts int
	if err := harness.db.QueryRow(context.Background(), "SELECT COUNT(*) FROM sample_receipts").Scan(&receipts); err != nil {
		t.Fatalf("count receipts: %v", err)
	}
	if receipts != 0 {
		t.Fatalf("replaced credential committed %d sample batches", receipts)
	}
}

// TestReplacementEnrollmentDropsBatchQueuedInTelemetry pauses the write until
// after a replacement enrollment. The batch was already accepted into the
// asynchronous telemetry queue, so the transport's pre-commit credential check
// cannot see the replacement; the write transaction itself must drop the batch
// and persist neither a receipt nor machine_latest.
func TestReplacementEnrollmentDropsBatchQueuedInTelemetry(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-queued", MachineID: "m-queued", Token: "token-queued"}
	harness := newGatewayHarnessWithTelemetry(t, identity, &storage.TelemetryOptions{
		FlushInterval: 5 * time.Second,
	}, nil)

	connection := harness.dial(identity.Token)
	defer connection.CloseNow()
	internalSend(t, connection, internalHello(identity.CollectorID, "run-queued"))
	if _, ok := internalRead(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("session was not welcomed")
	}
	go func() {
		_, _, _ = connection.Read(context.Background())
	}()
	internalSend(t, connection, protocol.SamplesMessage{
		Mode:    protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{internalRecord(t, identity.CollectorID, "run-queued", 1, time.Now())},
	})
	harness.waitFor(3*time.Second, "batch to wait in the telemetry queue", func() bool {
		return harness.telemetry.Stats().QueuedBatches > 0
	})

	// Replace the credential while the batch is queued. Reusing the collector
	// id is the common reinstall path and exercises the exact-deliverable fence.
	enrollReplacement(t, harness, identity.MachineID, identity.CollectorID)

	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := harness.telemetry.Flush(flushCtx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	harness.waitFor(2*time.Second, "superseded batch accounting", func() bool {
		return harness.telemetry.Stats().SupersededBatches >= 1
	})
	var receipts, latest int
	if err := harness.db.QueryRow(context.Background(), "SELECT COUNT(*) FROM sample_receipts").Scan(&receipts); err != nil {
		t.Fatalf("count receipts: %v", err)
	}
	if err := harness.db.QueryRow(context.Background(), "SELECT COUNT(*) FROM machine_latest").Scan(&latest); err != nil {
		t.Fatalf("count machine_latest: %v", err)
	}
	if receipts != 0 || latest != 0 {
		t.Fatalf("superseded queued batch persisted receipts=%d machine_latest=%d", receipts, latest)
	}
}

// TestDelayedReplacementInvalidationKeepsNewCredentialConnected pauses after a
// replacement enrollment commits but before its invalidation is published. A
// concurrent retry recovers the same new credential and connects first; the
// delayed message must fence only the old credential, not this valid successor
// that reused the collector id.
func TestDelayedReplacementInvalidationKeepsNewCredentialConnected(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-delayed", MachineID: "m-delayed", Token: "token-delayed"}
	harness := newGatewayHarness(t, identity, nil)
	ctx := context.Background()

	grant, err := harness.registry.CreateGrant(ctx, "owner", storage.GrantOptions{
		ReplaceMachineID: identity.MachineID,
	})
	if err != nil {
		t.Fatalf("create replacement grant: %v", err)
	}
	requestID := "delayed-request"
	recoverySecret := strings.Repeat("r", 32)
	encode := func() []byte {
		body, err := api.EncodeEnrollmentRequest(api.EnrollmentRequest{
			CollectorID: identity.CollectorID, RequestID: &requestID, RecoverySecret: &recoverySecret,
		})
		if err != nil {
			t.Fatalf("encode enrollment: %v", err)
		}
		return body
	}

	paused := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan int, 1)
	var once sync.Once
	handler := NewEnrollmentHandler(EnrollmentOptions{
		Registry: harness.registry,
		State:    harness.state,
		testBeforeInvalidate: func(identity storage.Identity) {
			if identity.ReplacedCollectorID == "" {
				return
			}
			once.Do(func() {
				close(paused)
				<-release
			})
		},
	})
	enroll := func(body []byte) (int, []byte) {
		request := httptest.NewRequest(http.MethodPost, "/api/collector/enroll", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+grant.Token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code, recorder.Body.Bytes()
	}
	go func() {
		status, _ := enroll(encode())
		firstDone <- status
	}()
	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement did not pause before invalidation")
	}

	// The lost response is recoverable: the retry returns the same credential
	// instead of issuing another, and that credential connects before the
	// delayed invalidation runs.
	status, body := enroll(encode())
	if status != http.StatusCreated {
		t.Fatalf("recovery enrollment = %d, body %s", status, body)
	}
	recovered, err := api.DecodeEnrollmentIdentity(body)
	if err != nil {
		t.Fatalf("decode recovered identity: %v", err)
	}
	connection := harness.dial(recovered.Token)
	defer connection.CloseNow()
	internalSend(t, connection, internalHello(identity.CollectorID, "run-new"))
	if _, ok := internalRead(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("recovered credential was not welcomed")
	}

	close(release)
	select {
	case status := <-firstDone:
		if status != http.StatusCreated {
			t.Fatalf("first replacement enrollment = %d", status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replacement enrollment did not finish")
	}
	harness.waitFor(2*time.Second, "new credential to stay connected", func() bool {
		return harness.gateway.ConnectedCount() == 1
	})
	// A live request/ack proves the socket was not closed with 1008 and that
	// the mapping still serves the recovered credential.
	internalSend(t, connection, protocol.SamplesMessage{
		Mode:    protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{internalRecord(t, identity.CollectorID, "run-new", 1, time.Now())},
	})
	if _, ok := internalRead(t, connection).(protocol.AckMessage); !ok {
		t.Fatal("recovered credential did not receive an ack after the delayed invalidation")
	}
	if _, removed := harness.state.RemoveCollector(identity.CollectorID); !removed {
		t.Fatal("the recovered credential's machine mapping was removed")
	}
	_ = ctx
}

// TestDelayedInvalidationDoesNotRemoveReusedCollectorMapping covers a collector
// id reused for a different machine while an old invalidation is delayed. The
// exact-credential message must leave the new machine's socket and mapping
// alone.
func TestDelayedInvalidationDoesNotRemoveReusedCollectorMapping(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-reused", MachineID: "m-new", Token: "token-reused"}
	harness := newGatewayHarness(t, identity, nil)

	connection := harness.dial(identity.Token)
	defer connection.CloseNow()
	internalSend(t, connection, internalHello(identity.CollectorID, "run-reused"))
	if _, ok := internalRead(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("reused collector mapping was not welcomed")
	}

	harness.state.InvalidateCredential(live.CredentialInvalidation{
		CollectorID: identity.CollectorID,
		MachineID:   "m-old",
		TokenHash:   "hash-of-a-previous-credential",
	})
	harness.waitFor(time.Second, "reused mapping to stay connected", func() bool {
		return harness.gateway.ConnectedCount() == 1
	})
	internalSend(t, connection, protocol.SamplesMessage{
		Mode:    protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{internalRecord(t, identity.CollectorID, "run-reused", 1, time.Now())},
	})
	if _, ok := internalRead(t, connection).(protocol.AckMessage); !ok {
		t.Fatal("reused collector id lost its live socket")
	}
	if _, removed := harness.state.RemoveCollector(identity.CollectorID); !removed {
		t.Fatal("reused collector id lost its machine mapping")
	}
}

// TestLiveEligibilityAndCommitUseOneReceiptTime places one record inside the
// replay horizon at ingest time but outside it at the storage clock. The single
// receipt captured by the gateway must govern both live eligibility and
// persistence; otherwise the record is published as fresh and then rejected
// forever.
func TestLiveEligibilityAndCommitUseOneReceiptTime(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-receipt", MachineID: "m-receipt", Token: "token-receipt"}
	base := time.Now().UTC().Truncate(time.Millisecond)
	telemetryNow := base.Add(2 * time.Hour)
	telemetryOptions := storage.TelemetryOptions{
		Now:          func() time.Time { return telemetryNow },
		MaxReplayAge: time.Hour,
		ClockSkew:    time.Hour,
	}
	harness := newGatewayHarnessWithTelemetry(t, identity, &telemetryOptions, func(opts *GatewayOptions) {
		opts.Now = func() time.Time { return base }
	})

	connection := harness.dial(identity.Token)
	defer connection.CloseNow()
	internalSend(t, connection, internalHello(identity.CollectorID, "run-receipt"))
	if _, ok := internalRead(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("session was not welcomed")
	}
	internalSend(t, connection, protocol.SamplesMessage{
		Mode: protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{
			internalRecord(t, identity.CollectorID, "run-receipt", 1, base.Add(-30*time.Minute)),
		},
	})
	ack, ok := internalRead(t, connection).(protocol.AckMessage)
	if !ok {
		t.Fatal("sample batch was not acknowledged")
	}
	if len(ack.Accepted) != 1 || len(ack.Rejected) != 0 {
		t.Fatalf("ack = %+v, want the record accepted by both live and storage", ack)
	}
}

// TestReplacementUnderNewCollectorReportsTheOldSessionDisconnectOnce replaces
// a machine that is connected through its old collector with a new collector
// id. The replacement forgets the old row, so the superseded socket ending is
// the only signal left that the machine's viewers lost their source: it must
// fire OnDisconnect exactly once.
func TestReplacementUnderNewCollectorReportsTheOldSessionDisconnectOnce(t *testing.T) {
	identity := storage.Identity{CollectorID: "c-old", MachineID: "m-replace", Token: "token-old"}
	var mu sync.Mutex
	var calls []string
	harness := newGatewayHarness(t, identity, func(options *GatewayOptions) {
		options.OnDisconnect = func(machineID string) {
			mu.Lock()
			calls = append(calls, machineID)
			mu.Unlock()
		}
	})

	old := harness.dial(identity.Token)
	defer old.CloseNow()
	internalSend(t, old, internalHello(identity.CollectorID, "run-old"))
	if _, ok := internalRead(t, old).(protocol.WelcomeMessage); !ok {
		t.Fatal("old session was not welcomed")
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_, _, _ = old.Read(context.Background())
	}()

	enrollReplacement(t, harness, identity.MachineID, "c-new")
	<-closed
	harness.waitFor(2*time.Second, "old session disconnect", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(calls) > 0
	})
	// Give a duplicate report a chance to show up before asserting.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != identity.MachineID {
		t.Fatalf("OnDisconnect calls = %v, want exactly [%s]", calls, identity.MachineID)
	}
}
