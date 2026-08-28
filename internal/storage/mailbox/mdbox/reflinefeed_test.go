package mdbox

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

// The reference stores bodies with bare LF. Serving those bytes as they lie
// puts bare LF on the wire, which RFC 3501 does not allow, and it is what this
// server did until #1527.
//
// The fixture's third record is 67 bytes on disk over five lines, and its
// trailer says V48 -- 72 -- which is the same body counted with CRLF.
func TestAReferenceBodyIsServedAsCRLF(t *testing.T) {
	const (
		off      = 4690
		physical = 67
		virtual  = 72
	)
	path := filepath.Join(t.TempDir(), "m.1")
	if err := os.WriteFile(path, dboxref.MdboxFile(t), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := openRecordBody(f, off)
	if err != nil {
		t.Fatalf("open body: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if len(got) != virtual {
		t.Errorf("served %d octets, want %d -- the body is %d on disk and the trailer counts it as %d",
			len(got), virtual, physical, virtual)
	}
	for i := 0; i < len(got); i++ {
		if got[i] == '\n' && (i == 0 || got[i-1] != '\r') {
			t.Fatalf("a bare LF reached the caller at offset %d", i)
		}
	}
}

// What the client is promised has to be what it is given.
//
// Reporting the physical size while serving CRLF is a five-octet lie on this
// record, and it is the shape the drivers disagreed on before #1527: sdbox took
// V from the trailer, mdbox took the physical size from the record header.
func TestTheScanReportsTheSizeThatWillBeServed(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, mdboxRoot, storageDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "m.1"), dboxref.MdboxFile(t), 0o600); err != nil {
		t.Fatal(err)
	}

	u := &userMailbox{home: home}
	recs, err := u.scanMFileAt(filepath.Join(dir, "m.1"))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("scanned %d records, want 3", len(recs))
	}
	for i, want := range []struct{ size, vsize uint32 }{
		{62, 67},
		{4430, 4494},
		{67, 72},
	} {
		if recs[i].scan.Size != want.size {
			t.Errorf("record %d physical size %d, want %d", i+1, recs[i].scan.Size, want.size)
		}
		if recs[i].scan.VSize != want.vsize {
			t.Errorf("record %d reports %d octets, want %d -- that is what Fetch now serves",
				i+1, recs[i].scan.VSize, want.vsize)
		}
	}
}
