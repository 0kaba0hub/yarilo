package loginproto

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	masterclient "github.com/yarilomail/yarilo/pkg/authclient"
)

// countingMaster answers the master protocol and counts connections, which is
// the only place the claim can be checked: a pool is a statement about how many
// connections exist, and only the far end knows.
type countingMaster struct {
	ln    net.Listener
	mu    sync.Mutex
	conns int
}

func newCountingMaster(t *testing.T) *countingMaster {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &countingMaster{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
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

func (m *countingMaster) serve(conn net.Conn) {
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
		case "USER":
			fmt.Fprintf(conn, "USER\t%s\thome=/srv/u\tuid=1000\tgid=1000\n", id)
		case "NOOP":
			fmt.Fprintf(conn, "OK\t%s\n", id)
		default:
			fmt.Fprintf(conn, "FAIL\t%s\treason=unknown command\n", id)
		}
	}
}

func (m *countingMaster) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns
}

// Every session handshake resolves the user's storage identity, and it used to
// dial for it -- 2.6ms of connection for 0.3ms of lookup, on every session,
// which made this the loudest consumer of the master listener (#1419).
//
// The lookup is exercised rather than the whole handshake: staging that needs a
// second fake service for the VERIFY step on the client protocol, and it is not
// what this change touches. What it does touch -- how many connections N
// lookups cost -- is measured at the far end, over real TCP.
func TestSessionLookupsShareTheMasterConnection(t *testing.T) {
	m := newCountingMaster(t)

	pooled := &PreambleListener{
		MasterAddr: m.ln.Addr().String(),
		MasterPool: masterclient.NewPool(m.ln.Addr().String(), nil, 2, time.Minute),
	}
	t.Cleanup(func() { pooled.MasterPool.Close() }) //nolint:errcheck

	for i := 0; i < 5; i++ {
		if _, err := pooled.masterUserdb("u@example.com"); err != nil {
			t.Fatalf("pooled lookup %d: %v", i, err)
		}
	}
	if got := m.count(); got > 2 {
		t.Errorf("five pooled lookups opened %d connections, want at most the pool size (2)", got)
	}

	// Without a pool the old shape stays available, so a standalone or test
	// wiring that has none still works -- and pays per lookup.
	before := m.count()
	plain := &PreambleListener{MasterAddr: m.ln.Addr().String()}
	for i := 0; i < 3; i++ {
		if _, err := plain.masterUserdb("u@example.com"); err != nil {
			t.Fatalf("unpooled lookup %d: %v", i, err)
		}
	}
	if got := m.count() - before; got != 3 {
		t.Errorf("three unpooled lookups opened %d connections, want 3", got)
	}
}

// A protocol added later must not quietly go back to dialling per session. The
// listener is constructed in each protocol's server file, so the rule lives
// where it can be broken: wherever MasterAddr is handed to the listener, the
// pool goes with it.
func TestEveryProtocolPassesThePoolWithTheAddress(t *testing.T) {
	root := ".."
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(body)
		if !strings.Contains(src, "PreambleListener{") {
			return nil
		}
		// Only the constructions that ask for a master lookup at all.
		if !strings.Contains(src, "MasterAddr:") {
			return nil
		}
		if !strings.Contains(src, "MasterPool:") {
			t.Errorf("%s builds a PreambleListener with MasterAddr but no MasterPool: "+
				"every session handshake would dial the master again", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
