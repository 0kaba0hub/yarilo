package jmap

import (
	"sync"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// stateCache holds the per-folder markers the Email state is built from, so a
// polling client does not reopen every folder on every request. Until push
// lands, polling is the normal pattern, and the walk is linear in folders
// (#1343).
//
// Validity is a PROOF, never a clock (#1248): each use compares the folder's
// current file stamp with the one the entry was built from, and a difference
// invalidates. The log is what makes this sound -- it grows on every write and
// shrinks when a fold truncates it, so both directions of change are visible,
// and its SIZE covers the case where mtime granularity is too coarse to notice,
// as on NFS.
//
// It is deliberately not a substitute for reading: a miss opens the folder
// exactly as before, and a stale entry is never served.
type stateCache struct {
	mu sync.Mutex
	// byUser keys on the account, so one user's markers can never answer for
	// another's -- the folder name alone would collide across accounts.
	byUser map[string]map[string]cachedMark
}

type cachedMark struct {
	stamp  mailbox.FolderStamp
	key    [8]byte
	fields []uint64
}

func newStateCache() *stateCache {
	return &stateCache{byUser: map[string]map[string]cachedMark{}}
}

// get returns the marker for a folder when the stamp still matches what it was
// built from.
func (c *stateCache) get(user, folder string, stamp mailbox.FolderStamp) (cachedMark, bool) {
	if c == nil {
		return cachedMark{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.byUser[user][folder]
	if !ok || entry.stamp != stamp {
		return cachedMark{}, false
	}
	return entry, true
}

func (c *stateCache) put(user, folder string, mark cachedMark) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	folders, ok := c.byUser[user]
	if !ok {
		folders = map[string]cachedMark{}
		c.byUser[user] = folders
	}
	folders[folder] = mark
}
