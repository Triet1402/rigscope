package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/hub/storage"
)

func TestLiveAuthenticatorMapsSessionAndEndsStreamsOnLogout(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users:       []storage.User{{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.MachineScope("m-1")}},
		withServers: true,
	})
	session, token, ok, err := harness.manager.Login(context.Background(), "203.0.113.1", "owner", "owner-password")
	if err != nil || !ok {
		t.Fatalf("login: ok=%v err=%v", ok, err)
	}
	adapter := NewLiveAuthenticator(harness.manager)
	defer adapter.Close()

	request := httptest.NewRequest(http.MethodGet, "/api/live", nil)
	request.Host = strings.TrimPrefix(harness.origin, "http://")
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	request.Header.Set("Origin", harness.origin)
	liveSession, err := adapter.Authenticate(request)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if liveSession.ID != session.TokenHash || liveSession.Username != "owner" || liveSession.CSRFToken != session.CSRFToken {
		t.Fatalf("live session = %+v", liveSession)
	}
	if liveSession.Scope.All || len(liveSession.Scope.IDs) != 1 || liveSession.Scope.IDs[0] != "m-1" {
		t.Fatalf("live scope = %+v", liveSession.Scope)
	}
	if !adapter.CheckOrigin(request) {
		t.Fatal("the fixture origin must be trusted")
	}
	select {
	case <-liveSession.Revoked:
		t.Fatal("a live session must not be pre-revoked")
	default:
	}

	mutation := httptest.NewRequest(http.MethodPost, "/api/processes?machineId=m-1&viewerId=v", nil)
	mutation.Host = request.Host
	mutation.Header.Set("Origin", harness.origin)
	mutation.Header.Set("X-CSRF-Token", session.CSRFToken)
	if err := adapter.CheckMutation(mutation, liveSession); err != nil {
		t.Fatalf("valid mutation rejected: %v", err)
	}
	mutation.Header.Set("X-CSRF-Token", "wrong")
	if err := adapter.CheckMutation(mutation, liveSession); err == nil {
		t.Fatal("a wrong CSRF token must be rejected")
	}

	if err := harness.manager.Logout(context.Background(), session); err != nil {
		t.Fatalf("logout: %v", err)
	}
	select {
	case <-liveSession.Revoked:
	case <-time.After(2 * time.Second):
		t.Fatal("logout did not close the session revocation channel")
	}
	// The deleted row and the tombstone both refuse the stolen cookie, even
	// before the request that observed the session has finished.
	if _, err := adapter.Authenticate(request); err == nil {
		t.Fatal("a logged-out cookie authenticated after logout")
	}
}

// TestRevocationTombstonesAreBoundedAndRefuseEvictedSessions fills the hard
// tombstone cap and proves an evicted entry still refuses a session created
// before its revocation time, so the bound never reopens a revoked-cookie race.
func TestRevocationTombstonesAreBoundedAndRefuseEvictedSessions(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users: []storage.User{{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines()}},
	})
	adapter := NewLiveAuthenticator(harness.manager)
	defer adapter.Close()

	base := time.Now().UTC()
	current := base
	adapter.now = func() time.Time { return current }
	adapter.maxTombstones = 2

	adapter.revoke("session-one")
	current = base.Add(time.Second)
	adapter.revoke("session-two")
	current = base.Add(2 * time.Second)
	adapter.revoke("session-three")

	adapter.mu.Lock()
	count := len(adapter.tomb)
	evictedThrough := adapter.evictedThrough
	adapter.mu.Unlock()
	if count != 2 {
		t.Fatalf("tombstones = %d, want the hard cap of 2", count)
	}
	if !evictedThrough.Equal(base) {
		t.Fatalf("evictedThrough = %s, want the oldest revocation %s", evictedThrough, base)
	}

	// The evicted tombstone still refuses a session created before it, so the
	// eviction cannot reopen the revoked-cookie race.
	select {
	case <-adapter.revocation(context.Background(), "session-one", base.Add(-time.Second), adapter.revocationCount()):
	default:
		t.Fatal("an evicted tombstone's session subscribed to an open channel")
	}
	// A retained tombstone closes its own session.
	select {
	case <-adapter.revocation(context.Background(), "session-two", base.Add(500*time.Millisecond), adapter.revocationCount()):
	default:
		t.Fatal("a retained tombstone did not close its session")
	}
	// A session created after every eviction is still allowed to subscribe.
	live := adapter.revocation(context.Background(), "fresh-session", base.Add(3*time.Second), adapter.revocationCount())
	select {
	case <-live:
		t.Fatal("a session created after the eviction horizon was pre-revoked")
	default:
	}

	current = base.Add(3*time.Second + harness.manager.TTL() + time.Second)
	adapter.mu.Lock()
	adapter.pruneLocked(current)
	remaining := len(adapter.tomb)
	adapter.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expired tombstones = %d, want periodic pruning to empty them", remaining)
	}
}

// TestRevocationTombstoneCapKeepsValidSessionsLive reproduces a valid session
// whose created time predates every eviction horizon because unrelated
// logins/logouts filled the tombstone cap. Eviction must not refuse a session
// that is still present and unexpired in the durable table.
func TestRevocationTombstoneCapKeepsValidSessionsLive(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users: []storage.User{{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines()}},
	})
	session, token, ok, err := harness.manager.Login(context.Background(), "203.0.113.1", "owner", "owner-password")
	if err != nil || !ok {
		t.Fatalf("login: ok=%v err=%v", ok, err)
	}
	adapter := NewLiveAuthenticator(harness.manager)
	defer adapter.Close()
	adapter.now = harness.clock
	adapter.maxTombstones = 4

	// Six unrelated ends evict two unexpired tombstones and advance the
	// eviction horizon past the valid session's creation time.
	for index := 0; index < 6; index++ {
		adapter.revoke(fmt.Sprintf("unrelated-session-%d", index))
	}
	adapter.mu.Lock()
	evictedThrough := adapter.evictedThrough
	adapter.mu.Unlock()
	if evictedThrough.IsZero() || session.CreatedAt.After(evictedThrough) {
		t.Fatalf("fixture did not push the valid session behind the eviction horizon: created=%s horizon=%s",
			session.CreatedAt, evictedThrough)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/live", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	liveSession, err := adapter.Authenticate(request)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	select {
	case <-liveSession.Revoked:
		t.Fatal("an evicted tombstone pre-revoked a valid session")
	default:
	}
}

// TestRevocationLeavesTombstoneWhenWatcherExists reproduces an authentication
// that already passed its durable session lookup while an earlier stream still
// held a watcher. Logout closes and removes that watcher; the late
// authentication must still observe a closed revocation channel instead of
// creating a fresh, permanently open one.
func TestRevocationLeavesTombstoneWhenWatcherExists(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users:       []storage.User{{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines()}},
		withServers: true,
	})
	session, token, ok, err := harness.manager.Login(context.Background(), "203.0.113.1", "owner", "owner-password")
	if err != nil || !ok {
		t.Fatalf("login: ok=%v err=%v", ok, err)
	}
	adapter := NewLiveAuthenticator(harness.manager)
	defer adapter.Close()

	// An earlier stream already subscribed for this token.
	earlier := adapter.revocation(context.Background(), session.TokenHash, session.CreatedAt, adapter.revocationCount())

	request := httptest.NewRequest(http.MethodGet, "/api/live", nil)
	request.Host = strings.TrimPrefix(harness.origin, "http://")
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	request.Header.Set("Origin", harness.origin)

	paused := make(chan struct{})
	release := make(chan struct{})
	adapter.testAfterSessionLookup = func() {
		close(paused)
		<-release
	}
	type result struct {
		revoked <-chan struct{}
		err     error
	}
	results := make(chan result, 1)
	go func() {
		liveSession, err := adapter.Authenticate(request)
		results <- result{revoked: liveSession.Revoked, err: err}
	}()

	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatal("authentication never passed its durable session lookup")
	}
	if err := harness.manager.Logout(context.Background(), session); err != nil {
		t.Fatalf("logout: %v", err)
	}
	select {
	case <-earlier:
	case <-time.After(2 * time.Second):
		t.Fatal("the earlier watcher was not closed by logout")
	}
	close(release)

	got := <-results
	if got.err != nil {
		t.Fatalf("late authentication failed outright: %v", got.err)
	}
	select {
	case <-got.revoked:
	case <-time.After(2 * time.Second):
		t.Fatal("late authentication created a new open revocation channel after logout")
	}
}

// TestRevocationBetweenLookupAndSubscribeSurvivesTombstoneExpiry pauses an
// authentication after its durable lookup, logs the session out, and lets the
// tombstone disappear before the authentication subscribes. The revocation
// counter sampled before the lookup must still force a durable re-read, so the
// late subscription is refused without depending on tombstone lifetime.
func TestRevocationBetweenLookupAndSubscribeSurvivesTombstoneExpiry(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users:       []storage.User{{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines()}},
		withServers: true,
	})
	session, token, ok, err := harness.manager.Login(context.Background(), "203.0.113.1", "owner", "owner-password")
	if err != nil || !ok {
		t.Fatalf("login: ok=%v err=%v", ok, err)
	}
	adapter := NewLiveAuthenticator(harness.manager)
	defer adapter.Close()

	request := httptest.NewRequest(http.MethodGet, "/api/live", nil)
	request.Host = strings.TrimPrefix(harness.origin, "http://")
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	request.Header.Set("Origin", harness.origin)

	paused := make(chan struct{})
	release := make(chan struct{})
	adapter.testAfterSessionLookup = func() {
		close(paused)
		<-release
	}
	results := make(chan (<-chan struct{}), 1)
	go func() {
		liveSession, err := adapter.Authenticate(request)
		if err != nil {
			t.Errorf("Authenticate: %v", err)
		}
		results <- liveSession.Revoked
	}()
	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatal("authentication never passed its durable session lookup")
	}
	if err := harness.manager.Logout(context.Background(), session); err != nil {
		t.Fatalf("logout: %v", err)
	}
	// The tombstone expires (or is pruned) before the stalled authentication
	// subscribes.
	adapter.mu.Lock()
	delete(adapter.tomb, session.TokenHash)
	adapter.mu.Unlock()
	close(release)

	select {
	case <-<-results:
	case <-time.After(2 * time.Second):
		t.Fatal("a session revoked during authentication got an open revocation channel after its tombstone expired")
	}
}

// TestDurableRecheckIgnoresOneRequestsCancellation verifies the subscription
// re-read does not inherit a request's cancellation. The watcher is shared by
// every stream of the session, so one client hanging up must not close it.
func TestDurableRecheckIgnoresOneRequestsCancellation(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users:       []storage.User{{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines()}},
		withServers: true,
	})
	session, _, ok, err := harness.manager.Login(context.Background(), "203.0.113.1", "owner", "owner-password")
	if err != nil || !ok {
		t.Fatalf("login: ok=%v err=%v", ok, err)
	}
	adapter := NewLiveAuthenticator(harness.manager)
	defer adapter.Close()
	adapter.now = harness.clock
	// Force the durable re-read path: the session predates an evicted tombstone.
	adapter.mu.Lock()
	adapter.evictedThrough = session.CreatedAt.Add(time.Hour)
	adapter.mu.Unlock()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	first := adapter.revocation(cancelled, session.TokenHash, session.CreatedAt, adapter.revocationCount())
	sibling := adapter.revocation(context.Background(), session.TokenHash, session.CreatedAt, adapter.revocationCount())
	for name, ch := range map[string]<-chan struct{}{"first": first, "sibling": sibling} {
		select {
		case <-ch:
			t.Fatalf("%s stream of a valid session was closed by a cancelled request's re-read", name)
		default:
		}
	}
}
