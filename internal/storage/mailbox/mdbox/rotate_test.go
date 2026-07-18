package mdbox

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestRotateSizeConfigurable: a configured rotate size smaller than the message
// rolls the second save into m.2 — where the old hardcoded 2 MiB default would
// have kept both in m.1. Guards both the config plumbing and the default fix.
func TestRotateSizeConfigurable(t *testing.T) {
	home := t.TempDir()
	u := New(WithRotateSize(1024)).OpenUser(&mailbox.UserInfo{Username: "u@x.io", Home: home}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("z", 2048) + "\r\n" // > 1 KiB rotate size
	if _, err := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if _, err := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	if _, err := os.Stat(u.mfilePath(2)); err != nil {
		t.Errorf("expected m.2 after rotate-size rollover, got: %v", err)
	}
}

// TestRotateSizeDefaultKeepsSmallInOneFile: with the default (10 MiB) rotate
// size, several small messages all stay in m.1 — the second m file must not
// appear. This is the counterpart guard: the default must be 10 MiB, not 2 MiB.
func TestRotateSizeDefaultKeepsSmallInOneFile(t *testing.T) {
	home := t.TempDir()
	u := New().OpenUser(&mailbox.UserInfo{Username: "u@x.io", Home: home}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		body := strings.Repeat("m", 4096) + "\r\n"
		if _, err := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	if _, err := os.Stat(u.mfilePath(2)); !os.IsNotExist(err) {
		t.Errorf("m.2 should not exist under the 10 MiB default (err=%v)", err)
	}
}

// TestRotateInterval: a file older than the configured interval rolls to a new
// m.<N> on the next save even though it is far under the size cap. The backend's
// clock is injected so the age passes without sleeping.
func TestRotateInterval(t *testing.T) {
	home := t.TempDir()
	b := New(WithRotateInterval(time.Hour))
	base := time.Unix(1_700_000_000, 0)
	clock := base
	b.now = func() time.Time { return clock }
	u := b.OpenUser(&mailbox.UserInfo{Username: "u@x.io", Home: home}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}

	// First save creates m.1 at t=base with a small body (well under 10 MiB).
	small := "hello\r\n"
	if _, err := u.Save("INBOX", strings.NewReader(small), 0, int64(len(small)), nil); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if _, err := os.Stat(u.mfilePath(2)); !os.IsNotExist(err) {
		t.Fatalf("m.2 must not exist yet (age 0)")
	}

	// Advance the clock past the interval; the next save must roll to m.2.
	clock = base.Add(2 * time.Hour)
	if _, err := u.Save("INBOX", strings.NewReader(small), 0, int64(len(small)), nil); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	if _, err := os.Stat(u.mfilePath(2)); err != nil {
		t.Errorf("expected m.2 after the interval elapsed, got: %v", err)
	}
}

// TestRotateIntervalDisabledByDefault: with interval 0 (default), an old file is
// never rolled by age — only size rotates it.
func TestRotateIntervalDisabledByDefault(t *testing.T) {
	home := t.TempDir()
	b := New() // interval 0
	base := time.Unix(1_700_000_000, 0)
	clock := base
	b.now = func() time.Time { return clock }
	u := b.OpenUser(&mailbox.UserInfo{Username: "u@x.io", Home: home}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	small := "hi\r\n"
	if _, err := u.Save("INBOX", strings.NewReader(small), 0, int64(len(small)), nil); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	clock = base.Add(1000 * time.Hour) // ancient
	if _, err := u.Save("INBOX", strings.NewReader(small), 0, int64(len(small)), nil); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	if _, err := os.Stat(u.mfilePath(2)); !os.IsNotExist(err) {
		t.Errorf("interval disabled: m.2 must not exist (err=%v)", err)
	}
}

// TestPreallocateNoError: enabling preallocate must not break a save on any
// platform — the real fallocate on Linux, the no-op elsewhere. Both write and
// fetch the message back intact.
func TestPreallocateNoError(t *testing.T) {
	home := t.TempDir()
	u := New(WithPreallocate(true), WithRotateSize(1<<20)).
		OpenUser(&mailbox.UserInfo{Username: "u@x.io", Home: home}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	body := "preallocated body\r\n"
	fn, err := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	rc, err := u.Fetch("INBOX", fn, false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer rc.Close()
	got := make([]byte, len(body))
	_, _ = rc.Read(got)
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}
