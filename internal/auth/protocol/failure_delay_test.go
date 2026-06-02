package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// errPassdb returns a TempFail with a non-nil error — exercises
// the internal-failure-delay branch in handleAuth.
type errPassdb struct{ err error }

func (p *errPassdb) Authenticate(req *Request) (Result, error) {
	return ResultTempFail, p.err
}

// TestWire_FailureDelay_HoldsAuthFail — wrong password held back
// by configured FailureDelay before the FAIL reply lands.
func TestWire_FailureDelay_HoldsAuthFail(t *testing.T) {
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "secret"}},
		WithFailureDelay(150*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	t0 := time.Now()
	fmt.Fprintf(conn, "AUTH\t60\tPLAIN\tservice=imap\tresp=\x00alice\x00wrong\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	elapsed := time.Since(t0)
	if !strings.HasPrefix(sc.Text(), "FAIL\t60") {
		t.Fatalf("unexpected reply: %q", sc.Text())
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("FAIL replied in %v, want ≥150ms", elapsed)
	}
}

// TestWire_FailureDelay_OK_NotDelayed — successful auth must not
// pay the failure-delay penalty.
func TestWire_FailureDelay_OK_NotDelayed(t *testing.T) {
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "secret"}},
		WithFailureDelay(500*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	t0 := time.Now()
	fmt.Fprintf(conn, "AUTH\t61\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	elapsed := time.Since(t0)
	if !strings.HasPrefix(sc.Text(), "OK\t61") {
		t.Fatalf("unexpected reply: %q", sc.Text())
	}
	if elapsed >= 500*time.Millisecond {
		t.Errorf("OK paid failure delay (%v); should be fast", elapsed)
	}
}

// TestWire_InternalFailureDelay_AppliesOnTempFail — backend
// errors use a separate (typically shorter) delay knob.
func TestWire_InternalFailureDelay_AppliesOnTempFail(t *testing.T) {
	srv := NewServer(
		[]Passdb{&errPassdb{err: errors.New("sql down")}},
		WithFailureDelay(2*time.Second),                // user-facing delay
		WithInternalFailureDelay(120*time.Millisecond), // shorter internal delay
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	t0 := time.Now()
	fmt.Fprintf(conn, "AUTH\t62\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	elapsed := time.Since(t0)
	if !strings.HasPrefix(sc.Text(), "FAIL\t62\ttemp_fail") {
		t.Fatalf("unexpected reply: %q", sc.Text())
	}
	if elapsed < 120*time.Millisecond {
		t.Errorf("internal-fail replied in %v, want ≥120ms", elapsed)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("internal-fail used user-facing delay (%v); should be ~120ms", elapsed)
	}
}

// TestWire_FailureDelay_Zero_NoSleep — explicit disable for the
// test-fast path.
func TestWire_FailureDelay_Zero_NoSleep(t *testing.T) {
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "secret"}},
		// No WithFailureDelay — defaults to 0.
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	t0 := time.Now()
	fmt.Fprintf(conn, "AUTH\t63\tPLAIN\tservice=imap\tresp=\x00alice\x00wrong\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	elapsed := time.Since(t0)
	if !strings.HasPrefix(sc.Text(), "FAIL\t63") {
		t.Fatalf("unexpected reply: %q", sc.Text())
	}
	if elapsed >= 100*time.Millisecond {
		t.Errorf("zero delay still slept (%v)", elapsed)
	}
}

// TestWire_FailureDelay_HandshakeNotDelayed — the VERSION/MECH/
// DONE handshake must arrive immediately on connect; the delay
// only applies to the FAIL line itself.
func TestWire_FailureDelay_HandshakeNotDelayed(t *testing.T) {
	srv := NewServer(
		nil,
		WithFailureDelay(500*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	t0 := time.Now()
	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()
	handshakeElapsed := time.Since(t0)

	if handshakeElapsed >= 500*time.Millisecond {
		t.Errorf("handshake delayed by FailureDelay (%v); should be fast", handshakeElapsed)
	}
	_ = sc
}
