package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seededUsers(t *testing.T, db *Database) *Users {
	t.Helper()
	users := NewUsers(db)
	for _, user := range []User{
		{Username: "owner", PasswordHash: "hash-owner", Scope: AllMachines(), Admin: true},
		{Username: "bob", PasswordHash: "hash-bob", Scope: MachineScope("m-1")},
	} {
		if err := users.Create(context.Background(), "test", user); err != nil {
			t.Fatalf("create user %q: %v", user.Username, err)
		}
	}
	return users
}

func TestAccountVersionBumpsOnEveryChange(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	users := seededUsers(t, db)

	bob, err := users.Get(ctx, "bob")
	if err != nil || bob.Version != 1 {
		t.Fatalf("created version = %d, %v", bob.Version, err)
	}
	if err := users.Update(ctx, "owner", User{Username: "bob", Scope: MachineScope("m-2")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	updated, err := users.Get(ctx, "bob")
	if err != nil || updated.Version != 2 {
		t.Fatalf("update version = %d, %v", updated.Version, err)
	}
	if err := users.SetPasswordHash(ctx, "owner", "bob", "hash-rotated"); err != nil {
		t.Fatalf("password: %v", err)
	}
	rotated, err := users.Get(ctx, "bob")
	if err != nil || rotated.Version != 3 {
		t.Fatalf("password version = %d, %v", rotated.Version, err)
	}
}

func TestSessionLookupRejectsStaleAccount(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	users := seededUsers(t, db)
	sessions := NewSessions(db)
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	db.now = func() time.Time { return clock }

	session := Session{
		TokenHash: "hash-bob-session",
		Username:  "bob",
		CSRFToken: "csrf-bob",
		CreatedAt: clock,
		ExpiresAt: clock.Add(time.Hour),
	}
	if err := sessions.Create(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if found, err := sessions.Find(ctx, "hash-bob-session"); err != nil || found.Username != "bob" {
		t.Fatalf("find = %+v, %v", found, err)
	}
	// A scope change bumps the version; the cookie minted before it is dead.
	if err := users.Update(ctx, "owner", User{Username: "bob", Scope: MachineScope("m-2")}); err != nil {
		t.Fatalf("scope change: %v", err)
	}
	if _, err := sessions.Find(ctx, "hash-bob-session"); CodeOf(err) != CodeNotFound {
		t.Fatalf("stale version find error = %v", err)
	}
	// Even a matching version is rejected while the account is disabled.
	if err := users.Update(ctx, "owner", User{Username: "bob", Scope: MachineScope("m-2"), Disabled: true}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := sessions.Create(ctx, Session{
		TokenHash: "hash-bob-disabled",
		Username:  "bob",
		CSRFToken: "csrf-bob",
		CreatedAt: clock,
		ExpiresAt: clock.Add(time.Hour),
	}); !errors.Is(err, ErrAccountChanged) {
		t.Fatalf("create on disabled account error = %v", err)
	}
	// A deleted account can no longer own a session.
	if err := users.Update(ctx, "owner", User{Username: "bob", Scope: MachineScope("m-2"), Disabled: false}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := users.Delete(ctx, "owner", "bob"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := sessions.Find(ctx, "hash-bob-session"); CodeOf(err) != CodeNotFound {
		t.Fatalf("deleted account find error = %v", err)
	}
}

// dbWriteSession is a small helper so the disabled-account case can attempt an
// insert with a chosen version.
func dbWriteSession(ctx context.Context, sessions *Sessions, session Session, version int64, require bool) (Session, error) {
	if require {
		stored, _, err := sessions.CreateForUser(ctx, session, version)
		return stored, err
	}
	return Session{}, sessions.Create(ctx, session)
}

func TestCreateForUserFailsWhenAccountChangeCommitsFirst(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	users := seededUsers(t, db)
	sessions := NewSessions(db)

	bob, err := users.Get(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	mutation := make(chan error, 1)
	go func() {
		_, err := users.MutateAccount(ctx, "owner", "bob", func(tx *Tx) error {
			close(started)
			<-release
			return users.UpdateTx(tx, "owner", User{Username: "bob", Scope: AllMachines()})
		})
		mutation <- err
	}()
	<-started
	// The account transaction holds the single writer. A login that verified
	// the old version must wait and then lose.
	type result struct {
		err error
	}
	login := make(chan result, 1)
	go func() {
		_, _, err := sessions.CreateForUser(ctx, Session{
			TokenHash: "racing-login",
			Username:  bob.Username,
			CSRFToken: "csrf",
			CreatedAt: time.Now(),
			ExpiresAt: time.Now().Add(time.Hour),
		}, bob.Version)
		login <- result{err: err}
	}()
	close(release)
	if err := <-mutation; err != nil {
		t.Fatalf("account mutation: %v", err)
	}
	if got := <-login; !errors.Is(got.err, ErrAccountChanged) {
		t.Fatalf("racing login error = %v, want ErrAccountChanged", got.err)
	}
	count, err := sessions.Count(ctx)
	if err != nil || count != 0 {
		t.Fatalf("sessions = %d, %v", count, err)
	}
}

func TestAccountMutationDeletesSessionsInTransaction(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	users := seededUsers(t, db)
	sessions := NewSessions(db)

	if err := sessions.Create(ctx, Session{
		TokenHash: "bob-session", Username: "bob", CSRFToken: "csrf",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	ended, err := users.MutateAccount(ctx, "owner", "bob", func(tx *Tx) error {
		return users.UpdateTx(tx, "owner", User{Username: "bob", Scope: MachineScope("m-2"), Disabled: true})
	})
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if len(ended) != 1 || ended[0].TokenHash != "bob-session" {
		t.Fatalf("ended rows = %+v", ended)
	}
	count, err := sessions.Count(ctx)
	if err != nil || count != 0 {
		t.Fatalf("sessions after account change = %d, %v", count, err)
	}
}

func TestUnauthorizedMutationChangesNothing(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	users := seededUsers(t, db)
	sessions := NewSessions(db)

	if err := sessions.Create(ctx, Session{
		TokenHash: "bob-session", Username: "bob", CSRFToken: "csrf",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	// bob is not an administrator, so the durable re-check rejects the change.
	ended, err := users.MutateAccount(ctx, "bob", "bob", func(tx *Tx) error {
		return users.UpdateTx(tx, "bob", User{Username: "bob", Scope: AllMachines(), Admin: true})
	})
	if CodeOf(err) != CodeForbidden {
		t.Fatalf("authorize error = %v", err)
	}
	if len(ended) != 0 {
		t.Fatalf("ended rows = %+v", ended)
	}
	bob, err := users.Get(ctx, "bob")
	if err != nil || bob.Admin || bob.Version != 1 {
		t.Fatalf("unauthorized mutation changed bob: %+v, %v", bob, err)
	}
	count, err := sessions.Count(ctx)
	if err != nil || count != 1 {
		t.Fatalf("unauthorized mutation ended sessions: %d, %v", count, err)
	}
}

func TestResetPasswordFailureKeepsAccountAndSessions(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	users := seededUsers(t, db)
	sessions := NewSessions(db)

	if err := sessions.Create(ctx, Session{
		TokenHash: "bob-session", Username: "bob", CSRFToken: "csrf",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	before, err := users.Get(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := users.ResetPassword(cancelled, "local-recovery", "bob", "hash-new"); err == nil {
		t.Fatal("cancelled reset succeeded")
	}
	after, err := users.Get(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if after.PasswordHash != before.PasswordHash || after.Version != before.Version {
		t.Fatalf("failed reset changed the account: %+v -> %+v", before, after)
	}
	count, err := sessions.Count(ctx)
	if err != nil || count != 1 {
		t.Fatalf("failed reset ended sessions: %d, %v", count, err)
	}
	// The successful path changes the hash and ends the session together.
	ended, err := users.ResetPassword(ctx, "local-recovery", "bob", "hash-new")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(ended) != 1 {
		t.Fatalf("ended rows = %+v", ended)
	}
	count, err = sessions.Count(ctx)
	if err != nil || count != 0 {
		t.Fatalf("sessions after reset = %d, %v", count, err)
	}
}
