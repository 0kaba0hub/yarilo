package imap

import "strings"

// ownerVar is the owner variable v1 supports in a namespace prefix. A prefix
// containing it is owner-templated: the segment filling its slot names the
// owner of the instance. %n / %d split-slot forms are a documented follow-up
// (docs/OWNER_SHARED_NS.md 3.1-3.2).
const ownerVar = "%u"

// isOwnerTemplated reports whether spec's prefix carries the owner variable.
func isOwnerTemplated(spec NamespaceSpec) bool {
	return strings.Contains(spec.Prefix, ownerVar)
}

// extractOwner parses a wire mailbox name under an owner-templated namespace
// into the owner it names and the folder relative to that owner's store.
//
// prefix = "user/%u/", sep = "/":
//
//	"user/alice/Sent" -> owner "alice", rel "Sent"
//	"user/alice"      -> owner "alice", rel "INBOX"  (bare namespace name)
//
// The owner is a userdb key, never a path component: the caller passes it to
// the userdb lookup, and the storage path comes only from the userdb result and
// the location template, never by concatenating this segment into a path. Even
// so, a segment that could not be a username -- empty, "." or ".." -- is
// rejected here, so a malformed owner resolves to *nobody*, not to the
// requesting user. That distinction is what keeps isOwner honest: a name that
// does not name a real owner leaves h.owner "", and "" is owned by no one.
//
// ok is false when the name is not under this namespace, when the owner segment
// is malformed, or when the prefix's post-variable literal is anything but the
// separator (the v1 limitation). A false result must resolve to no owner, never
// to the session user.
func extractOwner(spec NamespaceSpec, name string) (owner, rel string, ok bool) {
	if !isOwnerTemplated(spec) {
		return "", "", false
	}
	before, after, _ := strings.Cut(spec.Prefix, ownerVar)
	if !strings.HasPrefix(name, before) {
		return "", "", false
	}
	sep := string(spec.Separator)
	if sep == "" {
		sep = "/"
	}
	// v1: the variable is the last meaningful element of the prefix, followed by
	// the separator or nothing. A richer post-variable literal (e.g.
	// "user/%u/mail/") is not parsed here rather than parsed wrongly.
	if after != "" && after != sep {
		return "", "", false
	}
	rest := name[len(before):]
	seg, tail, hasSep := strings.Cut(rest, sep)
	if !validOwnerSegment(seg) {
		return "", "", false
	}
	if !hasSep || tail == "" {
		// "user/alice" or "user/alice/" -> the owner's INBOX.
		return seg, "INBOX", true
	}
	return seg, tail, true
}

// validOwnerSegment rejects a segment that cannot be a username: empty, or the
// filesystem-relative "." / "..". Everything else is left to the userdb lookup,
// which fails closed for an owner that does not exist.
func validOwnerSegment(seg string) bool {
	switch seg {
	case "", ".", "..":
		return false
	}
	return true
}
