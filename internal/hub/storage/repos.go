package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	// MaxAuditEvents bounds the audit history per scope. Appending prunes the
	// oldest rows beyond this bound.
	MaxAuditEvents = 1000
	// DefaultAuditLimit is the default page size for List.
	DefaultAuditLimit = 100
)

// Audit scopes.
const (
	AuditScopeAdmin     = "admin"
	AuditScopeCollector = "collector"
)

// AuditEntry is one operation-log row. It deliberately carries no payload,
// password hash or credential material.
type AuditEntry struct {
	Scope  string
	Actor  string
	Action string
	Target string
	At     time.Time
}

// Audit is the bounded operation log.
type Audit struct {
	db  *Database
	now func() time.Time
}

// NewAudit returns the audit repository over db.
func NewAudit(db *Database) *Audit {
	return &Audit{db: db, now: db.Now}
}

func validateAuditScope(scope string) error {
	if scope != AuditScopeAdmin && scope != AuditScopeCollector {
		return invalidError("audit scope must be %q or %q", AuditScopeAdmin, AuditScopeCollector)
	}
	return nil
}

func validateAuditField(name, value string) error {
	if value == "" || len(value) > 256 || containsNUL(value) {
		return invalidError("%s must be a nonempty string of at most 256 bytes", name)
	}
	return nil
}

func containsNUL(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] == 0 {
			return true
		}
	}
	return false
}

// Append writes one audit row and prunes its scope to MaxAuditEvents, in the
// same transaction as any caller mutation.
func (a *Audit) Append(ctx context.Context, entry AuditEntry) error {
	if err := validateAuditScope(entry.Scope); err != nil {
		return err
	}
	if err := validateAuditField("actor", entry.Actor); err != nil {
		return err
	}
	if err := validateAuditField("action", entry.Action); err != nil {
		return err
	}
	if err := validateAuditField("target", entry.Target); err != nil {
		return err
	}
	at := entry.At
	if at.IsZero() {
		at = a.now()
	}
	return a.db.Write(ctx, func(tx *Tx) error {
		return appendAuditTx(tx, entry.Scope, entry.Actor, entry.Action, entry.Target, at)
	})
}

// List returns a newest-first page for one scope, never more than
// MaxAuditEvents rows.
func (a *Audit) List(ctx context.Context, scope string, limit int) ([]AuditEntry, error) {
	if err := validateAuditScope(scope); err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = DefaultAuditLimit
	}
	if limit < 1 {
		return nil, invalidError("audit limit must be a positive integer")
	}
	if limit > MaxAuditEvents {
		limit = MaxAuditEvents
	}
	rows, err := a.db.Query(ctx,
		"SELECT scope, actor, action, target, at_ms FROM audit_events WHERE scope = ? ORDER BY id DESC LIMIT ?",
		scope, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []AuditEntry
	for rows.Next() {
		var (
			entry AuditEntry
			atMS  int64
		)
		if err := rows.Scan(&entry.Scope, &entry.Actor, &entry.Action, &entry.Target, &atMS); err != nil {
			return nil, mapError(err)
		}
		entry.At = msToTime(atMS)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return entries, nil
}

func appendAuditTx(tx *Tx, scope, actor, action, target string, at time.Time) error {
	if err := validateAuditScope(scope); err != nil {
		return err
	}
	if err := validateAuditField("actor", actor); err != nil {
		return err
	}
	if err := validateAuditField("action", action); err != nil {
		return err
	}
	if err := validateAuditField("target", target); err != nil {
		return err
	}
	if _, err := tx.Exec(
		"INSERT INTO audit_events (scope, actor, action, target, at_ms) VALUES (?, ?, ?, ?, ?)",
		scope, actor, action, target, at.UnixMilli(),
	); err != nil {
		return err
	}
	_, err := tx.Exec(
		"DELETE FROM audit_events WHERE scope = ? AND id NOT IN (SELECT id FROM audit_events WHERE scope = ? ORDER BY id DESC LIMIT ?)",
		scope, scope, MaxAuditEvents,
	)
	return err
}

// Settings is the small durable key/value table used for one-time markers and
// hub preferences. It is not a cache: every read goes to SQLite.
type Settings struct {
	db  *Database
	now func() time.Time
}

// NewSettings returns the settings repository over db.
func NewSettings(db *Database) *Settings {
	return &Settings{db: db, now: db.Now}
}

const maxSettingValueBytes = 64 * 1024

func validateSettingKey(key string) error {
	if len(key) == 0 || len(key) > 128 || containsNUL(key) {
		return invalidError("setting key must be 1..128 bytes without NUL")
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c < 0x21 || c > 0x7e {
			return invalidError("setting key must be printable ASCII without spaces")
		}
	}
	return nil
}

// Get returns one setting.
func (s *Settings) Get(ctx context.Context, key string) (string, bool, error) {
	if err := validateSettingKey(key); err != nil {
		return "", false, err
	}
	var value string
	err := s.db.QueryRow(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, mapError(err)
	}
	return value, true, nil
}

// Set creates or replaces one setting.
func (s *Settings) Set(ctx context.Context, key, value string) error {
	if err := validateSettingKey(key); err != nil {
		return err
	}
	if len(value) > maxSettingValueBytes {
		return invalidError("setting value exceeds %d bytes", maxSettingValueBytes)
	}
	now := s.now().UnixMilli()
	return s.db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(
			"INSERT INTO settings (key, value, updated_at_ms) VALUES (?, ?, ?) "+
				"ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at_ms = excluded.updated_at_ms",
			key, value, now,
		)
		return err
	})
}

// Delete removes one setting; deleting an absent key is not an error.
func (s *Settings) Delete(ctx context.Context, key string) error {
	if err := validateSettingKey(key); err != nil {
		return err
	}
	return s.db.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec("DELETE FROM settings WHERE key = ?", key)
		return err
	})
}

// All returns every setting, ordered by key.
func (s *Settings) All(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.Query(ctx, "SELECT key, value FROM settings ORDER BY key")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, mapError(err)
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return values, nil
}

// Sessions is the durable browser sign-in table. The auth package owns token
// generation and cookie handling; storage keys a session by the caller's
// SHA-256 token hash and never sees the raw cookie value.
type Sessions struct {
	db  *Database
	now func() time.Time
}

// NewSessions returns the session repository over db.
func NewSessions(db *Database) *Sessions {
	return &Sessions{db: db, now: db.Now}
}

// Session is one browser sign-in.
type Session struct {
	TokenHash string
	Username  string
	CSRFToken string
	Scope     Scope
	Admin     bool
	// UserVersion is the account version at sign-in and UserIncarnation is the
	// account's never-reused incarnation. Session lookup rejects a row whose
	// account changed since, so a scope, admin or password change cannot be
	// outrun by an existing cookie. The incarnation also rejects a session that
	// was minted before the username was deleted and recreated.
	UserVersion     int64
	UserIncarnation string
	CreatedAt       time.Time
	ExpiresAt       time.Time
}

const (
	sessionColumns = "token_hash, username, csrf_token, scope_json, is_admin, user_version, user_incarnation, created_at_ms, expires_at_ms"
	// sessionDeleteBatch bounds how many rows one sweep transaction deletes.
	sessionDeleteBatch = 64
)

// ErrAccountChanged reports that the account behind a login changed between the
// caller's password verification and the atomic session insert. Login answers
// it with the same generic credential failure as a wrong password.
var ErrAccountChanged = errors.New("storage: account changed")

// scanSession reads one session row. It never includes the raw cookie value.
func scanSession(row rowScanner) (Session, error) {
	var (
		session     Session
		scopeJSON   string
		admin       int64
		createdAtMS int64
		expiresAtMS int64
	)
	if err := row.Scan(&session.TokenHash, &session.Username, &session.CSRFToken, &scopeJSON, &admin,
		&session.UserVersion, &session.UserIncarnation, &createdAtMS, &expiresAtMS); err != nil {
		return Session{}, err
	}
	scope, err := parseScope(scopeJSON)
	if err != nil {
		return Session{}, err
	}
	session.Scope = scope
	session.Admin = intToBool(admin)
	session.CreatedAt = msToTime(createdAtMS)
	session.ExpiresAt = msToTime(expiresAtMS)
	return session, nil
}

func validateSession(session Session) error {
	if session.TokenHash == "" || len(session.TokenHash) > 256 || containsNUL(session.TokenHash) {
		return invalidError("session token hash must be a nonempty string of at most 256 bytes")
	}
	if err := validateUsername(session.Username); err != nil {
		return err
	}
	if session.CSRFToken == "" || len(session.CSRFToken) > 256 || containsNUL(session.CSRFToken) {
		return invalidError("session CSRF token must be a nonempty string of at most 256 bytes")
	}
	if len(session.UserIncarnation) > 256 || containsNUL(session.UserIncarnation) {
		return invalidError("session account incarnation must be at most 256 bytes")
	}
	now := session.CreatedAt
	if !now.IsZero() && !session.ExpiresAt.After(now) {
		return invalidError("session expiry must be after its creation time")
	}
	return nil
}

// querySessionsTx reads session rows inside tx. It closes the rows before
// returning so the same connection can run the deletes that follow.
func querySessionsTx(tx *Tx, query string, args ...any) ([]Session, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			if CodeOf(err) != "" {
				return nil, err
			}
			return nil, mapError(err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return sessions, nil
}

// accountStateTx reads the account fields a session snapshots. A missing or
// disabled account is reported by the caller as ErrAccountChanged.
func accountStateTx(tx *Tx, username string) (version int64, incarnation string, disabled bool, scopeJSON string, admin bool, err error) {
	var adminInt, disabledInt int64
	err = tx.QueryRow("SELECT version, incarnation, disabled, scope_json, is_admin FROM users WHERE username = ?", username).
		Scan(&version, &incarnation, &disabledInt, &scopeJSON, &adminInt)
	return version, incarnation, intToBool(disabledInt), scopeJSON, intToBool(adminInt), err
}

// trimSessionsTx deletes expired rows and then evicts the oldest rows until a
// new session fits under MaxSessions. It returns every removed row so the auth
// manager can broadcast the end after the transaction commits.
func trimSessionsTx(tx *Tx, now time.Time) ([]Session, []Session, error) {
	expired, err := querySessionsTx(tx, "SELECT "+sessionColumns+" FROM browser_sessions WHERE expires_at_ms <= ?", now.UnixMilli())
	if err != nil {
		return nil, nil, err
	}
	if len(expired) != 0 {
		if _, err := tx.Exec("DELETE FROM browser_sessions WHERE expires_at_ms <= ?", now.UnixMilli()); err != nil {
			return nil, nil, err
		}
	}
	var count int64
	if err := tx.QueryRow("SELECT COUNT(*) FROM browser_sessions").Scan(&count); err != nil {
		return nil, nil, mapError(err)
	}
	evicted := make([]Session, 0)
	for count >= MaxSessions {
		rows, err := querySessionsTx(tx,
			"SELECT "+sessionColumns+" FROM browser_sessions ORDER BY created_at_ms ASC, token_hash ASC LIMIT 1")
		if err != nil {
			return nil, nil, err
		}
		if len(rows) == 0 {
			break
		}
		oldest := rows[0]
		if _, err := tx.Exec("DELETE FROM browser_sessions WHERE token_hash = ?", oldest.TokenHash); err != nil {
			return nil, nil, err
		}
		evicted = append(evicted, oldest)
		count--
	}
	return expired, evicted, nil
}

// Create adds a session. The account must exist and be enabled; the session
// snapshots its current scope, admin flag, incarnation and version.
func (s *Sessions) Create(ctx context.Context, session Session) error {
	_, _, err := s.create(ctx, session, 0, "", false)
	return err
}

// CreateForUser adds a session only when the account still matches
// expectedVersion (and Session.UserIncarnation when it is set) and is enabled.
// The check, the table trim and the insert run in one write transaction, so an
// account change or revocation that commits first makes the login fail instead
// of minting a stale session. It returns the stored row plus every row removed
// to honour expiry and MaxSessions; the caller broadcasts those removals after
// commit.
func (s *Sessions) CreateForUser(ctx context.Context, session Session, expectedVersion int64) (Session, []Session, error) {
	return s.create(ctx, session, expectedVersion, session.UserIncarnation, true)
}

func (s *Sessions) create(ctx context.Context, session Session, expectedVersion int64, expectedIncarnation string, requireVersion bool) (Session, []Session, error) {
	if err := validateSession(session); err != nil {
		return Session{}, nil, err
	}
	now := session.CreatedAt
	if now.IsZero() {
		now = s.now()
	}
	var (
		stored  Session
		removed []Session
	)
	err := s.db.Write(ctx, func(tx *Tx) error {
		version, incarnation, disabled, scopeJSON, admin, err := accountStateTx(tx, session.Username)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAccountChanged
		}
		if err != nil {
			return mapError(err)
		}
		if disabled || (requireVersion && (version != expectedVersion ||
			// An empty expected incarnation proves nothing about which account
			// the password was verified against. It must never act as a
			// wildcard, even when the stored value is also empty (the default a
			// schema-5 writer leaves behind).
			expectedIncarnation == "" || incarnation != expectedIncarnation)) {
			return ErrAccountChanged
		}
		scope, err := parseScope(scopeJSON)
		if err != nil {
			return err
		}
		expired, evicted, err := trimSessionsTx(tx, now)
		if err != nil {
			return err
		}
		removed = append(expired, evicted...)
		stored = session
		stored.Scope = scope
		stored.Admin = admin
		stored.UserVersion = version
		stored.UserIncarnation = incarnation
		stored.CreatedAt = now
		_, err = tx.Exec(
			"INSERT INTO browser_sessions (token_hash, username, csrf_token, scope_json, is_admin, user_version, user_incarnation, created_at_ms, expires_at_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			stored.TokenHash, stored.Username, stored.CSRFToken, scopeJSON,
			boolToInt(stored.Admin), stored.UserVersion, stored.UserIncarnation, now.UnixMilli(), stored.ExpiresAt.UnixMilli(),
		)
		return err
	})
	if err != nil {
		return Session{}, nil, err
	}
	return stored, removed, nil
}

// Find returns one live session. A session whose account was deleted or
// disabled, or whose recorded account version is stale, is treated as absent;
// so is an expired row, which is deleted.
func (s *Sessions) Find(ctx context.Context, tokenHash string) (Session, error) {
	if tokenHash == "" || len(tokenHash) > 256 || containsNUL(tokenHash) {
		return Session{}, notFoundError("session not found")
	}
	session, err := scanSession(s.db.QueryRow(ctx,
		"SELECT s.token_hash, s.username, s.csrf_token, s.scope_json, s.is_admin, s.user_version, s.user_incarnation, s.created_at_ms, s.expires_at_ms "+
			"FROM browser_sessions s JOIN users u ON u.username = s.username "+
			"WHERE s.token_hash = ? AND u.disabled = 0 AND u.version = s.user_version AND u.incarnation = s.user_incarnation",
		tokenHash,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, notFoundError("session not found")
	}
	if err != nil {
		if CodeOf(err) != "" {
			return Session{}, err
		}
		return Session{}, mapError(err)
	}
	if !session.ExpiresAt.After(s.now()) {
		_, _ = s.Delete(ctx, tokenHash)
		return Session{}, notFoundError("session expired")
	}
	return session, nil
}

// List returns every adoptable session ordered by creation time. Sessions of a
// deleted or disabled account, or with a stale account version, are omitted so
// a hub restart never resurrects an ended sign-in. Expired rows are filtered by
// the caller.
func (s *Sessions) List(ctx context.Context) ([]Session, error) {
	rows, err := s.db.Query(ctx,
		"SELECT s.token_hash, s.username, s.csrf_token, s.scope_json, s.is_admin, s.user_version, s.user_incarnation, s.created_at_ms, s.expires_at_ms "+
			"FROM browser_sessions s JOIN users u ON u.username = s.username "+
			"WHERE u.disabled = 0 AND u.version = s.user_version AND u.incarnation = s.user_incarnation "+
			"ORDER BY s.created_at_ms ASC, s.token_hash ASC",
	)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			if CodeOf(err) != "" {
				return nil, err
			}
			return nil, mapError(err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return sessions, nil
}

// Delete ends one session and reports whether it existed.
func (s *Sessions) Delete(ctx context.Context, tokenHash string) (bool, error) {
	if tokenHash == "" || len(tokenHash) > 256 || containsNUL(tokenHash) {
		return false, nil
	}
	var existed bool
	err := s.db.Write(ctx, func(tx *Tx) error {
		result, err := tx.Exec("DELETE FROM browser_sessions WHERE token_hash = ?", tokenHash)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return mapError(err)
		}
		existed = affected > 0
		return nil
	})
	return existed, err
}

// DeleteMany removes a bounded batch of sessions and reports how many ended.
// The expiry sweep uses it after it has already notified the ended sessions.
func (s *Sessions) DeleteMany(ctx context.Context, tokenHashes []string) (int, error) {
	deleted := 0
	for start := 0; start < len(tokenHashes); start += sessionDeleteBatch {
		end := start + sessionDeleteBatch
		if end > len(tokenHashes) {
			end = len(tokenHashes)
		}
		batch := tokenHashes[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for index, hash := range batch {
			placeholders[index] = "?"
			args[index] = hash
		}
		err := s.db.Write(ctx, func(tx *Tx) error {
			result, err := tx.Exec(
				"DELETE FROM browser_sessions WHERE token_hash IN ("+strings.Join(placeholders, ", ")+")",
				args...,
			)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return mapError(err)
			}
			deleted += int(affected)
			return nil
		})
		if err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// deleteUserSessionsTx removes every session of one account inside the caller's
// transaction and returns the removed rows. The caller broadcasts them after
// commit.
func deleteUserSessionsTx(tx *Tx, username string) ([]Session, error) {
	sessions, err := querySessionsTx(tx, "SELECT "+sessionColumns+" FROM browser_sessions WHERE username = ?", username)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec("DELETE FROM browser_sessions WHERE username = ?", username); err != nil {
		return nil, err
	}
	return sessions, nil
}

// DeleteUser ends every session for one username and reports how many ended.
func (s *Sessions) DeleteUser(ctx context.Context, username string) (int, error) {
	ended, err := s.DeleteUserRows(ctx, username)
	return len(ended), err
}

// DeleteUserRows ends every session for one username and returns the removed
// rows so the caller can broadcast them.
func (s *Sessions) DeleteUserRows(ctx context.Context, username string) ([]Session, error) {
	if err := validateUsername(username); err != nil {
		return nil, err
	}
	var ended []Session
	err := s.db.Write(ctx, func(tx *Tx) error {
		sessions, err := deleteUserSessionsTx(tx, username)
		if err != nil {
			return err
		}
		ended = sessions
		return nil
	})
	return ended, err
}

// SweepExpired removes every session past its fixed expiry and reports how
// many were removed.
func (s *Sessions) SweepExpired(ctx context.Context) (int, error) {
	deleted := 0
	err := s.db.Write(ctx, func(tx *Tx) error {
		result, err := tx.Exec("DELETE FROM browser_sessions WHERE expires_at_ms <= ?", s.now().UnixMilli())
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return mapError(err)
		}
		deleted = int(affected)
		return nil
	})
	return deleted, err
}

// Count reports the number of stored sessions.
func (s *Sessions) Count(ctx context.Context) (int, error) {
	var count int64
	if err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM browser_sessions").Scan(&count); err != nil {
		return 0, mapError(err)
	}
	return int(count), nil
}

// Machines owns hub-local machine presentation metadata. It never replaces
// inventory or telemetry and is safe to update for a machine the hub has not
// seen yet.
type Machines struct {
	db  *Database
	now func() time.Time
}

// NewMachines returns the machine metadata repository over db.
func NewMachines(db *Database) *Machines {
	return &Machines{db: db, now: db.Now}
}

// MachineMeta is the hub-local presentation state for one machine.
type MachineMeta struct {
	MachineID   string
	DisplayName *string
	Tags        []string
	UpdatedAt   time.Time
}

// MetaUpdate is a partial update: nil fields are preserved.
type MetaUpdate struct {
	DisplayName      *string
	ClearDisplayName bool
	Tags             *[]string
}

// Get returns one machine's metadata. An unknown machine returns an empty
// record with no error, matching the "no presentation metadata yet" state.
func (m *Machines) Get(ctx context.Context, machineID string) (MachineMeta, error) {
	if !identifierPattern.MatchString(machineID) {
		return MachineMeta{}, invalidError("machine id must be 1..128 printable ASCII characters")
	}
	var (
		displayName sql.NullString
		tagsJSON    string
		updatedAtMS int64
	)
	err := m.db.QueryRow(ctx,
		"SELECT display_name, tags_json, updated_at_ms FROM machine_meta WHERE machine_id = ?", machineID,
	).Scan(&displayName, &tagsJSON, &updatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return MachineMeta{MachineID: machineID, Tags: []string{}}, nil
	}
	if err != nil {
		return MachineMeta{}, mapError(err)
	}
	meta := MachineMeta{MachineID: machineID, Tags: []string{}, UpdatedAt: msToTime(updatedAtMS)}
	if displayName.Valid {
		name := displayName.String
		meta.DisplayName = &name
	}
	tags, err := decodeTags(tagsJSON)
	if err != nil {
		return MachineMeta{}, err
	}
	meta.Tags = tags
	return meta, nil
}

// List returns every machine with metadata, ordered by machine id.
func (m *Machines) List(ctx context.Context) ([]MachineMeta, error) {
	rows, err := m.db.Query(ctx, "SELECT machine_id, display_name, tags_json, updated_at_ms FROM machine_meta ORDER BY machine_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []MachineMeta
	for rows.Next() {
		var (
			meta        MachineMeta
			displayName sql.NullString
			tagsJSON    string
			updatedAtMS int64
		)
		if err := rows.Scan(&meta.MachineID, &displayName, &tagsJSON, &updatedAtMS); err != nil {
			return nil, mapError(err)
		}
		if displayName.Valid {
			name := displayName.String
			meta.DisplayName = &name
		}
		tags, err := decodeTags(tagsJSON)
		if err != nil {
			return nil, err
		}
		meta.Tags = tags
		meta.UpdatedAt = msToTime(updatedAtMS)
		result = append(result, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return result, nil
}

// Update applies a partial metadata update in one transaction.
func (m *Machines) Update(ctx context.Context, machineID string, update MetaUpdate) error {
	return m.db.Write(ctx, func(tx *Tx) error {
		return m.UpdateTx(tx, machineID, update)
	})
}

// UpdateTx applies a partial metadata update inside tx. The caller authorizes
// the acting administrator; the admin handler wraps it in Users.Authorize so a
// concurrent demotion cannot leave a committed change.
func (m *Machines) UpdateTx(tx *Tx, machineID string, update MetaUpdate) error {
	if !identifierPattern.MatchString(machineID) {
		return invalidError("machine id must be 1..128 printable ASCII characters")
	}
	if update.DisplayName != nil && update.ClearDisplayName {
		return invalidError("set or clear the display name, not both")
	}
	var (
		displayName sql.NullString
		tagsJSON    string
	)
	if update.DisplayName != nil {
		displayName = sql.NullString{String: *update.DisplayName, Valid: true}
	}
	if displayName.Valid {
		if len(displayName.String) > 256 || containsNUL(displayName.String) {
			return invalidError("display name must be at most 256 bytes without NUL")
		}
	}
	if update.Tags != nil {
		tags, err := validateTags(*update.Tags)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(tags)
		if err != nil {
			return wrapStorageError(CodeInvalid, err, "encode machine tags")
		}
		tagsJSON = string(encoded)
	}
	now := m.now().UnixMilli()
	switch {
	case update.DisplayName != nil || update.ClearDisplayName:
		tags := update.Tags != nil
		if tags {
			_, err := tx.Exec(
				"INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES (?, ?, ?, ?) "+
					"ON CONFLICT(machine_id) DO UPDATE SET display_name = excluded.display_name, tags_json = excluded.tags_json, updated_at_ms = excluded.updated_at_ms",
				machineID, displayName, tagsJSON, now,
			)
			return err
		}
		_, err := tx.Exec(
			"INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES (?, ?, '[]', ?) "+
				"ON CONFLICT(machine_id) DO UPDATE SET display_name = excluded.display_name, updated_at_ms = excluded.updated_at_ms",
			machineID, displayName, now,
		)
		return err
	case update.Tags != nil:
		_, err := tx.Exec(
			"INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES (?, NULL, ?, ?) "+
				"ON CONFLICT(machine_id) DO UPDATE SET tags_json = excluded.tags_json, updated_at_ms = excluded.updated_at_ms",
			machineID, tagsJSON, now,
		)
		return err
	default:
		_, err := tx.Exec(
			"INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES (?, NULL, '[]', ?) "+
				"ON CONFLICT(machine_id) DO UPDATE SET updated_at_ms = excluded.updated_at_ms",
			machineID, now,
		)
		return err
	}
}

func validateTags(tags []string) ([]string, error) {
	if len(tags) > 64 {
		return nil, invalidError("tags must contain at most 64 entries")
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if len(tag) == 0 || len(tag) > 64 || containsNUL(tag) {
			return nil, invalidError("tags must be nonempty strings of at most 64 bytes")
		}
		if _, duplicate := seen[tag]; duplicate {
			return nil, invalidError("tags must be unique")
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out, nil
}

func decodeTags(raw string) ([]string, error) {
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		// Corrupt presentation metadata must not take down telemetry reads.
		return []string{}, nil
	}
	if tags == nil {
		tags = []string{}
	}
	return tags, nil
}
