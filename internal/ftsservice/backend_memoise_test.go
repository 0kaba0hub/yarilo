package ftsservice

import (
	"context"
	"testing"

	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// FTS caches a per-user handle, so the builder ran per account -- one backend
// and one write semaphore per user, the second-heaviest write path. New must
// memoise it (#1149).
func TestFTS_New_MemoisesBackendByDriver(t *testing.T) {
	chain, err := language.NewMultiChain(nil, nil, nil, 100, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var builds int
	svc, err := New(Options{
		Engine:          stubEngine{},
		Mailbox:         maildir.New(),
		Index:           file.New(),
		ResolveUser:     func(string) (*mailbox.UserInfo, error) { return nil, nil },
		Chain:           chain,
		MailboxByDriver: func(string) mailbox.MailboxBackend { builds++; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() }) //nolint:errcheck

	svc.opts.MailboxByDriver("mdbox")
	svc.opts.MailboxByDriver("mdbox")
	if builds != 1 {
		t.Errorf("mdbox backend built %d times across two users, want 1 (#1149)", builds)
	}
}

type stubEngine struct{}

func (stubEngine) Name() string   { return "stub" }
func (stubEngine) Caps() fts.Caps { return fts.Caps{} }
func (stubEngine) OpenUser(context.Context, fts.UserRef) (fts.UserIndex, error) {
	return nil, nil
}
func (stubEngine) Close() error { return nil }
