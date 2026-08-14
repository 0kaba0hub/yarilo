package client

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// The impersonation target travels in the SASL PLAIN response, which is where
// the auth service reads it. This client used to rebuild that response with an
// empty authzid, so every master login left the pod as an ordinary login of the
// master and was refused — the chain was never asked (#1305).
//
// The request is asserted on the wire, because that is the layer that lost it:
// a test of the chain (which had them, and passed) proved nothing about what
// the client sent.
func TestAuthenticateAsCarriesTheTargetOnTheWire(t *testing.T) {
	tests := []struct {
		name       string
		authzid    string
		authcid    string
		wantResp   string
		wantUserIs string
	}{
		{
			name:       "impersonation carries both identities",
			authzid:    "u1@d00001.test",
			authcid:    "admin-master",
			wantResp:   "u1@d00001.test\x00admin-master\x00secret",
			wantUserIs: "admin-master",
		},
		{
			// An ordinary login is the same request with no target, so the
			// service cannot tell it from what the old client always sent.
			name:       "an ordinary login sends no target",
			authzid:    "",
			authcid:    "u1@d00001.test",
			wantResp:   "\x00u1@d00001.test\x00secret",
			wantUserIs: "u1@d00001.test",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan string, 1)
			addr := serveOneAuthLine(t, got)

			c, err := New(addr, nil, Options{})
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			defer c.Close() //nolint:errcheck

			// The reply is what the fake server wrote; only the request matters
			// here, so an error from parsing it is not the subject.
			_, _ = c.AuthenticateAs(tc.authzid, tc.authcid, "secret", "imap", "10.0.0.1", "sess1")

			select {
			case line := <-got:
				if !strings.Contains(line, "\tresp="+tc.wantResp) {
					t.Errorf("wire request\n  %q\ndoes not carry resp=%q", line, tc.wantResp)
				}
				if !strings.Contains(line, "\tuser="+tc.wantUserIs) {
					t.Errorf("wire request names user=%q, want %q", line, tc.wantUserIs)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no request reached the server")
			}
		})
	}
}

// serveOneAuthLine listens on a unix socket, hands the first AUTH line to got
// and answers FAIL, which is enough: the assertion is about the request.
func serveOneAuthLine(t *testing.T, got chan<- string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		r := bufio.NewReader(conn)
		// The client will not send AUTH until the VERSION handshake completes.
		_, _ = conn.Write([]byte("VERSION\t1\t0\nDONE\n"))
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if !strings.HasPrefix(line, "AUTH\t") {
				continue // handshake and anything else the client sends first
			}
			got <- line
			id := "1"
			if f := strings.Split(line, "\t"); len(f) > 1 {
				id = f[1]
			}
			_, _ = conn.Write([]byte("FAIL\t" + id + "\n"))
			return
		}
	}()
	return ln.Addr().String()
}
