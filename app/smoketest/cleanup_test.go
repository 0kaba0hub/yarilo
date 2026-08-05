package main

import (
	"strings"
	"testing"
)

// These call the cleanup directly, with no connection, because both decisions
// are made before any byte is written: the root is recognised from the name,
// and a scoped removal with nothing to remove issues no command. A cleanup that
// reached the network would therefore be doing something it should not.
//
// The AST guards in root_cleanup_guard_test.go cannot see this: whether the
// folder is destroyed is a runtime branch, not a literal in the source. Reading
// the tree catches the caller that names INBOX; this catches the helper that
// forgets what INBOX is.

func TestCleanupOfTheRootNeverDeletesTheFolder(t *testing.T) {
	c := &imapClient{} // no connection: any command would panic or hang
	for _, folder := range []string{"INBOX", "inbox", ""} {
		if err := c.cleanupAfterCheck(folder, nil); err != nil {
			t.Errorf("cleanupAfterCheck(%q) = %v; cleaning up after a check on the "+
				"mailbox root must not touch the folder itself", folder, err)
		}
	}
}

func TestDeleteFolderRefusesTheMailboxRoot(t *testing.T) {
	c := &imapClient{}
	for _, folder := range []string{"INBOX", "inbox", "InBoX", ""} {
		err := c.deleteFolder(folder)
		if err == nil {
			t.Errorf("deleteFolder(%q) was carried out — on maildir that is the account", folder)
			continue
		}
		if !strings.Contains(err.Error(), "refusing") {
			t.Errorf("deleteFolder(%q) = %v; the refusal should say the run asked for something it must not", folder, err)
		}
	}
}

// A check against the root without the Message-IDs it seeded is refused before
// it connects, so the fault is reported against the caller that wrote it rather
// than surfacing as a mailbox that quietly lost its mail.
func TestCheckFolderRefusesTheRootWithoutSeededIDs(t *testing.T) {
	err := checkFolder("u@d.test", "pass", "INBOX")
	if err == nil {
		t.Fatal("checkFolder on the root with no seeded IDs was accepted")
	}
	if !strings.Contains(err.Error(), "nothing to clean up by") {
		t.Errorf("checkFolder refusal = %v; it should name the missing scope", err)
	}
}
