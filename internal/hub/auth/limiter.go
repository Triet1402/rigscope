package auth

import (
	"sync"
	"time"
)

// Login budget constants from the browser API contract: a hub-wide token
// bucket with a ten-attempt burst and one attempt recovered every 500 ms, and
// at most four verifications in flight.
const (
	defaultLoginBurst     = 10
	loginRefillInterval   = 500 * time.Millisecond
	maxConcurrentLogins   = 4
	limiterMaxEntries     = 512
	limiterIdleTTL        = 10 * time.Minute
	loginPenaltyThreshold = 20
	loginPenaltyBase      = 100 * time.Millisecond
	loginPenaltyMax       = time.Second
)

// loginLimiter bounds login attempts three ways: a hub-wide bucket (the
// contract's budget), a per-client-IP bucket and a per-username bucket, plus
// an exponential cooldown after repeated credential failures. All state is
// bounded and pruned, so a spoofed address or username cannot grow it without
// limit.
type loginLimiter struct {
	mu       sync.Mutex
	now      func() time.Time
	global   tokenBucket
	byIP     map[string]*loginEntry
	byUser   map[string]*loginEntry
	maxEntry int
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

type loginEntry struct {
	bucket       tokenBucket
	failures     int
	blockedUntil time.Time
	lastSeen     time.Time
}

func newLoginLimiter(now func() time.Time) *loginLimiter {
	return &loginLimiter{
		now:      now,
		global:   tokenBucket{tokens: defaultLoginBurst},
		byIP:     make(map[string]*loginEntry),
		byUser:   make(map[string]*loginEntry),
		maxEntry: limiterMaxEntries,
	}
}

// allow consumes one attempt from every bucket. A blocked address or username
// is refused without touching the hub-wide budget.
func (l *loginLimiter) allow(ip, username string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ip, username = limitKey(ip), limitKey(username)
	l.refill(&l.global, now)
	ipEntry := l.entry(l.byIP, ip, now)
	userEntry := l.entry(l.byUser, username, now)
	if now.Before(ipEntry.blockedUntil) || now.Before(userEntry.blockedUntil) {
		return false
	}
	if !l.take(&l.global, now) || !l.take(&ipEntry.bucket, now) || !l.take(&userEntry.bucket, now) {
		return false
	}
	ipEntry.lastSeen = now
	userEntry.lastSeen = now
	return true
}

// failure records a rejected credential attempt and arms the exponential
// cooldown once the threshold is crossed. The cooldown is capped at one second
// so the contract's recovery window is preserved.
func (l *loginLimiter) failure(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ip, username = limitKey(ip), limitKey(username)
	for _, pair := range []struct {
		table map[string]*loginEntry
		key   string
	}{{l.byIP, ip}, {l.byUser, username}} {
		entry := l.entry(pair.table, pair.key, now)
		entry.failures++
		entry.lastSeen = now
		if entry.failures >= loginPenaltyThreshold {
			delay := loginPenaltyBase << minInt(entry.failures-loginPenaltyThreshold, 6)
			if delay > loginPenaltyMax {
				delay = loginPenaltyMax
			}
			entry.blockedUntil = now.Add(delay)
		}
	}
}

// success clears a cooldown after a valid sign-in.
func (l *loginLimiter) success(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ip, username = limitKey(ip), limitKey(username)
	for _, pair := range []struct {
		table map[string]*loginEntry
		key   string
	}{{l.byIP, ip}, {l.byUser, username}} {
		entry := l.entry(pair.table, pair.key, now)
		entry.failures = 0
		entry.blockedUntil = time.Time{}
		entry.lastSeen = now
	}
}

// limitKey keeps a caller-supplied key inside the identifier bound so the
// maps cannot grow per byte of an oversized probe.
func limitKey(value string) string {
	if len(value) > 128 {
		return value[:128]
	}
	return value
}

func (l *loginLimiter) entry(table map[string]*loginEntry, key string, now time.Time) *loginEntry {
	if entry, ok := table[key]; ok {
		return entry
	}
	if len(table) >= l.maxEntry {
		l.prune(table, now, true)
	}
	entry := &loginEntry{
		bucket:   tokenBucket{tokens: defaultLoginBurst, last: now},
		lastSeen: now,
	}
	table[key] = entry
	return entry
}

func (l *loginLimiter) prune(table map[string]*loginEntry, now time.Time, force bool) {
	before := len(table)
	for key, entry := range table {
		if now.Sub(entry.lastSeen) > limiterIdleTTL {
			delete(table, key)
		}
	}
	if !force || len(table) < before {
		return
	}
	// Still full of recently active entries: drop the oldest one so memory has
	// a hard ceiling.
	var oldestKey string
	var oldest time.Time
	for key, entry := range table {
		if oldestKey == "" || entry.lastSeen.Before(oldest) {
			oldestKey, oldest = key, entry.lastSeen
		}
	}
	if oldestKey != "" {
		delete(table, oldestKey)
	}
}

func (l *loginLimiter) refill(bucket *tokenBucket, now time.Time) {
	if bucket.last.IsZero() {
		bucket.tokens = defaultLoginBurst
		bucket.last = now
		return
	}
	if now.After(bucket.last) {
		bucket.tokens += float64(now.Sub(bucket.last)) / float64(loginRefillInterval)
		if bucket.tokens > defaultLoginBurst {
			bucket.tokens = defaultLoginBurst
		}
		bucket.last = now
	}
}

func (l *loginLimiter) take(bucket *tokenBucket, now time.Time) bool {
	l.refill(bucket, now)
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
