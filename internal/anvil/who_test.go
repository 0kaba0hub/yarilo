package anvil_test

import (
	"context"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/anvil"
)

func startServerWithRef(t *testing.T, max int) (*anvil.Server, string, context.CancelFunc) {
	t.Helper()
	srv := anvil.NewServer(max)
	ctx, cancel := context.WithCancel(context.Background())
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(30 * time.Millisecond)
	return srv, addr, cancel
}

func TestServerSessionsRegisterAndRelease(t *testing.T) {
	srv, addr, cancel := startServerWithRef(t, 5)
	defer cancel()

	c, err := anvil.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := c.Connect("s1", "alice@example.com", "1.1.1.1", "imap"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := len(srv.Sessions()); got != 1 {
		t.Fatalf("sessions=%d want 1", got)
	}
	if err := c.Disconnect("s1", "alice@example.com", "1.1.1.1", "imap"); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := len(srv.Sessions()); got != 0 {
		t.Fatalf("sessions=%d want 0", got)
	}
}

func TestClientWhoReturnsActiveSessions(t *testing.T) {
	_, addr, cancel := startServerWithRef(t, 5)
	defer cancel()

	// Two connect-only clients (no disconnect) so WHO sees them.
	c1, err := anvil.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("dial1: %v", err)
	}
	defer c1.Close()
	if err := c1.Connect("s1", "alice@example.com", "1.1.1.1", "imap"); err != nil {
		t.Fatalf("connect1: %v", err)
	}
	c2, err := anvil.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	defer c2.Close()
	if err := c2.Connect("s2", "bob@example.com", "2.2.2.2", "pop3"); err != nil {
		t.Fatalf("connect2: %v", err)
	}

	// Issue WHO via a third connection so we don't interleave reads
	// on c1/c2 (whose readers are sync with their own Connect/Disconnect).
	who, err := anvil.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("dial who: %v", err)
	}
	defer who.Close()

	all, err := who.Who(anvil.WhoFilter{})
	if err != nil {
		t.Fatalf("who all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("who all returned %d sessions, want 2: %+v", len(all), all)
	}

	imapOnly, err := who.Who(anvil.WhoFilter{Service: "imap"})
	if err != nil {
		t.Fatalf("who imap: %v", err)
	}
	if len(imapOnly) != 1 || imapOnly[0].User != "alice@example.com" {
		t.Fatalf("who imap returned %+v, want only alice@example.com", imapOnly)
	}

	byUser, err := who.Who(anvil.WhoFilter{User: "bob@example.com"})
	if err != nil {
		t.Fatalf("who user: %v", err)
	}
	if len(byUser) != 1 || byUser[0].Service != "pop3" {
		t.Fatalf("who user returned %+v, want only pop3", byUser)
	}
}
