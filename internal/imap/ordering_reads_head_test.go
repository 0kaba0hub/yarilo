package imap

import (
	"os"
	"strings"
	"testing"
)

// The ordering path must ask the cache for the head, not for the envelope.
//
// Both answers are correct, which is why this is a guard and not a test: a
// future edit that reaches for Envelope() here would compile, pass every test,
// return the right order, and quietly put back the six address lists that were
// 30.6% of the objects a SORT (DATE) allocated in the field (#1490). Nothing
// about the output would say so.
//
// Scoped to the function rather than the file: FETCH's own path is in this
// package too and must keep reading the whole envelope.
func TestOrderingReadsTheHeadNotTheWholeEnvelope(t *testing.T) {
	src, err := os.ReadFile("thread.go")
	if err != nil {
		t.Fatalf("read thread.go: %v", err)
	}
	body := functionBody(t, string(src), "func (s *session) orderingMessage(")

	for _, banned := range []string{"envCache.Envelope(", "envCache.EnvelopeAndReferences("} {
		if strings.Contains(body, banned) {
			t.Errorf("orderingMessage calls %s; ordering compares the date, the subject and the first mailbox of "+
				"From/To/Cc, so it must use the head decode and leave the address lists unbuilt", banned)
		}
	}
	// A guard that finds nothing to guard has stopped guarding.
	if !strings.Contains(body, "envCache.HeadAndReferences(") || !strings.Contains(body, "envCache.Head(") {
		t.Error("orderingMessage no longer calls the head reads; this guard is watching a function that moved")
	}
}

// functionBody returns the source between a function's opening line and the
// brace that closes it. Textual rather than an AST walk because what is being
// asserted is a call by name, and the name is what an edit would change.
func functionBody(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("cannot find %q; the guard is watching a function that moved or was renamed", signature)
	}
	depth := 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces after %q", signature)
	return ""
}
