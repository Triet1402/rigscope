package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Migration is one irreversible, ordered schema change. Versions start at 1
// and are contiguous.
//
// CompatibleDown is the lowest schema version whose binary can safely open and
// write the database after this migration. An additive N→N+1 migration
// declares N: an older binary sees only the columns and tables it knows and
// can keep writing them. A breaking migration declares N+1, because the older
// binary would read or write the file unsafely. The database-wide floor is the
// maximum CompatibleDown over every applied migration, and a binary whose own
// schema S is lower than the ledger's latest version may open the file only
// when S is at least that floor.
//
// Prefer additive statements. After runs inside the migration transaction for
// data that SQL cannot derive (for example hashing legacy plaintext values).
type Migration struct {
	Version        int
	Name           string
	CompatibleDown int
	Statements     []string
	After          func(*Tx) error
}

// AppliedMigration is one row of the schema ledger.
type AppliedMigration struct {
	Version        int
	Name           string
	CompatibleDown int
	AppliedAt      time.Time
}

// SchemaInfo is the migration ledger plus the database-wide compatibility
// floor.
//
// Version is the highest version recorded in the file, which can be newer than
// this build when an older binary opened a database migrated by a newer one.
// CompatibleDown is the lowest schema version whose binary can still open and
// write the file: the maximum CompatibleDown over every applied migration. A
// rollback to a binary that writes schema P is safe exactly when P >=
// CompatibleDown.
type SchemaInfo struct {
	Version        int
	CompatibleDown int
	Applied        []AppliedMigration
}

// migrations is the ordered ledger. Append new versions; never edit an
// applied migration in place.
//
// Version 1 is the 1.0 baseline. There is no 0.2 importer: an installation
// starts from this schema, and a 0.2 database is rejected by the contiguity
// and name checks because it carries a different migration history.
// CompatibleDown is the floor contract from docs/rewrite/PLAN.md: an
// additive migration keeps the floor of the migration before it, and a
// breaking migration to version N declares N. The ledger floor is the maximum
// over applied migrations, so it equals the version of the latest breaking
// migration.
var migrations = []Migration{
	{
		Version:        1,
		Name:           "foundation",
		CompatibleDown: 1,
		Statements: []string{
			`CREATE TABLE users (
				username TEXT PRIMARY KEY,
				password_hash TEXT NOT NULL,
				scope_json TEXT NOT NULL,
				is_admin INTEGER NOT NULL DEFAULT 0,
				disabled INTEGER NOT NULL DEFAULT 0,
				created_at_ms INTEGER NOT NULL,
				updated_at_ms INTEGER NOT NULL
			)`,
			`CREATE TABLE browser_sessions (
				token_hash TEXT PRIMARY KEY,
				username TEXT NOT NULL,
				csrf_token TEXT NOT NULL,
				scope_json TEXT NOT NULL,
				is_admin INTEGER NOT NULL DEFAULT 0,
				created_at_ms INTEGER NOT NULL,
				expires_at_ms INTEGER NOT NULL
			)`,
			`CREATE INDEX browser_sessions_expiry ON browser_sessions (expires_at_ms)`,
			`CREATE INDEX browser_sessions_username ON browser_sessions (username)`,
			`CREATE TABLE settings (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL,
				updated_at_ms INTEGER NOT NULL
			)`,
			`CREATE TABLE audit_events (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				scope TEXT NOT NULL,
				actor TEXT NOT NULL,
				action TEXT NOT NULL,
				target TEXT NOT NULL,
				at_ms INTEGER NOT NULL
			)`,
			`CREATE INDEX audit_events_scope_id ON audit_events (scope, id DESC)`,
			`CREATE TABLE machine_meta (
				machine_id TEXT PRIMARY KEY,
				display_name TEXT,
				tags_json TEXT NOT NULL DEFAULT '[]',
				updated_at_ms INTEGER NOT NULL
			)`,
		},
	},
	{
		Version:        2,
		Name:           "collector_enrollment",
		CompatibleDown: 1,
		Statements: []string{
			// The registry is authoritative. A machine mapping is hub-local and
			// never accepted from the collector. The credential is stored as
			// issued because a lost enrollment response must be replayable with
			// the same grant; the database file itself is 0600 in a 0700
			// directory.
			`CREATE TABLE collector_registry (
				collector_id TEXT PRIMARY KEY,
				machine_id TEXT NOT NULL UNIQUE,
				token TEXT NOT NULL UNIQUE,
				revoked INTEGER NOT NULL DEFAULT 0,
				created_at_ms INTEGER NOT NULL,
				updated_at_ms INTEGER NOT NULL
			)`,
			// Grants are keyed by SHA-256 of the one-time token, never the token
			// itself. consumed_by marks a spent grant; request_id and the
			// recovery hash allow exactly one replay of a lost enrollment
			// response until recovery_until_ms.
			`CREATE TABLE collector_grants (
				token_hash TEXT PRIMARY KEY,
				expires_at_ms INTEGER NOT NULL,
				replace_machine_id TEXT,
				consumed_by TEXT,
				request_id TEXT,
				recovery_hash TEXT,
				recovery_until_ms INTEGER,
				actor TEXT NOT NULL,
				created_at_ms INTEGER NOT NULL
			)`,
			`CREATE INDEX collector_grants_expiry ON collector_grants (expires_at_ms)`,
			`CREATE TABLE collector_registry_meta (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL
			)`,
		},
	},
	{
		Version:        3,
		Name:           "alerts",
		CompatibleDown: 1, // additive
		Statements: []string{
			// Rules are durable. Per-rule pending/incident state lives here so a
			// restart can continue a duration or avoid opening a duplicate
			// incident.
			`CREATE TABLE alert_rules (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL,
				machine_id TEXT NOT NULL,
				metric TEXT NOT NULL,
				resource_id TEXT NOT NULL DEFAULT '',
				condition TEXT NOT NULL,
				threshold REAL,
				duration_ms INTEGER NOT NULL DEFAULT 0,
				enabled INTEGER NOT NULL DEFAULT 1,
				maintenance_until_ms INTEGER,
				state TEXT NOT NULL DEFAULT 'ok',
				pending_since_ms INTEGER,
				active_incident_id TEXT,
				last_value REAL,
				last_observed_at_ms INTEGER,
				created_at_ms INTEGER NOT NULL,
				updated_at_ms INTEGER NOT NULL
			)`,
			`CREATE INDEX alert_rules_machine ON alert_rules (machine_id)`,
			// Incident history is bounded by the engine, but an open row is
			// never lost. At most one open incident per rule is a schema
			// invariant.
			`CREATE TABLE alert_incidents (
				id TEXT PRIMARY KEY,
				rule_id TEXT NOT NULL,
				rule_name TEXT NOT NULL,
				machine_id TEXT NOT NULL,
				metric TEXT NOT NULL,
				resource_id TEXT NOT NULL DEFAULT '',
				condition TEXT NOT NULL,
				status TEXT NOT NULL,
				started_at_ms INTEGER NOT NULL,
				resolved_at_ms INTEGER,
				started_value REAL,
				resolved_value REAL,
				last_observed_at_ms INTEGER NOT NULL,
				message TEXT NOT NULL
			)`,
			`CREATE INDEX alert_incidents_started ON alert_incidents (started_at_ms DESC)`,
			`CREATE INDEX alert_incidents_machine_started ON alert_incidents (machine_id, started_at_ms DESC)`,
			`CREATE INDEX alert_incidents_rule_status ON alert_incidents (rule_id, status)`,
			`CREATE UNIQUE INDEX alert_incidents_one_open ON alert_incidents (rule_id) WHERE status = 'open'`,
			// One durable job per logical incident event; the primary key is the
			// deterministic idempotency key sent to the webhook receiver.
			`CREATE TABLE alert_jobs (
				id TEXT PRIMARY KEY,
				rule_id TEXT NOT NULL,
				incident_id TEXT NOT NULL,
				machine_id TEXT NOT NULL,
				event TEXT NOT NULL,
				status TEXT NOT NULL,
				attempts INTEGER NOT NULL DEFAULT 0,
				max_attempts INTEGER NOT NULL DEFAULT 6,
				next_attempt_ms INTEGER,
				last_attempt_ms INTEGER,
				last_error TEXT,
				created_at_ms INTEGER NOT NULL,
				delivered_at_ms INTEGER,
				payload_json TEXT NOT NULL,
				UNIQUE (incident_id, event)
			)`,
			`CREATE INDEX alert_jobs_due ON alert_jobs (status, next_attempt_ms)`,
			`CREATE INDEX alert_jobs_created ON alert_jobs (created_at_ms DESC)`,
			// Exactly one webhook channel. URL, headers and optional HMAC secret
			// are private database state and are never returned by the browser
			// API.
			`CREATE TABLE alert_webhook (
				id INTEGER PRIMARY KEY CHECK (id = 1),
				url TEXT NOT NULL,
				headers_json TEXT NOT NULL DEFAULT '{}',
				secret TEXT,
				enabled INTEGER NOT NULL DEFAULT 1,
				updated_at_ms INTEGER NOT NULL,
				last_test_at_ms INTEGER,
				last_test_ok INTEGER,
				last_error TEXT
			)`,
			`CREATE TABLE alert_machine_progress (
				machine_id TEXT PRIMARY KEY,
				collector_id TEXT NOT NULL,
				run_id TEXT NOT NULL,
				seq INTEGER NOT NULL,
				received_at_ms INTEGER NOT NULL,
				updated_at_ms INTEGER NOT NULL
			)`,
			`CREATE TABLE alert_observed_resources (
				rule_id TEXT NOT NULL,
				metric TEXT NOT NULL,
				resource_id TEXT NOT NULL,
				first_seen_ms INTEGER NOT NULL,
				last_seen_ms INTEGER NOT NULL,
				PRIMARY KEY (rule_id, metric, resource_id)
			)`,
		},
	},
	{
		Version:        4,
		Name:           "telemetry",
		CompatibleDown: 1, // additive
		Statements: []string{
			// Integer series catalog. resource_id is '' for single-resource
			// families and a stable id for per-resource families.
			`CREATE TABLE series (
				series_id INTEGER PRIMARY KEY,
				machine_id TEXT NOT NULL,
				metric TEXT NOT NULL,
				resource_id TEXT NOT NULL DEFAULT '',
				UNIQUE (machine_id, metric, resource_id)
			)`,
			// Compact immutable summary tiers. min/max preserve spikes; count
			// counts non-NULL readings and null_count explicit unavailable
			// readings, so unavailable is never mistaken for zero. WITHOUT ROWID
			// makes (series_id, bucket_ms) the clustered range key.
			`CREATE TABLE points_10s (
				series_id INTEGER NOT NULL,
				bucket_ms INTEGER NOT NULL,
				min REAL,
				max REAL,
				sum REAL NOT NULL DEFAULT 0,
				count INTEGER NOT NULL DEFAULT 0,
				null_count INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (series_id, bucket_ms)
			) WITHOUT ROWID`,
			`CREATE TABLE points_1m (
				series_id INTEGER NOT NULL,
				bucket_ms INTEGER NOT NULL,
				min REAL,
				max REAL,
				sum REAL NOT NULL DEFAULT 0,
				count INTEGER NOT NULL DEFAULT 0,
				null_count INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (series_id, bucket_ms)
			) WITHOUT ROWID`,
			`CREATE TABLE points_1h (
				series_id INTEGER NOT NULL,
				bucket_ms INTEGER NOT NULL,
				min REAL,
				max REAL,
				sum REAL NOT NULL DEFAULT 0,
				count INTEGER NOT NULL DEFAULT 0,
				null_count INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (series_id, bucket_ms)
			) WITHOUT ROWID`,
			// Durable receipt / dedup key. A record's side effects are written
			// only when this insert reported one changed row, so at-least-once
			// delivery (including a lost ack after COMMIT) is idempotent.
			`CREATE TABLE sample_receipts (
				collector_id TEXT NOT NULL,
				run_id TEXT NOT NULL,
				seq INTEGER NOT NULL,
				machine_id TEXT NOT NULL,
				kind TEXT NOT NULL,
				mode TEXT NOT NULL,
				captured_at_ms INTEGER NOT NULL,
				received_at_ms INTEGER NOT NULL,
				expected_interval_ms INTEGER,
				PRIMARY KEY (collector_id, run_id, seq)
			) WITHOUT ROWID`,
			`CREATE INDEX sample_receipts_machine_captured ON sample_receipts (machine_id, captured_at_ms)`,
			`CREATE INDEX sample_receipts_captured ON sample_receipts (captured_at_ms)`,
			// Collector-reported dropped intervals.
			`CREATE TABLE gaps (
				collector_id TEXT NOT NULL,
				run_id TEXT NOT NULL,
				seq INTEGER NOT NULL,
				machine_id TEXT NOT NULL,
				from_ms INTEGER NOT NULL,
				to_ms INTEGER NOT NULL,
				dropped INTEGER NOT NULL,
				reason TEXT NOT NULL,
				received_at_ms INTEGER NOT NULL,
				PRIMARY KEY (collector_id, run_id, seq)
			) WITHOUT ROWID`,
			`CREATE INDEX gaps_machine_to ON gaps (machine_id, to_ms)`,
			`CREATE INDEX gaps_to ON gaps (to_ms)`,
			// Latest LIVE metrics per machine, persisted so a restart can show
			// last-known values as stale rather than fresh. Replay never touches
			// this table.
			`CREATE TABLE machine_latest (
				machine_id TEXT PRIMARY KEY,
				collector_id TEXT NOT NULL,
				run_id TEXT NOT NULL,
				boot_id TEXT NOT NULL DEFAULT '',
				seq INTEGER NOT NULL,
				captured_at_ms INTEGER NOT NULL,
				received_at_ms INTEGER NOT NULL,
				expected_interval_ms INTEGER,
				hostname TEXT,
				payload_json TEXT NOT NULL
			)`,
			`CREATE TABLE inventory (
				machine_id TEXT PRIMARY KEY,
				captured_at_ms INTEGER NOT NULL,
				received_at_ms INTEGER NOT NULL,
				inventory_json TEXT NOT NULL
			)`,
		},
	},
	{
		Version: 5,
		Name:    "collector_credential_hashing",
		// Breaking: previous binaries stored and compared plaintext collector
		// credentials and cannot authenticate the hashed form this migration
		// leaves behind.
		CompatibleDown: 5,
		Statements: []string{
			`ALTER TABLE collector_registry ADD COLUMN token_hash TEXT`,
			// A bounded encrypted copy of the issued credential exists only
			// while a lost enrollment response can still be recovered. It is
			// sealed with a key derived from the collector's recovery secret
			// and deleted after the first successful authenticated connection
			// or when the recovery window closes.
			`ALTER TABLE collector_registry ADD COLUMN credential_sealed BLOB`,
			`ALTER TABLE collector_registry ADD COLUMN credential_nonce BLOB`,
			`CREATE UNIQUE INDEX collector_registry_token_hash ON collector_registry (token_hash)`,
		},
		After: hashLegacyCollectorCredentials,
	},
	{
		Version:        6,
		Name:           "account_version",
		CompatibleDown: 5, // additive: older binaries ignore the new columns
		Statements: []string{
			// Every account change bumps users.version. A login compares it
			// after password verification, inside the session-insert
			// transaction, and session lookup rejects a row whose recorded
			// version is stale.
			`ALTER TABLE users ADD COLUMN version INTEGER NOT NULL DEFAULT 1`,
			// Incarnation is a random 128-bit identifier that never repeats.
			// It distinguishes a recreated username from the account that used
			// the name before, which a per-account version alone cannot.
			`ALTER TABLE users ADD COLUMN incarnation TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE browser_sessions ADD COLUMN user_version INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE browser_sessions ADD COLUMN user_incarnation TEXT NOT NULL DEFAULT ''`,
		},
		// Existing sessions adopt their account only when the stored snapshot
		// (enabled, admin flag and scope) still matches exactly. A disabled,
		// orphaned or privilege-changed session is deleted instead of being
		// granted the account's current version.
		After: adoptAccountSessions,
	},
	{
		Version: 7,
		Name:    "pairing_grant_binding",
		// Additive: older binaries ignore the new columns and keep issuing and
		// consuming grants by token hash. The compatibility floor therefore
		// stays at 5, the last breaking migration.
		CompatibleDown: 5,
		Statements: []string{
			// purpose distinguishes the installer's automatic colocated
			// pairing grant from an administrator-issued one. An automatic
			// grant is only redeemable from the loopback peer and by the
			// collector id the installer intended.
			`ALTER TABLE collector_grants ADD COLUMN purpose TEXT`,
			// collector_id is the installation identity the installer bound an
			// automatic grant to. NULL for every other grant, including the
			// legacy admin-issued form.
			`ALTER TABLE collector_grants ADD COLUMN collector_id TEXT`,
		},
	},
}

// adoptAccountSessions is the data step for migration 6. It gives every
// existing account a never-reused incarnation and carries forward only the
// browser sessions whose stored snapshot still matches that account. 1.0 has
// not shipped, so migration 6 is amended in place rather than adding a newer
// migration for the same columns.
func adoptAccountSessions(tx *Tx) error {
	rows, err := tx.Query("SELECT username FROM users ORDER BY username")
	if err != nil {
		return err
	}
	var usernames []string
	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err != nil {
			rows.Close()
			return mapError(err)
		}
		usernames = append(usernames, username)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return mapError(err)
	}
	rows.Close()
	for _, username := range usernames {
		incarnation, err := newUUID()
		if err != nil {
			return wrapStorageError(CodeUnavailable, err, "generate account incarnation")
		}
		if _, err := tx.Exec("UPDATE users SET incarnation = ? WHERE username = ?", incarnation, username); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`DELETE FROM browser_sessions WHERE NOT EXISTS (
			SELECT 1 FROM users u
			WHERE u.username = browser_sessions.username
				AND u.disabled = 0
				AND u.is_admin = browser_sessions.is_admin
				AND u.scope_json = browser_sessions.scope_json
		)`,
	); err != nil {
		return err
	}
	_, err = tx.Exec(
		`UPDATE browser_sessions SET
			user_version = (SELECT u.version FROM users u WHERE u.username = browser_sessions.username),
			user_incarnation = (SELECT u.incarnation FROM users u WHERE u.username = browser_sessions.username)`,
	)
	return err
}

// LatestSchemaVersion is the schema version this build writes.
func LatestSchemaVersion() int {
	return migrations[len(migrations)-1].Version
}

// MigrationLedger returns a copy of the ordered migration list. Callers use it
// for diagnostics and upgrade planning; it is not mutable.
func MigrationLedger() []Migration {
	out := make([]Migration, len(migrations))
	for i, migration := range migrations {
		out[i] = migration
		out[i].Statements = append([]string(nil), migration.Statements...)
	}
	return out
}

func (d *Database) migrate(ctx context.Context) error {
	if _, err := d.write.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		compatible_down INTEGER NOT NULL,
		applied_at_ms INTEGER NOT NULL
	)`); err != nil {
		return wrapStorageError(CodeUnavailable, mapError(err), "create hub migration ledger")
	}
	rows, err := d.write.QueryContext(ctx, "SELECT version, name, compatible_down, applied_at_ms FROM schema_migrations ORDER BY version")
	if err != nil {
		return wrapStorageError(CodeUnavailable, mapError(err), "read hub migration ledger")
	}
	applied := make([]AppliedMigration, 0, len(migrations))
	for rows.Next() {
		var (
			version        int64
			name           string
			compatibleDown int64
			appliedAtMS    int64
		)
		if err := rows.Scan(&version, &name, &compatibleDown, &appliedAtMS); err != nil {
			rows.Close()
			return wrapStorageError(CodeUnavailable, mapError(err), "read hub migration ledger")
		}
		applied = append(applied, AppliedMigration{
			Version:        int(version),
			Name:           name,
			CompatibleDown: int(compatibleDown),
			AppliedAt:      time.UnixMilli(appliedAtMS).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return wrapStorageError(CodeUnavailable, mapError(err), "read hub migration ledger")
	}
	rows.Close()

	knownLatest := LatestSchemaVersion()
	ledgerLatest := 0
	floor := 1
	for index, row := range applied {
		if row.Version != index+1 {
			return &Error{
				Code: CodeUnavailable,
				Message: fmt.Sprintf(
					"hub database migration history is not contiguous at version %d; it was written by a hub this build does not recognize",
					row.Version,
				),
			}
		}
		if row.CompatibleDown < 1 || row.CompatibleDown > row.Version {
			return &Error{
				Code: CodeUnavailable,
				Message: fmt.Sprintf(
					"hub database migration %d declares compatible_down %d outside 1..%d",
					row.Version, row.CompatibleDown, row.Version,
				),
			}
		}
		if row.Version <= knownLatest {
			expected := migrations[row.Version-1]
			if row.Name != expected.Name {
				return &Error{
					Code:    CodeUnavailable,
					Message: fmt.Sprintf("hub database migration %d is named %q, expected %q", row.Version, row.Name, expected.Name),
				}
			}
			expectedDown := expected.CompatibleDown
			if expectedDown < 1 || expectedDown > expected.Version {
				expectedDown = expected.Version
			}
			if row.CompatibleDown != expectedDown {
				return &Error{
					Code: CodeUnavailable,
					Message: fmt.Sprintf(
						"hub database migration %d declares compatible_down %d, expected %d",
						row.Version, row.CompatibleDown, expectedDown,
					),
				}
			}
		}
		ledgerLatest = row.Version
		if row.CompatibleDown > floor {
			floor = row.CompatibleDown
		}
	}

	if ledgerLatest > knownLatest {
		// A binary older than the file may operate on the subset it knows, but
		// only when its own schema is still at or above the compatibility
		// floor. It must not run the unknown migrations.
		if knownLatest < floor {
			return &Error{
				Code: CodeUnavailable,
				Message: fmt.Sprintf(
					"hub database schema version %d requires a hub that writes at least schema version %d, but this hub writes version %d; upgrade the hub before opening this database",
					ledgerLatest, floor, knownLatest,
				),
			}
		}
		d.schema = SchemaInfo{Version: ledgerLatest, CompatibleDown: floor, Applied: applied}
		return nil
	}

	for _, migration := range migrations[len(applied):] {
		appliedAt := d.now().UnixMilli()
		err := d.Write(ctx, func(tx *Tx) error {
			for _, statement := range migration.Statements {
				if _, err := tx.Exec(statement); err != nil {
					return wrapStorageError(
						CodeUnavailable, err,
						"hub database migration %d (%s) failed", migration.Version, migration.Name,
					)
				}
			}
			if migration.After != nil {
				if err := migration.After(tx); err != nil {
					return wrapStorageError(
						CodeUnavailable, err,
						"hub database migration %d (%s) data step failed", migration.Version, migration.Name,
					)
				}
			}
			_, err := tx.Exec(
				"INSERT INTO schema_migrations (version, name, compatible_down, applied_at_ms) VALUES (?, ?, ?, ?)",
				migration.Version, migration.Name, migration.CompatibleDown, appliedAt,
			)
			return err
		})
		if err != nil {
			return err
		}
		applied = append(applied, AppliedMigration{
			Version:        migration.Version,
			Name:           migration.Name,
			CompatibleDown: migration.CompatibleDown,
			AppliedAt:      time.UnixMilli(appliedAt).UTC(),
		})
		ledgerLatest = migration.Version
		if migration.CompatibleDown > floor {
			floor = migration.CompatibleDown
		}
	}

	d.schema = SchemaInfo{Version: ledgerLatest, CompatibleDown: floor, Applied: applied}
	return nil
}

// txQueryRowExists reports whether a query returns a row.
func txQueryRowExists(tx *Tx, query string, args ...any) (bool, error) {
	var one int
	err := tx.QueryRow(query, args...).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, mapError(err)
	default:
		return true, nil
	}
}
