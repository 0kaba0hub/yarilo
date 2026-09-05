package imap_test

import (
	"strings"
	"testing"
)

// A malformed command is answered BAD, not NO [SERVERBUG]: the latter tells the
// client and the operator the server is broken, over the client's syntax (#1689).
func TestAMalformedCommandIsAnsweredBad(t *testing.T) {
	addr := startNotifyServer(t)
	for _, tc := range []struct{ name, cmd string }{
		{"sequence-set", `SEARCH HEADER Subject fileinto test`},
		{"mailbox name", `SELECT "&"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := dialRaw(t, addr)
			rc.login()
			got := lastLine(rc.cmd(tc.cmd))
			if strings.Contains(got, "SERVERBUG") {
				t.Errorf("a malformed command was answered %q: the client is told the "+
					"server is broken by its own syntax", got)
			}
			if !strings.Contains(got, " BAD ") {
				t.Errorf("answer was %q, want BAD", got)
			}
		})
	}
}

// A well-formed command the handler refuses is not BAD, so this stays a
// classification rather than "answer BAD to everything".
func TestAHandlerRefusalIsNotBad(t *testing.T) {
	rc := dialRaw(t, startNotifyServer(t))
	rc.login()
	if got := lastLine(rc.cmd("SELECT nosuchbox")); strings.Contains(got, " BAD ") {
		t.Errorf("a well-formed command the handler refused was answered %q", got)
	}
}

func lastLine(resp string) string {
	lines := strings.Split(strings.TrimRight(resp, "\n"), "\n")
	return lines[len(lines)-1]
}
