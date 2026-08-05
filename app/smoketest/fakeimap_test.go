package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// recordingServer is an IMAP server that answers OK to everything and keeps the
// commands it was asked to run.
//
// The point is the transcript. A cleanup is destructive or not according to
// what it *sends*, and no assertion about return values can tell the difference
// between a DELETE that was refused by the server and a DELETE that was never
// issued -- which is exactly the distinction #1070 turns on, since the server
// has refused it since 2.3.52 while the run kept asking.
type recordingServer struct {
	mu   sync.Mutex
	sent []string
}

func (s *recordingServer) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

func (s *recordingServer) sentAny(prefix string) bool {
	for _, c := range s.commands() {
		if strings.HasPrefix(strings.ToUpper(c), strings.ToUpper(prefix)) {
			return true
		}
	}
	return false
}

// newFakeClient returns a client wired to a recording server. exists is what
// SELECT reports, so a caller can drive the "message arrived" path.
func newFakeClient(t *testing.T, exists int) (*imapClient, *recordingServer) {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	srv := &recordingServer{}
	t.Cleanup(func() { clientSide.Close(); serverSide.Close() })

	go func() {
		r := bufio.NewReader(serverSide)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			tag, rest, _ := strings.Cut(line, " ")
			srv.mu.Lock()
			srv.sent = append(srv.sent, rest)
			srv.mu.Unlock()

			verb, _, _ := strings.Cut(rest, " ")
			switch strings.ToUpper(verb) {
			case "SELECT":
				fmt.Fprintf(serverSide, "* %d EXISTS\r\n", exists) //nolint:errcheck
			case "UID":
				fmt.Fprintf(serverSide, "* SEARCH 1\r\n") //nolint:errcheck
			}
			fmt.Fprintf(serverSide, "%s OK done\r\n", tag) //nolint:errcheck
		}
	}()

	return &imapClient{conn: clientSide, r: bufio.NewReader(clientSide)}, srv
}

// The transcript, not the error: cleaning up after a check on INBOX must issue
// no DELETE at all. A version that issues one and has it refused looks the same
// from the caller's side and empties nothing only by the server's grace.
func TestRootCleanupIssuesNoDelete(t *testing.T) {
	c, srv := newFakeClient(t, 1)
	if err := c.cleanupAfterCheck("INBOX", []string{"seed-1@test"}); err != nil {
		t.Fatalf("cleanupAfterCheck: %v", err)
	}
	if srv.sentAny("DELETE ") {
		t.Errorf("the run asked to DELETE the mailbox root; transcript: %v", srv.commands())
	}
	if srv.sentAny("UID SEARCH ALL") {
		t.Errorf("the run searched the whole mailbox; transcript: %v", srv.commands())
	}
	if !srv.sentAny("UID SEARCH HEADER") {
		t.Errorf("the cleanup did not scope itself to what it seeded; transcript: %v", srv.commands())
	}
}

// The owned-folder case still removes the folder, or the guard above would push
// the next author into leaving state behind on every run.
func TestOwnedFolderCleanupIssuesTheDelete(t *testing.T) {
	c, srv := newFakeClient(t, 1)
	if err := c.cleanupAfterCheck("SmokeSieve", nil); err != nil {
		t.Fatalf("cleanupAfterCheck: %v", err)
	}
	if !srv.sentAny("DELETE ") {
		t.Errorf("a folder the run created was not removed; transcript: %v", srv.commands())
	}
}

// Nothing is expunged that the run did not put there: with a seeded ID the
// search is by header, and an empty result expunges nothing at all.
func TestRootCleanupWithNothingSeededExpungesNothing(t *testing.T) {
	c, srv := newFakeClient(t, 1)
	if err := c.cleanupAfterCheck("INBOX", nil); err != nil {
		t.Fatalf("cleanupAfterCheck: %v", err)
	}
	if srv.sentAny("UID STORE") || srv.sentAny("EXPUNGE") {
		t.Errorf("a cleanup with nothing to clean still expunged; transcript: %v", srv.commands())
	}
}
