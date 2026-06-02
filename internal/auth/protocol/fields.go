package protocol

import (
	"fmt"
	"strings"
)

// Fields is an ordered key/value bag that carries the result of a
// passdb (and, eventually, userdb) lookup through the auth pipeline.
// Insertion order is preserved so the wire output is deterministic
// for any given mutation sequence — two processes that build the
// same Fields produce byte-identical OK responses.
//
// Scope is derived from the key prefix at iteration / serialise
// time, not stored on the entry:
//
//	auth_*    → ScopeInternal — never crosses the client wire
//	userdb_*  → ScopeUserdb   — passed through with the prefix
//	                            preserved so login pods can tell
//	                            them apart from passdb-only fields
//	(other)   → ScopePassdb   — passdb-only field, emitted on the
//	                            wire verbatim
//
// Phase AUTH-2 PR 1 introduces Fields alongside the legacy typed
// AuthResponse fields. Both populate in parallel so the existing
// SQL passdb + handleAuth call sites stay byte-compatible with the
// pre-AUTH-2 wire. PR 2 swaps the Passdb interface to take a shared
// Fields instance so chains can mutate it; PR 3 wires userdb
// prefetch through the same bag.
type Fields struct {
	keys []string
	vals []string
	idx  map[string]int
}

// NewFields constructs an empty bag.
func NewFields() *Fields { return &Fields{idx: make(map[string]int)} }

// Snapshot captures the bag's current state so it can be restored
// later via Rollback. Used by Chain to isolate each passdb's
// mutations: snapshot before the call, rollback on ResultNext so
// the next driver sees a clean slate. The snapshot is read-only —
// every mutating method on Fields is safe to call between
// Snapshot and Rollback / discard.
//
// Snapshot is O(n) in the current bag size (copies the keys, vals
// and idx maps); chain depth + bag size are both bounded by config
// so the cost stays trivial.
type Snapshot struct {
	keys []string
	vals []string
	idx  map[string]int
}

// Snapshot captures the bag's state. Returns nil for a nil bag
// (Rollback is also nil-safe) so callers can use the pair without
// pre-allocation when the bag is optional.
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

// Rollback restores the bag to the state captured by snap. Calling
// Rollback with a nil snap is a no-op so callers do not need to
// branch on Snapshot returning nil.
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

// SetValidated parses value through the reserved-field registry
// before storing. When key matches a known reserved name (or
// `userdb_<base>` where base is reserved), the validator returns
// the canonical form and that goes into the bag. Unknown keys
// (including everything `forward_*`-prefixed and anything not in
// the registry) pass through verbatim — same behaviour as Set.
//
// Returns a non-nil error when validation fails; the bag is NOT
// mutated in that case so callers can retry / surface the parse
// error without rolling back. Intended for admin / wire-side
// callers that ingest arbitrary input; driver-side passdb code
// keeps using Set (the driver knows its own schema).
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

// Set adds or overwrites a key. Stable: rewriting a key keeps its
// original insertion-order position so the wire serialisation does
// not flip mid-stream.
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

// Delete removes a key. No-op when the key is absent. Subsequent
// keys shift to fill the gap so iteration order stays sequential.
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

// Len returns the number of fields. Returns 0 for a nil bag so
// callers do not have to nil-check before length-based decisions.
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

// Scope classifies a field by its key prefix. Defined as a type so
// switch statements stay exhaustive when phase AUTH-2 PR 2 adds
// snapshot tracking.
type Scope int

const (
	// ScopePassdb — passdb-side fields (no prefix). Crosses the
	// client wire as `key=value`.
	ScopePassdb Scope = iota
	// ScopeUserdb — fields prefetched by passdb for the userdb
	// layer. Crosses the client wire as `userdb_key=value` so
	// login pods can split the response into passdb / userdb
	// halves the way Dovecot's auth-client does.
	ScopeUserdb
	// ScopeInternal — internal-only metadata. Never crosses the
	// client wire. Used for state-tracking fields like
	// `auth_cache_key`, `auth_failure_attempted`, etc. in later
	// AUTH-N phases.
	ScopeInternal
)

// ScopeOf returns the scope a key belongs to. The classification is
// derived from the prefix so callers do not need to remember which
// flags to set when calling Set.
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

// WireForm renders Fields as the tab-delimited `key=value` sequence
// the client protocol's OK response carries. ScopeInternal entries
// are dropped unconditionally; ScopeUserdb and ScopePassdb entries
// are emitted in insertion order with values escape-encoded so
// embedded tabs / newlines / NULs cannot break the wire framing.
//
// The first element returned is the response's user= token when
// `user=` is present in the bag — the rest of the auth pipeline
// expects the username token to lead. Callers that surface
// username via a different mechanism may pass it explicitly and
// drop the `user=` entry before serialising.
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

// escapeFieldValue stops TAB / LF / NUL / backslash from breaking
// the line-oriented framing of the client / master protocols.
// Implementation duplicates the escape semantics of master.go's
// escapeValue so callers do not need to import the master file —
// the wire convention is the same on both sockets.
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
