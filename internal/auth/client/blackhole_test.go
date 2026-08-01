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

// startBlackhole completes the VERSION handshake, then never reads again:
// writes succeed until the send buffer fills, then block in the kernel.
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
				// Complete the handshake, then hold the conn open without
				// reading.
				rd := bufio.NewReader(c)
				_, _ = rd.ReadString('\n')
				fmt.Fprint(c, "VERSION\t1\t0\nSPID\t1\nDONE\n")
				select {}
			}(c)
		}
	}()
	return ln.Addr().String()
}

// A write to a blackholed peer must not block forever under c.mu: N concurrent
// requests must fail in about one request timeout, not serialize into
// N * timeout (#926).
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

	// Larger than any default socket send buffer, so the write genuinely
	// blocks and exercises the write deadline.
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
	// All N ran concurrently: the batch takes about one request timeout plus
	// reconnect slack, not n * RequestTimeout.
	if max := 4 * c.opts.RequestTimeout; elapsed > max {
		t.Fatalf("batch took %v; a bounded, independent failure should be well under %v (n*timeout = %v)",
			elapsed, max, time.Duration(n)*c.opts.RequestTimeout)
	}
}
