package imap_test

import (
	"errors"
	"testing"

	"github.com/emersion/go-imap/v2"

	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// RFC 4314 §3.3: the ACL commands answer NO for a mailbox that does not exist.
// They answered OK for any name at all, reporting full rights on a mailbox that
// is not there -- so a client asking whether it may write somewhere was told
// yes, and an audit of rights returned an answer for every name typed (#1075).
//
// This is not the traversal family: a plain absent name behaved exactly like
// "..", because nothing on the path checked existence at all.
func TestACLCommandsRefuseNamesThatAreNotThere(t *testing.T) {
	names := []string{"no-such-mailbox-at-all", "..", ".", "../victim@x/Maildir"}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			_, dial := enforceServerWithShared(t)
			c := dial("alice")
			defer func() { c.Logout().Wait() }() //nolint:errcheck

			t.Run("GETACL", func(t *testing.T) {
				data, err := c.GetACL(name).Wait()
				assertNoSuchMailbox(t, "GETACL", err, data)
			})
			t.Run("MYRIGHTS", func(t *testing.T) {
				data, err := c.MyRights(name).Wait()
				assertNoSuchMailbox(t, "MYRIGHTS", err, data)
			})
			t.Run("LISTRIGHTS", func(t *testing.T) {
				data, err := c.ListRights(name, "alice").Wait()
				assertNoSuchMailbox(t, "LISTRIGHTS", err, data)
			})
			t.Run("SETACL", func(t *testing.T) {
				err := c.SetACL(name, "bob", imap.RightModificationReplace, imap.RightSet("lr")).Wait()
				assertNoSuchMailbox(t, "SETACL", err, nil)
			})
			t.Run("DELETEACL", func(t *testing.T) {
				err := c.DeleteACL(name, "bob").Wait()
				assertNoSuchMailbox(t, "DELETEACL", err, nil)
			})
		})
	}
}

func assertNoSuchMailbox(t *testing.T, cmd string, err error, data any) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s answered for a mailbox that does not exist: %+v", cmd, data)
	}
	var ie *imap.Error
	if !errors.As(err, &ie) {
		t.Fatalf("%s returned %T (%v), want an IMAP error", cmd, err, err)
	}
	if ie.Type != imap.StatusResponseTypeNo {
		t.Errorf("%s answered %v, want NO", cmd, ie.Type)
	}
	if ie.Code == "SERVERBUG" {
		t.Errorf("%s answered SERVERBUG for a name the client chose: %q", cmd, ie.Text)
	}
}

// The same commands must still work on a real mailbox, or the test above would
// pass on a server that refuses every ACL command.
func TestACLCommandsStillAnswerForRealMailboxes(t *testing.T) {
	_, dial := enforceServerWithShared(t)
	c := dial("alice")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	if _, err := c.GetACL("INBOX").Wait(); err != nil {
		t.Errorf("GETACL INBOX: %v", err)
	}
	if _, err := c.MyRights("INBOX").Wait(); err != nil {
		t.Errorf("MYRIGHTS INBOX: %v", err)
	}
}

// RENAME validates both arguments, but only the source reported the right
// class: the destination went through renameInbox, which wrapped the storage
// error in a plain fmt.Errorf, so the library reported an internal server
// error for a name the client chose (#1075).
func TestRenameDestinationIsNotReportedAsAServerBug(t *testing.T) {
	c := startServerWith(t, maildirBackend(t))
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	for _, dest := range []string{"../escaped2", "..", ""} {
		err := c.Rename("INBOX", dest, nil).Wait()
		if err == nil {
			t.Errorf("RENAME INBOX %q was accepted", dest)
			continue
		}
		var ie *imap.Error
		if !errors.As(err, &ie) {
			t.Errorf("RENAME INBOX %q returned %T (%v), want an IMAP error", dest, err, err)
			continue
		}
		if ie.Code == "SERVERBUG" {
			t.Errorf("RENAME INBOX %q answered SERVERBUG: %q — both arguments are mailbox names and both should answer alike", dest, ie.Text)
		}
	}
}

// SUBSCRIBE may name a mailbox that does not exist -- RFC 9051 §6.3.7 permits
// it and clients rely on it -- but not one this server would refuse everywhere
// else. A stored ".." can only ever produce a subscription no command can act
// on, handed back by LSUB as though it meant something (#1075).
func TestSubscribeRefusesNamesNoCommandWouldAccept(t *testing.T) {
	// Wrapped as mailboxbuild.ByDriver wraps it in production: the rules live
	// in the decorator, so a test on a bare driver would assert nothing.
	mb := mailboxpkg.Validating(maildirBackend(t), mailboxpkg.DefaultNameRules())
	c := startServerWith(t, mb)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	for _, name := range []string{"..", ".", "../victim@x/Maildir"} {
		if err := c.Subscribe(name).Wait(); err == nil {
			t.Errorf("SUBSCRIBE %q was stored; no other command accepts that name", name)
		}
	}

	// The permitted case: a mailbox that simply is not there yet.
	if err := c.Subscribe("NotYetCreated").Wait(); err != nil {
		t.Errorf("SUBSCRIBE of an absent but valid name was refused: %v — RFC 9051 §6.3.7 allows it", err)
	}
}
