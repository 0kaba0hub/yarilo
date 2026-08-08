package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// fetchServer answers SELECT and FETCH the way a given build would, so each
// failure shape the gate has to catch can be driven directly.
type fetchServer struct {
	mu      sync.Mutex
	fetches int
	// reply holds the untagged FETCH lines; empty sends none. tear drops the
	// connection mid-command, which is what a panicking server looks like
	// from a client (#1184).
	reply []string
	tear  bool
}

func newFetchClient(t *testing.T, exists int, srv *fetchServer) *imapClient {
	t.Helper()
	clientSide, serverSide := net.Pipe()
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
			verb, _, _ := strings.Cut(rest, " ")
			switch strings.ToUpper(verb) {
			case "SELECT":
				fmt.Fprintf(serverSide, "* %d EXISTS\r\n", exists) //nolint:errcheck
			case "FETCH":
				srv.mu.Lock()
				srv.fetches++
				tear, reply := srv.tear, srv.reply
				srv.mu.Unlock()
				if tear {
					serverSide.Close()
					return
				}
				for _, l := range reply {
					fmt.Fprintf(serverSide, "%s\r\n", l) //nolint:errcheck
				}
			}
			fmt.Fprintf(serverSide, "%s OK done\r\n", tag) //nolint:errcheck
		}
	}()
	return &imapClient{conn: clientSide, r: bufio.NewReader(clientSide)}
}

// row renders one untagged FETCH line carrying both items.
func row(seq int) string {
	return fmt.Sprintf(`* %d FETCH (ENVELOPE (NIL "s" NIL NIL NIL NIL NIL NIL NIL NIL) `+
		`BODYSTRUCTURE ("text" "plain" NIL NIL NIL "7bit" 4 1))`, seq)
}

func TestFetchEnvelopeProbe(t *testing.T) {
	full := []string{row(1), row(2), row(3)}
	cases := []struct {
		name    string
		reply   []string
		tear    bool
		wantErr string
	}{
		{name: "every row answered", reply: full},
		// A tagged OK with rows missing: the shape a check that greps for
		// BAD/NO calls healthy.
		{name: "partial answer", reply: full[:2], wantErr: "untagged rows, want 3"},
		// The shipped crash: the command is never answered and the
		// connection goes away.
		{name: "connection torn mid-command", tear: true, wantErr: "FETCH"},
		{name: "no untagged data at all", wantErr: "0 untagged rows"},
		{
			name:    "a row without body structure",
			reply:   []string{row(1), `* 2 FETCH (ENVELOPE (NIL "s" NIL NIL NIL NIL NIL NIL NIL NIL))`, row(3)},
			wantErr: "BODYSTRUCTURE",
		},
		{
			name:    "a row without envelope",
			reply:   []string{row(1), `* 2 FETCH (BODYSTRUCTURE ("text" "plain" NIL NIL NIL "7bit" 4 1))`, row(3)},
			wantErr: "ENVELOPE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &fetchServer{reply: tc.reply, tear: tc.tear}
			c := newFetchClient(t, 3, srv)
			err := fetchEnvelopeProbe(c)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want success, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want an error mentioning %q, got success", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// The cache is only exercised on a second read: the first pass fills it. A
// probe that fetches once would have passed on a build whose cached path
// answers wrongly, which is the half this gate exists for.
func TestFetchEnvelopeProbeReadsTwice(t *testing.T) {
	srv := fetchServer{reply: []string{row(1), row(2), row(3)}}
	c := newFetchClient(t, 3, &srv)
	if err := fetchEnvelopeProbe(c); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.fetches != 2 {
		t.Errorf("issued %d FETCHes, want 2 (cold then cached)", srv.fetches)
	}
}
