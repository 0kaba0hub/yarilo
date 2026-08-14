package jmap

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// RFC 8621 §4.1.1 maps the five IMAP system flags onto $-keywords, and carries
// every other IMAP keyword through as it stands. A mapping that knows only the
// five answers the standard list correctly and silently loses a client's own
// labels (#1278), so both halves are pinned here, in the function that maps.
func TestKeywordsOfCarriesCustomKeywordsThrough(t *testing.T) {
	tests := []struct {
		name  string
		meta  *mailbox.MessageMeta
		want  []string
		unwan []string
	}{
		{
			name: "system flags map to their keywords",
			meta: &mailbox.MessageMeta{Flags: []string{`\Seen`, `\Flagged`, `\Answered`, `\Draft`, `\Deleted`}},
			want: []string{"$seen", "$flagged", "$answered", "$draft", "$deleted"},
		},
		{
			name: "a custom keyword survives beside a system flag",
			meta: &mailbox.MessageMeta{Flags: []string{`\Seen`}, Keywords: []string{"$smokelabel"}},
			want: []string{"$seen", "$smokelabel"},
		},
		{
			name: "keyword case is folded, as JMAP keywords are case-insensitive",
			meta: &mailbox.MessageMeta{Keywords: []string{"$SmokeLabel"}},
			want: []string{"$smokelabel"},
		},
		{
			name:  "session state is not a keyword",
			meta:  &mailbox.MessageMeta{Flags: []string{`\Seen`, `\Recent`}},
			want:  []string{"$seen"},
			unwan: []string{`\recent`, "$recent"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := keywordsOf(tc.meta)
			for _, kw := range tc.want {
				if !got[kw] {
					t.Errorf("keyword %q missing from %v", kw, got)
				}
			}
			for _, kw := range tc.unwan {
				if got[kw] {
					t.Errorf("keyword %q should not be reported (%v)", kw, got)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("keywords = %v, want exactly %v", got, tc.want)
			}
		})
	}
}
