package mimesalvage

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func bodyOf(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// A message that parses as it stands must come back untouched: the repair path
// is for damage, and a caller reading Salvaged has to be able to trust it.
func TestHealthyMessageIsNotTouched(t *testing.T) {
	raw := "From: alice@example.com\r\nSubject: hello\r\n\r\nplain body\r\n"
	e, res, err := Read(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.Salvaged || res.DroppedHeaderLines != 0 {
		t.Errorf("result = %+v, want an untouched message", res)
	}
	if got := e.Header.Get("Subject"); got != "hello" {
		t.Errorf("subject = %q", got)
	}
	if got := bodyOf(t, e.Body); got != "plain body" {
		t.Errorf("body = %q", got)
	}
}

// The damaged shapes, and what must survive each. The point of every row is
// that the message comes back as a message -- headers addressable, body
// separate -- rather than as a heap of bytes with the header noise in it.
func TestDamagedMessagesAreRepaired(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantFrom    string
		wantSubject string
		wantBody    string
		wantDropped int
	}{
		{
			// The damage is in the middle: the fields around it survive.
			name:        "a line that is not a header, mid-block",
			raw:         "From: alice@example.com\r\nJustSomeText\r\nSubject: hi\r\n\r\nbody wordzz\r\n",
			wantFrom:    "alice@example.com",
			wantSubject: "hi",
			wantBody:    "body wordzz",
			wantDropped: 1,
		},
		{
			name:        "a space inside the field name",
			raw:         "From: alice@example.com\r\nBad Header: v\r\nSubject: hi\r\n\r\nbody wordzz\r\n",
			wantFrom:    "alice@example.com",
			wantSubject: "hi",
			wantBody:    "body wordzz",
			wantDropped: 1,
		},
		{
			name:        "a first line that is not a header",
			raw:         "NoColonHeaderLine\r\nFrom: bob@example.com\r\nSubject: salvaged\r\n\r\nbody wordzz\r\n",
			wantFrom:    "bob@example.com",
			wantSubject: "salvaged",
			wantBody:    "body wordzz",
			wantDropped: 1,
		},
		{
			name: "a folded value whose first line was dropped",
			raw: "Broken line here\r\n continuation of the broken one\r\n" +
				"Subject: kept\r\n\r\nbody wordzz\r\n",
			wantSubject: "kept",
			wantBody:    "body wordzz",
			wantDropped: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, res, err := Read(strings.NewReader(tc.raw))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if !res.Salvaged {
				t.Error("a damaged message was reported as untouched")
			}
			if res.DroppedHeaderLines != tc.wantDropped {
				t.Errorf("dropped %d header lines, want %d", res.DroppedHeaderLines, tc.wantDropped)
			}
			if got := e.Header.Get("From"); got != tc.wantFrom {
				t.Errorf("from = %q, want %q", got, tc.wantFrom)
			}
			if got := e.Header.Get("Subject"); got != tc.wantSubject {
				t.Errorf("subject = %q, want %q", got, tc.wantSubject)
			}
			if got := bodyOf(t, e.Body); got != tc.wantBody {
				t.Errorf("body = %q, want %q", got, tc.wantBody)
			}
		})
	}
}

// A message with no blank line between headers and body: the parser refuses
// it, and the lines that cannot be headers are what the message says. Losing
// them would leave a message with a subject and nothing to search.
func TestBodyIsRecoveredWhenNoBoundaryExists(t *testing.T) {
	raw := "From: a@example.com\r\nSubject: nb\r\nbody wordzz right after\r\n"
	e, res, err := Read(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !res.Salvaged {
		t.Error("reported as untouched")
	}
	if got := e.Header.Get("Subject"); got != "nb" {
		t.Errorf("subject = %q, want nb", got)
	}
	if got := bodyOf(t, e.Body); !strings.Contains(got, "body wordzz") {
		t.Errorf("body = %q, want the text that followed the headers", got)
	}
}

// The structure survives the repair: a multipart message whose Content-Type
// line was broken comes back walkable, with its parts intact. This is the
// difference between repairing a message and giving up on it -- the parts
// carry their own types and encodings, so an attachment stays an attachment
// instead of becoming base64 in the middle of the text.
func TestRepairKeepsTheStructure(t *testing.T) {
	raw := "From: a@example.com\r\nNotAHeaderAtAll\r\nSubject: mixed\r\n" +
		"Content-Type: multipart/mixed; boundary=BOUND\r\n\r\n" +
		"--BOUND\r\nContent-Type: text/plain\r\n\r\nfirst partzz\r\n" +
		"--BOUND\r\nContent-Type: text/plain\r\n\r\nsecond partzz\r\n--BOUND--\r\n"

	e, res, err := Read(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !res.Salvaged {
		t.Error("the damaged message was reported as untouched")
	}
	mt, _, err := e.Header.ContentType()
	if err != nil || mt != "multipart/mixed" {
		t.Fatalf("content type = %q (%v), want multipart/mixed", mt, err)
	}
	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("the repaired message is not walkable as multipart")
	}
	var parts []string
	for {
		p, perr := mr.NextPart()
		if perr != nil {
			break
		}
		parts = append(parts, bodyOf(t, p.Body))
	}
	if len(parts) != 2 || parts[0] != "first partzz" || parts[1] != "second partzz" {
		t.Errorf("parts = %q, want both bodies", parts)
	}
}

// Input with nothing that could be a header and no boundary is not a damaged
// message, and returning an empty one would hide that from every caller.
func TestNothingToRepairIsAnError(t *testing.T) {
	_, _, err := Read(strings.NewReader("\x00\x01 not mail at all, no colon, no blank line"))
	if !errors.Is(err, ErrUnsalvageable) {
		t.Errorf("err = %v, want ErrUnsalvageable", err)
	}
}

// A header line long enough to be a denial of service is dropped, not read
// whole: the line is discarded either way, so reading it into memory buys
// nothing and costs whatever the sender chose.
func TestOverlongHeaderLineIsBounded(t *testing.T) {
	raw := "X-Huge " + strings.Repeat("A", 1<<20) + "\r\nSubject: kept\r\n\r\nbody wordzz\r\n"
	e, res, err := Read(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.DroppedHeaderLines != 1 {
		t.Errorf("dropped %d, want the overlong line", res.DroppedHeaderLines)
	}
	if got := e.Header.Get("Subject"); got != "kept" {
		t.Errorf("subject = %q, want the header after the overlong line", got)
	}
}
