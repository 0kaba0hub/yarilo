package imap_test

import (
	"net"
	"reflect"
	"sort"
	"strings"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	imapserver "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// aclPassdb is a local single-user passdb so acl_test.go does not
// depend on the unexported stubPassdb sitting beside server_test.go.
type aclPassdb struct{ user, pass string }

func (a *aclPassdb) Authenticate(username, password, _ string) (*protocol.AuthResponse, error) {
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

func TestACL_GetEmptyMailboxReturnsEmptyACL(t *testing.T) {
	c := startACLServer(t, true)
	data, err := c.GetACL("INBOX").Wait()
	if err != nil {
		t.Fatalf("GETACL: %v", err)
	}
	if data.Mailbox != "INBOX" {
		t.Errorf("mailbox = %q, want INBOX", data.Mailbox)
	}
	if len(data.Rights) != 0 {
		t.Errorf("expected empty ACL on fresh mailbox, got %+v", data.Rights)
	}
}

func TestACL_SetAddGetRoundTrip(t *testing.T) {
	c := startACLServer(t, true)
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
		bob: imaplib.RightSet("lrsw"), // canonical sort: l,r,s,w
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
	if len(data.Rights) != 1 || string(data.Rights[bob]) != "lrsw" {
		t.Errorf("expected single bob entry with rights lrsw, got %+v", data.Rights)
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
	if len(data.Rights) != 1 || string(data.Rights[bob]) != "lrsw" {
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
	if len(data.Rights) != 0 {
		t.Errorf("expected empty ACL after replace-empty, got %+v", data.Rights)
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

func TestACL_MyRightsReturnsExplicitGrant(t *testing.T) {
	c := startACLServer(t, true)
	// Owner has no explicit entry → PR C MyRights returns empty.
	// Grant alice an explicit entry and verify MyRights surfaces it.
	alice, _ := imaplib.NewRightsIdentifierUsername("alice@test.com")
	if err := c.SetACL("INBOX", alice, imaplib.RightModificationReplace, imaplib.RightSet("lrswa")).Wait(); err != nil {
		t.Fatalf("SETACL: %v", err)
	}
	data, err := c.MyRights("INBOX").Wait()
	if err != nil {
		t.Fatalf("MYRIGHTS: %v", err)
	}
	if got := sortedString(string(data.Rights)); got != "alrsw" {
		t.Errorf("rights = %q, want alrsw (sorted)", got)
	}
}

func TestACL_ListRightsAllOptional(t *testing.T) {
	c := startACLServer(t, true)
	bob, _ := imaplib.NewRightsIdentifierUsername("bob@test.com")
	data, err := c.ListRights("INBOX", bob).Wait()
	if err != nil {
		t.Fatalf("LISTRIGHTS: %v", err)
	}
	if len(data.RequiredRights) != 0 {
		t.Errorf("required rights = %v, want empty", data.RequiredRights)
	}
	if len(data.OptionalRights) != 1 || string(data.OptionalRights[0]) != string(imaplib.RightSetAll) {
		t.Errorf("optional rights = %v, want [RightSetAll]", data.OptionalRights)
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
	if len(data.Rights) != 1 {
		t.Fatalf("expected persisted entry, got %+v", data.Rights)
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
