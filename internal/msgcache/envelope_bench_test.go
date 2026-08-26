package msgcache

import (
	"fmt"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
)

// realisticEnvelope is the shape a mailing-list message has: a handful of
// recipients rather than one, which is what makes the address lists the bulk of
// the record.
func realisticEnvelope() *imaplib.Envelope {
	addrs := func(n int, host string) []imaplib.Address {
		out := make([]imaplib.Address, n)
		for i := range out {
			out[i] = imaplib.Address{
				Name:    fmt.Sprintf("Person Number %d", i),
				Mailbox: fmt.Sprintf("person.%d", i),
				Host:    host,
			}
		}
		return out
	}
	return &imaplib.Envelope{
		Date:      time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		Subject:   "Re: [list] a subject of the length subjects actually have",
		From:      addrs(1, "example.com"),
		Sender:    addrs(1, "example.com"),
		ReplyTo:   addrs(1, "lists.example.com"),
		To:        addrs(4, "example.org"),
		Cc:        addrs(2, "example.net"),
		InReplyTo: []string{"<parent-1@example.com>"},
		MessageID: "<msg-1@example.com>",
	}
}

func BenchmarkDecodeEnvelope(b *testing.B) {
	enc := encodeEnvelope(realisticEnvelope())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := decodeEnvelope(enc); !ok {
			b.Fatal("decode failed")
		}
	}
}

// Ten thousand of them, which is what one THREAD or SORT over the field fixture
// decodes.
func BenchmarkDecodeEnvelope10k(b *testing.B) {
	enc := encodeEnvelope(realisticEnvelope())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 10000; j++ {
			if _, ok := decodeEnvelope(enc); !ok {
				b.Fatal("decode failed")
			}
		}
	}
}
