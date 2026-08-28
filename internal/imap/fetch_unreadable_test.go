package imap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A message the driver cannot read is answered as an empty FETCH, and that has
// to be counted.
//
// This is the shape both format faults took (#1525 and the rollback window):
// the message is in the mailbox, SELECT counts it, the size comes back from the
// index, and every content section is empty. A client cannot tell it from an
// empty message, so the only place it can be noticed is a counter -- and this
// counter stayed at zero through both.
//
// The file is removed rather than corrupted because the point is the answer,
// not the failure mode: any read error reaches the same swallow.
func TestFetchCountsAMessageItCouldNotRead(t *testing.T) {
	root, addr := startEnvelopeCacheServer(t)
	c := dialRaw(t, addr)
	c.login()
	appendEnvelopeMsg(t, c)
	c.cmd(`SELECT INBOX`)

	// Removed before anything reads it, so nothing is cached: a cached
	// envelope would answer from memory and prove nothing.
	var msgFile string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() &&
			(strings.Contains(p, "/cur/") || strings.Contains(p, "/new/")) {
			msgFile = p
		}
		return nil
	})
	if msgFile == "" {
		t.Fatal("message file not found on disk")
	}
	if err := os.Remove(msgFile); err != nil {
		t.Fatal(err)
	}

	before := unreadableCount(t, "fetch")
	out := c.cmd(`FETCH 1 (ENVELOPE BODY.PEEK[TEXT])`)

	// The answer really is the silent one: no envelope, no error.
	if strings.Contains(out, "alice") {
		t.Fatalf("the message was readable after all, so this proves nothing:\n%s", out)
	}
	if strings.Contains(out, "NO ") || strings.Contains(out, "BAD ") {
		t.Fatalf("the fetch failed loudly, which is a different case:\n%s", out)
	}

	if got := unreadableCount(t, "fetch") - before; got != 1 {
		t.Errorf("counted %v unreadable messages for one message answered empty, want 1\nresponse: %s", got, out)
	}
}

// Two attributes lost on one message is one message, not two: the counter says
// how many messages were answered short, and an alert on it must not scale with
// how much the client happened to ask for.
func TestOneMessageIsCountedOnceHoweverManyAttributesAreLost(t *testing.T) {
	root, addr := startEnvelopeCacheServer(t)
	c := dialRaw(t, addr)
	c.login()
	appendEnvelopeMsg(t, c)
	c.cmd(`SELECT INBOX`)

	var msgFile string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() &&
			(strings.Contains(p, "/cur/") || strings.Contains(p, "/new/")) {
			msgFile = p
		}
		return nil
	})
	if err := os.Remove(msgFile); err != nil {
		t.Fatal(err)
	}

	before := unreadableCount(t, "fetch")
	c.cmd(`FETCH 1 (ENVELOPE BODYSTRUCTURE BODY.PEEK[HEADER] BODY.PEEK[TEXT])`)

	if got := unreadableCount(t, "fetch") - before; got != 1 {
		t.Errorf("counted %v for one message with four attributes lost, want 1", got)
	}
}

// A message whose body really is empty is not counted.
//
// The body here is empty, not merely short: headers and nothing after them, so
// BODY[TEXT] is a legal `{0}`. A message with a body of "hi" would not test
// this at all -- it would pass whether the counter fires on "could not
// produce" or on "produced nothing", which are the two things that must stay
// apart. Empty mail is legal, and a counter that fires on it is one an
// operator learns to ignore.
func TestAMessageWithAnEmptyBodyIsNotCounted(t *testing.T) {
	_, addr := startEnvelopeCacheServer(t)
	c := dialRaw(t, addr)
	c.login()
	appendRawMessage(t, c, "From: a@example.com\r\nSubject: no body\r\n\r\n")
	c.cmd(`SELECT INBOX`)

	before := unreadableCount(t, "fetch")
	out := c.cmd(`FETCH 1 (BODY.PEEK[TEXT])`)
	if strings.Contains(out, "NO ") || strings.Contains(out, "BAD ") {
		t.Fatalf("fetch of a readable message failed:\n%s", out)
	}
	if !strings.Contains(out, "{0}") {
		t.Fatalf("the body was not empty, so this row proves nothing:\n%s", out)
	}
	if got := unreadableCount(t, "fetch") - before; got != 0 {
		t.Errorf("counted %v for a message whose body is legally empty", got)
	}
}

func appendRawMessage(t *testing.T, rc *rawConn, body string) {
	t.Helper()
	rc.seq++
	tag := "e002"
	rc.conn.Write([]byte(tag + " APPEND INBOX {" + itoa(len(body)) + "}\r\n")) //nolint:errcheck
	rc.readLine()
	rc.conn.Write([]byte(body + "\r\n")) //nolint:errcheck
	for {
		if strings.HasPrefix(rc.readLine(), tag+" ") {
			return
		}
	}
}
