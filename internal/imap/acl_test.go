package imap_test

import (
	"net"
	"reflect"
	"sort"
	"strings"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// aclPassdb is a local single-user passdb so acl_test.go does not
// depend on the unexported stubPassdb sitting beside server_test.go.
type aclPassdb struct{ user, pass string }

func (a *aclPassdb) Authenticate(username, password, _, _ string) (*protocol.AuthResponse, error) {
	if username == a.user && password == a.pass {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: username}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

// startACLServer brings up an IMAP server with ACL enabled / disabled
// per the flag and returns an authenticated client.
func startACLServer(t *testing.T, aclEnabled bool) *imapclient.Client {
	t.Helper()

	dir := t.TempDir()
	mb := maildir.New()
	idx := file.New()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}

	srv := imapserver.New(imapserver.Options{
		Mailbox:    mb,
		Index:      idx,
		Resolver:   resolver,
		Auth:       &aclPassdb{user: "alice@test.com", pass: "pw"},
		ACLEnabled: aclEnabled,
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
	if err := c.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting: %v", err)
	}
	if err := c.Login("alice@test.com", "pw").Wait(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	t.Cleanup(func() { c.Logout().Wait() }) //nolint:errcheck
	return c
}

func TestACL_CapabilityAdvertised(t *testing.T) {
	c := startACLServer(t, true)
	caps := c.Caps()
	if !caps.Has("ACL") {
		t.Errorf("CAPABILITY missing ACL: %v", caps)
	}
}

func TestACL_GetEmptyMailboxReturnsOwnerEntry(t *testing.T) {
	c := startACLServer(t, true)
	data, err := c.GetACL("INBOX").Wait()
	if err != nil {
		t.Fatalf("GETACL: %v", err)
	}
	if data.Mailbox != "INBOX" {
		t.Errorf("mailbox = %q, want INBOX", data.Mailbox)
	}
	alice, _ := imaplib.NewRightsIdentifierUsername("alice@test.com")
	if got := string(data.Rights[alice]); got != string(mailbox.FullRights) {
		t.Errorf("owner implicit rights = %q, want %q", got, mailbox.FullRights)
	}
}

func TestACL_SetAddGetRoundTrip(t *testing.T) {
	c := startACLServer(t, true)
	alice, _ := imaplib.NewRightsIdentifierUsername("alice@test.com")
	bob, err := imaplib.NewRightsIdentifierUsername("bob@test.com")
	if err != nil {
		t.Fatalf("NewRightsIdentifierUsername: %v", err)
	}
	if err := c.SetACL("INBOX", bob, imaplib.RightModificationReplace, imaplib.RightSet("lrws")).Wait(); err != nil {
		t.Fatalf("SETACL: %v", err)
	}
	data, err := c.GetACL("INBOX").Wait()
	if err != nil {
		t.Fatalf("GETACL: %v", err)
	}
	want := map[imaplib.RightsIdentifier]imaplib.RightSet{
		alice: imaplib.RightSet(mailbox.FullRights), // implicit owner grant
		bob:   imaplib.RightSet("lrsw"),             // canonical sort: l,r,s,w
	}
	if !reflect.DeepEqual(data.Rights, want) {
		t.Errorf("ACL = %+v, want %+v", data.Rights, want)
	}
}

func TestACL_SetAddModifiesExisting(t *testing.T) {
	c := startACLServer(t, true)
	bob, _ := imaplib.NewRightsIdentifierUsername("bob@test.com")
	if err := c.SetACL("INBOX", bob, imaplib.RightModificationReplace, imaplib.RightSet("lr")).Wait(); err != nil {
		t.Fatalf("SETACL replace: %v", err)
	}
	if err := c.SetACL("INBOX", bob, imaplib.RightModificationAdd, imaplib.RightSet("ws")).Wait(); err != nil {
		t.Fatalf("SETACL add: %v", err)
	}
	data, err := c.GetACL("INBOX").Wait()
	if err != nil {
		t.Fatalf("GETACL: %v", err)
	}
	// owner implicit + bob explicit
	if len(data.Rights) != 2 || string(data.Rights[bob]) != "lrsw" {
		t.Errorf("expected owner+bob entries, bob rights lrsw, got %+v", data.Rights)
	}
}

func TestACL_SetRemoveSubtracts(t *testing.T) {
	c := startACLServer(t, true)
	bob, _ := imaplib.NewRightsIdentifierUsername("bob@test.com")
	if err := c.SetACL("INBOX", bob, imaplib.RightModificationReplace, imaplib.RightSet("lrswa")).Wait(); err != nil {
		t.Fatalf("SETACL replace: %v", err)
	}
	if err := c.SetACL("INBOX", bob, imaplib.RightModificationRemove, imaplib.RightSet("a")).Wait(); err != nil {
		t.Fatalf("SETACL remove: %v", err)
	}
	data, _ := c.GetACL("INBOX").Wait()
	// owner implicit + bob explicit
	if len(data.Rights) != 2 || string(data.Rights[bob]) != "lrsw" {
		t.Errorf("expected lrsw after remove a, got %+v", data.Rights)
	}
}

func TestACL_SetReplaceEmptyDropsEntry(t *testing.T) {
	c := startACLServer(t, true)
	bob, _ := imaplib.NewRightsIdentifierUsername("bob@test.com")
	if err := c.SetACL("INBOX", bob, imaplib.RightModificationReplace, imaplib.RightSet("lr")).Wait(); err != nil {
		t.Fatalf("SETACL: %v", err)
	}
	if err := c.SetACL("INBOX", bob, imaplib.RightModificationReplace, imaplib.RightSet("")).Wait(); err != nil {
		t.Fatalf("SETACL empty: %v", err)
	}
	data, _ := c.GetACL("INBOX").Wait()
	// bob entry is gone; implicit owner entry remains
	alice, _ := imaplib.NewRightsIdentifierUsername("alice@test.com")
	bob2, _ := imaplib.NewRightsIdentifierUsername("bob@test.com")
	if _, hasBob := data.Rights[bob2]; hasBob {
		t.Errorf("expected bob entry removed after replace-empty, got %+v", data.Rights)
	}
	if got := string(data.Rights[alice]); got != string(mailbox.FullRights) {
		t.Errorf("owner should still have implicit FullRights, got %q", got)
	}
}

func TestACL_DeleteRemovesIdentifier(t *testing.T) {
	c := startACLServer(t, true)
	bob, _ := imaplib.NewRightsIdentifierUsername("bob@test.com")
	carol, _ := imaplib.NewRightsIdentifierUsername("carol@test.com")
	if err := c.SetACL("INBOX", bob, imaplib.RightModificationReplace, imaplib.RightSet("lr")).Wait(); err != nil {
		t.Fatalf("SETACL bob: %v", err)
	}
	if err := c.SetACL("INBOX", carol, imaplib.RightModificationReplace, imaplib.RightSet("lr")).Wait(); err != nil {
		t.Fatalf("SETACL carol: %v", err)
	}
	if err := c.DeleteACL("INBOX", bob).Wait(); err != nil {
		t.Fatalf("DELETEACL: %v", err)
	}
	data, _ := c.GetACL("INBOX").Wait()
	if _, hasBob := data.Rights[bob]; hasBob {
		t.Errorf("after DELETEACL bob, bob should be gone, got %+v", data.Rights)
	}
	if _, hasCarol := data.Rights[carol]; !hasCarol {
		t.Errorf("after DELETEACL bob, carol should remain, got %+v", data.Rights)
	}
}

func TestACL_MyRightsOwnerGetsFullRights(t *testing.T) {
	c := startACLServer(t, true)
	// Owner (alice) always gets FullRights from Effective() regardless of
	// explicit entries — RFC 4314 §4 implicit owner grant.
	data, err := c.MyRights("INBOX").Wait()
	if err != nil {
		t.Fatalf("MYRIGHTS: %v", err)
	}
	if got := sortedString(string(data.Rights)); got != sortedString(string(mailbox.FullRights)) {
		t.Errorf("owner rights = %q, want %q", got, sortedString(string(mailbox.FullRights)))
	}
}

func TestACL_ListRights(t *testing.T) {
	c := startACLServer(t, true)

	// RFC 4314 §3.7: no right is implied for any identifier — required set is
	// always empty, every right (incl. the obsolete c/d compounds) is
	// individually grantable. Matches the reference implementation's uniform
	// LISTRIGHTS output for every identifier.
	bob, _ := imaplib.NewRightsIdentifierUsername("bob@test.com")
	data, err := c.ListRights("INBOX", bob).Wait()
	if err != nil {
		t.Fatalf("LISTRIGHTS bob: %v", err)
	}
	if len(data.RequiredRights) != 0 {
		t.Errorf("bob required rights = %v, want empty", data.RequiredRights)
	}
	if len(data.OptionalRights) != 13 {
		t.Errorf("bob optional rights = %v, want 13 elements (l r w s t p i e k x a c d)", data.OptionalRights)
	}

	// The owner keyword gets the same uniform list — no required rights.
	owner := imaplib.RightsIdentifier("owner")
	od, err := c.ListRights("INBOX", owner).Wait()
	if err != nil {
		t.Fatalf("LISTRIGHTS owner: %v", err)
	}
	if len(od.RequiredRights) != 0 {
		t.Errorf("owner required rights = %q, want empty", od.RequiredRights)
	}
	if len(od.OptionalRights) != 13 {
		t.Errorf("owner optional rights = %v, want 13 elements (l r w s t p i e k x a c d)", od.OptionalRights)
	}
}

func TestACL_PersistsAcrossSetGet(t *testing.T) {
	c := startACLServer(t, true)
	bob, _ := imaplib.NewRightsIdentifierUsername("bob@test.com")
	if err := c.SetACL("INBOX", bob, imaplib.RightModificationReplace, imaplib.RightSet("lrws")).Wait(); err != nil {
		t.Fatalf("SETACL: %v", err)
	}
	data, err := c.GetACL("INBOX").Wait()
	if err != nil {
		t.Fatalf("GETACL: %v", err)
	}
	// owner implicit + bob explicit = 2 entries
	if len(data.Rights) != 2 {
		t.Fatalf("expected 2 entries (owner+bob), got %+v", data.Rights)
	}
}

func TestACL_DisabledReturnsNO(t *testing.T) {
	c := startACLServer(t, false)
	if _, err := c.GetACL("INBOX").Wait(); err == nil {
		t.Error("GETACL on disabled ACL should NO")
	} else if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("expected 'disabled' in error, got %v", err)
	}
	if _, err := c.MyRights("INBOX").Wait(); err == nil {
		t.Error("MYRIGHTS on disabled ACL should NO")
	}
}

func sortedString(s string) string {
	b := []byte(s)
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	return string(b)
}

// TestACL_SetACLNegativeRoundTrip verifies RFC 4314 §3.1 negative-rights
// entries: a "-<identifier>" SETACL stores a negative entry that coexists with
// the identifier's positive entry and round-trips through GETACL.
func TestACL_SetACLNegativeRoundTrip(t *testing.T) {
	c := startACLServer(t, true)
	posBob, _ := imaplib.NewRightsIdentifierUsername("bob@test.com")
	negBob := imaplib.RightsIdentifier("-bob@test.com")

	if err := c.SetACL("INBOX", posBob, imaplib.RightModificationReplace, imaplib.RightSet("lrs")).Wait(); err != nil {
		t.Fatalf("SETACL positive: %v", err)
	}
	if err := c.SetACL("INBOX", negBob, imaplib.RightModificationReplace, imaplib.RightSet("s")).Wait(); err != nil {
		t.Fatalf("SETACL negative: %v", err)
	}
	data, err := c.GetACL("INBOX").Wait()
	if err != nil {
		t.Fatalf("GETACL: %v", err)
	}
	if got := string(data.Rights[posBob]); got != "lrs" {
		t.Errorf("positive bob = %q, want lrs", got)
	}
	if got := string(data.Rights[negBob]); got != "s" {
		t.Errorf("negative -bob = %q, want s; full=%+v", got, data.Rights)
	}

	// DELETEACL of the negative drops only the negative entry.
	if err := c.DeleteACL("INBOX", negBob).Wait(); err != nil {
		t.Fatalf("DELETEACL negative: %v", err)
	}
	data2, err := c.GetACL("INBOX").Wait()
	if err != nil {
		t.Fatalf("GETACL after delete: %v", err)
	}
	if _, ok := data2.Rights[negBob]; ok {
		t.Error("negative entry should be gone after DELETEACL -bob")
	}
	if string(data2.Rights[posBob]) != "lrs" {
		t.Error("positive entry should survive DELETEACL of the negative")
	}
}
