package imap_test

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// sandboxLikeServer matches the accept chain the field runs: both line
// scanners are enabled (imap_max_line_length and imap_id_send), which is what
// #1370 needed to appear -- one of them knew about literals and the other did
// not.
func sandboxLikeServer(t *testing.T) string {
	addr, _ := sandboxLikeServerWithHome(t)
	return addr
}

// sandboxLikeServerWithHome also returns the user's home, for tests that reach
// past the protocol to the files under it.
func sandboxLikeServerWithHome(t *testing.T) (addr, home string) {
	t.Helper()
	root := t.TempDir()
	srv := imapserver.New(imapserver.Options{
		Mailbox:       mdbox.New(),
		Index:         file.New(),
		Resolver:      &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		Auth:          &enforcePassdb{users: map[string]string{"alice": "pw"}},
		MaxLineLength: 65536,
		IDSend:        "name *",
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck
	return ln.Addr().String(), filepath.Join(root, "alice")
}

func selectedSession(t *testing.T) *rawConn {
	t.Helper()
	c := dialRaw(t, sandboxLikeServer(t))
	if !strings.Contains(c.cmd("LOGIN alice pw"), "OK") {
		t.Fatal("login")
	}
	if !strings.Contains(c.cmd("SELECT INBOX"), "OK") {
		t.Fatal("select")
	}
	return c
}

// appendAndRead runs one APPEND, drains its replies, then sends one more
// command and returns the first line that comes back.
func appendAndRead(t *testing.T, c *rawConn, body string, nonSync bool) string {
	t.Helper()
	if nonSync {
		fmt.Fprintf(c.conn, "b1 APPEND INBOX {%d+}\r\n%s\r\n", len(body), body)
	} else {
		fmt.Fprintf(c.conn, "b1 APPEND INBOX {%d}\r\n", len(body))
		if got := c.readLine(); !strings.HasPrefix(got, "+") {
			t.Fatalf("expected a continuation request, got %q", got)
		}
		fmt.Fprintf(c.conn, "%s\r\n", body)
	}
	for {
		l := c.readLine()
		if strings.HasPrefix(l, "b1 ") {
			if !strings.Contains(l, "OK") {
				t.Fatalf("APPEND: %s", l)
			}
			break
		}
	}
	fmt.Fprint(c.conn, "b2 NOOP\r\n")
	return c.readLine()
}

// TestCommandAfterAppendIsAnswered is #1370 in the shape the field reported it:
// the command after a successful APPEND is never answered, the client waits for
// a tag that never comes, and nothing is logged.
//
// The body carries a line whose second token is "ID". That line used to be
// intercepted by the ID wrapper -- answered as if it were a command and removed
// from the stream -- so the literal came up short and made up the difference
// from the command that followed. Message bodies are not commands, and the two
// line scanners in the accept chain must agree on where a literal starts.
func TestCommandAfterAppendIsAnswered(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"body line reads like an ID command", "From: a@b\r\nSubject: t\r\nX ID here\r\n\r\nhello\r\n"},
		{"ordinary body", "From: a@b\r\nSubject: t\r\n\r\nhello, a body!!\r\n"},
	} {
		for _, sync := range []bool{true, false} {
			name := tc.name
			if sync {
				name += ", synchronizing literal"
			}
			t.Run(name, func(t *testing.T) {
				c := selectedSession(t)
				got := appendAndRead(t, c, tc.body, !sync)
				if !strings.HasPrefix(got, "b2 ") {
					t.Fatalf("the command after APPEND was not answered; first line back was %q", got)
				}
			})
		}
	}
}

// TestIDCommandStillWorks: the interception itself must survive the fix -- ID
// is answered by the wrapper, since go-imap has no server-side ID.
func TestIDCommandStillWorks(t *testing.T) {
	c := selectedSession(t)
	got := c.cmd(`ID ("name" "probe")`)
	if !strings.Contains(got, "* ID (") || !strings.Contains(got, "OK ID completed") {
		t.Errorf("ID no longer answered: %q", got)
	}
}

// TestIDCommandWithLiteralArgument: an ID command may carry its arguments as
// literals. The wrapper answers that line itself, so the payload must not be
// left in the stream -- read as commands it would derail everything after it.
func TestIDCommandWithLiteralArgument(t *testing.T) {
	c := selectedSession(t)
	name := "probe"
	fmt.Fprintf(c.conn, "c1 ID (\"name\" {%d+}\r\n%s)\r\n", len(name), name)
	for {
		l := c.readLine()
		if strings.HasPrefix(l, "c1 ") {
			break
		}
	}
	fmt.Fprint(c.conn, "c2 NOOP\r\n")
	if got := c.readLine(); !strings.HasPrefix(got, "c2 ") {
		t.Fatalf("the command after a literal-carrying ID was not answered; got %q", got)
	}
}

// TestMalformedAppendTailKeepsTheNextCommand is #1370 as the field hit it: a
// client that omits the CRLF terminating its APPEND command line. The message
// is stored and answered OK (#1129), the framing fault is logged -- and the
// command that follows, which the client sent correctly, must still be served.
//
// Kept here as well as in the fork because this is the property yarilo depends
// on, and a pin bump is where it would silently come back.
func TestMalformedAppendTailKeepsTheNextCommand(t *testing.T) {
	c := selectedSession(t)
	body := "From: a@b\r\nSubject: t\r\n\r\nhello\r\n"
	// No terminating CRLF after the literal; the next command follows at once.
	fmt.Fprintf(c.conn, "b1 APPEND INBOX {%d+}\r\n%s", len(body), body)
	fmt.Fprint(c.conn, "b2 NOOP\r\n")
	for {
		l := c.readLine()
		if strings.HasPrefix(l, "b1 ") {
			if !strings.Contains(l, "OK") {
				t.Fatalf("a stored message must still be answered OK: %s", l)
			}
			break
		}
	}
	if got := c.readLine(); !strings.HasPrefix(got, "b2 ") {
		t.Fatalf("the client's next command was discarded by the resync; got %q", got)
	}
}
