package imap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/userstate/acl"
)

// In a namespace nobody owns, a mailbox could end up with no administrator at
// all: the only source of 'a' was the root grant, and when it omitted one, the
// creator could not administer what it had just created -- and nor could
// anyone else, because repairing it needs the right that is missing (#1320).
//
// The creator gets 'a' now. The rows below are the two halves of that
// sentence: it is granted, and nothing else is.
func TestCreatorGetsAdminInANamespaceNobodyOwns(t *testing.T) {
	publicRoot, dial := publicNSServer(t)

	// The root grant deliberately omits 'a' -- and 't' as well, which is the
	// control: the fix must not hand out rights the root withheld.
	if err := os.WriteFile(filepath.Join(publicRoot, acl.RootFileName), []byte("user=alice lrswipkxe\n"), 0o600); err != nil {
		t.Fatalf("seed root acl: %v", err)
	}

	a := dial("alice")
	if err := a.Create("Public/Sales", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE: %v", err)
	}

	rights, err := a.MyRights("Public/Sales").Wait()
	if err != nil {
		t.Fatalf("alice MYRIGHTS on her own new mailbox: %v", err)
	}
	got := string(rights.Rights)
	if !strings.ContainsRune(got, 'a') {
		t.Errorf("the creator holds %q on the mailbox it just created -- nobody can administer it", got)
	}
	// The restriction the root expressed by leaving 't' out still holds; a fix
	// that granted full rights would make CREATE a way around the root ACL.
	if strings.ContainsRune(got, 't') {
		t.Errorf("the creator holds %q, which includes a right the root ACL withheld", got)
	}

	// The right is real, not just reported: the creator can administer.
	if err := a.SetACL("Public/Sales", "user=bob", imap.RightModificationReplace, imap.RightSet("lr")).Wait(); err != nil {
		t.Errorf("the creator cannot SETACL on its own mailbox: %v", err)
	}
}

// A creator that already inherits 'a' must not have anything written for it --
// the ordinary case, and the one where an extra entry would quietly turn an
// inherited grant into a pinned one that later root changes no longer reach.
func TestCreatorWithInheritedAdminIsLeftAlone(t *testing.T) {
	publicRoot, dial := publicNSServer(t)
	if err := os.WriteFile(filepath.Join(publicRoot, acl.RootFileName), []byte("user=alice lrswipkxtea\n"), 0o600); err != nil {
		t.Fatalf("seed root acl: %v", err)
	}

	a := dial("alice")
	if err := a.Create("Public/Ops", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE: %v", err)
	}

	// MaterialiseOnCreate copies the inherited entry verbatim; this asserts
	// nothing was added on top of it.
	body, err := os.ReadFile(filepath.Join(publicRoot, ".Ops", "yarilo-acl"))
	if err != nil {
		if os.IsNotExist(err) {
			return // nothing materialised at all is also "nothing added"
		}
		t.Fatalf("read mailbox acl: %v", err)
	}
	if n := strings.Count(string(body), "user=alice"); n > 1 {
		t.Errorf("the creator was written into the ACL %d times:\n%s", n, body)
	}
}
