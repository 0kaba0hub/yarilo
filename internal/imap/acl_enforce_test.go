package imap_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/userstate/acl"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// enforceServer brings up an IMAP server with ACL enforcement enabled
// and two users. The owner ("alice") fully owns their personal home
// — INBOX is auto-granted via the personal-namespace path. The
// peer ("bob") logs in to their own personal home (so for any folder
// reached via Shared/, the peer is a non-owner relative to that
// namespace handle's ownership).
//
// Returns:
//   - aliceDir: alice's storage root, useful when seeding yarilo-acl
//     files for tests that need to grant bob access to alice's data.
//   - dial: factory that returns a fresh authenticated client for the
//     given user — every test gets its own connection so leftover
//     state cannot bleed.
func enforceServer(t *testing.T) (aliceDir string, dial func(user string) *imapclient.Client) {
	t.Helper()

	root := t.TempDir()
	mb := maildir.New()
	idx := file.New()
	resolver := &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"}

	passdb := &enforcePassdb{users: map[string]string{
		"alice": "pw",
		"bob":   "pw",
	}}

	srv := imapserver.New(imapserver.Options{
		Mailbox:    mb,
		Index:      idx,
		Resolver:   resolver,
		Auth:       passdb,
		ACLEnabled: true,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck

	dial = func(user string) *imapclient.Client {
		t.Helper()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })

		c := imapclient.New(conn, nil)
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("WaitGreeting: %v", err)
		}
		if err := c.Login(user, "pw").Wait(); err != nil {
			t.Fatalf("Login(%q): %v", user, err)
		}
		t.Cleanup(func() { c.Logout().Wait() }) //nolint:errcheck
		return c
	}
	return filepath.Join(root, "alice"), dial
}

type enforcePassdb struct {
	users  map[string]string
	groups map[string][]string
}

func (p *enforcePassdb) Authenticate(username, password, _, _ string) (*protocol.AuthResponse, error) {
	if want, ok := p.users[username]; ok && want == password {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: username, Groups: p.groups[username]}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

// seedACL writes a yarilo-acl file directly to alice's INBOX index
// dir. Tests use this to grant a peer (bob) specific rights on
// alice's mailbox so SELECT / FETCH / etc. cross-namespace cases
// can be exercised without a SETACL round-trip (which would itself
// need owner credentials).
// seedRootACL writes the namespace-root ACL — the file that did not exist
// before #1091, and the only place a top-level create right can go.
func seedRootACL(t *testing.T, aliceHome, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(aliceHome, acl.RootFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", acl.RootFileName, err)
	}
}

func seedACL(t *testing.T, aliceHome, folder string, body string) {
	t.Helper()
	dir := filepath.Join(aliceHome, mailboxpkg.FolderSubpath("maildir", folder, folder, "."))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "yarilo-acl"), []byte(body), 0o600); err != nil {
		t.Fatalf("write yarilo-acl: %v", err)
	}
}

func TestACLEnforce_OwnerSelectsOwnInboxWithoutACL(t *testing.T) {
	_, dial := enforceServer(t)
	c := dial("alice")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Errorf("owner SELECT of own INBOX: %v", err)
	}
}

func TestACLEnforce_OwnerAppendsAndExpungesWithoutACL(t *testing.T) {
	_, dial := enforceServer(t)
	c := dial("alice")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	// Owner: no ACL file → still allowed everywhere.
	if err := c.Expunge().Close(); err != nil {
		t.Errorf("owner EXPUNGE: %v", err)
	}
}

func aclErrCode(err error) imaplib.ResponseCode {
	if err == nil {
		return ""
	}
	var ie *imaplib.Error
	if errors.As(err, &ie) {
		return ie.Code
	}
	return ""
}

func TestACLEnforce_DisabledLetsEverythingThrough(t *testing.T) {
	// With ACLEnabled=false we still load and serve files, but no
	// check fires. Spin a server with the flag off and confirm a
	// blocked-by-rights owner-foreign mailbox is still reachable.
	root := t.TempDir()
	mb := maildir.New()
	idx := file.New()
	resolver := &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"}
	srv := imapserver.New(imapserver.Options{
		Mailbox:  mb,
		Index:    idx,
		Resolver: resolver,
		Auth: &enforcePassdb{users: map[string]string{
			"alice": "pw",
		}},
		ACLEnabled: false,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	c.WaitGreeting()                     //nolint:errcheck
	c.Login("alice", "pw").Wait()        //nolint:errcheck
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Errorf("SELECT with ACL disabled: %v", err)
	}
}

// The cross-namespace tests below rely on a shared namespace pointing
// at alice's home root. Bob's session sees Shared/alice/<folder>; the
// effective-rights computation runs against alice's INBOX yarilo-acl.

func enforceServerWithShared(t *testing.T) (aliceHome string, dial func(user string) *imapclient.Client) {
	t.Helper()
	return enforceServerWithSharedGroups(t, nil)
}

// enforceServerWithSharedGroups is the same server with supplementary groups on
// the peer, so a grant to $group can be exercised the way a userdb would
// deliver it.
func enforceServerWithSharedGroups(t *testing.T, groups map[string][]string) (aliceHome string, dial func(user string) *imapclient.Client) {
	t.Helper()
	root := t.TempDir()
	mb := maildir.New()
	idx := file.New()
	resolver := &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"}

	passdb := &enforcePassdb{users: map[string]string{
		"alice": "pw",
		"bob":   "pw",
	}, groups: groups}

	// Shared namespace anchored at alice's home so bob can reach
	// "Shared/INBOX" → alice's INBOX. This is a synthetic mapping
	// for the test, not a production layout.
	srv := imapserver.New(imapserver.Options{
		Mailbox:    mb,
		Index:      idx,
		Resolver:   resolver,
		Auth:       passdb,
		ACLEnabled: true,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: true},
			{Type: imapserver.NamespaceShared, Prefix: "Shared/", Separator: '/', Location: "maildir:" + filepath.Join(root, "alice", "Maildir"), List: true},
		},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck

	dial = func(user string) *imapclient.Client {
		t.Helper()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })

		c := imapclient.New(conn, nil)
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("WaitGreeting: %v", err)
		}
		if err := c.Login(user, "pw").Wait(); err != nil {
			t.Fatalf("Login(%q): %v", user, err)
		}
		t.Cleanup(func() { c.Logout().Wait() }) //nolint:errcheck
		return c
	}
	return filepath.Join(root, "alice", "Maildir"), dial
}

// A peer with no rights at all must not learn the mailbox is there. The reply
// is the one an absent mailbox gets, so the two cannot be told apart -- which
// is what closes the enumeration oracle in a shared namespace (#1068).
func TestACLEnforce_PeerWithoutLookupCannotTellTheMailboxExists(t *testing.T) {
	_, dial := enforceServerWithShared(t)

	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	// No yarilo-acl seeded — bob is a non-owner with zero rights.

	b := dial("bob")
	_, errPresent := b.Select("Shared/INBOX", nil).Wait()
	if errPresent == nil {
		t.Fatal("peer SELECT without rights should fail")
	}
	_, errAbsent := b.Select("Shared/NoSuchMailboxAtAll", nil).Wait()
	if errAbsent == nil {
		t.Fatal("SELECT of an absent mailbox should fail")
	}

	if got := aclErrCode(errPresent); got != imaplib.ResponseCodeNonExistent {
		t.Errorf("existing-but-unreadable answered %q, want NONEXISTENT: %v", got, errPresent)
	}
	// The property, not the code: the two replies must be identical, or the
	// difference between them is the disclosure.
	if errPresent.Error() != errAbsent.Error() {
		t.Errorf("a peer can tell a mailbox it may not see from one that is not there:\n present: %v\n absent:  %v",
			errPresent, errAbsent)
	}
}

// A peer that *does* hold the lookup right already knows the mailbox exists, so
// the refusal names the right it lacks. This is the case that distinguishes the
// policy from "always answer NONEXISTENT", which would be simpler and useless.
func TestACLEnforce_PeerWithLookupIsToldWhichRightIsMissing(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)

	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	seedACL(t, aliceHome, "INBOX", "user=bob l\n") // lookup, but no read

	b := dial("bob")
	_, err := b.Select("Shared/INBOX", nil).Wait()
	if err == nil {
		t.Fatal("peer SELECT without 'r' should fail")
	}
	if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("got code %q, want NOPERM (%q): err=%v", code, imaplib.ResponseCodeNoPerm, err)
	}
	if !strings.Contains(err.Error(), "'r'") {
		t.Errorf("error should name missing right 'r', got %v", err)
	}
}

func TestACLEnforce_PeerSelectAllowedWithRead(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n")

	b := dial("bob")
	if _, err := b.Select("Shared/INBOX", nil).Wait(); err != nil {
		t.Errorf("peer SELECT with 'r': %v", err)
	}
}

func TestACLEnforce_PeerAppendDeniedWithoutPost(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	// Grant bob read only — APPEND on a shared namespace needs 'p'.
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n")

	b := dial("bob")
	body := []byte("From: x@y\r\nSubject: t\r\n\r\nbody\r\n")
	ac := b.Append("Shared/INBOX", int64(len(body)), nil)
	_, _ = ac.Write(body)
	_ = ac.Close()
	if _, err := ac.Wait(); err == nil {
		t.Fatal("peer APPEND without 'p' should fail")
	} else if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("got code %q, want NOPERM: err=%v", code, err)
	}
}

func TestACLEnforce_PeerAppendAllowedWithPost(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	// Shared namespace APPEND requires 'p', not 'i'.
	seedACL(t, aliceHome, "INBOX", "user=bob lrp\n")

	b := dial("bob")
	body := []byte("From: x@y\r\nSubject: t\r\n\r\nbody\r\n")
	ac := b.Append("Shared/INBOX", int64(len(body)), nil)
	if _, err := ac.Write(body); err != nil {
		t.Fatalf("APPEND write: %v", err)
	}
	if err := ac.Close(); err != nil {
		t.Fatalf("APPEND close: %v", err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Errorf("peer APPEND with 'p': %v", err)
	}
}

func TestACLEnforce_StoreFlagsRespectCategoryRights(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	body := []byte("From: x@y\r\nSubject: t\r\n\r\nbody\r\n")
	ac := a.Append("INBOX", int64(len(body)), nil)
	_, _ = ac.Write(body)
	_ = ac.Close()
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("alice APPEND: %v", err)
	}
	// Grant bob l + r + s — bob can mark \Seen but not \Deleted, not \Flagged.
	seedACL(t, aliceHome, "INBOX", "user=bob lrs\n")

	b := dial("bob")
	if _, err := b.Select("Shared/INBOX", nil).Wait(); err != nil {
		t.Fatalf("bob SELECT: %v", err)
	}

	// STORE \Seen — allowed ('s').
	uidSet := imaplib.UIDSetNum(1)
	sCmd := b.Store(uidSet, &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagSeen}}, nil)
	if err := sCmd.Close(); err != nil {
		t.Errorf("STORE \\Seen with 's': %v", err)
	}

	// STORE \Deleted — denied ('t' missing).
	tCmd := b.Store(uidSet, &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}}, nil)
	err := tCmd.Close()
	if err == nil {
		t.Error("STORE \\Deleted without 't' should fail")
	} else if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("got code %q, want NOPERM: err=%v", code, err)
	}

	// STORE \Flagged — denied ('w' missing).
	wCmd := b.Store(uidSet, &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagFlagged}}, nil)
	err = wCmd.Close()
	if err == nil {
		t.Error("STORE \\Flagged without 'w' should fail")
	}
}

func TestACLEnforce_ExpungeNeedsExpunge(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n")

	b := dial("bob")
	if _, err := b.Select("Shared/INBOX", nil).Wait(); err != nil {
		t.Fatalf("bob SELECT: %v", err)
	}
	err := b.Expunge().Close()
	if err == nil {
		t.Error("peer EXPUNGE without 'e' should fail")
	} else if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("got code %q, want NOPERM: err=%v", code, err)
	}
}

func TestACLEnforce_CopyNeedsInsertOnDest(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	// Seed a message in alice's INBOX so bob has something to COPY.
	body := []byte("From: x@y\r\nSubject: t\r\n\r\nbody\r\n")
	ac := a.Append("INBOX", int64(len(body)), nil)
	_, _ = ac.Write(body)
	_ = ac.Close()
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("alice APPEND: %v", err)
	}
	// Bob has read on alice's INBOX (source) but no rights on dest
	// — Shared/INBOX is alice's INBOX itself; for a destination-
	// denial check we COPY back into Shared/INBOX which needs 'p'
	// (shared namespace) that bob does not have.
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n")

	b := dial("bob")
	if _, err := b.Select("Shared/INBOX", nil).Wait(); err != nil {
		t.Fatalf("bob SELECT: %v", err)
	}
	_, err := b.Copy(imaplib.UIDSetNum(1), "Shared/INBOX").Wait()
	if err == nil {
		t.Fatal("peer COPY without dest 'p' should fail")
	} else if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("got code %q, want NOPERM: err=%v", code, err)
	}
}

func TestACLEnforce_InheritsFromParent(t *testing.T) {
	// Grant bob 'r' on alice's INBOX → bob can also SELECT
	// INBOX/Subfolder via inheritance (no explicit ACL on the
	// child, walk hits parent).
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	if err := a.Create("INBOX/News", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE INBOX/News: %v", err)
	}
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n")

	b := dial("bob")
	if _, err := b.Select("Shared/INBOX/News", nil).Wait(); err != nil {
		t.Errorf("peer SELECT inheriting 'r' from parent: %v", err)
	}
}

func TestACLEnforce_LeafACLOverridesInheritedParent(t *testing.T) {
	// Parent grants 'lr'; leaf has its own ACL that does NOT grant
	// bob anything → bob is denied on the leaf even though the
	// parent would grant.
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	if err := a.Create("INBOX.Private", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE INBOX.Private: %v", err)
	}
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n")
	seedACL(t, aliceHome, "INBOX.Private", "user=alice lrswipkxtea\n")

	b := dial("bob")
	_, err := b.Select("Shared/INBOX.Private", nil).Wait()
	if err == nil {
		t.Error("peer SELECT on leaf with restrictive ACL should fail (leaf wins)")
	}
}

func TestACLEnforce_CreateNeedsKOnParent(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	// Bob has lr on alice's INBOX — read but not create.
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n")

	b := dial("bob")
	err := b.Create("Shared/INBOX/Hijack", nil).Wait()
	if err == nil {
		t.Fatal("peer CREATE without 'k' on parent should fail")
	}
	if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("got code %q, want NOPERM: err=%v", code, err)
	}
}

func TestACLEnforce_CreateAllowedWithKOnParent(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	seedACL(t, aliceHome, "INBOX", "user=bob lrk\n")

	b := dial("bob")
	if err := b.Create("Shared/INBOX/Box", nil).Wait(); err != nil {
		t.Errorf("peer CREATE with 'k' on parent: %v", err)
	}
}

func TestACLEnforce_DeleteNeedsX(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	if err := a.Create("INBOX/Temp", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE INBOX/Temp: %v", err)
	}
	// Bob can SELECT (inherits 'lr') but cannot DELETE — needs 'x'.
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n")

	b := dial("bob")
	err := b.Delete("Shared/INBOX/Temp").Wait()
	if err == nil {
		t.Fatal("peer DELETE without 'x' should fail")
	}
	if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("got code %q, want NOPERM: err=%v", code, err)
	}
}

func TestACLEnforce_DeleteAllowedWithX(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	if err := a.Create("INBOX/Temp", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE INBOX/Temp: %v", err)
	}
	// 'x' on parent inherits to children.
	seedACL(t, aliceHome, "INBOX", "user=bob lrx\n")

	b := dial("bob")
	if err := b.Delete("Shared/INBOX/Temp").Wait(); err != nil {
		t.Errorf("peer DELETE with 'x': %v", err)
	}
}

func TestACLEnforce_RenameNeedsXOnSourceAndKOnDestParent(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	if err := a.Create("INBOX/Source", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE INBOX/Source: %v", err)
	}
	// Bob has only 'lr' — RENAME should fail at the 'x' check.
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n")

	b := dial("bob")
	err := b.Rename("Shared/INBOX/Source", "Shared/INBOX/Renamed", nil).Wait()
	if err == nil {
		t.Fatal("peer RENAME without 'x' should fail")
	}
	if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("got code %q, want NOPERM: err=%v", code, err)
	}
}

func TestACLEnforce_RenameAllowedWithBothRights(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	if err := a.Create("INBOX/Source", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE INBOX/Source: %v", err)
	}
	// 'x' (for source delete) + 'k' (for dest-parent create), both
	// inherited from the parent ACL.
	seedACL(t, aliceHome, "INBOX", "user=bob lrxk\n")

	b := dial("bob")
	if err := b.Rename("Shared/INBOX/Source", "Shared/INBOX/Renamed", nil).Wait(); err != nil {
		t.Errorf("peer RENAME with x+k: %v", err)
	}
}

func TestACLEnforce_TopLevelCreateNeedsRootK(t *testing.T) {
	// Bob tries CREATE "Shared/NewTop" — the namespace-relative name
	// is "NewTop" with no separator, so the parent is the namespace
	// root. Without a root ACL grant, the request must NOPERM even
	// for a non-owner that has rights on other mailboxes.
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	// Give bob lots of rights on INBOX so the failure mode is
	// definitely the missing root grant, not some other denial.
	seedACL(t, aliceHome, "INBOX", "user=bob lrwsipkxtea\n")

	b := dial("bob")
	err := b.Create("Shared/NewTop", nil).Wait()
	if err == nil {
		t.Fatal("peer top-level CREATE without root 'k' should fail")
	}
	if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("got code %q, want NOPERM: err=%v", code, err)
	}
}

// Namespace-root default ACL grants for maildir were tested here previously,
// but with INBOX now at the maildir root the local namespace-root default
// collides with INBOX and is disabled. For
// maildir, root-level defaults come from a global ACL or acl_defaults_from_inbox
// — deferred features. The root-default mechanism itself is covered on mdbox in
// internal/userstate/acl (TestStore_EffectiveForFallsThroughToRoot*).

func TestACLEnforce_StatusNeedsRead(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	// Lookup but not read: the test is about the missing 'r'. With no rights
	// at all the answer is NONEXISTENT instead, which is a different claim and
	// has its own test (#1068).
	seedACL(t, aliceHome, "INBOX", "user=bob l\n")

	b := dial("bob")
	_, err := b.Status("Shared/INBOX", &imaplib.StatusOptions{NumMessages: true}).Wait()
	if err == nil {
		t.Error("peer STATUS without 'r' should fail")
	} else if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("got code %q, want NOPERM: err=%v", code, err)
	}
}

// sharedServerDial builds the two-user shared-namespace enforce server with an
// explicit acl_defaults_from_inbox toggle, returning alice's home and a dial
// factory. Mirrors enforceServerWithShared but parameterises the flag.
func sharedServerDial(t *testing.T, defaultsFromInbox bool) (aliceHome string, dial func(user string) *imapclient.Client) {
	t.Helper()
	root := t.TempDir()
	resolver := &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"}
	passdb := &enforcePassdb{users: map[string]string{"alice": "pw", "bob": "pw"}}
	md, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("open memory dict: %v", err)
	}
	t.Cleanup(func() { _ = md.Close() })
	srv := imapserver.New(imapserver.Options{
		Mailbox:              maildir.New(),
		Index:                file.New(),
		Resolver:             resolver,
		Auth:                 passdb,
		ACLEnabled:           true,
		ACLDefaultsFromInbox: defaultsFromInbox,
		MetadataDict:         md,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: true},
			{Type: imapserver.NamespaceShared, Prefix: "Shared/", Separator: '/', Location: "maildir:" + filepath.Join(root, "alice", "Maildir"), List: true},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck
	dial = func(user string) *imapclient.Client {
		t.Helper()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		c := imapclient.New(conn, nil)
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("WaitGreeting: %v", err)
		}
		if err := c.Login(user, "pw").Wait(); err != nil {
			t.Fatalf("Login(%q): %v", user, err)
		}
		t.Cleanup(func() { c.Logout().Wait() }) //nolint:errcheck
		return c
	}
	return filepath.Join(root, "alice", "Maildir"), dial
}

// TestACLEnforce_MaildirRootDefaultFromInbox exercises acl_defaults_from_inbox
// end-to-end: with the flag, a peer's rights on a non-INBOX maildir folder that
// has no ACL of its own are inherited from the owner's INBOX ACL. This is the
// coverage that was dropped when maildir INBOX moved to the maildir root (the
// local namespace-root default collides with INBOX and is disabled).
func TestACLEnforce_MaildirRootDefaultFromInbox(t *testing.T) {
	aliceHome, dial := sharedServerDial(t, true)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	if err := a.Create("Projects", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE Projects: %v", err)
	}
	// Grant bob read on the *root default* by seeding INBOX's ACL only.
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n")

	b := dial("bob")
	// Projects has no ACL of its own → rights come from the INBOX default.
	if _, err := b.Select("Shared/Projects", nil).Wait(); err != nil {
		t.Errorf("peer SELECT with INBOX-default 'r' should succeed: %v", err)
	}
}

// TestACLEnforce_MaildirNoRootDefaultWithoutFlag is the contrast: without
// acl_defaults_from_inbox, maildir has no root default source, so the INBOX
// ACL does not extend to sibling folders and the peer is denied.
func TestACLEnforce_MaildirNoRootDefaultWithoutFlag(t *testing.T) {
	aliceHome, dial := sharedServerDial(t, false)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	if err := a.Create("Projects", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE Projects: %v", err)
	}
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n")

	b := dial("bob")
	// bob holds rights on INBOX and none on Projects, so Projects must look
	// absent to him: rights on one mailbox must not disclose the existence of
	// its siblings (#1068).
	if _, err := b.Select("Shared/Projects", nil).Wait(); err == nil {
		t.Fatal("peer SELECT without a root default should fail")
	} else if code := aclErrCode(err); code != imaplib.ResponseCodeNonExistent {
		t.Errorf("got code %q, want NONEXISTENT: err=%v", code, err)
	}
}

// TestACLEnforce_GlobalGrant exercises the config-driven global ACL end-to-end:
// a global rule grants a peer read on every mailbox, so the peer can SELECT a
// shared folder that has no per-mailbox ACL of its own.
func TestACLEnforce_GlobalGrant(t *testing.T) {
	global, err := acl.NewGlobal([]config.GlobalACLRule{
		{Mailbox: "*", Entries: []config.GlobalACLEntry{{Identifier: "user=bob", Rights: "lr"}}},
	})
	if err != nil {
		t.Fatalf("NewGlobal: %v", err)
	}
	root := t.TempDir()
	resolver := &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"}
	passdb := &enforcePassdb{users: map[string]string{"alice": "pw", "bob": "pw"}}
	srv := imapserver.New(imapserver.Options{
		Mailbox:    maildir.New(),
		Index:      file.New(),
		Resolver:   resolver,
		Auth:       passdb,
		ACLEnabled: true,
		ACLGlobal:  global,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: true},
			{Type: imapserver.NamespaceShared, Prefix: "Shared/", Separator: '/', Location: "maildir:" + filepath.Join(root, "alice", "Maildir"), List: true},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck
	dial := func(user string) *imapclient.Client {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		c := imapclient.New(conn, nil)
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("greeting: %v", err)
		}
		if err := c.Login(user, "pw").Wait(); err != nil {
			t.Fatalf("login %s: %v", user, err)
		}
		t.Cleanup(func() { c.Logout().Wait() }) //nolint:errcheck
		return c
	}
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	if err := a.Create("Reports", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE Reports: %v", err)
	}
	// No per-mailbox ACL seeded anywhere — rights come solely from the global rule.
	b := dial("bob")
	if _, err := b.Select("Shared/Reports", nil).Wait(); err != nil {
		t.Errorf("peer SELECT with global 'r' should succeed: %v", err)
	}
}

// TestACLEnforce_ListHidesWithoutLookup verifies RFC 4314 LIST hiding: a peer
// sees only shared folders it has the 'l' right on; a no-lookup folder that is
// an ancestor of a visible one survives as a \NoSelect placeholder so the path
// to the visible child stays navigable.
func TestACLEnforce_ListHidesWithoutLookup(t *testing.T) {
	aliceHome, dial := sharedServerDial(t, false)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	for _, f := range []string{"Secret", "Public", "Work", "Work.Report"} {
		if err := a.Create(f, nil).Wait(); err != nil {
			t.Fatalf("alice CREATE %s: %v", f, err)
		}
	}
	// bob gets lookup on Public and the nested Work.Report only.
	seedACL(t, aliceHome, "Public", "user=bob lr\n")
	seedACL(t, aliceHome, "Work.Report", "user=bob lr\n")

	b := dial("bob")
	data, err := b.List("", "*", nil).Collect()
	if err != nil {
		t.Fatalf("bob LIST: %v", err)
	}
	seen := map[string][]imaplib.MailboxAttr{}
	for _, m := range data {
		seen[m.Mailbox] = m.Attrs
	}
	for _, want := range []string{"Shared/Public", "Shared/Work", "Shared/Work/Report"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("%q should be visible; got %v", want, keysOf(seen))
		}
	}
	for _, hidden := range []string{"Shared/Secret", "Shared/INBOX"} {
		if _, ok := seen[hidden]; ok {
			t.Errorf("%q must be hidden (no lookup right)", hidden)
		}
	}
	hasAttr := func(as []imaplib.MailboxAttr, want imaplib.MailboxAttr) bool {
		for _, x := range as {
			if x == want {
				return true
			}
		}
		return false
	}
	if !hasAttr(seen["Shared/Work"], imaplib.MailboxAttrNoSelect) {
		t.Errorf("Shared/Work should be a \\NoSelect placeholder, attrs=%v", seen["Shared/Work"])
	}
}

func keysOf(m map[string][]imaplib.MailboxAttr) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestACLEnforce_MyRightsUsesInheritance verifies MYRIGHTS resolves the full
// effective rights (ancestor inheritance), matching enforcement — not just the
// folder's own ACL file. bob has no ACL on the child, only on the parent.
func TestACLEnforce_MyRightsUsesInheritance(t *testing.T) {
	aliceHome, dial := sharedServerDial(t, false)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	for _, f := range []string{"Parent", "Parent.Child"} {
		if err := a.Create(f, nil).Wait(); err != nil {
			t.Fatalf("alice CREATE %s: %v", f, err)
		}
	}
	// Grant bob lr on the parent only; the child has no ACL of its own.
	seedACL(t, aliceHome, "Parent", "user=bob lr\n")

	b := dial("bob")
	data, err := b.MyRights("Shared/Parent/Child").Wait()
	if err != nil {
		t.Fatalf("bob MYRIGHTS: %v", err)
	}
	if got := sortedString(string(data.Rights)); got != "lr" {
		t.Errorf("inherited MYRIGHTS = %q, want lr (from the parent ACL)", got)
	}
}

// TestACLEnforce_MetadataRequiresRights verifies RFC 5464 mailbox METADATA is
// ACL-gated: a peer needs the 'l' right plus one access right (r/s/w/i/p) to
// read a shared mailbox's metadata; with no ACL it is denied.
func TestACLEnforce_MetadataRequiresRights(t *testing.T) {
	aliceHome, dial := sharedServerDial(t, false)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	if err := a.Create("Box", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE Box: %v", err)
	}
	v := []byte("shared-note")
	if err := a.SetMetadata("Box", map[string]*[]byte{"/shared/comment": &v}).Wait(); err != nil {
		t.Fatalf("alice SETMETADATA: %v", err)
	}
	a.Logout() //nolint:errcheck

	// bob has no ACL at all → the mailbox must look absent to him, not
	// merely forbidden (#1068).
	b := dial("bob")
	if _, err := b.GetMetadata("Shared/Box", []string{"/shared/comment"}, nil).Wait(); err == nil {
		t.Fatal("bob GETMETADATA without rights should fail")
	} else if code := aclErrCode(err); code != imaplib.ResponseCodeNonExistent {
		t.Errorf("got code %q, want NONEXISTENT: err=%v", code, err)
	}

	// With lookup but no access right, the refusal names what is missing: he
	// already knows the mailbox is there.
	seedACL(t, aliceHome, "Box", "user=bob l\n")
	if _, err := b.GetMetadata("Shared/Box", []string{"/shared/comment"}, nil).Wait(); err == nil {
		t.Fatal("bob GETMETADATA with only 'l' should fail")
	} else if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("with lookup: got code %q, want NOPERM: err=%v", code, err)
	}

	// Grant bob lr → the ACL gate now passes (the metadata command is
	// accepted rather than denied). Value round-trip across namespaces is a
	// separate metadata-keying concern, not what this test covers.
	seedACL(t, aliceHome, "Box", "user=bob lr\n")
	b2 := dial("bob")
	if _, err := b2.GetMetadata("Shared/Box", []string{"/shared/comment"}, nil).Wait(); err != nil {
		t.Errorf("bob GETMETADATA with lr should be permitted: %v", err)
	}
	// A peer with only lookup (no access right) is still denied.
	seedACL(t, aliceHome, "Box", "user=bob l\n")
	b3 := dial("bob")
	if _, err := b3.GetMetadata("Shared/Box", []string{"/shared/comment"}, nil).Wait(); err == nil {
		t.Error("bob GETMETADATA with only 'l' (no r/s/w/i/p) should fail")
	} else if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("got code %q, want NOPERM: err=%v", code, err)
	}
}

// TestACLEnforce_IgnoreACLBypasses verifies acl_ignore: a namespace flagged
// IgnoreACL is fully accessible even when ACL is enabled and the peer has no
// grant — no NOPERM, no LIST hiding.
func TestACLEnforce_IgnoreACLBypasses(t *testing.T) {
	root := t.TempDir()
	resolver := &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"}
	passdb := &enforcePassdb{users: map[string]string{"alice": "pw", "bob": "pw"}}
	srv := imapserver.New(imapserver.Options{
		Mailbox:    maildir.New(),
		Index:      file.New(),
		Resolver:   resolver,
		Auth:       passdb,
		ACLEnabled: true,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: true},
			{Type: imapserver.NamespaceShared, Prefix: "Open/", Separator: '/', Location: "maildir:" + filepath.Join(root, "alice", "Maildir"), List: true, IgnoreACL: true},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck
	dial := func(user string) *imapclient.Client {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		c := imapclient.New(conn, nil)
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("greeting: %v", err)
		}
		if err := c.Login(user, "pw").Wait(); err != nil {
			t.Fatalf("login %s: %v", user, err)
		}
		t.Cleanup(func() { c.Logout().Wait() }) //nolint:errcheck
		return c
	}
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	if err := a.Create("Vault", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE Vault: %v", err)
	}
	// bob has NO ACL anywhere, but the Open/ namespace ignores ACL.
	b := dial("bob")
	if _, err := b.Select("Open/Vault", nil).Wait(); err != nil {
		t.Errorf("acl_ignore namespace should be accessible without grants: %v", err)
	}
	// And LIST is not hidden — bob sees Vault under Open/.
	data, err := b.List("", "*", nil).Collect()
	if err != nil {
		t.Fatalf("bob LIST: %v", err)
	}
	seen := false
	for _, m := range data {
		if m.Mailbox == "Open/Vault" {
			seen = true
		}
	}
	if !seen {
		t.Error("acl_ignore namespace folder should be visible in LIST")
	}
}

// SETACL and GETACL must answer the same question the same way. The writing
// pair used to authorise themselves, honouring 'a' only from an explicit user=
// entry on the mailbox itself, so a peer granted it any other way could read
// the ACL and not write it -- one ACL saying the peer both has and has not got
// the right (#1107).
//
// The assertion is the difference between the two replies, not the wording of
// either: both are defensible alone, and it is the pair that is wrong.
func TestACLEnforce_AdminRightResolvesTheSameForReadsAndWrites(t *testing.T) {
	carol, err := imaplib.NewRightsIdentifierUsername("carol")
	if err != nil {
		t.Fatalf("NewRightsIdentifierUsername: %v", err)
	}

	cases := []struct {
		name   string
		groups map[string][]string
		// seed writes the grant that gives bob 'a' by some route other than a
		// bare user= entry on the folder itself.
		seed   func(t *testing.T, aliceHome string)
		folder string
	}{
		{
			name:   "through a group",
			groups: map[string][]string{"bob": {"admins"}},
			seed: func(t *testing.T, aliceHome string) {
				seedACL(t, aliceHome, "INBOX", "group=admins lra\n")
			},
			folder: "Shared/INBOX",
		},
		{
			name: "through anyone",
			seed: func(t *testing.T, aliceHome string) {
				seedACL(t, aliceHome, "INBOX", "anyone lra\n")
			},
			folder: "Shared/INBOX",
		},
		{
			// Inheritance: the grant is on the parent and the child has an ACL
			// of its own that says nothing about bob's admin right.
			name: "through an ancestor",
			seed: func(t *testing.T, aliceHome string) {
				seedACL(t, aliceHome, "Work", "user=bob lra\n")
				seedACL(t, aliceHome, "Work/Reports", "user=bob lr\n")
			},
			folder: "Shared/Work/Reports",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aliceHome, dial := enforceServerWithSharedGroups(t, tc.groups)

			a := dial("alice")
			for _, f := range []string{"Work", "Work/Reports"} {
				a.Create(f, nil).Wait() //nolint:errcheck — only Work/* cases need them
			}
			if _, err := a.Select("INBOX", nil).Wait(); err != nil {
				t.Fatalf("alice SELECT INBOX: %v", err)
			}
			tc.seed(t, aliceHome)

			b := dial("bob")
			_, readErr := b.GetACL(tc.folder).Wait()
			writeErr := b.SetACL(tc.folder, carol, imaplib.RightModificationReplace, imaplib.RightSet("lr")).Wait()

			switch {
			case readErr == nil && writeErr != nil:
				t.Errorf("GETACL %s allowed and SETACL refused on the same ACL: %v", tc.folder, writeErr)
			case readErr != nil && writeErr == nil:
				t.Errorf("SETACL %s allowed and GETACL refused on the same ACL: %v", tc.folder, readErr)
			case readErr != nil:
				t.Errorf("the peer holds 'a' by this route and neither command allowed it: %v", readErr)
			}
		})
	}
}

// The other direction, and the case that actually distinguishes the fix: a peer
// with no admin right by any route is refused by both commands.
//
// It is the sharp one because the old check compared s.userInfo.Username with
// h.userInfo.Username, and a shared namespace's handle carries the *session*
// user's name (dispatch.go: Username: personalUI.Username). The comparison was
// therefore a user against themselves -- true for every peer -- so SETACL and
// DELETEACL were open to anyone who could reach the mailbox at all.
func TestACLEnforce_WithoutAdminRightNeitherReadNorWrite(t *testing.T) {
	carol, err := imaplib.NewRightsIdentifierUsername("carol")
	if err != nil {
		t.Fatalf("NewRightsIdentifierUsername: %v", err)
	}
	aliceHome, dial := enforceServerWithSharedGroups(t, map[string][]string{"bob": {"users"}})

	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	// Lookup and read, and an admin grant to a group bob is not in.
	seedACL(t, aliceHome, "INBOX", "user=bob lr\ngroup=admins lra\n")

	b := dial("bob")
	if _, err := b.GetACL("Shared/INBOX").Wait(); err == nil {
		t.Error("GETACL answered a peer without the admin right")
	}
	if err := b.SetACL("Shared/INBOX", carol, imaplib.RightModificationReplace, imaplib.RightSet("lr")).Wait(); err == nil {
		t.Error("SETACL answered a peer without the admin right")
	}
}

// The sequence from #1111, end to end: an administrator who holds rights only
// at the namespace root creates a mailbox and grants a peer on it, and must
// still hold everything afterwards.
//
// It works because CREATE materialises what the mailbox inherits into its own
// ACL, so the administrator is named there before any SETACL can replace the
// grant they were acting under. Resolving inheritance live instead would make
// the per-mailbox file additive, which fixes this case by widening every other
// mailbox in the namespace -- the wrong direction for access control.
func TestACLEnforce_GrantingOnAMailboxDoesNotRevokeTheGranter(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)

	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	seedRootACL(t, aliceHome, "user=bob lrskxa\n")

	b := dial("bob")
	if err := b.Create("Shared/Matrix", nil).Wait(); err != nil {
		t.Fatalf("bob CREATE with 'k' on the root: %v", err)
	}
	carol, err := imaplib.NewRightsIdentifierUsername("carol")
	if err != nil {
		t.Fatalf("NewRightsIdentifierUsername: %v", err)
	}
	if err := b.SetACL("Shared/Matrix", carol, imaplib.RightModificationReplace, imaplib.RightSet("l")).Wait(); err != nil {
		t.Fatalf("bob SETACL: %v", err)
	}

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"SELECT", func() error { _, err := b.Select("Shared/Matrix", nil).Wait(); return err }},
		{"GETACL", func() error { _, err := b.GetACL("Shared/Matrix").Wait(); return err }},
		{"SETACL again", func() error {
			return b.SetACL("Shared/Matrix", carol, imaplib.RightModificationReplace, imaplib.RightSet("lr")).Wait()
		}},
		{"DELETE", func() error { return b.Delete("Shared/Matrix").Wait() }},
	} {
		if err := tc.run(); err != nil {
			t.Errorf("after granting a peer, %s fails for the granter: %v", tc.name, err)
		}
	}
}

// The other direction, which the merge model could not keep: a per-mailbox ACL
// stays exhaustive. An identifier the root names and the mailbox does not is
// not reachable there, so restriction by omission still restricts.
func TestACLEnforce_AMailboxACLStaysExhaustive(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)

	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	if err := a.Create("Shared/Private", nil).Wait(); err == nil {
		// alice is not the owner of a shared namespace either; seed and retry.
		t.Log("unexpected success without a grant")
	}
	seedRootACL(t, aliceHome, "user=alice lrskxa\nuser=bob lrs\n")
	if err := a.Create("Shared/Private", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE: %v", err)
	}
	// The mailbox is then narrowed on purpose: only alice.
	seedACL(t, aliceHome, "Private", "user=alice lrswipkxtea\n")

	b := dial("bob")
	if _, err := b.Select("Shared/Private", nil).Wait(); err == nil {
		t.Error("bob reached a mailbox whose ACL leaves him out — omission stopped restricting")
	}
}

// DELETEACL must answer a caller without 'a' the same way whichever mailbox
// they name. It did not: the callback returned early when the mailbox had no
// ACL of its own, before the rights check, so a mailbox with nothing to delete
// answered OK while one carrying an entry answered NOPERM. That tells a caller
// who may not administer these mailboxes which of them has an ACL at all --
// the oracle #1085 closed, in the one command still answering it (#1109).
//
// Asserted as the difference between the two replies rather than as a code: a
// server leaking through wording would pass a check on the code alone.
func TestACLEnforce_DeleteACLDoesNotRevealWhetherAnACLExists(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)

	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	// bob may see and read both mailboxes and administer neither: the rights
	// come from the namespace root, which both inherit.
	seedRootACL(t, aliceHome, "user=alice lrskxa\nuser=bob lr\n")
	if err := a.Create("Shared/WithACL", nil).Wait(); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if err := a.Create("Shared/Bare", nil).Wait(); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	// One carries an ACL of its own; the other has none.
	seedACL(t, aliceHome, "WithACL", "user=bob lr\nuser=carol lr\n")
	removeACL(t, aliceHome, "Bare")

	carol, err := imaplib.NewRightsIdentifierUsername("carol")
	if err != nil {
		t.Fatalf("NewRightsIdentifierUsername: %v", err)
	}

	b := dial("bob")
	errWith := b.DeleteACL("Shared/WithACL", carol).Wait()
	errBare := b.DeleteACL("Shared/Bare", carol).Wait()

	if errWith == nil || errBare == nil {
		t.Fatalf("DELETEACL succeeded without the admin right: with=%v bare=%v", errWith, errBare)
	}
	if comparableACLError(errWith) != comparableACLError(errBare) {
		t.Errorf("DELETEACL tells a mailbox with an ACL from one without:\n  with: %v\n  bare: %v",
			errWith, errBare)
	}
}

// removeACL deletes a mailbox's own ACL file, leaving it to inherit. CREATE
// materialises one (#1111), so a test that needs a bare mailbox has to say so.
func removeACL(t *testing.T, aliceHome, folder string) {
	t.Helper()
	path := filepath.Join(aliceHome, mailboxpkg.FolderSubpath("maildir", folder, folder, "."), "yarilo-acl")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove %s: %v", path, err)
	}
}

// comparableACLError strips the tag so two replies to the same session can be
// compared to each other rather than to a constant.
func comparableACLError(err error) string {
	if err == nil {
		return "<ok>"
	}
	line := err.Error()
	if _, rest, ok := strings.Cut(line, " "); ok && strings.HasPrefix(line, "imap:") {
		line = rest
	}
	return line
}

// A folder created in a decomposed (NFD) form must resolve, list and index as
// one folder, because the name is normalised once at dispatch before it reaches
// any tree. Under the pre-#1113 arrangement the mail tree normalised and the
// index tree did not, so the same mailbox had two directories (#1092).
//
// This test declares what it needs rather than disappearing where it cannot
// run: on a filesystem that composes names on creation (APFS on macOS, the
// developer machine) it cannot tell the code path from the filesystem's own
// normalisation, so it skips and says so. On the CI filesystem (self-hosted
// Linux, which does not compose) it carries the weight -- and production is
// Linux, so it guards where it matters. Deleting it for being unreliable on the
// developer machine would remove the only coverage from where it is reliable.
func TestACLEnforce_DecomposedNameResolvesToOneFolder(t *testing.T) {
	aliceDir, dial := enforceServer(t)
	c := dial("alice")

	const (
		composed = "Rendezé"  // 'é' precomposed (stand-in non-ASCII name)
		decomp   = "Rendezé" // 'e' + combining acute
	)
	if composed == decomp {
		t.Fatal("fixture spellings are identical")
	}

	if err := c.Create(decomp, nil).Wait(); err != nil {
		t.Fatalf("CREATE %q: %v", decomp, err)
	}

	// Precondition: did the filesystem keep the decomposed spelling? If the
	// mail directory came back composed, the FS normalised on creation and this
	// test cannot distinguish the code from the filesystem -- skip honestly.
	entries, err := os.ReadDir(aliceDir)
	if err != nil {
		t.Fatalf("read %s: %v", aliceDir, err)
	}
	var sawDecomposed bool
	for _, e := range entries {
		if strings.Contains(e.Name(), "́") {
			sawDecomposed = true
		}
	}
	if !sawDecomposed {
		t.Skip("filesystem composes names on creation (APFS); this test distinguishes the code path only where the FS does not")
	}

	// The FS preserves the decomposed bytes, so the dispatch normalisation is
	// the only thing that can make the composed spelling reach the same folder.
	if _, err := c.Select(composed, nil).Wait(); err != nil {
		t.Errorf("SELECT of the composed spelling did not reach the folder: %v — dispatch did not normalise", err)
	}
	listed, err := c.List("", decomp, nil).Collect()
	if err != nil {
		t.Fatalf("LIST: %v", err)
	}
	if len(listed) != 1 {
		t.Errorf("LIST returned %d entries for one mailbox: %+v", len(listed), listed)
	}
}
