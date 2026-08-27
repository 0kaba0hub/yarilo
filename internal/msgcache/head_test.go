package msgcache

import (
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
)

// The head decode must answer exactly what the full decode answers for the
// fields it claims, on every shape an envelope can take. It is a second reader
// of one format, and two readers of one format are two chances to disagree --
// so this compares them rather than asserting expected values, which would let
// both drift together.
func TestHeadAgreesWithTheFullDecode(t *testing.T) {
	addr := func(name, mailbox, host string) imaplib.Address {
		return imaplib.Address{Name: name, Mailbox: mailbox, Host: host}
	}
	tests := []struct {
		name string
		env  *imaplib.Envelope
	}{
		{"empty", &imaplib.Envelope{}},
		{"subject only", &imaplib.Envelope{Subject: "hello"}},
		{
			name: "one address in each wanted list",
			env: &imaplib.Envelope{
				Date:    time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
				Subject: "Re: something",
				From:    []imaplib.Address{addr("A", "a", "example.com")},
				To:      []imaplib.Address{addr("B", "b", "example.org")},
				Cc:      []imaplib.Address{addr("C", "c", "example.net")},
			},
		},
		{
			name: "several addresses — only the first mailbox is taken",
			env: &imaplib.Envelope{
				From: []imaplib.Address{addr("A", "first", "example.com"), addr("Z", "second", "example.com")},
				To:   []imaplib.Address{addr("B", "b1", "x"), addr("B2", "b2", "y"), addr("B3", "b3", "z")},
				Cc:   []imaplib.Address{addr("C", "c1", "x"), addr("C2", "c2", "y")},
			},
		},
		{
			name: "the skipped lists are populated — they must not shift the walk",
			env: &imaplib.Envelope{
				Subject: "with sender, reply-to and bcc",
				Sender:  []imaplib.Address{addr("S", "s", "example.com"), addr("S2", "s2", "example.com")},
				ReplyTo: []imaplib.Address{addr("R", "r", "example.com")},
				Bcc:     []imaplib.Address{addr("X", "x", "example.com"), addr("Y", "y", "example.com")},
				From:    []imaplib.Address{addr("A", "a", "example.com")},
				To:      []imaplib.Address{addr("B", "b", "example.org")},
				Cc:      []imaplib.Address{addr("C", "c", "example.net")},
			},
		},
		{
			name: "empty strings inside an address",
			env: &imaplib.Envelope{
				From: []imaplib.Address{addr("", "", "")},
				To:   []imaplib.Address{addr("", "only-mailbox", "")},
			},
		},
		{
			name: "in-reply-to with several entries",
			env: &imaplib.Envelope{
				InReplyTo: []string{"<a@x>", "<b@x>", "<c@x>"},
				MessageID: "<self@x>",
			},
		},
		{
			name: "no in-reply-to at all",
			env:  &imaplib.Envelope{MessageID: "<self@x>"},
		},
		{
			name: "non-ASCII, which is bytes to both readers",
			env: &imaplib.Envelope{
				Subject: "Привіт, це тема",
				From:    []imaplib.Address{addr("Ім'я", "поштова.скринька", "приклад.укр")},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc := encodeEnvelope(tt.env)
			full, ok := decodeEnvelope(enc)
			if !ok {
				t.Fatal("full decode refused its own encoding")
			}
			head, ok := decodeHead(enc)
			if !ok {
				t.Fatal("head decode refused the same bytes the full decode read")
			}
			if !head.Date.Equal(full.Date) {
				t.Errorf("Date = %v, full decode says %v", head.Date, full.Date)
			}
			for _, f := range []struct {
				name       string
				head, full string
			}{
				{"Subject", head.Subject, full.Subject},
				{"From", head.From, firstMailbox(full.From)},
				{"To", head.To, firstMailbox(full.To)},
				{"Cc", head.Cc, firstMailbox(full.Cc)},
				{"MessageID", head.MessageID, full.MessageID},
			} {
				if f.head != f.full {
					t.Errorf("%s = %q, full decode says %q", f.name, f.head, f.full)
				}
			}
			if len(head.InReplyTo) != len(full.InReplyTo) {
				t.Fatalf("InReplyTo has %d entries, full decode says %d", len(head.InReplyTo), len(full.InReplyTo))
			}
			for i := range head.InReplyTo {
				if head.InReplyTo[i] != full.InReplyTo[i] {
					t.Errorf("InReplyTo[%d] = %q, full decode says %q", i, head.InReplyTo[i], full.InReplyTo[i])
				}
			}
		})
	}
}

func firstMailbox(addrs []imaplib.Address) string {
	if len(addrs) == 0 {
		return ""
	}
	return addrs[0].Mailbox
}

// A truncated record must be refused, not read past. The head decode skips
// where the full one builds, and a skip that trusts its length prefix would
// walk off the end of a record a disk gave back short.
func TestHeadRefusesATruncatedRecord(t *testing.T) {
	enc := encodeEnvelope(realisticEnvelope())
	for n := 0; n < len(enc); n++ {
		if _, ok := decodeHead(enc[:n]); ok {
			t.Errorf("head decode accepted a record truncated to %d of %d bytes", n, len(enc))
		}
	}
}

func BenchmarkDecodeHead(b *testing.B) {
	enc := encodeEnvelope(realisticEnvelope())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := decodeHead(enc); !ok {
			b.Fatal("decode failed")
		}
	}
}

// A message with no Message-ID is a hit, not a miss.
//
// The first version of HeadAndReferences reported success as
// `head.MessageID != ""`, which reads as "did the decode produce anything" and
// is not: a message whose header carried no Message-ID would have been called a
// miss and reopened on every SORT and every THREAD, for ever, while the cache
// held a perfectly good record for it. Nothing about the output would differ --
// only the work.
//
// Asked of the Handle rather than of decodeHead, because the defect was in what
// "ok" meant, not in the bytes.
func TestAMessageWithNoMessageIDIsAHit(t *testing.T) {
	tests := []struct {
		name string
		env  *imaplib.Envelope
	}{
		{"no message id", &imaplib.Envelope{Subject: "Plan", Date: time.Unix(1770000000, 0).UTC()}},
		{"no message id and nothing else", &imaplib.Envelope{}},
		{"with a message id, the ordinary case", &imaplib.Envelope{Subject: "Plan", MessageID: "<a@x>"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx, f, m := compatFolder(t)
			fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
			if fc == nil {
				t.Fatal("cache unavailable")
			}
			fc.StoreEnvelope(m, tt.env)
			fc.StoreReferences(m, []string{"<root@x>"})
			fc.Close()

			read := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
			if read == nil {
				t.Fatal("cache unavailable")
			}
			defer read.Close()
			m = reread(t, idx, f.ID, m.UID)

			head, refs, ok := read.HeadAndReferences(m)
			if !ok {
				t.Fatal("the cached record reads as a miss, so every command would reopen the message")
			}
			if head.Subject != tt.env.Subject {
				t.Errorf("Subject = %q, want %q", head.Subject, tt.env.Subject)
			}
			if len(refs) != 1 || refs[0] != "<root@x>" {
				t.Errorf("References = %v, want [<root@x>]", refs)
			}
			if _, ok := read.Head(m); !ok {
				t.Error("Head reads the same record as a miss")
			}
		})
	}
}
