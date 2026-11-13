package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

const (
	// MaxUsers is the browser account directory bound.
	MaxUsers = 50
	// MaxScopeBytes bounds a serialized machine scope, not a machine count.
	MaxScopeBytes = 16 * 1024
	// MaxSessions bounds the durable browser session table.
	MaxSessions = 256
)

var identifierPattern = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)

// Scope is a browser user's machine visibility: either every machine or an
// explicit, bounded list of machine IDs.
type Scope struct {
	All        bool
	MachineIDs []string
}

// AllMachines returns a wildcard scope.
func AllMachines() Scope { return Scope{All: true} }

// MachineScope returns an explicit scope. The slice is copied.
func MachineScope(ids ...string) Scope {
	return Scope{MachineIDs: append([]string(nil), ids...)}
}

// Contains reports whether the scope covers machineID.
func (s Scope) Contains(machineID string) bool {
	if s.All {
		return true
	}
	for _, id := range s.MachineIDs {
		if id == machineID {
			return true
		}
	}
	return false
}

// Validate checks the scope's identifiers and serialized size bound.
func (s Scope) Validate() error {
	if s.All {
		if len(s.MachineIDs) != 0 {
			return invalidError("a wildcard scope must not list machine ids")
		}
		return nil
	}
	seen := make(map[string]struct{}, len(s.MachineIDs))
	bytes := 2
	for _, id := range s.MachineIDs {
		if !identifierPattern.MatchString(id) {
			return invalidError("machine scope contains an invalid identifier")
		}
		bytes += len(id) + 3
		if bytes > MaxScopeBytes {
			return invalidError("machine scope exceeds its size budget")
		}
		if _, duplicate := seen[id]; duplicate {
			return invalidError("machine scope contains a duplicate identifier")
		}
		seen[id] = struct{}{}
	}
	return nil
}

func encodeScope(scope Scope) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if scope.All {
		return "*", nil
	}
	ids := scope.MachineIDs
	if ids == nil {
		ids = []string{}
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return "", wrapStorageError(CodeInvalid, err, "encode machine scope")
	}
	return string(raw), nil
}

func parseScope(encoded string) (Scope, error) {
	if encoded == "*" {
		return Scope{All: true}, nil
	}
	if len(encoded) > MaxScopeBytes {
		return Scope{}, invalidError("stored machine scope exceeds its size budget")
	}
	var ids []string
	if err := json.Unmarshal([]byte(encoded), &ids); err != nil {
		return Scope{}, wrapStorageError(CodeUnavailable, err, "parse stored machine scope")
	}
	scope := Scope{MachineIDs: ids}
	if err := scope.Validate(); err != nil {
		return Scope{}, err
	}
	return scope, nil
}

func validateUsername(value string) error {
	if !identifierPattern.MatchString(value) {
		return invalidError("username must be 1..128 printable ASCII characters without spaces")
	}
	return nil
}

func validateOpaqueHash(value string) error {
	if value == "" || len(value) > 4096 || strings.ContainsRune(value, 0) {
		return invalidError("password hash must be a nonempty opaque string of at most 4096 bytes")
	}
	return nil
}

func msToTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func timeToMS(value time.Time) int64 { return value.UnixMilli() }

func scanNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return mapError(err)
}

func ensureAffected(result sql.Result, code ErrorCode, format string, args ...any) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return mapError(err)
	}
	if affected == 0 {
		return storageError(code, format, args...)
	}
	return nil
}

// Users is the durable browser account directory. Password hashing is the
// auth package's job: storage treats the hash as an opaque string.
type Users struct {
	db  *Database
	now func() time.Time
}

// NewUsers returns the account repository over db.
func NewUsers(db *Database) *Users {
	return &Users{db: db, now: db.Now}
}

// User is one browser account.
type User struct {
	Username     string
	PasswordHash string
	Scope        Scope
	Admin        bool
	Disabled     bool
	// Incarnation is a random 128-bit identifier created with the row. It never
	// changes for that account and is never reused, so deleting a user and
	// recreating the same username produces a new incarnation. Login compares
	// both it and Version inside the session-creation transaction; session
	// lookup rejects a row whose recorded incarnation or version is stale.
	Incarnation string
	// Version is bumped by every account change. Login compares it inside the
	// session-creation transaction and session lookup rejects a session whose
	// recorded version is stale.
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

const userColumns = "username, password_hash, scope_json, is_admin, disabled, incarnation, version, created_at_ms, updated_at_ms"

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(row rowScanner) (User, error) {
	var (
		user                     User
		scopeJSON                string
		admin, disabled          int64
		version                  int64
		createdAtMS, updatedAtMS int64
	)
	if err := row.Scan(&user.Username, &user.PasswordHash, &scopeJSON, &admin, &disabled, &user.Incarnation,
		&version, &createdAtMS, &updatedAtMS); err != nil {
		return User{}, err
	}
	scope, err := parseScope(scopeJSON)
	if err != nil {
		return User{}, err
	}
	user.Scope = scope
	user.Admin = intToBool(admin)
	user.Disabled = intToBool(disabled)
	user.Version = version
	user.CreatedAt = msToTime(createdAtMS)
	user.UpdatedAt = msToTime(updatedAtMS)
	return user, nil
}

// Get returns one user including its opaque password hash.
func (s *Users) Get(ctx context.Context, username string) (User, error) {
	if err := validateUsername(username); err != nil {
		return User{}, err
	}
	user, err := scanUser(s.db.QueryRow(ctx, "SELECT "+userColumns+" FROM users WHERE username = ?", username))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, notFoundError("user %q not found", username)
	}
	if err != nil {
		return User{}, mapError(err)
	}
	return user, nil
}

// List returns every account, including disabled ones, ordered by username.
func (s *Users) List(ctx context.Context) ([]User, error) {
	return s.query(ctx, "SELECT "+userColumns+" FROM users ORDER BY username")
}

// Active returns enabled accounts ordered by username.
func (s *Users) Active(ctx context.Context) ([]User, error) {
	return s.query(ctx, "SELECT "+userColumns+" FROM users WHERE disabled = 0 ORDER BY username")
}

func (s *Users) query(ctx context.Context, query string, args ...any) ([]User, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, mapError(err)
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return users, nil
}

// IsAdmin reports whether username is an existing, enabled administrator.
func (s *Users) IsAdmin(ctx context.Context, username string) (bool, error) {
	if !identifierPattern.MatchString(username) {
		return false, nil
	}
	var admin, disabled int64
	err := s.db.QueryRow(ctx, "SELECT is_admin, disabled FROM users WHERE username = ?", username).Scan(&admin, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapError(err)
	}
	return intToBool(admin) && !intToBool(disabled), nil
}

func validateUser(user User) error {
	if err := validateUsername(user.Username); err != nil {
		return err
	}
	// Grants record the issuing administrator in their actor column, and a
	// colocated pairing grant imported by a binary without the binding columns
	// is recognised by this actor value. An account with the same name would
	// make its grants look like unbound pairing grants, so the name is reserved.
	if user.Username == BootstrapGrantUseColocatedPairing {
		return invalidError("username %q is reserved", user.Username)
	}
	if err := validateOpaqueHash(user.PasswordHash); err != nil {
		return err
	}
	if err := user.Scope.Validate(); err != nil {
		return err
	}
	return nil
}

// newAccountIncarnation returns a fresh random identifier for one account
// row. Recreation must never reuse an earlier incarnation, so it is generated
// rather than derived from the username or row id.
func newAccountIncarnation() (string, error) {
	incarnation, err := newUUID()
	if err != nil {
		return "", wrapStorageError(CodeUnavailable, err, "generate account incarnation")
	}
	return incarnation, nil
}

// Create adds one user and writes an audit row in the same transaction.
// Administrative callers use Authorize with CreateTx so the actor check and
// the insert commit together.
func (s *Users) Create(ctx context.Context, actor string, user User) error {
	return s.db.Write(ctx, func(tx *Tx) error {
		return s.CreateTx(tx, actor, user)
	})
}

// CreateTx adds one user and its audit row inside tx. The caller is
// responsible for authorizing actor; the admin handler uses Authorize so the
// check and the insert commit together.
func (s *Users) CreateTx(tx *Tx, actor string, user User) error {
	if err := validateUser(user); err != nil {
		return err
	}
	now := s.now()
	scopeJSON, err := encodeScope(user.Scope)
	if err != nil {
		return err
	}
	var count int64
	if err := tx.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return mapError(err)
	}
	if count >= MaxUsers {
		return conflictError("at most %d users are supported", MaxUsers)
	}
	incarnation, err := newAccountIncarnation()
	if err != nil {
		return err
	}
	result, err := tx.Exec(
		"INSERT INTO users (username, password_hash, scope_json, is_admin, disabled, incarnation, version, created_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)",
		user.Username, user.PasswordHash, scopeJSON, boolToInt(user.Admin), boolToInt(user.Disabled), incarnation, now.UnixMilli(), now.UnixMilli(),
	)
	if err != nil {
		if CodeOf(err) == CodeConflict {
			return conflictError("user %q already exists", user.Username)
		}
		return err
	}
	if err := ensureAffected(result, CodeConflict, "user %q already exists", user.Username); err != nil {
		return err
	}
	return appendAuditTx(tx, "admin", actor, "user.create", user.Username, now)
}

// Update replaces the mutable fields of an existing user. An empty
// PasswordHash preserves the stored hash. The last active administrator can
// neither be demoted nor disabled. Administrative callers use Authorize with
// UpdateTx so the actor check and the write commit together.
func (s *Users) Update(ctx context.Context, actor string, user User) error {
	return s.db.Write(ctx, func(tx *Tx) error {
		return s.UpdateTx(tx, actor, user)
	})
}

// UpdateTx replaces one user inside tx, bumping its version so sessions issued
// before the change become stale. The caller authorizes actor.
func (s *Users) UpdateTx(tx *Tx, actor string, user User) error {
	if err := validateUsername(user.Username); err != nil {
		return err
	}
	if user.PasswordHash != "" {
		if err := validateOpaqueHash(user.PasswordHash); err != nil {
			return err
		}
	}
	if err := user.Scope.Validate(); err != nil {
		return err
	}
	now := s.now()
	scopeJSON, err := encodeScope(user.Scope)
	if err != nil {
		return err
	}
	var (
		existingHash                    string
		existingAdmin, existingDisabled int64
	)
	err = tx.QueryRow("SELECT password_hash, is_admin, disabled FROM users WHERE username = ?", user.Username).
		Scan(&existingHash, &existingAdmin, &existingDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		return notFoundError("user %q not found", user.Username)
	}
	if err != nil {
		return mapError(err)
	}
	if intToBool(existingAdmin) && !intToBool(existingDisabled) && (!user.Admin || user.Disabled) {
		if err := requireAnotherAdmin(tx, user.Username); err != nil {
			return err
		}
	}
	hash := user.PasswordHash
	action := "user.update"
	if hash == "" {
		hash = existingHash
	} else {
		action = "user.update-password"
	}
	if _, err := tx.Exec(
		"UPDATE users SET password_hash = ?, scope_json = ?, is_admin = ?, disabled = ?, version = version + 1, updated_at_ms = ? WHERE username = ?",
		hash, scopeJSON, boolToInt(user.Admin), boolToInt(user.Disabled), now.UnixMilli(), user.Username,
	); err != nil {
		return err
	}
	return appendAuditTx(tx, "admin", actor, action, user.Username, now)
}

// SetPasswordHash replaces one user's opaque hash and audits the change.
func (s *Users) SetPasswordHash(ctx context.Context, actor, username, hash string) error {
	return s.db.Write(ctx, func(tx *Tx) error {
		return s.SetPasswordHashTx(tx, actor, username, hash)
	})
}

// SetPasswordHashTx replaces one user's hash inside tx and bumps its version.
func (s *Users) SetPasswordHashTx(tx *Tx, actor, username, hash string) error {
	if err := validateUsername(username); err != nil {
		return err
	}
	if err := validateOpaqueHash(hash); err != nil {
		return err
	}
	now := s.now()
	result, err := tx.Exec(
		"UPDATE users SET password_hash = ?, version = version + 1, updated_at_ms = ? WHERE username = ?",
		hash, now.UnixMilli(), username,
	)
	if err != nil {
		return err
	}
	if err := ensureAffected(result, CodeNotFound, "user %q not found", username); err != nil {
		return err
	}
	return appendAuditTx(tx, "admin", actor, "user.update-password", username, now)
}

// Delete removes one user; the last active administrator cannot be deleted.
// Administrative callers use MutateAccount with DeleteTx so the actor check,
// the delete and the session revocation commit together.
func (s *Users) Delete(ctx context.Context, actor, username string) error {
	return s.db.Write(ctx, func(tx *Tx) error {
		return s.DeleteTx(tx, actor, username)
	})
}

// DeleteTx removes one user inside tx. The caller authorizes actor and is
// responsible for ending that user's sessions in the same transaction.
func (s *Users) DeleteTx(tx *Tx, actor, username string) error {
	if err := validateUsername(username); err != nil {
		return err
	}
	now := s.now()
	var admin, disabled int64
	err := tx.QueryRow("SELECT is_admin, disabled FROM users WHERE username = ?", username).Scan(&admin, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return notFoundError("user %q not found", username)
	}
	if err != nil {
		return mapError(err)
	}
	if intToBool(admin) && !intToBool(disabled) {
		if err := requireAnotherAdmin(tx, username); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM users WHERE username = ?", username); err != nil {
		return err
	}
	return appendAuditTx(tx, "admin", actor, "user.delete", username, now)
}

func requireAnotherAdmin(tx *Tx, excluded string) error {
	var count int64
	if err := tx.QueryRow(
		"SELECT COUNT(*) FROM users WHERE is_admin = 1 AND disabled = 0 AND username <> ?", excluded,
	).Scan(&count); err != nil {
		return mapError(err)
	}
	if count == 0 {
		return conflictError("the last active administrator cannot be removed or demoted")
	}
	return nil
}

// BootstrapMarkerKey is the settings key that records the one-time import of
// configured browser accounts.
const BootstrapMarkerKey = "admin.bootstrap_v1"

// BootstrapUser is one configured account imported once at first start. The
// password hash is opaque and must already be the auth package's format.
type BootstrapUser struct {
	Username     string
	PasswordHash string
	Scope        Scope
	Admin        bool
}

// BootstrapUsers imports the configured account directory exactly once. A
// marker row in settings prevents a later start from resurrecting a deleted
// or disabled user or from overwriting an operator-managed password, scope or
// admin flag. An explicit admin list elevates named existing accounts only
// during that first import. An unconfigured first start leaves the marker
// unset so the next start can still import its configuration.
//
// It reports whether anything changed.
func (s *Users) BootstrapUsers(ctx context.Context, users []BootstrapUser, adminUsernames []string) (bool, error) {
	if len(users) > MaxUsers {
		return false, invalidError("bootstrap user directory exceeds %d entries", MaxUsers)
	}
	seen := make(map[string]struct{}, len(users))
	for _, user := range users {
		if err := validateUsername(user.Username); err != nil {
			return false, err
		}
		if err := validateOpaqueHash(user.PasswordHash); err != nil {
			return false, err
		}
		if err := user.Scope.Validate(); err != nil {
			return false, err
		}
		if _, duplicate := seen[user.Username]; duplicate {
			return false, invalidError("duplicate bootstrap username %q", user.Username)
		}
		seen[user.Username] = struct{}{}
	}
	elevated := make(map[string]struct{}, len(adminUsernames))
	for _, username := range adminUsernames {
		if err := validateUsername(username); err != nil {
			return false, err
		}
		elevated[username] = struct{}{}
	}
	now := s.now()
	changed := false
	err := s.db.Write(ctx, func(tx *Tx) error {
		marker, err := txQueryRowExists(tx, "SELECT 1 FROM settings WHERE key = ?", BootstrapMarkerKey)
		if err != nil {
			return err
		}
		if marker {
			return nil
		}
		var count int64
		if err := tx.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
			return mapError(err)
		}
		if len(users) == 0 && count == 0 {
			return nil
		}
		for _, user := range users {
			scopeJSON, err := encodeScope(user.Scope)
			if err != nil {
				return err
			}
			incarnation, err := newAccountIncarnation()
			if err != nil {
				return err
			}
			admin := user.Admin
			if _, explicit := elevated[user.Username]; explicit {
				admin = true
			}
			result, err := tx.Exec(
				"INSERT OR IGNORE INTO users (username, password_hash, scope_json, is_admin, disabled, incarnation, version, created_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, 0, ?, 1, ?, ?)",
				user.Username, user.PasswordHash, scopeJSON, boolToInt(admin), incarnation, now.UnixMilli(), now.UnixMilli(),
			)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return mapError(err)
			}
			if affected == 1 {
				changed = true
				continue
			}
			if admin {
				// An existing row from an earlier pre-release database keeps
				// its hash and scope but honours the one-time elevation.
				if _, err := tx.Exec("UPDATE users SET is_admin = 1, version = version + 1, updated_at_ms = ? WHERE username = ?", now.UnixMilli(), user.Username); err != nil {
					return err
				}
				changed = true
			}
		}
		if _, err := tx.Exec(
			"INSERT INTO settings (key, value, updated_at_ms) VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at_ms = excluded.updated_at_ms",
			BootstrapMarkerKey, "1", now.UnixMilli(),
		); err != nil {
			return err
		}
		return nil
	})
	return changed, err
}
