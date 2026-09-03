package jmap

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// The two states are separate because the two object types change for different
// reasons. Asserting only "the state moved" would pass on a single state shared
// by both -- which is the design this replaces, and which sends every client to
// refetch everything whenever anything happens.
func TestStatesMoveForTheirOwnTypeOnly(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(t *testing.T, s *Server, id string)
		wantEmail   bool
		wantMailbox bool
	}{
		{
			name: "a keyword change moves the Email state",
			mutate: func(t *testing.T, s *Server, id string) {
				emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:{"keywords/$flagged":true}}}`, testUser, id))
			},
			wantEmail: true,
			// The counts a Mailbox reports are unread and total; flagging
			// changes neither, so the mailbox has not changed for a client.
			wantMailbox: false,
		},
		{
			name: "marking read moves both, because unreadEmails is a Mailbox property",
			mutate: func(t *testing.T, s *Server, id string) {
				emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:{"keywords/$seen":null}}}`, testUser, id))
			},
			wantEmail:   true,
			wantMailbox: true,
		},
		{
			name:        "nothing changes neither",
			mutate:      func(*testing.T, *Server, string) {},
			wantEmail:   false,
			wantMailbox: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, id, _ := storedServerWithMessageAt(t, setTestMessage, 0)
			h := openHandleForTest(t, s)

			emailBefore, err := s.emailState(h)
			if err != nil {
				t.Fatalf("email state: %v", err)
			}
			listBefore, err := s.mailboxList(h)
			if err != nil {
				t.Fatalf("mailbox list: %v", err)
			}
			mailboxBefore := mailboxState(listBefore)

			tc.mutate(t, s, id)

			h2 := openHandleForTest(t, s)
			emailAfter, err := s.emailState(h2)
			if err != nil {
				t.Fatalf("email state after: %v", err)
			}
			listAfter, err := s.mailboxList(h2)
			if err != nil {
				t.Fatalf("mailbox list after: %v", err)
			}
			mailboxAfter := mailboxState(listAfter)

			if moved := emailAfter != emailBefore; moved != tc.wantEmail {
				t.Errorf("Email state moved = %v, want %v (%q -> %q)", moved, tc.wantEmail, emailBefore, emailAfter)
			}
			if moved := mailboxAfter != mailboxBefore; moved != tc.wantMailbox {
				t.Errorf("Mailbox state moved = %v, want %v", moved, tc.wantMailbox)
			}
		})
	}
}

// Both states must be readable back as descriptions, since Foo/changes is a
// diff of two of them, and must carry the version that lets a later consumer
// refuse a layout it does not know.
func TestEmittedStatesAreVersionedDescriptions(t *testing.T) {
	s, _, _ := storedServerWithMessageAt(t, setTestMessage, 0)
	h := openHandleForTest(t, s)

	email, err := s.emailState(h)
	if err != nil {
		t.Fatalf("email state: %v", err)
	}
	list, err := s.mailboxList(h)
	if err != nil {
		t.Fatalf("mailbox list: %v", err)
	}
	for _, tc := range []struct {
		name  string
		state string
		kind  byte
	}{
		{"email", email, jmapcore.KindEmail},
		{"mailbox", mailboxState(list), jmapcore.KindMailbox},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A version, not THIS version: the format grows, and a test that
			// pins the number turns every bump into an unrelated failure while
			// proving nothing more than the prefix does.
			if !versionPrefixed(tc.state) {
				t.Errorf("state %q carries no format version", tc.state)
			}
			desc, err := jmapcore.ParseDescription(tc.state, tc.kind)
			if err != nil {
				t.Fatalf("the state we emit does not parse: %v", err)
			}
			if len(desc.Entries) == 0 {
				t.Error("the description names no folder; a diff would report everything as deleted")
			}
		})
	}
}

// openHandleForTest opens the user's store the way a request does.
func openHandleForTest(t *testing.T, s *Server) *userHandle {
	t.Helper()
	h, err := s.opts.Storage.open(testUser, "test-session")
	if err != nil {
		t.Fatalf("open user: %v", err)
	}
	t.Cleanup(func() { h.close() })
	return h
}

// versionPrefixed reports whether a state string carries a numeric format
// version, which is what makes an unknown layout answerable with
// cannotCalculateChanges instead of a confident misreading.
func versionPrefixed(state string) bool {
	dash := strings.IndexByte(state, '-')
	if dash <= 0 {
		return false
	}
	for i := 0; i < dash; i++ {
		if state[i] < '0' || state[i] > '9' {
			return false
		}
	}
	return true
}
