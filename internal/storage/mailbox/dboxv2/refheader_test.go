package dboxv2

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// referenceFile builds a u.<uid> file the way the reference dbox v2
// implementation writes it: a file-header line announcing the header size, a
// message header of exactly that size with LF as the LAST byte, the body, then
// the metadata trailer.
//
// From bytes rather than through encodeMessageHeader: a round-trip through our
// own writer proves only that we agree with ourselves, which is what the
// 32-byte header did while every file the reference wrote was being refused.
func referenceFile(hdrSize int, body string, guid [16]byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "2 M%x C1a2b3c\n", hdrSize)
	hdr := bytes.Repeat([]byte{' '}, hdrSize)
	hdr[0], hdr[1], hdr[2] = magicPre[0], magicPre[1], messageType
	copy(hdr[13:29], fmt.Sprintf("%016x", len(body)))
	hdr[hdrSize-1] = '\n'
	buf.Write(hdr)
	buf.WriteString(body)
	buf.WriteString(magicPost)
	fmt.Fprintf(&buf, "%c%s\n", metaKeyGUID, hex.EncodeToString(guid[:]))
	fmt.Fprintf(&buf, "%c%x\n", metaKeyReceived, 0x1a2b3c)
	fmt.Fprintf(&buf, "%c%x\n", metaKeyVirtualSize, len(body))
	buf.WriteByte('\n')
	return buf.Bytes()
}

// A file written by the reference is read, and so is one written by a build of
// ours from before the change.
//
// The 30-byte case did not merely read wrongly before: decodeMessageHeader
// checked byte 31 for LF, and in a 30-byte header byte 31 is the second byte of
// the body — so every file the reference wrote was refused outright with
// "missing trailing newline at offset 31" (#1522). Reading and refusing are
// different failures, and this row is the one that told them apart.
func TestFilesAreReadAtTheSizeTheyAnnounce(t *testing.T) {
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
			_, mb, _ := newTestUser(t)
			u := mb.(*userMailbox)
			path := filepath.Join(u.folderPath("INBOX"), "u.1")
			if err := os.WriteFile(path, referenceFile(tc.hdrSize, body, guid), 0o600); err != nil {
				t.Fatal(err)
			}

			rc, err := mb.Fetch("INBOX", "u.1", false)
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			got, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != body {
				t.Errorf("body read as %q, want %q", got, body)
			}

			// The metadata reader walks the same header and must land on the
			// trailer, or the GUID comes back empty and a rebuild loses
			// message identity.
			gotGUID, _, _, err := readMetadata(path)
			if err != nil {
				t.Fatalf("read metadata: %v", err)
			}
			if gotGUID != guid {
				t.Errorf("guid = %x, want %x", gotGUID, guid)
			}
		})
	}
}

// What we write is what the reference expects.
func TestWeWriteTheReferenceHeader(t *testing.T) {
	if got := string(encodeFileHeaderLine(0x1a2b3c)); got != "2 M1e C1a2b3c\n" {
		t.Errorf("file header = %q, want it to announce M1e", got)
	}
	hdr := encodeMessageHeader(messageHeader{Size: 6})
	if len(hdr) != 30 {
		t.Fatalf("message header is %d bytes, want 30", len(hdr))
	}
	if hdr[29] != '\n' {
		t.Errorf("byte 29 is %q, want LF as the last byte", hdr[29])
	}
	if hdr[0] != magicPre[0] || hdr[1] != magicPre[1] || hdr[2] != messageType {
		t.Errorf("header starts %x", hdr[:3])
	}
}
