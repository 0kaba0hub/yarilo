package imap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
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

// unreadableByReason reads one command's count for one reason. Summing the
// reasons would defeat the point of splitting them.
func unreadableByReason(t *testing.T, command, reason string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "imap_unreadable_messages_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var gotCmd, gotReason string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "command":
					gotCmd = l.GetValue()
				case "reason":
					gotReason = l.GetValue()
				}
			}
			if gotCmd == command && gotReason == reason {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// The two reasons are told apart by what is on disk, and this is the pair that
// makes the counter usable.
//
// A message removed while a client fetches it is ordinary: one connection
// expunges, another is working from an index snapshot taken before that, and a
// clean gate produced 239 of them. A message that is still there and cannot be
// read is the fault the counter exists for -- and before this split an alert on
// the counter fired on both, which is an alert that gets switched off (#1538).
//
// Both rows drive the same code down to the last step. The only difference is
// whether the file is gone or merely unreadable, which is exactly the
// distinction under test.
func TestTheTwoReasonsAreToldApart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, path string)
		reason string
		other  string
	}{
		{
			name:   "the message was removed while it was being fetched",
			damage: func(t *testing.T, path string) { mustRemove(t, path) },
			reason: "gone",
			other:  "unreadable",
		},
		{
			name: "the message is there and cannot be read",
			damage: func(t *testing.T, path string) {
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
			},
			reason: "unreadable",
			other:  "gone",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, addr := startEnvelopeCacheServer(t)
			c := dialRaw(t, addr)
			c.login()
			appendEnvelopeMsg(t, c)
			c.cmd(`SELECT INBOX`)
			tc.damage(t, storedMessagePath(t, root))

			before := unreadableByReason(t, "fetch", tc.reason)
			beforeOther := unreadableByReason(t, "fetch", tc.other)
			out := c.cmd(`FETCH 1 (ENVELOPE BODY.PEEK[TEXT])`)
			if strings.Contains(out, "alice") {
				t.Fatalf("the message was readable, so this proves nothing:\n%s", out)
			}

			if got := unreadableByReason(t, "fetch", tc.reason) - before; got != 1 {
				t.Errorf("counted %v under reason=%s, want 1", got, tc.reason)
			}
			if got := unreadableByReason(t, "fetch", tc.other) - beforeOther; got != 0 {
				t.Errorf("counted %v under reason=%s, which is the other thing entirely", got, tc.other)
			}
		})
	}
}

func storedMessagePath(t *testing.T, root string) string {
	t.Helper()
	var found string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() &&
			(strings.Contains(p, "/cur/") || strings.Contains(p, "/new/")) {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatal("message file not found on disk")
	}
	return found
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
