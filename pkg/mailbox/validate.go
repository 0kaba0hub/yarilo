package mailbox

import (
	"fmt"
	"strings"
)

// NameRules are the folder-name checks a deployment applies. They come from
// storage config so an operator can turn the filesystem checks off, the way
// the reference implementation exposes mailbox_list_validate_fs_names.
type NameRules struct {
	// ValidateFSNames enables the path-shaped checks: "." and ".." segments,
	// adjacent separators, a leading "/" or "~", and the separator-mismatch
	// rule. Off means the storage driver is trusted to be safe with any name,
	// which is only true of a driver that does not build a path from it.
	ValidateFSNames bool

	// ReservedSegments are names a single hierarchy segment may not equal:
	// the layout's own internal directories (cur/new/tmp, dbox-Mails). A
	// folder called "cur" collides with maildir's own subdirectory and
	// corrupts the mailbox without ever leaving it, so this is a distinct
	// hazard from traversal rather than more of the same.
	//
	// Empty disables the check. It is retroactive -- a user who already owns
	// such a folder is refused access to a folder that exists -- so it is a
	// knob rather than a constant.
	ReservedSegments []string
}

// DefaultNameRules validate paths and reserve the layout directories. The
// sandbox holds 996 maildir and 2338 dbox folders and none of them collides
// with a reserved name, so this default costs nothing there; a deployment that
// finds otherwise turns ReservedSegments off rather than losing access.
func DefaultNameRules() NameRules {
	return NameRules{
		ValidateFSNames:  true,
		ReservedSegments: []string{"cur", "new", "tmp", dboxMailsSubdir},
	}
}

// ValidateName checks a client-supplied mailbox name before it becomes a path.
//
// nsSep is the hierarchy separator the client speaks; layoutSep is the one the
// storage layout writes to disk. They differ on maildir++, where the namespace
// may present "/" while the layout uses "." -- and that difference is the
// reason the traversal exposure looked separator-dependent: a name is rewritten
// from one to the other, and whether a ".." survived the rewrite decided
// whether it escaped. Refusing the layout separator outright when it is not the
// namespace separator removes the accident (#1069).
//
// INBOX is not exempt here and does not need to be: it contains no segment any
// rule refuses. Whether INBOX may be *destroyed* is a different question that
// belongs to the command, not to its name (#1071).
func ValidateName(name, nsSep, layoutSep string, rules NameRules) error {
	if name == "" {
		return fmt.Errorf("%w: empty name resolves to the mailbox root", ErrInvalidFolderName)
	}
	if strings.Contains(name, "\x00") {
		return fmt.Errorf("%w: name contains a NUL", ErrInvalidFolderName)
	}
	if !rules.ValidateFSNames && len(rules.ReservedSegments) == 0 {
		return nil
	}
	if nsSep == "" {
		nsSep = "/"
	}
	if layoutSep == "" {
		layoutSep = "/"
	}

	if rules.ValidateFSNames {
		if strings.HasPrefix(name, "/") {
			return fmt.Errorf("%w: %q begins with %q", ErrInvalidFolderName, name, "/")
		}
		if strings.HasPrefix(name, "~") {
			return fmt.Errorf("%w: %q begins with %q", ErrInvalidFolderName, name, "~")
		}
		// The layout separator cannot appear in a name that is written with a
		// different one: it would silently become a hierarchy level on disk,
		// which is a level the client did not ask for and cannot address.
		if layoutSep != nsSep && strings.Contains(name, layoutSep) {
			return fmt.Errorf("%w: %q contains %q, which is the on-disk hierarchy separator here",
				ErrInvalidFolderName, name, layoutSep)
		}
	}

	// Segments are examined under every separator that can split this name on
	// its way to disk, not only the configured one: the rewrite between them
	// is exactly where a name stopped being what it looked like.
	seps := []string{nsSep, layoutSep, "/"}
	for _, sep := range seps {
		for _, segment := range strings.Split(name, sep) {
			if rules.ValidateFSNames {
				switch segment {
				case "":
					// Only meaningful when the separator actually occurs;
					// splitting on an absent separator yields the whole name.
					if strings.Contains(name, sep) {
						return fmt.Errorf("%w: %q has an empty hierarchy segment", ErrInvalidFolderName, name)
					}
				case ".", "..":
					return fmt.Errorf("%w: %q contains a %q path segment", ErrInvalidFolderName, name, segment)
				}
			}
			for _, reserved := range rules.ReservedSegments {
				if strings.EqualFold(segment, reserved) {
					return fmt.Errorf("%w: %q uses %q, which the storage layout owns",
						ErrInvalidFolderName, name, reserved)
				}
			}
		}
	}
	return nil
}

// LayoutSeparator is the hierarchy separator a driver writes to disk. maildir++
// is flat and encodes hierarchy with "."; the dbox layouts nest directories and
// use "/".
func LayoutSeparator(driver string) string {
	switch strings.ToLower(driver) {
	case "mdbox", "sdbox", "dbox":
		return "/"
	default:
		return "."
	}
}
