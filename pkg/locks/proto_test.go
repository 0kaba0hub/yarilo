package locks

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestWriteFieldsRoundtrip(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		want   string
	}{
		{"version", []string{cmdVersion, protocolVersion}, "VERSION\t1\n"},
		{"lock", []string{cmdLock, "mbox:alice:INBOX", "owner-1", "30000"}, "LOCK\tmbox:alice:INBOX\towner-1\t30000\n"},
		{"event", []string{respEvent, "mbox:alice:INBOX", "delivered", "msg-9"}, "EVENT\tmbox:alice:INBOX\tdelivered\tmsg-9\n"},
		{"single", []string{respOK}, "OK\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeFields(&buf, tc.fields...); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
			got, err := newReader(&buf).readFields()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(got) != len(tc.fields) {
				t.Fatalf("field count: want %d got %d", len(tc.fields), len(got))
			}
			for i := range got {
				if got[i] != tc.fields[i] {
					t.Fatalf("field %d: want %q got %q", i, tc.fields[i], got[i])
				}
			}
		})
	}
}

func TestWriteFieldsRejectsTabOrLF(t *testing.T) {
	cases := []string{"a\tb", "a\nb", "tab\there", "lf\nhere"}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeFields(&buf, c); !errors.Is(err, ErrProtocol) {
				t.Fatalf("want ErrProtocol, got %v", err)
			}
		})
	}
}

func TestReaderRejectsOverLongLine(t *testing.T) {
	long := strings.Repeat("a", maxLineLen+1) + "\n"
	r := newReader(strings.NewReader(long))
	_, err := r.readFields()
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("want ErrProtocol, got %v", err)
	}
}

func TestReaderEOF(t *testing.T) {
	r := newReader(strings.NewReader(""))
	_, err := r.readFields()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestReaderRejectsEmptyLine(t *testing.T) {
	r := newReader(strings.NewReader("\n"))
	_, err := r.readFields()
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("want ErrProtocol for empty line, got %v", err)
	}
}

func TestFormatAndParseTTL(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"30s", 30 * time.Second, "30000"},
		{"500ms", 500 * time.Millisecond, "500"},
		{"1ms", time.Millisecond, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := formatTTL(tc.in)
			if err != nil {
				t.Fatalf("format: %v", err)
			}
			if got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
			parsed, err := parseTTL(got)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if parsed != tc.in {
				t.Fatalf("roundtrip: want %v, got %v", tc.in, parsed)
			}
		})
	}
}

func TestFormatTTLRejectsNonPositive(t *testing.T) {
	if _, err := formatTTL(0); err == nil {
		t.Fatal("expected error for zero ttl")
	}
	if _, err := formatTTL(-time.Second); err == nil {
		t.Fatal("expected error for negative ttl")
	}
}

func TestParseTTLRejectsGarbage(t *testing.T) {
	cases := []string{"", "abc", "-1", "0"}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if _, err := parseTTL(c); err == nil {
				t.Fatalf("want error for %q", c)
			}
		})
	}
}

func TestResourceKeys(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"idx", IndexKey("alice"), "idx:alice"},
		{"mbox", MailboxKey("alice", "INBOX"), "mbox:alice:INBOX"},
		{"deliver", DeliverKey("alice", "INBOX"), "deliver:alice:INBOX"},
		{"sieve", SieveScriptsKey("alice"), "sieve:alice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, tc.got)
			}
		})
	}
}
