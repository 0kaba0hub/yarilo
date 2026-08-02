package ftsservice

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// stubBackend marks which backend mailboxFor picked.
type stubBackend struct {
	mailbox.MailboxBackend
	name string
}

func TestMailboxForPicksPerUserDriver(t *testing.T) {
	global := &stubBackend{name: "global"}
	mdboxBackend := &stubBackend{name: "mdbox"}
	s := &Service{opts: Options{
		Mailbox: global,
		MailboxByDriver: func(driver string) mailbox.MailboxBackend {
			if driver == "mdbox" {
				return mdboxBackend
			}
			return nil
		},
	}}

	tests := []struct {
		name   string
		driver string
		want   string
	}{
		{"per-user mdbox driver", "mdbox", "mdbox"},
		{"unknown driver falls back to global", "sdbox", "global"},
		{"empty driver uses global", "", "global"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.mailboxFor(&mailbox.UserInfo{Driver: tc.driver}).(*stubBackend).name
			if got != tc.want {
				t.Fatalf("mailboxFor(%q) = %q, want %q", tc.driver, got, tc.want)
			}
		})
	}
}

func TestMailboxForNilFactory(t *testing.T) {
	global := &stubBackend{name: "global"}
	s := &Service{opts: Options{Mailbox: global}}
	if got := s.mailboxFor(&mailbox.UserInfo{Driver: "mdbox"}).(*stubBackend).name; got != "global" {
		t.Fatalf("nil factory must fall back to global, got %q", got)
	}
}
