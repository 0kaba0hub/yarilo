package msgcache

import (
	"reflect"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
)

// Round-trip with inputs that distinguish: UTF-8 in names and subject, an
// empty address list next to a populated one (nil vs empty must not matter
// to the wire), multiple recipients, and a zero date.
func TestEnvelopeCodecRoundTrip(t *testing.T) {
	cases := []*imaplib.Envelope{
		{
			Date:    time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC),
			Subject: "Тема з UTF-8 — і тире",
			From:    []imaplib.Address{{Name: "Аліса Ліддел", Mailbox: "alice", Host: "example.com"}},
			To: []imaplib.Address{
				{Mailbox: "bob", Host: "example.com"},
				{Name: "Carol", Mailbox: "carol", Host: "example.org"},
			},
			InReplyTo: []string{"<a@x>", "<b@y>"},
			MessageID: "<m1@example.com>",
		},
		{}, // everything empty, date zero
		{Subject: "only-subject", Bcc: []imaplib.Address{{Mailbox: "hidden", Host: "h"}}},
	}
	for i, env := range cases {
		got, ok := decodeEnvelope(encodeEnvelope(env))
		if !ok {
			t.Fatalf("case %d: decode failed", i)
		}
		if !env.Date.IsZero() && !got.Date.Equal(env.Date) {
			t.Errorf("case %d: date %v != %v", i, got.Date, env.Date)
		}
		got.Date, env.Date = time.Time{}, time.Time{}
		if !reflect.DeepEqual(got, env) {
			t.Errorf("case %d:\n got %+v\nwant %+v", i, got, env)
		}
	}
}

// Malformation is a miss, never a panic or an error surfaced upward.
func TestEnvelopeCodecMalformedIsAMiss(t *testing.T) {
	good := encodeEnvelope(&imaplib.Envelope{Subject: "s"})
	for _, b := range [][]byte{
		nil,
		{},
		{99},               // unknown version
		good[:len(good)-2], // truncated
		append(append([]byte{}, good[:5]...), 0xff, 0xff, 0xff, 0xff), // implausible length
	} {
		if env, ok := decodeEnvelope(b); ok {
			t.Errorf("malformed %v decoded to %+v", b[:min(8, len(b))], env)
		}
	}
}
