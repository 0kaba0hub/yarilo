package login

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
)

// A master login in the separator form types "target*master" and authenticates
// as the TARGET: the auth service answers user=target and issues the token for
// it. Everything after authentication therefore belongs to the resolved
// identity — the backend's VERIFY compares against it, the director routes by
// it, the connection limit counts it — and forwarding the raw login string
// refused a session that had authenticated correctly (#1306).
//
// The resolution is one expression, so it is asserted as one: what the session
// acts as, given what the client sent and what the service resolved.
func TestSessionActsAsTheResolvedIdentity(t *testing.T) {
	tests := []struct {
		name     string
		claimed  string // what the client typed
		resolved string // what the auth service answered
		want     string
	}{
		{
			name:     "separator form resolves to the target",
			claimed:  "u1@d00001.test*admin-master",
			resolved: "u1@d00001.test",
			want:     "u1@d00001.test",
		},
		{
			name:     "authzid form resolves to the target",
			claimed:  "admin-master",
			resolved: "u1@d00001.test",
			want:     "u1@d00001.test",
		},
		{
			name:     "an ordinary login resolves to itself",
			claimed:  "u1@d00001.test",
			resolved: "u1@d00001.test",
			want:     "u1@d00001.test",
		},
		{
			// A service that answers OK without naming a user leaves the login
			// string as the only identity there is; falling back to it keeps a
			// deployment on an older auth service working rather than sending
			// an empty name to the backend.
			name:     "no resolved name falls back to what was claimed",
			claimed:  "u1@d00001.test",
			resolved: "",
			want:     "u1@d00001.test",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvedIdentity(&authclient.AuthResult{Username: tc.resolved}, tc.claimed); got != tc.want {
				t.Errorf("session acts as %q, want %q", got, tc.want)
			}
		})
	}
}

// The wiring, not the expression: the backend receives the RESOLVED identity in
// the preamble it verifies against the token. The pure function above passes
// even when nothing downstream uses it — this row is what fails then, and it is
// the shape #1306 actually had (authentication succeeded, the session was
// refused).
func TestBackendPreambleCarriesTheResolvedIdentity(t *testing.T) {
	claimed := make(chan string, 1)
	backend := startPreambleCapturingBackend(t, claimed)

	s := &Server{
		opts: Options{
			Protocol:    ProtocolIMAP,
			AuthAddr:    startOKAuth(t), // answers user=alice, whatever was sent
			BackendAddr: backend,
		},
		sessions: make(map[string][]*liveSession),
	}

	srv, cli := pipePair(t)
	go s.handleConn(srv)
	crd := bufio.NewReader(cli)
	crd.ReadString('\n') //nolint:errcheck // greeting

	// The separator form: the client types target*master, the service resolves
	// the target.
	cli.Write([]byte("a1 LOGIN alice*admin-master secret\r\n")) //nolint:errcheck

	select {
	case line := <-claimed:
		if !strings.Contains(line, "USER=alice") {
			t.Errorf("backend preamble does not claim the resolved identity:\n  %q", line)
		}
		if strings.Contains(line, "admin-master") {
			t.Errorf("the master's name reached the backend as the session identity:\n  %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no preamble reached the backend")
	}
}

// startPreambleCapturingBackend accepts one connection, hands the preamble line
// to claimed and then goes quiet — the session's fate afterwards is not this
// row's subject.
func startPreambleCapturingBackend(t *testing.T, claimed chan<- string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close() //nolint:errcheck
				line, err := bufio.NewReader(c).ReadString('\n')
				if err == nil {
					claimed <- line
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

// The third floor of the same defect: the re-route re-LOOKUPs after a failed
// dial, and a lookup by the raw login string hashes to a different pod than the
// user's own — so a master session would land on the wrong backend at its first
// retry. The wire test above cannot see this, because the re-route only runs
// when a dial fails.
func TestRerouteLookupUsesTheResolvedIdentity(t *testing.T) {
	looked := make(chan string, 4)
	var reports int32
	dead := deadBackendAddr(t)
	dir := lookupNameCapturingDirector(t, dead, looked, &reports)

	s := &Server{
		opts: Options{
			Protocol:     ProtocolIMAP,
			AuthAddr:     startOKAuth(t), // resolves every login to alice
			DirectorAddr: dir,
			LocalIP:      "127.0.0.1",
		},
		sessions: make(map[string][]*liveSession),
	}

	srv, cli := pipePair(t)
	go s.handleConn(srv)
	crd := bufio.NewReader(cli)
	crd.ReadString('\n')                                        //nolint:errcheck // greeting
	cli.Write([]byte("a1 LOGIN alice*admin-master secret\r\n")) //nolint:errcheck

	deadline := time.After(5 * time.Second)
	seen := 0
	for seen < 2 { // the first lookup and at least one re-lookup
		select {
		case name := <-looked:
			seen++
			if name != "alice" {
				t.Fatalf("director lookup %d asked for %q, want the resolved identity", seen, name)
			}
		case <-deadline:
			if seen == 0 {
				t.Fatal("no director lookup happened")
			}
			t.Fatalf("only %d lookup(s) observed; the re-route never re-looked-up", seen)
		}
	}
}

// deadBackendAddr is an address nothing listens on, so every dial fails and the
// re-route path runs.
func deadBackendAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() //nolint:errcheck
	return addr
}

// lookupNameCapturingDirector answers every LOOKUP with the same (dead) backend
// and reports the username each one asked about.
func lookupNameCapturingDirector(t *testing.T, backend string, names chan<- string, reports *int32) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close() //nolint:errcheck
				rd := bufio.NewReader(conn)
				fmt.Fprintf(conn, "VERSION\tyarilo-director\t1\t0\nDONE\n")
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimRight(line, "\n") == "DONE" {
						break
					}
				}
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					fields := strings.Split(strings.TrimRight(line, "\n"), "\t")
					switch fields[0] {
					case "LOOKUP":
						if len(fields) > 2 {
							names <- fields[2]
						}
						host, port, _ := net.SplitHostPort(backend)
						fmt.Fprintf(conn, "HOST\t%s\t%s\t%s\n", fields[1], host, port)
					case "BACKEND-UNREACHABLE":
						atomic.AddInt32(reports, 1)
						fmt.Fprintf(conn, "OK\n")
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}
