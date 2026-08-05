package imap_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

var statusAll = imap.StatusOptions{NumMessages: true, UIDValidity: true, UIDNext: true}

func appendN(t *testing.T, c *imapclient.Client, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("From: a@b.com\r\nSubject: m%d\r\n\r\nbody\r\n", i)
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
	}
}

// STATUS must refuse a name that resolves outside the mailbox instead of
// answering for it. The answer is not merely wrong: OpenFolder creates the
// index it is asked for, so STATUS on such a name initialised a fresh index at
// that path and reported an empty mailbox to the client (#1072).
func TestStatusRefusesNamesOutsideTheMailbox(t *testing.T) {
	for _, bf := range backends {
		t.Run(bf.name, func(t *testing.T) {
			c := startServerWith(t, bf.new(t))
			defer func() { c.Logout().Wait() }() //nolint:errcheck
			appendN(t, c, 8)

			for _, name := range []string{"..", ".", "", "../victim@x/Maildir", "./../victim@x/Maildir"} {
				sd, err := c.Status(name, &statusAll).Wait()
				if err == nil {
					n := uint32(0)
					if sd.NumMessages != nil {
						n = *sd.NumMessages
					}
					t.Errorf("STATUS %q answered MESSAGES=%d UIDVALIDITY=%d; the name resolves outside the mailbox",
						name, n, sd.UIDValidity)
					continue
				}
				var ie *imap.Error
				if !errors.As(err, &ie) {
					t.Errorf("STATUS %q returned %T (%v), want an IMAP error", name, err, err)
					continue
				}
				if ie.Code != imap.ResponseCodeNonExistent {
					t.Errorf("STATUS %q answered code %q, want NONEXISTENT — the same answer SELECT and DELETE give for these names",
						name, ie.Code)
				}
			}
		})
	}
}

// STATUS must still answer for a mailbox that exists — otherwise the test
// above passes on a server that refuses every STATUS.
func TestStatusStillAnswersForRealMailboxes(t *testing.T) {
	c := startServerWith(t, maildirBackend(t))
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	appendN(t, c, 3)

	sd, err := c.Status("INBOX", &statusAll).Wait()
	if err != nil {
		t.Fatalf("STATUS INBOX: %v", err)
	}
	if sd.NumMessages == nil || *sd.NumMessages != 3 {
		t.Errorf("STATUS INBOX reported %v messages, want 3", sd.NumMessages)
	}
}

// A refused name is a client error, not a server fault. NO [SERVERBUG] tells
// the operator to go looking for a crash that never happened (#1072).
func TestCreateWithInvalidNameIsNotReportedAsAServerBug(t *testing.T) {
	c := startServerWith(t, maildirBackend(t))
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	err := c.Create("../escaped", nil).Wait()
	if err == nil {
		t.Fatal("CREATE \"../escaped\" was accepted")
	}
	var ie *imap.Error
	if !errors.As(err, &ie) {
		t.Fatalf("CREATE returned %T (%v), want an IMAP error", err, err)
	}
	if ie.Code == "SERVERBUG" {
		t.Errorf("CREATE answered SERVERBUG for an invalid name: %q", ie.Text)
	}
	if ie.Code != imap.ResponseCodeCannot {
		t.Errorf("CREATE answered code %q, want CANNOT", ie.Code)
	}
}
