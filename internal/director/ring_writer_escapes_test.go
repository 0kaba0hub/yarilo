package director

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/proto"
)

// ringTap registers a fake ring connection and returns a reader for whatever
// this member broadcasts. The line is read off the wire rather than rebuilt by
// the test: a test that assembles the expected line from the same pieces the
// code uses proves only that the pieces are consistent with themselves.
func ringTap(t *testing.T, m *Membership) *bufio.Reader {
	t.Helper()
	mine, theirs := net.Pipe()
	t.Cleanup(func() { mine.Close(); theirs.Close() })
	m.rightMu.Lock()
	m.ringConns[theirs] = ringConnMeta{}
	m.rightMu.Unlock()
	_ = mine.SetReadDeadline(time.Now().Add(3 * time.Second))
	return bufio.NewReader(mine)
}

func readTapped(t *testing.T, rd *bufio.Reader) []string {
	t.Helper()
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("nothing reached the ring: %v", err)
	}
	return strings.Split(strings.TrimRight(line, "\n"), "\t")
}

// #1365 step 2: user events go out escaped, under the escaped kind. The name
// below is the one that decides it -- a TAB inside a username shifts every
// field after it, which is the corruption this arc exists to remove.
const tabbedUser = "a\tb@example.com"

func TestUserEventsGoOutEscaped(t *testing.T) {
	tests := []struct {
		name     string
		send     func(s *Server)
		wantKind string
		wantTail []string
	}{
		{
			name:     "plain kick",
			send:     func(s *Server) { s.originateUserKick(tabbedUser, "", nil) },
			wantKind: "USER-KICKED-ESC",
		},
		{
			name:     "conditional kick keeps the old backend as its own field",
			send:     func(s *Server) { s.originateUserKick(tabbedUser, "10.0.0.20", nil) },
			wantKind: "USER-KICKED-ESC",
			wantTail: []string{"10.0.0.20"},
		},
		{
			name:     "move keeps host and port as their own fields",
			send:     func(s *Server) { s.moveUser(tabbedUser, "10.0.0.30:993", nil) },
			wantKind: "USER-MOVED-ESC",
			wantTail: []string{"10.0.0.30", "993"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := startServer(t)
			rd := ringTap(t, srv.membership)

			go tc.send(srv)

			fields := readTapped(t, rd)
			// KIND origin port seq user [tail...]
			if len(fields) < 5 {
				t.Fatalf("envelope has %d fields: %q", len(fields), fields)
			}
			if fields[0] != tc.wantKind {
				t.Errorf("kind = %q, want %q", fields[0], tc.wantKind)
			}
			if got := proto.TabUnescape(fields[4]); got != tabbedUser {
				t.Errorf("username decoded to %q, want %q", got, tabbedUser)
			}
			if strings.Contains(fields[4], "\t") {
				t.Errorf("the username field still contains a raw TAB: %q", fields[4])
			}
			if got := fields[5:]; len(got) != len(tc.wantTail) {
				t.Fatalf("fields after the username = %q, want %q -- escaping shifted the envelope", got, tc.wantTail)
			}
			for i, want := range tc.wantTail {
				if fields[5+i] != want {
					t.Errorf("field %d = %q, want %q", 5+i, fields[5+i], want)
				}
			}
		})
	}
}

// The origin field carries this run's incarnation, and the address half still
// names the member (#1362).
func TestOriginCarriesTheIncarnation(t *testing.T) {
	srv, _ := startServerOpts(t, Options{LocalIP: "10.0.0.7", LocalPort: 9102, PingInterval: time.Hour})
	rd := ringTap(t, srv.membership)

	go srv.originateUserKick("u@example.com", "", nil)

	fields := readTapped(t, rd)
	addr, incarnation := splitOrigin(fields[1])
	if addr != "10.0.0.7" {
		t.Errorf("origin address = %q, want 10.0.0.7", addr)
	}
	if incarnation == "" {
		t.Error("origin carries no incarnation: a restart on this address would inherit the dead run's dedup state")
	}
	if incarnation != srv.membership.incarnation {
		t.Errorf("incarnation on the wire = %q, want this process's %q", incarnation, srv.membership.incarnation)
	}
}

// Writer and reader are separate tables; a kind that can be written and not
// read (or the reverse) is the failure that split invites.
func TestEscapedKindTablesAreInverse(t *testing.T) {
	for plain, esc := range plainToEscapedUserEvent {
		back, ok := escapedUserEvents[esc]
		if !ok {
			t.Errorf("%q is written as %q, which no reader accepts", plain, esc)
			continue
		}
		if back != plain {
			t.Errorf("%q -> %q -> %q: the tables disagree", plain, esc, back)
		}
	}
	for esc, plain := range escapedUserEvents {
		if got, ok := plainToEscapedUserEvent[plain]; !ok || got != esc {
			t.Errorf("%q is read as %q, which is never written that way", esc, plain)
		}
	}
}
