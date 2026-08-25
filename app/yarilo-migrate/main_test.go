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

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
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
	sourceGUIDs := map[string]bool{}
	for i, body := range bodies {
		var g [16]byte
		_, _ = rand.Read(g[:])
		sourceGUIDs[fmt.Sprintf("%x", g)] = true
		writeDboxV1(t,
			filepath.Join(srcHome, "INBOX"),
			// Sparse on purpose: 7, 9, 11. A destination that carried the
			// source UIDs through would reproduce the gaps, and the row below
			// would catch it -- with 1, 2, 3 the two implementations are
			// indistinguishable.
			fmt.Sprintf("u.%016x", 7+i*2),
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
		rc, err := verifyBox.Fetch("INBOX", m.Filename, false)
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
	// Per-message GUIDs come across. The migrator passes the source id into
	// Save, which mints one only for a message that arrives without one --
	// so a message keeps its identity across a conversion even though its UID
	// does not, and a JMAP client recognises mail it already has.
	//
	// This comment used to say the opposite, describing the behaviour before
	// the pass-through was added; the public migration page now states the
	// preservation, so the claim is asserted here rather than believed.
	metas, err := verifyIdx.GetMessages(inbox.ID, nil)
	if err != nil {
		t.Fatalf("read destination index: %v", err)
	}
	var carried, minted int
	for _, m := range metas {
		if sourceGUIDs[fmt.Sprintf("%x", m.GUID)] {
			carried++
			continue
		}
		minted++
	}
	if carried != len(bodies) {
		t.Errorf("%d of %d INBOX messages kept their GUID (%d were minted fresh) -- a converted mailbox loses its EMAILIDs, and every JMAP client sees new mail",
			carried, len(bodies), minted)
	}
	// UIDs are the destination's own: identity survives, position does not.
	// The source was seeded sparsely (7, 9, 11), so carrying its UIDs through
	// would show as gaps here -- which is the only way this row can tell the
	// two behaviours apart.
	seen := map[uint32]bool{}
	for _, m := range metas {
		seen[m.UID] = true
	}
	for uid := uint32(1); uid <= uint32(len(bodies)); uid++ {
		if !seen[uid] {
			t.Errorf("destination UIDs are %v, want a dense 1..%d -- the source's own numbering came through, and a client that had synced the source would collide",
				seen, len(bodies))
			break
		}
	}
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
		rc, err := verifyBox.Fetch("INBOX", mm.Filename, false)
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
