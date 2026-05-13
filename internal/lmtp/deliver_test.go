package lmtp

import (
	"context"
	"strings"
	"testing"

	fileindex "github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

var resolveMailboxCases = []struct {
	rcpt       string
	wantUser   string
	wantFolder string
	wantErr    bool
}{
	{"user@example.com", "user@example.com", "INBOX", false},
	{"<user@example.com>", "user@example.com", "INBOX", false},
	{"user+Sent@example.com", "user@example.com", "Sent", false},
	{"user+Drafts@example.com", "user@example.com", "Drafts", false},
	{"nodomain", "", "", true},
}

func TestResolveMailbox(t *testing.T) {
	for _, tc := range resolveMailboxCases {
		tc := tc
		t.Run(tc.rcpt, func(t *testing.T) {
			user, folder, err := resolveMailbox(tc.rcpt)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q", tc.rcpt)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if user != tc.wantUser {
				t.Errorf("user = %q, want %q", user, tc.wantUser)
			}
			if folder != tc.wantFolder {
				t.Errorf("folder = %q, want %q", folder, tc.wantFolder)
			}
		})
	}
}

func TestDeliver_SingleRcpt(t *testing.T) {
	dir := t.TempDir()
	mb, err := maildir.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx := fileindex.New(dir)

	d := New(mb, idx)

	const rcpt = "alice@example.com"
	if err := mb.Init(rcpt); err != nil {
		t.Fatalf("Init: %v", err)
	}
	const msg = "From: sender@example.com\r\nTo: alice@example.com\r\nSubject: Test\r\n\r\nHello\r\n"

	results := d.Deliver(context.Background(), "sender@example.com", []string{rcpt}, strings.NewReader(msg))
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("delivery error: %v", results[0].Err)
	}

	// Verify message is indexed.
	f, err := idx.OpenFolder(rcpt, "INBOX", 0)
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 indexed message, got %d", len(msgs))
	}
	idx.Close() //nolint:errcheck
}

func TestDeliver_MultipleRcpts(t *testing.T) {
	dir := t.TempDir()
	mb, _ := maildir.New(dir)
	idx := fileindex.New(dir)
	d := New(mb, idx)

	rcpts := []string{"a@example.com", "b@example.com", "c@example.com"}
	for _, r := range rcpts {
		if err := mb.Init(r); err != nil {
			t.Fatalf("Init(%q): %v", r, err)
		}
	}
	msg := "From: x@y.com\r\n\r\nMulti\r\n"

	results := d.Deliver(context.Background(), "x@y.com", rcpts, strings.NewReader(msg))
	if len(results) != len(rcpts) {
		t.Fatalf("expected %d results, got %d", len(rcpts), len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("delivery to %q failed: %v", r.Rcpt, r.Err)
		}
	}
	idx.Close() //nolint:errcheck
}
