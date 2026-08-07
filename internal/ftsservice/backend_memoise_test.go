package ftsservice

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/yarilomail/yarilo/internal/fts/language"
	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type instrumentedBackend struct {
	inner mailbox.MailboxBackend
	opens *atomic.Int64
}

func (b instrumentedBackend) OpenUser(ui *mailbox.UserInfo) mailbox.UserMailbox {
	b.opens.Add(1)
	return b.inner.OpenUser(ui)
}

// FTS caches a per-user handle, so the backend was selected per ACCOUNT -- one
// backend and one write semaphore per user, the second-heaviest write path after
// LMTP. Two users on one driver must share the built instance, and the handle
// path must be what uses it (#1149, #1155).
func TestFTS_UserHandlesShareTheMemoisedBackend(t *testing.T) {
	dir := t.TempDir()
	chain, err := language.NewMultiChain(nil, nil, nil, 100, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var builds, opens atomic.Int64

	svc, err := New(Options{
		Engine:  stubEngine{},
		Mailbox: maildir.New(), // global default; the per-user driver differs
		Index:   fileindex.New(),
		ResolveUser: func(u string) (*mailbox.UserInfo, error) {
			home := filepath.Join(dir, u)
			return &mailbox.UserInfo{
				Username: u, Home: home,
				MailPath: filepath.Join(home, "mdbox"), Driver: "mdbox",
			}, nil
		},
		Chain: chain,
		MailboxByDriver: func(string) mailbox.MailboxBackend {
			builds.Add(1)
			return instrumentedBackend{inner: mdbox.New(), opens: &opens}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() }) //nolint:errcheck

	for _, u := range []string{"alice@test.com", "bob@test.com"} {
		if _, herr := svc.handle(u); herr != nil {
			t.Fatalf("handle(%s): %v", u, herr)
		}
	}
	if got := builds.Load(); got != 1 {
		t.Errorf("backend built %d times across two users, want 1 (#1149)", got)
	}
	if got := opens.Load(); got < 2 {
		t.Errorf("the memoised backend served %d opens, want >= 2: the handle path did not go through it (#1155)", got)
	}
}

type stubEngine struct{}

func (stubEngine) Name() string   { return "stub" }
func (stubEngine) Caps() fts.Caps { return fts.Caps{} }
func (stubEngine) OpenUser(context.Context, fts.UserRef) (fts.UserIndex, error) {
	return stubUserIndex{}, nil
}
func (stubEngine) Close() error { return nil }

type stubUserIndex struct{}

func (stubUserIndex) Checkpoint(fts.MailboxRef) (uint32, uint32, uint32, error)  { return 0, 0, 0, nil }
func (stubUserIndex) SetCheckpoint(fts.MailboxRef, uint32, uint32, uint32) error { return nil }
func (stubUserIndex) BeginUpdate(fts.MailboxRef) (fts.Update, error)             { return nil, nil }
func (stubUserIndex) Expunge(fts.MailboxRef, uint32) error                       { return nil }
func (stubUserIndex) Rescan(fts.MailboxRef, []uint32) ([]uint32, error)          { return nil, nil }
func (stubUserIndex) Optimize() error                                            { return nil }
func (stubUserIndex) OptimizeMailbox(fts.MailboxRef) error                       { return nil }
func (stubUserIndex) Refresh() error                                             { return nil }
func (stubUserIndex) Lookup(fts.MailboxRef, fts.Query) (fts.Result, error)       { return fts.Result{}, nil }
func (stubUserIndex) Close() error                                               { return nil }
