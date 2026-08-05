package ftsservice

import (
	"path"
	"strings"
)

// Exclusion decides which mailboxes autoindexing skips.
//
// The two folders it exists for are junk and trash: high volume, large and
// attachment-heavy, almost never searched, and indexing is dominated by
// tokenisation. That is the cost of keeping current an index of content nobody
// queries.
//
// A pattern is either a special-use flag written with its backslash (`\Junk`)
// or a mailbox name with `*` and `?` wildcards (`.EXPUNGED/*`). Names are
// matched case-sensitively, as IMAP treats them everywhere else except INBOX.
type Exclusion struct {
	flags    map[string]bool
	patterns []string
	// roleOf maps a folder name to its special-use attribute, from the
	// configured defaults.
	roleOf map[string]string
}

// NewExclusion builds a matcher. specialUse maps folder name to attribute, as
// imap_special_use_defaults carries it.
func NewExclusion(patterns []string, specialUse map[string]string) *Exclusion {
	e := &Exclusion{flags: map[string]bool{}, roleOf: map[string]string{}}
	for folder, attr := range specialUse {
		e.roleOf[folder] = strings.ToLower(attr)
	}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
		case strings.HasPrefix(p, `\`):
			e.flags[strings.ToLower(p)] = true
		default:
			e.patterns = append(e.patterns, p)
		}
	}
	return e
}

// Excludes reports whether autoindexing skips this mailbox.
func (e *Exclusion) Excludes(folder string) bool {
	if e == nil || (len(e.flags) == 0 && len(e.patterns) == 0) {
		return false
	}
	if role, ok := e.roleOf[folder]; ok && e.flags[role] {
		return true
	}
	for _, p := range e.patterns {
		// path.Match is the shell-glob semantics the setting describes: `*`
		// does not cross the separator, so `.EXPUNGED/*` names the children and
		// `.EXPUNGED` names the folder itself — which is why the example in
		// #1051 lists both.
		if ok, err := path.Match(p, folder); err == nil && ok {
			return true
		}
	}
	return false
}

// Empty reports whether nothing is excluded, so a caller can skip the check
// and the logging around it.
func (e *Exclusion) Empty() bool {
	return e == nil || (len(e.flags) == 0 && len(e.patterns) == 0)
}
