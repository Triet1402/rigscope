package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestUsersCreateListAndScope(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	users := NewUsers(db)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	if err := users.Create(ctx, "bootstrap", User{
		Username:     "owner",
		PasswordHash: "scrypt$opaque-owner",
		Scope:        AllMachines(),
		Admin:        true,
		CreatedAt:    now,
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := users.Create(ctx, "owner", User{
		Username:     "viewer",
		PasswordHash: "scrypt$opaque-viewer",
		Scope:        MachineScope("m-1", "m-2"),
	}); err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	if err := users.Create(ctx, "owner", User{
		Username:     "owner",
		PasswordHash: "scrypt$other",
		Scope:        AllMachines(),
	}); CodeOf(err) != CodeConflict {
		t.Fatalf("duplicate create error = %v", err)
	}
	owner, err := users.Get(ctx, "owner")
	if err != nil {
		t.Fatalf("get owner: %v", err)
	}
	if !owner.Admin || !owner.Scope.All || owner.PasswordHash != "scrypt$opaque-owner" {
		t.Fatalf("owner = %+v", owner)
	}
	viewer, err := users.Get(ctx, "viewer")
	if err != nil {
		t.Fatalf("get viewer: %v", err)
	}
	if viewer.Scope.All || len(viewer.Scope.MachineIDs) != 2 || !viewer.Scope.Contains("m-1") {
		t.Fatalf("viewer scope = %+v", viewer.Scope)
	}
	admin, err := users.IsAdmin(ctx, "owner")
	if err != nil || !admin {
		t.Fatalf("IsAdmin(owner) = %v, %v", admin, err)
	}
	admin, err = users.IsAdmin(ctx, "viewer")
	if err != nil || admin {
		t.Fatalf("IsAdmin(viewer) = %v, %v", admin, err)
	}
	listed, err := users.List(ctx)
	if err != nil || len(listed) != 2 {
		t.Fatalf("list = %v, %v", listed, err)
	}
}

func TestUsersPreserveHashAndAuditWithoutSecrets(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	users := NewUsers(db)
	audit := NewAudit(db)
	if err := users.Create(ctx, "bootstrap", User{Username: "owner", PasswordHash: "hash-owner", Scope: AllMachines(), Admin: true}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := users.Create(ctx, "owner", User{Username: "bob", PasswordHash: "hash-bob", Scope: AllMachines()}); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	// An empty hash preserves the stored one.
	if err := users.Update(ctx, "owner", User{Username: "bob", Scope: MachineScope("m-3"), Admin: true}); err != nil {
		t.Fatalf("update bob: %v", err)
	}
	bob, err := users.Get(ctx, "bob")
	if err != nil {
		t.Fatalf("get bob: %v", err)
	}
	if bob.PasswordHash != "hash-bob" || !bob.Admin || len(bob.Scope.MachineIDs) != 1 {
		t.Fatalf("bob = %+v", bob)
	}
	if err := users.SetPasswordHash(ctx, "owner", "bob", "hash-rotated"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	bob, _ = users.Get(ctx, "bob")
	if bob.PasswordHash != "hash-rotated" {
		t.Fatalf("hash = %q", bob.PasswordHash)
	}
	events, err := audit.List(ctx, AuditScopeAdmin, 0)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("audit events = %d, want 4: %+v", len(events), events)
	}
	if events[0].Action != "user.update-password" || events[0].Actor != "owner" || events[0].Target != "bob" {
		t.Fatalf("newest event = %+v", events[0])
	}
	serialized := fmt.Sprint(events)
	for _, secret := range []string{"hash-bob", "hash-rotated", "hash-owner"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("audit leaked %q: %s", secret, serialized)
		}
	}
}

func TestUsersProtectLastActiveAdministrator(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	users := NewUsers(db)
	if err := users.Create(ctx, "bootstrap", User{Username: "owner", PasswordHash: "hash", Scope: AllMachines(), Admin: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := users.Update(ctx, "owner", User{Username: "owner", Scope: AllMachines(), Admin: false}); CodeOf(err) != CodeConflict {
		t.Fatalf("demote error = %v", err)
	}
	if err := users.Update(ctx, "owner", User{Username: "owner", Scope: AllMachines(), Admin: true, Disabled: true}); CodeOf(err) != CodeConflict {
		t.Fatalf("disable error = %v", err)
	}
	if err := users.Delete(ctx, "owner", "owner"); CodeOf(err) != CodeConflict {
		t.Fatalf("delete error = %v", err)
	}
	// A second administrator makes both operations legal.
	if err := users.Create(ctx, "owner", User{Username: "admin2", PasswordHash: "hash", Scope: AllMachines(), Admin: true}); err != nil {
		t.Fatalf("create second admin: %v", err)
	}
	if err := users.Update(ctx, "owner", User{Username: "owner", Scope: AllMachines(), Admin: false}); err != nil {
		t.Fatalf("demote with another admin: %v", err)
	}
	if err := users.Delete(ctx, "admin2", "admin2"); CodeOf(err) != CodeConflict {
		t.Fatalf("delete last admin error = %v", err)
	}
}

func TestUsersBoundAndValidation(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	users := NewUsers(db)
	for index := 0; index < MaxUsers; index++ {
		username := fmt.Sprintf("user-%d", index)
		if err := users.Create(ctx, "bootstrap", User{
			Username:     username,
			PasswordHash: "hash",
			Scope:        AllMachines(),
			Admin:        index == 0,
		}); err != nil {
			t.Fatalf("create %s: %v", username, err)
		}
	}
	if err := users.Create(ctx, "user-0", User{Username: "one-too-many", PasswordHash: "hash", Scope: AllMachines()}); CodeOf(err) != CodeConflict {
		t.Fatalf("cap error = %v", err)
	}
	cases := []User{
		{Username: "bad name", PasswordHash: "hash", Scope: AllMachines()},
		{Username: "ok", PasswordHash: "", Scope: AllMachines()},
		{Username: "ok", PasswordHash: "hash", Scope: Scope{MachineIDs: []string{"dup", "dup"}}},
		{Username: "ok", PasswordHash: "hash", Scope: Scope{All: true, MachineIDs: []string{"m-1"}}},
	}
	for _, input := range cases {
		if err := users.Create(ctx, "user-0", input); CodeOf(err) != CodeInvalid {
			t.Fatalf("invalid user %+v error = %v", input, err)
		}
	}
	many := make([]string, 60)
	for index := range many {
		many[index] = fmt.Sprintf("m-%d", index)
	}
	if err := users.Delete(ctx, "user-0", "user-1"); err != nil {
		t.Fatalf("free a slot: %v", err)
	}
	if err := users.Create(ctx, "user-0", User{Username: "scoped", PasswordHash: "hash", Scope: MachineScope(many...)}); err != nil {
		t.Fatalf("wide scope: %v", err)
	}
	scoped, err := users.Get(ctx, "scoped")
	if err != nil || len(scoped.Scope.MachineIDs) != 60 {
		t.Fatalf("scoped = %+v, %v", scoped, err)
	}
	if _, err := users.Get(ctx, "missing"); CodeOf(err) != CodeNotFound {
		t.Fatalf("missing user error = %v", err)
	}
}

func TestSessionsLifecycleAndBounds(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	db.now = func() time.Time { return clock }
	users := NewUsers(db)
	for _, user := range []User{
		{Username: "alice", PasswordHash: "hash-alice", Scope: AllMachines(), Admin: true},
		{Username: "bob", PasswordHash: "hash-bob", Scope: AllMachines()},
	} {
		if err := users.Create(ctx, "test", user); err != nil {
			t.Fatalf("create user %q: %v", user.Username, err)
		}
	}
	sessions := NewSessions(db)

	session := Session{
		TokenHash: "hash-alice",
		Username:  "alice",
		CSRFToken: "csrf-1",
		Scope:     AllMachines(),
		Admin:     true,
		CreatedAt: clock,
		ExpiresAt: clock.Add(12 * time.Hour),
	}
	if err := sessions.Create(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	found, err := sessions.Find(ctx, "hash-alice")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found.Username != "alice" || !found.Admin || !found.Scope.All || found.CSRFToken != "csrf-1" {
		t.Fatalf("found = %+v", found)
	}

	// Fixed expiry is enforced: moving the clock forward drops the session.
	clock = clock.Add(13 * time.Hour)
	if _, err := sessions.Find(ctx, "hash-alice"); CodeOf(err) != CodeNotFound {
		t.Fatalf("expired find error = %v", err)
	}
	count, err := sessions.Count(ctx)
	if err != nil || count != 0 {
		t.Fatalf("count after expiry = %d, %v", count, err)
	}

	// Many sessions are bounded, and DeleteUser ends every session for one
	// user without touching another.
	for index := 0; index < MaxSessions+20; index++ {
		if err := sessions.Create(ctx, Session{
			TokenHash: fmt.Sprintf("token-%d", index),
			Username:  "alice",
			CSRFToken: fmt.Sprintf("csrf-%d", index),
			Scope:     AllMachines(),
			CreatedAt: clock.Add(time.Duration(index) * time.Millisecond),
			ExpiresAt: clock.Add(24 * time.Hour),
		}); err != nil {
			t.Fatalf("create %d: %v", index, err)
		}
	}
	if err := sessions.Create(ctx, Session{
		TokenHash: "bob-token", Username: "bob", CSRFToken: "csrf-bob",
		Scope: AllMachines(), CreatedAt: clock, ExpiresAt: clock.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	count, _ = sessions.Count(ctx)
	if count != MaxSessions {
		t.Fatalf("session count = %d, want %d", count, MaxSessions)
	}
	ended, err := sessions.DeleteUser(ctx, "alice")
	if err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if ended == 0 {
		t.Fatal("expected alice sessions to end")
	}
	if _, err := sessions.Find(ctx, "bob-token"); err != nil {
		t.Fatalf("bob session should survive: %v", err)
	}
	if deleted, err := sessions.Delete(ctx, "bob-token"); err != nil || !deleted {
		t.Fatalf("delete session = %v, %v", deleted, err)
	}
	if deleted, _ := sessions.Delete(ctx, "bob-token"); deleted {
		t.Fatal("delete must be idempotent but report false")
	}
}

func TestSettingsAndMachinesMetadata(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	settings := NewSettings(db)
	if _, found, err := settings.Get(ctx, "missing"); err != nil || found {
		t.Fatalf("missing = %v, %v", found, err)
	}
	if err := settings.Set(ctx, "bootstrap_v1", "1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	value, found, err := settings.Get(ctx, "bootstrap_v1")
	if err != nil || !found || value != "1" {
		t.Fatalf("get = %q, %v, %v", value, found, err)
	}
	if err := settings.Set(ctx, "bootstrap_v1", "2"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	all, err := settings.All(ctx)
	if err != nil || all["bootstrap_v1"] != "2" {
		t.Fatalf("all = %v, %v", all, err)
	}
	if err := settings.Delete(ctx, "bootstrap_v1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := settings.Get(ctx, "bootstrap_v1"); found {
		t.Fatal("setting survived delete")
	}

	machines := NewMachines(db)
	meta, err := machines.Get(ctx, "machine-1")
	if err != nil || meta.DisplayName != nil || len(meta.Tags) != 0 {
		t.Fatalf("missing meta = %+v, %v", meta, err)
	}
	name := "Desk"
	tags := []string{"prod", "desk"}
	if err := machines.Update(ctx, "machine-1", MetaUpdate{DisplayName: &name, Tags: &tags}); err != nil {
		t.Fatalf("set meta: %v", err)
	}
	// A partial update preserves the fields it does not mention.
	renamed := "Workstation"
	if err := machines.Update(ctx, "machine-1", MetaUpdate{DisplayName: &renamed}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	meta, _ = machines.Get(ctx, "machine-1")
	if meta.DisplayName == nil || *meta.DisplayName != "Workstation" || len(meta.Tags) != 2 {
		t.Fatalf("meta = %+v", meta)
	}
	if err := machines.Update(ctx, "machine-1", MetaUpdate{ClearDisplayName: true}); err != nil {
		t.Fatalf("clear name: %v", err)
	}
	meta, _ = machines.Get(ctx, "machine-1")
	if meta.DisplayName != nil || len(meta.Tags) != 2 {
		t.Fatalf("meta after clear = %+v", meta)
	}
	if err := machines.Update(ctx, "machine-2", MetaUpdate{Tags: &[]string{"lab"}}); err != nil {
		t.Fatalf("tags only: %v", err)
	}
	listed, err := machines.List(ctx)
	if err != nil || len(listed) != 2 {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	if err := machines.Update(ctx, "machine-1", MetaUpdate{Tags: &[]string{"a", "a"}}); CodeOf(err) != CodeInvalid {
		t.Fatalf("duplicate tags error = %v", err)
	}
	if err := machines.Update(ctx, "machine-1", MetaUpdate{DisplayName: &name, ClearDisplayName: true}); CodeOf(err) != CodeInvalid {
		t.Fatalf("ambiguous update error = %v", err)
	}
}

func TestAuditIsBoundedPerScope(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	audit := NewAudit(db)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	// Seed an oversized history directly, then one real append prunes it.
	if err := db.Write(ctx, func(tx *Tx) error {
		for index := 0; index < 1200; index++ {
			if _, err := tx.Exec(
				"INSERT INTO audit_events (scope, actor, action, target, at_ms) VALUES ('admin', 'owner', 'user.update', ?, ?)",
				fmt.Sprintf("target-%d", index), now.UnixMilli(),
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed audit: %v", err)
	}
	if err := audit.Append(ctx, AuditEntry{Scope: AuditScopeAdmin, Actor: "owner", Action: "user.delete", Target: "bob", At: now}); err != nil {
		t.Fatalf("append: %v", err)
	}
	entries, err := audit.List(ctx, AuditScopeAdmin, 5000)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != MaxAuditEvents {
		t.Fatalf("audit rows = %d, want %d", len(entries), MaxAuditEvents)
	}
	if entries[0].Action != "user.delete" {
		t.Fatalf("newest = %+v", entries[0])
	}
	if err := audit.Append(ctx, AuditEntry{Scope: AuditScopeCollector, Actor: "owner", Action: "grant-create", Target: "new-installation"}); err != nil {
		t.Fatalf("append collector: %v", err)
	}
	collectorEntries, err := audit.List(ctx, AuditScopeCollector, 0)
	if err != nil || len(collectorEntries) != 1 {
		t.Fatalf("collector audit = %+v, %v", collectorEntries, err)
	}
	if _, err := audit.List(ctx, AuditScopeAdmin, 0); err != nil {
		t.Fatalf("admin audit after collector append: %v", err)
	}
	if err := audit.Append(ctx, AuditEntry{Scope: "other", Actor: "a", Action: "b", Target: "c"}); CodeOf(err) != CodeInvalid {
		t.Fatalf("scope error = %v", err)
	}
	if _, err := audit.List(ctx, AuditScopeAdmin, 0); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := audit.List(ctx, AuditScopeAdmin, -1); CodeOf(err) != CodeInvalid {
		t.Fatalf("negative limit error = %v", err)
	}
}

var _ = context.Background

func TestBootstrapUsersImportsOnceAndPreservesManagedState(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	users := NewUsers(db)

	changed, err := users.BootstrapUsers(ctx, nil, nil)
	if err != nil || changed {
		t.Fatalf("empty first start = %v, %v", changed, err)
	}
	configured := []BootstrapUser{
		{Username: "owner", PasswordHash: "hash-owner", Scope: AllMachines()},
		{Username: "viewer", PasswordHash: "hash-viewer", Scope: AllMachines()},
	}
	changed, err = users.BootstrapUsers(ctx, configured, []string{"owner"})
	if err != nil || !changed {
		t.Fatalf("configured start = %v, %v", changed, err)
	}
	owner, err := users.Get(ctx, "owner")
	if err != nil || owner.PasswordHash != "hash-owner" || !owner.Admin || !owner.Scope.All {
		t.Fatalf("owner = %+v, %v", owner, err)
	}
	viewer, err := users.Get(ctx, "viewer")
	if err != nil || viewer.Admin {
		t.Fatalf("viewer = %+v, %v", viewer, err)
	}
	// A second start never overwrites managed state or resurrects a deleted
	// user, even if the explicit elevation list changed.
	if err := users.Update(ctx, "owner", User{Username: "owner", PasswordHash: "managed-hash", Scope: MachineScope("m-1"), Admin: true}); err != nil {
		t.Fatalf("manage owner: %v", err)
	}
	if err := users.Delete(ctx, "owner", "viewer"); err != nil {
		t.Fatalf("delete viewer: %v", err)
	}
	changed, err = users.BootstrapUsers(ctx, []BootstrapUser{
		{Username: "owner", PasswordHash: "hash-owner", Scope: AllMachines()},
		{Username: "viewer", PasswordHash: "hash-viewer", Scope: AllMachines()},
		{Username: "elevated-later", PasswordHash: "hash-late", Scope: AllMachines()},
	}, []string{"elevated-later"})
	if err != nil || changed {
		t.Fatalf("second start = %v, %v", changed, err)
	}
	owner, _ = users.Get(ctx, "owner")
	if owner.PasswordHash != "managed-hash" || owner.Scope.All || len(owner.Scope.MachineIDs) != 1 {
		t.Fatalf("bootstrap overwrote managed owner: %+v", owner)
	}
	if _, err := users.Get(ctx, "viewer"); CodeOf(err) != CodeNotFound {
		t.Fatalf("viewer resurrected: %v", err)
	}
	if _, err := users.Get(ctx, "elevated-later"); CodeOf(err) != CodeNotFound {
		t.Fatalf("second-start user imported: %v", err)
	}

	// The directory bound applies before anything is written.
	tooMany := make([]BootstrapUser, MaxUsers+1)
	for index := range tooMany {
		tooMany[index] = BootstrapUser{Username: fmt.Sprintf("u-%d", index), PasswordHash: "hash", Scope: AllMachines()}
	}
	otherDB, _ := openTestDatabase(t)
	if _, err := NewUsers(otherDB).BootstrapUsers(ctx, tooMany, nil); CodeOf(err) != CodeInvalid {
		t.Fatalf("bootstrap cap error = %v", err)
	}
	if listed, _ := NewUsers(otherDB).List(ctx); len(listed) != 0 {
		t.Fatalf("oversized bootstrap wrote %d users", len(listed))
	}
}

func TestColocatedPairingActorIsAReservedUsername(t *testing.T) {
	ctx := testContext(t)
	db, _ := openTestDatabase(t)
	users := NewUsers(db)
	err := users.Create(ctx, "bootstrap", User{
		Username: BootstrapGrantUseColocatedPairing, PasswordHash: "hash", Scope: AllMachines(), Admin: true,
	})
	if CodeOf(err) != CodeInvalid {
		t.Fatalf("creating the reserved username = %v, want an invalid-input error", err)
	}
}
