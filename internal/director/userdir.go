package director

import (
	"crypto/md5"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

// UserEntry records which backend is serving a user, plus a
// conflict-resolution stamp (#772): AssignSeq is a Lamport-clock value (a
// logical counter, NOT wall-clock — pod clocks are not synchronized and
// unix-nano would make the replica with the fastest clock "win"
// nondeterministically), and AssignBy is the id ("ip:port") of the
// director that produced the assignment. The total order is
// (AssignSeq, AssignBy) lexicographic: a higher AssignSeq is "newer"; on a
// tie the lower AssignBy wins — deterministic and reproducible in unit
// tests without a fake clock, matching the reference's monotonic per-origin
// sync rather than a timestamp race.
type UserEntry struct {
	Hash      uint32
	Host      string // "ip:port"
	Weak      bool   // soft assignment — may be overridden by a new LOOKUP
	ExpiresAt time.Time
	AssignSeq uint64
	AssignBy  string
}

// newer reports whether e should replace o under the (AssignSeq, AssignBy)
// total order — a strictly newer assignment, or an equal-seq assignment
// from a lower-id director (the deterministic conflict tiebreak).
func (e *UserEntry) newer(o *UserEntry) bool {
	if e.AssignSeq != o.AssignSeq {
		return e.AssignSeq > o.AssignSeq
	}
	return e.AssignBy < o.AssignBy
}

// UserDir is a hash-keyed, TTL-expiring user→backend directory.
// Thread-safe.
type UserDir struct {
	mu        sync.RWMutex
	byHash    map[uint32]*UserEntry
	expire    time.Duration
	lowercase bool          // #738: lowercase usernames before hashing
	self      string        // this director's id ("ip:port"), stamped on local assignments (#772)
	clock     atomic.Uint64 // Lamport clock for assignment ordering (#772)
}

// NewUserDir creates a UserDir with the given per-entry TTL. lowercase
// controls whether usernames are normalized before hashing
// (director_username_hash_lowercase, #738) — must match the Ring's setting
// so HashUsername and the ring's own hash never diverge for the same user.
// self is this director's id ("ip:port"), stamped on locally-originated
// assignments for cross-director conflict resolution (#772).
func NewUserDir(expire time.Duration, lowercase bool, self string) *UserDir {
	return &UserDir{
		byHash:    make(map[uint32]*UserEntry),
		expire:    expire,
		lowercase: lowercase,
		self:      self,
	}
}

// tick advances the Lamport clock for a locally-originated assignment.
func (d *UserDir) tick() uint64 { return d.clock.Add(1) }

// observe advances the Lamport clock past a received assignment's seq, so
// this node's next local assignment sorts after anything it has seen.
func (d *UserDir) observe(seq uint64) {
	for {
		cur := d.clock.Load()
		if seq < cur || d.clock.CompareAndSwap(cur, seq) {
			return
		}
	}
}

// HashUsername returns the MD5-based uint32 hash used to identify users in
// the director protocol. Matches the ring's userHash exactly given the same
// lowercase setting — both delegate to ring.NormalizeUsername.
func HashUsername(username string, lowercase bool) uint32 {
	if lowercase {
		username = ring.NormalizeUsername(username)
	}
	sum := md5.Sum([]byte(username))
	return binary.LittleEndian.Uint32(sum[:4])
}

// Set stores a locally-originated user→backend mapping (stamped with a
// fresh Lamport seq + this director's id) and returns the username hash.
func (d *UserDir) Set(username, host string, weak bool) uint32 {
	h := HashUsername(username, d.lowercase)
	d.SetByHash(h, host, weak)
	return h
}

// SetByHash stores a locally-originated mapping by pre-computed hash.
func (d *UserDir) SetByHash(hash uint32, host string, weak bool) {
	seq := d.tick()
	d.mu.Lock()
	d.byHash[hash] = &UserEntry{
		Hash:      hash,
		Host:      host,
		Weak:      weak,
		ExpiresAt: time.Now().Add(d.expire),
		AssignSeq: seq,
		AssignBy:  d.self,
	}
	d.mu.Unlock()
}

// LastAssign returns the (seq, by) stamp of the entry for hash, for a
// caller that needs to propagate the assignment it just made (#772 PR-2).
func (d *UserDir) LastAssign(hash uint32) (seq uint64, by string, ok bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if e := d.byHash[hash]; e != nil {
		return e.AssignSeq, e.AssignBy, true
	}
	return 0, "", false
}

// MergeByHash applies a REMOTE assignment (from a ring snapshot or a
// propagated event, #772) under the (AssignSeq, AssignBy) total order:
// it wins only if strictly newer, or equal-seq from a lower-id director.
// The Lamport clock is advanced past the incoming seq regardless, so a
// later local assignment sorts after it. Returns true if it changed the
// live mapping to a DIFFERENT backend (the signal PR-3 turns into a kick).
func (d *UserDir) MergeByHash(hash uint32, host string, weak bool, seq uint64, by string) bool {
	d.observe(seq)
	incoming := &UserEntry{
		Hash:      hash,
		Host:      host,
		Weak:      weak,
		ExpiresAt: time.Now().Add(d.expire),
		AssignSeq: seq,
		AssignBy:  by,
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cur := d.byHash[hash]
	if cur != nil && !incoming.newer(cur) {
		return false
	}
	changedBackend := cur == nil || cur.Host != host
	d.byHash[hash] = incoming
	return changedBackend
}

// Get returns the entry for username, or nil if not found or expired.
func (d *UserDir) Get(username string) *UserEntry {
	return d.GetByHash(HashUsername(username, d.lowercase))
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
	d.DeleteByHash(HashUsername(username, d.lowercase))
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
