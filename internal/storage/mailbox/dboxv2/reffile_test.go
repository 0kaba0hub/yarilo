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
	for _, tc := range []struct {
		name    string
		file    []byte
		size    int
		subject string
	}{
		{"short body", dboxref.SdboxShort(t), 63, "sfirst"},
		{"body past the peek window", dboxref.SdboxLong(t), 4431, "slong"},
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
