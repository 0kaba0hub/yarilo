package imap_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// A subscription is the subscriber's state. In an owner-templated namespace it
// must be kept in the SUBSCRIBER's store under the client-visible name, not in
// the store of whoever owns the mailbox -- which is what let a peer write into a
// stranger's file, and left rows the peer could not remove.
func TestSubscriptions_FollowTheSubscriberNotTheOwner(t *testing.T) {
	root, dial := ownerTemplatedServer(t)
	a := dial("alice")

	if err := a.Subscribe("user/alice/INBOX").Wait(); err != nil {
		t.Fatalf("owner subscribe: %v", err)
	}
	// The row is in alice's own personal subscriptions file, keyed by the
	// client-visible name so it cannot merge with her own INBOX.
	personal, err := os.ReadFile(filepath.Join(root, "alice", "subscriptions"))
	if err != nil {
		t.Fatalf("personal subscriptions file: %v", err)
	}
	if !strings.Contains(string(personal), "user/alice/INBOX") {
		t.Errorf("personal file does not hold the visible name: %q", personal)
	}
	// And the namespace keeps no file of its own any more.
	if _, err := os.Stat(filepath.Join(root, "alice", "Maildir", "subscriptions-user")); !os.IsNotExist(err) {
		t.Errorf("owner-templated namespace still keeps its own subscriptions file: %v", err)
	}
}

// The prefixed key is what keeps user/alice/INBOX distinct from the caller's own
// INBOX: unsubscribing one must not remove the other.
func TestSubscriptions_PrefixedKeyDoesNotMergeWithOwnInbox(t *testing.T) {
	root, dial := ownerTemplatedServer(t)
	a := dial("alice")

	if err := a.Subscribe("INBOX").Wait(); err != nil {
		t.Fatalf("subscribe own INBOX: %v", err)
	}
	if err := a.Subscribe("user/alice/INBOX").Wait(); err != nil {
		t.Fatalf("subscribe templated INBOX: %v", err)
	}
	if err := a.Unsubscribe("user/alice/INBOX").Wait(); err != nil {
		t.Fatalf("unsubscribe templated INBOX: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, "alice", "subscriptions"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(body))
	var ownINBOX, templated bool
	for _, l := range lines {
		switch l {
		case "INBOX":
			ownINBOX = true
		case "user/alice/INBOX":
			templated = true
		}
	}
	if !ownINBOX {
		t.Error("unsubscribing the templated name removed the caller's own INBOX subscription")
	}
	if templated {
		t.Error("the templated subscription survived unsubscribe")
	}
}

// A peer with no rights cannot subscribe (the #1138 gate), but a row they
// already hold is theirs to remove: UNSUBSCRIBE must not be gated, or the row
// would be one nothing can remove.
func TestSubscriptions_PeerCanRemoveTheirOwnRow(t *testing.T) {
	_, dial := ownerTemplatedServer(t)
	b := dial("bob")

	if err := b.Subscribe("user/alice/INBOX").Wait(); err == nil {
		t.Fatal("peer with no rights subscribed to a stranger's mailbox")
	}
	// Unsubscribe answers on the caller's own list, whatever the gate says
	// about the owner's space.
	if err := b.Unsubscribe("user/alice/INBOX").Wait(); err != nil {
		t.Errorf("peer cannot remove a row from their own list: %v", err)
	}
}

// The key rule is "visible name minus the storing namespace's prefix", not
// "the full visible name". Where the personal namespace has a prefix of its own,
// a name outside it has no storing namespace at all, and that must refuse --
// with an empty personal prefix both readings agree, so only this configuration
// tells them apart.
func TestSubscriptions_NoStoringNamespaceRefuses(t *testing.T) {
	root := t.TempDir()
	srv := imapserver.New(imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		Auth:     &enforcePassdb{users: map[string]string{"alice": "pw"}},
		Namespaces: []imapserver.NamespaceSpec{
			// The personal namespace does not cover the whole name space.
			{Type: imapserver.NamespacePersonal, Prefix: "Mail/", Separator: '/', List: true},
			{Type: imapserver.NamespaceShared, Prefix: "Team/", Separator: '/', List: true,
				Location: "maildir:" + filepath.Join(root, "team"), Subscriptions: boolPtr(false)},
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
	// Team/ delegates, and no namespace that keeps subscriptions covers
	// "Team/News" -- the personal one is prefixed Mail/.
	if err := c.Subscribe("Team/News").Wait(); err == nil {
		t.Error("subscribed with no namespace to keep it; want a refusal, not a key nothing would match")
	}
}

func boolPtr(b bool) *bool { return &b }
