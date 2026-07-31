package client

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// startBlackhole accepts one connection, completes the VERSION handshake so the
// client reaches stateLive, then never reads again — modelling a peer that was
// blackholed (pod killed without FIN/RST, conntrack dropped). Writes into it
// succeed until the send buffer fills, after which the next write blocks in the
// kernel forever.
func startBlackhole(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				// Complete the handshake, then read nothing, ever.
				rd := bufio.NewReader(c)
				// Read the client's VERSION line so the handshake can proceed.
				_, _ = rd.ReadString('\n')
				fmt.Fprint(c, "VERSION\t1\t0\nSPID\t1\nDONE\n")
				// Deliberately stop reading. Hold the conn open.
				select {}
			}(c)
		}
	}()
	return ln.Addr().String()
}

// TestBlackholedWriteDoesNotWedgeEveryCaller is the #926 regression: a write to a
// blackholed peer must not block forever under c.mu and pile every other caller
// up behind it. N concurrent requests against such a peer must all fail in about
// one request timeout — INDEPENDENTLY — not serialize into N * timeout (which is
// the signature of the mutex holding the wedge), and the process must not hang.
func TestBlackholedWriteDoesNotWedgeEveryCaller(t *testing.T) {
	addr := startBlackhole(t)

	c, err := New(addr, nil, Options{
		WriteTimeout:   200 * time.Millisecond,
		RequestTimeout: 1500 * time.Millisecond,
		DialTimeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// A payload far larger than any default socket send buffer, so a single
	// write cannot drain into the buffer and genuinely blocks against the
	// non-reading peer — exercising the write deadline, not just the reply wait.
	huge := strings.Repeat("x", 8<<20)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _, e := c.Verify(huge, "u", "s")
			errs[i] = e
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, e := range errs {
		if e == nil {
			t.Fatalf("caller %d unexpectedly succeeded against a blackholed peer", i)
		}
	}
	// Independence: all N ran concurrently, so the batch takes about one request
	// timeout plus reconnect slack — NOT n * RequestTimeout, which is what a
	// mutex-serialised wedge would produce.
	if max := 4 * c.opts.RequestTimeout; elapsed > max {
		t.Fatalf("batch took %v; a bounded, independent failure should be well under %v (n*timeout = %v)",
			elapsed, max, time.Duration(n)*c.opts.RequestTimeout)
	}
}
