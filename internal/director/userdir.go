package director

import (
	"crypto/md5"
	"encoding/binary"
	"sync"
	"time"
)

// UserEntry records which backend is serving a user.
type UserEntry struct {
	Hash      uint32
	Host      string // "ip:port"
	Weak      bool   // soft assignment — may be overridden by a new LOOKUP
	ExpiresAt time.Time
}

// UserDir is a hash-keyed, TTL-expiring user→backend directory.
// Thread-safe.
type UserDir struct {
	mu     sync.RWMutex
	byHash map[uint32]*UserEntry
	expire time.Duration
}

// NewUserDir creates a UserDir with the given per-entry TTL.
func NewUserDir(expire time.Duration) *UserDir {
	return &UserDir{
		byHash: make(map[uint32]*UserEntry),
		expire: expire,
	}
}

// HashUsername returns the MD5-based uint32 hash used to identify users in the
// director protocol. Matches the ring's userHash function exactly.
func HashUsername(username string) uint32 {
	sum := md5.Sum([]byte(username))
	return binary.LittleEndian.Uint32(sum[:4])
}

// Set stores a user→backend mapping and returns the username hash.
func (d *UserDir) Set(username, host string, weak bool) uint32 {
	h := HashUsername(username)
	d.SetByHash(h, host, weak)
	return h
}

// SetByHash stores a mapping by pre-computed hash.
func (d *UserDir) SetByHash(hash uint32, host string, weak bool) {
	e := &UserEntry{
		Hash:      hash,
		Host:      host,
		Weak:      weak,
		ExpiresAt: time.Now().Add(d.expire),
	}
	d.mu.Lock()
	d.byHash[hash] = e
	d.mu.Unlock()
}

// Get returns the entry for username, or nil if not found or expired.
func (d *UserDir) Get(username string) *UserEntry {
	return d.GetByHash(HashUsername(username))
}

// GetByHash returns the entry for hash, or nil if not found or expired.
func (d *UserDir) GetByHash(hash uint32) *UserEntry {
	d.mu.RLock()
	e := d.byHash[hash]
	d.mu.RUnlock()
	if e == nil || time.Now().After(e.ExpiresAt) {
		return nil
	}
	return e
}

// Delete removes the entry for username.
func (d *UserDir) Delete(username string) {
	d.DeleteByHash(HashUsername(username))
}

// DeleteByHash removes the entry for hash.
func (d *UserDir) DeleteByHash(hash uint32) {
	d.mu.Lock()
	delete(d.byHash, hash)
	d.mu.Unlock()
}

// Snapshot returns a copy of all non-expired entries.
func (d *UserDir) Snapshot() []UserEntry {
	now := time.Now()
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]UserEntry, 0, len(d.byHash))
	for _, e := range d.byHash {
		if now.Before(e.ExpiresAt) {
			out = append(out, *e)
		}
	}
	return out
}

// CountByBackend returns the number of non-expired user→backend entries per
// backend address ("ip:port"). Used for approximate session counts in metrics.
func (d *UserDir) CountByBackend() map[string]int {
	now := time.Now()
	d.mu.RLock()
	defer d.mu.RUnlock()
	counts := make(map[string]int)
	for _, e := range d.byHash {
		if now.Before(e.ExpiresAt) {
			counts[e.Host]++
		}
	}
	return counts
}

// Purge removes all expired entries.
func (d *UserDir) Purge() {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for h, e := range d.byHash {
		if now.After(e.ExpiresAt) {
			delete(d.byHash, h)
		}
	}
}
