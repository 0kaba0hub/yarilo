package mailbox

import "strings"

// OwnerVar is the owner variable a namespace prefix carries to become
// owner-templated: the segment filling its slot names the owner of the instance
// (docs/OWNER_SHARED_NS.md 3.1). v1 supports %u (the full username); %n / %d
// split-slot forms are a documented follow-up.
const OwnerVar = "%u"

// PrefixIsOwnerTemplated reports whether a namespace prefix carries the owner
// variable. One definition, shared by config validation and the IMAP resolver,
// so the two cannot disagree about what "owner-templated" means.
func PrefixIsOwnerTemplated(prefix string) bool {
	return strings.Contains(prefix, OwnerVar)
}
