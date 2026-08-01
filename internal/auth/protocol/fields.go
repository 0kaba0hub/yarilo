package protocol

import (
	"fmt"
	"strings"
)

// Fields is an ordered key/value bag carrying a passdb/userdb lookup
// result through the auth pipeline. Insertion order is preserved so two
// processes that build the same Fields produce byte-identical OK
// responses. Scope is derived from the key prefix at serialise time,
// not stored on the entry:
//
//	auth_*    → ScopeInternal — never crosses the client wire
//	userdb_*  → ScopeUserdb   — passed through with the prefix preserved
//	(other)   → ScopePassdb   — passdb-only field, emitted verbatim
type Fields struct {
	keys []string
	vals []string
	idx  map[string]int
}

// NewFields constructs an empty bag.
func NewFields() *Fields { return &Fields{idx: make(map[string]int)} }

// Snapshot captures the bag's state for later Rollback. Chain uses it
// to isolate each passdb's mutations: snapshot before the call, roll
// back on ResultNext so the next driver sees a clean slate. O(n) in the
// current bag size (copies keys, vals, idx).
type Snapshot struct {
	keys []string
	vals []string
	idx  map[string]int
}

// Snapshot captures the bag's state. Returns nil for a nil bag
// (Rollback is also nil-safe).
func (f *Fields) Snapshot() *Snapshot {
	if f == nil {
		return nil
	}
	keys := make([]string, len(f.keys))
	copy(keys, f.keys)
	vals := make([]string, len(f.vals))
	copy(vals, f.vals)
	idx := make(map[string]int, len(f.idx))
	for k, v := range f.idx {
		idx[k] = v
	}
	return &Snapshot{keys: keys, vals: vals, idx: idx}
}

// Rollback restores the bag to the state captured by snap. Nil snap is
// a no-op.
func (f *Fields) Rollback(snap *Snapshot) {
	if f == nil || snap == nil {
		return
	}
	f.keys = append(f.keys[:0], snap.keys...)
	f.vals = append(f.vals[:0], snap.vals...)
	for k := range f.idx {
		delete(f.idx, k)
	}
	for k, v := range snap.idx {
		f.idx[k] = v
	}
}

// SetValidated canonicalises value through the reserved-field registry
// before storing. When key (or its `userdb_<base>` form) is reserved,
// the validator's canonical form goes into the bag; unknown keys pass
// through verbatim like Set. On validation failure the bag is NOT
// mutated. For admin / wire-side callers ingesting arbitrary input;
// driver-side passdb code uses Set (it knows its own schema).
func (f *Fields) SetValidated(key, value string) error {
	base := key
	if rest, ok := strings.CutPrefix(key, "userdb_"); ok {
		base = rest
	}
	if v, ok := reservedValidators[base]; ok {
		canonical, err := v(value)
		if err != nil {
			return fmt.Errorf("auth/protocol: field %q: %w", key, err)
		}
		f.Set(key, canonical)
		return nil
	}
	f.Set(key, value)
	return nil
}

// Set adds or overwrites a key. Rewriting a key keeps its original
// insertion-order position.
func (f *Fields) Set(key, value string) {
	if i, ok := f.idx[key]; ok {
		f.vals[i] = value
		return
	}
	f.idx[key] = len(f.keys)
	f.keys = append(f.keys, key)
	f.vals = append(f.vals, value)
}

// Get returns (value, true) when the key is present.
func (f *Fields) Get(key string) (string, bool) {
	if f == nil {
		return "", false
	}
	i, ok := f.idx[key]
	if !ok {
		return "", false
	}
	return f.vals[i], true
}

// Has reports whether the key is set.
func (f *Fields) Has(key string) bool {
	if f == nil {
		return false
	}
	_, ok := f.idx[key]
	return ok
}

// Delete removes a key (no-op when absent). Subsequent keys shift to
// fill the gap so iteration order stays sequential.
func (f *Fields) Delete(key string) {
	if f == nil {
		return
	}
	i, ok := f.idx[key]
	if !ok {
		return
	}
	delete(f.idx, key)
	f.keys = append(f.keys[:i], f.keys[i+1:]...)
	f.vals = append(f.vals[:i], f.vals[i+1:]...)
	for k, ki := range f.idx {
		if ki > i {
			f.idx[k] = ki - 1
		}
	}
}

// Len returns the number of fields. Returns 0 for a nil bag.
func (f *Fields) Len() int {
	if f == nil {
		return 0
	}
	return len(f.keys)
}

// Each iterates in insertion order. The visitor returns false to
// stop early; true to continue. nil bag yields nothing.
func (f *Fields) Each(fn func(key, value string) bool) {
	if f == nil {
		return
	}
	for i, k := range f.keys {
		if !fn(k, f.vals[i]) {
			return
		}
	}
}

// Scope classifies a field by its key prefix.
type Scope int

const (
	// ScopePassdb — passdb-side fields (no prefix). Cross the client
	// wire as `key=value`.
	ScopePassdb Scope = iota
	// ScopeUserdb — fields prefetched by passdb for the userdb layer.
	// Cross the client wire as `userdb_key=value` so login pods can
	// split the response into passdb / userdb halves.
	ScopeUserdb
	// ScopeInternal — internal-only metadata (e.g. `auth_cache_key`).
	// Never crosses the client wire.
	ScopeInternal
)

// ScopeOf returns the scope a key belongs to, derived from its prefix.
func ScopeOf(key string) Scope {
	switch {
	case strings.HasPrefix(key, "auth_"):
		return ScopeInternal
	case strings.HasPrefix(key, "userdb_"):
		return ScopeUserdb
	default:
		return ScopePassdb
	}
}

// WireForm renders Fields as the tab-delimited `key=value` sequence the
// client protocol's OK response carries. ScopeInternal entries are
// dropped; the rest emit in insertion order with values escape-encoded
// so embedded tabs / newlines / NULs cannot break the wire framing.
func (f *Fields) WireForm() []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, len(f.keys))
	for i, k := range f.keys {
		if ScopeOf(k) == ScopeInternal {
			continue
		}
		out = append(out, k+"="+escapeFieldValue(f.vals[i]))
	}
	return out
}

// escapeFieldValue stops TAB / LF / NUL / backslash from breaking the
// line-oriented framing of the client / master protocols. Matches the
// escape semantics of master.go's escapeValue — same wire convention on
// both sockets.
func escapeFieldValue(v string) string {
	if !strings.ContainsAny(v, "\t\n\x00\\") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for _, r := range v {
		switch r {
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case 0:
			b.WriteString(`\0`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
