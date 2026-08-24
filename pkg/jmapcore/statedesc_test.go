package jmapcore

import (
	"errors"
	"strings"
	"testing"
)

// The gate the format exists for: a consumer meeting a version it does not know
// must say so, so the caller can answer cannotCalculateChanges. The failure it
// must never have is a confident diff of a layout it misread -- and by the time
// that could happen, unversioned strings are already in clients' hands, which
// is why the version ships with the first release that emits a state at all.
func TestParseRefusesAnotherFormatVersion(t *testing.T) {
	good := Description{
		Kind:    KindEmail,
		Entries: []StateEntry{{Key: [8]byte{1}, Fields: []uint64{1, 2, 3}}},
	}.String()

	tests := []struct {
		name  string
		state string
		want  error
	}{
		{"a newer version", "3-" + strings.SplitN(good, "-", 2)[1], ErrStateVersion},
		// Version 1 is a real predecessor, not a hypothetical: the format grew
		// the account-wide field, and a client holding a v1 string must be told
		// to resync rather than have its old layout guessed at.
		{"the previous version", "1-" + strings.SplitN(good, "-", 2)[1], ErrStateVersion},
		{"an older version", "0-" + strings.SplitN(good, "-", 2)[1], ErrStateVersion},
		{"the placeholder shipped before this format", "0", ErrStateFormat},
		{"a value a client invented", "hello", ErrStateFormat},
		{"truncated payload", good[:len(good)-4], ErrStateFormat},
		{"empty", "", ErrStateFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDescription(tc.state, KindEmail)
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// An Email state handed to a Mailbox method is not a diff with a surprising
// result; it is a different object type, and the markers do not mean the same
// thing. Refused rather than compared.
func TestParseRefusesTheWrongObjectType(t *testing.T) {
	email := Description{Kind: KindEmail}.String()
	if _, err := ParseDescription(email, KindMailbox); !errors.Is(err, ErrStateFormat) {
		t.Errorf("an Email state was accepted as a Mailbox state: %v", err)
	}
}

// A description survives the round trip, because Foo/changes is a diff of two
// of them: what cannot be read back cannot be diffed.
func TestDescriptionRoundTrips(t *testing.T) {
	in := Description{
		Kind: KindEmail,
		Entries: []StateEntry{
			{Key: [8]byte{9, 9}, Fields: []uint64{7, 1 << 40, 3}},
			{Key: [8]byte{1}, Fields: []uint64{1, 2, 3}},
			{Key: [8]byte{5}, Fields: nil},
		},
	}
	out, err := ParseDescription(in.String(), KindEmail)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Entries) != len(in.Entries) {
		t.Fatalf("got %d entries, want %d", len(out.Entries), len(in.Entries))
	}
	// Sorted on the way out, so two servers listing folders in different orders
	// produce the same state for the same account.
	for i := 1; i < len(out.Entries); i++ {
		if string(out.Entries[i-1].Key[:]) >= string(out.Entries[i].Key[:]) {
			t.Fatalf("entries are not sorted: %v", out.Entries)
		}
	}
	for _, e := range out.Entries {
		if e.Key == [8]byte{9, 9} && (len(e.Fields) != 3 || e.Fields[1] != 1<<40) {
			t.Errorf("fields did not survive: %v", e.Fields)
		}
	}
}

// The property that killed max-over-folders: removing a folder must change the
// state without producing something a client could read as older.
func TestRemovingAFolderChangesTheStateWithoutOrder(t *testing.T) {
	two := Description{Kind: KindEmail, Entries: []StateEntry{
		{Key: [8]byte{1}, Fields: []uint64{1, 5, 9}},
		{Key: [8]byte{2}, Fields: []uint64{1, 9, 9}},
	}}
	one := Description{Kind: KindEmail, Entries: two.Entries[:1]}
	if two.String() == one.String() {
		t.Fatal("dropping a folder left the state unchanged; a client would never resync")
	}
	// And what remains is still readable, which is what a diff needs: the
	// deletion has to be visible as a missing entry, not as an unparseable
	// string or a smaller number.
	got, err := ParseDescription(one.String(), KindEmail)
	if err != nil || len(got.Entries) != 1 {
		t.Fatalf("state after a deletion: %v, %v", got, err)
	}
}
