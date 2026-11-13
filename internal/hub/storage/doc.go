// Package storage owns the hub's durable SQLite state and its bounded
// telemetry history.
//
// It is the only package that knows the hub schema. Callers get small
// repositories over one open Database:
//
//   - Users is the browser account directory. Password hashing and password
//     verification belong to the auth package; storage keeps the opaque hash
//     string it is given. The repository enforces the account invariants that
//     outlive any one request: at most MaxUsers accounts, a unique username,
//     and the last active administrator can never be demoted, disabled or
//     deleted.
//   - Sessions is the durable browser sign-in table. The auth package owns
//     token generation; storage keys sessions by the caller's token hash and
//     never stores a raw session token. Each row snapshots the account's scope,
//     admin flag and version at sign-in; the table trim, the insert and the
//     account-version comparison run in one write transaction, and lookup
//     rejects a row whose account was deleted, disabled or changed since.
//     Removing sessions to keep the bound or to honour an account change
//     returns the removed rows so the caller can broadcast the end.
//   - Registry is collector enrollment: short-lived single-use grants,
//     issued collector credentials, lost-enrollment-response recovery, machine
//     mapping and revocation. Grant tokens and issued collector credentials are
//     stored only as SHA-256 hashes and compared in constant time. A lost
//     enrollment response stays replayable through a bounded AES-GCM copy of
//     the credential sealed with a key derived (HKDF-SHA256) from the
//     collector's high-entropy recovery secret; the copy is deleted after the
//     first successful authenticated connection or when the recovery window
//     closes.
//   - Machines is hub-local presentation metadata (display name and tags).
//   - Settings is a small typed key/value table for one-time markers and hub
//     preferences.
//   - Audit is a bounded operation log (newest 1000 rows per scope) that
//     never carries secrets or request bodies.
//   - Alerts owns alert rules, incidents and the single webhook channel.
//     The evaluation and delivery engine is a later package; this layer only
//     validates and stores its state.
//   - Telemetry owns summary history plus the in-memory 1-second ring. The
//     hub defaults to a separate bounded RAM database; persistent mode uses
//     the durable control database. Both modes store one compact
//     10-second summary per series and bucket, folded into one-minute and
//     one-hour rollups that preserve min/max/sum/count/null_count. A bounded
//     per-machine ring keeps the most recent seconds at full resolution, so a
//     chart can show sub-10-second detail without writing raw rows.
//
// Durability and bounds:
//
// Samples are acknowledged only after acceptance in the selected database.
// Durable mode runs in WAL with synchronous=FULL (a commit fsyncs before
// returning); RAM mode advertises volatile acceptance. Both use a
// bounded busy timeout. Writes are serialized through one writer connection;
// reads use a separate pool, so a slow history query cannot block ingestion.
// Open fails closed when the database file is not private (0600 in a 0700
// directory) or was written by a newer schema.
//
// Storage work is bounded everywhere: retention walks a persisted series
// cursor in small pages with a short per-pass time budget and a small deletion
// cap, queries share one scanned-row budget and one deadline, reads have a
// bounded lifetime, the in-memory series catalog is a bounded LRU, and the
// flush queue is bounded. Every writer, not only telemetry ingestion, is
// admitted against a finite main-file page cap plus a WAL reserve, so disk
// exhaustion is an explicit typed busy/degraded error instead of silent loss.
// The newest hour of history is never deleted to make a too-small budget fit.
//
// Open applies ordered, irreversible migrations from empty. It does not import
// 0.2 data: 1.0 installations start from the baseline schema (version 1).
//
// Compatibility is a floor, not a ceiling: each migration records
// CompatibleDown, the lowest schema version whose binary can safely open and
// write the database after it. An additive N→N+1 migration declares N; a
// breaking one declares N+1. Schema().CompatibleDown is the maximum over the
// applied ledger, so a rollback to a binary that writes schema P is safe
// exactly when P is at least that floor. A binary whose own schema S is lower
// than the ledger's latest version may open the file when S is at or above the
// floor; it runs no unknown migrations and writes only the subset it knows.
// Otherwise Open refuses with an error naming the required schema version.
//
// It does not own HTTP handlers, authentication policy, alert evaluation,
// webhook delivery, exports or the collector session registry. It never logs
// credentials or audit payloads.
package storage
