// Package imapthread implements the THREAD command of RFC 5256.
//
// It is deliberately separate from internal/threading, which computes the
// durable conversation identity behind JMAP's threadId and IMAP's FETCH
// THREADID. That one follows the mail; this one follows the specification to
// the letter, including a base subject rule that only knows English prefixes.
// The two may group edge cases differently. See docs-internal INTERNALS.md §23
// -- the divergence is intended and must not be "fixed" by merging them.
package imapthread

import "strings"

// BaseSubject implements RFC 5256 §2.1, and reports whether the subject was a
// reply or a forward -- which §4 defines as "the extraction removed a
// subj-refwd, a (fwd) trailer, or a subj-fwd-hdr/trl pair", so it is answered
// by the extraction itself rather than by a second look at the text.
//
// The specification says servers MUST use exactly this algorithm, because a
// disconnected client runs it too: a server that normalises subjects its own
// way sorts a mailbox differently from the client displaying it.
func BaseSubject(subject string) (base string, refOrFwd bool) {
	s := collapse(subject)
	for {
		// (2) Remove every trailing subj-trailer: "(fwd)" or whitespace.
		for {
			trimmed := strings.TrimRight(s, " ")
			if rest, ok := cutSuffixFold(trimmed, "(fwd)"); ok {
				s, refOrFwd = rest, true
				continue
			}
			if trimmed == s {
				break
			}
			s = trimmed
		}
		// (3)(4)(5) Remove leading subj-leader, then one subj-blob, until
		// neither matches.
		for {
			if rest, removed := cutLeader(s); removed {
				s = rest
				continue
			}
			if rest, ok := cutRefwd(s); ok {
				s, refOrFwd = rest, true
				continue
			}
			// (4) A leading blob is removed only if something is left: a
			// subject that is nothing but "[tag]" IS its own base subject.
			if rest, ok := cutBlob(s); ok && strings.TrimLeft(rest, " ") != "" {
				s = rest
				continue
			}
			break
		}
		// (6) "[fwd: ... ]" wrapping the whole subject: unwrap and start over.
		if inner, ok := cutFwdWrapper(s); ok {
			s, refOrFwd = inner, true
			continue
		}
		return s, refOrFwd
	}
}

// collapse implements step (1) for text already decoded to UTF-8: tabs and
// continuations become spaces, runs of spaces become one.
func collapse(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch r {
		case '\t', '\r', '\n', ' ':
			space = true
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cutLeader removes leading whitespace, which is the WSP branch of subj-leader.
func cutLeader(s string) (string, bool) {
	rest := strings.TrimLeft(s, " ")
	return rest, rest != s
}

// cutRefwd removes one subj-refwd:
//
//	subj-refwd = ("re" / ("fw" ["d"])) *WSP [subj-blob] ":"
//
// The optional blob between the keyword and the colon is what makes
// "Re[2]: subject" and "re [list]: subject" replies too.
func cutRefwd(s string) (string, bool) {
	rest, ok := cutPrefixFold(s, "re")
	if !ok {
		if rest, ok = cutPrefixFold(s, "fwd"); !ok {
			if rest, ok = cutPrefixFold(s, "fw"); !ok {
				return s, false
			}
		}
	}
	rest = strings.TrimLeft(rest, " ")
	if blob, cut := cutBlob(rest); cut {
		rest = blob
	}
	rest, ok = strings.CutPrefix(rest, ":")
	if !ok {
		return s, false
	}
	return rest, true
}

// cutBlob removes one subj-blob: "[" *BLOBCHAR "]" *WSP, where BLOBCHAR is
// anything but a bracket -- so blobs do not nest and the first "]" closes.
func cutBlob(s string) (string, bool) {
	if !strings.HasPrefix(s, "[") {
		return s, false
	}
	end := strings.IndexAny(s[1:], "[]")
	if end < 0 || s[1+end] != ']' {
		return s, false
	}
	return strings.TrimLeft(s[end+2:], " "), true
}

// cutFwdWrapper implements step (6): the whole subject wrapped in "[fwd:" and
// "]". Only an exact wrapper counts, so "[fwd: a] b" is not one.
func cutFwdWrapper(s string) (string, bool) {
	inner, ok := cutPrefixFold(s, "[fwd:")
	if !ok {
		return s, false
	}
	inner, ok = strings.CutSuffix(inner, "]")
	if !ok {
		return s, false
	}
	return inner, true
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

func cutSuffixFold(s, suffix string) (string, bool) {
	if len(s) < len(suffix) || !strings.EqualFold(s[len(s)-len(suffix):], suffix) {
		return s, false
	}
	return s[:len(s)-len(suffix)], true
}
