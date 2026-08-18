package jmapcore

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// A JMAP state string is opaque to clients, but not to us: Foo/changes is
// computed by DIFFING two of them, so the string has to carry a description of
// what the account looked like, not a digest of it. A hash cannot be diffed.
//
// The format is versioned from the first release that emits it, deliberately.
// A client persists a state the moment Foo/get returns one and hands it back
// later -- possibly to a build whose encoding has moved on. A consumer that
// meets a version it does not know must degrade to cannotCalculateChanges and
// let the client resync; the failure it must never have is a confident diff of
// a layout it misread. That costs one byte now and is unaddable later, because
// by then unversioned strings are already in clients' hands.
const stateVersion = 1

// Description kinds. Carried in the payload so an Email state handed to a
// Mailbox method is refused rather than diffed as though it meant anything.
const (
	KindEmail   = 'e'
	KindMailbox = 'm'
)

var (
	// ErrStateVersion says the string was written by a different version of
	// this encoding. Callers answer cannotCalculateChanges.
	ErrStateVersion = errors.New("jmapcore: state string from another format version")
	// ErrStateFormat says the string is not one of ours at all -- a client
	// inventing a value, or a state from a different object type.
	ErrStateFormat = errors.New("jmapcore: malformed state string")
)

// StateEntry is one folder's contribution. Key is the first 8 bytes of the
// folder GUID: rename-stable, and short enough that a mailbox with hundreds of
// folders still produces a state a client can hold.
type StateEntry struct {
	Key    [8]byte
	Fields []uint64
}

// Description is the whole account, as a sorted set of entries. A set, not a
// maximum: a deleted folder removes its entry, which changes the state without
// lowering anything, so the string can never travel backwards.
type Description struct {
	Kind    byte
	Entries []StateEntry
}

// String renders the state a client sees.
func (d Description) String() string {
	entries := append([]StateEntry(nil), d.Entries...)
	sort.Slice(entries, func(i, j int) bool {
		return string(entries[i].Key[:]) < string(entries[j].Key[:])
	})
	buf := make([]byte, 0, 16+len(entries)*24)
	buf = append(buf, d.Kind)
	buf = binary.AppendUvarint(buf, uint64(len(entries)))
	for _, e := range entries {
		buf = append(buf, e.Key[:]...)
		buf = binary.AppendUvarint(buf, uint64(len(e.Fields)))
		for _, f := range e.Fields {
			buf = binary.AppendUvarint(buf, f)
		}
	}
	return fmt.Sprintf("%d-%s", stateVersion, base64.RawURLEncoding.EncodeToString(buf))
}

// ParseDescription is the inverse, and the gate: a string from another version
// is reported as such rather than guessed at.
func ParseDescription(s string, kind byte) (Description, error) {
	dash := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			dash = i
			break
		}
	}
	if dash <= 0 {
		return Description{}, ErrStateFormat
	}
	var version int
	if _, err := fmt.Sscanf(s[:dash], "%d", &version); err != nil {
		return Description{}, ErrStateFormat
	}
	if version != stateVersion {
		return Description{}, fmt.Errorf("%w: %s", ErrStateVersion, s[:dash])
	}
	buf, err := base64.RawURLEncoding.DecodeString(s[dash+1:])
	if err != nil || len(buf) == 0 {
		return Description{}, ErrStateFormat
	}
	if buf[0] != kind {
		// An Email state passed to a Mailbox method, or the reverse. Diffing
		// them would compare folder markers of different meanings.
		return Description{}, fmt.Errorf("%w: wrong object type", ErrStateFormat)
	}
	out := Description{Kind: buf[0]}
	rest := buf[1:]
	n, used := binary.Uvarint(rest)
	if used <= 0 {
		return Description{}, ErrStateFormat
	}
	rest = rest[used:]
	for i := uint64(0); i < n; i++ {
		if len(rest) < 8 {
			return Description{}, ErrStateFormat
		}
		var e StateEntry
		copy(e.Key[:], rest[:8])
		rest = rest[8:]
		count, used := binary.Uvarint(rest)
		if used <= 0 {
			return Description{}, ErrStateFormat
		}
		rest = rest[used:]
		for j := uint64(0); j < count; j++ {
			v, used := binary.Uvarint(rest)
			if used <= 0 {
				return Description{}, ErrStateFormat
			}
			rest = rest[used:]
			e.Fields = append(e.Fields, v)
		}
		out.Entries = append(out.Entries, e)
	}
	if len(rest) != 0 {
		return Description{}, ErrStateFormat
	}
	return out, nil
}
