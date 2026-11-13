package storage

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const publicProof = "synthetic-public-recovery-proof-1234567890"

func openRegistry(t *testing.T, clock *time.Time, opts RegistryOptions) *Registry {
	t.Helper()
	db, _ := openTestDatabase(t)
	if clock != nil {
		db.now = func() time.Time { return *clock }
	}
	registry, err := NewRegistry(testContext(t), db, opts)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	return registry
}

func TestEnrollmentRecoveryAndRevocation(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	registry := openRegistry(t, &clock, RegistryOptions{})
	grant, err := registry.CreateGrant(ctx, "owner", GrantOptions{})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	request := EnrollRequest{
		CollectorID:    "new-installation",
		Token:          grant.Token,
		RequestID:      "saved-request",
		RecoverySecret: publicProof,
	}
	first, err := registry.Enroll(ctx, request)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if first.MachineID == "" || first.Token == "" || first.Token == grant.Token {
		t.Fatalf("issued identity = %+v", first)
	}
	// A lost success reply is recoverable once with the saved proof.
	replayed, err := registry.Enroll(ctx, request)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if replayed != first {
		t.Fatalf("recovered identity = %+v, want %+v", replayed, first)
	}
	// Without the proof the same grant is denied.
	if _, err := registry.Enroll(ctx, EnrollRequest{CollectorID: request.CollectorID, Token: grant.Token}); CodeOf(err) != CodeForbidden {
		t.Fatalf("replay without proof error = %v", err)
	}
	if _, err := registry.Enroll(ctx, EnrollRequest{
		CollectorID: request.CollectorID, Token: grant.Token,
		RequestID: request.RequestID, RecoverySecret: publicProof + "wrong",
	}); CodeOf(err) != CodeForbidden {
		t.Fatalf("wrong proof error = %v", err)
	}
	// The active credential authenticates; revoking ends that and a recovery
	// retry can no longer resurrect it.
	authenticated, ok, err := registry.Authenticate(ctx, first.Token)
	if err != nil || !ok || authenticated.CollectorID != first.CollectorID {
		t.Fatalf("authenticate = %+v, %v, %v", authenticated, ok, err)
	}
	if err := registry.Revoke(ctx, "owner", first.CollectorID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if identities, err := registry.Identities(ctx); err != nil || len(identities) != 0 {
		t.Fatalf("identities after revoke = %+v, %v", identities, err)
	}
	if _, ok, _ := registry.Authenticate(ctx, first.Token); ok {
		t.Fatal("revoked credential still authenticates")
	}
	if _, err := registry.Enroll(ctx, request); CodeOf(err) != CodeForbidden {
		t.Fatalf("recovery after revoke error = %v", err)
	}
	listed, err := registry.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listedJSON, _ := json.Marshal(listed)
	if strings.Contains(string(listedJSON), first.Token) {
		t.Fatalf("collector list leaked the credential: %s", listedJSON)
	}
	audited, err := registry.AuditEvents(ctx)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	auditedJSON, _ := json.Marshal(audited)
	if strings.Contains(string(auditedJSON), grant.Token) || strings.Contains(string(auditedJSON), first.Token) {
		t.Fatalf("audit leaked token material: %s", auditedJSON)
	}
}

func TestEnrollmentReplacementRotatesTokenAndPreservesMapping(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	old := Identity{CollectorID: "old-installation", MachineID: "original-machine", Token: "public-old-token"}
	registry := openRegistry(t, &clock, RegistryOptions{BootstrapIdentities: []Identity{old}})
	if identities, err := registry.Identities(ctx); err != nil || len(identities) != 1 ||
		identities[0].CollectorID != old.CollectorID || identities[0].MachineID != old.MachineID || identities[0].Token != "" {
		t.Fatalf("bootstrap identities = %+v, %v", identities, err)
	}
	// Rotating the same installation without a replacement grant is denied.
	grant, err := registry.CreateGrant(ctx, "owner", GrantOptions{})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := registry.Enroll(ctx, EnrollRequest{CollectorID: old.CollectorID, Token: grant.Token}); CodeOf(err) != CodeConflict {
		t.Fatalf("rotation without replacement error = %v", err)
	}
	replacement, err := registry.CreateGrant(ctx, "owner", GrantOptions{ReplaceMachineID: old.MachineID})
	if err != nil {
		t.Fatalf("replacement grant: %v", err)
	}
	next, err := registry.Enroll(ctx, EnrollRequest{CollectorID: "replacement-installation", Token: replacement.Token})
	if err != nil {
		t.Fatalf("replacement enroll: %v", err)
	}
	if next.MachineID != old.MachineID || next.Token == old.Token {
		t.Fatalf("replacement = %+v", next)
	}
	if _, ok, _ := registry.Authenticate(ctx, old.Token); ok {
		t.Fatal("old credential survived replacement")
	}
	if authenticated, ok, _ := registry.Authenticate(ctx, next.Token); !ok || authenticated.CollectorID != next.CollectorID {
		t.Fatalf("new credential = %+v, %v", authenticated, ok)
	}
	// A restart with the same bootstrap configuration never resurrects the old
	// credential or mapping.
	db := registry.db
	reopened, err := NewRegistry(ctx, db, RegistryOptions{BootstrapIdentities: []Identity{old}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	identities, err := reopened.Identities(ctx)
	if err != nil || len(identities) != 1 || identities[0].CollectorID != next.CollectorID ||
		identities[0].MachineID != next.MachineID || identities[0].Token != "" {
		t.Fatalf("reopened identities = %+v, %v", identities, err)
	}
}

func TestEnrollmentStoresOnlyHashedCredentialsAndDropsRecoveryCopy(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	registry := openRegistry(t, &clock, RegistryOptions{})
	grant, err := registry.CreateGrant(ctx, "owner", GrantOptions{})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	request := EnrollRequest{
		CollectorID: "hashed-installation", Token: grant.Token,
		RequestID: "hashed-request", RecoverySecret: publicProof,
	}
	issued, err := registry.Enroll(ctx, request)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	var (
		legacy   string
		hash     string
		sealed   []byte
		material []byte
	)
	if err := registry.db.QueryRow(ctx,
		"SELECT token, token_hash, credential_sealed, credential_nonce FROM collector_registry WHERE collector_id = ?",
		issued.CollectorID,
	).Scan(&legacy, &hash, &sealed, &material); err != nil {
		t.Fatalf("read credential row: %v", err)
	}
	if legacy == issued.Token || strings.Contains(legacy, issued.Token) {
		t.Fatal("legacy token column still holds the credential")
	}
	if hash != hashToken(issued.Token) || legacy != hash {
		t.Fatalf("token_hash = %q legacy = %q", hash, legacy)
	}
	if len(sealed) == 0 || strings.Contains(string(sealed), issued.Token) {
		t.Fatal("recovery copy is missing or stored in plaintext")
	}
	recovered, err := openCredential(publicProof, sealed, material)
	if err != nil || recovered != issued.Token {
		t.Fatalf("sealed copy does not decrypt with the recovery secret: %q, %v", recovered, err)
	}
	if _, err := openCredential(publicProof+"wrong", sealed, material); err == nil {
		t.Fatal("sealed copy decrypted with a wrong recovery secret")
	}
	// The same-credential retry contract still holds before authentication.
	replayed, err := registry.Enroll(ctx, request)
	if err != nil || replayed != issued {
		t.Fatalf("recovery before auth = %+v, %v", replayed, err)
	}
	// A first successful authenticated connection removes the recovery copy.
	if authenticated, ok, err := registry.Authenticate(ctx, issued.Token); err != nil || !ok ||
		authenticated.CollectorID != issued.CollectorID {
		t.Fatalf("authenticate = %+v, %v, %v", authenticated, ok, err)
	}
	var remaining []byte
	if err := registry.db.QueryRow(ctx,
		"SELECT credential_sealed FROM collector_registry WHERE collector_id = ?", issued.CollectorID,
	).Scan(&remaining); err != nil {
		t.Fatalf("read recovery copy: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatal("recovery copy survived the first authenticated connection")
	}
	if _, err := registry.Enroll(ctx, request); CodeOf(err) != CodeForbidden {
		t.Fatalf("recovery after authentication error = %v", err)
	}
}

func TestMigrationHashesLegacyCollectorCredentials(t *testing.T) {
	ctx := testContext(t)
	dir := privateTempDir(t)
	path := filepath.Join(dir, "legacy-credentials.db")
	saved := migrations
	defer func() { migrations = saved }()

	migrations = saved[:4]
	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("open schema 4: %v", err)
	}
	plaintext := "legacy-plaintext-credential"
	if err := db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(
			"INSERT INTO collector_registry (collector_id, machine_id, token, revoked, created_at_ms, updated_at_ms) VALUES (?, ?, ?, 0, 1, 1)",
			"legacy-installation", "legacy-machine", plaintext,
		)
		return err
	}); err != nil {
		t.Fatalf("seed legacy credential: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	migrations = saved
	upgraded, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer upgraded.Close()
	var token, hash string
	if err := upgraded.QueryRow(ctx,
		"SELECT token, token_hash FROM collector_registry WHERE collector_id = 'legacy-installation'",
	).Scan(&token, &hash); err != nil {
		t.Fatalf("read hashed row: %v", err)
	}
	if token == plaintext || strings.Contains(token, plaintext) {
		t.Fatal("migration left the plaintext credential in place")
	}
	if hash != hashToken(plaintext) || token != hash {
		t.Fatalf("hashed row = %q/%q", token, hash)
	}
	registry, err := NewRegistry(ctx, upgraded, RegistryOptions{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	if identity, ok, err := registry.Authenticate(ctx, plaintext); err != nil || !ok || identity.MachineID != "legacy-machine" {
		t.Fatalf("legacy credential authenticate = %+v, %v, %v", identity, ok, err)
	}
}

func TestEnrollmentRetainsSpentGrantHashes(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	token := "public-already-consumed-grant"
	registry := openRegistry(t, &clock, RegistryOptions{
		ConsumedGrantHashes: []string{hashToken(token)},
		Grants:              []EnrollmentGrant{{Token: token, ExpiresAt: clock.Add(10 * time.Minute)}},
	})
	if _, err := registry.Enroll(ctx, EnrollRequest{CollectorID: "another-installation", Token: token}); CodeOf(err) != CodeForbidden {
		t.Fatalf("spent grant error = %v", err)
	}
	// The token itself is never stored in the grant table.
	var count int
	if err := registry.db.QueryRow(ctx, "SELECT COUNT(*) FROM collector_grants WHERE token_hash = ?", token).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatal("grant stored a plaintext token")
	}
}

func TestEnrollmentEmptyFirstStartDoesNotConsumeBootstrap(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	db, _ := openTestDatabase(t)
	db.now = func() time.Time { return clock }
	empty, err := NewRegistry(ctx, db, RegistryOptions{})
	if err != nil {
		t.Fatalf("empty registry: %v", err)
	}
	if identities, err := empty.Identities(ctx); err != nil || len(identities) != 0 {
		t.Fatalf("empty identities = %+v, %v", identities, err)
	}
	identity := Identity{CollectorID: "first-configured", MachineID: "first-machine", Token: "public-bootstrap-token"}
	configured, err := NewRegistry(ctx, db, RegistryOptions{BootstrapIdentities: []Identity{identity}})
	if err != nil {
		t.Fatalf("configured registry: %v", err)
	}
	if identities, err := configured.Identities(ctx); err != nil || len(identities) != 1 ||
		identities[0].CollectorID != identity.CollectorID || identities[0].MachineID != identity.MachineID {
		t.Fatalf("configured identities = %+v, %v", identities, err)
	}
	if err := configured.Revoke(ctx, "owner", identity.CollectorID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	reopened, err := NewRegistry(ctx, db, RegistryOptions{BootstrapIdentities: []Identity{identity}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if identities, _ := reopened.Identities(ctx); len(identities) != 0 {
		t.Fatalf("revoked bootstrap identity resurrected: %+v", identities)
	}
}

func TestEnrollmentOverFiftyPersistsAndRevokesIndependently(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	registry := openRegistry(t, &clock, RegistryOptions{})
	issued := make([]Identity, 0, 51)
	for index := 0; index < 51; index++ {
		grant, err := registry.CreateGrant(ctx, "owner", GrantOptions{})
		if err != nil {
			t.Fatalf("grant %d: %v", index, err)
		}
		identity, err := registry.Enroll(ctx, EnrollRequest{CollectorID: "installation-" + itoa(index), Token: grant.Token})
		if err != nil {
			t.Fatalf("enroll %d: %v", index, err)
		}
		issued = append(issued, identity)
	}
	machines := make(map[string]struct{})
	for _, identity := range issued {
		machines[identity.MachineID] = struct{}{}
	}
	if len(machines) != 51 {
		t.Fatalf("machine ids = %d, want 51", len(machines))
	}
	reopened, err := NewRegistry(ctx, registry.db, RegistryOptions{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	identities, err := reopened.Identities(ctx)
	if err != nil || len(identities) != 51 {
		t.Fatalf("reopened identities = %d, %v", len(identities), err)
	}
	if err := reopened.Revoke(ctx, "owner", issued[10].CollectorID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	identities, _ = reopened.Identities(ctx)
	if len(identities) != 50 {
		t.Fatalf("identities after revoke = %d", len(identities))
	}
	if _, ok, _ := reopened.Authenticate(ctx, issued[10].Token); ok {
		t.Fatal("revoked credential still authenticates")
	}
	if _, ok, _ := reopened.Authenticate(ctx, issued[11].Token); !ok {
		t.Fatal("surviving credential stopped authenticating")
	}
}

func TestEnrollmentGrantBoundsAndExpiry(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	registry := openRegistry(t, &clock, RegistryOptions{})
	if _, err := registry.CreateGrant(ctx, "owner", GrantOptions{TTL: 30 * time.Second}); CodeOf(err) != CodeInvalid {
		t.Fatalf("short ttl error = %v", err)
	}
	if _, err := registry.CreateGrant(ctx, "owner", GrantOptions{TTL: 20 * time.Minute}); CodeOf(err) != CodeInvalid {
		t.Fatalf("long ttl error = %v", err)
	}
	if _, err := registry.CreateGrant(ctx, "owner", GrantOptions{ReplaceMachineID: "missing-machine"}); CodeOf(err) != CodeInvalid {
		t.Fatalf("missing replacement error = %v", err)
	}
	grant, err := registry.CreateGrant(ctx, "owner", GrantOptions{TTL: time.Minute})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	clock = clock.Add(2 * time.Minute)
	if _, err := registry.Enroll(ctx, EnrollRequest{CollectorID: "late", Token: grant.Token}); CodeOf(err) != CodeForbidden {
		t.Fatalf("expired grant error = %v", err)
	}
	// The grant budget is finite and expired grants are pruned.
	for index := 0; index < MaxEnrollmentGrants; index++ {
		if _, err := registry.CreateGrant(ctx, "owner", GrantOptions{}); err != nil {
			t.Fatalf("grant %d: %v", index, err)
		}
	}
	if _, err := registry.CreateGrant(ctx, "owner", GrantOptions{}); CodeOf(err) != CodeConflict {
		t.Fatalf("grant cap error = %v", err)
	}
}

func TestBootstrapGrantHashIsImportedOnceAndUsable(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	token, hash, err := NewEnrollmentGrantToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if len(hash) != 64 || strings.Contains(hash, token) {
		t.Fatalf("token hash = %q for token %q", hash, token)
	}
	db, _ := openTestDatabase(t)
	db.now = func() time.Time { return clock }
	opts := RegistryOptions{BootstrapGrants: []BootstrapGrant{{
		TokenHash:   hash,
		ExpiresAt:   clock.Add(time.Hour),
		Use:         BootstrapGrantUseColocatedPairing,
		CollectorID: "auto-paired",
	}}}
	registry, err := NewRegistry(ctx, db, opts)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	request := EnrollRequest{CollectorID: "auto-paired", Token: token, LocalPeer: true}
	identity, err := registry.Enroll(ctx, request)
	if err != nil {
		t.Fatalf("enroll with bootstrap grant: %v", err)
	}
	if identity.MachineID == "" || identity.Token == "" {
		t.Fatalf("issued identity = %+v", identity)
	}
	// A restart re-reads the same bootstrap options; the consumed grant must
	// stay consumed and never issue a second machine.
	reopened, err := NewRegistry(ctx, db, opts)
	if err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	if _, err := reopened.Enroll(ctx, EnrollRequest{CollectorID: "second", Token: token, LocalPeer: true}); CodeOf(err) != CodeForbidden {
		t.Fatalf("replayed bootstrap grant error = %v", err)
	}
	if identities, err := reopened.Identities(ctx); err != nil || len(identities) != 1 {
		t.Fatalf("identities = %+v, %v", identities, err)
	}
}

// TestRotatedColocatedGrantRecoversALostResponseWithTheSavedProof proves the
// installer's grant rotation keeps a lost enrollment response recoverable: a
// reinstall stages a fresh colocated grant bound to the same installation and
// keeps the proof saved before the lost response.
func TestRotatedColocatedGrantRecoversALostResponseWithTheSavedProof(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	token, hash, err := NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	registry := openRegistry(t, &clock, RegistryOptions{BootstrapGrants: []BootstrapGrant{{
		TokenHash:   hash,
		ExpiresAt:   clock.Add(time.Hour),
		Use:         BootstrapGrantUseColocatedPairing,
		CollectorID: "reinstalled",
	}}})
	request := EnrollRequest{
		CollectorID:    "reinstalled",
		Token:          token,
		RequestID:      "lost-response",
		RecoverySecret: publicProof,
		LocalPeer:      true,
	}
	first, err := registry.Enroll(ctx, request)
	if err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	// A reinstall stages a fresh grant for the same installation but keeps the
	// proof saved before the lost response. Redeeming the fresh grant with that
	// proof returns the credential the hub already issued.
	secondToken, secondHash, err := NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.ImportBootstrapGrants(ctx, []BootstrapGrant{{
		TokenHash:   secondHash,
		ExpiresAt:   clock.Add(time.Hour),
		Use:         BootstrapGrantUseColocatedPairing,
		CollectorID: request.CollectorID,
	}}); err != nil {
		t.Fatalf("import the rotated grant: %v", err)
	}
	recovered, err := registry.Enroll(ctx, EnrollRequest{
		CollectorID:    request.CollectorID,
		Token:          secondToken,
		RequestID:      request.RequestID,
		RecoverySecret: request.RecoverySecret,
		LocalPeer:      true,
	})
	if err != nil {
		t.Fatalf("recover with a rotated grant: %v", err)
	}
	if recovered != first {
		t.Fatalf("recovered identity = %+v, want %+v", recovered, first)
	}
	// A rotated grant with a different proof is not a recovery and the
	// collector already exists, so it is denied.
	thirdToken, thirdHash, err := NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.ImportBootstrapGrants(ctx, []BootstrapGrant{{
		TokenHash:   thirdHash,
		ExpiresAt:   clock.Add(time.Hour),
		Use:         BootstrapGrantUseColocatedPairing,
		CollectorID: request.CollectorID,
	}}); err != nil {
		t.Fatalf("import the third grant: %v", err)
	}
	if _, err := registry.Enroll(ctx, EnrollRequest{
		CollectorID:    request.CollectorID,
		Token:          thirdToken,
		RequestID:      "another-request",
		RecoverySecret: publicProof,
		LocalPeer:      true,
	}); err == nil {
		t.Fatal("rotated grant with a mismatched proof enrolled a second identity")
	}
}

// TestSameGrantRecoveryOutlivesGrantExpiry documents the one recovery branch a
// spent grant keeps: its recorded 24 h window and the proof, not the grant's own
// expiry. The grant was validly redeemed, so its expiry protects nothing.
func TestSameGrantRecoveryOutlivesGrantExpiry(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	registry := openRegistry(t, &clock, RegistryOptions{})
	grant, err := registry.CreateGrant(ctx, "owner", GrantOptions{TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	request := EnrollRequest{
		CollectorID:    "slow-response",
		Token:          grant.Token,
		RequestID:      "lost-response",
		RecoverySecret: publicProof,
	}
	first, err := registry.Enroll(ctx, request)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	clock = clock.Add(2 * time.Minute)
	recovered, err := registry.Enroll(ctx, request)
	if err != nil {
		t.Fatalf("recover after the grant expired: %v", err)
	}
	if recovered != first {
		t.Fatalf("recovered identity = %+v, want %+v", recovered, first)
	}
}

// TestExpiredUnrelatedGrantCannotRecoverAColocatedCredential is the reviewer's
// scenario: an unrelated, already expired administrator grant presented with a
// colocated collector's proof must not return that collector's credential, must
// not be consumed, and must not acquire a recovery window. Before the fix this
// cross-grant recovery was gated only by the proof, so any grant worked as a key.
func TestExpiredUnrelatedGrantCannotRecoverAColocatedCredential(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	token, hash, err := NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	db, _ := openTestDatabase(t)
	db.now = func() time.Time { return clock }
	registry, err := NewRegistry(ctx, db, RegistryOptions{BootstrapGrants: []BootstrapGrant{{
		TokenHash:   hash,
		ExpiresAt:   clock.Add(time.Hour),
		Use:         BootstrapGrantUseColocatedPairing,
		CollectorID: "colocated-collector",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	colocated := EnrollRequest{
		CollectorID:    "colocated-collector",
		Token:          token,
		RequestID:      "pairing-request",
		RecoverySecret: publicProof,
		LocalPeer:      true,
	}
	issued, err := registry.Enroll(ctx, colocated)
	if err != nil {
		t.Fatalf("colocated enroll: %v", err)
	}
	adminGrant, err := registry.CreateGrant(ctx, "owner", GrantOptions{TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	adminHash := hashToken(adminGrant.Token)

	// The same proof from a remote peer, through the unrelated admin grant.
	for _, localPeer := range []bool{false, true} {
		_, err := registry.Enroll(ctx, EnrollRequest{
			CollectorID:    colocated.CollectorID,
			Token:          adminGrant.Token,
			RequestID:      colocated.RequestID,
			RecoverySecret: colocated.RecoverySecret,
			LocalPeer:      localPeer,
		})
		if CodeOf(err) != CodeForbidden {
			t.Fatalf("expired administrator grant recovery (localPeer=%v) error = %v, want forbidden", localPeer, err)
		}
	}
	var consumedBy, recoveryHash sql.NullString
	var recoveryUntil sql.NullInt64
	if err := db.QueryRow(ctx,
		"SELECT consumed_by, recovery_hash, recovery_until_ms FROM collector_grants WHERE token_hash = ?", adminHash,
	).Scan(&consumedBy, &recoveryHash, &recoveryUntil); err != nil {
		t.Fatalf("read the administrator grant: %v", err)
	}
	if consumedBy.Valid || recoveryHash.Valid || recoveryUntil.Valid {
		t.Fatalf("denied recovery mutated the unrelated grant: consumed_by=%v recovery_hash=%v recovery_until=%v",
			consumedBy, recoveryHash, recoveryUntil)
	}
	if _, ok, err := registry.Authenticate(ctx, issued.Token); err != nil || !ok {
		t.Fatalf("the colocated credential was invalidated by the denied request: ok=%v err=%v", ok, err)
	}
}

// TestColocatedRecoveryIgnoresAConsumedGrantForAnotherCollector proves the
// matched grant's binding is part of the rule: another installation's consumed
// colocated grant never unlocks this collector's credential.
func TestColocatedRecoveryIgnoresAConsumedGrantForAnotherCollector(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	firstToken, firstHash, err := NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	registry := openRegistry(t, &clock, RegistryOptions{BootstrapGrants: []BootstrapGrant{{
		TokenHash:   firstHash,
		ExpiresAt:   clock.Add(time.Hour),
		Use:         BootstrapGrantUseColocatedPairing,
		CollectorID: "first-install",
	}}})
	first := EnrollRequest{
		CollectorID:    "first-install",
		Token:          firstToken,
		RequestID:      "shared-request",
		RecoverySecret: publicProof,
		LocalPeer:      true,
	}
	if _, err := registry.Enroll(ctx, first); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	secondToken, secondHash, err := NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.ImportBootstrapGrants(ctx, []BootstrapGrant{{
		TokenHash:   secondHash,
		ExpiresAt:   clock.Add(time.Hour),
		Use:         BootstrapGrantUseColocatedPairing,
		CollectorID: "second-install",
	}}); err != nil {
		t.Fatalf("import second grant: %v", err)
	}
	// Same request id and proof, different collector: no recovery, and the
	// second installation has no identity yet, so this is an ordinary first
	// redemption, not a conflict.
	issued, err := registry.Enroll(ctx, EnrollRequest{
		CollectorID:    "second-install",
		Token:          secondToken,
		RequestID:      first.RequestID,
		RecoverySecret: first.RecoverySecret,
		LocalPeer:      true,
	})
	if err != nil {
		t.Fatalf("second enroll: %v", err)
	}
	if issued.CollectorID != "second-install" || issued.MachineID == "" {
		t.Fatalf("issued identity = %+v", issued)
	}
}

// TestRolledBackColocatedImportIsRefused covers a schema-6 binary importing the
// bootstrap after a rollback: it records actor=colocated-pairing but has no
// purpose or collector binding columns. Such a row must never be redeemable as
// an ordinary administrator grant; a reinstall stages a fresh bound grant.
func TestRolledBackColocatedImportIsRefused(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	grant := "rolled-back-bootstrap-grant"
	db, _ := openTestDatabase(t)
	db.now = func() time.Time { return clock }
	registry, err := NewRegistry(ctx, db, RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(
			"INSERT INTO collector_grants (token_hash, expires_at_ms, actor, created_at_ms) VALUES (?, ?, ?, ?)",
			hashToken(grant), clock.Add(time.Hour).UnixMilli(), BootstrapGrantUseColocatedPairing, clock.UnixMilli(),
		)
		return err
	}); err != nil {
		t.Fatalf("simulate a rolled-back import: %v", err)
	}
	for _, localPeer := range []bool{true, false} {
		if _, err := registry.Enroll(ctx, EnrollRequest{
			CollectorID: "local-install",
			Token:       grant,
			LocalPeer:   localPeer,
		}); CodeOf(err) != CodeForbidden {
			t.Fatalf("unbound rolled-back grant (localPeer=%v) error = %v, want forbidden", localPeer, err)
		}
	}
	if identities, err := registry.Identities(ctx); err != nil || len(identities) != 0 {
		t.Fatalf("identities after the refused import = %+v, %v", identities, err)
	}
}

func TestColocatedPairingGrantRequiresLoopbackAndBoundCollector(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	token, hash, err := NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	registry := openRegistry(t, &clock, RegistryOptions{BootstrapGrants: []BootstrapGrant{{
		TokenHash:   hash,
		ExpiresAt:   clock.Add(time.Hour),
		Use:         BootstrapGrantUseColocatedPairing,
		CollectorID: "bound-collector",
	}}})
	// Another collector id cannot spend the grant even from loopback.
	if _, err := registry.Enroll(ctx, EnrollRequest{CollectorID: "intruder", Token: token, LocalPeer: true}); CodeOf(err) != CodeForbidden {
		t.Fatalf("wrong collector id error = %v", err)
	}
	// The bound collector cannot spend it from a remote socket.
	if _, err := registry.Enroll(ctx, EnrollRequest{CollectorID: "bound-collector", Token: token}); CodeOf(err) != CodeForbidden {
		t.Fatalf("remote peer error = %v", err)
	}
	request := EnrollRequest{
		CollectorID:    "bound-collector",
		Token:          token,
		RequestID:      "pairing-request",
		RecoverySecret: publicProof,
		LocalPeer:      true,
	}
	first, err := registry.Enroll(ctx, request)
	if err != nil {
		t.Fatalf("loopback enroll: %v", err)
	}
	// Recovery is subject to the same rule: a remote retry with the right
	// proof is still denied, and the loopback retry recovers the credential.
	remote := request
	remote.LocalPeer = false
	if _, err := registry.Enroll(ctx, remote); CodeOf(err) != CodeForbidden {
		t.Fatalf("remote recovery error = %v", err)
	}
	recovered, err := registry.Enroll(ctx, request)
	if err != nil {
		t.Fatalf("loopback recovery: %v", err)
	}
	if recovered != first {
		t.Fatalf("recovered identity = %+v, want %+v", recovered, first)
	}
}

func TestExpiredBootstrapGrantIsIgnored(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	token, hash, err := NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	registry := openRegistry(t, &clock, RegistryOptions{BootstrapGrants: []BootstrapGrant{{
		TokenHash:   hash,
		ExpiresAt:   clock.Add(-time.Minute),
		Use:         BootstrapGrantUseColocatedPairing,
		CollectorID: "late",
	}}})
	if _, err := registry.Enroll(ctx, EnrollRequest{CollectorID: "late", Token: token, LocalPeer: true}); CodeOf(err) != CodeForbidden {
		t.Fatalf("expired bootstrap grant error = %v", err)
	}
}

func TestBootstrapGrantValidation(t *testing.T) {
	ctx := testContext(t)
	clock := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	token, hash, err := NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		grant BootstrapGrant
	}{
		{"bad hash", BootstrapGrant{TokenHash: token, ExpiresAt: clock.Add(time.Minute), Use: BootstrapGrantUseColocatedPairing, CollectorID: "c"}},
		{"missing expiry", BootstrapGrant{TokenHash: hash, Use: BootstrapGrantUseColocatedPairing, CollectorID: "c"}},
		{"too long", BootstrapGrant{TokenHash: hash, ExpiresAt: clock.Add(2 * time.Hour), Use: BootstrapGrantUseColocatedPairing, CollectorID: "c"}},
		{"missing use", BootstrapGrant{TokenHash: hash, ExpiresAt: clock.Add(time.Minute)}},
		{"missing collector id", BootstrapGrant{TokenHash: hash, ExpiresAt: clock.Add(time.Minute), Use: BootstrapGrantUseColocatedPairing}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := openTestDatabase(t)
			db.now = func() time.Time { return clock }
			if _, err := NewRegistry(ctx, db, RegistryOptions{BootstrapGrants: []BootstrapGrant{tc.grant}}); CodeOf(err) != CodeInvalid {
				t.Fatalf("error = %v, want invalid", err)
			}
		})
	}
}

func TestNewEnrollmentGrantTokenIsRandom(t *testing.T) {
	first, firstHash, err := NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	second, secondHash, err := NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || firstHash == secondHash {
		t.Fatalf("tokens repeat: %q %q", first, second)
	}
}
