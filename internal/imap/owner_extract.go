package imap

import "github.com/yarilomail/yarilo/pkg/mailbox"

// isOwnerTemplated reports whether spec's prefix carries the owner variable.
// The detection and the variable itself live in pkg/mailbox, so config
// validation and this resolver share one definition (mailbox.OwnerVar,
// mailbox.PrefixIsOwnerTemplated).
func isOwnerTemplated(spec NamespaceSpec) bool {
	return mailbox.PrefixIsOwnerTemplated(spec.Prefix)
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
	return mailbox.ExtractOwner(spec.Prefix, byte(spec.Separator), name)
}
