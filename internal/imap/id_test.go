package imap_test

import (
	"fmt"
	"strings"
	"testing"
)

// TestIDAnswersFromConfig: imap_id_send is what the server answers with, and
// "*" still expands to the server defaults. Asserted on the wire, because the
// wire is what changed when ID moved from a connection wrapper into the parser.
func TestIDAnswersFromConfig(t *testing.T) {
	c := selectedSession(t)
	got := c.cmd(`ID ("name" "probe" "version" "1.0")`)
	if !strings.Contains(got, `"name" "yarilo"`) {
		t.Errorf(`ID answer lost the configured name: %q`, got)
	}
	if !strings.Contains(got, "OK ID completed") {
		t.Errorf("ID not completed: %q", got)
	}
}

// TestIDIsAdvertised: a client only sends ID if CAPABILITY says it may. The
// wrapper used to append it to the capability line on the way out; it is a
// declared capability now, and losing it would silently stop clients asking.
func TestIDIsAdvertised(t *testing.T) {
	c := dialRaw(t, sandboxLikeServer(t))
	got := c.cmd("CAPABILITY")
	if !strings.Contains(got, " ID") {
		t.Errorf("CAPABILITY does not advertise ID: %q", got)
	}
}

// TestIDBeforeLogin: RFC 2971 puts ID in any state, and clients send it before
// authenticating to identify themselves in the server's logs.
func TestIDBeforeLogin(t *testing.T) {
	c := dialRaw(t, sandboxLikeServer(t))
	if got := c.cmd(`ID ("name" "probe")`); !strings.Contains(got, "OK ID completed") {
		t.Errorf("ID must be answered before login: %q", got)
	}
}

// TestIDInsideLiteralIsMessageData is #1375 from the yarilo side: a body line
// that reads like an ID command must be stored, not answered. The wrapper could
// not tell the two apart; the parser does not have to be told.
func TestIDInsideLiteralIsMessageData(t *testing.T) {
	c := selectedSession(t)
	body := "From: a@b\r\nSubject: t\r\nX-Note: X ID here\r\n\r\nhello\r\n"
	fmt.Fprintf(c.conn, "b1 APPEND INBOX {%d+}\r\n%s\r\n", len(body), body)
	for {
		l := c.readLine()
		if strings.Contains(l, "ID completed") || strings.HasPrefix(l, "* ID ") {
			t.Fatalf("a line inside the literal was answered as an ID command: %q", l)
		}
		if strings.HasPrefix(l, "b1 ") {
			break
		}
	}
	// The message must be there, whole: FETCH it back and look for the line
	// that used to be eaten.
	got := c.cmd("FETCH 1 (BODY.PEEK[])")
	if !strings.Contains(got, "X ID here") {
		t.Errorf("the body line that reads like a command was not stored: %q", got)
	}
}
