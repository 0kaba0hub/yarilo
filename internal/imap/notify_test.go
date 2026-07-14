package imap_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	imapserver "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// startNotifyServer starts a bare IMAP server and returns its listen address for
// raw-connection NOTIFY tests (imapclient has no NOTIFY command).
func startNotifyServer(t *testing.T) string {
	t.Helper()
	srv := imapserver.New(imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"},
		Auth:     &stubPassdb{user: "user@test.com", pass: "testpass"},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// rawConn is a minimal line-oriented IMAP client for tests.
type rawConn struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	seq  int
}

func dialRaw(t *testing.T, addr string) *rawConn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	rc := &rawConn{t: t, conn: conn, r: bufio.NewReader(conn)}
	rc.readLine() // greeting
	return rc
}

func (rc *rawConn) readLine() string {
	rc.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := rc.r.ReadString('\n')
	if err != nil && line == "" {
		rc.t.Fatalf("read: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// cmd sends a tagged command and returns the accumulated untagged lines up to
// (and including) the tagged completion line.
func (rc *rawConn) cmd(command string) string {
	rc.seq++
	tag := fmt.Sprintf("a%03d", rc.seq)
	fmt.Fprintf(rc.conn, "%s %s\r\n", tag, command)
	var b strings.Builder
	for {
		line := rc.readLine()
		b.WriteString(line)
		b.WriteString("\n")
		if strings.HasPrefix(line, tag+" ") {
			return b.String()
		}
	}
}

func (rc *rawConn) login() {
	if !strings.Contains(rc.cmd(`LOGIN "user@test.com" "testpass"`), "OK") {
		rc.t.Fatal("login failed")
	}
}

func (rc *rawConn) appendMsg(mbox string) {
	rc.seq++
	tag := fmt.Sprintf("a%03d", rc.seq)
	body := "Subject: probe\r\n\r\nhi\r\n"
	fmt.Fprintf(rc.conn, "%s APPEND %s {%d}\r\n", tag, mbox, len(body))
	rc.readLine() // continuation "+ ..."
	fmt.Fprintf(rc.conn, "%s\r\n", body)
	for {
		if strings.HasPrefix(rc.readLine(), tag+" ") {
			return
		}
	}
}

// TestNotifySelectedSuppressesNewMessages verifies RFC 5465 SELECTED gating: with
// NOTIFY SET SELECTED listing only MessageExpunge/FlagChange, a message appended
// by another session does NOT produce an unsolicited EXISTS; the control session
// (no NOTIFY) does receive it.
func TestNotifySelectedSuppressesNewMessages(t *testing.T) {
	addr := startNotifyServer(t)

	// Control: default behavior — NOOP after a sibling APPEND shows EXISTS.
	ctrl := dialRaw(t, addr)
	ctrl.login()
	ctrl.cmd(`SELECT INBOX`)
	writer := dialRaw(t, addr)
	writer.login()
	writer.appendMsg("INBOX")
	if !strings.Contains(ctrl.cmd("NOOP"), "EXISTS") {
		t.Fatal("control (no NOTIFY) should receive EXISTS after sibling APPEND")
	}

	// NOTIFY SET SELECTED without MessageNew — EXISTS must be suppressed.
	c := dialRaw(t, addr)
	c.login()
	c.cmd(`SELECT INBOX`)
	if !strings.Contains(c.cmd(`NOTIFY SET (SELECTED (MessageExpunge FlagChange))`), "OK") {
		t.Fatal("NOTIFY SET failed")
	}
	writer.appendMsg("INBOX")
	if resp := c.cmd("NOOP"); strings.Contains(resp, "EXISTS") {
		t.Fatalf("NOTIFY without MessageNew should suppress EXISTS, got: %q", resp)
	}

	// NOTIFY NONE reverts to default — EXISTS delivered again.
	c.cmd(`NOTIFY NONE`)
	writer.appendMsg("INBOX")
	if !strings.Contains(c.cmd("NOOP"), "EXISTS") {
		t.Fatal("after NOTIFY NONE, EXISTS should be delivered again")
	}
}
