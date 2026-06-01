package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func randomGUID(t *testing.T) [16]byte {
	t.Helper()
	var g [16]byte
	if _, err := rand.Read(g[:]); err != nil {
		t.Fatal(err)
	}
	return g
}

// ---- maildir walker -----------------------------------------

func TestMaildirWalkerYieldsCurMessages(t *testing.T) {
	home := t.TempDir()
	for _, sub := range []string{"INBOX/cur", "INBOX/new", ".Sent/cur"} {
		if err := os.MkdirAll(filepath.Join(home, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// INBOX/cur: one seen, one new (no flags trailer).
	if err := os.WriteFile(filepath.Join(home, "INBOX", "cur", "1.M.host,S=20,W=20:2,S"), []byte("body of seen"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "INBOX", "cur", "2.M.host,S=5,W=5:2,"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".Sent", "cur", "3.M.host:2,FS"), []byte("sent body"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := map[string]sourceMessage{}
	err := maildirWalker{}.Walk(home, func(m sourceMessage) error {
		got[string(m.Body)] = m
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d msgs, want 3 (keys: %v)", len(got), keys(got))
	}
	if m, ok := got["body of seen"]; ok {
		if m.Folder != "INBOX" {
			t.Errorf("seen msg in folder %q, want INBOX", m.Folder)
		}
		if !hasFlag(m.Flags, `\Seen`) {
			t.Errorf("seen msg missing \\Seen: %v", m.Flags)
		}
	}
	if m, ok := got["sent body"]; ok {
		if m.Folder != "Sent" {
			t.Errorf("sent msg in folder %q, want Sent", m.Folder)
		}
	}
}

// ---- dbox-v1 walker -----------------------------------------

func TestDboxV1WalkerRoundTrip(t *testing.T) {
	home := t.TempDir()
	g1 := randomGUID(t)
	now := uint32(time.Now().Unix())
	writeDboxV1(t, filepath.Join(home, "INBOX"), "u.0000000000000001", []byte("hello dbox"), g1, now)

	got := []sourceMessage{}
	err := dboxV1Walker{}.Walk(home, func(m sourceMessage) error {
		got = append(got, m)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d msgs, want 1", len(got))
	}
	if !bytes.Equal(got[0].Body, []byte("hello dbox")) {
		t.Errorf("body drift: %q", got[0].Body)
	}
	if got[0].GUID != g1 {
		t.Errorf("guid drift: got %x want %x", got[0].GUID, g1)
	}
	if got[0].InternalDate.Unix() != int64(now) {
		t.Errorf("internal date drift: %v vs %d", got[0].InternalDate, now)
	}
}

// writeDboxV1 mirrors the legacy yarilo dbox.Save record shape.
func writeDboxV1(t *testing.T, dir, name string, body []byte, guid [16]byte, received uint32) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.WriteByte(0x01)
	buf.WriteByte(0x02)
	fmt.Fprintf(&buf, " N %08x %016x\n", 0, len(body))
	buf.Write(body)
	buf.WriteString("\n\x01\x03\n")
	fmt.Fprintf(&buf, "G%x\n", guid)
	fmt.Fprintf(&buf, "R%08x\n", received)
	fmt.Fprintf(&buf, "Z%08x\n", len(body))
	fmt.Fprintf(&buf, "V%08x\n", len(body))
	buf.WriteByte('\n')
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---- mdbox-v1 walker ----------------------------------------

func TestMdboxV1WalkerSkipsExpunged(t *testing.T) {
	home := t.TempDir()
	storage := filepath.Join(home, "mdbox-storage")
	if err := os.MkdirAll(storage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "INBOX"), 0o700); err != nil {
		t.Fatal(err)
	}
	g1 := randomGUID(t)
	now := uint32(time.Now().Unix())

	// Two messages in m.1; one expunged.
	off1 := writeMdboxV1Record(t, storage, 1, []byte("live one"), g1, now)
	off2 := writeMdboxV1Record(t, storage, 1, []byte("dead one"), randomGUID(t), now)

	// Map TSV:
	mapBody := fmt.Sprintf("1 %d 8 0\n1 %d 8 1\n", off1, off2)
	if err := os.WriteFile(filepath.Join(home, "INBOX", "dbox.map"), []byte(mapBody), 0o600); err != nil {
		t.Fatal(err)
	}

	got := []sourceMessage{}
	err := mdboxV1Walker{}.Walk(home, func(m sourceMessage) error {
		got = append(got, m)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d msgs, want 1 (expunged should be skipped)", len(got))
	}
	if !bytes.Equal(got[0].Body, []byte("live one")) {
		t.Errorf("body drift: %q", got[0].Body)
	}
	if got[0].GUID != g1 {
		t.Errorf("guid drift")
	}
}

func writeMdboxV1Record(t *testing.T, storage string, fileID uint32, body []byte, guid [16]byte, received uint32) uint32 {
	t.Helper()
	mpath := filepath.Join(storage, fmt.Sprintf("m.%d", fileID))
	st, _ := os.Stat(mpath)
	off := uint32(0)
	if st != nil {
		off = uint32(st.Size())
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "\x01\x02N %08x %016x\n", 0, len(body))
	buf.Write(body)
	buf.WriteString("\n\x01\x03\n")
	fmt.Fprintf(&buf, "G%x\n", guid)
	fmt.Fprintf(&buf, "R%08x\n", received)
	fmt.Fprintf(&buf, "Z%08x\n", len(body))
	fmt.Fprintf(&buf, "V%08x\n", len(body))
	buf.WriteByte('\n')
	f, err := os.OpenFile(mpath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return off
}

// ---- pickWalker ---------------------------------------------

func TestPickWalkerAccepts(t *testing.T) {
	for _, src := range []string{"maildir", "dbox-v1", "dboxv1", "mdbox-v1", "yarilo-mdbox"} {
		if _, err := pickWalker(src); err != nil {
			t.Errorf("pickWalker(%q): %v", src, err)
		}
	}
	if _, err := pickWalker("garbage"); err == nil {
		t.Error("pickWalker(garbage) should error")
	}
}

// ---- helpers ------------------------------------------------

func keys(m map[string]sourceMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}
