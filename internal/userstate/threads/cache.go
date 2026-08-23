package threads

import (
	"os"
	"sync"
	"time"
)

// Cache keeps folded sidecars in memory, because folding one is O(account) and
// delivery happens per message.
//
// Measured on this machine, folding costs 0.39ms at a thousand messages, 2.7ms
// at ten thousand and 37.7ms at a hundred thousand. A delivery costs single
// milliseconds, so reading the sidecar per message would make delivery two to
// three times slower for the accounts that can least afford it -- the same
// O(account)-per-message trap the log format exists to avoid, entered from the
// other side.
//
// Bounded by idleness, not by process lifetime (#1396): a cache of every
// account ever delivered to holds their maps until restart, and an account
// nobody has written to in an hour is exactly the one whose memory should go
// back.
type Cache struct {
	mu      sync.Mutex
	entries map[string]*entry
	idle    time.Duration
	// folds counts how often a sidecar had to be read and folded. It is the
	// cost this cache exists to avoid, so it is the thing worth counting: a
	// caller that folds once per delivery is paying O(account) per message
	// again, and the number says so.
	folds int
}

type entry struct {
	state    *State
	size     int64
	modTime  time.Time
	lastUsed time.Time
}

// DefaultIdle is how long an unused account's threading state is kept.
// Matches the FTS handle timeout and the reference's own cache period: the
// same idea about idle state holding a resource.
const DefaultIdle = 300 * time.Second

func NewCache(idle time.Duration) *Cache {
	if idle <= 0 {
		idle = DefaultIdle
	}
	return &Cache{entries: map[string]*entry{}, idle: idle}
}

// Get returns the folded sidecar at path, reusing the cached fold when the file
// has not changed underneath it.
//
// Freshness is decided by size and mtime rather than by trusting the cache: the
// caller holds the account's thread lock, but a process that held the lock a
// moment ago may have appended, and threading from a stale state assigns a
// second thread id to a conversation that already has one.
func (c *Cache) Get(user, path string) (*State, error) {
	fi, statErr := os.Stat(path)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictIdleLocked()

	if e, ok := c.entries[user]; ok {
		switch {
		case statErr != nil && os.IsNotExist(statErr) && e.size == 0:
			// Still absent, still empty: an unmigrated account.
			e.lastUsed = time.Now()
			return e.state, nil
		case statErr == nil && e.size == fi.Size() && e.modTime.Equal(fi.ModTime()):
			e.lastUsed = time.Now()
			return e.state, nil
		}
	}

	st, err := Load(path)
	if err != nil {
		return nil, err
	}
	c.folds++
	e := &entry{state: st, lastUsed: time.Now()}
	if statErr == nil {
		e.size, e.modTime = fi.Size(), fi.ModTime()
	}
	c.entries[user] = e
	return st, nil
}

// Note records that this process just wrote to the sidecar, so the next Get
// does not refold what it already holds.
//
// Called after a successful append: the state in memory has been updated in
// step with the file, and re-reading it would cost the fold this cache exists
// to avoid.
func (c *Cache) Note(user, path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[user]; ok {
		e.size, e.modTime = fi.Size(), fi.ModTime()
		e.lastUsed = time.Now()
	}
}

// Forget drops an account, for a caller that knows the state on disk was
// replaced wholesale -- the migration step rebuilding it, say.
func (c *Cache) Forget(user string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, user)
}

func (c *Cache) evictIdleLocked() {
	if c.idle <= 0 {
		return
	}
	cutoff := time.Now().Add(-c.idle)
	for user, e := range c.entries {
		if e.lastUsed.Before(cutoff) {
			delete(c.entries, user)
		}
	}
}

// Folds reports how many times a sidecar was read and folded.
func (c *Cache) Folds() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.folds
}

// Len reports how many accounts are held, for a metric or a test.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
