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

// ---- #773: CIDR join filter ------------------------------------------------

// TestMembership_Join_CIDRFilter_RejectsOutsideNet verifies join_allowed_nets
// blocks a JOIN whose source IP falls outside the allow-list BEFORE the HMAC
// challenge (the joiner has the correct secret, so only the CIDR gate can be
// what stops it), and that a list which does contain the source still admits.
func TestMembership_Join_CIDRFilter_RejectsOutsideNet(t *testing.T) {
	tests := []struct {
		name     string
		cidr     string // acceptor's single allowed net
		wantJoin bool
	}{
		{name: "outside-net-rejected", cidr: "10.0.0.0/8", wantJoin: false}, // loopback joiner is 127.0.0.1
		{name: "inside-net-allowed", cidr: "127.0.0.0/8", wantJoin: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srvA, addrA := startRingNode(t, "shared-secret", nil, 3)
			_, allowed, err := net.ParseCIDR(tt.cidr)
			if err != nil {
				t.Fatalf("parse cidr: %v", err)
			}
			// A has no seeds and no incoming joins until B starts below, so
			// setting the filter here (post-Start) races with nothing.
			srvA.membership.joinAllowedNets = []*net.IPNet{allowed}

			srvB, _ := startRingNode(t, "shared-secret", []string{addrA}, 3)

			if tt.wantJoin {
				waitFor(t, 3*time.Second, func() bool {
					return len(srvB.membership.Members()) == 2
				})
			} else {
				time.Sleep(500 * time.Millisecond)
				if members := srvB.membership.Members(); len(members) != 1 {
					t.Fatalf("JOIN from outside join_allowed_nets must be rejected, got members=%v", members)
				}
			}
		})
	}
}

// ---- #773: dial-back verification ------------------------------------------

// TestMembership_Join_DialBackRejectsSpoofedME reproduces the spoofed-identity
// case: a joiner that knows the ring secret (so its HMAC proof is valid) but
// claims a ME address it does not control. The proof establishes membership;
// the dial-back then fails because no live director answers at the claimed
// address, so the JOIN is rejected and the forged member never enters the ring.
func TestMembership_Join_DialBackRejectsSpoofedME(t *testing.T) {
	srvA, addrA := startRingNode(t, "shared-secret", nil, 3)
	srvB, _ := startRingNode(t, "shared-secret", nil, 3)

	// Forge B's ME to a loopback port nothing listens on. joinVia sends this
	// as the JOIN's claimed address AND computes the proof over it, so the
	// HMAC still verifies — only the dial-back can catch the lie.
	spoofed := Member{IP: "127.0.0.1", Port: closedLoopbackPort(t)}
	srvB.membership.self = spoofed

	err := srvB.membership.joinVia(context.Background(), addrA)
	if err == nil {
		t.Fatal("JOIN with a spoofed ME (no director at claimed address) must be rejected")
	}
	// The forged member must never have been admitted on the acceptor.
	for _, m := range srvA.membership.Members() {
		if m.equal(spoofed) {
			t.Fatalf("spoofed member must not enter the ring, but A sees %v", srvA.membership.Members())
		}
	}
	if len(srvA.membership.Members()) != 1 {
		t.Fatalf("A membership must be unchanged after a rejected dial-back, got %v", srvA.membership.Members())
	}
}

// closedLoopbackPort returns a loopback TCP port with nothing listening — a
// dial to it fails fast (connection refused), well within dialBackTimeout.
func closedLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // free it — subsequent dials get refused
	return port
}

// ---- #773: dial-back does not break simultaneous formation -----------------

// TestMembership_Join_DialBack_N3FormationKillReplace is the integration gate:
// with dial-back unconditionally enabled, a clean simultaneous N=3 formation
// still converges (every acceptor dials each joiner back; those probes are
// anonymous handshake-only connections that must NOT trigger a reverse join or
// cascade), a kill converges to 2, and a fresh replacement rejoining through
// the load-balanced seed converges back to 3.
func TestMembership_Join_DialBack_N3FormationKillReplace(t *testing.T) {
	c := newJoinCluster(t)
	srvs := c.start(3)

	if !waitAllSee(t, srvs, 3, "", 20*time.Second) {
		t.Fatalf("N=3 formation with dial-back did not converge to 3; views=%v", c.views())
	}

	dead := srvs[0].membership.self.String()
	c.kill(dead)
	if !waitAllSee(t, c.live(), 2, dead, 20*time.Second) {
		t.Fatalf("kill did not converge survivors to 2 (dead=%s); views=%v", dead, c.views())
	}

	c.addNode()
	if !waitAllSee(t, c.live(), 3, dead, 20*time.Second) {
		t.Fatalf("replacement did not converge back to 3; views=%v", c.views())
	}
}

// joinCluster is a compact N-director harness behind a mock load-balanced seed
// (each seed connection is proxied to a RANDOM live member, like a ClusterIP
// behind kube-proxy) that supports adding a replacement node after a kill.
type joinCluster struct {
	t        *testing.T
	seedAddr string

	mu       sync.Mutex
	addrs    []string
	cancelOf map[string]context.CancelFunc
	lnOf     map[string]net.Listener
	srvOf    map[string]*Server
}

func newJoinCluster(t *testing.T) *joinCluster {
	t.Helper()
	c := &joinCluster{
		t:        t,
		cancelOf: map[string]context.CancelFunc{},
		lnOf:     map[string]net.Listener{},
		srvOf:    map[string]*Server{},
	}
	seedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	t.Cleanup(func() { seedLn.Close() })
	go func() {
		for {
			conn, aErr := seedLn.Accept()
			if aErr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				c.mu.Lock()
				var target string
				if len(c.addrs) > 0 {
					target = c.addrs[rand.IntN(len(c.addrs))]
				}
				c.mu.Unlock()
				if target == "" {
					return
				}
				b, dErr := net.Dial("tcp", target)
				if dErr != nil {
					return
				}
				defer b.Close()
				go func() { _, _ = io.Copy(b, conn) }()
				_, _ = io.Copy(conn, b)
			}(conn)
		}
	}()
	c.seedAddr = seedLn.Addr().String()
	return c
}

// addNode spins up one more director against the seed and returns it.
func (c *joinCluster) addNode() *Server {
	c.t.Helper()
	srv := NewWithOptions(Options{
		PingInterval:        24 * time.Hour,
		RingSecret:          []byte("shared-secret"),
		MinMembers:          3,
		AntiEntropyInterval: 500 * time.Millisecond,
		SeedPollInterval:    300 * time.Millisecond,
	})
	srv.membership.probeTimeout = 500 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	srv.opts.LocalIP, srv.opts.LocalPort = host, port
	srv.membership.self = Member{IP: host, Port: port}

	ctx, cancel := context.WithCancel(context.Background())
	c.t.Cleanup(func() { cancel(); ln.Close() })
	go func() { _ = srv.listenOn(ctx, ln) }()

	c.mu.Lock()
	c.addrs = append(c.addrs, addr)
	c.cancelOf[addr] = cancel
	c.lnOf[addr] = ln
	c.srvOf[addr] = srv
	c.mu.Unlock()

	srv.StartMembership(ctx, []string{c.seedAddr})
	return srv
}

// start brings up n nodes simultaneously.
func (c *joinCluster) start(n int) []*Server {
	srvs := make([]*Server, 0, n)
	for i := 0; i < n; i++ {
		srvs = append(srvs, c.addNode())
	}
	return srvs
}

// kill performs a real pod-kill of addr: drop it from the seed routing set,
// cancel its loops, close its listener and every live connection.
func (c *joinCluster) kill(addr string) {
	c.mu.Lock()
	kept := c.addrs[:0:0]
	for _, a := range c.addrs {
		if a != addr {
			kept = append(kept, a)
		}
	}
	c.addrs = kept
	cancel := c.cancelOf[addr]
	ln := c.lnOf[addr]
	srv := c.srvOf[addr]
	delete(c.srvOf, addr)
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if ln != nil {
		ln.Close()
	}
	if srv != nil {
		srv.clientMu.RLock()
		for cl := range srv.clients {
			cl.conn.Close()
		}
		srv.clientMu.RUnlock()
		srv.membership.rightMu.Lock()
		if srv.membership.dialConn != nil {
			srv.membership.dialConn.Close()
		}
		for cn := range srv.membership.ringConns {
			cn.Close()
		}
		srv.membership.rightMu.Unlock()
	}
}

// live returns the still-running servers.
func (c *joinCluster) live() []*Server {
	c.mu.Lock()
	defer c.mu.Unlock()
	srvs := make([]*Server, 0, len(c.srvOf))
	for _, s := range c.srvOf {
		srvs = append(srvs, s)
	}
	return srvs
}

func (c *joinCluster) views() []string {
	out := []string{}
	for _, s := range c.live() {
		out = append(out, formatMemberList(s.membership.Members()))
	}
	return out
}
