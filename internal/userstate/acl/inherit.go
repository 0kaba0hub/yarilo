package acl

import (
	"fmt"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// MaterialiseOnCreate writes the inherited ACL into a newly created mailbox's
// own file, so the file answers "who has rights here" from that moment on.
//
// This is inheritance as the reference implements it
// (acl_mailbox_copy_acls_from_parent): resolved once, at creation, into the new
// mailbox's own object, which is authoritative afterwards. The alternative --
// merging a namespace-root layer under every mailbox at resolve time -- is what
// the *global* ACL does, and it makes a per-mailbox ACL additive: restriction
// expressed by leaving somebody out stops working, silently (#1111).
//
// The practical consequence is the one this exists for: a user who creates a
// mailbox with a create right held at the namespace root is written into the
// new mailbox's ACL immediately, so the first SETACL they issue on it does not
// replace the grant they are acting under and revoke themselves.
//
// Global-ACL entries are not copied, matching the reference's
// `if (!update.rights.global)`. In yarilo they cannot be: the global ACL is
// operator configuration, never a stored entry, and it keeps merging live. A
// creator whose rights come from it holds them without them appearing here.
//
// Nothing is written when the mailbox already has an ACL, when there is nothing
// to inherit, or when ACLs are disabled for this store.
func (s *Store) MaterialiseOnCreate(folder string) error {
	if folder == "" || s.globalsOnly {
		return nil
	}
	existing, err := s.Get(folder)
	if err != nil {
		return fmt.Errorf("userstate/acl: materialise %s: %w", folder, err)
	}
	if existing != nil {
		return nil
	}
	inherited, err := s.inheritedFor(folder)
	if err != nil {
		return fmt.Errorf("userstate/acl: materialise %s: %w", folder, err)
	}
	if len(inherited) == 0 {
		return nil
	}
	return s.Set(folder, inherited)
}

// inheritedFor resolves what a mailbox inherits: the first ancestor with an
// explicit ACL, else the namespace-root default. It is the parent chain, not
// the mailbox itself, so it answers the same question before and after the
// mailbox has a file of its own.
func (s *Store) inheritedFor(folder string) (mailbox.ACL, error) {
	// The store's own separator, not the caller's: the ACL tree is laid out
	// with it, so a caller passing a different one would walk a hierarchy that
	// does not exist here.
	var sep byte
	if s.separator != "" {
		sep = s.separator[0]
	}
	parent := ""
	if sep != 0 {
		if idx := lastSepIndex(folder, sep); idx >= 0 {
			parent = folder[:idx]
		}
	}
	if parent != "" {
		acl, err := s.localACLFor(parent, sep)
		if err != nil {
			return nil, err
		}
		return acl, nil
	}
	return s.rootDefaultACL()
}

// MaterialiseReport is what one materialisation did, or would do on a dry run.
type MaterialiseReport struct {
	// Added maps a mailbox to the entries written into its ACL.
	Added map[string][]MaterialiseEntry
	// Skipped maps a mailbox to the identifiers it inherits and already names,
	// with the rights the mailbox's own entry gives them -- those are left
	// exactly as they are.
	Skipped map[string][]MaterialiseEntry
}

// MaterialiseEntry is one line of the report: who, and what they get.
//
// The rights are not decoration. The whole point of the dry run is that the
// tool cannot tell a mailbox orphaned by the old rule from one whose file was
// written to leave somebody out, so the judgement is handed to an operator --
// and a list of bare identifiers prints those two cases identically. With the
// rights, "anyone -> lr" on a mailbox whose ACL names one person reads as a
// mistake at a glance, and "user=u2 -> lrskxa" says the run hands u2 full
// administrative control rather than merely mentioning them.
type MaterialiseEntry struct {
	Identifier string `json:"identifier"`
	Rights     string `json:"rights"`
}

// MaterialiseExisting repairs mailboxes created before inheritance was
// materialised at creation: it adds the identifiers a mailbox inherits and does
// not already name.
//
// It exists because copy-at-create only helps mailboxes created after it. A
// mailbox that got its ACL under the old rule -- where a per-mailbox file
// replaced the inherited grant outright -- can name a single peer and nobody
// able to administer it, and in a shared namespace there is no owner to repair
// that from inside (#1111).
//
// Three properties it must have, all of them about the direction of the error:
//
//   - It only ever adds. An entry already in the file is an explicit statement
//     about that identifier; rewriting it would be guessing at intent. Those
//     are reported as skipped rather than passed over silently.
//   - It cannot be automatic. "Orphaned by the old rule" and "written to leave
//     that identifier out" are the same file on disk, so a resolver that
//     materialised on read would widen access exactly where it was narrowed
//     on purpose. An operator decides, per namespace, with a report.
//   - Running it twice adds nothing the second time.
//
// dryRun answers what it would do without writing.
func (s *Store) MaterialiseExisting(folders []string, dryRun bool) (*MaterialiseReport, error) {
	rep := &MaterialiseReport{Added: map[string][]MaterialiseEntry{}, Skipped: map[string][]MaterialiseEntry{}}
	for _, folder := range folders {
		if folder == "" {
			continue
		}
		inherited, err := s.inheritedFor(folder)
		if err != nil {
			return nil, fmt.Errorf("userstate/acl: materialise %s: %w", folder, err)
		}
		if len(inherited) == 0 {
			continue
		}
		current, err := s.Get(folder)
		if err != nil {
			return nil, fmt.Errorf("userstate/acl: materialise %s: %w", folder, err)
		}
		if current == nil {
			// No file of its own: it already resolves to what it inherits, and
			// writing one would only freeze a value that is still live.
			continue
		}
		named := map[string]mailbox.Rights{}
		for _, e := range current {
			named[identityKey(e)] = e.Rights
		}
		next := append(mailbox.ACL(nil), current...)
		for _, e := range inherited {
			if have, ok := named[identityKey(e)]; ok {
				// Reported with the rights the mailbox itself gives them, not
				// the ones it would have inherited: that is what stays in force.
				rep.Skipped[folder] = append(rep.Skipped[folder],
					MaterialiseEntry{Identifier: identityKey(e), Rights: string(have)})
				continue
			}
			next = append(next, e)
			rep.Added[folder] = append(rep.Added[folder],
				MaterialiseEntry{Identifier: identityKey(e), Rights: string(e.Rights)})
		}
		if len(rep.Added[folder]) == 0 || dryRun {
			continue
		}
		if err := s.Set(folder, next); err != nil {
			return nil, fmt.Errorf("userstate/acl: materialise %s: %w", folder, err)
		}
	}
	return rep, nil
}

// identityKey is the identifier as the report and the "already named" test see
// it: type, name, and whether the entry is negative. A negative entry for an
// identifier is a statement about that identifier, so a positive one is not
// added beside it.
func identityKey(e mailbox.Entry) string {
	if e.Negative {
		return "-" + e.Identifier.String()
	}
	return e.Identifier.String()
}
