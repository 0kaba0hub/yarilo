package imap_test

import (
	"strings"
	"testing"
)

// A keyword another session set between this session's SELECT and its FETCH
// must not deadlock the connection.
//
// The announcement of a new keyword is an untagged "* FLAGS (...)", written on
// the fetch writer. It used to be written from inside a message block, and that
// block holds the connection's encoder for as long as it is open: the same
// goroutine then asked for the same encoder again and stopped there. The
// session never answered, and because the message cache handle was still open,
// every other session of that user queued behind it for minutes (#1543).
//
// The window is the point: this session learns keywords at SELECT and from its
// own STORE, so only a keyword set elsewhere in between is unknown here. A
// single-session test cannot reach it.
//
// A hang shows up as the read deadline in readLine rather than as a stuck test.
func TestAKeywordSetByAnotherSessionDoesNotWedgeFetch(t *testing.T) {
	_, addr := startEnvelopeCacheServer(t)

	first := dialRaw(t, addr)
	first.login()
	appendEnvelopeMsg(t, first)
	first.cmd(`SELECT INBOX`) // knownKeywords is fixed here

	second := dialRaw(t, addr)
	second.login()
	second.cmd(`SELECT INBOX`)
	if out := second.cmd(`STORE 1 +FLAGS ($Important)`); !strings.Contains(out, "OK") {
		t.Fatalf("the other session could not set the keyword:\n%s", out)
	}

	out := first.cmd(`FETCH 1 (FLAGS)`)

	if !strings.Contains(out, "* FLAGS") {
		t.Errorf("the new keyword was never announced:\n%s", out)
	}
	if !strings.Contains(out, "$Important") {
		t.Errorf("the keyword did not reach the client:\n%s", out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("the fetch did not complete:\n%s", out)
	}

	// The connection is still usable: a wedged encoder would take the next
	// command with it, and an answered-but-broken session would not.
	if out := first.cmd(`NOOP`); !strings.Contains(out, "OK") {
		t.Errorf("the connection did not survive the fetch:\n%s", out)
	}
}
