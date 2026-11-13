package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/hub/storage"
)

// TestLoginFailsWhenAccountChangesDuringVerification pauses password
// verification, changes the account through the storage API, then releases the
// login. The atomic version check must reject the attempt instead of minting a
// session from the old password hash, scope, admin flag or enabled state.
func TestLoginFailsWhenAccountChangesDuringVerification(t *testing.T) {
	cases := []struct {
		name   string
		change func(context.Context, *storage.Users) error
	}{
		{"password", func(ctx context.Context, users *storage.Users) error {
			return users.SetPasswordHash(ctx, "test", "demo", "test:rotated")
		}},
		{"scope", func(ctx context.Context, users *storage.Users) error {
			return users.Update(ctx, "test", storage.User{Username: "demo", Scope: storage.MachineScope("m-2")})
		}},
		{"admin", func(ctx context.Context, users *storage.Users) error {
			return users.Update(ctx, "test", storage.User{Username: "demo", Scope: storage.AllMachines(), Admin: true})
		}},
		{"disabled", func(ctx context.Context, users *storage.Users) error {
			return users.Update(ctx, "test", storage.User{Username: "demo", Scope: storage.AllMachines(), Disabled: true})
		}},
		{"deleted", func(ctx context.Context, users *storage.Users) error {
			return users.Delete(ctx, "test", "demo")
		}},
		{"deleted and recreated", func(ctx context.Context, users *storage.Users) error {
			// A recreated username starts a new account. Its per-account
			// version restarts, so only the never-reused incarnation can reject
			// a login that verified the previous account's password.
			if err := users.Delete(ctx, "test", "demo"); err != nil {
				return err
			}
			return users.Create(ctx, "test", storage.User{
				Username: "demo", PasswordHash: "test:different-password",
				Scope: storage.AllMachines(), Admin: true,
			})
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			harness := newHarness(t, harnessOptions{users: []storage.User{
				{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
				{Username: "demo", PasswordHash: "test:demo-password", Scope: storage.AllMachines()},
			}})
			started := make(chan struct{})
			release := make(chan struct{})
			manager, err := New(Options{
				Users:    harness.users,
				Sessions: storage.NewSessions(harness.db),
				Origin:   &alwaysTrusted{},
				Now:      harness.clock,
				VerifyPassword: func(password, encoded string) (bool, error) {
					close(started)
					<-release
					return encoded == "test:"+password, nil
				},
				SweepInterval: time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()

			result := make(chan bool, 1)
			go func() {
				_, _, ok, _ := manager.Login(context.Background(), "203.0.113.9", "demo", "demo-password")
				result <- ok
			}()
			<-started
			if err := test.change(context.Background(), harness.users); err != nil {
				t.Fatalf("account change: %v", err)
			}
			close(release)
			if <-result {
				t.Fatal("login minted a session from a stale account")
			}
			count, err := storage.NewSessions(harness.db).Count(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("stale login stored %d sessions", count)
			}
		})
	}
}

// TestLoginRejectsSchemaFiveAccountWithEmptyIncarnation simulates the account a
// schema-5 binary legally leaves behind after a rollback: migration 6 defaults
// incarnation to empty. The account is deleted and recreated while verification is
// paused; the empty expected incarnation must not act as a wildcard and accept
// the previous account's password.
func TestLoginRejectsSchemaFiveAccountWithEmptyIncarnation(t *testing.T) {
	harness := newHarness(t, harnessOptions{})
	ctx := context.Background()
	now := harness.clock()
	if err := harness.db.Write(ctx, func(tx *storage.Tx) error {
		_, err := tx.Exec(
			"INSERT INTO users (username, password_hash, scope_json, is_admin, disabled, created_at_ms, updated_at_ms) "+
				"VALUES ('rolled-back', 'test:demo-password', '*', 0, 0, ?, ?)",
			now.UnixMilli(), now.UnixMilli(),
		)
		return err
	}); err != nil {
		t.Fatalf("insert schema-5 account: %v", err)
	}
	user, err := harness.users.Get(ctx, "rolled-back")
	if err != nil || user.Incarnation != "" {
		t.Fatalf("schema-5 account incarnation = %q, %v", user.Incarnation, err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	manager, err := New(Options{
		Users:    harness.users,
		Sessions: storage.NewSessions(harness.db),
		Origin:   &alwaysTrusted{},
		Now:      harness.clock,
		VerifyPassword: func(password, encoded string) (bool, error) {
			close(started)
			<-release
			return encoded == "test:"+password, nil
		},
		SweepInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	result := make(chan bool, 1)
	go func() {
		_, _, ok, _ := manager.Login(ctx, "203.0.113.9", "rolled-back", "demo-password")
		result <- ok
	}()
	<-started
	if err := harness.users.Delete(ctx, "test", "rolled-back"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := harness.users.Create(ctx, "test", storage.User{
		Username: "rolled-back", PasswordHash: "test:replacement-password",
		Scope: storage.AllMachines(), Admin: true,
	}); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	close(release)
	if <-result {
		t.Fatal("login minted a session from an empty incarnation after recreation")
	}
	count, err := storage.NewSessions(harness.db).Count(ctx)
	if err != nil || count != 0 {
		t.Fatalf("stale login stored %d sessions, %v", count, err)
	}
}

// TestSessionIsBoundToAccountVersion proves the defence-in-depth half: a
// session row whose account version moved on is treated as absent even if the
// row survived (the admin path also deletes the row in the same transaction).
func TestSessionIsBoundToAccountVersion(t *testing.T) {
	harness := newHarness(t, harnessOptions{users: []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.MachineScope("m-1"), Admin: true},
	}})
	session, token, ok, err := harness.manager.Login(context.Background(), "203.0.113.1", "owner", "owner-password")
	if err != nil || !ok {
		t.Fatalf("login: ok=%v err=%v", ok, err)
	}
	if !session.Admin || !session.Scope.Contains("m-1") || session.Scope.All {
		t.Fatalf("session snapshot = %+v", session)
	}
	request := httptest.NewRequest("GET", sessionPath, nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	if found, err := harness.manager.Session(request); err != nil || found == nil {
		t.Fatalf("live session lookup = %+v, %v", found, err)
	}
	if err := harness.users.Update(context.Background(), "test", storage.User{
		Username: "owner", Scope: storage.MachineScope("m-2"), Admin: true,
	}); err != nil {
		t.Fatalf("scope change: %v", err)
	}
	if found, err := harness.manager.Session(request); err != nil || found != nil {
		t.Fatalf("stale session was accepted: %+v, %v", found, err)
	}
}

// TestEvictionBroadcastsEveryRemovedSession fills the durable table, then runs
// two creations at once. The table bound and the in-memory index move under one
// lock, and every evicted row is broadcast instead of silently dropped.
func TestEvictionBroadcastsEveryRemovedSession(t *testing.T) {
	arrived := make(chan struct{}, 2)
	gate := make(chan struct{})
	harness := newHarness(t, harnessOptions{
		users: []storage.User{
			{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		},
		verify: func(password, encoded string) (bool, error) {
			arrived <- struct{}{}
			<-gate
			return encoded == "test:"+password, nil
		},
	})
	sessions := storage.NewSessions(harness.db)
	base := harness.clock().Add(-time.Second)
	for index := 0; index < storage.MaxSessions-1; index++ {
		if err := sessions.Create(context.Background(), storage.Session{
			TokenHash: fmt.Sprintf("filler-%03d", index),
			Username:  "owner",
			CSRFToken: "csrf",
			CreatedAt: base.Add(time.Duration(index) * time.Millisecond),
			ExpiresAt: base.Add(24 * time.Hour),
		}); err != nil {
			t.Fatalf("filler %d: %v", index, err)
		}
	}
	var mu sync.Mutex
	var events []Revocation
	unsubscribe := harness.manager.Subscribe(func(event Revocation) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})
	defer unsubscribe()

	results := make(chan bool, 2)
	for index := 0; index < 2; index++ {
		go func() {
			_, _, ok, _ := harness.manager.Login(context.Background(), "203.0.113.1", "owner", "owner-password")
			results <- ok
		}()
	}
	<-arrived
	<-arrived
	close(gate)
	for index := 0; index < 2; index++ {
		if !<-results {
			t.Fatal("concurrent login failed")
		}
	}
	count, err := sessions.Count(context.Background())
	if err != nil || count != storage.MaxSessions {
		t.Fatalf("session count = %d, %v", count, err)
	}
	harness.manager.mu.Lock()
	active := len(harness.manager.active)
	harness.manager.mu.Unlock()
	if active != 2 {
		t.Fatalf("in-memory index = %d sessions, want 2", active)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].TokenHash != "filler-000" || events[0].Reason != ReasonExpired {
		t.Fatalf("eviction events = %+v", events)
	}
}

// TestExpiryNotifiesBeforeDeletingRows proves the sweeper ends streams before
// it touches SQLite: the subscriber observes the durable row still present, and
// the row is gone once the sweep returns.
func TestExpiryNotifiesBeforeDeletingRows(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users: []storage.User{
			{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		},
		ttl: time.Minute,
	})
	if _, _, ok, err := harness.manager.Login(context.Background(), "203.0.113.1", "owner", "owner-password"); err != nil || !ok {
		t.Fatalf("login: ok=%v err=%v", ok, err)
	}
	var (
		mu      sync.Mutex
		seen    []int
		reasons []Reason
	)
	unsubscribe := harness.manager.Subscribe(func(event Revocation) {
		count, err := harness.manager.sessions.Count(context.Background())
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			count = -1
		}
		seen = append(seen, count)
		reasons = append(reasons, event.Reason)
	})
	defer unsubscribe()
	harness.advance(2 * time.Minute)
	harness.manager.sweepExpired()
	mu.Lock()
	if len(seen) != 1 || seen[0] != 1 || reasons[0] != ReasonExpired {
		mu.Unlock()
		t.Fatalf("notifications = %v %v, want one expiry observed with the row still present", seen, reasons)
	}
	mu.Unlock()
	count, err := harness.manager.sessions.Count(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("sessions after sweep = %d, %v", count, err)
	}
}

// TestRestoredSessionKeepsSnapshot verifies that a hub restart adopts the exact
// durable session: CSRF token, scope, admin snapshot, creation time, fixed
// expiry and account version.
func TestRestoredSessionKeepsSnapshot(t *testing.T) {
	harness := newHarness(t, harnessOptions{users: []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.MachineScope("m-1"), Admin: true},
	}})
	session, token, ok, err := harness.manager.Login(context.Background(), "203.0.113.1", "owner", "owner-password")
	if err != nil || !ok {
		t.Fatalf("login: ok=%v err=%v", ok, err)
	}
	request := httptest.NewRequest("GET", sessionPath, nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	restarted, err := New(Options{
		Users:          harness.users,
		Sessions:       storage.NewSessions(harness.db),
		Origin:         &alwaysTrusted{},
		Now:            harness.clock,
		VerifyPassword: fastVerify,
		SweepInterval:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	adopted, err := restarted.Session(request)
	if err != nil || adopted == nil {
		t.Fatalf("session did not survive a restart: %+v, %v", adopted, err)
	}
	if adopted.TokenHash != session.TokenHash || adopted.Username != session.Username ||
		adopted.CSRFToken != session.CSRFToken || adopted.Admin != session.Admin ||
		!adopted.Scope.Contains("m-1") || adopted.Scope.All ||
		!adopted.CreatedAt.Equal(session.CreatedAt) || !adopted.ExpiresAt.Equal(session.ExpiresAt) {
		t.Fatalf("adopted session = %+v, want %+v", adopted, session)
	}
}

// TestRestoredSessionEndsOnRevocationExpiryAndEviction covers the remaining
// lifecycle paths for a restored row: an account change deletes it, expiry
// drops it at restart, and cap eviction reaches it exactly like a fresh session.
func TestRestoredSessionEndsOnRevocationExpiryAndEviction(t *testing.T) {
	harness := newHarness(t, harnessOptions{users: []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
	}, ttl: time.Hour})
	login := func(manager *Manager) (*Session, string) {
		t.Helper()
		session, token, ok, err := manager.Login(context.Background(), "203.0.113.1", "owner", "owner-password")
		if err != nil || !ok {
			t.Fatalf("login: ok=%v err=%v", ok, err)
		}
		return session, token
	}
	restart := func() *Manager {
		t.Helper()
		manager, err := New(Options{
			Users:          harness.users,
			Sessions:       storage.NewSessions(harness.db),
			Origin:         &alwaysTrusted{},
			Now:            harness.clock,
			VerifyPassword: fastVerify,
			SweepInterval:  time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = manager.Close() })
		return manager
	}

	// Revocation: the account transaction returns the ended rows, the manager
	// broadcasts them, and the next process does not adopt the row.
	_, token := login(harness.manager)
	ended, err := harness.users.MutateAccount(context.Background(), "owner", "owner", func(tx *storage.Tx) error {
		return harness.users.UpdateTx(tx, "owner", storage.User{Username: "owner", Scope: storage.MachineScope("m-1"), Admin: true})
	})
	if err != nil {
		t.Fatalf("account change: %v", err)
	}
	harness.manager.EndSessions(ended)
	request := httptest.NewRequest("GET", sessionPath, nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	if found, err := restart().Session(request); err != nil || found != nil {
		t.Fatalf("revoked session was resurrected: %+v, %v", found, err)
	}

	// Expiry: a restart past the fixed lifetime deletes the row instead of
	// adopting it.
	_, expiringToken := login(harness.manager)
	harness.advance(2 * time.Hour)
	request = httptest.NewRequest("GET", sessionPath, nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: expiringToken})
	expired := restart()
	if found, err := expired.Session(request); err != nil || found != nil {
		t.Fatalf("expired session was adopted: %+v, %v", found, err)
	}
	count, err := storage.NewSessions(harness.db).Count(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("expired session row survived restart: %d, %v", count, err)
	}

	// Eviction: fill the table, then a fresh sign-in evicts the restored row
	// (it is the oldest) and broadcasts it.
	harness.advance(-2 * time.Hour)
	oldest, oldestToken := login(harness.manager)
	restored := restart()
	base := harness.clock().Add(time.Second)
	sessions := storage.NewSessions(harness.db)
	for index := 0; index < storage.MaxSessions-1; index++ {
		if err := sessions.Create(context.Background(), storage.Session{
			TokenHash: fmt.Sprintf("filler-%03d", index),
			Username:  "owner",
			CSRFToken: "csrf",
			CreatedAt: base.Add(time.Duration(index) * time.Millisecond),
			ExpiresAt: base.Add(24 * time.Hour),
		}); err != nil {
			t.Fatalf("filler %d: %v", index, err)
		}
	}
	var mu sync.Mutex
	var evicted []string
	unsubscribe := restored.Subscribe(func(event Revocation) {
		mu.Lock()
		evicted = append(evicted, event.TokenHash)
		mu.Unlock()
	})
	defer unsubscribe()
	login(restored)
	mu.Lock()
	if len(evicted) != 1 || evicted[0] != oldest.TokenHash {
		mu.Unlock()
		t.Fatalf("evicted = %v, want %s", evicted, oldest.TokenHash)
	}
	mu.Unlock()
	request = httptest.NewRequest("GET", sessionPath, nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: oldestToken})
	if found, err := restart().Session(request); err != nil || found != nil {
		t.Fatalf("evicted session was resurrected: %+v, %v", found, err)
	}
}
