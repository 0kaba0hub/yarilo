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

// Cache is a bytes-bounded LRU for verified auth lookups.
// Each entry stores a keyed digest of the password that produced it,
// compared in constant time on lookup; mismatch is a miss, so a
// negative entry only matches the same wrong password. The digest
// key is per-process random, never persisted.
type Cache struct {
	mu      sync.Mutex
	maxSize int64
	curSize int64
	ttl     time.Duration
	negTTL  time.Duration
	entries map[string]*list.Element
	lru     *list.List
	hmacKey []byte
	// telemetry counters
	hits   uint64
	misses uint64
}

// CacheEntry is the cached passdb result. Both OK and Fail entries
// carry the password digest; an empty digest is treated as a miss
// so an unset entry fails safe.
type CacheEntry struct {
	Result   Result
	Fields   *Fields
	pwdMAC   []byte
	username string // for selective flush by user mask
	expires  time.Time
}

// NewCache returns a Cache bounded by sizeBytes. Non-positive
// sizeBytes returns nil; all methods no-op on a nil Cache.
// ttl=0 disables positive caching, negTTL=0 negative caching.
func NewCache(sizeBytes int64, ttl, negTTL time.Duration) *Cache {
	if sizeBytes <= 0 {
		return nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// no RNG, digests would be precomputable; refuse to construct
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

// macPassword digests plain under the per-cache random key;
// a memory dump reveals digests, never plain passwords.
func (c *Cache) macPassword(plain string) []byte {
	var h hash.Hash = hmac.New(sha256.New, c.hmacKey)
	h.Write([]byte(plain))
	return h.Sum(nil)
}

// estimateEntrySize approximates an entry's memory weight for the
// byte budget. Ignores some pointer/map overhead.
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

// Lookup returns the entry for key if it is unexpired and the
// password digest matches. Expired entries are evicted on touch.
// Returns (nil, false) on miss or password mismatch.
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
	// verify the digest on negative entries too, so a wrong-password
	// entry can't block the correct password; constant-time compare
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

// observeSize publishes the fill gauges. Called with c.mu held.
func (c *Cache) observeSize() {
	cacheEntries.Set(float64(len(c.entries)))
	cacheBytes.Set(float64(c.curSize))
	cacheMaxBytes.Set(float64(c.maxSize))
}

// Insert stores an entry under key. The plain password is digested
// before storage, for negative entries too. fields is stored by
// reference; the caller must not mutate it after Insert returns.
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
		// Never downgrade a cached OK to Fail: a single mistyped
		// password would otherwise lock out the correct one for
		// the neg-TTL window.
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
	// Negative entries remember which password failed, so only that
	// password short-circuits on lookup.
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
		// oversized entry: skip rather than flush the whole cache
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
	// key lives only in the entries map; find it by scan
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

// ClearByUserMask evicts entries whose username matches any mask
// (`*` = any run, `?` = one char). Empty masks means full flush.
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

// userMaskMatch implements `*` / `?` glob matching.
func userMaskMatch(mask, name string) bool {
	if mask == "*" {
		return true
	}
	if !strings.ContainsAny(mask, "*?") {
		return mask == name
	}
	return globMatch(mask, name)
}

func globMatch(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			// compress star runs
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

// Stats returns hit / miss / size counters. sizeBytes is the
// byte estimate, not entry count.
func (c *Cache) Stats() (hits, misses uint64, sizeBytes, entryCount int64) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.curSize, int64(len(c.entries))
}

// MakeCacheKey builds the canonical cache key for a (service, user)
// lookup; shared by the wire layer and in-process auth so their
// cache hits are interchangeable.
func MakeCacheKey(service, username string) string {
	return fmt.Sprintf("%s\t%s", service, username)
}
