package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// The oracle half of the deploy check had never been executed: it is off
// without flags, so CI ran the file and not the code. Written as it was, it
// reported a leak on a perfectly behaved server, because the reply it compared
// included the command tag and the tag increments — the present probe was
// S0007 and the absent one S0008.
//
// That is the same trap the check exists to prevent, reproduced inside the
// checker. So the checker gets its own server.
func TestACLNoOracleAcceptsAServerThatHidesProperly(t *testing.T) {
	c, stop := replyingClient(t, func(cmd string) string {
		return "NO [NONEXISTENT] No such mailbox"
	})
	defer stop()

	if err := aclNoOracle(c, "Public/SmokeAclProbe", "Public/"); err != nil {
		t.Errorf("a server that answers identically for both names was reported as leaking: %v", err)
	}
}

// And it must still catch the thing it is for.
func TestACLNoOracleCatchesADifferenceThatMatters(t *testing.T) {
	cases := []struct {
		name  string
		reply func(cmd string) string
	}{
		{
			"a different code for the mailbox that exists",
			func(cmd string) string {
				if strings.Contains(cmd, "SmokeAclProbe") {
					return "NO [NOPERM] Permission denied: missing right 'r'"
				}
				return "NO [NONEXISTENT] No such mailbox"
			},
		},
		{
			"the same code, different wording",
			func(cmd string) string {
				if strings.Contains(cmd, "SmokeAclProbe") {
					return "NO [NONEXISTENT] Mailbox is not accessible"
				}
				return "NO [NONEXISTENT] No such mailbox"
			},
		},
		{
			"answered outright for the mailbox that exists",
			func(cmd string) string {
				if strings.Contains(cmd, "SmokeAclProbe") {
					return "OK done"
				}
				return "NO [NONEXISTENT] No such mailbox"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, stop := replyingClient(t, tc.reply)
			defer stop()
			if err := aclNoOracle(c, "Public/SmokeAclProbe", "Public/"); err == nil {
				t.Error("the difference was not reported")
			}
		})
	}
}

// A reply that names the mailbox must not read as a difference: the names
// differ by construction, and comparing them would fire on every deployment
// whose wording happens to include the folder.
func TestACLNoOracleIgnoresTheMailboxNameInTheReply(t *testing.T) {
	c, stop := replyingClient(t, func(cmd string) string {
		name := "Public/SmokeNoSuchMailbox"
		if strings.Contains(cmd, "SmokeAclProbe") {
			name = "Public/SmokeAclProbe"
		}
		return fmt.Sprintf("NO [NONEXISTENT] Mailbox %q does not exist", name)
	})
	defer stop()

	if err := aclNoOracle(c, "Public/SmokeAclProbe", "Public/"); err != nil {
		t.Errorf("a reply quoting the mailbox name was read as a disclosure: %v", err)
	}
}

// replyingClient serves one tagged reply per command, chosen by reply(). The
// tag is echoed back as the server must, which is what made the original
// comparison fail.
func replyingClient(t *testing.T, reply func(cmd string) string) (*imapClient, func()) {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	var once sync.Once
	stop := func() { once.Do(func() { clientSide.Close(); serverSide.Close() }) }
	t.Cleanup(stop)

	go func() {
		r := bufio.NewReader(serverSide)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			tag, rest, _ := strings.Cut(line, " ")
			fmt.Fprintf(serverSide, "%s %s\r\n", tag, reply(rest)) //nolint:errcheck
		}
	}()
	return &imapClient{conn: clientSide, r: bufio.NewReader(clientSide)}, stop
}

// A probe that could not be removed is a failed check, not a printed note. The
// old code reported it through fmt.Printf and returned nil, so every run left
// another mailbox in the shared namespace and still exited 0 (#1104).
func TestACLCleanupFailureIsACheckFailure(t *testing.T) {
	checkFailed := errors.New("SELECT answered a peer with no rights")
	deleteFailed := errors.New("NO [NOPERM] missing right 'x'")

	cases := []struct {
		name       string
		check      error
		cleanup    error
		wantErr    bool
		wantSubstr []string
	}{
		{"both clean", nil, nil, false, nil},
		{"cleanup failed alone", nil, deleteFailed, true,
			[]string{"missing right 'x'", "Public/SmokeAclProbe", "still in"}},
		{"check failed, cleanup fine", checkFailed, nil, true,
			[]string{"answered a peer with no rights"}},
		// Both: the check's own error is the one that names the defect, so it
		// stays the message and the wrapped error, while the survived probe is
		// still reported rather than dropped.
		{"both failed", checkFailed, deleteFailed, true,
			[]string{"answered a peer with no rights", "cleanup", "missing right 'x'"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := aclCleanupResult(tc.check, tc.cleanup, "Public/SmokeAclProbe", "Public/")
			if (got != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", got, tc.wantErr)
			}
			for _, want := range tc.wantSubstr {
				if !strings.Contains(got.Error(), want) {
					t.Errorf("error %q does not mention %q", got, want)
				}
			}
			if tc.check != nil && got != nil && !errors.Is(got, tc.check) {
				t.Errorf("the check's own error was lost: %v", got)
			}
		})
	}
}
