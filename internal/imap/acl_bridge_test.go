package imap

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Whitebox tests for the wire ↔ on-disk bridge helpers
// (rightsToIMAP / rightsFromIMAP / identifierFromIMAP). The end-to-end
// session behaviour is covered by acl_test.go in package imap_test.

func TestBridgeRightsRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   imaplib.RightSet
		want mailbox.Rights
	}{
		{"empty", imaplib.RightSet(""), ""},
		{"all standard", imaplib.RightSet("lrswipkxtea"), mailbox.FullRights},
		{"obsolete c expands", imaplib.RightSet("c"), "k"},
		{"obsolete d expands", imaplib.RightSet("d"), "te"},
		{"unsorted dedupes", imaplib.RightSet("walwarl"), "lrwa"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rightsFromIMAP(tc.in)
			if err != nil {
				t.Fatalf("rightsFromIMAP: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if back := rightsToIMAP(got); string(back) != string(tc.want) {
				t.Errorf("toIMAP round-trip = %q, want %q", back, tc.want)
			}
		})
	}
}

func TestBridgeIdentifierFromIMAP(t *testing.T) {
	tests := []struct {
		name string
		in   imaplib.RightsIdentifier
		want mailbox.Identifier
	}{
		{"anyone", imaplib.RightsIdentifier("anyone"), mailbox.Identifier{Type: mailbox.IDAnyone}},
		{"authenticated", imaplib.RightsIdentifier("authenticated"), mailbox.Identifier{Type: mailbox.IDAuthenticated}},
		{"owner", imaplib.RightsIdentifier("owner"), mailbox.Identifier{Type: mailbox.IDOwner}},
		{"bare user", imaplib.RightsIdentifier("bob@test.com"), mailbox.Identifier{Type: mailbox.IDUser, Name: "bob@test.com"}},
		{"dollar-prefixed group", imaplib.RightsIdentifier("$staff"), mailbox.Identifier{Type: mailbox.IDGroup, Name: "staff"}},
		{"group-override passthrough", imaplib.RightsIdentifier("group-override=admins"), mailbox.Identifier{Type: mailbox.IDGroupOverride, Name: "admins"}},
		// Bare 'user=' on the wire is treated as a literal username —
		// the `user=` prefix is disk-only.
		{"bare looks-like-user", imaplib.RightsIdentifier("user=carol"), mailbox.Identifier{Type: mailbox.IDUser, Name: "user=carol"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := identifierFromIMAP(tc.in)
			if err != nil {
				t.Fatalf("identifierFromIMAP: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBridgeIdentifierToIMAP(t *testing.T) {
	tests := []struct {
		name string
		in   mailbox.Identifier
		want imaplib.RightsIdentifier
	}{
		{"anyone", mailbox.Identifier{Type: mailbox.IDAnyone}, imaplib.RightsIdentifier("anyone")},
		{"authenticated", mailbox.Identifier{Type: mailbox.IDAuthenticated}, imaplib.RightsIdentifier("authenticated")},
		{"owner", mailbox.Identifier{Type: mailbox.IDOwner}, imaplib.RightsIdentifier("owner")},
		{"user bare", mailbox.Identifier{Type: mailbox.IDUser, Name: "bob@test.com"}, imaplib.RightsIdentifier("bob@test.com")},
		{"group with $", mailbox.Identifier{Type: mailbox.IDGroup, Name: "staff"}, imaplib.RightsIdentifier("$staff")},
		{"group-override passthrough", mailbox.Identifier{Type: mailbox.IDGroupOverride, Name: "admins"}, imaplib.RightsIdentifier("group-override=admins")},
		{"invalid emits empty", mailbox.Identifier{}, imaplib.RightsIdentifier("")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := identifierToIMAP(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBridgeIdentifierRejectsEmptyGroup(t *testing.T) {
	if _, _, err := identifierFromIMAP(imaplib.RightsIdentifier("$")); err == nil {
		t.Error("expected error on bare '$' group identifier")
	}
}

func TestBridgeNegativeIdentifierParsed(t *testing.T) {
	id, negative, err := identifierFromIMAP(imaplib.RightsIdentifier("-bob"))
	if err != nil {
		t.Fatalf("negative identifier should parse: %v", err)
	}
	if !negative {
		t.Error(`expected negative=true for "-bob"`)
	}
	if id.Type != mailbox.IDUser || id.Name != "bob" {
		t.Errorf("got %+v, want user=bob", id)
	}
}

func TestBridgeRejectsEmptyIdentifier(t *testing.T) {
	if _, _, err := identifierFromIMAP(imaplib.RightsIdentifier("")); err == nil {
		t.Error("expected error on empty identifier")
	}
}
