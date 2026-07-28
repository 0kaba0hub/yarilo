package director

import (
	"net"
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
	mu     sync.RWMutex
	byHash map[uint32]*UserEntry
	expire time.Duration
	hf     ring.HashFormat // #850: username→hash-key template (encodes #738 lowercasing via %L)
	self   string          // this director's id ("ip:port"), stamped on local assignments (#772)
	clock  atomic.Uint64   // Lamport clock for assignment ordering (#772)
}

// NewUserDir creates a UserDir with the given per-entry TTL. hf is the compiled
// username→hash-key template (director_service.username_hash, #850) and MUST be the same
// HashFormat the Ring uses so HashUsername and the ring's own hash never diverge for the
// same user. self is this director's id ("ip:port"), stamped on locally-originated
// assignments for cross-director conflict resolution (#772).
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

// HashUsername returns the uint32 hash used to identify users in the director protocol.
// It delegates to ring.Hash(hf.Key(username)) — the exact same code path the ring's own
// userHash uses — so the two can never diverge for the same user and format (#850).
func HashUsername(username string, hf ring.HashFormat) uint32 {
	return ring.Hash(hf.Key(username))
}

// Set stores a locally-originated user→backend mapping (stamped with a
// fresh Lamport seq + this director's id) and returns the username hash.
func (d *UserDir) Set(username, host string, weak bool) uint32 {
	h := HashUsername(username, d.hf)
	d.SetByHash(h, host, weak)
	return h
}

// SetByHash stores a locally-originated mapping by pre-computed hash.
// tick + write happen under the SAME lock so the persisted stamp is
// monotonic per hash (#772 PR-2 review): with tick outside the lock, two
// concurrent Sets on the same hash could persist their seqs out of order,
// and once PR-2 propagates every LOOKUP a backwards seq would hit the wire.
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
// later local assignment sorts after it. Returns the PREVIOUS host when
// the merge moved the user to a DIFFERENT backend — the caller kicks that
// user's now-stale sessions off it (#772 PR-3). Returns "" when the
// incoming assignment lost, when it is a first sighting (no old backend),
// or when it kept the same backend — all "no kick needed".
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
// (#708 PR-B). While a user has a live session (the #804 session registry), the
// director periodically touches the pin so a move/assignment does not
// TTL-expire out from under an active session; once the last session closes the
// touches stop and the pin lapses back to the ring hash after user_expire. This
// is deliberately NOT a re-assignment: AssignSeq/AssignBy/Host are untouched, so
// it neither propagates nor perturbs conflict resolution. Returns whether an
// entry existed.
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
// (compare-and-delete, #708). A move rewrites the pin to a NEW backend before
// the kick it triggers arrives, so this conditional leaves the fresh pin intact
// while a plain admin kick (which passes the current backend) still clears it.
// Returns whether it deleted.
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

// DeleteByBackend removes every pin pointing at backendIP (host part of the
// stored "ip:port" Host), returning the count removed (#706). Used when a
// backend is flushed so new LOOKUPs rehash away from it instead of sticking to
// a drained/evacuated backend.
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
