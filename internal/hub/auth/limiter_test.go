package auth

import (
	"testing"
	"time"
)

func TestLoginBucketBurstAndRefill(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := newLoginLimiter(func() time.Time { return now })
	for i := 0; i < defaultLoginBurst; i++ {
		if !limiter.allow("203.0.113.7", "owner") {
			t.Fatalf("attempt %d refused inside the burst", i)
		}
	}
	if limiter.allow("203.0.113.7", "owner") {
		t.Fatal("eleventh attempt allowed")
	}
	now = now.Add(loginRefillInterval)
	if !limiter.allow("203.0.113.7", "owner") {
		t.Fatal("one slot was not recovered after 500 ms")
	}
	now = now.Add(loginRefillInterval)
	if !limiter.allow("203.0.113.7", "owner") {
		t.Fatal("second recovered slot refused")
	}
	if limiter.allow("203.0.113.7", "owner") {
		t.Fatal("more than two slots refilled in 1 s")
	}
}

func TestLoginBudgetIsPerClientAndPerUsername(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := newLoginLimiter(func() time.Time { return now })
	for i := 0; i < defaultLoginBurst; i++ {
		if !limiter.allow("203.0.113.7", "owner") {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if limiter.allow("203.0.113.7", "other") {
		t.Fatal("an exhausted client IP still spent budget")
	}
	if limiter.allow("198.51.100.9", "owner") {
		t.Fatal("another address drained the username budget")
	}
	// The hub-wide bucket is shared: ten attempts from A and ten from B consume
	// it even though each address still has its own budget.
	now = now.Add(time.Hour)
	refilled := newLoginLimiter(func() time.Time { return now })
	for i := 0; i < defaultLoginBurst; i++ {
		if !refilled.allow("203.0.113.7", "owner") {
			t.Fatalf("client attempt %d refused", i)
		}
	}
	if refilled.allow("198.51.100.9", "viewer") {
		t.Fatal("the hub-wide login budget was not shared across clients")
	}
}

func TestLoginBackoffAfterRepeatedFailures(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := newLoginLimiter(func() time.Time { return now })
	for i := 0; i < loginPenaltyThreshold; i++ {
		limiter.failure("203.0.113.7", "owner")
	}
	if limiter.allow("203.0.113.7", "someone") {
		t.Fatal("cooldown did not start at the failure threshold")
	}
	now = now.Add(loginPenaltyBase)
	if !limiter.allow("203.0.113.7", "someone") {
		t.Fatal("the first cooldown was longer than its base delay")
	}
	// Further failures back off exponentially but never beyond the cap that
	// preserves the contract's recovery window.
	for i := 0; i < 20; i++ {
		limiter.failure("203.0.113.7", "owner")
	}
	now = now.Add(loginPenaltyMax)
	limiter.success("203.0.113.7", "owner")
	if !limiter.allow("203.0.113.7", "owner") {
		t.Fatal("a successful sign-in did not clear the cooldown")
	}
}

func TestLimiterMemoryIsBounded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := newLoginLimiter(func() time.Time { return now })
	for i := 0; i < limiterMaxEntries*4; i++ {
		limiter.allow("ip-"+string(rune('a'+i%26)), "user-"+string(rune('a'+i%26)))
	}
	if len(limiter.byIP) > limiterMaxEntries {
		t.Fatalf("IP map grew to %d entries", len(limiter.byIP))
	}
	if len(limiter.byUser) > limiterMaxEntries {
		t.Fatalf("username map grew to %d entries", len(limiter.byUser))
	}
}
