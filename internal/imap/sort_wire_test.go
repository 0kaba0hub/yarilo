package imap_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
)

func sortLine(t *testing.T, conn net.Conn, rd *bufio.Reader, tag, cmd string) string {
	t.Helper()
	for _, line := range command(t, conn, rd, tag, cmd) {
		if strings.HasPrefix(line, "* SORT") {
			return line
		}
	}
	return ""
}

func mailFrom(id, subject, date, from string) string {
	return fmt.Sprintf("Message-ID: <%s>\r\nSubject: %s\r\nDate: %s\r\nFrom: %s\r\n\r\nbody\r\n",
		id, subject, date, from)
}

// The row the whole key hangs on: FROM sorts by addr-mailbox -- the local part
// -- and NOT by the display name a client shows (RFC 5256 §3).
//
// Seeded so the two orders are opposite. By display name it is Alice then Bob;
// by local part it is zulu after alpha, so the answer is 2 1. An
// implementation that sorted by the visible name would return 1 2, which is
// the order a reader of the message list would call correct -- which is
// exactly why this needs a row rather than a reading.
func TestSortFromUsesTheAddressNotTheDisplayName(t *testing.T) {
	raws := []string{
		mailFrom("a@x", "One", "Sun, 1 Mar 2026 10:00:00 +0000", "Alice Adams <zulu@example.com>"),
		mailFrom("b@x", "Two", "Sun, 1 Mar 2026 11:00:00 +0000", "Bob Brown <alpha@example.com>"),
	}
	conn, rd, _ := threadServerIn(t, raws)

	if got := sortLine(t, conn, rd, "a3", "SORT (FROM) UTF-8 ALL"); got != "* SORT 2 1" {
		t.Errorf("SORT (FROM) = %q, want 2 1 -- alpha@ before zulu@, not Alice before Bob", got)
	}
}

// DATE is the sent date, ARRIVAL the internal date; the mailbox is seeded so
// the two disagree, otherwise one row would prove both keys.
func TestSortDateAndArrivalAreDifferentKeys(t *testing.T) {
	raws := []string{
		// Delivered first, written last.
		mailFrom("a@x", "One", "Sun, 1 Mar 2026 23:00:00 +0000", "a@example.com"),
		mailFrom("b@x", "Two", "Sun, 1 Mar 2026 01:00:00 +0000", "b@example.com"),
	}
	conn, rd, _ := threadServerIn(t, raws)

	if got := sortLine(t, conn, rd, "a3", "SORT (DATE) UTF-8 ALL"); got != "* SORT 2 1" {
		t.Errorf("SORT (DATE) = %q, want the earlier Date header first", got)
	}
	if got := sortLine(t, conn, rd, "a4", "SORT (ARRIVAL) UTF-8 ALL"); got != "* SORT 1 2" {
		t.Errorf("SORT (ARRIVAL) = %q, want mailbox arrival order", got)
	}
}

// A message with no Date header sorts by its internal date rather than by the
// zero time (§2.2), which would otherwise park every dateless message at the
// front of every DATE sort.
func TestSortFallsBackToTheInternalDate(t *testing.T) {
	raws := []string{
		"Message-ID: <a@x>\r\nSubject: One\r\nFrom: a@example.com\r\n\r\nbody\r\n",
		mailFrom("b@x", "Two", "Sun, 1 Mar 2026 01:00:00 +0000", "b@example.com"),
	}
	conn, rd, _ := threadServerIn(t, raws)

	// Internal dates are seeded in mailbox order, so the dateless message is
	// the earlier of the two and comes first -- not because it is dateless.
	if got := sortLine(t, conn, rd, "a3", "SORT (DATE) UTF-8 ALL"); got != "* SORT 1 2" {
		t.Errorf("SORT (DATE) = %q, want the dateless message ordered by its internal date", got)
	}
}

// UID SORT numbers the same order by UID.
func TestUIDSortAnswersUIDs(t *testing.T) {
	raws := []string{
		mailFrom("a@x", "One", "Sun, 1 Mar 2026 10:00:00 +0000", "a@example.com"),
		mailFrom("b@x", "Two", "Sun, 1 Mar 2026 11:00:00 +0000", "b@example.com"),
	}
	conn, rd, _ := threadServerIn(t, raws)
	command(t, conn, rd, "x1", "STORE 1 +FLAGS (\\Deleted)")
	command(t, conn, rd, "x2", "EXPUNGE")

	if got := sortLine(t, conn, rd, "a3", "SORT (DATE) UTF-8 ALL"); got != "* SORT 1" {
		t.Fatalf("SORT = %q, want sequence number 1", got)
	}
	if got := sortLine(t, conn, rd, "a4", "UID SORT (DATE) UTF-8 ALL"); got != "* SORT 2" {
		t.Errorf("UID SORT = %q, want UID 2", got)
	}
}

// The criteria select, the keys order: a message the search excludes is not in
// the answer at all.
func TestSortOrdersOnlyTheSearchedMessages(t *testing.T) {
	raws := []string{
		mailFrom("a@x", "Plan", "Sun, 1 Mar 2026 10:00:00 +0000", "a@example.com"),
		mailFrom("b@x", "Budget", "Sun, 1 Mar 2026 11:00:00 +0000", "b@example.com"),
	}
	conn, rd, _ := threadServerIn(t, raws)

	if got := sortLine(t, conn, rd, "a3", `SORT (DATE) UTF-8 SUBJECT "Budget"`); got != "* SORT 2" {
		t.Errorf("filtered SORT = %q, want only the searched message", got)
	}
}

// The third command on the shared counter: a body criterion over messages
// nobody can read excludes them, and says so under its own label.
func TestSortCountsMessagesItCouldNotRead(t *testing.T) {
	raws := []string{
		mailFrom("a@x", "One", "Sun, 1 Mar 2026 10:00:00 +0000", "a@example.com"),
		mailFrom("b@x", "Two", "Sun, 1 Mar 2026 11:00:00 +0000", "b@example.com"),
	}
	conn, rd, root := threadServerIn(t, raws)
	if n := makeStoredMessagesUnreadable(t, root); n != 2 {
		t.Fatalf("made %d messages unreadable, want 2", n)
	}

	before := unreadableCount(t, "sort")
	if got := sortLine(t, conn, rd, "a3", `SORT (DATE) UTF-8 TEXT "body"`); strings.TrimSpace(got) != "* SORT" {
		t.Errorf("SORT = %q, want no messages: a body criterion cannot match bytes nobody read", got)
	}
	if n := unreadableCount(t, "sort") - before; n != 2 {
		t.Errorf("counter for command=sort rose by %v, want 2", n)
	}
}
