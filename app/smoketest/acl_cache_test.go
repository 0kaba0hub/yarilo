package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// cachingACLServer models the one server behaviour the reconnect fix turns on:
// a session's view of an identifier's rights is fixed at the moment it
// connects, the way acl_cache_ttl holds a cached answer. SETACL updates the
// shared grant; a session sees it only if it connected afterward.
//
// It is the smallest server that can tell "reconnected after the grant" from
// "reused the pre-grant session" -- which is exactly the difference #1121 turns
// on, and which a server that answered OK to everything could not express.
type cachingACLServer struct {
	mu      sync.Mutex
	granted string // rights the owner has set so far, e.g. "lra"
	dials   int
}

func (s *cachingACLServer) accept(t *testing.T) *imapClient {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	t.Cleanup(func() { clientSide.Close(); serverSide.Close() })

	s.mu.Lock()
	view := s.granted // snapshot at connect time — this is the "cache"
	s.dials++
	s.mu.Unlock()

	go func() {
		r := bufio.NewReader(serverSide)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			tag, rest, _ := strings.Cut(line, " ")
			verb, args, _ := strings.Cut(rest, " ")

			switch strings.ToUpper(verb) {
			case "LOGIN":
				fmt.Fprintf(serverSide, "%s OK\r\n", tag) //nolint:errcheck
			case "SETACL":
				// owner path: "SETACL "folder" "peer" lra" -> update shared grant.
				fields := strings.Fields(args)
				s.mu.Lock()
				s.granted = strings.Trim(fields[len(fields)-1], "\"")
				s.mu.Unlock()
				fmt.Fprintf(serverSide, "%s OK\r\n", tag) //nolint:errcheck
			case "SELECT":
				if strings.Contains(view, "r") {
					fmt.Fprintf(serverSide, "%s OK [READ-WRITE]\r\n", tag) //nolint:errcheck
				} else if strings.Contains(view, "l") {
					fmt.Fprintf(serverSide, "%s NO [NOPERM] missing right 'r'\r\n", tag) //nolint:errcheck
				} else {
					fmt.Fprintf(serverSide, "%s NO [NONEXISTENT] No such mailbox\r\n", tag) //nolint:errcheck
				}
			case "MYRIGHTS":
				fmt.Fprintf(serverSide, "* MYRIGHTS folder %s\r\n%s OK\r\n", view, tag) //nolint:errcheck
			case "GETACL":
				if strings.Contains(view, "a") {
					fmt.Fprintf(serverSide, "* ACL folder peer %s\r\n%s OK\r\n", view, tag) //nolint:errcheck
				} else {
					fmt.Fprintf(serverSide, "%s NO [NOPERM] missing right 'a'\r\n", tag) //nolint:errcheck
				}
			default:
				fmt.Fprintf(serverSide, "%s OK\r\n", tag) //nolint:errcheck
			}
		}
	}()
	return &imapClient{conn: clientSide, r: bufio.NewReader(clientSide)}
}

// The grant ladder passes only because the peer reconnects after each SETACL:
// against a cache that fixes a session's view at connect time, the pre-grant
// session would keep answering NONEXISTENT and the very first probe would read
// as "a grant that changes nothing" (#1121).
func TestGrantsAreReachableReconnectsPastTheCache(t *testing.T) {
	srv := &cachingACLServer{}
	owner := srv.accept(t)
	peer := srv.accept(t)
	dialPeer := func() (*imapClient, error) { return srv.accept(t), nil }

	if err := aclGrantsAreReachable(owner, &peer, "Public/Probe", "peer@test", dialPeer); err != nil {
		t.Fatalf("ladder failed against a correct (caching) server: %v", err)
	}
	// One dial per grant (l, lr, lra) on top of owner + initial peer.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.dials != 5 {
		t.Errorf("dials = %d, want 5 (owner + initial peer + one reconnect per grant)", srv.dials)
	}
}
