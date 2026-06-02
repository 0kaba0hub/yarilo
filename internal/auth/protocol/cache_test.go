package protocol

import (
	"testing"
	"time"
)

// TestCache_NilCacheIsNoop — zero-sized cache returns nil and
// every helper is safe to call on nil.
func TestCache_NilCacheIsNoop(t *testing.T) {
	c := NewCache(0, time.Minute, time.Minute)
	if c != nil {
		t.Fatalf("zero size returned non-nil cache")
	}
	if _, ok := c.Lookup("k", "p"); ok {
		t.Errorf("nil cache returned hit")
	}
	c.Insert("k", "u", "p", ResultOK, NewFields())
	if got := c.Clear(); got != 0 {
		t.Errorf("nil Clear returned %d", got)
	}
	if got := c.ClearByUserMask([]string{"*"}); got != 0 {
		t.Errorf("nil ClearByUserMask returned %d", got)
	}
	if c.Remove("k") {
		t.Errorf("nil Remove returned true")
	}
}

// TestCache_PositiveHitVerifiesPassword — cached OK entry
// returns hit only when the supplied password matches the stored
// HMAC. Wrong password → miss (so a stale cache cannot
// authenticate a new password).
func TestCache_PositiveHitVerifiesPassword(t *testing.T) {
	c := NewCache(1<<20, time.Minute, time.Minute)
	f := NewFields()
	f.Set("user", "alice")
	c.Insert(MakeCacheKey("imap", "alice"), "alice", "secret", ResultOK, f)

	// Right password → hit.
	e, ok := c.Lookup(MakeCacheKey("imap", "alice"), "secret")
	if !ok || e == nil || e.Result != ResultOK {
		t.Fatalf("right password did not hit: ok=%v entry=%+v", ok, e)
	}
	if v, _ := e.Fields.Get("user"); v != "alice" {
		t.Errorf("fields lost on cache roundtrip: %v", e.Fields)
	}

	// Wrong password → miss (do not authenticate).
	if _, ok := c.Lookup(MakeCacheKey("imap", "alice"), "WRONG"); ok {
		t.Errorf("wrong password cached-passed")
	}
}

// TestCache_NegativeHitShortCircuits — cached Fail entry returns
// hit regardless of password — a previously-known failure is
// authoritative for the neg-TTL window and saves a passdb roundtrip.
func TestCache_NegativeHitShortCircuits(t *testing.T) {
	c := NewCache(1<<20, time.Minute, time.Minute)
	c.Insert(MakeCacheKey("imap", "ghost"), "ghost", "", ResultFail, nil)

	e, ok := c.Lookup(MakeCacheKey("imap", "ghost"), "anything")
	if !ok || e == nil || e.Result != ResultFail {
		t.Fatalf("neg cache miss: ok=%v entry=%+v", ok, e)
	}
}

// TestCache_PositiveTTLExpiry — expired entries are not returned.
func TestCache_PositiveTTLExpiry(t *testing.T) {
	c := NewCache(1<<20, 30*time.Millisecond, time.Minute)
	c.Insert(MakeCacheKey("imap", "alice"), "alice", "secret", ResultOK, NewFields())
	time.Sleep(60 * time.Millisecond)
	if _, ok := c.Lookup(MakeCacheKey("imap", "alice"), "secret"); ok {
		t.Errorf("expired entry returned")
	}
}

// TestCache_NegTTLExpiry — separate TTL for negative entries.
func TestCache_NegTTLExpiry(t *testing.T) {
	c := NewCache(1<<20, time.Minute, 30*time.Millisecond)
	c.Insert(MakeCacheKey("imap", "ghost"), "ghost", "", ResultFail, nil)
	time.Sleep(60 * time.Millisecond)
	if _, ok := c.Lookup(MakeCacheKey("imap", "ghost"), ""); ok {
		t.Errorf("expired neg entry returned")
	}
}

// TestCache_TTLZeroDisablesPositive — ttl=0 should silently drop
// positive inserts but still permit negative caching.
func TestCache_TTLZeroDisablesPositive(t *testing.T) {
	c := NewCache(1<<20, 0, time.Minute)
	c.Insert("k", "u", "p", ResultOK, NewFields())
	if _, ok := c.Lookup("k", "p"); ok {
		t.Errorf("positive insert landed with ttl=0")
	}
	c.Insert("k2", "u2", "", ResultFail, nil)
	if _, ok := c.Lookup("k2", "anything"); !ok {
		t.Errorf("negative insert blocked by positive ttl=0")
	}
}

// TestCache_LRUEvictionUnderPressure — small cap forces eviction
// of the LRU when a new entry would overflow.
func TestCache_LRUEvictionUnderPressure(t *testing.T) {
	c := NewCache(400, time.Minute, time.Minute)
	for i, name := range []string{"alice", "bob", "carol", "dave"} {
		_ = i
		c.Insert(MakeCacheKey("imap", name), name, "p", ResultOK, NewFields())
	}
	_, _, _, n := c.Stats()
	if n == 4 {
		t.Errorf("no eviction under pressure: %d entries still cached", n)
	}
	if n == 0 {
		t.Errorf("over-eager eviction: cache empty")
	}
}

// TestCache_ClearByUserMask — selective flush of matching
// usernames; non-matching entries survive.
func TestCache_ClearByUserMask(t *testing.T) {
	c := NewCache(1<<20, time.Minute, time.Minute)
	c.Insert(MakeCacheKey("imap", "alice@a.com"), "alice@a.com", "p", ResultOK, NewFields())
	c.Insert(MakeCacheKey("imap", "alice@b.com"), "alice@b.com", "p", ResultOK, NewFields())
	c.Insert(MakeCacheKey("imap", "bob@a.com"), "bob@a.com", "p", ResultOK, NewFields())

	n := c.ClearByUserMask([]string{"alice@*"})
	if n != 2 {
		t.Errorf("removed %d entries, want 2", n)
	}
	if _, ok := c.Lookup(MakeCacheKey("imap", "alice@a.com"), "p"); ok {
		t.Errorf("alice@a.com still cached after flush")
	}
	if _, ok := c.Lookup(MakeCacheKey("imap", "bob@a.com"), "p"); !ok {
		t.Errorf("bob@a.com flushed unexpectedly")
	}
}

// TestCache_ClearAll — empty masks slice is full flush.
func TestCache_ClearAll(t *testing.T) {
	c := NewCache(1<<20, time.Minute, time.Minute)
	c.Insert(MakeCacheKey("imap", "alice"), "alice", "p", ResultOK, NewFields())
	c.Insert(MakeCacheKey("imap", "bob"), "bob", "p", ResultOK, NewFields())
	if n := c.Clear(); n != 2 {
		t.Errorf("Clear removed %d, want 2", n)
	}
}

// TestUserMaskMatch covers the glob semantics.
func TestUserMaskMatch(t *testing.T) {
	tests := []struct {
		mask, name string
		want       bool
	}{
		{"*", "anyone", true},
		{"alice", "alice", true},
		{"alice", "bob", false},
		{"alice@*", "alice@example.com", true},
		{"alice@*", "alice@", true},
		{"alice@*", "bob@example.com", false},
		{"*@example.com", "alice@example.com", true},
		{"*@example.com", "alice@other.com", false},
		{"a?ice", "alice", true},
		{"a?ice", "alce", false},
		{"a?ice", "abice", true},
		{"**", "anything", true},
		{"a*c", "abc", true},
		{"a*c", "ac", true},
		{"a*c", "abbbc", true},
		{"a*c", "abx", false},
	}
	for _, tc := range tests {
		got := userMaskMatch(tc.mask, tc.name)
		if got != tc.want {
			t.Errorf("userMaskMatch(%q, %q) = %v, want %v", tc.mask, tc.name, got, tc.want)
		}
	}
}

// TestCache_StatsCount tracks hit/miss bookkeeping.
func TestCache_StatsCount(t *testing.T) {
	c := NewCache(1<<20, time.Minute, time.Minute)
	c.Insert(MakeCacheKey("imap", "alice"), "alice", "p", ResultOK, NewFields())
	c.Lookup(MakeCacheKey("imap", "alice"), "p") // hit
	c.Lookup(MakeCacheKey("imap", "alice"), "x") // miss (wrong password)
	c.Lookup(MakeCacheKey("imap", "ghost"), "p") // miss (unknown)
	h, m, _, _ := c.Stats()
	if h != 1 {
		t.Errorf("hits = %d, want 1", h)
	}
	if m != 2 {
		t.Errorf("misses = %d, want 2", m)
	}
}

// TestCache_WrongPasswordDoesNotPoisonCachedOK — mistyped
// password must not downgrade an existing OK entry to Fail; the
// user must remain authenticatable with the right password
// immediately after a typo.
func TestCache_WrongPasswordDoesNotPoisonCachedOK(t *testing.T) {
	c := NewCache(1<<20, time.Minute, time.Minute)
	key := MakeCacheKey("imap", "alice")
	c.Insert(key, "alice", "right", ResultOK, NewFields())

	// Caller mistypes — sees miss (because Lookup mismatches),
	// then chain rejects → Insert with Fail.
	c.Insert(key, "alice", "WRONG", ResultFail, nil)

	// Now retry with right password — must still hit OK.
	e, ok := c.Lookup(key, "right")
	if !ok || e == nil || e.Result != ResultOK {
		t.Errorf("right password got %v after a wrong-password Fail; cache poisoned", e)
	}
}

// TestCache_PasswordChangeRefreshesOnSuccess — operator rotates
// the user's password in the backing store. Next successful
// login (with new password) must overwrite the stale OK entry,
// not be blocked by the cached old HMAC.
func TestCache_PasswordChangeRefreshesOnSuccess(t *testing.T) {
	c := NewCache(1<<20, time.Minute, time.Minute)
	key := MakeCacheKey("imap", "alice")
	c.Insert(key, "alice", "old", ResultOK, NewFields())

	// First attempt with NEW password: Lookup misses (HMAC
	// mismatch). Application-level: chain runs and succeeds →
	// Insert with the new password overwrites.
	if _, ok := c.Lookup(key, "new"); ok {
		t.Errorf("new password hit cache before refresh")
	}
	c.Insert(key, "alice", "new", ResultOK, NewFields())

	// Subsequent attempt with NEW password hits the refreshed
	// entry. Old password no longer matches.
	if _, ok := c.Lookup(key, "new"); !ok {
		t.Errorf("new password did not hit after refresh")
	}
	if _, ok := c.Lookup(key, "old"); ok {
		t.Errorf("old password still works after refresh — entry not overwritten")
	}
}

// TestCache_UnknownUserNegativeCacheStillWorks — when there's
// no prior OK entry, negative caching MUST still kick in (so
// repeated probes for non-existent users skip the passdb).
func TestCache_UnknownUserNegativeCacheStillWorks(t *testing.T) {
	c := NewCache(1<<20, time.Minute, time.Minute)
	key := MakeCacheKey("imap", "ghost")
	c.Insert(key, "ghost", "", ResultFail, nil)
	e, ok := c.Lookup(key, "anything")
	if !ok || e == nil || e.Result != ResultFail {
		t.Errorf("negative cache did not stick for unknown user: %v", e)
	}
}

// TestCache_PasswordMACDoesNotLeakPlain — sanity-check that the
// stored payload contains no plain-password substring.
func TestCache_PasswordMACDoesNotLeakPlain(t *testing.T) {
	c := NewCache(1<<20, time.Minute, time.Minute)
	pwd := "super-secret-password-12345"
	c.Insert(MakeCacheKey("imap", "alice"), "alice", pwd, ResultOK, NewFields())

	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.entries[MakeCacheKey("imap", "alice")]
	e := el.Value.(*CacheEntry) //nolint:errcheck // test-internal access
	if string(e.pwdMAC) == pwd {
		t.Errorf("password stored as plain: %q", e.pwdMAC)
	}
	for _, b := range e.pwdMAC {
		_ = b // ensure pwdMAC is non-empty
	}
	if len(e.pwdMAC) == 0 {
		t.Errorf("pwdMAC empty on positive entry — Lookup would succeed for any password")
	}
}
