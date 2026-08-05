package imap_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// A mailbox that is not there is refused, not reported as deleted
// (RFC 9051 §6.3.5, code from RFC 5530).
//
// The client asked for it to be gone and it is gone, so the difference reads as
// academic — until a cleanup deletes the wrong name and reports success. That
// is how the destruction in #1063 stayed unattributed: ruling out a DELETE of a
// name that was not there took a manual isolation, because the protocol reply
// said nothing either way.
func TestDeleteNonexistentMailboxIsRefused(t *testing.T) {
	_, dial := enforceServerWithShared(t)
	c := dial("alice")

	err := c.Delete("no-such-mailbox").Wait()
	if err == nil {
		t.Fatal("DELETE of a mailbox that does not exist returned OK")
	}
	assertNonexistent(t, err, "DELETE")
}

func TestRenameNonexistentMailboxIsRefused(t *testing.T) {
	_, dial := enforceServerWithShared(t)
	c := dial("alice")

	err := c.Rename("no-such-mailbox", "somewhere-else", nil).Wait()
	if err == nil {
		t.Fatal("RENAME of a mailbox that does not exist returned OK")
	}
	assertNonexistent(t, err, "RENAME")
}

// And a mailbox that is there is still deleted and still renamed, or the tests
// above would pass on a server that refuses everything.
func TestDeleteAndRenameStillWorkOnRealMailboxes(t *testing.T) {
	_, dial := enforceServerWithShared(t)
	c := dial("alice")

	if err := c.Create("Work", nil).Wait(); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if err := c.Rename("Work", "Archive", nil).Wait(); err != nil {
		t.Fatalf("RENAME of an existing mailbox: %v", err)
	}
	if err := c.Delete("Archive").Wait(); err != nil {
		t.Fatalf("DELETE of an existing mailbox: %v", err)
	}
	// And it is gone, so the refusal above is about absence rather than about
	// the name.
	if err := c.Delete("Archive").Wait(); err == nil {
		t.Error("DELETE of the mailbox just deleted returned OK")
	}
}

// assertNonexistent checks the tagged NO carries the code a client acts on.
// The text is for a human; the code is what tells a client the mailbox is
// absent rather than that the server refused for some other reason.
func assertNonexistent(t *testing.T, err error, cmd string) {
	t.Helper()
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("%s returned %T (%v), want an IMAP error", cmd, err, err)
	}
	if imapErr.Type != imap.StatusResponseTypeNo {
		t.Errorf("%s answered %v, want NO", cmd, imapErr.Type)
	}
	if imapErr.Code != imap.ResponseCodeNonExistent {
		t.Errorf("%s answered code %q, want NONEXISTENT — a client cannot tell absence "+
			"from any other refusal without it", cmd, imapErr.Code)
	}
	if !strings.Contains(strings.ToLower(imapErr.Text), "mailbox") {
		t.Errorf("%s text %q does not mention the mailbox", cmd, imapErr.Text)
	}
}
