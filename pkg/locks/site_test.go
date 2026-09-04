package locks

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// siteTestServer runs the real service: the compatibility row is about the wire,
// and a backend call cannot make a four-field frame.
func siteTestServer(t *testing.T) func() *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl-site")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l.sock")
	backend := NewMemoryBackend()
	srv := NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
	ln, err := ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, derr := net.DialTimeout("unix", sock, 50*time.Millisecond); derr == nil {
			_ = c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		srv.Close()
		cancel()
		_ = backend.Close()
		<-done
	})
	return func() *Client {
		c, cerr := NewClient(context.Background(), DialUnix(sock))
		if cerr != nil {
			t.Fatal(cerr)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
}

func siteTestBackend(t *testing.T) *MemoryBackend {
	t.Helper()
	b := NewMemoryBackend()
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// A refusal names what the holder was doing: the site lived only as a counter
// label, so three questions in a row were answered by inference (#1676).
func TestARefusalNamesTheHoldersSite(t *testing.T) {
	b := siteTestBackend(t)
	ctx := context.Background()
	if _, _, err := b.Acquire(ctx, "mbox:u:INBOX", "p/1/u/s1", "write", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, current, err := b.Acquire(ctx, "mbox:u:INBOX", "p/2/u/s2", "read", time.Minute)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquisition returned %v, want ErrBusy", err)
	}
	if current.Site != "write" {
		t.Errorf("the refusal says the holder's site is %q, want \"write\": a waiter still cannot "+
			"tell what blocked it", current.Site)
	}
	if current.Owner != "p/1/u/s1" {
		t.Errorf("holder owner = %q", current.Owner)
	}
}

// Each refusal names its own holder's site: one holder is not enough, a
// hard-coded field passes that, and would have.
func TestTwoHoldersInTurnEachNameTheirOwnSite(t *testing.T) {
	b := siteTestBackend(t)
	ctx := context.Background()

	for _, want := range []string{"write", "open-probe"} {
		id, _, err := b.Acquire(ctx, "mbox:u:INBOX", "p/1/u/"+want, want, time.Minute)
		if err != nil {
			t.Fatalf("holder %q: %v", want, err)
		}
		_, current, err := b.Acquire(ctx, "mbox:u:INBOX", "p/2/u/waiter", "read", time.Minute)
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("waiter against holder %q returned %v, want ErrBusy", want, err)
		}
		if current.Site != want {
			t.Errorf("with a %q holder the refusal says %q", want, current.Site)
		}
		if err := b.Release(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
}

// A shared holder's site reaches the refused writer too.
func TestASharedHoldersSiteIsReported(t *testing.T) {
	b := siteTestBackend(t)
	ctx := context.Background()
	if _, _, err := b.AcquireShared(ctx, "mbox:u:INBOX", "p/1/u/r1", "read", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, current, err := b.Acquire(ctx, "mbox:u:INBOX", "p/2/u/w", "write", time.Minute)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("writer returned %v, want ErrBusy", err)
	}
	if current.Site != "read" {
		t.Errorf("a writer refused by a reader is told the site is %q, want \"read\"", current.Site)
	}
}

// The old request form still gets a lock: the service is deployed before the
// clients, so a four-field LOCK is answered, never refused (#1676).
func TestTheOldRequestFormStillGetsALock(t *testing.T) {
	dial := siteTestServer(t)
	c := dial()
	// Four fields: the pre-#1676 spelling, written by hand because no client
	// can produce it any more.
	resp, err := c.roundtrip(context.Background(), cmdLock, "mbox:u:INBOX", "p/1/u/old", "60000")
	if err != nil {
		t.Fatalf("old-form lock: %v", err)
	}
	if resp[0] != respOK {
		t.Fatalf("old-form lock answered %v, want OK: an older client is refused during a rollout", resp)
	}
	// And what it holds is reported as unknown, not as an empty field.
	blocked := dial()
	bctx := WithSite(context.Background(), "write")
	lk, err := blocked.Lock(bctx, "mbox:u:INBOX", "p/2/u/new", time.Minute)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second client got %v, want ErrBusy", err)
	}
	if lk.Site != SiteUnknown {
		t.Errorf("a holder that sent no site is reported as %q, want %q", lk.Site, SiteUnknown)
	}
}

// A context with no site does not reach the service: the server's unknown is for
// an older client's frame, ours setting none is a defect (#1670).
func TestAnAcquisitionWithNoSitePanicsUnderTest(t *testing.T) {
	dial := siteTestServer(t)
	c := dial()
	defer func() {
		if recover() == nil {
			t.Error("a bare context reached the lock service: a new call site would announce " +
				"unknown and no test would redden")
		}
	}()
	_, _ = c.Lock(context.Background(), "mbox:u:INBOX", "p/1/u/s", time.Minute)
}
