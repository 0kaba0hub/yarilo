package imap_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// heteroOwnerServer gives each owner a different driver in the userdb -- alice
// mdbox, bob maildir -- while the namespace template names maildir. This is the
// case #1144 needs: a homogeneous stand hides the bug, because the template
// driver happens to match every owner (the u51 case in the sandbox).
func heteroOwnerServer(t *testing.T) (root string, dial func(user string) *imapclient.Client) {
	t.Helper()
	root = t.TempDir()
	lookup := func(_ context.Context, owner string) (*mailboxpkg.UserInfo, error) {
		switch owner {
		case "alice":
			home := filepath.Join(root, "alice")
			return &mailboxpkg.UserInfo{Username: "alice", Home: home, MailPath: filepath.Join(home, "mdbox"), Driver: "mdbox"}, nil
		case "bob":
			home := filepath.Join(root, "bob")
			return &mailboxpkg.UserInfo{Username: "bob", Home: home, MailPath: filepath.Join(home, "Maildir"), Driver: "maildir"}, nil
		}
		return nil, &notFoundError{owner}
	}
	byDriver := func(d string) mailboxpkg.MailboxBackend {
		switch d {
		case "mdbox":
			return mdbox.New()
		case "sdbox":
			return dboxv2.New()
		case "maildir":
			return maildir.New()
		}
		return nil
	}
	srv := imapserver.New(imapserver.Options{
		Mailbox:         maildir.New(), // the namespace template / global driver
		MailboxByDriver: byDriver,
		Index:           file.New(),
		Resolver:        &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		Auth:            &enforcePassdb{users: map[string]string{"alice": "pw", "bob": "pw"}},
		ACLEnabled:      true,
		UserdbLookup:    lookup,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespaceShared, Prefix: "user/%u/", Separator: '/', List: imapserver.ListChildren, Location: "maildir:%h/Maildir"},
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
			t.Fatalf("greeting: %v", err)
		}
		if err := c.Login(user, "pw").Wait(); err != nil {
			t.Fatalf("login %q: %v", user, err)
		}
		t.Cleanup(func() { c.Logout().Wait() }) //nolint:errcheck
		return c
	}
	return root, dial
}

// An owner-templated namespace must open the OWNER's driver, from the userdb,
// not the namespace template's. alice's userdb driver is mdbox; the template is
// maildir. Before #1144 the backend was selected by prefix, so an owner-templated
// namespace fell to the global maildir backend and opened a maildir tree on
// alice's mdbox root -- a phantom store beside her real mail. SELECT alone does
// not catch it (the wrong backend is self-consistent), so the on-disk layout is
// the witness: mdbox writes storage/, the maildir phantom writes cur/.
func TestOwnerTemplated_OpensOwnersDriverNotTheTemplate(t *testing.T) {
	root, dial := heteroOwnerServer(t)
	a := dial("alice")

	body := []byte("From: x@y\r\nSubject: t\r\n\r\nhi\r\n")
	ac := a.Append("user/alice/INBOX", int64(len(body)), nil)
	_, _ = ac.Write(body)
	_ = ac.Close()
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("owner APPEND into her templated INBOX: %v", err)
	}
	// It is readable back through the same namespace.
	sel, err := a.Select("user/alice/INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("owner SELECT: %v", err)
	}
	if sel.NumMessages != 1 {
		t.Errorf("SELECT shows %d messages, want 1", sel.NumMessages)
	}

	// The store on disk is mdbox (alice's driver), not a maildir phantom at the
	// same root -- the witness the backend selection is right (#1144).
	mdboxRoot := filepath.Join(root, "alice", "mdbox")
	if !dirExists(filepath.Join(mdboxRoot, "storage")) {
		t.Errorf("owner store is not mdbox: no storage/ under %s", mdboxRoot)
	}
	if dirExists(filepath.Join(mdboxRoot, "cur")) {
		t.Errorf("a maildir phantom (cur/) was written at alice's mdbox root: the template driver opened her store (#1144)")
	}
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// MailboxByDriver builds a backend (mdbox.New(...) etc.), each with its own
// write semaphore. It must run once per driver per process, not once per handle:
// otherwise a session touching many owners holds many independent write budgets
// and max_concurrent_writes stops limiting the shared volume (#1144). Two owners
// on the same driver -- across two sessions -- must share one built backend.
func TestOwnerTemplated_BackendBuiltOncePerDriver(t *testing.T) {
	root := t.TempDir()
	var mu sync.Mutex
	builds := map[string]int{}
	lookup := func(_ context.Context, owner string) (*mailboxpkg.UserInfo, error) {
		if owner == "alice" || owner == "carol" {
			home := filepath.Join(root, owner)
			return &mailboxpkg.UserInfo{Username: owner, Home: home, MailPath: filepath.Join(home, "mdbox"), Driver: "mdbox"}, nil
		}
		return nil, &notFoundError{owner}
	}
	byDriver := func(d string) mailboxpkg.MailboxBackend {
		mu.Lock()
		builds[d]++
		mu.Unlock()
		if d == "mdbox" {
			return mdbox.New()
		}
		return nil
	}
	srv := imapserver.New(imapserver.Options{
		Mailbox:         maildir.New(),
		MailboxByDriver: byDriver,
		Index:           file.New(),
		Resolver:        &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		Auth:            &enforcePassdb{users: map[string]string{"alice": "pw", "carol": "pw"}},
		ACLEnabled:      true,
		UserdbLookup:    lookup,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespaceShared, Prefix: "user/%u/", Separator: '/', List: imapserver.ListChildren, Location: "maildir:%h/Maildir"},
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
			t.Fatalf("login %q: %v", user, err)
		}
		return c
	}

	// Two owners, same driver, two sessions -- each opens its own owner handle.
	for _, u := range []string{"alice", "carol"} {
		c := dial(u)
		if _, err := c.Select("user/"+u+"/INBOX", nil).Wait(); err != nil {
			t.Fatalf("%s SELECT own templated INBOX: %v", u, err)
		}
		c.Logout().Wait() //nolint:errcheck
	}

	mu.Lock()
	n := builds["mdbox"]
	mu.Unlock()
	if n != 1 {
		t.Errorf("mdbox backend built %d times across two owner handles, want 1 (the write semaphore must be shared)", n)
	}
}
