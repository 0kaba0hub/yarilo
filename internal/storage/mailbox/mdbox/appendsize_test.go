package mdbox

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A message appended to a file that announces the old size must be written at
// that size, and must read back.
//
// This is the shape of #1525: the readers were taught to take the header size
// from M and the writer was left on a constant, so a delivery into an existing
// M20 file wrote a 30-byte header that every reader then read as 32. LMTP
// answered Saved, the message took quota, and every FETCH of it came back
// empty. Nothing logged, nothing counted -- the only symptom was mail nobody
// could open.
func TestAppendingToAnOldFileUsesTheSizeItAnnounces(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)

	// A store as builds before #1523 wrote it: file header M20, one 32-byte
	// record.
	const first = "From: a@a.com\r\nSubject: old\r\n\r\nfirst\r\n"
	if err := os.MkdirAll(filepath.Dir(u.mfilePath(1)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(u.mfilePath(1), referenceRecord(32, first, [16]byte{9}), 0o600); err != nil {
		t.Fatal(err)
	}
	// Save appends to the highest file id, which is m.1 here: no map entry is
	// needed for the append itself, and the append is what this is about.
	// Now deliver into it.
	const second = "From: b@b.com\r\nSubject: new\r\n\r\nsecond\r\n"
	fn, _, _, err := u.Save("INBOX", strings.NewReader(second), 0, int64(len(second)), nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	rc, err := u.Fetch("INBOX", fn, false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != second {
		t.Fatalf("the appended message reads as %q, want %q — "+
			"it was accepted, it occupies quota, and no client can open it", got, second)
	}

	// And on disk, at the size the file announces.
	//
	// Asserted on the bytes rather than on the read-back above, because the
	// reader recovers a record written at the other size -- so a broken writer
	// still round-trips through our own code and only shows itself to another
	// implementation, or in the warning log. Reading it back proves the mail is
	// there; only the bytes prove it was written correctly.
	raw, err := os.ReadFile(u.mfilePath(1))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("2 M20 C")) {
		t.Fatalf("the file header changed under an append: %q", raw[:8])
	}
	line := bytes.IndexByte(raw, '\n') + 1
	// first record: 32-byte header + body + trailer; find where the second
	// record's header starts by walking the first one.
	firstHdr := raw[line : line+messageHeaderSizeLegacy]
	if firstHdr[len(firstHdr)-1] != '\n' {
		t.Fatalf("the seeded record is not 32 bytes as written")
	}
	secondHdrOff := bytes.Index(raw[line+messageHeaderSizeLegacy:], []byte{magicPreByte0, magicPreByte1, 'N'})
	if secondHdrOff < 0 {
		t.Fatal("no second record on disk")
	}
	secondHdr := raw[line+messageHeaderSizeLegacy+secondHdrOff:]
	if secondHdr[messageHeaderSizeLegacy-1] != '\n' {
		t.Errorf("the appended header is not %d bytes, though the file announces M20: "+
			"byte %d is %q — every reader takes the size from M and would land inside the body",
			messageHeaderSizeLegacy, messageHeaderSizeLegacy-1, secondHdr[messageHeaderSizeLegacy-1])
	}
}

// A record already written at the wrong size is recovered rather than lost.
//
// Mail delivered by a build between #1523 and #1525 is on disk and intact; only
// the size assumed for it was wrong. Purge does not repair it -- it compacts
// live records and this one does not read -- so if the reader cannot recover
// it, it is gone for good.
func TestARecordWrittenAtTheOtherSizeIsStillRead(t *testing.T) {
	for _, tc := range []struct {
		name              string
		announced, actual int
	}{
		{"file says M20, record written at 30 — the #1525 case", 32, 30},
		{"file says M1e, record written at 32 — the same mistake reversed", 30, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const body = "From: a@a.com\r\nSubject: mixed\r\n\r\nrecovered\r\n"
			path := filepath.Join(t.TempDir(), "m.1")

			// Announce one size, write the record at the other.
			var buf bytes.Buffer
			buf.WriteString("2 M" + map[int]string{30: "1e", 32: "20"}[tc.announced] + " C1a2b3c\n")
			rec := referenceRecord(tc.actual, body, [16]byte{7})
			buf.Write(rec[bytes.IndexByte(rec, '\n')+1:]) // record without its own header line
			if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}

			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close() //nolint:errcheck

			got, err := readRecordBody(f, 0)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != body {
				t.Errorf("body = %q, want %q", got, body)
			}
		})
	}
}
