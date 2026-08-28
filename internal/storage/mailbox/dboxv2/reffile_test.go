package dboxv2

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

// Files the reference implementation wrote are read, body and trailer both.
//
// The long one is the row that matters: its body runs past the window a reader
// peeks at to find the record header, so a reader that happened to fit the
// whole file in one read is not what is being measured here.
func TestReferenceFilesAreRead(t *testing.T) {
	// The sizes are the trailer's V, not the size in the record header: since
	// #1527 the body goes out CRLF-terminated, and these fixtures are stored
	// with bare LF. 63 and 4431 are what lies on disk; 68 and 4495 are what a
	// client is told and given.
	for _, tc := range []struct {
		name    string
		file    []byte
		size    int
		subject string
	}{
		{"short body", dboxref.SdboxShort(t), 68, "sfirst"},
		{"body past the peek window", dboxref.SdboxLong(t), 4495, "slong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mb, _ := newTestUser(t)
			u := mb.(*userMailbox)
			path := filepath.Join(u.folderPath("INBOX"), "u.1")
			if err := os.WriteFile(path, tc.file, 0o600); err != nil {
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
			if len(got) != tc.size {
				t.Errorf("body is %d bytes, want %d", len(got), tc.size)
			}
			if !strings.Contains(string(got), "Subject: "+tc.subject) {
				t.Errorf("body does not carry Subject: %s", tc.subject)
			}

			// The reference writes the trailer keys in the order R, V, G, B;
			// ours writes G, R, V, B. A reader that assumed a position rather
			// than a key comes back with an empty GUID, and a rebuild then
			// loses message identity.
			guid, _, _, err := readMetadata(path)
			if err != nil {
				t.Fatalf("read metadata: %v", err)
			}
			if guid == ([16]byte{}) {
				t.Error("guid came back empty, so the trailer was not reached or not parsed")
			}
		})
	}
}

// A reference file's body is served as CRLF, and what the metadata promises is
// what comes out.
//
// sdbox already took V from the trailer, so before #1527 it promised 63 octets
// on the short fixture and delivered 62 with a bare LF in them. The size was
// never the wrong number -- the bytes were.
func TestAReferenceBodyIsServedAsCRLF(t *testing.T) {
	for _, tc := range []struct {
		name     string
		file     []byte
		physical int
	}{
		{"short body", dboxref.SdboxShort(t), 63},
		{"body past the peek window", dboxref.SdboxLong(t), 4431},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mb, _ := newTestUser(t)
			u := mb.(*userMailbox)
			path := filepath.Join(u.folderPath("INBOX"), "u.1")
			if err := os.WriteFile(path, tc.file, 0o600); err != nil {
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

			for i := 0; i < len(got); i++ {
				if got[i] == '\n' && (i == 0 || got[i-1] != '\r') {
					t.Fatalf("a bare LF reached the caller at offset %d", i)
				}
			}
			_, vsize, _, err := readMetadata(path)
			if err != nil {
				t.Fatalf("read metadata: %v", err)
			}
			if int(vsize) != len(got) {
				t.Errorf("the trailer promises %d octets and %d were served (physical size on disk is %d)",
					vsize, len(got), tc.physical)
			}
		})
	}
}

// A message this server stored comes back byte for byte. The bodies here are
// already CRLF, so the conversion must be a no-op -- otherwise every fetch of
// ordinary mail would gain a CR per line.
func TestOurOwnMessageIsServedUnchanged(t *testing.T) {
	_, mb, _ := newTestUser(t)
	body := "From: y@y.com\r\nSubject: ours\r\n\r\nline one\r\nline two\r\n"
	name, _, _, err := mb.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{7})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := mb.Fetch("INBOX", name, false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("served %q, stored %q", got, body)
	}
}
