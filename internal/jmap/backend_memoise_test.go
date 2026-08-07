package jmap

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// JMAP has no session -- a store is built per request -- so without memoisation
// each request's per-user handle builds a fresh backend and its own write
// semaphore. New must memoise it (#1149).
func TestJMAP_New_MemoisesBackendByDriver(t *testing.T) {
	var builds int
	srv := New(Options{Storage: &Storage{
		MailboxByDriver: func(string) mailbox.MailboxBackend { builds++; return nil },
	}})
	srv.opts.Storage.MailboxByDriver("mdbox")
	srv.opts.Storage.MailboxByDriver("mdbox")
	if builds != 1 {
		t.Errorf("mdbox backend built %d times across two requests, want 1 (#1149)", builds)
	}
}
