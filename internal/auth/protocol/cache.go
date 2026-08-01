package protocol

import (
	"container/list"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"hash"
	"strings"
	"sync"
	"time"
)

// Cache is an in-process LRU cache for verified auth lookups.
//
//   - Bytes-bounded: the byte cap limits total payload weight; LRU
//     eviction makes room when an insert would exceed the cap.
//   - Two TTLs: positive entries (successful auth) live up to TTL,
//     negative entries (failed lookup — unknown user, wrong
//     password) live up to NegTTL. A separate, typically shorter
//     NegTTL means a fresh password attempt can succeed soon after
//     the user resets their password without waiting for the full
//     positive TTL to expire.
//   - Password verify on hit: every cached entry — positive OR
//     negative — stores an HMAC of the plain password that produced
//     it (computed with a process-local random key). On lookup the
//     incoming password is HMAC-ed and compared in constant time; a
//     mismatch is a miss and re-runs the chain. For a negative entry
//     this means only the SAME wrong password short-circuits, while
//     a different (e.g. the user's correct) password falls through to
//     the chain instead of inheriting the cached Fail (#950).
//
// Caching defence-in-depth: the HMAC key is generated per Cache
// construction and never persisted. A memory dump reveals the
// HMACs (which are not invertible) but not plain passwords. The
// key never crosses any wire — even the master-protocol
// CACHE-FLUSH only carries user masks, not entries.
//
// Cache key format is decided by the caller (typically
// `service\tusername`); Cache treats keys as opaque.
type Cache struct {
	mu      sync.Mutex
	maxSize int64
	curSize int64
	ttl     time.Duration
	negTTL  time.Duration
	entries map[string]*list.Element
	lru     *list.List
	hmacKey []byte
	// hits / misses bookkeeping for tests / telemetry.
	hits   uint64
	misses uint64
}

// CacheEntry is the cached value. ResultOK carries Fields (the
// passdb result the chain produced); both ResultOK and ResultFail
// carry pwdMAC (HMAC of the plain password that produced the entry).
// A Fail entry short-circuits the chain ONLY when the incoming
// password MACs to the same value (#950) — so a wrong password does
// not poison the user's correct one.
//
// Empty pwdMAC on any entry is treated as a cache miss by Lookup, so
// a legacy/unset entry fails safe (re-runs the chain).
type CacheEntry struct {
	Result   Result
	Fields   *Fields
	pwdMAC   []byte
	username string // for selective flush by user mask
	expires  time.Time
}

// NewCache returns a Cache bounded by sizeBytes total payload. A
// non-positive sizeBytes returns nil — caching is disabled and
// every helper that takes a *Cache no-ops on nil.
//
// ttl is the positive-entry lifetime; negTTL is the negative-entry
// lifetime. ttl=0 disables positive caching; negTTL=0 disables
// negative caching. Setting ttl=0 with sizeBytes>0 means only
// negatives are cached (and vice versa).
func NewCache(sizeBytes int64, ttl, negTTL time.Duration) *Cache {
	if sizeBytes <= 0 {
		return nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// crypto/rand failure means the OS RNG is broken;
		// without a key we cannot guarantee the password-MAC
		// cannot be precomputed. Refuse to construct.
		return nil
	}
	return &Cache{
		maxSize: sizeBytes,
		ttl:     ttl,
		negTTL:  negTTL,
		entries: make(map[string]*list.Element),
		lru:     list.New(),
		hmacKey: key,
	}
}

// macPassword computes HMAC-SHA256 of plain under the per-cache
// random key. Used for both insert and verify so a side-channel
// observer (cache dump) sees only MACs, never plain passwords.
func (c *Cache) macPassword(plain string) []byte {
	var h hash.Hash = hmac.New(sha256.New, c.hmacKey)
	h.Write([]byte(plain))
	return h.Sum(nil)
}

// estimateEntrySize is the bytes-budget weight assigned to an
// entry. Approximates total memory by summing fixed overhead +
// key length + serialized Fields bag + pwdMAC length. Off by
// some pointer/map overhead — close enough for LRU pressure.
func estimateEntrySize(key string, e *CacheEntry) int64 {
	n := int64(96) // fixed: list element + map slot + struct headers
	n += int64(len(key))
	n += int64(len(e.username))
	n += int64(len(e.pwdMAC))
	if e.Fields != nil {
		e.Fields.Each(func(k, v string) bool {
			n += int64(len(k)) + int64(len(v)) + 16
			return true
		})
	}
	return n
}

// Lookup checks the cache for key. On positive hit it verifies
// the incoming password against the stored HMAC in constant time;
// mismatch is treated as miss so a stale cached password cannot
// authenticate a new one. Negative hits return (entry, true) with
// Result=ResultFail and the caller short-circuits without
// touching the chain.
//
// Expired entries are evicted lazily — Lookup removes them on
// touch so capacity accounting stays current. neg_expired_r
// equivalent: when an expired entry is encountered the caller
// gets (nil, false) and runs the chain; the eviction means a
// reinsert can land cleanly.
//
// Returns (nil, false) for cache miss or password mismatch.
func (c *Cache) Lookup(key, password string) (*CacheEntry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.entries[key]
	if !ok {
		c.misses++
		cacheLookups.WithLabelValues("miss").Inc()
		c.observeSize()
		return nil, false
	}
	e := el.Value.(*CacheEntry) //nolint:errcheck // map+LRU only ever hold *CacheEntry
	if time.Now().After(e.expires) {
		c.removeElement(el)
		c.misses++
		cacheLookups.WithLabelValues("expired").Inc()
		c.observeSize()
		return nil, false
	}
	// Verify the incoming password against the stored MAC for BOTH positive
	// AND negative entries (#950). A negative entry now carries the MAC of the
	// wrong password that seeded it, so a DIFFERENT password — notably the
	// user's CORRECT one — no longer matches a poisoned Fail entry and falls
	// through to the chain. A repeated identical wrong password still matches
	// and short-circuits (repeat-failure / per-password anti-enumeration).
	// Constant-time compare so a hit/miss cannot be timed apart.
	if len(e.pwdMAC) == 0 || !hmac.Equal(e.pwdMAC, c.macPassword(password)) {
		c.misses++
		cacheLookups.WithLabelValues("pwd_mismatch").Inc()
		c.observeSize()
		return nil, false
	}
	c.lru.MoveToFront(el)
	c.hits++
	cacheLookups.WithLabelValues("hit").Inc()
	c.observeSize()
	return e, true
}

// observeSize publishes the cache fill gauges. Called with c.mu held from the
// paths that can change occupancy, so the gauges never report a torn view of
// curSize vs entry count.
func (c *Cache) observeSize() {
	cacheEntries.Set(float64(len(c.entries)))
	cacheBytes.Set(float64(c.curSize))
	cacheMaxBytes.Set(float64(c.maxSize))
}

// Insert stores an entry under key. password is the plain
// credential (HMAC-ed before storage; never retained as plain) and
// is recorded for negative entries too (#950), so a Fail entry only
// short-circuits a later lookup that presents the SAME password.
//
// fields is the AuthResponse bag captured at the chain's reply;
// it's stored by reference so the caller must not mutate it after
// Insert returns. Pass a snapshot (Fields.Snapshot then a fresh
// Fields rebuilt) if mutation is unavoidable.
func (c *Cache) Insert(key, username, password string, result Result, fields *Fields) {
	if c == nil {
		return
	}
	if result == ResultOK && c.ttl <= 0 {
		return
	}
	if result == ResultFail && c.negTTL <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.entries[key]; ok {
		// Anti-poisoning: do NOT downgrade an existing positive
		// entry to a negative one. Scenario: user mistypes pwd
		// once → chain returns Fail → without this guard we would
		// overwrite the cached OK with a Fail and short-circuit
		// every subsequent attempt — including the correct
		// password — for the neg-TTL window. Locks the user out
		// of their own account.
		//
		// With the guard, a wrong-password attempt just leaves
		// the old OK entry in place. Next attempt with the right
		// password hits cache and verifies against stored HMAC →
		// hit. Password change handled by HMAC mismatch (Lookup
		// returns miss) → chain runs → Insert overwrites with
		// the new HMAC (this code path, result=OK on existing
		// entry, which IS allowed to overwrite).
		//
		// We cannot distinguish "user unknown" from "user known
		// but wrong password" at the Chain layer (both surface
		// as ResultFail), so we conservatively never negative-
		// cache a key that previously authenticated. The first-
		// time-failed (unknown user) path still seeds correctly
		// because there's no prior entry.
		existing := el.Value.(*CacheEntry) //nolint:errcheck // map+LRU only ever hold *CacheEntry
		if result == ResultFail && existing.Result == ResultOK {
			return
		}
		c.removeElement(el)
	}

	e := &CacheEntry{
		Result:   result,
		Fields:   fields,
		username: username,
	}
	// Store the password MAC for BOTH positive and negative entries (#950): a
	// negative entry must remember WHICH password failed, so only that same
	// password short-circuits on lookup while a different (correct) password
	// re-runs the chain instead of inheriting the poisoned Fail verdict.
	e.pwdMAC = c.macPassword(password)
	if result == ResultOK {
		e.expires = time.Now().Add(c.ttl)
	} else {
		e.expires = time.Now().Add(c.negTTL)
	}

	size := estimateEntrySize(key, e)
	// Evict from the back of the LRU until the new entry fits.
	for c.curSize+size > c.maxSize && c.lru.Len() > 0 {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.removeElement(oldest)
	}
	if size > c.maxSize {
		// Single oversized entry — refuse to insert instead of
		// flushing the whole cache for it.
		return
	}

	el := c.lru.PushFront(e)
	c.entries[key] = el
	c.curSize += size
	c.observeSize()
}

// removeElement removes el from both indices and adjusts curSize.
// Caller must hold the mutex.
func (c *Cache) removeElement(el *list.Element) {
	e := el.Value.(*CacheEntry) //nolint:errcheck // map+LRU only ever hold *CacheEntry
	// Find key by linear scan; we keep the key in the entries
	// map only, so reuse it from there to avoid storing on entry.
	for k, v := range c.entries {
		if v == el {
			c.curSize -= estimateEntrySize(k, e)
			delete(c.entries, k)
			break
		}
	}
	c.lru.Remove(el)
}

// Remove deletes a single key. Returns true if an entry existed.
func (c *Cache) Remove(key string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return false
	}
	c.removeElement(el)
	return true
}

// Clear evicts every entry. Returns how many were removed.
func (c *Cache) Clear() uint32 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := uint32(len(c.entries))
	c.entries = make(map[string]*list.Element)
	c.lru = list.New()
	c.curSize = 0
	return n
}

// ClearByUserMask evicts every entry whose stored username matches
// any of the supplied masks. Mask syntax: `*` matches zero or more
// chars, `?` matches one (admin CLI:
// `yarctl auth cache flush [<user-mask>...]`).
//
// Empty masks slice → behaves like Clear (full flush).
func (c *Cache) ClearByUserMask(masks []string) uint32 {
	if c == nil {
		return 0
	}
	if len(masks) == 0 {
		return c.Clear()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	var removed uint32
	for k, el := range c.entries {
		e := el.Value.(*CacheEntry) //nolint:errcheck // map+LRU only ever hold *CacheEntry
		for _, m := range masks {
			if userMaskMatch(m, e.username) {
				c.curSize -= estimateEntrySize(k, e)
				delete(c.entries, k)
				c.lru.Remove(el)
				removed++
				break
			}
		}
	}
	return removed
}

// userMaskMatch implements `*` / `?` glob matching. Iterative,
// O(len(pattern) * len(name)) worst case — fine for short user
// names and small mask lists used by the admin CLI.
func userMaskMatch(mask, name string) bool {
	// Common cases first.
	if mask == "*" {
		return true
	}
	if !strings.ContainsAny(mask, "*?") {
		return mask == name
	}
	// Recursive backtracker.
	return globMatch(mask, name)
}

func globMatch(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			// Compress runs of stars.
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if globMatch(pattern, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		default:
			if len(s) == 0 || pattern[0] != s[0] {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		}
	}
	return len(s) == 0
}

// Stats returns hit / miss / size counters for telemetry. The
// returned size is the byte estimate, not entry count.
func (c *Cache) Stats() (hits, misses uint64, sizeBytes, entryCount int64) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.curSize, int64(len(c.entries))
}

// MakeCacheKey assembles the canonical cache key for a passdb /
// userdb lookup. Pinned to a single shape across drivers for
// predictability. Wire layer + chainAuthenticator both call this
// so cache hits are interchangeable between in-process and
// remote auth paths for the same (user, service).
func MakeCacheKey(service, username string) string {
	return fmt.Sprintf("%s\t%s", service, username)
}
