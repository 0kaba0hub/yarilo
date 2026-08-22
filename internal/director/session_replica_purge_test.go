package director

import (
	"fmt"
	"testing"
)

// #1393: a session replica outlived the director run that owned it. When that
// director dies nobody is left to send its SESSION-CLOSE, and before this the
// replica was anonymous, so nothing could tell whose it was. The count fed
// least_sessions and the kill-confirm, so the phantoms made every kill for
// those users wait out its timeout.
func TestReplicasOfAnEndedRunArePurged(t *testing.T) {
	tests := []struct {
		name string
		end  func(srv *Server) // how that director's run ends
	}{
		{
			name: "it announces its exit",
			end: func(srv *Server) {
				srv.membership.applyEnvelope("DIRECTOR-REMOVE", []string{"10.0.0.7", "9102"}, "10.0.0.8@x", 2)
			},
		},
		{
			name: "it is killed and comes back as a new run",
			end: func(srv *Server) {
				// No DIRECTOR-REMOVE: a SIGKILLed director sends none. The
				// only evidence is the next event under a new incarnation.
				srv.membership.handleRingLine([]string{
					"SESSION-OPEN", "10.0.0.7@second", "9102", "1", "after", "u2@example.com", "10.0.0.5", "imap",
				}, nil)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := startServer(t)

			// Two replicas from the director that will end, one from another
			// that keeps running.
			for i, id := range []string{"a1", "a2"} {
				srv.membership.handleRingLine([]string{
					"SESSION-OPEN", "10.0.0.7@first", "9102", fmt.Sprintf("%d", i+1),
					id, "u@example.com", "10.0.0.5", "imap",
				}, nil)
			}
			srv.membership.handleRingLine([]string{
				"SESSION-OPEN", "10.0.0.8@only", "9102", "1", "b1", "v@example.com", "10.0.0.6", "imap",
			}, nil)
			if got := sessionIDs(srv); len(got) != 3 {
				t.Fatalf("setup: %v", got)
			}

			tc.end(srv)

			got := sessionIDs(srv)
			if got["a1"] || got["a2"] {
				t.Errorf("replicas of the ended run survived it: %v", got)
			}
			if !got["b1"] {
				t.Errorf("a replica of a director that is still running was purged: %v", got)
			}
		})
	}
}

// A member that keeps sending events under the SAME incarnation is simply
// working; nothing of its may be dropped.
func TestSameRunKeepsItsReplicas(t *testing.T) {
	srv, _ := startServer(t)
	srv.membership.handleRingLine([]string{
		"SESSION-OPEN", "10.0.0.7@first", "9102", "1", "a1", "u@example.com", "10.0.0.5", "imap",
	}, nil)
	srv.membership.handleRingLine([]string{
		"SESSION-OPEN", "10.0.0.7@first", "9102", "2", "a2", "u2@example.com", "10.0.0.5", "imap",
	}, nil)

	if got := sessionIDs(srv); !got["a1"] || !got["a2"] {
		t.Errorf("a running director lost its replicas: %v", got)
	}
}

// A session this director owns has a live login connection behind it and is
// closed by that connection. Ring bookkeeping must never touch it -- purging
// by address alone would take out the sessions of the pod we are serving.
func TestPurgeLeavesLocallyOwnedSessionsAlone(t *testing.T) {
	srv, addr := startServer(t)
	loginConn, loginSc := dialTest(t, addr)
	readHandshake(t, loginSc)
	sendHandshake(t, loginConn)
	loginConn.Write([]byte("SESSION-OPEN\tlocal1\tu@example.com\t10.0.0.7\n"))
	if got := readLine(t, loginSc); got != "OK" {
		t.Fatalf("SESSION-OPEN: %q", got)
	}
	// A replica from the director that is about to end, on the same backend.
	srv.membership.handleRingLine([]string{
		"SESSION-OPEN", "10.0.0.7@first", "9102", "1", "a1", "u@example.com", "10.0.0.7", "imap",
	}, nil)

	srv.membership.applyEnvelope("DIRECTOR-REMOVE", []string{"10.0.0.7", "9102"}, "10.0.0.8@x", 2)

	got := sessionIDs(srv)
	if !got["local1"] {
		t.Error("a session this director owns was purged by ring bookkeeping")
	}
	if got["a1"] {
		t.Error("the ended run's replica survived")
	}
}

func sessionIDs(srv *Server) map[string]bool {
	srv.sessRecMu.RLock()
	defer srv.sessRecMu.RUnlock()
	out := make(map[string]bool, len(srv.sessById))
	for id := range srv.sessById {
		out[id] = true
	}
	return out
}
