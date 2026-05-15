// Package ring implements consistent hashing for yarilo-director.
// Algorithm: MD5, 100 virtual nodes per backend (configurable), binary search.
package ring

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
)

const defaultVhosts = 100

// Backend represents a backend node in the ring.
type Backend struct {
	IP               string
	Port             int
	Tag              string
	Up               bool
	Vhosts           int   // virtual nodes; 0 = defaultVhosts (100)
	LastUpdownChange int64 // Unix timestamp of last up/down state change
	Hostname         string
}

// Ring is a consistent-hashing ring.
type Ring struct {
	mu       sync.RWMutex
	backends map[string]*Backend // key: IP
	vhosts   []vhost             // sorted by hash
}

type vhost struct {
	hash uint32
	ip   string
}

// New returns an empty Ring.
func New() *Ring {
	return &Ring{backends: make(map[string]*Backend)}
}

// AddBackend inserts or replaces a backend and rebuilds the ring.
func (r *Ring) AddBackend(b *Backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
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

// SetUp marks a backend as up or down without removing it from the registry.
// Used for BACKEND-FLUSH: the backend stays known but stops receiving new lookups.
// Returns false if the backend is not found.
func (r *Ring) SetUp(ip string, up bool, ts int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.backends[ip]
	if !ok {
		return false
	}
	b.Up = up
	if ts != 0 {
		b.LastUpdownChange = ts
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

// Lookup returns the backend IP for the given username.
// Returns "" if the ring is empty.
func (r *Ring) Lookup(username string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.vhosts) == 0 {
		return ""
	}
	h := userHash(username)
	idx := sort.Search(len(r.vhosts), func(i int) bool {
		return r.vhosts[i].hash >= h
	})
	if idx == len(r.vhosts) {
		idx = 0
	}
	return r.vhosts[idx].ip
}

// LookupBackend returns a copy of the Backend for the given username.
// Returns nil if the ring is empty or the backend is not found.
func (r *Ring) LookupBackend(username string) *Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.vhosts) == 0 {
		return nil
	}
	h := userHash(username)
	idx := sort.Search(len(r.vhosts), func(i int) bool {
		return r.vhosts[i].hash >= h
	})
	if idx == len(r.vhosts) {
		idx = 0
	}
	b := r.backends[r.vhosts[idx].ip]
	if b == nil {
		return nil
	}
	cp := *b
	return &cp
}

func (r *Ring) rebuild() {
	r.vhosts = r.vhosts[:0]
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
			r.vhosts = append(r.vhosts, vhost{hash: h, ip: ip})
		}
	}
	sort.Slice(r.vhosts, func(i, j int) bool {
		return r.vhosts[i].hash < r.vhosts[j].hash
	})
}

func userHash(username string) uint32 {
	sum := md5.Sum([]byte(username))
	return binary.LittleEndian.Uint32(sum[:4])
}

func vhostHash(key string) uint32 {
	sum := md5.Sum([]byte(key))
	return binary.LittleEndian.Uint32(sum[:4])
}
