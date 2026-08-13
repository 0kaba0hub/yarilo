package imap

import (
	"strings"
	"sync"
)

// syncTokenCache remembers the change token of the last successful maildir
// reconcile, for the life of the process.
//
// Process-lived rather than session-lived, per the cache policy (#1248): a
// session-scoped cache is invalidated by the client's login pattern instead of
// by the data changing, so it misses exactly the case it exists for — a client
// that reconnects per cycle (imaptest, a phone) never arrives with a warm
// anything and walks cur/ and new/ on its first SELECT, every cycle.
//
// Sharing the entry between sessions is safe because the token is its own
// proof: every SELECT recomputes it from cur/ and new/ and compares, so a
// cached value is a starting point rather than an answer, and a stale entry
// costs one extra reconcile rather than a wrong view. Nothing here is trusted
// because it is recent; there is deliberately no TTL.
//
// The entry is not per-session state, so no session owns it and none evicts it
// on logout: a folder's token is as valid for the next login as for this one.
type syncTokenCache struct {
	mu     sync.Mutex
	tokens map[string]string
	// maxEntries bounds the map so a server with a very large user population
	// cannot grow it without limit. Overflow drops the whole map rather than
	// evicting by age: the entries carry no age (a TTL is what the policy
	// forbids), and rebuilding one costs a single reconcile that would have
	// happened anyway.
	maxEntries int
}

// maildirSyncTokens is the process-wide instance. 100k folders of tokens is a
// few MB; the cap exists as a bound, not as a tuning knob.
var maildirSyncTokens = &syncTokenCache{maxEntries: 100_000}

// key identifies one folder of one user's storage. The storage location is part
// of it because the same username reaches different maildirs through different
// namespaces, and their tokens must not be each other's.
func syncTokenKey(username, location, folder string) string {
	return strings.Join([]string{username, location, folder}, "\x00")
}

func (c *syncTokenCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tok, ok := c.tokens[key]
	return tok, ok
}

func (c *syncTokenCache) put(key, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = make(map[string]string)
	}
	if len(c.tokens) >= c.maxEntries {
		c.tokens = make(map[string]string)
	}
	c.tokens[key] = token
}
