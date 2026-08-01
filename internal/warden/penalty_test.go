package warden

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestPenaltyToSecs(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{-1, 0},
		{0, 0},
		{1, 2},
		{2, 4},
		{3, 8},
		{4, 15},
		{5, 15},  // cap
		{99, 15}, // cap
	}
	for _, c := range cases {
		if got := PenaltyToSecs(c.in); got != c.want {
			t.Errorf("PenaltyToSecs(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestServer_PenaltyLookup_DefaultZero — fresh IP returns 0.
func TestServer_PenaltyLookup_DefaultZero(t *testing.T) {
	srv := NewServer(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freePort(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	c := dialWarden(t, addr)
	defer c.Close()

	n, err := c.PenaltyLookup("203.0.113.42")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("PenaltyLookup on fresh IP = %d, want 0", n)
	}
}

// TestServer_PenaltyUpdate_RoundTrip — Update then Lookup returns
// the stored value.
func TestServer_PenaltyUpdate_RoundTrip(t *testing.T) {
	srv := NewServer(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freePort(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	c := dialWarden(t, addr)
	defer c.Close()

	if err := c.PenaltyUpdate("203.0.113.42", 3); err != nil {
		t.Fatal(err)
	}
	n, err := c.PenaltyLookup("203.0.113.42")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("PenaltyLookup = %d, want 3", n)
	}
}

// TestServer_PenaltyUpdate_ZeroClearsEntry — Update with count=0
// deletes the entry (auth-success reset semantics).
func TestServer_PenaltyUpdate_ZeroClearsEntry(t *testing.T) {
	srv := NewServer(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freePort(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	c := dialWarden(t, addr)
	defer c.Close()

	c.PenaltyUpdate("1.2.3.4", 4) //nolint:errcheck
	c.PenaltyUpdate("1.2.3.4", 0) //nolint:errcheck

	n, err := c.PenaltyLookup("1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("reset count = %d, want 0", n)
	}
}

// TestServer_PenaltyUpdate_Clamps — out-of-range values clamp to
// [0, MaxPenalty].
func TestServer_PenaltyUpdate_Clamps(t *testing.T) {
	srv := NewServer(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freePort(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	c := dialWarden(t, addr)
	defer c.Close()

	c.PenaltyUpdate("9.9.9.9", 999) //nolint:errcheck
	n, _ := c.PenaltyLookup("9.9.9.9")
	if n != MaxPenalty {
		t.Errorf("clamp high: got %d, want %d", n, MaxPenalty)
	}

	c.PenaltyUpdate("9.9.9.9", -5) //nolint:errcheck
	n, _ = c.PenaltyLookup("9.9.9.9")
	if n != 0 {
		t.Errorf("clamp low: got %d, want 0", n)
	}
}

// TestServer_PenaltyDecay — entries older than penaltyDecay are
// swept out by the background sweeper.
func TestServer_PenaltyDecay(t *testing.T) {
	srv := NewServer(0,
		WithPenaltyDecay(30*time.Millisecond),
		WithSweepInterval(15*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freePort(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	c := dialWarden(t, addr)
	defer c.Close()

	c.PenaltyUpdate("4.4.4.4", 3) //nolint:errcheck
	time.Sleep(80 * time.Millisecond)

	n, _ := c.PenaltyLookup("4.4.4.4")
	if n != 0 {
		t.Errorf("entry survived decay: got %d", n)
	}
}

// TestServer_PenaltyIPIsolation — penalty store keys by IP; one
// attacker IP doesn't affect another.
func TestServer_PenaltyIPIsolation(t *testing.T) {
	srv := NewServer(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freePort(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	c := dialWarden(t, addr)
	defer c.Close()

	c.PenaltyUpdate("1.1.1.1", 3) //nolint:errcheck
	c.PenaltyUpdate("2.2.2.2", 1) //nolint:errcheck

	n1, _ := c.PenaltyLookup("1.1.1.1")
	n2, _ := c.PenaltyLookup("2.2.2.2")
	n3, _ := c.PenaltyLookup("3.3.3.3") // never touched

	if n1 != 3 || n2 != 1 || n3 != 0 {
		t.Errorf("isolation broken: 1.1.1.1=%d 2.2.2.2=%d 3.3.3.3=%d", n1, n2, n3)
	}
}

// --- helpers ---

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func dialWarden(t *testing.T, addr string) *Conn {
	t.Helper()
	c, err := Dial(addr, nil, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}
