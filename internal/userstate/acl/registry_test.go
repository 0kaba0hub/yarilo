package acl

import (
	"context"
	"sort"
	"testing"

	"github.com/yarilomail/yarilo/pkg/dict"
	_ "github.com/yarilomail/yarilo/pkg/dict/memory"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func regDict(t *testing.T) dict.Dict {
	t.Helper()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func regEntry(id mailbox.Identifier, rights string, neg bool) ListEntry {
	return ListEntry{Mailbox: "INBOX", Identifier: id, Rights: mailbox.Rights(rights), Negative: neg}
}

func ownersOf(t *testing.T, d dict.Dict, user string, groups ...string) []string {
	t.Helper()
	owners, err := OwnersFor(context.Background(), d, user, groups)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(owners)
	return owners
}

// A grant makes the owner discoverable exactly by its subject; a revocation
// removes exactly that row; negatives and empty grants register nobody.
func TestRegistry_GrantRevokeVisibility(t *testing.T) {
	d := regDict(t)
	r := NewRegistry(d, "alice")

	entries := []ListEntry{
		regEntry(mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, "lr", false),
		regEntry(mailbox.Identifier{Type: mailbox.IDGroup, Name: "staff"}, "l", false),
		regEntry(mailbox.Identifier{Type: mailbox.IDUser, Name: "mallory"}, "lrwa", true), // negative: no discovery
		regEntry(mailbox.Identifier{Type: mailbox.IDUser, Name: "dan"}, "", false),        // empty grant: no discovery
	}
	if err := r.SyncFromList(entries, true); err != nil {
		t.Fatal(err)
	}

	if got := ownersOf(t, d, "bob"); len(got) != 1 || got[0] != "alice" {
		t.Errorf("bob should discover alice, got %v", got)
	}
	if got := ownersOf(t, d, "carol", "staff"); len(got) != 1 || got[0] != "alice" {
		t.Errorf("group grant should make alice discoverable to staff, got %v", got)
	}
	if got := ownersOf(t, d, "mallory"); len(got) != 0 {
		t.Errorf("a negative entry granted discovery: %v", got)
	}
	if got := ownersOf(t, d, "dan"); len(got) != 0 {
		t.Errorf("an empty grant granted discovery: %v", got)
	}

	// Revoke bob: his row goes, the group row stays.
	if err := r.SyncFromList(entries[1:2], true); err != nil {
		t.Fatal(err)
	}
	if got := ownersOf(t, d, "bob"); len(got) != 0 {
		t.Errorf("revoked grant still discoverable: %v", got)
	}
	if got := ownersOf(t, d, "carol", "staff"); len(got) != 1 {
		t.Errorf("group grant lost on unrelated revoke: %v", got)
	}
}

// anyone (and the reference's authenticated) make the owner discoverable to
// every caller.
func TestRegistry_AnyoneGrant(t *testing.T) {
	d := regDict(t)
	r := NewRegistry(d, "alice")
	if err := r.SyncFromList([]ListEntry{
		regEntry(mailbox.Identifier{Type: mailbox.IDAnyone}, "lr", false),
	}, true); err != nil {
		t.Fatal(err)
	}
	if got := ownersOf(t, d, "whoever"); len(got) != 1 || got[0] != "alice" {
		t.Errorf("anyone grant should be discoverable to all, got %v", got)
	}
}

// The no_removes rule: an incomplete snapshot only adds. Removing on partial
// knowledge is how someone else's visible space goes dark.
func TestRegistry_PartialSyncNeverRemoves(t *testing.T) {
	d := regDict(t)
	r := NewRegistry(d, "alice")
	full := []ListEntry{
		regEntry(mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, "lr", false),
		regEntry(mailbox.Identifier{Type: mailbox.IDUser, Name: "carol"}, "lr", false),
	}
	if err := r.SyncFromList(full, true); err != nil {
		t.Fatal(err)
	}
	// A partial view that no longer sees bob must not delete him.
	if err := r.SyncFromList(full[1:], false); err != nil {
		t.Fatal(err)
	}
	if got := ownersOf(t, d, "bob"); len(got) != 1 {
		t.Errorf("partial sync removed a row (no_removes violated): %v", got)
	}
	// The same set as a complete snapshot does remove.
	if err := r.SyncFromList(full[1:], true); err != nil {
		t.Fatal(err)
	}
	if got := ownersOf(t, d, "bob"); len(got) != 0 {
		t.Errorf("complete sync should remove, got %v", got)
	}
}

// Two owners share the dict: rows never bleed between them, and a caller
// granted by both sees both.
func TestRegistry_TwoOwnersCoexist(t *testing.T) {
	d := regDict(t)
	ra, rb := NewRegistry(d, "alice"), NewRegistry(d, "boss")
	grant := []ListEntry{regEntry(mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, "lr", false)}
	if err := ra.SyncFromList(grant, true); err != nil {
		t.Fatal(err)
	}
	if err := rb.SyncFromList(grant, true); err != nil {
		t.Fatal(err)
	}
	if got := ownersOf(t, d, "bob"); len(got) != 2 || got[0] != "alice" || got[1] != "boss" {
		t.Errorf("bob should discover both owners, got %v", got)
	}
	// alice revoking does not touch boss's row.
	if err := ra.SyncFromList(nil, true); err != nil {
		t.Fatal(err)
	}
	if got := ownersOf(t, d, "bob"); len(got) != 1 || got[0] != "boss" {
		t.Errorf("alice's revoke bled into boss's rows: %v", got)
	}
}

// The key space is the reference's, verbatim -- pinned so dict_import stays
// mechanical.
func TestRegistry_KeySpaceIsTheReferences(t *testing.T) {
	d := regDict(t)
	r := NewRegistry(d, "alice")
	if err := r.SyncFromList([]ListEntry{
		regEntry(mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, "lr", false),
	}, true); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"shared/shared-boxes/user/bob/alice",
		"shared/shared-user-boxes-rev/alice/user/bob",
	} {
		if _, found, err := d.Lookup(context.Background(), nil, key); err != nil || !found {
			t.Errorf("key %q missing (found=%v err=%v)", key, found, err)
		}
	}
}
