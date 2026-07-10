package lmtp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	fileindex "github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
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
	box, _, rel, closeP := s.deliveryTarget(rcptUI, rcptBox, rcptIdx, "Archive")
	if box != rcptBox || rel != "Archive" {
		t.Errorf("personal: box==rcpt=%v rel=%q, want own store + Archive", box == rcptBox, rel)
	}
	closeP()

	// Public/News → the public storage, prefix stripped to "News".
	box2, idx2, rel2, closePub := s.deliveryTarget(rcptUI, rcptBox, rcptIdx, "Public/News")
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
	if err := deliverOne(box2, idx2, rel2, bytes.NewReader([]byte("From: x@y\r\n\r\nhi\r\n")), 18, nil, "alice@x", "x@y", nil); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}
	if _, err := os.Stat(filepath.Join(publicDir, ".News", "new")); err != nil {
		t.Errorf("message should land under the public namespace root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "alice", "Maildir", ".News")); !os.IsNotExist(err) {
		t.Errorf("Public/News must NOT be created in the recipient's own store: err=%v", err)
	}

	// Bare namespace prefix → its INBOX.
	_, _, relInbox, closeI := s.deliveryTarget(rcptUI, rcptBox, rcptIdx, "Public/")
	if relInbox != "INBOX" {
		t.Errorf("bare prefix rel = %q, want INBOX", relInbox)
	}
	closeI()
}
