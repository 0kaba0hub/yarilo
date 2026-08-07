package mailbox

// ACLModify is how one entry changes an ACL, matching the three SETACL modes of
// RFC 4314 §3.1 and the reference's ACL_MODIFY_MODE_ADD/REMOVE/REPLACE.
//
// It lives here, not in the IMAP layer, because the admin path and the protocol
// path must transform an ACL identically or the two disagree about what a write
// means -- the shape this series kept finding at every seam. One owner, both
// callers.
type ACLModify int

const (
	// ACLReplace sets the identifier's rights to exactly the given set; an
	// empty set removes the entry (RFC 4314 §3.1: "empty rights deletes").
	ACLReplace ACLModify = iota
	// ACLAdd unions the given rights into the identifier's entry.
	ACLAdd
	// ACLRemove strips the given rights; an entry left empty is removed.
	ACLRemove
)

// ApplyEntry returns the ACL with one (identifier, sign) entry modified per
// mode. It is the single transform SETACL performs, whether the caller is the
// IMAP session or the admin API.
//
// Matching is by identifier and sign together, so a negative entry is modified
// independently of the positive one (RFC 4314 §3.1). The result is collapsed,
// so a caller cannot introduce the duplicate entries #1114 was about.
func (acl ACL) ApplyEntry(id Identifier, negative bool, mode ACLModify, rights Rights) ACL {
	idx := -1
	for i, e := range acl {
		if e.Negative == negative && e.Identifier == id {
			idx = i
			break
		}
	}
	switch mode {
	case ACLReplace:
		if rights == "" {
			if idx >= 0 {
				return append(acl[:idx:idx], acl[idx+1:]...).Collapse()
			}
			return acl.Collapse()
		}
		if idx >= 0 {
			acl[idx].Rights = rights
			return acl.Collapse()
		}
		return append(acl, Entry{Identifier: id, Rights: rights, Negative: negative}).Collapse()
	case ACLAdd:
		if idx >= 0 {
			acl[idx].Rights = acl[idx].Rights.Add(rights)
			return acl.Collapse()
		}
		return append(acl, Entry{Identifier: id, Rights: rights, Negative: negative}).Collapse()
	case ACLRemove:
		if idx >= 0 {
			acl[idx].Rights = acl[idx].Rights.Remove(rights)
			if acl[idx].Rights == "" {
				return append(acl[:idx:idx], acl[idx+1:]...).Collapse()
			}
		}
		return acl.Collapse()
	}
	return acl.Collapse()
}
