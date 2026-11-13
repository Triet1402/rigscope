package auth

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

// LiveAuthenticator adapts the session manager to the live/history/process
// handlers. Authentication reads the durable row (so a restart keeps browser
// sessions); revocation uses the manager's broadcaster to close the exact
// session's SSE streams and process-detail interests. A session that was ended
// before a handler could subscribe is remembered briefly so a racing request
// cannot observe a revoked cookie as live.
type LiveAuthenticator struct {
	manager *Manager
	now     func() time.Time

	mu       sync.Mutex
	watchers map[string]*sessionWatcher
	tomb     map[string]time.Time
	// tombstoneTTL bounds how long a revocation tombstone is kept. It only has
	// to cover the gap between a request reading the durable session row and
	// registering its watcher; expiry is enforced durably by the session row,
	// so a long-lived tombstone adds nothing.
	tombstoneTTL time.Duration
	// evictedThrough is the latest revocation time whose unexpired tombstone
	// was evicted to respect the hard cap. A session created at or before it
	// may have lost its tombstone, so a subscription for it verifies against
	// the durable row instead of trusting the tombstone map alone.
	evictedThrough time.Time
	// revocations counts every revocation. Authenticate reads it before the
	// durable session lookup; if it moved by the time the watcher registers,
	// a revocation may have landed in between and its tombstone may already
	// have expired, so the subscription re-reads the durable row. This keeps
	// the lookup/subscribe race closed without depending on tombstone TTL.
	revocations   uint64
	maxTombstones int
	unsub         func()

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}

	// testAfterSessionLookup runs after Authenticate resolved the durable
	// session and before it asks for the revocation channel. Tests use it to
	// interleave a logout with an authentication that has already passed the
	// durable lookup.
	testAfterSessionLookup func()
}

const (
	// maxRevocationTombstones bounds the in-memory tombstones. The cap is
	// generous: tombstones only need to cover authentications that read the
	// durable row just before a revocation, but a burst of account changes must
	// not grow the map without bound.
	maxRevocationTombstones = 4096
	maxTombstonePrune       = time.Minute
	// revocationTombstoneTTL bounds one tombstone's life. A login/logout loop
	// can therefore never accumulate tombstones beyond its request rate times
	// this window, and the hard cap is a backstop rather than a normal
	// occurrence. Expired or deleted sessions are already refused by the
	// durable row, so a tombstone never needs the full session lifetime.
	revocationTombstoneTTL = 2 * time.Minute
)

type sessionWatcher struct {
	done chan struct{}
	once sync.Once
}

// NewLiveAuthenticator builds the browser-session adapter and subscribes to
// the manager's revocation broadcaster.
func NewLiveAuthenticator(manager *Manager) *LiveAuthenticator {
	adapter := &LiveAuthenticator{
		manager:       manager,
		now:           time.Now,
		watchers:      make(map[string]*sessionWatcher),
		tomb:          make(map[string]time.Time),
		tombstoneTTL:  revocationTombstoneTTL,
		maxTombstones: maxRevocationTombstones,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	adapter.unsub = manager.Subscribe(func(revocation Revocation) {
		adapter.revoke(revocation.TokenHash)
	})
	go adapter.pruneLoop()
	return adapter
}

// Close releases the revocation subscription and stops periodic pruning.
func (a *LiveAuthenticator) Close() {
	if a.unsub != nil {
		a.unsub()
		a.unsub = nil
	}
	a.stopOnce.Do(func() { close(a.stop) })
	<-a.done
}

func (a *LiveAuthenticator) pruneLoop() {
	defer close(a.done)
	interval := a.manager.TTL() / 4
	if interval <= 0 || interval > maxTombstonePrune {
		interval = maxTombstonePrune
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-ticker.C:
			a.mu.Lock()
			a.pruneLocked(a.now())
			a.mu.Unlock()
		}
	}
}

// Authenticate implements live.Authenticator.
func (a *LiveAuthenticator) Authenticate(r *http.Request) (live.Session, error) {
	configured, err := a.manager.Configured(r.Context())
	if err != nil {
		return live.Session{}, live.ErrUnavailable
	}
	if !configured {
		return live.Session{}, live.ErrBrowserDisabled
	}
	observed := a.revocationCount()
	session, err := a.manager.Session(r)
	if err != nil {
		return live.Session{}, live.ErrUnavailable
	}
	if session == nil {
		return live.Session{}, live.ErrUnauthenticated
	}
	if a.testAfterSessionLookup != nil {
		a.testAfterSessionLookup()
	}
	return live.Session{
		ID:        session.TokenHash,
		Username:  session.Username,
		Admin:     session.Admin,
		Scope:     machineScope(session.Scope),
		CSRFToken: session.CSRFToken,
		Revoked:   a.revocation(r.Context(), session.TokenHash, session.CreatedAt, observed),
	}, nil
}

// CheckMutation implements live.CheckMutation: a mutating browser request must
// carry the exact trusted Origin and the session's CSRF token.
func (a *LiveAuthenticator) CheckMutation(r *http.Request, session live.Session) error {
	if !a.manager.Origin().Trusted(r) {
		return live.ErrMutationRejected
	}
	if !CSRFMatches(r, session.CSRFToken) {
		return live.ErrMutationRejected
	}
	return nil
}

// CheckOrigin is the pre-authentication Origin guard for streams and reads. A
// missing Origin stays valid; a present one must be the hub's exact origin.
func (a *LiveAuthenticator) CheckOrigin(r *http.Request) bool {
	if len(r.Header.Values("Origin")) == 0 {
		return true
	}
	return a.manager.Origin().Trusted(r)
}

func (a *LiveAuthenticator) revocation(ctx context.Context, hash string, createdAt time.Time, observedRevocations uint64) <-chan struct{} {
	a.mu.Lock()
	if expires, revoked := a.tomb[hash]; revoked {
		if expires.After(a.now()) {
			a.mu.Unlock()
			done := make(chan struct{})
			close(done)
			return done
		}
		delete(a.tomb, hash)
	}
	if watcher, ok := a.watchers[hash]; ok {
		a.mu.Unlock()
		return watcher.done
	}
	watcher := &sessionWatcher{done: make(chan struct{})}
	a.watchers[hash] = watcher
	// The tombstone that would fence this session may have been evicted under
	// the hard cap, or may have expired if a revocation landed between the
	// durable lookup and now. Neither may refuse an unrelated session: verify
	// against the durable row instead.
	verifyDurable := a.revocations != observedRevocations ||
		(!a.evictedThrough.IsZero() && !createdAt.After(a.evictedThrough))
	a.mu.Unlock()

	if !verifyDurable {
		return watcher.done
	}
	// Register the watcher before re-reading the row. A revocation that lands
	// after this point closes the watcher; one that landed before it deleted
	// the row, so the read sees it gone. Either way the original race is
	// closed without pre-revoking sessions created before the eviction horizon.
	active, definitive := a.sessionActive(ctx, hash)
	if active {
		return watcher.done
	}
	if !definitive {
		// The read failed for a reason other than the row being gone. Fail
		// closed for this caller only: the watcher is shared with sibling
		// streams of the same session, and one request's error or hang-up
		// must not end theirs.
		done := make(chan struct{})
		close(done)
		return done
	}
	a.mu.Lock()
	if current, ok := a.watchers[hash]; ok && current == watcher {
		delete(a.watchers, hash)
	}
	a.mu.Unlock()
	watcher.once.Do(func() { close(watcher.done) })
	return watcher.done
}

// revocationCount reports how many revocations have happened. Authenticate
// samples it before the durable lookup.
func (a *LiveAuthenticator) revocationCount() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.revocations
}

// sessionReadTimeout bounds the durable re-read in the subscription path.
const sessionReadTimeout = 5 * time.Second

// sessionActive re-reads one durable session row. definitive reports whether
// the answer is about the session itself (present and unexpired, or missing
// or expired) rather than a failed read. The read does not inherit the
// request's cancellation: the watcher it decides about is shared by every
// stream of the session, so one client hanging up must not decide for others.
func (a *LiveAuthenticator) sessionActive(ctx context.Context, hash string) (active, definitive bool) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionReadTimeout)
	defer cancel()
	stored, err := a.manager.sessions.Find(readCtx, hash)
	if err != nil {
		if storage.CodeOf(err) == storage.CodeNotFound {
			return false, true
		}
		return false, false
	}
	return stored.ExpiresAt.After(a.now()), true
}

func (a *LiveAuthenticator) revoke(hash string) {
	if hash == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.revocations++
	// Always leave a tombstone, even when a watcher already existed. An
	// authentication that read the durable row before this revocation can
	// subscribe afterwards; without the tombstone it would create a fresh open
	// channel that survives logout, revocation or eviction.
	a.tomb[hash] = now.Add(a.retention())
	if watcher, ok := a.watchers[hash]; ok {
		watcher.once.Do(func() { close(watcher.done) })
		delete(a.watchers, hash)
	}
	a.pruneLocked(now)
}

// retention is the tombstone lifetime, falling back to the package default for
// an adapter constructed without one.
func (a *LiveAuthenticator) retention() time.Duration {
	if a.tombstoneTTL > 0 {
		return a.tombstoneTTL
	}
	return revocationTombstoneTTL
}

// pruneLocked drops expired tombstones and then enforces the hard cap by
// evicting the oldest remaining entries. Evicting an unexpired tombstone opens
// a small race window; evictedThrough makes later subscriptions for sessions
// created before that revocation time re-check the durable row, so an evicted
// entry can neither revive a revoked cookie nor refuse a live bystander.
func (a *LiveAuthenticator) pruneLocked(now time.Time) {
	for key, expires := range a.tomb {
		if !expires.After(now) {
			delete(a.tomb, key)
		}
	}
	limit := a.maxTombstones
	if limit <= 0 {
		limit = maxRevocationTombstones
	}
	if len(a.tomb) <= limit {
		return
	}
	expiries := make([]time.Time, 0, len(a.tomb))
	byExpiry := make(map[time.Time][]string, len(a.tomb))
	for key, expires := range a.tomb {
		if _, ok := byExpiry[expires]; !ok {
			expiries = append(expiries, expires)
		}
		byExpiry[expires] = append(byExpiry[expires], key)
	}
	sort.Slice(expiries, func(i, j int) bool { return expiries[i].Before(expiries[j]) })
	for _, expires := range expiries {
		if len(a.tomb) <= limit {
			return
		}
		revokedAt := expires.Add(-a.retention())
		if revokedAt.After(a.evictedThrough) {
			a.evictedThrough = revokedAt
		}
		for _, key := range byExpiry[expires] {
			delete(a.tomb, key)
			if len(a.tomb) <= limit {
				return
			}
		}
	}
}

func machineScope(scope storage.Scope) api.MachineScope {
	if scope.All {
		return api.MachineScope{All: true}
	}
	return api.MachineScope{IDs: append([]string(nil), scope.MachineIDs...)}
}
