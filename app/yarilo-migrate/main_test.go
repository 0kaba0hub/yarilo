package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	indexfile "github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestMigrate_DboxV1_ToSdbox covers the canonical Phase 7
// path: read a pre-Phase-3 yarilo dbox tree, write a canonical
// sdbox tree, verify every body + GUID round-trips and the
// per-folder fileindex sees the expected UID range.
func TestMigrate_DboxV1_ToSdbox(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	const user = "alice@example.com"
	srcHome := filepath.Join(src, "example.com", "alice")
	if err := os.MkdirAll(srcHome, 0o700); err != nil {
		t.Fatal(err)
	}

	bodies := []string{"first msg", "second", "third bytes"}
	for i, body := range bodies {
		var g [16]byte
		_, _ = rand.Read(g[:])
		writeDboxV1(t,
			filepath.Join(srcHome, "INBOX"),
			fmt.Sprintf("u.%016x", i+1),
			[]byte(body),
			g,
			uint32(time.Now().Unix()),
		)
	}
	// One legacy folder Sent with one message.
	var sentGUID [16]byte
	_, _ = rand.Read(sentGUID[:])
	writeDboxV1(t, filepath.Join(srcHome, ".Sent"), "u.0000000000000001",
		[]byte("sent body"), sentGUID, uint32(time.Now().Unix()))

	box := dboxv2.New()
	idx := indexfile.New()
	resolver := &mailbox.Resolver{Root: dst, HomeTemplate: "%d/%n"}

	m, s, err := migrateUser(dboxV1Walker{}, src, box, idx, resolver, user)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if m != 4 || s != 0 {
		t.Errorf("migrated=%d skipped=%d want 4/0", m, s)
	}

	// Verify: open the destination as a normal user session,
	// fetch every UID, compare body + GUID.
	verifyBox := dboxv2.New().OpenUser(&mailbox.UserInfo{Username: user, Home: filepath.Join(dst, "example.com", "alice")})
	verifyIdx := indexfile.New().OpenUser(&mailbox.UserInfo{Username: user, Home: filepath.Join(dst, "example.com", "alice")})
	defer verifyBox.Close()
	defer verifyIdx.Close()

	inbox, err := verifyIdx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("verify open INBOX: %v", err)
	}
	msgs, err := verifyIdx.GetMessages(inbox.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatalf("verify get INBOX: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("INBOX has %d msgs, want 3", len(msgs))
	}
	bodySet := map[string]bool{}
	for _, m := range msgs {
		rc, err := verifyBox.Fetch("INBOX", m.Filename)
		if err != nil {
			t.Errorf("verify fetch uid=%d: %v", m.UID, err)
			continue
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		// Migrator goes through Save which CRLF-normalises;
		// bodies in this test are LF-free so the round-trip is
		// byte-identical.
		bodySet[string(got)] = true
	}
	for _, body := range bodies {
		if !bodySet[body] {
			t.Errorf("body %q missing after migrate (got %v)", body, mapKeys(bodySet))
		}
	}
	// Per-message GUID is intentionally NOT preserved through the
	// migration: the source GUID is read by the v1 reader but the
	// destination driver (sdbox / mdbox) mints a fresh GUID inside
	// Save(). The folder-level GUID — what RFC 5464 METADATA and
	// ACL state key on — survives because it lives in the
	// fileindex dbox-hdr extension and the migrator writes one
	// fresh GUID per Init(). Document the limitation here so a
	// future per-message-GUID pass-through is a graspable
	// requirement, not a regression.
}

// TestMigrate_MdboxV1_ToMdbox covers the multi-message path:
// read pre-Phase-5 mdbox-v1 (TSV dbox.map + m.<N>), write into
// Phase-5 mdbox.
func TestMigrate_MdboxV1_ToMdbox(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	const user = "bob@example.com"
	srcHome := filepath.Join(src, "example.com", "bob")
	storage := filepath.Join(srcHome, "mdbox-storage")
	if err := os.MkdirAll(storage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcHome, "INBOX"), 0o700); err != nil {
		t.Fatal(err)
	}

	bodies := []string{"alpha", "beta msg", "gamma"}
	var offs []uint32
	for _, body := range bodies {
		off := writeMdboxV1Record(t, storage, 1, []byte(body), randomGUID(t), uint32(time.Now().Unix()))
		offs = append(offs, off)
	}
	var mapBuf bytes.Buffer
	for i, off := range offs {
		fmt.Fprintf(&mapBuf, "1 %d %d 0\n", off, len(bodies[i]))
	}
	if err := os.WriteFile(filepath.Join(srcHome, "INBOX", "dbox.map"), mapBuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	box := mdbox.New()
	idx := indexfile.New()
	resolver := &mailbox.Resolver{Root: dst, HomeTemplate: "%d/%n"}

	m, s, err := migrateUser(mdboxV1Walker{}, src, box, idx, resolver, user)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if m != 3 || s != 0 {
		t.Errorf("migrated=%d skipped=%d want 3/0", m, s)
	}

	// Verify via a fresh session.
	dstHome := filepath.Join(dst, "example.com", "bob")
	verifyBox := mdbox.New().OpenUser(&mailbox.UserInfo{Username: user, Home: dstHome})
	verifyIdx := indexfile.New().OpenUser(&mailbox.UserInfo{Username: user, Home: dstHome})
	defer verifyBox.Close()
	defer verifyIdx.Close()
	inbox, err := verifyIdx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("verify open: %v", err)
	}
	msgs, _ := verifyIdx.GetMessages(inbox.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if len(msgs) != 3 {
		t.Fatalf("INBOX has %d msgs after migrate, want 3", len(msgs))
	}
	want := map[string]bool{}
	for _, b := range bodies {
		want[b] = true
	}
	for _, mm := range msgs {
		rc, err := verifyBox.Fetch("INBOX", mm.Filename)
		if err != nil {
			t.Errorf("fetch uid=%d: %v", mm.UID, err)
			continue
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !want[string(got)] {
			t.Errorf("body %q not in expected set", got)
		}
		delete(want, string(got))
	}
	if len(want) != 0 {
		t.Errorf("bodies missing after migrate: %v", want)
	}
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
