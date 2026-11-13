package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// authorizeAdminTx confirms, inside the caller's write transaction, that actor
// is an existing, enabled administrator. It is the durable half of the admin
// handler's authorization: a demotion, disable or delete that commits between
// the request's earlier check and this transaction makes the mutation fail
// instead of committing on stale privilege.
func authorizeAdminTx(tx *Tx, actor string) error {
	if !identifierPattern.MatchString(actor) {
		return forbiddenError("administrative access required")
	}
	var admin, disabled int64
	err := tx.QueryRow("SELECT is_admin, disabled FROM users WHERE username = ?", actor).Scan(&admin, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return forbiddenError("administrative access required")
	}
	if err != nil {
		return mapError(err)
	}
	if !intToBool(admin) || intToBool(disabled) {
		return forbiddenError("administrative access required")
	}
	return nil
}

// SessionAuthority identifies the browser session that originated a
// privileged mutation. Callers pass it into the write transaction so the
// session's durable row is re-checked at commit time, not only when the request
// started.
type SessionAuthority struct {
	Actor     string
	TokenHash string
}

// ErrScopeForbidden reports that the originating session's current machine
// scope does not include the mutation target. Repository writes return it so a
// scope change that commits while a request body is being read is refused with
// 403 instead of applying the mutation to a machine the account lost access to.
var ErrScopeForbidden = errors.New("storage: machine scope forbidden")

// authorizeSessionTx is authorizeAdminTx plus the originating browser session.
// It confirms inside the caller's write transaction that the session row still
// exists, is unexpired, and still snaps the account's current version,
// incarnation, admin flag and scope. A request whose session was revoked, or
// whose account changed, while its body was being read therefore cannot commit
// a mutation on stale privilege or machine scope. It returns the account's
// current scope so the caller can re-check a machine target.
func authorizeSessionTx(tx *Tx, actor, tokenHash string, now time.Time) (Scope, error) {
	if !identifierPattern.MatchString(actor) {
		return Scope{}, forbiddenError("administrative access required")
	}
	if tokenHash == "" {
		return Scope{}, unauthenticatedError("authentication required")
	}
	var (
		admin, disabled int64
		version         int64
		incarnation     string
		accountScope    string
	)
	err := tx.QueryRow(
		"SELECT is_admin, disabled, version, incarnation, scope_json FROM users WHERE username = ?",
		actor,
	).Scan(&admin, &disabled, &version, &incarnation, &accountScope)
	if errors.Is(err, sql.ErrNoRows) {
		return Scope{}, forbiddenError("administrative access required")
	}
	if err != nil {
		return Scope{}, mapError(err)
	}
	if !intToBool(admin) || intToBool(disabled) {
		return Scope{}, forbiddenError("administrative access required")
	}
	var (
		sessionUser                  string
		sessionScope                 string
		sessionIncarnation           string
		sessionAdmin, sessionVersion int64
		sessionExpiresAtMS           int64
	)
	err = tx.QueryRow(
		"SELECT username, scope_json, is_admin, user_version, user_incarnation, expires_at_ms "+
			"FROM browser_sessions WHERE token_hash = ?",
		tokenHash,
	).Scan(&sessionUser, &sessionScope, &sessionAdmin, &sessionVersion, &sessionIncarnation, &sessionExpiresAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return Scope{}, unauthenticatedError("authentication required")
	}
	if err != nil {
		return Scope{}, mapError(err)
	}
	if sessionExpiresAtMS <= now.UnixMilli() {
		return Scope{}, unauthenticatedError("authentication required")
	}
	if sessionUser != actor || sessionAdmin != admin || sessionVersion != version ||
		sessionIncarnation != incarnation || sessionScope != accountScope {
		return Scope{}, forbiddenError("administrative access required")
	}
	scope, err := parseScope(accountScope)
	if err != nil {
		return Scope{}, err
	}
	return scope, nil
}

// AuthorizeSessionTx is the cross-package form of authorizeSessionTx. The
// alert engine uses it so its writes re-check the originating session inside
// the same transaction that persists the change.
func AuthorizeSessionTx(tx *Tx, authority SessionAuthority, now time.Time) (Scope, error) {
	return authorizeSessionTx(tx, authority.Actor, authority.TokenHash, now)
}

// Authorize runs fn inside one write transaction after re-checking that actor
// is an existing, enabled administrator. Every privileged mutation (users,
// collectors, grants, machine metadata) uses it so the privilege check and the
// durable change commit or roll back together.
func (s *Users) Authorize(ctx context.Context, actor string, fn func(*Tx) error) error {
	return s.db.Write(ctx, func(tx *Tx) error {
		if err := authorizeAdminTx(tx, actor); err != nil {
			return err
		}
		return fn(tx)
	})
}

// AuthorizeSession is Authorize plus validation of the originating browser
// session inside the same transaction. Admin HTTP routes use it so a session
// revoked between the request's earlier lookup and the write cannot commit a
// pending mutation.
func (s *Users) AuthorizeSession(ctx context.Context, actor, tokenHash string, fn func(*Tx) error) error {
	return s.db.Write(ctx, func(tx *Tx) error {
		if _, err := authorizeSessionTx(tx, actor, tokenHash, s.now()); err != nil {
			return err
		}
		return fn(tx)
	})
}

// MutateAccount is Authorize plus revocation: after fn succeeds, every session
// of target is deleted inside the same transaction and returned. The caller
// broadcasts the returned rows after the commit, so a failed revocation can
// never leave a session valid for a changed account.
func (s *Users) MutateAccount(ctx context.Context, actor, target string, fn func(*Tx) error) ([]Session, error) {
	return s.mutateAccount(ctx, target, func(tx *Tx) error {
		return authorizeAdminTx(tx, actor)
	}, fn)
}

// MutateAccountSession is MutateAccount with the originating session validated
// in the same transaction. The admin user routes use it so a revoked or
// account-changed session cannot complete a delayed account mutation.
func (s *Users) MutateAccountSession(ctx context.Context, actor, tokenHash, target string, fn func(*Tx) error) ([]Session, error) {
	return s.mutateAccount(ctx, target, func(tx *Tx) error {
		_, err := authorizeSessionTx(tx, actor, tokenHash, s.now())
		return err
	}, fn)
}

func (s *Users) mutateAccount(ctx context.Context, target string, authorize func(*Tx) error, fn func(*Tx) error) ([]Session, error) {
	if err := validateUsername(target); err != nil {
		return nil, err
	}
	var ended []Session
	err := s.db.Write(ctx, func(tx *Tx) error {
		if err := authorize(tx); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		sessions, err := deleteUserSessionsTx(tx, target)
		if err != nil {
			return err
		}
		ended = sessions
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ended, nil
}

// ResetPassword replaces one account's opaque hash and deletes every session
// of that account in a single transaction, returning the ended rows. It is the
// offline recovery path: the hub holds its exclusive state lock, so there is
// no acting administrator to re-check.
func (s *Users) ResetPassword(ctx context.Context, actor, username, hash string) ([]Session, error) {
	if err := validateUsername(username); err != nil {
		return nil, err
	}
	var ended []Session
	err := s.db.Write(ctx, func(tx *Tx) error {
		if err := s.SetPasswordHashTx(tx, actor, username, hash); err != nil {
			return err
		}
		sessions, err := deleteUserSessionsTx(tx, username)
		if err != nil {
			return err
		}
		ended = sessions
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ended, nil
}
