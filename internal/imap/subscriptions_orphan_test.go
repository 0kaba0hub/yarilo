package imap_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// startOrphanServer is ownerTemplatedServer for raw-connection tests: LSUB and
// extended LIST need the wire, not imapclient.
func startOrphanServer(t *testing.T) (root, addr string) {
	t.Helper()
	root = t.TempDir()
	lookup := func(_ context.Context, owner string) (*mailboxpkg.UserInfo, error) {
		if owner != "alice" && owner != "bob" {
			return nil, &notFoundError{owner}
		}
		home := filepath.Join(root, owner)
		return &mailboxpkg.UserInfo{
			Username: owner, Home: home, MailPath: filepath.Join(home, "Maildir"), Driver: "maildir",
		}, nil
	}
	srv := imapserver.New(imapserver.Options{
		Mailbox:      maildir.New(),
		Index:        file.New(),
		Resolver:     &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		Auth:         &enforcePassdb{users: map[string]string{"alice": "pw", "bob": "pw"}},
		ACLEnabled:   true,
		UserdbLookup: lookup,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: true},
			{Type: imapserver.NamespaceShared, Prefix: "user/%u/", Separator: '/', List: true, Location: "maildir:%h/Maildir"},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck
	return root, ln.Addr().String()
}

func orphanLogin(t *testing.T, addr, user string) *rawConn {
	t.Helper()
	rc := dialRaw(t, addr)
	if !strings.Contains(rc.cmd(`LOGIN "`+user+`" "pw"`), "OK") {
		t.Fatalf("login %q failed", user)
	}
	return rc
}

// An orphaned subscription is shown, and the flags follow the command form:
// \NonExistent under extended LIST, nothing under LSUB, absent from a plain
// LIST (which never selects subscriptions).
func TestOrphanSubscription_FlagsPerCommandForm(t *testing.T) {
	_, addr := startOrphanServer(t)
	a := orphanLogin(t, addr, "alice")
	a.cmd(`SELECT INBOX`)
	if !strings.Contains(a.cmd(`SUBSCRIBE Ghost`), "OK") {
		t.Fatal("subscribe to a not-yet-created mailbox must succeed")
	}

	ext := a.cmd(`LIST (SUBSCRIBED) "" "*"`)
	if !strings.Contains(ext, `\NonExistent) "/" "Ghost"`) {
		t.Errorf("extended LIST should flag the orphan \\NonExistent, got:\n%s", ext)
	}

	ret := a.cmd(`LIST (SUBSCRIBED) "" "*" RETURN (SUBSCRIBED)`)
	if !strings.Contains(ret, `\NonExistent \Subscribed) "/" "Ghost"`) {
		t.Errorf("RETURN (SUBSCRIBED) should add \\Subscribed to the orphan, got:\n%s", ret)
	}

	lsub := a.cmd(`LSUB "" "*"`)
	if !strings.Contains(lsub, `LSUB () "/" "Ghost"`) {
		t.Errorf("LSUB should show the orphan with no flags, got:\n%s", lsub)
	}
	if strings.Contains(lsub, `NonExistent`) {
		t.Errorf("LSUB must not carry \\NonExistent, got:\n%s", lsub)
	}

	plain := a.cmd(`LIST "" "*"`)
	if strings.Contains(plain, "Ghost") {
		t.Errorf("plain LIST selects no subscriptions, Ghost should be absent, got:\n%s", plain)
	}
}

// A live owner-templated subscription is listed without \NonExistent; the
// namespace opens no handle until referenced, so this exercises the
// templated pass, not the regular per-namespace loop.
func TestOrphanSubscription_LiveTemplatedRowListed(t *testing.T) {
	_, addr := startOrphanServer(t)
	a := orphanLogin(t, addr, "alice")
	a.cmd(`SELECT INBOX`)
	if !strings.Contains(a.cmd(`SUBSCRIBE user/alice/INBOX`), "OK") {
		t.Fatal("owner subscribe failed")
	}
	ext := a.cmd(`LIST (SUBSCRIBED) "" "*"`)
	if !strings.Contains(ext, `"user/alice/INBOX"`) {
		t.Errorf("templated subscription missing from LIST (SUBSCRIBED):\n%s", ext)
	}
	for _, line := range strings.Split(ext, "\n") {
		if strings.Contains(line, "user/alice/INBOX") && strings.Contains(line, "NonExistent") {
			t.Errorf("live templated subscription flagged \\NonExistent:\n%s", line)
		}
	}
}

// A row naming a space the caller cannot see and a row naming an invented
// owner must render identically: both are \NonExistent orphans. The rows are
// shown (they are the caller's own state) but the mailbox's existence is
// resolved through the same gate as every verb, so #1138 stays closed.
func TestOrphanSubscription_HiddenAndInventedOwnersRenderTheSame(t *testing.T) {
	root, addr := startOrphanServer(t)
	// Rows planted directly: SUBSCRIBE would refuse both today (#1138), but
	// rows created before a rights revocation are exactly this state.
	if err := os.MkdirAll(filepath.Join(root, "bob"), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := "user/alice/INBOX\nuser/nosuch/INBOX\n"
	if err := os.WriteFile(filepath.Join(root, "bob", "subscriptions"), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}

	b := orphanLogin(t, addr, "bob")
	ext := b.cmd(`LIST (SUBSCRIBED) "" "user/*"`)
	var got []string
	for _, line := range strings.Split(ext, "\n") {
		if strings.HasPrefix(line, "* LIST") && strings.Contains(line, "INBOX") {
			got = append(got, line)
		}
	}
	if len(got) != 2 {
		t.Fatalf("want both rows listed, got:\n%s", ext)
	}
	for _, line := range got {
		if !strings.Contains(line, `\NonExistent`) {
			t.Errorf("row should be \\NonExistent, got: %s", line)
		}
	}
	// Same rendering modulo the name: no attribute may distinguish the real
	// owner from the invented one.
	norm := func(line string) string {
		line = strings.ReplaceAll(line, "user/alice/INBOX", "X")
		return strings.ReplaceAll(line, "user/nosuch/INBOX", "X")
	}
	if norm(got[0]) != norm(got[1]) {
		t.Errorf("rows distinguish a real owner from an invented one:\n%s\n%s", got[0], got[1])
	}
}

// Deleting a mailbox strands its subscription; the orphan then surfaces in the
// subscribed listing instead of vanishing.
func TestOrphanSubscription_DeletedMailboxBecomesOrphan(t *testing.T) {
	_, addr := startOrphanServer(t)
	a := orphanLogin(t, addr, "alice")
	a.cmd(`SELECT INBOX`)
	if !strings.Contains(a.cmd(`CREATE Doomed`), "OK") {
		t.Fatal("create failed")
	}
	if !strings.Contains(a.cmd(`SUBSCRIBE Doomed`), "OK") {
		t.Fatal("subscribe failed")
	}
	before := a.cmd(`LIST (SUBSCRIBED) "" "Doomed"`)
	if strings.Contains(before, "NonExistent") {
		t.Fatalf("live mailbox flagged \\NonExistent:\n%s", before)
	}
	if !strings.Contains(a.cmd(`DELETE Doomed`), "OK") {
		t.Fatal("delete failed")
	}
	after := a.cmd(`LIST (SUBSCRIBED) "" "Doomed"`)
	if !strings.Contains(after, `\NonExistent) "/" "Doomed"`) {
		t.Errorf("deleted mailbox's subscription should list as \\NonExistent, got:\n%s", after)
	}
}
