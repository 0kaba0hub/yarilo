package lmtp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/userstate/acl"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// TestDeliveryTargetRoutesNamespaces verifies delivery-through-namespaces:
// a namespace-prefixed folder routes to that namespace's storage (prefix
// stripped), while a personal folder stays in the recipient's own store.
func TestDeliveryTargetRoutesNamespaces(t *testing.T) {
	root := t.TempDir()
	publicDir := filepath.Join(root, "public")
	mb := maildir.New()
	idx := fileindex.New()

	s := &session{opts: Options{
		Mailbox: mb,
		Index:   idx,
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/"},
			{Type: "public", Prefix: "Public/", Separator: "/", Location: "maildir:" + publicDir},
		},
	}}

	rcptUI := &mailbox.UserInfo{
		Username: "alice@x", Home: filepath.Join(root, "alice"),
		MailPath: filepath.Join(root, "alice", "Maildir"),
	}
	rcptBox := mb.OpenUser(rcptUI)
	if err := rcptBox.Init(); err != nil {
		t.Fatalf("rcpt init: %v", err)
	}
	rcptIdx := idx.OpenUser(rcptUI)

	// Personal folder → the recipient's own store, name unchanged.
	box, _, rel, closeP := s.deliveryTarget(rcptUI, rcptBox, rcptIdx, "Archive", false)
	if box != rcptBox || rel != "Archive" {
		t.Errorf("personal: box==rcpt=%v rel=%q, want own store + Archive", box == rcptBox, rel)
	}
	closeP()

	// Public/News → the public storage, prefix stripped to "News".
	box2, idx2, rel2, closePub := s.deliveryTarget(rcptUI, rcptBox, rcptIdx, "Public/News", false)
	defer closePub()
	if box2 == rcptBox {
		t.Fatal("Public/ should route to a separate store, not the recipient's")
	}
	if rel2 != "News" {
		t.Errorf("rel = %q, want News", rel2)
	}
	// Deliver a message and confirm it lands under the public root, not alice's.
	if err := box2.Create(rel2); err != nil {
		t.Fatalf("create News: %v", err)
	}
	if _, err := deliverOne(box2, idx2, rel2, bytes.NewReader([]byte("From: x@y\r\n\r\nhi\r\n")), 18, nil, "alice@x", "x@y", nil); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}
	if _, err := os.Stat(filepath.Join(publicDir, ".News", "new")); err != nil {
		t.Errorf("message should land under the public namespace root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "alice", "Maildir", ".News")); !os.IsNotExist(err) {
		t.Errorf("Public/News must NOT be created in the recipient's own store: err=%v", err)
	}

	// Bare namespace prefix → its INBOX.
	_, _, relInbox, closeI := s.deliveryTarget(rcptUI, rcptBox, rcptIdx, "Public/", false)
	if relInbox != "INBOX" {
		t.Errorf("bare prefix rel = %q, want INBOX", relInbox)
	}
	closeI()
}

// TestDeliveryTargetPostRight verifies #499 item 2: delivery into a public
// namespace requires the recipient's 'p' (post) right on the target folder.
// With the grant the message routes to the public store; without it, delivery
// falls back to the recipient's INBOX (implicit keep) rather than being lost.
func TestDeliveryTargetPostRight(t *testing.T) {
	seedPostACL := func(t *testing.T, publicDir, folder, body string) {
		t.Helper()
		st := acl.New(publicDir, publicDir, "maildir", "/", "", "", "", acl.Policy{}, nil)
		p := st.Path(folder)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir acl dir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write yarilo-acl: %v", err)
		}
	}

	newSession := func(publicDir string, mb *maildir.Backend, idx *fileindex.Backend) *session {
		return &session{opts: Options{
			Mailbox:    mb,
			Index:      idx,
			ACLEnabled: true,
			Namespaces: []config.NamespaceConfig{
				{Type: "personal", Prefix: "", Separator: "/"},
				{Type: "public", Prefix: "Public/", Separator: "/", Location: "maildir:" + publicDir},
			},
		}}
	}

	newRcpt := func(t *testing.T, root string, mb *maildir.Backend, idx *fileindex.Backend) (*mailbox.UserInfo, mailbox.UserMailbox, mailbox.UserIndex) {
		ui := &mailbox.UserInfo{
			Username: "alice@x", Home: filepath.Join(root, "alice"),
			MailPath: filepath.Join(root, "alice", "Maildir"),
		}
		box := mb.OpenUser(ui)
		if err := box.Init(); err != nil {
			t.Fatalf("rcpt init: %v", err)
		}
		return ui, box, idx.OpenUser(ui)
	}

	t.Run("granted routes to public", func(t *testing.T) {
		root := t.TempDir()
		publicDir := filepath.Join(root, "public")
		mb, idx := maildir.New(), fileindex.New()
		seedPostACL(t, publicDir, "News", "user=alice@x lrp\n")
		s := newSession(publicDir, mb, idx)
		ui, rcptBox, rcptIdx := newRcpt(t, root, mb, idx)

		box, _, rel, done := s.deliveryTarget(ui, rcptBox, rcptIdx, "Public/News", true)
		defer done()
		if box == rcptBox {
			t.Fatal("with post right, delivery must route to the public store")
		}
		if rel != "News" {
			t.Errorf("rel = %q, want News", rel)
		}
	})

	t.Run("denied falls back to INBOX", func(t *testing.T) {
		root := t.TempDir()
		publicDir := filepath.Join(root, "public")
		mb, idx := maildir.New(), fileindex.New()
		seedPostACL(t, publicDir, "News", "user=alice@x lr\n") // lookup+read, no post
		s := newSession(publicDir, mb, idx)
		ui, rcptBox, rcptIdx := newRcpt(t, root, mb, idx)

		box, _, rel, done := s.deliveryTarget(ui, rcptBox, rcptIdx, "Public/News", true)
		defer done()
		if box != rcptBox {
			t.Fatal("without post right, delivery must fall back to the recipient's store")
		}
		if rel != "INBOX" {
			t.Errorf("fallback rel = %q, want INBOX", rel)
		}
	})

	t.Run("no ACL falls back to INBOX", func(t *testing.T) {
		root := t.TempDir()
		publicDir := filepath.Join(root, "public")
		mb, idx := maildir.New(), fileindex.New()
		s := newSession(publicDir, mb, idx) // no yarilo-acl seeded → zero rights
		ui, rcptBox, rcptIdx := newRcpt(t, root, mb, idx)

		box, _, rel, done := s.deliveryTarget(ui, rcptBox, rcptIdx, "Public/News", true)
		defer done()
		if box != rcptBox || rel != "INBOX" {
			t.Errorf("no ACL: box==rcpt=%v rel=%q, want fallback to INBOX", box == rcptBox, rel)
		}
	})
}
