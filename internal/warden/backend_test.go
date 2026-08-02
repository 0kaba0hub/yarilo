package warden_test

import (
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/warden"
)

func TestBackendPushSurfacedInWho(t *testing.T) {
	_, addr, cancel := startServerWithRef(t, 5)
	defer cancel()

	c, err := warden.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := c.Connect("s1", "u@d.test", "1.1.1.1", "imap"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := c.Backend("s1", "10.0.0.7"); err != nil {
		t.Fatalf("backend push: %v", err)
	}
	// Unknown session id must not error the client (OK reason=unknown).
	if err := c.Backend("nope", "10.0.0.9"); err != nil {
		t.Fatalf("backend push unknown: %v", err)
	}

	who := dialWho(t, addr)
	defer who.Close()
	sessions, err := who.Who(warden.WhoFilter{})
	if err != nil {
		t.Fatalf("who: %v", err)
	}
	var found bool
	for _, s := range sessions {
		if s.ID == "s1" {
			found = true
			if s.Backend != "10.0.0.7" {
				t.Fatalf("backend = %q, want 10.0.0.7", s.Backend)
			}
		}
	}
	if !found {
		t.Fatal("session s1 not in WHO")
	}
}

func dialWho(t *testing.T, addr string) *warden.Conn {
	t.Helper()
	c, err := warden.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("dial who: %v", err)
	}
	return c
}
