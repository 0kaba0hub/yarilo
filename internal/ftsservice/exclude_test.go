package ftsservice

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// The setting has two spellings for two different questions: which role a
// mailbox plays, and what it is called. Both are pinned, because the first is
// what an operator reaches for and the second is what they fall back to when
// the folder has a name no convention covers.
func TestExclusionMatching(t *testing.T) {
	specialUse := map[string]string{
		"Sent":    `\Sent`,
		"Drafts":  `\Drafts`,
		"Trash":   `\Trash`,
		"Junk":    `\Junk`,
		"Archive": `\Archive`,
	}

	for _, tc := range []struct {
		name     string
		patterns []string
		folder   string
		excluded bool
	}{
		// Nothing configured excludes nothing — the setting must be inert by
		// default, since an over-broad default would silently empty an index.
		{"no patterns", nil, "Junk", false},

		{"flag matches its folder", []string{`\Junk`}, "Junk", true},
		{"flag does not match another folder", []string{`\Junk`}, "INBOX", false},
		{"flag is case-insensitive", []string{`\JUNK`}, "Junk", true},
		{"two flags", []string{`\Junk`, `\Trash`}, "Trash", true},

		{"exact name", []string{".EXPUNGED"}, ".EXPUNGED", true},
		{"wildcard names children", []string{".EXPUNGED/*"}, ".EXPUNGED/2026", true},
		{"wildcard does not name the parent", []string{".EXPUNGED/*"}, ".EXPUNGED", false},
		{"wildcard does not cross the separator", []string{".EXPUNGED/*"}, ".EXPUNGED/a/b", false},
		{"single-character wildcard", []string{"Temp?"}, "Temp1", true},
		{"single-character wildcard is not a star", []string{"Temp?"}, "Temp12", false},

		// Names are case-sensitive, as IMAP treats them.
		{"names are case-sensitive", []string{"Junk"}, "junk", false},

		// A flag pattern must not be read as a name, or `\Junk` would match a
		// folder literally called that and nothing else.
		{"flag is not a name match", []string{`\Junk`}, `\Junk`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewExclusion(tc.patterns, specialUse, "/")
			if got := e.Excludes(tc.folder); got != tc.excluded {
				t.Errorf("Excludes(%q) with %v = %v, want %v", tc.folder, tc.patterns, got, tc.excluded)
			}
		})
	}
}

// A folder whose special-use is configured to a different name still matches by
// role: that is what makes the flag form worth having over a name list.
func TestExclusionFollowsTheConfiguredSpecialUse(t *testing.T) {
	e := NewExclusion([]string{`\Junk`}, map[string]string{"Spam": `\Junk`}, "/")
	if !e.Excludes("Spam") {
		t.Error("a folder mapped to \\Junk was not excluded; the flag form is matching names rather than roles")
	}
	if e.Excludes("Junk") {
		t.Error("a folder named Junk was excluded although nothing maps it to the role")
	}
}

func TestExclusionEmpty(t *testing.T) {
	if !NewExclusion(nil, nil, "/").Empty() {
		t.Error("no patterns should be empty")
	}
	if NewExclusion([]string{`\Junk`}, nil, "/").Empty() {
		t.Error("a configured pattern should not be empty")
	}
	// Whitespace and blank entries are not patterns: a values file that wraps a
	// list over lines must not accidentally exclude everything.
	if !NewExclusion([]string{"", "  "}, nil, "/").Empty() {
		t.Error("blank entries should not count as patterns")
	}
}

// The half that matters operationally: exclusion applies to autoindexing and
// nothing else. An excluded mailbox must stay rebuildable and stay searchable,
// or the setting turns a folder that is merely un-pre-indexed into one that
// cannot be found in at all.
func TestExclusionAppliesToAutoindexOnly(t *testing.T) {
	s := &Service{
		exclude: NewExclusion([]string{`\Junk`}, map[string]string{"Junk": `\Junk`}, "/"),
		queue:   newQueue(),
	}
	junk := fts.MailboxRef{Name: "Junk"}

	if err := s.Index("u1", junk, 10, 0); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if got := s.queue.depth(); got != 0 {
		t.Errorf("autoindex queued %d jobs for an excluded mailbox, want 0", got)
	}

	// The search catch-up goes through Prepend, which must not consult the
	// exclusion at all.
	if err := s.Prepend("u1", junk, 10); err != nil {
		t.Fatalf("Prepend: %v", err)
	}
	if got := s.queue.depth(); got != 1 {
		t.Errorf("the search catch-up queued %d jobs for an excluded mailbox, want 1 — "+
			"an excluded folder is un-pre-indexed, not unsearchable", got)
	}
}

// And a mailbox that is not excluded still queues, or the test above would pass
// on a service that queues nothing at all.
func TestUnexcludedMailboxStillAutoindexes(t *testing.T) {
	s := &Service{
		exclude: NewExclusion([]string{`\Junk`}, map[string]string{"Junk": `\Junk`}, "/"),
		queue:   newQueue(),
	}
	if err := s.Index("u1", fts.MailboxRef{Name: "INBOX"}, 10, 0); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if got := s.queue.depth(); got != 1 {
		t.Errorf("INBOX queued %d jobs, want 1", got)
	}
}

// The separator is configuration, and path.Match hard-codes "/" as the boundary
// `*` may not cross. On a deployment using "." every comment in this file
// promised a boundary that was not there: a pattern meant for one folder also
// excluded its whole subtree, silently and more broadly than configured (#1062).
func TestExclusionHonoursTheConfiguredSeparator(t *testing.T) {
	for _, tc := range []struct {
		name      string
		separator string
		pattern   string
		folder    string
		excluded  bool
	}{
		// With ".", a star must stop at the dot.
		{"star does not cross a dot separator", ".", "Temp*", "Temp.Sub", false},
		{"star matches within the segment", ".", "Temp*", "Temporary", true},
		{"pattern names the children", ".", "EXPUNGED.*", "EXPUNGED.2026", true},
		{"pattern does not name grandchildren", ".", "EXPUNGED.*", "EXPUNGED.a.b", false},
		{"pattern does not name the parent", ".", "EXPUNGED.*", "EXPUNGED", false},
		{"exact name still matches", ".", "EXPUNGED", "EXPUNGED", true},

		// And the default is unchanged.
		{"star does not cross a slash separator", "/", "Temp*", "Temp/Sub", false},
		{"slash pattern names the children", "/", "EXPUNGED/*", "EXPUNGED/2026", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewExclusion([]string{tc.pattern}, nil, tc.separator)
			if got := e.Excludes(tc.folder); got != tc.excluded {
				t.Errorf("separator %q, pattern %q, folder %q = %v, want %v",
					tc.separator, tc.pattern, tc.folder, got, tc.excluded)
			}
		})
	}
}

// A malformed pattern matches nothing, and a pattern that matches nothing is
// indistinguishable from one that was never written.
//
// The log line is the whole of the difference, so the log line is what this
// asserts. Checking only the behaviour cannot tell "dropped at construction"
// from "kept and never matching" — both answer false to everything, which is
// how a typo would stay invisible.
func TestMalformedPatternIsReported(t *testing.T) {
	var logged bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(old)

	e := NewExclusion([]string{"[unclosed", "Junk"}, nil, "/")

	if !strings.Contains(logged.String(), "[unclosed") {
		t.Errorf("a malformed pattern was accepted without a word: %q", logged.String())
	}
	if e.Excludes("[unclosed") {
		t.Error("a malformed pattern was kept as a literal")
	}
	// And the rest of the list keeps working, or one typo would disable every
	// exclusion the operator wrote.
	if !e.Excludes("Junk") {
		t.Error("a valid pattern after a malformed one was dropped too")
	}
}
