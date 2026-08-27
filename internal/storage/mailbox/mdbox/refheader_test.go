package mdbox

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// referenceRecord builds one m.* file the way the reference dbox v2
// implementation writes it: a file-header line announcing the header size, a
// message header of exactly that size with the 16-char hex body size at 13..29
// and LF as the LAST byte, the body, then the metadata trailer.
//
// Built from bytes rather than from our writer on purpose. A round-trip through
// our own code proves we agree with ourselves, which is what the 32-byte header
// did for months while the reference refused to append to our files (#1522).
func referenceRecord(hdrSize int, body string, guid [16]byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "2 M%x C1a2b3c\n", hdrSize)
	hdr := bytes.Repeat([]byte{' '}, hdrSize)
	hdr[0], hdr[1], hdr[2] = magicPreByte0, magicPreByte1, 'N'
	copy(hdr[13:29], fmt.Sprintf("%016x", len(body)))
	hdr[hdrSize-1] = '\n'
	buf.Write(hdr)
	buf.WriteString(body)
	buf.WriteString(magicPost)
	fmt.Fprintf(&buf, "G%s\n", hex.EncodeToString(guid[:]))
	fmt.Fprintf(&buf, "R%x\n", 0x1a2b3c)
	fmt.Fprintf(&buf, "V%x\n", len(body))
	buf.WriteByte('\n')
	return buf.Bytes()
}

// A record written by the reference reads from its first body byte, and so does
// one written by a build of ours from before the change. The size comes from M
// in the file header; nothing here may assume the size this binary writes.
func TestRecordsAreReadAtTheSizeTheFileAnnounces(t *testing.T) {
	const body = "From: a@a.com\r\nSubject: reference\r\n\r\nhello\r\n"
	guid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	for _, tc := range []struct {
		name    string
		hdrSize int
	}{
		{"30 bytes, as the reference writes and as we write now", 30},
		{"32 bytes, as we wrote before #1522", 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "m.1")
			if err := os.WriteFile(path, referenceRecord(tc.hdrSize, body, guid), 0o600); err != nil {
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
				t.Errorf("body read as %q, want %q", trim(string(got)), trim(body))
			}

			// And the trailer parses after a header of that size: a body read
			// two bytes wide leaves the file positioned inside the body, and
			// the GUID comes back empty or wrong.
			gotBody, gotGUID, _, err := readRecordBodyAndTrailer(f, 0)
			if err != nil {
				t.Fatalf("read with trailer: %v", err)
			}
			if string(gotBody) != body {
				t.Errorf("body via trailer path read as %q", trim(string(gotBody)))
			}
			if gotGUID != guid {
				t.Errorf("guid = %x, want %x", gotGUID, guid)
			}
		})
	}
}

// What we write is what the reference expects: M1e, a 30-byte header, LF last.
func TestWeWriteTheReferenceHeader(t *testing.T) {
	rec := buildDboxRecord([]byte("body\r\n"), [16]byte{}, "INBOX")

	if !bytes.HasPrefix(rec, []byte("2 M1e C")) {
		t.Errorf("file header is %q, want it to announce M1e", trim(string(rec[:16])))
	}
	line := bytes.IndexByte(rec, '\n') + 1
	hdr := rec[line : line+30]
	if hdr[0] != magicPreByte0 || hdr[1] != magicPreByte1 || hdr[2] != 'N' {
		t.Errorf("message header starts %x, want 0102 4e", hdr[:3])
	}
	if hdr[29] != '\n' {
		t.Errorf("byte 29 is %q, want LF as the last byte of a 30-byte header", hdr[29])
	}
	if got := strings.TrimSpace(string(hdr[13:29])); got != fmt.Sprintf("%016x", len("body\r\n")) {
		t.Errorf("size field is %q", got)
	}
}

func trim(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
