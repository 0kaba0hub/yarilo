package warden_test

import (
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/warden"
)

func TestClient_ConnectOK(t *testing.T) {
	addr, cancel := startServer(t, 5)
	defer cancel()
	time.Sleep(20 * time.Millisecond)

	c, err := warden.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.Connect("1", "alice@example.com", "1.2.3.4", "imap"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func TestClient_ConnectTooMany(t *testing.T) {
	addr, cancel := startServer(t, 1)
	defer cancel()
	time.Sleep(20 * time.Millisecond)

	c, err := warden.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.Connect("1", "alice@example.com", "1.2.3.4", "imap"); err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	err = c.Connect("2", "alice@example.com", "1.2.3.4", "imap")
	if err != warden.ErrTooManyConns {
		t.Fatalf("expected ErrTooManyConns, got: %v", err)
	}
}

func TestClient_DisconnectReleasesSlot(t *testing.T) {
	addr, cancel := startServer(t, 1)
	defer cancel()
	time.Sleep(20 * time.Millisecond)

	c, err := warden.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.Connect("1", "alice@example.com", "1.2.3.4", "imap"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// Disconnect the id that was connected (release is by session id).
	if err := c.Disconnect("1", "alice@example.com", "1.2.3.4", "imap"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	// Slot released — second connect must succeed.
	if err := c.Connect("3", "alice@example.com", "1.2.3.4", "imap"); err != nil {
		t.Fatalf("Connect after disconnect: %v", err)
	}
}

func TestClient_DialUnreachable(t *testing.T) {
	_, err := warden.Dial("127.0.0.1:1", nil, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error dialling unreachable addr")
	}
}
