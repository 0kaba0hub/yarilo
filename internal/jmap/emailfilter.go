package jmap

import (
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// filterEvaluator decides which messages a filter matches. It exists as an
// interface so the full-text conditions can be added by supplying another
// evaluator rather than by editing the query body: a condition an evaluator
// cannot answer is named, not ignored, and the query refuses with that name.
//
// The three methods are the whole contract:
//
//   - unsupported reports the conditions this evaluator cannot answer, before
//     any storage is read, so a refusal costs nothing.
//   - prepare runs once per folder in scope, which is where an evaluator that
//     needs an external index does its lookup for that folder.
//   - match answers per message, using whatever prepare resolved.
type filterEvaluator interface {
	unsupported(f *jmapcore.EmailFilter) []string
	prepare(h *userHandle, sf scopeFolder, f *jmapcore.EmailFilter) error
	match(m *mailbox.MessageMeta, sf scopeFolder, scope *queryScope, f *jmapcore.EmailFilter) bool
}

// indexEvaluator answers every condition derivable from the mail index alone.
// The full-text conditions are the ones it names as unsupported; an evaluator
// that can answer them composes with this one rather than replacing it.
type indexEvaluator struct{}

// unsupported names the full-text conditions. A client reading the refusal can
// tell which part to drop and retry — "unsupported filter" alone would leave it
// guessing, and dropping the condition silently would return a confidently
// wrong result set.
func (indexEvaluator) unsupported(f *jmapcore.EmailFilter) []string {
	return f.TextConditions()
}

// prepare has nothing to resolve: every condition this evaluator answers is in
// the message metadata already. A full-text evaluator does its per-folder
// lookup here.
func (indexEvaluator) prepare(*userHandle, scopeFolder, *jmapcore.EmailFilter) error { return nil }

func (indexEvaluator) match(m *mailbox.MessageMeta, sf scopeFolder, scope *queryScope, f *jmapcore.EmailFilter) bool {
	if f == nil {
		return true
	}
	for _, id := range f.InMailboxOtherThan {
		if scope.byMailboxID[id] == sf.id {
			return false
		}
	}
	if f.MinSize != nil && m.Size < *f.MinSize {
		return false
	}
	// maxSize is exclusive (RFC 8621 §4.4.1): "size < maxSize".
	if f.MaxSize != nil && m.Size >= *f.MaxSize {
		return false
	}
	if f.Before != nil {
		t, err := time.Parse(time.RFC3339, *f.Before)
		if err != nil || !m.InternalDate.Before(t) {
			return false
		}
	}
	if f.After != nil {
		t, err := time.Parse(time.RFC3339, *f.After)
		if err != nil || m.InternalDate.Before(t) {
			return false
		}
	}
	if f.HasKeyword != nil || f.NotKeyword != nil {
		kw := keywordsOf(m)
		if f.HasKeyword != nil && !kw[strings.ToLower(*f.HasKeyword)] {
			return false
		}
		if f.NotKeyword != nil && kw[strings.ToLower(*f.NotKeyword)] {
			return false
		}
	}
	return true
}
