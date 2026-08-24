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

	before := threadUnreadableCount(t)
	if got := threadLine(t, conn, rd, "THREAD REFERENCES UTF-8 ALL"); got != "* THREAD (1 2)" {
		t.Fatalf("baseline THREAD = %q, want the conversation", got)
	}
	if got := threadUnreadableCount(t) - before; got != 0 {
		t.Fatalf("readable messages counted as unreadable: %v", got)
	}

	// Take the stored bytes away under the session's feet, leaving the index
	// records in place: the shape a storage failure produces.
	removed := makeStoredMessagesUnreadable(t, root)
	if removed != 2 {
		t.Fatalf("made %d messages unreadable, want 2", removed)
	}

	before = threadUnreadableCount(t)
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
