package client

import (
	"bufio"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// flakyAuth is a fake auth server whose availability can be toggled. While up
// it completes the handshake and answers OK; while down it drops connections
// during the handshake and closes any live ones.
type flakyAuth struct {
	addr string

	mu    sync.Mutex
	up    bool
	conns map[net.Conn]struct{}
}

func newFlakyAuth(t *testing.T) *flakyAuth {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	fa := &flakyAuth{addr: ln.Addr().String(), up: true, conns: map[net.Conn]struct{}{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go fa.handle(c)
		}
	}()
	return fa
}

func (fa *flakyAuth) setUp() { fa.mu.Lock(); fa.up = true; fa.mu.Unlock() }
func (fa *flakyAuth) setDown() {
	fa.mu.Lock()
	fa.up = false
	for c := range fa.conns {
		_ = c.Close()
	}
	fa.conns = map[net.Conn]struct{}{}
	fa.mu.Unlock()
}

func (fa *flakyAuth) handle(c net.Conn) {
	defer c.Close()
	fa.mu.Lock()
	up := fa.up
	if up {
		fa.conns[c] = struct{}{}
	}
	fa.mu.Unlock()
	if !up {
		return // drop during handshake — dial() fails, redial retries
	}
	rd := bufio.NewReader(c)
	fmt.Fprint(c, "VERSION\t1\t0\nSPID\t1\nDONE\n")
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
		if len(fields) < 2 || fields[0] == "VERSION" {
			continue
		}
		fmt.Fprintf(c, "OK\t%s\tuser=alice\n", fields[1])
	}
}

// A long auth outage must not leave the client stuck in reconnecting with
// nothing to revive it: requests during the outage fail with ErrTimeout, and
// once auth returns the redial loop recovers on its own (#932).
func TestRedialRecoversFromAnOutageLongerThanTheBudget(t *testing.T) {
	fa := newFlakyAuth(t)

	c, err := New(fa.addr, nil, Options{
		RequestTimeout: 400 * time.Millisecond,
		DialTimeout:    400 * time.Millisecond,
		WriteTimeout:   200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	baseline := runtime.NumGoroutine()

	// Drop the live connection and refuse handshakes; the reader triggers the
	// reconnect and redial loops.
	fa.setDown()

	// Requests during the outage must fail, not hang or succeed.
	outageDeadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(outageDeadline) {
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, _, e := c.Verify("t", "u", "s"); e == nil {
					t.Errorf("Verify unexpectedly succeeded during the outage")
				}
			}()
		}
		wg.Wait()
	}

	// Auth returns; the redial loop must recover without any request write
	// triggering it.
	fa.setUp()
	recovered := false
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		if _, _, _, e := c.Verify("t", "u", "s"); e == nil {
			recovered = true
			break
		}
	}
	if !recovered {
		t.Fatal("client never recovered after auth returned — redial gave up (zombie)")
	}

	// No busy-spin pileup: goroutine count settles back near the baseline.
	time.Sleep(200 * time.Millisecond)
	if got := runtime.NumGoroutine(); got > baseline+10 {
		t.Fatalf("goroutine pileup: %d goroutines, baseline was %d", got, baseline)
	}
}
