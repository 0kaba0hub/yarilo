package imap_test

import (
	"strings"
	"testing"
)

func threadUnreadableCount(t *testing.T) float64 {
	return unreadableCount(t, "thread")
}

// A message THREAD cannot read is still in the reply -- as a conversation of
// one, detached from the thread it belongs to.
//
// This is quieter than the SEARCH case it mirrors (#1283). There, the answer
// is a set the client can compare against its own count. Here, a message that
// lost its ancestry looks exactly like a message that never had any: the tree
// is well-formed, plausible, and wrong. Nothing on the wire can say so -- RFC
// 5256 has no way to mark a message the server could not read -- so the claim
// this row makes is that the operator is told.
//
// The counter is the assertion: a log line cannot be asserted without
// capturing the handler, and the metric is what an operator alerts on.
func TestThreadCountsMessagesItCouldNotRead(t *testing.T) {
	raws := []string{
		mailOf("a@x", "Plan", "Sun, 1 Mar 2026 10:00:00 +0000", ""),
		mailOf("b@x", "Re: Plan", "Sun, 1 Mar 2026 11:00:00 +0000", "<a@x>"),
	}
	conn, rd, root := threadServerIn(t, raws)

	// Broken BEFORE anything reads them, so the cache is cold. With a warm
	// cache THREAD answers from it and never learns the bytes are gone --
	// which is the point of caching References and is pinned separately
	// below. This row is about the cold path, where the read still happens
	// and still has to be reported.
	removed := makeStoredMessagesUnreadable(t, root)
	if removed != 2 {
		t.Fatalf("made %d messages unreadable, want 2", removed)
	}

	before := threadUnreadableCount(t)
	got := threadLine(t, conn, rd, "THREAD REFERENCES UTF-8 ALL")
	// Both messages are still named -- they exist, and the client can fetch
	// them -- but the reply no longer says one answers the other.
	if got != "* THREAD (1)(2)" {
		t.Errorf("THREAD over unreadable messages = %q, want each on its own", got)
	}
	if n := threadUnreadableCount(t) - before; n != float64(removed) {
		t.Errorf("counter rose by %v, want %d -- the tree lost their ancestry silently", n, removed)
	}
}

// When the criteria need the message body, an unreadable message is excluded
// rather than detached: it is not a message that matched, it is a message
// nobody looked at (#1283).
func TestThreadExcludesUnreadableMessagesFromABodySearch(t *testing.T) {
	raws := []string{
		mailOf("a@x", "Plan", "Sun, 1 Mar 2026 10:00:00 +0000", ""),
		mailOf("b@x", "Re: Plan", "Sun, 1 Mar 2026 11:00:00 +0000", "<a@x>"),
	}
	conn, rd, root := threadServerIn(t, raws)
	if n := makeStoredMessagesUnreadable(t, root); n != 2 {
		t.Fatalf("made %d messages unreadable, want 2", n)
	}

	before := threadUnreadableCount(t)
	got := threadLine(t, conn, rd, `THREAD REFERENCES UTF-8 TEXT "body"`)
	if strings.TrimSpace(got) != "* THREAD" {
		t.Errorf("THREAD = %q, want no messages: a body criterion cannot match bytes nobody read", got)
	}
	if n := threadUnreadableCount(t) - before; n != 2 {
		t.Errorf("counter rose by %v, want 2", n)
	}
}

// The row that proves the last reason to open a message is gone.
//
// A THREAD runs first and warms the cache; then every stored message is
// destroyed; then THREAD runs again and must still answer the whole
// conversation. That is only possible if the second run opened nothing --
// References included, which is the field this exists to cache (#1461).
//
// A timing assertion would have measured this machine. This measures the
// property: the answer no longer depends on the files.
func TestThreadAnswersFromTheCacheWithoutOpeningMessages(t *testing.T) {
	raws := []string{
		mailOf("a@x", "Plan", "Sun, 1 Mar 2026 10:00:00 +0000", ""),
		mailOf("b@x", "Re: Plan", "Sun, 1 Mar 2026 11:00:00 +0000", "<a@x>"),
		mailOf("c@x", "Budget", "Sun, 1 Mar 2026 12:00:00 +0000", ""),
	}
	conn, rd, root := threadServerIn(t, raws)

	if got := threadLine(t, conn, rd, "THREAD REFERENCES UTF-8 ALL"); got != "* THREAD (1 2)(3)" {
		t.Fatalf("warming THREAD = %q, want the conversation and the stranger", got)
	}

	if n := makeStoredMessagesUnreadable(t, root); n != 3 {
		t.Fatalf("made %d messages unreadable, want 3", n)
	}
	before := threadUnreadableCount(t)

	for _, tag := range []string{"w1", "w2"} {
		if got := threadLine(t, conn, rd, "THREAD REFERENCES UTF-8 ALL"); got != "* THREAD (1 2)(3)" {
			t.Errorf("%s: THREAD over destroyed messages = %q, want the same tree -- it came from the cache or not at all", tag, got)
		}
	}
	if n := threadUnreadableCount(t) - before; n != 0 {
		t.Errorf("the scan reported %v unreadable messages, so it opened them despite the cache", n)
	}
}
