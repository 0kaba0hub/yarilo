package lmtplogin

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/authclient"
)

// fakeMaster answers the master protocol and counts connections. The count is
// the claim: only the far end knows how many connections a delivery cost.
type fakeMaster struct {
	ln net.Listener

	mu    sync.Mutex
	conns int
}

func newFakeMaster(t *testing.T) *fakeMaster {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeMaster{ln: ln}
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			m.mu.Lock()
			m.conns++
			m.mu.Unlock()
			go m.serve(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return m
}

func (m *fakeMaster) serve(conn net.Conn) {
	defer conn.Close()
	fmt.Fprint(conn, "VERSION\tyarilo-auth-master\t1\t0\nDONE\n")
	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		f := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
		id := "0"
		if len(f) > 1 {
			id = f[1]
		}
		switch f[0] {
		case "SESSION":
			fmt.Fprintf(conn, "OK\t%s\ttoken=tok-1\n", id)
		case "USER":
			fmt.Fprintf(conn, "USER\t%s\thome=/srv/u\tdirector_tag=t1\n", id)
		default:
			fmt.Fprintf(conn, "FAIL\t%s\treason=unknown command\n", id)
		}
	}
}

func (m *fakeMaster) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns
}

// Delivery used to open a connection to the auth master per LMTP session, for
// the session token and the director-tag lookup (#1423). Both now travel to a
// shared connection.
//
// Sessions are driven directly rather than over SMTP: what changed is which
// connection the master calls use, and a delivery would add a second fake
// service (warden) and a backend to prove the same thing.
func TestDeliveriesShareTheMasterConnection(t *testing.T) {
	m := newFakeMaster(t)
	pool := authclient.NewPool(m.ln.Addr().String(), nil, 2, time.Minute)
	defer pool.Close() //nolint:errcheck

	opts := Options{AuthMasterAddr: m.ln.Addr().String(), AuthMasterPool: pool}
	for i := 0; i < 4; i++ {
		s := &session{opts: opts, peerIP: "10.0.0.1"}
		if _, err := s.issueToken("u@example.com", "warden-1"); err != nil {
			t.Fatalf("session %d token: %v", i, err)
		}
		if tag := s.resolveDirectorTag("u@example.com"); tag != "t1" {
			t.Fatalf("session %d tag = %q, want t1", i, tag)
		}
	}
	if got := m.count(); got > 2 {
		t.Errorf("four deliveries opened %d connections, want at most the pool size (2)", got)
	}

	// And without a pool the old shape stays available: a session dials for
	// itself, which is what a standalone or test wiring gets.
	before := m.count()
	plain := &session{opts: Options{AuthMasterAddr: m.ln.Addr().String()}, peerIP: "10.0.0.1"}
	if _, err := plain.issueToken("u@example.com", "warden-1"); err != nil {
		t.Fatalf("unpooled token: %v", err)
	}
	if got := m.count() - before; got != 1 {
		t.Errorf("an unpooled session opened %d connections, want 1", got)
	}
}
