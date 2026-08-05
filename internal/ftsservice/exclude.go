package ftsservice

import (
	"log/slog"
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
	// separator is the hierarchy delimiter the deployment uses. Both the
	// pattern and the folder are rewritten to "/" before matching, because
	// path.Match hard-codes that as the boundary `*` may not cross — so a
	// deployment with "." would have had `*` crossing the separator while
	// every comment promised it did not.
	separator string
}

// NewExclusion builds a matcher.
//
// specialUse maps folder name to attribute, as imap_special_use_defaults
// carries it; separator is the hierarchy delimiter, empty meaning "/".
//
// A malformed pattern is dropped with a log line rather than silently ignored
// or fatal: ignoring it makes a typo inert with no way to notice, and refusing
// to start takes a mail server down over an exclusion list. Dropping it indexes
// more than intended, which is the recoverable direction.
func NewExclusion(patterns []string, specialUse map[string]string, separator string) *Exclusion {
	if separator == "" {
		separator = "/"
	}
	e := &Exclusion{flags: map[string]bool{}, roleOf: map[string]string{}, separator: separator}
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
			normalised := e.normalise(p)
			if _, err := path.Match(normalised, "probe"); err != nil {
				slog.Error("fts: autoindex exclusion pattern is malformed and will not match anything",
					"pattern", p, "err", err)
				continue
			}
			e.patterns = append(e.patterns, normalised)
		}
	}
	return e
}

// normalise rewrites the configured separator to "/" so path.Match treats it as
// the boundary. A folder name cannot contain "/" on a deployment whose
// separator is something else -- it is the filesystem path separator -- so the
// rewrite cannot collide with a literal.
func (e *Exclusion) normalise(s string) string {
	if e.separator == "/" {
		return s
	}
	return strings.ReplaceAll(s, e.separator, "/")
}

// Excludes reports whether autoindexing skips this mailbox.
func (e *Exclusion) Excludes(folder string) bool {
	if e == nil || (len(e.flags) == 0 && len(e.patterns) == 0) {
		return false
	}
	if role, ok := e.roleOf[folder]; ok && e.flags[role] {
		return true
	}
	name := e.normalise(folder)
	for _, p := range e.patterns {
		// `*` does not cross the separator, so `EXPUNGED<sep>*` names the
		// children and `EXPUNGED` names the folder itself — which is why the
		// example in #1051 lists both. Patterns were checked and normalised at
		// construction, so an error here is impossible rather than ignored.
		if ok, _ := path.Match(p, name); ok {
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
