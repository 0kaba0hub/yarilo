package specialuse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// countingLocker records how many times the store goes to the lock service.
// Counting is the assertion, not wall-clock: a fast enough lock service would
// let an O(N) implementation pass a timing test and prove nothing.
type countingLocker struct {
	// Embedded and left nil on purpose: a call this double does not implement
	// panics instead of being silently answered, so a store that started using
	// some other part of the lock service would be seen rather than counted as
	// zero.
	locks.Locker
	acquired atomic.Int64
	released atomic.Int64
}

func (c *countingLocker) Lock(_ context.Context, resource, owner string, _ time.Duration) (locks.Lock, error) {
	c.acquired.Add(1)
	return locks.Lock{ID: fmt.Sprintf("%s/%s", resource, owner), Resource: resource, Owner: owner}, nil
}

func (c *countingLocker) LockShared(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	return c.Lock(ctx, resource, owner, ttl)
}

func (c *countingLocker) Unlock(_ context.Context, _ string) error {
	c.released.Add(1)
	return nil
}

func (c *countingLocker) HoldsResource(_ string) bool { return false }

// Resolving every folder in a listing must cost one trip to the lock service,
// not one per folder. The document is per user: asking per name asks the same
// question again, and each ask is an acquire and a release over the network, so
// a user with 35 folders paid 70 round trips for a single LIST (#1240).
func TestResolvingEveryFolderCostsOneRoundTrip(t *testing.T) {
	for _, folders := range []int{1, 35, 200} {
		t.Run(fmt.Sprintf("%d folders", folders), func(t *testing.T) {
			lk := &countingLocker{}
			store := New(t.TempDir(), "alice@example.com", "owner", lk,
				map[string]string{"Sent": `\Sent`, "Drafts": `\Drafts`})

			attrs := store.Attrs()
			for i := range folders {
				_ = attrs[fmt.Sprintf("Folder%d", i)]
			}
			_ = attrs["Sent"]

			if got := lk.acquired.Load(); got != 1 {
				t.Errorf("%d folders took %d lock acquisitions, want 1", folders, got)
			}
			if got := lk.released.Load(); got != 1 {
				t.Errorf("%d folders took %d lock releases, want 1", folders, got)
			}
		})
	}
}

// The cheap read must still be the correct one: overrides laid over defaults,
// with the override winning. A snapshot that dropped a layer would be O(1) and
// wrong.
func TestOneReadStillLayersOverridesOverDefaults(t *testing.T) {
	lk := &countingLocker{}
	store := New(t.TempDir(), "alice@example.com", "owner", lk,
		map[string]string{"Sent": `\Sent`, "Drafts": `\Drafts`})
	if err := store.Set("Archive", `\Archive`); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := store.Set("Sent", `\Junk`); err != nil {
		t.Fatalf("set: %v", err)
	}

	attrs := store.Attrs()
	for folder, want := range map[string]imaplib.MailboxAttr{
		"Sent":    `\Junk`,    // override beats the default
		"Drafts":  `\Drafts`,  // default survives
		"Archive": `\Archive`, // override with no default
		"Random":  "",
	} {
		if got := attrs[folder]; got != want {
			t.Errorf("%s = %q, want %q", folder, got, want)
		}
	}
}

// failingLocker makes the read fail the way an unreachable lock service would.
type failingLocker struct {
	locks.Locker
}

func (failingLocker) HoldsResource(_ string) bool { return false }

func (failingLocker) Lock(_ context.Context, _, _ string, _ time.Duration) (locks.Lock, error) {
	return locks.Lock{}, errors.New("lock service unreachable")
}

func (f failingLocker) LockShared(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	return f.Lock(ctx, resource, owner, ttl)
}

// Falling back to the defaults is the right answer — a presentation hint is not
// worth failing a LIST over — but it must not be a silent one. A degraded
// answer nobody is told about cannot be told apart from a correct one, which is
// the failure this whole change is about.
func TestUnreadableOverridesFallBackLoudly(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(old)

	store := New(t.TempDir(), "alice@example.com", "owner", failingLocker{},
		map[string]string{"Sent": `\Sent`})

	attrs := store.Attrs()
	if got := attrs["Sent"]; got != `\Sent` {
		t.Errorf("Sent = %q, want the configured default", got)
	}
	if !strings.Contains(buf.String(), "specialuse") || !strings.Contains(buf.String(), "alice@example.com") {
		t.Errorf("the fallback was not reported: %q", buf.String())
	}
}
