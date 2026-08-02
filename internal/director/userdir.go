package director

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

// UserEntry records which backend serves a user plus a conflict-resolution
// stamp (#772). AssignSeq is a Lamport clock (logical, NOT wall-clock: pod
// clocks are unsynced), AssignBy is the originating director's id ("ip:port").
// Total order (AssignSeq, AssignBy): higher seq wins; on a tie the lower id
// wins — deterministic and reproducible without a fake clock.
type UserEntry struct {
	Hash      uint32
	Host      string // "ip:port"
	Weak      bool   // soft assignment — may be overridden by a new LOOKUP
	ExpiresAt time.Time
	AssignSeq uint64
	AssignBy  string
}

// newer reports whether e replaces o under the (AssignSeq, AssignBy) total
// order: strictly newer seq, or equal seq from a lower-id director.
func (e *UserEntry) newer(o *UserEntry) bool {
	if e.AssignSeq != o.AssignSeq {
		return e.AssignSeq > o.AssignSeq
	}
	return e.AssignBy < o.AssignBy
}

// UserDir is a hash-keyed, TTL-expiring user→backend directory.
// Thread-safe.
type UserDir struct {
	mu     sync.RWMutex
	byHash map[uint32]*UserEntry
	expire time.Duration
	hf     ring.HashFormat // username→hash-key template (#850)
	self   string          // this director's id ("ip:port"), stamped on local assignments (#772)
	clock  atomic.Uint64   // Lamport clock for assignment ordering (#772)
}

// NewUserDir creates a UserDir with the given per-entry TTL. hf MUST be the
// same HashFormat the Ring uses so hashes never diverge for a user (#850).
// self is this director's id, stamped on local assignments (#772).
func NewUserDir(expire time.Duration, hf ring.HashFormat, self string) *UserDir {
	return &UserDir{
		byHash: make(map[uint32]*UserEntry),
		expire: expire,
		hf:     hf,
		self:   self,
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

// HashUsername returns the uint32 hash identifying users in the director
// protocol. Same code path as the ring's own userHash, so the two never diverge
// for the same user and format (#850).
func HashUsername(username string, hf ring.HashFormat) uint32 {
	return ring.Hash(hf.Key(username))
}

// Set stores a locally-originated mapping (fresh Lamport seq + this director's
// id) and returns the username hash.
func (d *UserDir) Set(username, host string, weak bool) uint32 {
	h := HashUsername(username, d.hf)
	d.SetByHash(h, host, weak)
	return h
}

// SetByHash stores a locally-originated mapping by pre-computed hash. tick +
// write happen under the SAME lock so the persisted stamp is monotonic per
// hash; otherwise concurrent Sets could persist seqs out of order and a
// backwards seq would hit the wire (#772).
func (d *UserDir) SetByHash(hash uint32, host string, weak bool) {
	d.mu.Lock()
	d.byHash[hash] = &UserEntry{
		Hash:      hash,
		Host:      host,
		Weak:      weak,
		ExpiresAt: time.Now().Add(d.expire),
		AssignSeq: d.tick(),
		AssignBy:  d.self,
	}
	d.mu.Unlock()
}

// LastAssign returns the (seq, by) stamp for hash, for a caller propagating the
// assignment it just made (#772).
func (d *UserDir) LastAssign(hash uint32) (seq uint64, by string, ok bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if e := d.byHash[hash]; e != nil {
		return e.AssignSeq, e.AssignBy, true
	}
	return 0, "", false
}

// MergeByHash applies a REMOTE assignment under the (AssignSeq, AssignBy) total
// order: wins only if strictly newer, or equal-seq from a lower-id director. The
// Lamport clock advances past the incoming seq regardless. Returns the PREVIOUS
// host when the merge moved the user to a DIFFERENT backend, so the caller kicks
// that user's stale sessions off it; "" when the assignment lost, was a first
// sighting, or kept the same backend (#772).
func (d *UserDir) MergeByHash(hash uint32, host string, weak bool, seq uint64, by string) (kickOldHost string) {
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
		return ""
	}
	old := ""
	if cur != nil && cur.Host != host {
		old = cur.Host
	}
	d.byHash[hash] = incoming
	return old
}

// Get returns the entry for username, or nil if not found or expired.
func (d *UserDir) Get(username string) *UserEntry {
	return d.GetByHash(HashUsername(username, d.hf))
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

// Touch extends the pin's TTL without changing its target or assignment stamp
// (#708). While a user has a live session (#804) the director periodically
// touches the pin so a move does not TTL-expire under an active session; touches
// stop when the last session closes. NOT a re-assignment: AssignSeq/AssignBy/Host
// are untouched, so it neither propagates nor perturbs conflict resolution.
// Returns whether an entry existed.
func (d *UserDir) Touch(username string) bool {
	return d.TouchByHash(HashUsername(username, d.hf))
}

// TouchByHash is Touch by pre-computed hash.
func (d *UserDir) TouchByHash(hash uint32) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byHash[hash]
	if !ok {
		return false
	}
	e.ExpiresAt = time.Now().Add(d.expire)
	return true
}

// Delete removes the entry for username.
func (d *UserDir) Delete(username string) {
	d.DeleteByHash(HashUsername(username, d.hf))
}

// DeleteByHash removes the entry for hash.
func (d *UserDir) DeleteByHash(hash uint32) {
	d.mu.Lock()
	delete(d.byHash, hash)
	d.mu.Unlock()
}

// DeleteIfBackend removes the user's pin ONLY if it still points at backendIP
// (compare-and-delete, #708): a move rewrites the pin to a new backend before
// its kick arrives, so this leaves the fresh pin intact while a plain admin kick
// (passing the current backend) still clears it. Returns whether it deleted.
func (d *UserDir) DeleteIfBackend(username, backendIP string) bool {
	hash := HashUsername(username, d.hf)
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byHash[hash]
	if !ok {
		return false
	}
	host, _, err := net.SplitHostPort(e.Host)
	if err != nil {
		host = e.Host
	}
	if host != backendIP {
		return false
	}
	delete(d.byHash, hash)
	return true
}

// DeleteByBackend removes every pin pointing at backendIP, returning the count
// removed (#706). Used on backend flush so new LOOKUPs rehash away from it.
func (d *UserDir) DeleteByBackend(backendIP string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for h, e := range d.byHash {
		host, _, err := net.SplitHostPort(e.Host)
		if err != nil {
			host = e.Host
		}
		if host == backendIP {
			delete(d.byHash, h)
			n++
		}
	}
	return n
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
