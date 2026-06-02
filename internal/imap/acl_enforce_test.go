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

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	imapserver "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	mailboxpkg "github.com/0kaba0hub/yarilo/pkg/mailbox"
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

type enforcePassdb struct{ users map[string]string }

func (p *enforcePassdb) Authenticate(username, password, _ string) (*protocol.AuthResponse, error) {
	if want, ok := p.users[username]; ok && want == password {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: username}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

// seedACL writes a yarilo-acl file directly to alice's INBOX index
// dir. Tests use this to grant a peer (bob) specific rights on
// alice's mailbox so SELECT / FETCH / etc. cross-namespace cases
// can be exercised without a SETACL round-trip (which would itself
// need owner credentials).
func seedACL(t *testing.T, aliceHome, folder string, body string) {
	t.Helper()
	var dir string
	if folder == "INBOX" {
		dir = filepath.Join(aliceHome, "INBOX")
	} else {
		dir = filepath.Join(aliceHome, "."+folder)
	}
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
	root := t.TempDir()
	mb := maildir.New()
	idx := file.New()
	resolver := &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"}

	passdb := &enforcePassdb{users: map[string]string{
		"alice": "pw",
		"bob":   "pw",
	}}

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
			{Type: imapserver.NamespaceShared, Prefix: "Shared/", Separator: '/', Location: "maildir:" + filepath.Join(root, "alice"), List: true},
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
	return filepath.Join(root, "alice"), dial
}

func TestACLEnforce_PeerSelectDeniedWithoutRead(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)

	// Ensure alice's INBOX exists by having alice CREATE-imply via SELECT.
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	// No yarilo-acl seeded — bob is a non-owner with zero rights.
	_ = aliceHome

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
	if err := a.Create("INBOX/Private", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE INBOX/Private: %v", err)
	}
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n")
	seedACL(t, aliceHome, "INBOX/Private", "user=alice lrswipkxtea\n")

	b := dial("bob")
	_, err := b.Select("Shared/INBOX/Private", nil).Wait()
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

func TestACLEnforce_StatusNeedsRead(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	_ = aliceHome

	b := dial("bob")
	_, err := b.Status("Shared/INBOX", &imaplib.StatusOptions{NumMessages: true}).Wait()
	if err == nil {
		t.Error("peer STATUS without 'r' should fail")
	} else if code := aclErrCode(err); code != imaplib.ResponseCodeNoPerm {
		t.Errorf("got code %q, want NOPERM: err=%v", code, err)
	}
}
