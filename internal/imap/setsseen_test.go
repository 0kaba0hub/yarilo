package imap

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
)

// What decides the \Seen write also decides the sharing, so it is asserted here
// rather than through either caller (#1673).
func TestSetsSeenDistinguishesPeek(t *testing.T) {
	body := func(peek bool) *imaplib.FetchOptions {
		return &imaplib.FetchOptions{
			BodySection: []*imaplib.FetchItemBodySection{{Peek: peek}},
		}
	}
	if !setsSeen(body(false)) {
		t.Error("BODY[] does not set \\Seen, so a FETCH that writes a flag would share the folder key")
	}
	if setsSeen(body(true)) {
		t.Error("BODY.PEEK[] sets \\Seen, so every peeking FETCH takes the folder key exclusively")
	}
	if setsSeen(&imaplib.FetchOptions{Envelope: true}) {
		t.Error("an ENVELOPE-only FETCH sets \\Seen")
	}
	// One peeking section beside one that does not: the command sets \Seen.
	mixed := &imaplib.FetchOptions{BodySection: []*imaplib.FetchItemBodySection{{Peek: true}, {Peek: false}}}
	if !setsSeen(mixed) {
		t.Error("a command with one non-peeking section does not set \\Seen")
	}
}
