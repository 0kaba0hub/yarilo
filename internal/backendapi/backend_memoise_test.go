package backendapi

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// backend-API replaced its bespoke driverCache with the shared primitive; New
// must memoise so a driver builds once (#1149).
func TestBackendAPI_New_MemoisesBackendByDriver(t *testing.T) {
	var builds int
	srv := New(Options{
		MailboxByDriver: func(string) mailbox.MailboxBackend { builds++; return nil },
	})
	srv.opts.MailboxByDriver("mdbox")
	srv.opts.MailboxByDriver("mdbox")
	if builds != 1 {
		t.Errorf("mdbox backend built %d times, want 1 (#1149)", builds)
	}
}
