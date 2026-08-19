package imap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/userstate/acl"
)

// What a client is told when the ACL state cannot be read must not be our
// plumbing. The old text carried package paths, the transport error and the
// lock key -- which contains the account name -- and none of it means anything
// to a mail client (#1341).
//
// The forbidden strings are checked rather than the exact wording: pinning the
// sentence would fail on a rephrasing and pass on a new leak, which is the
// wrong way round.
func TestACLFailureTextCarriesNoInternals(t *testing.T) {
	publicRoot, dial := publicNSServer(t)
	if err := os.WriteFile(filepath.Join(publicRoot, acl.RootFileName), []byte("user=alice lrswipkxtea\n"), 0o600); err != nil {
		t.Fatalf("seed root acl: %v", err)
	}
	a := dial("alice")
	if err := a.Create("Public/Broken", nil).Wait(); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Make the mailbox's ACL unreadable: a directory where the file goes, which
	// is an I/O error rather than a parse one.
	aclPath := filepath.Join(publicRoot, ".Broken", "yarilo-acl")
	if err := os.Remove(aclPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove acl: %v", err)
	}
	if err := os.MkdirAll(aclPath, 0o700); err != nil {
		t.Fatalf("block acl: %v", err)
	}

	_, err := a.Select("Public/Broken", nil).Wait()
	if err == nil {
		t.Fatal("SELECT succeeded with an unreadable ACL")
	}
	text := err.Error()
	for _, leak := range []string{
		"userstate/acl", // our package path
		"yarilo-acl",    // our on-disk file name
		"mbox:",         // the lock key, which carries the account name
		"alice",         // the account name itself
		publicRoot,      // a server-side path
	} {
		if strings.Contains(text, leak) {
			t.Errorf("the client is told %q, which contains internal detail %q", text, leak)
		}
	}
	// And it still has to say something a user can act on, rather than being
	// emptied out to pass this test.
	if !strings.Contains(strings.ToLower(text), "try again") {
		t.Errorf("the refusal %q does not tell the client what to do", text)
	}
}
