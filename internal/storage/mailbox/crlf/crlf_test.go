package crlf_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/crlf"
)

func TestLineEndings(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"bare LF becomes CRLF", "a\nb\n", "a\r\nb\r\n"},
		{"CRLF is left alone", "a\r\nb\r\n", "a\r\nb\r\n"},
		{"mixed endings all end up CRLF", "a\r\nb\nc\r\n", "a\r\nb\r\nc\r\n"},
		{"a lone CR is not a line ending and is not touched", "a\rb", "a\rb"},
		{"an unterminated last line stays unterminated", "a\nb", "a\r\nb"},
		{"empty input", "", ""},
		{"nothing but line endings", "\n\n", "\r\n\r\n"},
		{"CR at the end with no LF after it", "a\n\r", "a\r\n\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := io.ReadAll(crlf.New(strings.NewReader(tc.in)))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("read %q, want %q", got, tc.want)
			}
		})
	}
}

// Wrapping twice must change nothing, because most bodies on this path need no
// conversion at all and the wrapper cannot know which.
func TestWrappingIsIdempotent(t *testing.T) {
	const in = "From: a@b\nSubject: s\r\n\r\nbody\nmore\n"
	once, err := io.ReadAll(crlf.New(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	twice, err := io.ReadAll(crlf.New(bytes.NewReader(once)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) {
		t.Errorf("second pass changed the bytes:\n once %q\ntwice %q", once, twice)
	}
}

// oneByteReader hands out a single byte per Read, so every CR and its LF land
// in different calls.
type oneByteReader struct{ s string }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.s == "" {
		return 0, io.EOF
	}
	p[0] = r.s[0]
	r.s = r.s[1:]
	return 1, nil
}

// A CR inserted at the end of the caller's buffer must not be handed out
// without its LF: a reader that split the pair would produce a corrupt line
// whenever the buffer boundary fell between them, which is a bug that only
// shows under a particular buffer size.
func TestTheInsertedPairSurvivesAnyBufferBoundary(t *testing.T) {
	const in = "a\nb\nc\n"
	const want = "a\r\nb\r\nc\r\n"

	for size := 1; size <= len(want)+2; size++ {
		var out bytes.Buffer
		c := crlf.New(&oneByteReader{s: in})
		buf := make([]byte, size)
		for {
			n, err := c.Read(buf)
			out.Write(buf[:n])
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("buffer %d: %v", size, err)
			}
		}
		if out.String() != want {
			t.Errorf("buffer %d produced %q, want %q", size, out.String(), want)
		}
	}
}
