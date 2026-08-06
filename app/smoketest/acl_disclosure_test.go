package main

import (
	"bufio"
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
