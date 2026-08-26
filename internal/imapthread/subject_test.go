package imapthread

import (
	"fmt"
	"strings"
	"testing"
)

// RFC 5256 §2.1 says servers MUST use exactly this algorithm, and the reason
// is in the specification: a disconnected client runs it too, so a server that
// normalises subjects its own way sorts a mailbox differently from the client
// showing it. The rows below are the ABNF's own distinctions -- each one is a
// case the grammar separates, not a subject someone happened to think of.
func TestBaseSubjectFollowsTheGrammar(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  string
		refwd bool
	}{
		{"plain", "Plan for Friday", "Plan for Friday", false},
		{"reply", "Re: Plan for Friday", "Plan for Friday", true},
		{"case is not significant", "RE: Plan", "Plan", true},
		{"forward, both spellings", "Fwd: Plan", "Plan", true},
		{"fw without the d", "FW: Plan", "Plan", true},
		{"stacked prefixes", "Re: Fwd: Re: Plan", "Plan", true},
		{"blob before the keyword", "[list] Re: Plan", "Plan", true},
		{"blob between keyword and colon", "Re[2]: Plan", "Plan", true},
		{"space before the colon", "Re : Plan", "Plan", true},
		{"trailing (fwd)", "Plan (fwd)", "Plan", true},
		{"repeated trailers", "Plan (fwd) (fwd) ", "Plan", true},
		{"the whole subject wrapped", "[fwd: Plan]", "Plan", true},
		{"wrapped around a reply", "[fwd: Re: Plan]", "Plan", true},
		{"list tag alone", "[list] Plan", "Plan", false},

		// The distinctions that separate this from "strip anything that looks
		// like a prefix", which is what a hand-rolled version does.
		{
			// (4): removing the blob must leave a non-empty base, so a subject
			// that is nothing but a tag IS its own base subject. Stripping it
			// would file every "[nightly]" build report under one conversation.
			name: "a subject that is only a blob",
			in:   "[nightly]",
			want: "[nightly]",
		},
		{
			// "Rest" begins with "re" and is not a reply: the grammar requires
			// the colon.
			name: "a word that starts with re",
			in:   "Rest of the plan",
			want: "Rest of the plan",
		},
		{
			// BLOBCHAR excludes brackets, so this blob never closes and the
			// text stands as written.
			name: "an unterminated blob",
			in:   "[list Plan",
			want: "[list Plan",
		},
		{
			// Step (6) needs the wrapper to end the subject. Here it does not,
			// so the leading "[fwd: Plan]" is an ordinary blob instead.
			name: "a wrapper that does not wrap everything",
			in:   "[fwd: Plan] and more",
			want: "and more",
		},
		{"tabs and runs of spaces collapse", "Re:\tPlan   for  Friday", "Plan for Friday", true},
		{"empty stays empty", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, refwd := BaseSubject(tc.in)
			if got != tc.want {
				t.Errorf("BaseSubject(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if refwd != tc.refwd {
				t.Errorf("BaseSubject(%q) reported reply/forward = %v, want %v", tc.in, refwd, tc.refwd)
			}
		})
	}
}

// slowCollapse is what collapse does when it decides there is work: kept here
// so the fast path can be checked against it rather than against a description
// of it.
func slowCollapse(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\t', '\r', '\n', ' ':
			space = true
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteByte(c)
		}
	}
	return b.String()
}

// The fast path skips the rebuild when it decides nothing would change, so the
// property it rests on is exactly: needsCollapse(s) == (slowCollapse(s) != s).
//
// A predicate that says "no work" where work was needed loses a
// transformation the specification asks for; one that says "work" where none
// was needed only costs an allocation. So the rows below are chosen to press
// the first direction -- including a malformed UTF-8 subject, which is where
// the two used to disagree: iterating runes rewrote each bad byte as U+FFFD,
// a normalisation nobody asked for and the fast path never performed.
func TestCollapseFastPathAgreesWithTheSlowOne(t *testing.T) {
	subjects := []string{
		"",
		"Plan",
		"Plan for Friday",
		" leading space",
		"trailing space ",
		"double  space",
		"tab\tinside",
		"crlf\r\nfolded",
		"   ",
		"\t",
		"ends with tab\t",
		"поточні справи",  // non-ASCII, nothing to collapse
		"поточні  справи", // non-ASCII with a double space
		"\xff\xfe broken", // invalid UTF-8
		"broken\xff",      // invalid UTF-8 at the end
		"multi   space   subject",
	}
	for _, s := range subjects {
		t.Run(fmt.Sprintf("%q", s), func(t *testing.T) {
			want := slowCollapse(s)
			if got := collapse(s); got != want {
				t.Errorf("collapse(%q) = %q, the full pass gives %q", s, got, want)
			}
			if needsCollapse(s) != (want != s) {
				t.Errorf("needsCollapse(%q) = %v, but the full pass %s the string",
					s, needsCollapse(s), map[bool]string{true: "changes", false: "leaves"}[want != s])
			}
		})
	}
}
