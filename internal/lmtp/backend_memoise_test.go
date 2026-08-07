package lmtp

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// LMTP selected the per-driver backend once per recipient per message -- the
// highest-volume write path -- so max_concurrent_writes never bounded delivery.
// New must memoise it, so two selections on one driver build once (#1149).
func TestLMTP_New_MemoisesBackendByDriver(t *testing.T) {
	var builds int
	srv := New(Options{MailboxByDriver: func(string) mailbox.MailboxBackend { builds++; return nil }})
	srv.opts.MailboxByDriver("mdbox")
	srv.opts.MailboxByDriver("mdbox")
	if builds != 1 {
		t.Errorf("mdbox backend built %d times across two deliveries, want 1 (#1149)", builds)
	}
}
