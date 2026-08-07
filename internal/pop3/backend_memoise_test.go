package pop3

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// POP3 selected the per-driver backend once per session, so each login built a
// fresh backend and its own write semaphore. New must memoise it, so two
// selections on one driver build once (#1149).
func TestPOP3_New_MemoisesBackendByDriver(t *testing.T) {
	var builds int
	srv := New(Options{MailboxByDriver: func(string) mailbox.MailboxBackend { builds++; return nil }})
	srv.opts.MailboxByDriver("mdbox")
	srv.opts.MailboxByDriver("mdbox")
	if builds != 1 {
		t.Errorf("mdbox backend built %d times across two selections, want 1 (#1149)", builds)
	}
}
