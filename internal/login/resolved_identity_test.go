package login

import (
	"bufio"
	"net"
	"strings"
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
