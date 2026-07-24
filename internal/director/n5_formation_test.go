package director

import (
	"context"
	"io"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// n5Cluster spins up n in-process directors behind a mock load-balanced
// seed (each seed connection is proxied to a RANDOM member, exactly like a
// ClusterIP behind kube-proxy — harder than the headless fan-out), starts
// them simultaneously, and returns the servers plus a kill(addr) that
// removes a member from the seed's routing set and force-closes all of its
// connections (a real pod-kill, not just a listener close). Used by the
// N=5 regression guards (#768 comment: the reported N=5 formation/kill
// failures were measurement artifacts; these lock in that the ring really
// does converge at N>3).
func n5Cluster(t *testing.T, n int) (srvs []*Server, addrsOf []string, kill func(addr string)) {
	t.Helper()

	var (
		mu       sync.Mutex
		addrs    []string
		cancelOf = map[string]context.CancelFunc{} // addr -> ctx cancel (stops listener/dials)
		lnOf     = map[string]net.Listener{}       // addr -> its listener, for a real kill
	)

	seedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	t.Cleanup(func() { seedLn.Close() })
	go func() {
		for {
			c, aErr := seedLn.Accept()
			if aErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				mu.Lock()
				var target string
				if len(addrs) > 0 {
					target = addrs[rand.IntN(len(addrs))]
				}
				mu.Unlock()
				if target == "" {
					return
				}
				b, dErr := net.Dial("tcp", target)
				if dErr != nil {
					return
				}
				defer b.Close()
				go func() { _, _ = io.Copy(b, c) }()
				_, _ = io.Copy(c, b)
			}(c)
		}
	}()
	seedAddr := seedLn.Addr().String()

	starts := make([]func(), 0, n)
	for i := 0; i < n; i++ {
		srv := NewWithOptions(Options{
			PingInterval:        24 * time.Hour,
			RingSecret:          []byte("shared-secret"),
			MinMembers:          n,
			AntiEntropyInterval: 500 * time.Millisecond,
			SeedPollInterval:    300 * time.Millisecond,
		})
		srv.membership.probeTimeout = 500 * time.Millisecond
		ln, lErr := net.Listen("tcp", "127.0.0.1:0")
		if lErr != nil {
			t.Fatalf("listen: %v", lErr)
		}
		addr := ln.Addr().String()
		host, portStr, _ := net.SplitHostPort(addr)
		port, _ := strconv.Atoi(portStr)
		srv.opts.LocalIP, srv.opts.LocalPort = host, port
		srv.membership.self = Member{IP: host, Port: port}

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			ln.Close()
		})
		go func() { _ = srv.listenOn(ctx, ln) }()

		mu.Lock()
		addrs = append(addrs, addr)
		cancelOf[addr] = cancel
		lnOf[addr] = ln
		mu.Unlock()

		theSrv := srv
		starts = append(starts, func() { theSrv.StartMembership(ctx, []string{seedAddr}) })
		srvs = append(srvs, srv)
		addrsOf = append(addrsOf, addr)
	}

	for _, start := range starts {
		start()
	}

	kill = func(addr string) {
		mu.Lock()
		kept := addrs[:0:0]
		for _, a := range addrs {
			if a != addr {
				kept = append(kept, a)
			}
		}
		addrs = kept
		cancel := cancelOf[addr]
		ln := lnOf[addr]
		mu.Unlock()
		// A real pod-kill: stop the node's accept/dial loops (cancel) and
		// close its listener, so survivors' dials to it FAIL rather than
		// being served by a still-running process — then force-close its
		// existing connections too.
		if cancel != nil {
			cancel()
		}
		if ln != nil {
			ln.Close()
		}
		for _, srv := range srvs {
			if srv.membership.self.String() != addr {
				continue
			}
			srv.clientMu.RLock()
			for c := range srv.clients {
				c.conn.Close()
			}
			srv.clientMu.RUnlock()
			srv.membership.rightMu.Lock()
			if srv.membership.dialConn != nil {
				srv.membership.dialConn.Close()
			}
			for c := range srv.membership.ringConns {
				c.Close()
			}
			srv.membership.rightMu.Unlock()
		}
	}
	return srvs, addrsOf, kill
}

func waitAllSee(t *testing.T, srvs []*Server, want int, absent string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok := true
		for _, srv := range srvs {
			members := srv.membership.Members()
			if len(members) != want {
				ok = false
				break
			}
			if absent != "" {
				for _, m := range members {
					if m.String() == absent {
						ok = false
						break
					}
				}
			}
		}
		if ok {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestMembership_N5_FormationConverges locks in that a clean simultaneous
// N=5 start converges to exactly 5 on every node (#768 comment retraction:
// the reported "5/4/4/5/5" was a log-scraping / scale-churn artifact, not a
// propagation bug — the ring really does converge at N>3).
func TestMembership_N5_FormationConverges(t *testing.T) {
	srvs, _, _ := n5Cluster(t, 5)
	if !waitAllSee(t, srvs, 5, "", 20*time.Second) {
		views := make([]string, len(srvs))
		for i, s := range srvs {
			views[i] = formatMemberList(s.membership.Members())
		}
		t.Fatalf("N=5 formation did not converge to 5 everywhere; views=%v", views)
	}
}

// TestMembership_N5_KillConverges kills one member of a converged 5-ring and
// requires every survivor to converge to exactly 4 with the dead member
// gone — no lingering non-neighbor phantom (the other reported-then-retracted
// defect).
func TestMembership_N5_KillConverges(t *testing.T) {
	srvs, addrs, kill := n5Cluster(t, 5)
	if !waitAllSee(t, srvs, 5, "", 20*time.Second) {
		t.Fatal("N=5 did not form before kill")
	}

	dead := addrs[2]
	survivors := make([]*Server, 0, 4)
	for _, s := range srvs {
		if s.membership.self.String() != dead {
			survivors = append(survivors, s)
		}
	}
	kill(dead)

	if !waitAllSee(t, survivors, 4, dead, 25*time.Second) {
		views := make([]string, len(survivors))
		for i, s := range survivors {
			views[i] = formatMemberList(s.membership.Members())
		}
		t.Fatalf("survivors did not converge to 4 without %s; views=%v", dead, views)
	}
}
