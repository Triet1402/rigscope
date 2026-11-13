package storage

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	// MaxEnrollmentGrantTTL is the longest lifetime a one-time grant may have.
	MaxEnrollmentGrantTTL = 15 * time.Minute
	// MaxBootstrapGrantTTL bounds a grant imported from the hub's bootstrap
	// file. The automatic local pairing grant must survive a slow first hub
	// start, so it is much longer than an administrator-issued grant.
	MaxBootstrapGrantTTL = time.Hour
	// BootstrapGrantUseColocatedPairing marks the grant the installer creates
	// to pair a collector with the hub on the same machine.
	BootstrapGrantUseColocatedPairing = "colocated-pairing"
	// MaxEnrollmentGrants bounds unspent grants.
	MaxEnrollmentGrants = 256
	// enrollmentRecoveryTTL is how long a lost enrollment response can be
	// replayed with the saved request id and recovery secret.
	enrollmentRecoveryTTL = 24 * time.Hour
	bearerBytes           = 32

	recoverySaltBytes  = 16
	recoveryNonceBytes = 12
	recoveryInfo       = "rigscope collector credential recovery v1"
)

// credentialSentinel is the non-secret value kept in the legacy UNIQUE token
// column. Authentication reads token_hash only; the sentinel exists because the
// 1.0 baseline column is NOT NULL and cannot be dropped additively.
func credentialSentinel(hash string) string { return hash }

var recoverySecretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)

// Identity is one issued collector credential and its hub-local machine
// mapping. ReplacedCollectorID is set only by a successful replacement
// enrollment and names the collector whose credential the new one superseded;
// ReplacedTokenHash is that credential's hash, so the gateway can fence exactly
// the old sockets without touching a newer credential that reused the id.
type Identity struct {
	CollectorID         string
	MachineID           string
	Token               string
	TokenHash           string
	ReplacedCollectorID string
	ReplacedTokenHash   string
}

// Collector is the public registry view; it never contains the credential.
type Collector struct {
	CollectorID string
	MachineID   string
	Revoked     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// EnrollmentGrant is one short-lived, single-use bootstrap grant.
type EnrollmentGrant struct {
	Token     string
	ExpiresAt time.Time
}

// BootstrapGrant is one hashed single-use grant imported from the hub-owned
// bootstrap file. The plaintext token is never written to that file: the
// installer keeps it only in the collector's private enrollment file.
type BootstrapGrant struct {
	TokenHash string
	ExpiresAt time.Time
	// Use names the operation that created the grant, for example
	// BootstrapGrantUseColocatedPairing.
	Use string
	// CollectorID is the installation identity an automatic colocated
	// pairing grant is bound to. It is empty for every other use.
	CollectorID string
}

// NewEnrollmentGrantToken returns a fresh single-use grant token and the
// SHA-256 hash the registry stores.
func NewEnrollmentGrantToken() (token, tokenHash string, err error) {
	token, err = randomToken()
	if err != nil {
		return "", "", err
	}
	return token, hashToken(token), nil
}

// GrantOptions configures CreateGrant. ReplaceMachineID names an existing
// machine whose collector is being reinstalled; an ordinary grant leaves it
// empty and always creates a fresh hub-local machine.
type GrantOptions struct {
	ReplaceMachineID string
	TTL              time.Duration
}

// EnrollRequest is one claim of a bootstrap grant. RequestID and
// RecoverySecret are optional together and make a lost enrollment response
// recoverable exactly once.
type EnrollRequest struct {
	CollectorID    string
	Token          string
	RequestID      string
	RecoverySecret string
	// LocalPeer reports that the redemption request arrived on a socket the
	// hub can attribute to its own machine: a loopback address, or a peer
	// whose address is one of the host's own addresses. Forwarded headers are
	// never used: a proxy cannot make a remote peer look local.
	LocalPeer bool
}

// RegistryOptions bootstraps the registry from static configuration. Bootstrap
// state is imported exactly once; a later start never resurrects a revoked or
// deleted collector.
type RegistryOptions struct {
	BootstrapIdentities []Identity
	Grants              []EnrollmentGrant
	// BootstrapGrants are hashed grants that cannot be read from the database
	// before it opens, such as the installer's automatic colocated pairing
	// grant. Expired entries are ignored; unexpired entries are imported at
	// most once per hash.
	BootstrapGrants     []BootstrapGrant
	ConsumedGrantHashes []string
}

// Registry is the authoritative collector enrollment store.
type Registry struct {
	db  *Database
	now func() time.Time
}

const bootstrapMarkerKey = "bootstrapped"

// NewRegistry opens the enrollment registry over db and imports bootstrap
// configuration at most once.
func NewRegistry(ctx context.Context, db *Database, opts RegistryOptions) (*Registry, error) {
	if len(opts.BootstrapIdentities) > 4096 {
		return nil, invalidError("bootstrap collector directory exceeds its bound")
	}
	if len(opts.Grants) > MaxEnrollmentGrants {
		return nil, invalidError("too many bootstrap enrollment grants")
	}
	registry := &Registry{db: db, now: db.Now}
	for _, identity := range opts.BootstrapIdentities {
		if err := validateCollectorID(identity.CollectorID); err != nil {
			return nil, err
		}
		if !identifierPattern.MatchString(identity.MachineID) {
			return nil, invalidError("bootstrap machine id is invalid")
		}
		if identity.Token == "" || len(identity.Token) > 256 {
			return nil, invalidError("bootstrap credential token is invalid")
		}
	}
	for _, grant := range opts.Grants {
		if grant.Token == "" || len(grant.Token) > 256 {
			return nil, invalidError("bootstrap enrollment grant token is invalid")
		}
		if grant.ExpiresAt.IsZero() {
			return nil, invalidError("bootstrap enrollment grant expiry is required")
		}
		if grant.ExpiresAt.Sub(registry.now()) > MaxEnrollmentGrantTTL {
			return nil, invalidError("enrollment grants may live at most fifteen minutes")
		}
	}
	for _, grant := range opts.BootstrapGrants {
		if err := validateBootstrapGrant(grant, registry.now()); err != nil {
			return nil, err
		}
	}
	for _, hash := range opts.ConsumedGrantHashes {
		if !isSHA256Hex(hash) {
			return nil, invalidError("legacy consumed grant hash is invalid")
		}
	}

	now := registry.now()
	err := db.Write(ctx, func(tx *Tx) error {
		marker, err := txQueryRowExists(tx, "SELECT 1 FROM collector_registry_meta WHERE key = ?", bootstrapMarkerKey)
		if err != nil {
			return err
		}
		if !marker {
			for _, identity := range opts.BootstrapIdentities {
				hash := hashToken(identity.Token)
				if _, err := tx.Exec(
					"INSERT INTO collector_registry (collector_id, machine_id, token, token_hash, revoked, created_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, 0, ?, ?)",
					identity.CollectorID, identity.MachineID, credentialSentinel(hash), hash, now.UnixMilli(), now.UnixMilli(),
				); err != nil {
					return fmt.Errorf("import bootstrap collector identity: %w", err)
				}
			}
			for _, hash := range opts.ConsumedGrantHashes {
				if _, err := tx.Exec(
					"INSERT INTO collector_grants (token_hash, expires_at_ms, consumed_by, actor, created_at_ms) VALUES (?, 0, 'legacy-consumed', 'migration', ?) "+
						"ON CONFLICT(token_hash) DO UPDATE SET consumed_by = COALESCE(consumed_by, 'legacy-consumed')",
					hash, now.UnixMilli(),
				); err != nil {
					return err
				}
			}
			// An unconfigured first start must stay importable on the next
			// start. Once anything was imported, the marker prevents
			// resurrection.
			if len(opts.BootstrapIdentities) > 0 || len(opts.Grants) > 0 || len(opts.ConsumedGrantHashes) > 0 {
				if _, err := tx.Exec(
					"INSERT INTO collector_registry_meta (key, value) VALUES (?, '1') ON CONFLICT(key) DO UPDATE SET value = excluded.value",
					bootstrapMarkerKey,
				); err != nil {
					return err
				}
			}
		}
		for _, grant := range opts.Grants {
			if _, err := tx.Exec(
				"INSERT OR IGNORE INTO collector_grants (token_hash, expires_at_ms, actor, created_at_ms) VALUES (?, ?, 'bootstrap', ?)",
				hashToken(grant.Token), grant.ExpiresAt.UnixMilli(), now.UnixMilli(),
			); err != nil {
				return err
			}
		}
		for _, grant := range opts.BootstrapGrants {
			// An expired bootstrap grant is ignored: the file is deleted by
			// its importer either way, and the collector must ask for a fresh
			// one through the normal enrollment endpoint.
			if !grant.ExpiresAt.After(now) {
				continue
			}
			if err := insertBootstrapGrantTx(tx, grant, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return registry, nil
}

func validateCollectorID(collectorID string) error {
	if !identifierPattern.MatchString(collectorID) {
		return invalidError("collector id must be 1..128 printable ASCII characters")
	}
	return nil
}

// validateBootstrapGrant checks one hashed grant from the hub bootstrap file.
// Expired grants are valid input and skipped at import, so an old file cannot
// stop the hub from starting. An automatic colocated pairing grant must name
// the collector installation it is bound to; an administrator-issued grant
// keeps its old shape.
func validateBootstrapGrant(grant BootstrapGrant, now time.Time) error {
	if !isSHA256Hex(grant.TokenHash) {
		return invalidError("bootstrap enrollment grant hash is invalid")
	}
	if grant.ExpiresAt.IsZero() {
		return invalidError("bootstrap enrollment grant expiry is required")
	}
	if grant.ExpiresAt.Sub(now) > MaxBootstrapGrantTTL {
		return invalidError("bootstrap enrollment grants may live at most one hour")
	}
	if !identifierPattern.MatchString(grant.Use) {
		return invalidError("bootstrap enrollment grant use is invalid")
	}
	if grant.Use == BootstrapGrantUseColocatedPairing {
		if err := validateCollectorID(grant.CollectorID); err != nil {
			return invalidError("bootstrap pairing grant must name the intended collector id")
		}
	}
	return nil
}

// insertBootstrapGrantTx records one unexpired hashed grant. INSERT OR IGNORE
// makes both the startup import and a lazy import idempotent: a hash is
// imported at most once, even when a crash left the bootstrap file behind.
func insertBootstrapGrantTx(tx *Tx, grant BootstrapGrant, now time.Time) error {
	_, err := tx.Exec(
		"INSERT OR IGNORE INTO collector_grants (token_hash, expires_at_ms, actor, purpose, collector_id, created_at_ms) VALUES (?, ?, ?, ?, ?, ?)",
		grant.TokenHash, grant.ExpiresAt.UnixMilli(), grant.Use, grant.Use, nullIfEmpty(grant.CollectorID), now.UnixMilli(),
	)
	return err
}

// ImportBootstrapGrants adds hashed grants that appeared after the registry was
// opened. The hub calls it when an enrollment request arrives, so adding a
// collector to an already-running hub needs no restart. Import is idempotent
// per token hash, exactly like the startup import.
func (r *Registry) ImportBootstrapGrants(ctx context.Context, grants []BootstrapGrant) error {
	now := r.now()
	for _, grant := range grants {
		if err := validateBootstrapGrant(grant, now); err != nil {
			return err
		}
	}
	return r.db.Write(ctx, func(tx *Tx) error {
		for _, grant := range grants {
			if !grant.ExpiresAt.After(now) {
				continue
			}
			if err := insertBootstrapGrantTx(tx, grant, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// CreateGrant issues one single-use enrollment grant. The plaintext token is
// returned once and stored only as a SHA-256 hash.
func (r *Registry) CreateGrant(ctx context.Context, actor string, opts GrantOptions) (EnrollmentGrant, error) {
	var grant EnrollmentGrant
	err := r.db.Write(ctx, func(tx *Tx) error {
		var err error
		grant, err = r.CreateGrantTx(tx, actor, opts)
		return err
	})
	if err != nil {
		return EnrollmentGrant{}, err
	}
	return grant, nil
}

// CreateGrantTx issues one grant inside tx. The caller authorizes the acting
// administrator; the admin handler wraps it in Users.Authorize so the grant
// cannot commit after that administrator was demoted.
func (r *Registry) CreateGrantTx(tx *Tx, actor string, opts GrantOptions) (EnrollmentGrant, error) {
	ttl := opts.TTL
	if ttl == 0 {
		ttl = MaxEnrollmentGrantTTL
	}
	if ttl < time.Minute || ttl > MaxEnrollmentGrantTTL {
		return EnrollmentGrant{}, invalidError("grant lifetime must be between one and fifteen minutes")
	}
	if opts.ReplaceMachineID != "" && !identifierPattern.MatchString(opts.ReplaceMachineID) {
		return EnrollmentGrant{}, invalidError("replacement machine id is invalid")
	}
	token, err := randomToken()
	if err != nil {
		return EnrollmentGrant{}, wrapStorageError(CodeUnavailable, err, "generate enrollment grant")
	}
	now := r.now()
	expires := now.Add(ttl)
	if opts.ReplaceMachineID != "" {
		exists, err := txQueryRowExists(tx, "SELECT 1 FROM collector_registry WHERE machine_id = ?", opts.ReplaceMachineID)
		if err != nil {
			return EnrollmentGrant{}, err
		}
		if !exists {
			return EnrollmentGrant{}, invalidError("replacement machine does not exist")
		}
	}
	if _, err := tx.Exec(
		"UPDATE collector_registry SET credential_sealed = NULL, credential_nonce = NULL "+
			"WHERE collector_id IN (SELECT consumed_by FROM collector_grants "+
			"WHERE recovery_until_ms IS NOT NULL AND recovery_until_ms < ? AND consumed_by IS NOT NULL)",
		now.UnixMilli(),
	); err != nil {
		return EnrollmentGrant{}, err
	}
	if _, err := tx.Exec(
		"DELETE FROM collector_grants WHERE expires_at_ms < ? AND (recovery_until_ms IS NULL OR recovery_until_ms < ?)",
		now.UnixMilli(), now.UnixMilli(),
	); err != nil {
		return EnrollmentGrant{}, err
	}
	var count int64
	if err := tx.QueryRow("SELECT COUNT(*) FROM collector_grants").Scan(&count); err != nil {
		return EnrollmentGrant{}, mapError(err)
	}
	if count >= MaxEnrollmentGrants {
		return EnrollmentGrant{}, conflictError("enrollment grant budget exhausted")
	}
	if _, err := tx.Exec(
		"INSERT INTO collector_grants (token_hash, expires_at_ms, replace_machine_id, actor, created_at_ms) VALUES (?, ?, ?, ?, ?)",
		hashToken(token), expires.UnixMilli(), nullIfEmpty(opts.ReplaceMachineID), actor, now.UnixMilli(),
	); err != nil {
		return EnrollmentGrant{}, err
	}
	action := "grant-create"
	target := "new-installation"
	if opts.ReplaceMachineID != "" {
		action = "grant-replacement"
		target = opts.ReplaceMachineID
	}
	if err := appendAuditTx(tx, AuditScopeCollector, actor, action, target, now); err != nil {
		return EnrollmentGrant{}, err
	}
	return EnrollmentGrant{Token: token, ExpiresAt: expires.UTC()}, nil
}

// Enroll claims a grant and returns the issued identity. A retry with the same
// grant, request id and recovery secret returns the same credential without
// issuing a second one; any other replay is denied.
func (r *Registry) Enroll(ctx context.Context, req EnrollRequest) (Identity, error) {
	if err := validateCollectorID(req.CollectorID); err != nil {
		return Identity{}, err
	}
	if req.Token == "" || len(req.Token) > 256 {
		return Identity{}, invalidError("enrollment token is invalid")
	}
	recoverable := req.RequestID != "" || req.RecoverySecret != ""
	if recoverable {
		if !identifierPattern.MatchString(req.RequestID) || !recoverySecretPattern.MatchString(req.RecoverySecret) {
			return Identity{}, invalidError("enrollment recovery proof is invalid")
		}
	}

	var issued Identity
	now := r.now()
	err := r.db.Write(ctx, func(tx *Tx) error {
		var (
			expiresAtMS    int64
			replaceMachine sql.NullString
			consumedBy     sql.NullString
			requestID      sql.NullString
			recoveryHash   sql.NullString
			recoveryUntil  sql.NullInt64
			purpose        sql.NullString
			boundCollector sql.NullString
			actor          string
		)
		err := tx.QueryRow(
			"SELECT expires_at_ms, replace_machine_id, consumed_by, request_id, recovery_hash, recovery_until_ms, purpose, collector_id, actor FROM collector_grants WHERE token_hash = ?",
			hashToken(req.Token),
		).Scan(&expiresAtMS, &replaceMachine, &consumedBy, &requestID, &recoveryHash, &recoveryUntil, &purpose, &boundCollector, &actor)
		if errors.Is(err, sql.ErrNoRows) {
			return enrollmentDenied()
		}
		if err != nil {
			return mapError(err)
		}
		// An automatic colocated pairing grant is only redeemable over a socket
		// from the hub's own machine and only by the collector installation the
		// installer named. This applies to the first redemption and to a
		// recovery retry alike: another machine, or a proxy forwarding a local
		// request, must not spend it.
		colocated, binding := grantBinding(purpose, boundCollector, actor)
		if colocated {
			if binding == "" {
				// A grant imported by a binary that predates the binding
				// columns (a rollback to schema 6) records the colocated actor
				// but no purpose and no collector id. It cannot be attributed
				// to an installation, so it is never redeemable; reinstalling
				// stages a fresh, bound grant.
				return enrollmentDenied()
			}
			if !req.LocalPeer {
				return enrollmentDenied()
			}
			if binding != req.CollectorID {
				return enrollmentDenied()
			}
		}
		if consumedBy.Valid {
			// The grant was validly spent, so its expiry no longer gates
			// anything: the recorded recovery window and the proof are the
			// authority until that window closes.
			if !recoverable || consumedBy.String != req.CollectorID ||
				requestID.String != req.RequestID || !recoveryHash.Valid ||
				!secureEqual(hashToken(req.RecoverySecret), recoveryHash.String) ||
				!recoveryUntil.Valid || recoveryUntil.Int64 <= now.UnixMilli() {
				return enrollmentDenied()
			}
			identity, err := recoverIdentity(tx, req)
			if err != nil {
				return err
			}
			issued = identity
			return nil
		}
		// An unspent grant's expiry is checked before it can take part in any
		// recovery: an unrelated expired grant must never act as a key to
		// another collector's credential.
		if expiresAtMS <= now.UnixMilli() {
			return enrollmentDenied()
		}
		// A reinstall rotates the installer's colocated grant but preserves the
		// recovery proof of a pending enrollment whose request may already have
		// consumed the previous grant. Recover the credential bound to that
		// proof instead of reporting the collector as already registered. Only
		// the installer's own colocated grant may do this, and only for the
		// installation it is bound to: an administrator-issued grant is never a
		// recovery vehicle.
		if recoverable && colocated && !replaceMachine.Valid {
			identity, recovered, err := recoverByProof(tx, req, now)
			if err != nil {
				return err
			}
			if recovered {
				// Only the presented row is consumed and given a window; no
				// other grant's recovery state is touched.
				if _, err := tx.Exec(
					"UPDATE collector_grants SET consumed_by = ?, request_id = ?, recovery_hash = ?, recovery_until_ms = ? WHERE token_hash = ?",
					req.CollectorID, req.RequestID, hashToken(req.RecoverySecret), now.Add(enrollmentRecoveryTTL).UnixMilli(), hashToken(req.Token),
				); err != nil {
					return err
				}
				issued = identity
				return nil
			}
		}
		existingMachine, existingCollector, err := collectorForID(tx, req.CollectorID)
		if err != nil {
			return err
		}
		if existingCollector != "" && (!replaceMachine.Valid || existingMachine != replaceMachine.String) {
			return conflictError("collector already has an identity; an explicit replacement grant is required")
		}

		machineID := replaceMachine.String
		replacedCollector := ""
		replacedTokenHash := ""
		if replaceMachine.Valid {
			previousCollector, previousHash, err := collectorForMachine(tx, replaceMachine.String)
			if err != nil {
				return err
			}
			if previousCollector == "" {
				return &Error{Code: CodeUnavailable, Message: "replacement mapping disappeared"}
			}
			replacedCollector = previousCollector
			replacedTokenHash = previousHash
			if _, err := tx.Exec("UPDATE collector_grants SET recovery_hash = NULL WHERE consumed_by = ?", previousCollector); err != nil {
				return err
			}
			if _, err := tx.Exec("DELETE FROM collector_registry WHERE machine_id = ?", replaceMachine.String); err != nil {
				return err
			}
		} else {
			machineID, err = uniqueMachineID(tx)
			if err != nil {
				return err
			}
		}

		token, err := randomToken()
		if err != nil {
			return wrapStorageError(CodeUnavailable, err, "issue collector credential")
		}
		hash := hashToken(token)
		if _, err := tx.Exec(
			"INSERT INTO collector_registry (collector_id, machine_id, token, token_hash, revoked, created_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, 0, ?, ?)",
			req.CollectorID, machineID, credentialSentinel(hash), hash, now.UnixMilli(), now.UnixMilli(),
		); err != nil {
			return err
		}
		var recoveryUntilMS any
		var recoveryProof any
		if recoverable {
			sealed, nonce, err := sealCredential(req.RecoverySecret, token)
			if err != nil {
				return wrapStorageError(CodeUnavailable, err, "seal collector credential recovery copy")
			}
			if _, err := tx.Exec(
				"UPDATE collector_registry SET credential_sealed = ?, credential_nonce = ? WHERE collector_id = ?",
				sealed, nonce, req.CollectorID,
			); err != nil {
				return err
			}
			recoveryProof = hashToken(req.RecoverySecret)
			recoveryUntilMS = now.Add(enrollmentRecoveryTTL).UnixMilli()
		}
		if _, err := tx.Exec(
			"UPDATE collector_grants SET consumed_by = ?, request_id = ?, recovery_hash = ?, recovery_until_ms = ? WHERE token_hash = ?",
			req.CollectorID, nullIfEmpty(req.RequestID), recoveryProof, recoveryUntilMS, hashToken(req.Token),
		); err != nil {
			return err
		}
		action := "enroll"
		if replaceMachine.Valid {
			action = "enroll-replacement"
		}
		if err := appendAuditTx(tx, AuditScopeCollector, "collector", action, req.CollectorID, now); err != nil {
			return err
		}
		issued = Identity{
			CollectorID:         req.CollectorID,
			MachineID:           machineID,
			Token:               token,
			TokenHash:           hash,
			ReplacedCollectorID: replacedCollector,
			ReplacedTokenHash:   replacedTokenHash,
		}
		return nil
	})
	if err != nil {
		return Identity{}, err
	}
	return issued, nil
}

// Revoke marks one collector credential unusable and invalidates any pending
// recovery proof. The machine mapping remains for history and metadata.
func (r *Registry) Revoke(ctx context.Context, actor, collectorID string) error {
	return r.db.Write(ctx, func(tx *Tx) error {
		return r.RevokeTx(tx, actor, collectorID)
	})
}

// RevokeTx marks one collector credential unusable inside tx. The caller
// authorizes the acting administrator.
func (r *Registry) RevokeTx(tx *Tx, actor, collectorID string) error {
	if err := validateCollectorID(collectorID); err != nil {
		return err
	}
	now := r.now()
	exists, err := txQueryRowExists(tx, "SELECT 1 FROM collector_registry WHERE collector_id = ?", collectorID)
	if err != nil {
		return err
	}
	if !exists {
		return invalidError("collector does not exist")
	}
	if _, err := tx.Exec(
		"UPDATE collector_registry SET revoked = 1, credential_sealed = NULL, credential_nonce = NULL, updated_at_ms = ? WHERE collector_id = ?",
		now.UnixMilli(), collectorID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE collector_grants SET recovery_hash = NULL WHERE consumed_by = ?", collectorID); err != nil {
		return err
	}
	return appendAuditTx(tx, AuditScopeCollector, actor, "revoke", collectorID, now)
}

// Identities returns every active (non-revoked) collector mapping. Credential
// material is never returned: authentication compares a presented token's hash
// inside the registry.
func (r *Registry) Identities(ctx context.Context) ([]Identity, error) {
	rows, err := r.db.Query(ctx,
		"SELECT collector_id, machine_id FROM collector_registry WHERE revoked = 0 ORDER BY machine_id",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var identities []Identity
	for rows.Next() {
		var identity Identity
		if err := rows.Scan(&identity.CollectorID, &identity.MachineID); err != nil {
			return nil, mapError(err)
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return identities, nil
}

// Authenticate resolves a presented bearer token to an active identity by
// comparing hashes in constant time. The first successful connection also
// deletes the encrypted lost-response copy, which is no longer needed.
func (r *Registry) Authenticate(ctx context.Context, token string) (Identity, bool, error) {
	if token == "" || len(token) > 256 {
		return Identity{}, false, nil
	}
	presented := hashToken(token)
	var (
		identity Identity
		stored   string
		sealed   []byte
	)
	err := r.db.QueryRow(ctx,
		"SELECT collector_id, machine_id, token_hash, credential_sealed FROM collector_registry WHERE token_hash = ? AND revoked = 0",
		presented,
	).Scan(&identity.CollectorID, &identity.MachineID, &stored, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, mapError(err)
	}
	if !secureEqual(presented, stored) {
		return Identity{}, false, nil
	}
	identity.Token = token
	identity.TokenHash = stored
	if len(sealed) > 0 {
		// Best effort: authentication already succeeded, and a later connection
		// retries the same idempotent cleanup.
		_ = r.db.Write(ctx, func(tx *Tx) error {
			_, err := tx.Exec(
				"UPDATE collector_registry SET credential_sealed = NULL, credential_nonce = NULL WHERE collector_id = ?",
				identity.CollectorID,
			)
			return err
		})
	}
	return identity, true, nil
}

// List returns every mapping including revoked ones, without credentials.
func (r *Registry) List(ctx context.Context) ([]Collector, error) {
	rows, err := r.db.Query(ctx,
		"SELECT collector_id, machine_id, revoked, created_at_ms, updated_at_ms FROM collector_registry ORDER BY machine_id",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var collectors []Collector
	for rows.Next() {
		var (
			collector                Collector
			revoked                  int64
			createdAtMS, updatedAtMS int64
		)
		if err := rows.Scan(&collector.CollectorID, &collector.MachineID, &revoked, &createdAtMS, &updatedAtMS); err != nil {
			return nil, mapError(err)
		}
		collector.Revoked = intToBool(revoked)
		collector.CreatedAt = msToTime(createdAtMS)
		collector.UpdatedAt = msToTime(updatedAtMS)
		collectors = append(collectors, collector)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return collectors, nil
}

// AuditEvents returns the newest collector-scope audit rows.
func (r *Registry) AuditEvents(ctx context.Context) ([]AuditEntry, error) {
	return NewAudit(r.db).List(ctx, AuditScopeCollector, 0)
}

func enrollmentDenied() error {
	// One message for unknown, expired and replayed grants: callers get no
	// oracle that distinguishes a guessed token from a spent one.
	return &Error{Code: CodeForbidden, Message: "enrollment denied"}
}

// recoverIdentity decrypts the bounded credential copy with the collector's
// high-entropy recovery secret. The copy is deleted after the first successful
// authenticated connection or when the recovery window closes, so a spent or
// expired copy is indistinguishable from an unknown grant.
func recoverIdentity(tx *Tx, req EnrollRequest) (Identity, error) {
	var (
		identity Identity
		sealed   []byte
		nonce    []byte
	)
	err := tx.QueryRow(
		"SELECT collector_id, machine_id, credential_sealed, credential_nonce FROM collector_registry WHERE collector_id = ? AND revoked = 0",
		req.CollectorID,
	).Scan(&identity.CollectorID, &identity.MachineID, &sealed, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, enrollmentDenied()
	}
	if err != nil {
		return Identity{}, mapError(err)
	}
	if len(sealed) == 0 || len(nonce) == 0 {
		return Identity{}, enrollmentDenied()
	}
	token, err := openCredential(req.RecoverySecret, sealed, nonce)
	if err != nil {
		return Identity{}, enrollmentDenied()
	}
	identity.Token = token
	identity.TokenHash = hashToken(token)
	return identity, nil
}

// grantBinding reports whether a stored grant is the installer's colocated
// pairing grant and, when it is, the collector installation it is bound to. A
// grant imported by a binary that predates the binding columns carries the
// colocated actor but no purpose: it is reported as colocated with an empty
// binding so the caller refuses it instead of treating it as an ordinary
// administrator-issued grant.
func grantBinding(purpose, collectorID sql.NullString, actor string) (colocated bool, binding string) {
	if purpose.Valid && purpose.String == BootstrapGrantUseColocatedPairing {
		if collectorID.Valid {
			return true, collectorID.String
		}
		return true, ""
	}
	if !purpose.Valid && actor == BootstrapGrantUseColocatedPairing {
		return true, ""
	}
	return false, ""
}

// recoverByProof finds a consumed colocated pairing grant whose recovery window
// is still open, whose purpose and collector binding match the presented grant,
// and whose request id and recovery secret match this request. A reinstall
// issues a fresh grant but keeps the pending proof, so the collector can still
// recover the credential a lost response already issued. The grant row itself
// is not needed: the sealed credential is stored on the collector row and the
// proof hash on the consumed grant.
func recoverByProof(tx *Tx, req EnrollRequest, now time.Time) (Identity, bool, error) {
	presented := hashToken(req.RecoverySecret)
	rows, err := tx.Query(
		"SELECT recovery_hash FROM collector_grants WHERE consumed_by = ? AND request_id = ? AND recovery_hash IS NOT NULL "+
			"AND recovery_until_ms > ? AND purpose = ? AND collector_id = ?",
		req.CollectorID, req.RequestID, now.UnixMilli(), BootstrapGrantUseColocatedPairing, req.CollectorID,
	)
	if err != nil {
		return Identity{}, false, mapError(err)
	}
	matched := false
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			rows.Close()
			return Identity{}, false, mapError(err)
		}
		if secureEqual(presented, stored) {
			matched = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Identity{}, false, mapError(err)
	}
	rows.Close()
	if !matched {
		return Identity{}, false, nil
	}
	identity, err := recoverIdentity(tx, req)
	if err != nil {
		// A failed decryption means the proof does not actually own the sealed
		// copy; treat it like an unknown grant rather than leaking why.
		if CodeOf(err) == CodeForbidden {
			return Identity{}, false, nil
		}
		return Identity{}, false, err
	}
	return identity, true, nil
}

func collectorForID(tx *Tx, collectorID string) (machineID, collector string, err error) {
	var machine, found string
	err = tx.QueryRow("SELECT machine_id, collector_id FROM collector_registry WHERE collector_id = ?", collectorID).Scan(&machine, &found)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", mapError(err)
	}
	return machine, found, nil
}

func collectorForMachine(tx *Tx, machineID string) (collector, tokenHash string, err error) {
	var found string
	var hash sql.NullString
	err = tx.QueryRow("SELECT collector_id, token_hash FROM collector_registry WHERE machine_id = ?", machineID).
		Scan(&found, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", mapError(err)
	}
	return found, hash.String, nil
}

func uniqueMachineID(tx *Tx) (string, error) {
	for attempt := 0; attempt < 4; attempt++ {
		candidate, err := newUUID()
		if err != nil {
			return "", wrapStorageError(CodeUnavailable, err, "generate machine id")
		}
		exists, err := txQueryRowExists(tx, "SELECT 1 FROM collector_registry WHERE machine_id = ?", candidate)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", &Error{Code: CodeUnavailable, Message: "generated machine id collided with an existing mapping"}
}

func randomToken() (string, error) {
	raw := make([]byte, bearerBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func newUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// hashLegacyCollectorCredentials is the migration data step for schema 5. SQL
// cannot compute SHA-256, so plaintext credentials from a schema-4 database are
// hashed inside the same transaction that adds the hash column. The legacy
// token column then holds the hash (never the credential) to satisfy its NOT
// NULL and UNIQUE constraints.
func hashLegacyCollectorCredentials(tx *Tx) error {
	type legacyCredential struct {
		collectorID string
		token       string
	}
	rows, err := tx.Query("SELECT collector_id, token FROM collector_registry")
	if err != nil {
		return err
	}
	var pending []legacyCredential
	for rows.Next() {
		var item legacyCredential
		if err := rows.Scan(&item.collectorID, &item.token); err != nil {
			rows.Close()
			return mapError(err)
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return mapError(err)
	}
	rows.Close()
	for _, item := range pending {
		hash := hashToken(item.token)
		if _, err := tx.Exec(
			"UPDATE collector_registry SET token_hash = ?, token = ? WHERE collector_id = ?",
			hash, credentialSentinel(hash), item.collectorID,
		); err != nil {
			return err
		}
	}
	return nil
}

// sealCredential encrypts the issued credential with a key derived from the
// collector's high-entropy recovery secret (HKDF-SHA256) using AES-GCM. The
// stored material is the public salt followed by the nonce.
func sealCredential(secret, credential string) (sealed, material []byte, err error) {
	salt := make([]byte, recoverySaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, err
	}
	key, err := hkdf.Key(sha256.New, []byte(secret), salt, recoveryInfo, 32)
	if err != nil {
		return nil, nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	sealed = gcm.Seal(nil, nonce, []byte(credential), nil)
	material = make([]byte, 0, len(salt)+len(nonce))
	material = append(material, salt...)
	material = append(material, nonce...)
	return sealed, material, nil
}

func openCredential(secret string, sealed, material []byte) (string, error) {
	if len(sealed) == 0 || len(material) != recoverySaltBytes+recoveryNonceBytes {
		return "", errors.New("invalid credential recovery material")
	}
	salt := material[:recoverySaltBytes]
	nonce := material[recoverySaltBytes:]
	key, err := hkdf.Key(sha256.New, []byte(secret), salt, recoveryInfo, 32)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func isSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func secureEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
