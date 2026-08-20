package imap_test

import (
	"fmt"
	"strings"
	"testing"
)

// #1377: a message with a header line that has no colon was stored, reported
// its real RFC822.SIZE, and then fetched back empty in every section. A client
// is told a message is there and cannot read it -- and mail like this arrives.
//
// Asserted over the wire on a real session, because the contradiction was
// between two answers to the same client.
func TestFetchOfMessageWithMalformedHeader(t *testing.T) {
	c := selectedSession(t)
	body := "From: a@b\r\nSubject: t\r\nX ID here\r\nX-Note: kept\r\n\r\nhello\r\n"
	fmt.Fprintf(c.conn, "b1 APPEND INBOX {%d+}\r\n%s\r\n", len(body), body)
	for {
		if strings.HasPrefix(c.readLine(), "b1 ") {
			break
		}
	}

	whole := c.cmd("FETCH 1 (RFC822.SIZE BODY.PEEK[])")
	if !strings.Contains(whole, fmt.Sprintf("RFC822.SIZE %d", len(body))) {
		t.Errorf("size not reported: %q", whole)
	}
	if !strings.Contains(whole, "X ID here") || !strings.Contains(whole, "hello") {
		t.Errorf("BODY[] did not return the stored message: %q", whole)
	}
	if strings.Contains(whole, "BODY[] {0}") {
		t.Errorf("BODY[] is empty while the size says otherwise: %q", whole)
	}

	if got := c.cmd("FETCH 1 (BODY.PEEK[TEXT])"); !strings.Contains(got, "hello") {
		t.Errorf("BODY[TEXT] = %q", got)
	}
	if got := c.cmd("FETCH 1 (BODY.PEEK[HEADER])"); !strings.Contains(got, "Subject: t") {
		t.Errorf("BODY[HEADER] = %q", got)
	}
	got := c.cmd("FETCH 1 (BODY.PEEK[HEADER.FIELDS (SUBJECT)])")
	if !strings.Contains(got, "Subject: t") || strings.Contains(got, "From: a@b") {
		t.Errorf("BODY[HEADER.FIELDS (SUBJECT)] = %q", got)
	}
}
