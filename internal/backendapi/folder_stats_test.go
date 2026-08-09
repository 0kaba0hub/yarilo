package backendapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// statsServer stores one message in INBOX on the given driver.
func statsServer(t *testing.T, be mailbox.MailboxBackend) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	const user = "alice@example.com"

	info := resolver.UserInfo(user, "")
	box := be.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	idx := file.New()
	ui := idx.OpenUser(info)
	raw := "Subject: t\r\n\r\nbody\r\n"
	name, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), 1, int64(len(raw)), nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	f, err := ui.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: 1, Filename: name, Size: uint32(len(raw)), VSize: vsize, GUID: guid,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = ui.Close()
	_ = box.Close()

	s := New(Options{
		Mailbox:  be,
		Index:    file.New(),
		Resolver: resolver,
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true},
		},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, user
}

// A driver that cannot count files per folder must say so, not answer zero.
// Zero is a fact about an empty folder, and reporting it for a folder holding
// messages breaks every comparison between drivers and fires every "empty on
// disk" alert forever (#1224).
//
// Both directions, or the test pins nothing: the file-per-message driver must
// still return numbers for the same folder with the same message in it.
func TestFolderStatsSaysNullWhereItCannotCount(t *testing.T) {
	cases := []struct {
		name     string
		backend  mailbox.MailboxBackend
		wantNull bool
	}{
		{"maildir counts files", maildir.New(), false},
		{"sdbox stores a file per message", dboxv2.New(), false},
		{"mdbox keeps messages user-wide", mdbox.New(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, user := statsServer(t, tc.backend)
			_, raw := doJSON(t, ts, http.MethodPost, "/api/backend/folder/stats", "",
				map[string]any{"user": user, "folder": "INBOX"})

			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("decode: %v: %s", err, raw)
			}
			// The folder does hold a message either way, which is what makes
			// a zero here a lie rather than a coincidence.
			if got := out["messages"]; got != float64(1) {
				t.Fatalf("messages = %v, want 1 — the fixture stored nothing", got)
			}
			for _, field := range []string{"on_disk_count", "size_bytes"} {
				value, present := out[field]
				if !present {
					t.Errorf("%s is missing entirely; it must be present and null", field)
					continue
				}
				if tc.wantNull && value != nil {
					t.Errorf("%s = %v, want null: this driver has no per-folder count", field, value)
				}
				if !tc.wantNull && value == nil {
					t.Errorf("%s is null although this driver stores a file per message", field)
				}
			}
		})
	}
}
