package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/hub/storage"
)

// DefaultSessionTTL is the fixed browser session lifetime (12 hours).
const DefaultSessionTTL = 12 * time.Hour

const (
	defaultSweepInterval = time.Second
	bootstrapUsername    = "owner"
)

// Reason explains why a session ended.
type Reason string

// Session end reasons. Every end goes through the same broadcaster so SSE
// streams, development upgrades and process-detail interests stop promptly.
const (
	ReasonLogout  Reason = "logout"
	ReasonExpired Reason = "expired"
	ReasonRevoked Reason = "revoked"
)

// Revocation is one ended session. TokenHash is the value the session table
// stores; only the hash ever leaves this package.
type Revocation struct {
	Username  string
	TokenHash string
	Reason    Reason
	At        time.Time
}

// Session is one live browser sign-in. Scope and Admin are snapshots taken at
// sign-in; authorization-sensitive routes re-check the directory. The durable
// table is keyed by the token hash and survives a hub restart until the
// session's fixed expiry. Logout, account changes and eviction delete the row
// and broadcast the revocation, so a restarted hub neither signs browsers out
// nor resurrects an ended session.
type Session struct {
	TokenHash string
	Username  string
	CSRFToken string
	Scope     storage.Scope
	Admin     bool
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Options configures the authentication manager.
type Options struct {
	Users    *storage.Users
	Sessions *storage.Sessions
	// Origin is the trusted-origin rule set for mutations. Required.
	Origin OriginChecker
	// TTL is the fixed session lifetime. Zero means DefaultSessionTTL.
	TTL time.Duration
	// Now is the clock. Defaults to time.Now.
	Now func() time.Time
	// Secure reports whether the request uses HTTPS (which selects the
	// __Host- cookie and the Secure attribute). Defaults to r.TLS != nil; the
	// hub composition root passes the trusted-proxy-aware scheme instead.
	Secure func(*http.Request) bool
	// VerifyPassword is injected by tests; production uses VerifyPassword.
	VerifyPassword func(password, encoded string) (bool, error)
	// SweepInterval is the expiry sweep cadence. Zero means one second.
	SweepInterval time.Duration
	Logger        *slog.Logger
}

// Manager owns password verification, login budgets, session issuance and the
// revocation broadcaster.
type Manager struct {
	users    *storage.Users
	sessions *storage.Sessions
	origin   OriginChecker
	ttl      time.Duration
	now      func() time.Time
	secure   func(*http.Request) bool
	verify   func(string, string) (bool, error)
	logger   *slog.Logger

	dummyHash string
	limiter   *loginLimiter

	// sessionsMu serializes every durable session transition with its
	// in-memory index update, so a concurrent create cannot be evicted by a
	// trim that this process has not seen yet. It is always taken before mu.
	sessionsMu sync.Mutex

	mu       sync.Mutex
	active   map[string]storage.Session
	subs     map[uint64]func(Revocation)
	nextSub  uint64
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// New prepares the manager and starts its expiry sweeper.
func New(opts Options) (*Manager, error) {
	if opts.Users == nil {
		return nil, errors.New("auth: user directory is required")
	}
	if opts.Sessions == nil {
		return nil, errors.New("auth: session store is required")
	}
	if opts.Origin == nil {
		return nil, errors.New("auth: origin rules are required")
	}
	ttl := opts.TTL
	if ttl == 0 {
		ttl = DefaultSessionTTL
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("auth: session ttl %s must be positive", ttl)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	secure := opts.Secure
	if secure == nil {
		secure = func(r *http.Request) bool { return r != nil && r.TLS != nil }
	}
	verify := opts.VerifyPassword
	if verify == nil {
		verify = VerifyPassword
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// A fixed dummy hash makes an unknown username cost exactly one real
	// verification without reusing a user's stored salt.
	dummyPassword := make([]byte, 32)
	if _, err := rand.Read(dummyPassword); err != nil {
		return nil, fmt.Errorf("auth: generate probe secret: %w", err)
	}
	dummyHash, err := HashPassword(base64.RawURLEncoding.EncodeToString(dummyPassword))
	if err != nil {
		return nil, fmt.Errorf("auth: initialise password probe: %w", err)
	}
	sweep := opts.SweepInterval
	if sweep == 0 {
		sweep = defaultSweepInterval
	}
	manager := &Manager{
		users:     opts.Users,
		sessions:  opts.Sessions,
		origin:    opts.Origin,
		ttl:       ttl,
		now:       now,
		secure:    secure,
		verify:    verify,
		logger:    logger,
		dummyHash: dummyHash,
		limiter:   newLoginLimiter(now),
		active:    make(map[string]storage.Session),
		subs:      make(map[uint64]func(Revocation)),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	if err := manager.loadSessions(); err != nil {
		return nil, err
	}
	go manager.sweepLoop(sweep)
	return manager, nil
}

// loadSessions adopts the durable session table into this process. Only a
// session issued by this hub process is tracked in memory, but the issue
// decision lives in SQLite: a restart re-adopts every unexpired row so the
// browser stays signed in, and the sweeper/revocation broadcaster still covers
// it. Expired rows are deleted instead of adopted.
func (m *Manager) loadSessions() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, err := m.sessions.List(ctx)
	if err != nil {
		return fmt.Errorf("auth: load sessions: %w", err)
	}
	now := m.now()
	for _, session := range stored {
		if session.ExpiresAt.After(now) {
			m.active[session.TokenHash] = session
			continue
		}
		if _, err := m.sessions.Delete(ctx, session.TokenHash); err != nil && storage.CodeOf(err) != storage.CodeNotFound {
			return fmt.Errorf("auth: drop expired session: %w", err)
		}
	}
	return nil
}

// Close stops the expiry sweeper. The session table is left intact.
func (m *Manager) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	<-m.done
	m.mu.Lock()
	m.subs = map[uint64]func(Revocation){}
	m.active = map[string]storage.Session{}
	m.mu.Unlock()
	return nil
}

// TTL reports the fixed session lifetime.
func (m *Manager) TTL() time.Duration { return m.ttl }

// Origin exposes the trusted-origin rules to the admin handler.
func (m *Manager) Origin() OriginChecker { return m.origin }

// Secure reports whether the request uses HTTPS, which selects the remote
// cookie and its Secure attribute.
func (m *Manager) Secure(r *http.Request) bool { return m.secure(r) }

// Configured reports whether any enabled browser account exists. Login,
// session and every browser data path fail closed when it returns false.
func (m *Manager) Configured(ctx context.Context) (bool, error) {
	users, err := m.users.Active(ctx)
	if err != nil {
		return false, err
	}
	return len(users) > 0, nil
}

// IsAdmin re-checks the durable directory on every authorization decision; a
// session snapshot is presentation state, never a grant.
func (m *Manager) IsAdmin(ctx context.Context, username string) (bool, error) {
	return m.users.IsAdmin(ctx, username)
}

// Subscribe registers a revocation listener and returns a cancel function.
// Listeners are called synchronously outside the manager lock; a listener
// panic is contained. Slow listeners must not block, so consumers should hand
// work to their own queue.
func (m *Manager) Subscribe(listener func(Revocation)) func() {
	m.mu.Lock()
	id := m.nextSub
	m.nextSub++
	m.subs[id] = listener
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.subs, id)
		m.mu.Unlock()
	}
}

// Session resolves the browser session carried by the request cookie.
func (m *Manager) Session(r *http.Request) (*Session, error) {
	if r == nil {
		return nil, nil
	}
	secure := m.secure(r)
	cookie, err := r.Cookie(CookieName(secure))
	if err != nil || cookie.Value == "" {
		return nil, nil
	}
	return m.sessionForToken(r.Context(), cookie.Value)
}

func (m *Manager) sessionForToken(ctx context.Context, token string) (*Session, error) {
	hash := HashToken(token)
	stored, err := m.sessions.Find(ctx, hash)
	if err != nil {
		if storage.CodeOf(err) == storage.CodeNotFound {
			m.forgetExpired(hash)
			return nil, nil
		}
		return nil, err
	}
	if !stored.ExpiresAt.After(m.now()) {
		_, _ = m.sessions.Delete(ctx, hash)
		m.forgetExpired(hash)
		return nil, nil
	}
	// The row is the source of truth. A restart adopted every unexpired row,
	// and logout, account changes and eviction delete it before broadcasting,
	// so a valid row here is a live session for this hub.
	session := &Session{
		TokenHash: stored.TokenHash,
		Username:  stored.Username,
		CSRFToken: stored.CSRFToken,
		Scope:     stored.Scope,
		Admin:     stored.Admin,
		CreatedAt: stored.CreatedAt,
		ExpiresAt: stored.ExpiresAt,
	}
	return session, nil
}

// Login verifies credentials and issues a session. It returns ok=false for
// every credential failure so callers answer with one generic message.
func (m *Manager) Login(ctx context.Context, ip string, username, password string) (session *Session, token string, ok bool, err error) {
	if !m.limiter.allow(ip, username) {
		return nil, "", false, ErrRateLimited
	}
	user, getErr := m.users.Get(ctx, username)
	if getErr != nil {
		if code := storage.CodeOf(getErr); code != storage.CodeNotFound && code != storage.CodeInvalid {
			return nil, "", false, getErr
		}
	}
	known := getErr == nil && !user.Disabled
	encoded := m.dummyHash
	if known {
		encoded = user.PasswordHash
	}
	valid, verifyErr := m.verify(password, encoded)
	if verifyErr != nil || !known || !valid {
		m.limiter.failure(ip, username)
		return nil, "", false, nil
	}
	// Password verification stays outside the write transaction. The insert
	// re-reads the account inside BEGIN IMMEDIATE and compares user.Version, so
	// a password, scope, admin or enabled change that commits first makes this
	// attempt fail instead of minting a stale session.
	session, token, err = m.issueSession(ctx, user)
	if err != nil {
		if errors.Is(err, storage.ErrAccountChanged) {
			m.limiter.failure(ip, username)
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	m.limiter.success(ip, username)
	return session, token, true, nil
}

// ErrRateLimited asks the handler for a 429 without revealing credentials.
var ErrRateLimited = errors.New("auth: login rate limit exceeded")

func (m *Manager) issueSession(ctx context.Context, user storage.User) (*Session, string, error) {
	token, err := randomToken()
	if err != nil {
		return nil, "", err
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, "", err
	}
	now := m.now()
	stored := storage.Session{
		TokenHash:       HashToken(token),
		Username:        user.Username,
		CSRFToken:       csrf,
		UserIncarnation: user.Incarnation,
		CreatedAt:       now,
		ExpiresAt:       now.Add(m.ttl),
	}
	// The durable trim and the in-memory index move under one lock, so a
	// concurrent creation cannot evict a row this transition just adopted.
	m.sessionsMu.Lock()
	session, removed, err := m.sessions.CreateForUser(ctx, stored, user.Version)
	if err != nil {
		m.sessionsMu.Unlock()
		return nil, "", err
	}
	m.mu.Lock()
	for _, row := range removed {
		delete(m.active, row.TokenHash)
	}
	m.active[session.TokenHash] = session
	m.mu.Unlock()
	m.sessionsMu.Unlock()
	m.notifyExpired(removed)
	return &Session{
		TokenHash: session.TokenHash,
		Username:  session.Username,
		CSRFToken: session.CSRFToken,
		Scope:     session.Scope,
		Admin:     session.Admin,
		CreatedAt: session.CreatedAt,
		ExpiresAt: session.ExpiresAt,
	}, token, nil
}

// notifyExpired broadcasts rows that the durable session table already removed
// to honour expiry or the MaxSessions bound.
func (m *Manager) notifyExpired(sessions []storage.Session) {
	now := m.now()
	for _, session := range sessions {
		m.notify(Revocation{Username: session.Username, TokenHash: session.TokenHash, Reason: ReasonExpired, At: now})
	}
}

// Logout ends one session and broadcasts the end.
func (m *Manager) Logout(ctx context.Context, session *Session) error {
	if session == nil {
		return nil
	}
	m.sessionsMu.Lock()
	if _, err := m.sessions.Delete(ctx, session.TokenHash); err != nil {
		m.sessionsMu.Unlock()
		return err
	}
	m.mu.Lock()
	delete(m.active, session.TokenHash)
	m.mu.Unlock()
	m.sessionsMu.Unlock()
	m.notify(Revocation{Username: session.Username, TokenHash: session.TokenHash, Reason: ReasonLogout, At: m.now()})
	return nil
}

// RevokeUser ends every session of one username. Account changes use
// EndSessions with the rows their own transaction already deleted; this method
// remains for callers that revoke outside an account mutation.
func (m *Manager) RevokeUser(ctx context.Context, username string) (int, error) {
	if username == "" {
		return 0, nil
	}
	m.sessionsMu.Lock()
	ended, err := m.sessions.DeleteUserRows(ctx, username)
	if err != nil {
		m.sessionsMu.Unlock()
		return 0, err
	}
	m.mu.Lock()
	for _, row := range ended {
		delete(m.active, row.TokenHash)
	}
	m.mu.Unlock()
	m.sessionsMu.Unlock()
	now := m.now()
	for _, row := range ended {
		m.notify(Revocation{Username: row.Username, TokenHash: row.TokenHash, Reason: ReasonRevoked, At: now})
	}
	return len(ended), nil
}

// EndSessions drops already-deleted session rows from the in-memory index and
// broadcasts their revocation. Account mutations delete the rows inside their
// own transaction (so a committed account change can never leave a session
// valid) and call this after commit. It needs no context: only memory is
// touched, so a cancelled request cannot suppress the notification and no
// second write can fail.
func (m *Manager) EndSessions(sessions []storage.Session) {
	if len(sessions) == 0 {
		return
	}
	m.sessionsMu.Lock()
	m.mu.Lock()
	ended := make([]storage.Session, 0, len(sessions))
	for _, row := range sessions {
		if cached, ok := m.active[row.TokenHash]; ok {
			row = cached
		}
		delete(m.active, row.TokenHash)
		ended = append(ended, row)
	}
	m.mu.Unlock()
	m.sessionsMu.Unlock()
	now := m.now()
	for _, row := range ended {
		m.notify(Revocation{Username: row.Username, TokenHash: row.TokenHash, Reason: ReasonRevoked, At: now})
	}
}

func (m *Manager) forgetExpired(hash string) {
	m.mu.Lock()
	session, ok := m.active[hash]
	delete(m.active, hash)
	m.mu.Unlock()
	if ok {
		m.notify(Revocation{Username: session.Username, TokenHash: hash, Reason: ReasonExpired, At: m.now()})
	}
}

func (m *Manager) notify(revocation Revocation) {
	m.mu.Lock()
	listeners := make([]func(Revocation), 0, len(m.subs))
	for _, listener := range m.subs {
		listeners = append(listeners, listener)
	}
	m.mu.Unlock()
	for _, listener := range listeners {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					m.logger.Error("session revocation listener panicked", "panic", recovered)
				}
			}()
			listener(revocation)
		}()
	}
}

func (m *Manager) sweepLoop(interval time.Duration) {
	defer close(m.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.sweepExpired()
		}
	}
}

// sweepExpired ends expired sessions from the in-memory index first, then
// removes their rows in bounded batches. Streams must not wait behind SQLite
// cleanup: the index has already dropped the session, so the notification is
// the end of record and the durable delete can retry on the next pass.
func (m *Manager) sweepExpired() {
	now := m.now()
	m.mu.Lock()
	expired := make([]storage.Session, 0)
	for hash, session := range m.active {
		if !session.ExpiresAt.After(now) {
			expired = append(expired, session)
			delete(m.active, hash)
		}
	}
	m.mu.Unlock()
	if len(expired) == 0 {
		return
	}
	for _, session := range expired {
		m.notify(Revocation{Username: session.Username, TokenHash: session.TokenHash, Reason: ReasonExpired, At: now})
	}
	hashes := make([]string, len(expired))
	for index, session := range expired {
		hashes[index] = session.TokenHash
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.sessions.DeleteMany(ctx, hashes); err != nil {
		m.logger.Warn("delete expired sessions", "error", err)
	}
}

// HashToken derives the stored form of a session token. Only this value is
// persisted; the plaintext cookie token lives only in the browser.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// BootstrapOptions configures the first-start administrator.
type BootstrapOptions struct {
	// Username defaults to "owner".
	Username string
	// Scope defaults to every machine.
	Scope storage.Scope
}

// Bootstrap creates the owner administrator once, when the user table is
// empty and no earlier bootstrap has been recorded. It returns the generated
// password when it created the account, and "" when it did nothing. Callers
// print the password exactly once and never store it.
func Bootstrap(ctx context.Context, users *storage.Users, opts BootstrapOptions) (string, error) {
	if users == nil {
		return "", errors.New("auth: user directory is required")
	}
	existing, err := users.List(ctx)
	if err != nil {
		return "", err
	}
	if len(existing) != 0 {
		return "", nil
	}
	username := opts.Username
	if username == "" {
		username = bootstrapUsername
	}
	scope := opts.Scope
	if !scope.All && len(scope.MachineIDs) == 0 {
		scope = storage.AllMachines()
	}
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate bootstrap password: %w", err)
	}
	password := base64.RawURLEncoding.EncodeToString(raw[:])
	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	created, err := users.BootstrapUsers(ctx, []storage.BootstrapUser{{
		Username:     username,
		PasswordHash: hash,
		Scope:        scope,
		Admin:        true,
	}}, nil)
	if err != nil {
		return "", err
	}
	if !created {
		return "", nil
	}
	return password, nil
}
