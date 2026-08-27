package mdbox

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
)

// fileHeaderPrefix is the stable start of the dbox v2 file-header line
// ("<version> M<hdr-size> C<stamp>\n"): version 2, message-header size 0x1e --
// the reference's 30 bytes. Stores written before #1522 announce M20 and are
// still read; nothing writes M20 any more.
const fileHeaderPrefix = "2 M1e C"

// TestPeekFileHeaderLen locks the discriminator that makes the reader
// self-describing: a message header (magic 0x01) has no file-header line, a line
// starting with the ASCII version digit does, and anything else is malformed.
func TestPeekFileHeaderLen(t *testing.T) {
	cases := []struct {
		name     string
		window   []byte
		wantSkip int
		wantOK   bool
	}{
		{"message header directly (no file header)", []byte{magicPreByte0, magicPreByte1, 'N', ' '}, 0, true},
		{"file header line then message", []byte("2 M1e C1a2b3c\n\x01\x02N"), len("2 M1e C1a2b3c\n"), true},
		{"a store written before #1522 announces M20", []byte("2 M20 C1a2b3c\n\x01\x02N"), len("2 M20 C1a2b3c\n"), true},
		{"malformed: no magic, no LF", []byte("2 M1e C no newline here"), 0, false},
		{"empty window", []byte{}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			skip, ok := peekFileHeaderLen(c.window)
			if skip != c.wantSkip || ok != c.wantOK {
				t.Errorf("peekFileHeaderLen = (%d,%v), want (%d,%v)", skip, ok, c.wantSkip, c.wantOK)
			}
		})
	}
}

// TestSaveWritesFileHeaderOncePerFile is the core #622 regression: several
// messages that all land in one m.<N> must produce the file-header line exactly
// once (at offset 0), not once per message — the divergence that broke the reference implementation
// interop. Every body must still fetch back intact.
func TestSaveWritesFileHeaderOncePerFile(t *testing.T) {
	mb, home := newTestUser(t)
	u := mb.(*userMailbox)

	bodies := []string{"first message\r\n", "second message\r\n", "third message\r\n"}
	for _, b := range bodies {
		if _, _, _, err := u.Save("INBOX", strings.NewReader(b), 0, int64(len(b)), nil, [16]byte{}); err != nil {
			t.Fatalf("save %q: %v", b, err)
		}
	}
	_ = home

	raw, err := os.ReadFile(u.mfilePath(1))
	if err != nil {
		t.Fatalf("read m.1: %v", err)
	}
	if got := bytes.Count(raw, []byte(fileHeaderPrefix)); got != 1 {
		t.Fatalf("file-header line appears %d times in m.1, want exactly 1 (dbox v2 layout)", got)
	}

	// Every message still fetches back.
	for i, b := range bodies {
		rc, err := u.Fetch("INBOX", strconv.Itoa(i+1), false)
		if err != nil {
			t.Fatalf("fetch uid=%d: %v", i+1, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != b {
			t.Errorf("uid=%d body = %q, want %q", i+1, got, b)
		}
	}
}

// TestAppendRecordToFileHeaderOnce covers the compaction writer (purge/altmove):
// the destination file gets the header line only before its first record.
func TestAppendRecordToFileHeaderOnce(t *testing.T) {
	dstPath := t.TempDir() + "/m.99"
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	offsets := make([]uint32, 3)
	for i := range offsets {
		off, err := appendRecordToFile(dst, []byte("body\r\n"), randomGUID(), "INBOX")
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		offsets[i] = off
	}
	dst.Close()

	raw, _ := os.ReadFile(dstPath)
	if got := bytes.Count(raw, []byte(fileHeaderPrefix)); got != 1 {
		t.Fatalf("compaction dst has %d header lines, want 1", got)
	}
	// Only the first record carries a file-header line.
	rf, _ := os.Open(dstPath)
	defer rf.Close()
	for i, off := range offsets {
		win := make([]byte, 64)
		rf.Seek(int64(off), io.SeekStart) //nolint:errcheck
		n, _ := rf.Read(win)
		skip, ok := peekFileHeaderLen(win[:n])
		if !ok {
			t.Fatalf("record %d @%d unreadable", i, off)
		}
		if i == 0 && skip == 0 {
			t.Errorf("first record must carry the file-header line")
		}
		if i > 0 && skip != 0 {
			t.Errorf("record %d must start at its message header (skip=%d)", i, skip)
		}
	}
}

// TestReaderAcceptsLegacyAndNewLayout is the backward-compat guarantee: a file in
// the LEGACY layout (file-header line before every record) and one in the NEW
// layout (file-header line once) with identical messages both parse to the same
// record set and the same bodies. Mirrors the ORIG_MAILBOX unknown-key skip of
// #614: old on-disk stores stay readable without migration.
func TestReaderAcceptsLegacyAndNewLayout(t *testing.T) {
	mb, _ := newTestUser(t)
	u := mb.(*userMailbox)

	bodies := [][]byte{[]byte("alpha\r\n"), []byte("bravo\r\n"), []byte("charlie\r\n")}

	// Legacy: buildDboxRecord (file header + message) per record.
	var legacy bytes.Buffer
	for _, b := range bodies {
		legacy.Write(buildDboxRecord(b, randomGUID(), "INBOX"))
	}
	// New: one file header, then message records only.
	var modern bytes.Buffer
	modern.Write(buildDboxFileHeader())
	for _, b := range bodies {
		modern.Write(buildDboxMessageRecord(b, randomGUID(), "INBOX"))
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"legacy per-record header", legacy.Bytes()},
		{"new file-header once", modern.Bytes()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/m.1"
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			recs, err := u.scanMFileAt(path)
			if err != nil {
				t.Fatalf("scanMFileAt: %v", err)
			}
			if len(recs) != len(bodies) {
				t.Fatalf("scanned %d records, want %d", len(recs), len(bodies))
			}
			f, _ := os.Open(path)
			defer f.Close()
			for i, r := range recs {
				got, err := readRecordBody(f, r.physicalOffset)
				if err != nil {
					t.Fatalf("readRecordBody @%d: %v", r.physicalOffset, err)
				}
				if !bytes.Equal(got, bodies[i]) {
					t.Errorf("record %d body = %q, want %q", i, got, bodies[i])
				}
			}
		})
	}
}
