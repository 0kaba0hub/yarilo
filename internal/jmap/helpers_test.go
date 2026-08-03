package jmap

import (
	"encoding/hex"
	"io/fs"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// storedServerWithMessage delivers one raw message into INBOX and returns the
// server plus that message's JMAP id, so a test can ask for it by id.
func storedServerWithMessage(t *testing.T, raw string, ceiling uint32) (*Server, string) {
	t.Helper()
	s, id, _ := storedServerWithMessageAt(t, raw, ceiling)
	return s, id
}

// storedServerWithMessageAt also returns the user's home, so a test can take
// the message away and prove a code path never reads it.
func storedServerWithMessageAt(t *testing.T, raw string, ceiling uint32) (*Server, string, string) {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	locker := &testLocker{}

	mb := maildir.New()
	box := mb.OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}

	idx := file.New(file.WithLocker(locker))
	ui := idx.OpenUser(info)

	flags := []string{`\Seen`}
	name, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), 1, int64(len(raw)), flags, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	f, err := ui.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	// A real delivery stamps InternalDate; without it the date filters would be
	// tested against the zero time, which is before every plausible bound.
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: 1, Filename: name, Size: uint32(len(raw)), VSize: vsize, Flags: flags, GUID: guid,
		InternalDate: time.Now(),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := ui.Close(); err != nil {
		t.Fatalf("index close: %v", err)
	}

	s := New(Options{
		Trust:             ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits:            testLimits(),
		MaxBodyValueBytes: ceiling,
		Storage: &Storage{
			Mailbox:     maildir.New(),
			Index:       file.New(file.WithLocker(locker)),
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:      locker,
		},
	})
	return s, hex.EncodeToString(guid[:]), home
}

// removeMailFiles deletes every delivered message but leaves the index, so a
// read of the message fails while the index still answers.
func removeMailFiles(t *testing.T, home string) {
	t.Helper()
	err := filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.Contains(path, "/cur/") || strings.Contains(path, "/new/") {
			return os.Remove(path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("remove mail files: %v", err)
	}
}

// postAPIRaw posts a batch and returns the recorder, for the cases that need
// the failure rather than the result.
func postAPIRaw(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, apiRequest(body))
	return w
}
