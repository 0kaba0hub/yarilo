package imap_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// DELETE INBOX must be refused on every backend: on maildir INBOX is the mail
// root, so performing it destroys the account (#1063). The name stays valid
// for every other verb, which is why the refusal is asserted per command.
func TestDeleteInboxIsRefusedOnEveryBackend(t *testing.T) {
	for _, bf := range backends {
		t.Run(bf.name, func(t *testing.T) {
			mb := bf.new(t)
			c := startServerWith(t, mb)
			defer func() { c.Logout().Wait() }() //nolint:errcheck

			const body = "From: a@b.com\r\nSubject: keep\r\n\r\nkeep me\r\n"
			ac := c.Append("INBOX", int64(len(body)), nil)
			if _, err := ac.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
			if err := ac.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := ac.Wait(); err != nil {
				t.Fatal(err)
			}

			for _, name := range []string{"INBOX", "inbox", "InBoX"} {
				err := c.Delete(name).Wait()
				if err == nil {
					t.Fatalf("DELETE %q was performed; on maildir that removes the mail root", name)
				}
				var ie *imap.Error
				if !errors.As(err, &ie) {
					t.Fatalf("DELETE %q returned %T (%v), want an IMAP error", name, err, err)
				}
				if ie.Type != imap.StatusResponseTypeNo {
					t.Errorf("DELETE %q answered %v, want NO", name, ie.Type)
				}
				// The code, not the text, is what a client acts on (#1067):
				// CANNOT says the server will never perform this, as opposed
				// to a transient or rights-based refusal.
				if ie.Code != imap.ResponseCodeCannot {
					t.Errorf("DELETE %q answered code %q, want CANNOT — a client cannot tell a permanent refusal from a temporary one without it", name, ie.Code)
				}
				if !strings.Contains(strings.ToUpper(ie.Text), "INBOX") {
					t.Errorf("DELETE %q text %q does not say which mailbox was refused", name, ie.Text)
				}
			}

			// The message must still be fetchable: a refusal that answered NO
			// after deleting would satisfy the assertions above.
			sd, err := c.Select("INBOX", nil).Wait()
			if err != nil {
				t.Fatalf("SELECT INBOX after refused DELETE: %v", err)
			}
			if sd.NumMessages != 1 {
				t.Errorf("INBOX holds %d messages after refused DELETE, want 1", sd.NumMessages)
			}
		})
	}
}

// A folder that is not INBOX must still be deletable — otherwise the test above
// passes on a server that refuses every DELETE.
func TestDeleteStillWorksForOrdinaryFolders(t *testing.T) {
	mb := maildirBackend(t)
	c := startServerWith(t, mb)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	if err := c.Create("Archive", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete("Archive").Wait(); err != nil {
		t.Errorf("DELETE Archive: %v", err)
	}
}
