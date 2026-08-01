// Package ring implements the director's consistent-hashing ring:
// MD5, 100 virtual nodes per backend (configurable), binary search.
package ring

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const defaultVhosts = 100

// Backend represents a backend node in the ring.
type Backend struct {
	IP       string
	Port     int
	Tag      string
	Up       bool
	Vhosts   int   // virtual nodes; 0 = defaultVhosts (100)
	LastUp   int64 // Unix timestamp of last transition to Up
	LastDown int64 // Unix timestamp of last transition to Down (0 if never)
	Hostname string
}

// Ring is a consistent-hashing ring.
type Ring struct {
	mu        sync.RWMutex
	backends  map[string]*Backend // key: IP
	vhosts    []vhost             // all Up backends, sorted by hash
	tagVhosts map[string][]vhost  // tag → Up backends for that tag, sorted by hash
	hf        HashFormat          // username→hash-key template
}

type vhost struct {
	hash uint32
	ip   string
}

// NormalizeUsername lowercases a username for hashing, so two spellings
// of the same account route to the same backend. director.HashUsername
// delegates here.
func NormalizeUsername(username string) string {
	return strings.ToLower(username)
}

// New returns an empty Ring. hf must be the same HashFormat the
// director's userDir uses, so the two hashes never diverge for a user.
func New(hf HashFormat) *Ring {
	return &Ring{
		backends:  make(map[string]*Backend),
		tagVhosts: make(map[string][]vhost),
		hf:        hf,
	}
}

// AddBackend inserts or updates a backend and rebuilds the ring.
// Zero-valued LastUp/LastDown/Hostname on the incoming struct don't
// clobber an existing entry — heartbeats carry only LastUp, and the
// timestamp-based up/down merge between peers depends on LastDown
// surviving them.
func (r *Ring) AddBackend(b *Backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.backends[b.IP]; ok {
		if b.LastDown == 0 {
			b.LastDown = prev.LastDown
		}
		if b.LastUp == 0 {
			b.LastUp = prev.LastUp
		}
		if b.Hostname == "" {
			b.Hostname = prev.Hostname
		}
	}
	r.backends[b.IP] = b
	r.rebuild()
}

// RemoveBackend removes a backend and rebuilds the ring.
func (r *Ring) RemoveBackend(ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.backends, ip)
	r.rebuild()
}

// SetUp marks a backend up or down without removing it (BACKEND-FLUSH:
// stays known, stops receiving new lookups). Returns false if unknown.
func (r *Ring) SetUp(ip string, up bool, ts int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.backends[ip]
	if !ok {
		return false
	}
	b.Up = up
	if ts != 0 {
		if up {
			b.LastUp = ts
		} else {
			b.LastDown = ts
		}
	}
	r.rebuild()
	return true
}

// Backends returns a snapshot of all registered backends (up and down).
func (r *Ring) Backends() []Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Backend, 0, len(r.backends))
	for _, b := range r.backends {
		out = append(out, *b)
	}
	return out
}

// Len returns the number of registered backends. Takes the same r.mu as
// Lookup, so it doubles as the liveness probe: a wedged ring mutex
// blocks here too.
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.backends)
}

// CountBackendsInTag returns how many backends are registered under tag;
// the lease-expiry guard never removes the last one.
func (r *Ring) CountBackendsInTag(tag string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, b := range r.backends {
		if b.Tag == tag {
			n++
		}
	}
	return n
}

// Lookup returns the backend IP for the given username.
// Returns "" if the ring is empty.
func (r *Ring) Lookup(username string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.vhosts) == 0 {
		return ""
	}
	h := r.userHash(username)
	idx := sort.Search(len(r.vhosts), func(i int) bool {
		return r.vhosts[i].hash >= h
	})
	if idx == len(r.vhosts) {
		idx = 0
	}
	return r.vhosts[idx].ip
}

// Tags returns the set of distinct tags currently registered in the ring.
func (r *Ring) Tags() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tagVhosts))
	for t, vhs := range r.tagVhosts {
		if len(vhs) > 0 {
			out = append(out, t)
		}
	}
	return out
}

// GetBackend returns a copy of the Backend registered for ip, or nil if not found.
// Includes backends that are currently Down.
func (r *Ring) GetBackend(ip string) *Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b := r.backends[ip]
	if b == nil {
		return nil
	}
	cp := *b
	return &cp
}

// LookupBackend returns a copy of the Backend for the given username.
// Returns nil if the ring is empty or the backend is not found.
func (r *Ring) LookupBackend(username string) *Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lookupLocked(username, r.vhosts)
}

// LookupBackendByTag returns a backend for username restricted to backends
// whose Tag exactly matches tag (including "" for untagged backends).
// Returns nil if no backends with that tag exist.
// To route over the full ring regardless of tag, use LookupBackend.
func (r *Ring) LookupBackendByTag(username, tag string) *Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lookupLocked(username, r.tagVhosts[tag])
}

// lookupLocked performs a consistent-hash lookup in vhs. Must be called with r.mu held.
func (r *Ring) lookupLocked(username string, vhs []vhost) *Backend {
	if len(vhs) == 0 {
		return nil
	}
	h := r.userHash(username)
	idx := sort.Search(len(vhs), func(i int) bool {
		return vhs[i].hash >= h
	})
	if idx == len(vhs) {
		idx = 0
	}
	b := r.backends[vhs[idx].ip]
	if b == nil {
		return nil
	}
	cp := *b
	return &cp
}

func (r *Ring) rebuild() {
	r.vhosts = r.vhosts[:0]
	// reuse the map, clear the slices
	for t := range r.tagVhosts {
		r.tagVhosts[t] = r.tagVhosts[t][:0]
	}

	for ip, b := range r.backends {
		if !b.Up {
			continue
		}
		n := b.Vhosts
		if n <= 0 {
			n = defaultVhosts
		}
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("%s-%d", ip, i)
			h := vhostHash(key)
			vh := vhost{hash: h, ip: ip}
			r.vhosts = append(r.vhosts, vh)
			r.tagVhosts[b.Tag] = append(r.tagVhosts[b.Tag], vh)
		}
	}

	sort.Slice(r.vhosts, func(i, j int) bool {
		return r.vhosts[i].hash < r.vhosts[j].hash
	})
	for t := range r.tagVhosts {
		tvh := r.tagVhosts[t]
		sort.Slice(tvh, func(i, j int) bool { return tvh[i].hash < tvh[j].hash })
	}
}

func (r *Ring) userHash(username string) uint32 {
	return Hash(r.hf.Key(username))
}

func vhostHash(key string) uint32 {
	sum := md5.Sum([]byte(key))
	return binary.LittleEndian.Uint32(sum[:4])
}
