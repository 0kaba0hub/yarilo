package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// stageStore writes one user with n messages and then removes the guid
// extension from the index, which is the shape an older build left behind.
func stageStore(t *testing.T, n int) (root, user string) {
	t.Helper()
	root = t.TempDir()
	user = "u1@example.test"
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	info := resolver.UserInfo(user, "")
	box := maildir.New().OpenUser(info)
	idx := indexfile.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	for i := 0; i < n; i++ {
		uid, err := idx.AllocateUID(folder.ID)
		if err != nil {
			t.Fatalf("allocate: %v", err)
		}
		body := fmt.Sprintf("Subject: m%d\r\n\r\nbody\r\n", i)
		name, vsize, _, err := box.Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{
			UID: uid, Filename: name, Size: uint32(len(body)), VSize: vsize,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := box.Close(); err != nil {
		t.Fatalf("close box: %v", err)
	}
	dropGUIDExt(t, root)
	return root, user
}

func dropGUIDExt(t *testing.T, root string) {
	t.Helper()
	stripped := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != indexfile.IndexFileName {
			return nil
		}
		f, err := mailindex.Open(path)
		if err != nil {
			return err
		}
		var exts []mailindex.Extension
		for _, e := range f.Extensions {
			if e.Name != "guid" {
				exts = append(exts, e)
			}
		}
		if len(exts) == len(f.Extensions) {
			return nil
		}
		layout, err := mailindex.ComputeRecordLayout(exts)
		if err != nil {
			return err
		}
		extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
		if err != nil {
			return err
		}
		for _, rec := range f.Records {
			delete(rec.Ext, "guid")
		}
		f.Extensions = layout.Extensions
		f.Layout = layout
		f.Header.RecordSize = layout.RecordSize
		f.Header.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
		if _, err := mailindex.Recreate(f.ToRecreateInput(path)); err != nil {
			return err
		}
		stripped++
		return nil
	})
	if err != nil {
		t.Fatalf("drop guid ext: %v", err)
	}
	if stripped == 0 {
		t.Fatal("no index file found")
	}
}

func guidsOf(t *testing.T, root, user string) map[uint32][16]byte {
	t.Helper()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	idx := indexfile.New().OpenUser(resolver.UserInfo(user, ""))
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	out := make(map[uint32][16]byte, len(msgs))
	for _, m := range msgs {
		out[m.UID] = m.GUID
	}
	return out
}

func TestGUIDBackfillStampsStore(t *testing.T) {
	var zero [16]byte
	root, user := stageStore(t, 3)

	for uid, g := range guidsOf(t, root, user) {
		if g != zero {
			t.Fatalf("uid=%d already has GUID %x before the run", uid, g)
		}
	}
	if err := runGUIDBackfill(guidOpts{Driver: "maildir", Root: root}); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	got := guidsOf(t, root, user)
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	seen := map[[16]byte]bool{}
	for uid, g := range got {
		if g == zero {
			t.Errorf("uid=%d still zero after the run", uid)
		}
		if seen[g] {
			t.Errorf("uid=%d duplicates GUID %x", uid, g)
		}
		seen[g] = true
	}
}

func TestGUIDBackfillDryRunWritesNothing(t *testing.T) {
	var zero [16]byte
	root, user := stageStore(t, 2)
	if err := runGUIDBackfill(guidOpts{Driver: "maildir", Root: root, DryRun: true}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	for uid, g := range guidsOf(t, root, user) {
		if g != zero {
			t.Errorf("dry run stamped uid=%d with %x", uid, g)
		}
	}
}

func TestGUIDBackfillIsIdempotent(t *testing.T) {
	root, user := stageStore(t, 3)
	if err := runGUIDBackfill(guidOpts{Driver: "maildir", Root: root}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := guidsOf(t, root, user)
	if err := runGUIDBackfill(guidOpts{Driver: "maildir", Root: root}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	for uid, g := range guidsOf(t, root, user) {
		if first[uid] != g {
			t.Errorf("uid=%d changed on a repeat run: %x -> %x", first[uid], uid, g)
		}
	}
}

func TestGUIDBackfillRejectsUnknownDriver(t *testing.T) {
	if err := runGUIDBackfill(guidOpts{Driver: "bogus", Root: t.TempDir(), DryRun: true}); err == nil {
		t.Fatal("expected an error for an unknown driver")
	}
}

func TestGUIDUsersEnumeratesDomainUserPairs(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a.test/u1", "a.test/u2", "b.test/u3"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	users, err := guidUsers(root, "%d/%n", "")
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("got %v, want 3 users", users)
	}
	want := map[string]bool{"u1@a.test": true, "u2@a.test": true, "u3@b.test": true}
	for _, u := range users {
		if !want[u] {
			t.Errorf("unexpected user %q", u)
		}
	}
	single, err := guidUsers(root, "%d/%n", "x@y.test")
	if err != nil || len(single) != 1 || single[0] != "x@y.test" {
		t.Errorf("--user override = %v, %v", single, err)
	}
}

// stageStoreLayout writes one user under an arbitrary home template, the way a
// deployment with a non-default mail_home_template stores it.
func stageStoreLayout(t *testing.T, template, user string, n int) string {
	t.Helper()
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: template}
	info := resolver.UserInfo(user, "")
	box := maildir.New().OpenUser(info)
	idx := indexfile.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	for i := 0; i < n; i++ {
		uid, err := idx.AllocateUID(folder.ID)
		if err != nil {
			t.Fatalf("allocate: %v", err)
		}
		body := fmt.Sprintf("Subject: m%d\r\n\r\nbody\r\n", i)
		name, vsize, _, err := box.Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{
			UID: uid, Filename: name, Size: uint32(len(body)), VSize: vsize,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := box.Close(); err != nil {
		t.Fatalf("close box: %v", err)
	}
	dropGUIDExt(t, root)
	return root
}

func guidsOfLayout(t *testing.T, root, template, user string) map[uint32][16]byte {
	t.Helper()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: template}
	idx := indexfile.New().OpenUser(resolver.UserInfo(user, ""))
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	out := make(map[uint32][16]byte, len(msgs))
	for _, m := range msgs {
		out[m.UID] = m.GUID
	}
	return out
}

// A deployment on any layout other than the default must still be reachable,
// both walking every user and pinned to one with --user.
func TestGUIDBackfillHonoursHomeTemplate(t *testing.T) {
	var zero [16]byte
	const user = "u51@d00001.test"
	for _, tt := range []struct {
		name     string
		template string
		only     string
	}{
		{name: "domain and full address", template: "%d/%u"},
		{name: "domain and local part", template: "%d/%n"},
		{name: "flat by address", template: "%u"},
		{name: "single user pinned", template: "%d/%u", only: user},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := stageStoreLayout(t, tt.template, user, 2)
			err := runGUIDBackfill(guidOpts{
				Driver: "maildir", Root: root, Template: tt.template, User: tt.only,
			})
			if err != nil {
				t.Fatalf("backfill: %v", err)
			}
			got := guidsOfLayout(t, root, tt.template, user)
			if len(got) != 2 {
				t.Fatalf("got %d messages, want 2", len(got))
			}
			for uid, g := range got {
				if g == zero {
					t.Errorf("uid=%d still zero on layout %q", uid, tt.template)
				}
			}
		})
	}
}

// The layout comes from the config when no flag overrides it, so an operator
// only has to point the tool at yarilo.yaml.
func TestGUIDBackfillResolverFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.MaildirRoot = "/srv/mail"
	cfg.Storage.MailHomeTemplate = "%d/%u"
	cfg.Storage.IndexDir = "/srv/index/%d/%u"

	r := guidResolver(cfg, guidOpts{})
	if r.Root != "/srv/mail" || r.HomeTemplate != "%d/%u" {
		t.Errorf("config ignored: root=%q template=%q", r.Root, r.HomeTemplate)
	}
	if r.DefaultIndexDir != "/srv/index/%d/%u" {
		t.Errorf("index dir not carried: %q", r.DefaultIndexDir)
	}

	r = guidResolver(cfg, guidOpts{Root: "/tmp/other", Template: "%n"})
	if r.Root != "/tmp/other" || r.HomeTemplate != "%n" {
		t.Errorf("flags did not override: root=%q template=%q", r.Root, r.HomeTemplate)
	}

	r = guidResolver(&config.Config{}, guidOpts{})
	if r.Root != "/var/mail/vhosts" || r.HomeTemplate != "%d/%n" {
		t.Errorf("defaults changed: root=%q template=%q", r.Root, r.HomeTemplate)
	}
}

// A layout whose leaf does not name the user cannot be enumerated, and saying
// so beats walking the wrong directories.
func TestGUIDUsersRejectsUnwalkableLayout(t *testing.T) {
	if _, err := guidUsers(t.TempDir(), "%d/%n/Maildir", ""); err == nil {
		t.Error("expected an error for a layout whose leaf is not the user")
	}
	if users, err := guidUsers(t.TempDir(), "%d/%n/Maildir", "u@d.test"); err != nil || len(users) != 1 {
		t.Errorf("--user should bypass enumeration: %v %v", users, err)
	}
}

func TestGUIDUsersEnumeratesFullAddressLayout(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a.test/u1@a.test", "b.test/u2@b.test"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	users, err := guidUsers(root, "%d/%u", "")
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	want := map[string]bool{"u1@a.test": true, "u2@b.test": true}
	if len(users) != 2 {
		t.Fatalf("got %v, want 2 users", users)
	}
	for _, u := range users {
		if !want[u] {
			t.Errorf("unexpected user %q", u)
		}
	}
}
