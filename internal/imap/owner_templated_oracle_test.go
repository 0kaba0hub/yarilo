package imap_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	aclstore "github.com/yarilomail/yarilo/internal/userstate/acl"
	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// answer renders an IMAP error the way a client sees it: code + text. Two
// probes are indistinguishable only when these match exactly.
func answer(err error) string {
	if err == nil {
		return "OK"
	}
	var ie *imaplib.Error
	if errors.As(err, &ie) {
		return string(ie.Code) + "|" + ie.Text
	}
	return err.Error()
}

// The owner segment of an owner-templated name is a username, so an answer that
// distinguishes "this account exists but you hold nothing" from "no such
// account" is a directory of the deployment's accounts, readable by anyone who
// can log in. CREATE split them (NOPERM vs NONEXISTENT) and SUBSCRIBE was worse
// -- it accepted a subscription to a stranger's mailbox (#1138).
func TestOwnerTemplated_WriteVerbsAreNotAnAccountOracle(t *testing.T) {
	_, dial := ownerTemplatedServer(t)
	b := dial("bob") // holds nothing on alice's space

	verbs := []struct {
		name           string
		real, invented func() error
	}{
		{
			"CREATE",
			func() error { return b.Create("user/alice/Probe", nil).Wait() },
			func() error { return b.Create("user/ghost/Probe", nil).Wait() },
		},
		{
			"SUBSCRIBE",
			func() error { return b.Subscribe("user/alice/INBOX").Wait() },
			func() error { return b.Subscribe("user/ghost/INBOX").Wait() },
		},
	}
	for _, v := range verbs {
		t.Run(v.name, func(t *testing.T) {
			gotReal, gotInvented := answer(v.real()), answer(v.invented())
			if gotReal == "OK" {
				t.Errorf("%s on a stranger's space succeeded (%q): a caller with no rights must not write there", v.name, gotReal)
			}
			if gotReal != gotInvented {
				t.Errorf("%s answers differ: real owner %q, invented owner %q -- the pair enumerates accounts (#1138)", v.name, gotReal, gotInvented)
			}
		})
	}
}

// The gate must hide only from callers who hold nothing. A peer with a grant
// already knows the space is there, so the precise refusal is not a disclosure
// -- and hiding from them would break the namespace instead of protecting it.
func TestOwnerTemplated_GrantedPeerIsNotHidden(t *testing.T) {
	root, dial := ownerTemplatedServer(t)
	aliceHome := filepath.Join(root, "alice")

	// Grant bob the lookup right on the root of alice's space.
	st := aclstore.New(aliceHome, filepath.Join(aliceHome, "Maildir"), "maildir", "/", "", "alice", "test", aclstore.Policy{}, nil)
	if err := st.Set("", mailboxpkg.ACL{
		{Identifier: mailboxpkg.Identifier{Type: mailboxpkg.IDUser, Name: "bob"}, Rights: "l"},
	}); err != nil {
		t.Fatalf("seed root ACL: %v", err)
	}

	b := dial("bob")
	err := b.Create("user/alice/Probe", nil).Wait()
	if err == nil {
		t.Fatal("lookup alone must not allow CREATE")
	}
	if aclErrCode(err) == imaplib.ResponseCodeNonExistent {
		t.Errorf("a granted peer was hidden from a space they may see: %v", err)
	}
}

// The owner's own space is unaffected by the gate.
func TestOwnerTemplated_OwnerStillWritesTheirOwnSpace(t *testing.T) {
	_, dial := ownerTemplatedServer(t)
	a := dial("alice")
	if err := a.Create("user/alice/Own", nil).Wait(); err != nil {
		t.Errorf("owner CREATE in her own space: %v", err)
	}
	if err := a.Subscribe("user/alice/Own").Wait(); err != nil {
		t.Errorf("owner SUBSCRIBE in her own space: %v", err)
	}
}

// The per-namespace subscriptions file must be a FILE, for a namespace that
// keeps one: the slug came from the client-visible prefix, so a prefix carrying
// the separator made a directory where a file was intended (#1159). An
// owner-templated namespace no longer keeps its own file at all -- subscriptions
// follow the subscriber -- so the property is pinned on a fixed shared namespace
// whose prefix has a separator in it, opted in with explicit subscriptions: true
// (the default there is to delegate).
func TestNamespaceSubscriptionsFileIsAFileNotAPath(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	subsYes := true
	srv := imapserver.New(imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		Auth:     &enforcePassdb{users: map[string]string{"alice": "pw"}},
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: true},
			{Type: imapserver.NamespaceShared, Prefix: "Team/Sub/", Separator: '/', List: true,
				Location: "maildir:" + shared, Subscriptions: &subsYes},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck

	conn, derr := net.Dial("tcp", ln.Addr().String())
	if derr != nil {
		t.Fatal(derr)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	if err := c.Login("alice", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Subscribe("Team/Sub/News").Wait(); err != nil {
		t.Fatalf("subscribe in a namespace that keeps its own file: %v", err)
	}
	st, serr := os.Stat(filepath.Join(shared, "subscriptions-team-sub"))
	if serr != nil {
		t.Fatalf("per-namespace subscriptions file missing: %v", serr)
	}
	if st.IsDir() {
		t.Error("the subscriptions slug was taken as a path: it created a directory where a file was intended (#1159)")
	}
}
