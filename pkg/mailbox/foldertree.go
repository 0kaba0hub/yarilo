package mailbox

import (
	"os"
	"path/filepath"
	"strings"
)

// FolderEntry is one mailbox surfaced by ListFolders. Selectable=false marks
// a \NoSelect container that exists only on disk to hold child mailboxes
// (e.g. a parent auto-created when a nested child was created). The IMAP LIST
// layer turns !Selectable into a \NoSelect attribute; admin/index callers
// skip such entries because they carry no messages.
type FolderEntry struct {
	Name       string
	Selectable bool
}

// SelectableNames returns just the names of the selectable entries — the
// subset that admin and index operations act on.
func SelectableNames(entries []FolderEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Selectable {
			out = append(out, e.Name)
		}
	}
	return out
}

// WalkDboxTree performs a depth-first walk of a dbox-style "mailboxes/" root
// (the fs layout shared by mdbox and sdbox, where folders nest physically) and
// returns one FolderEntry per folder. A folder's Name is its path components
// joined by sep (the namespace separator, matching the flat maildir listing
// convention).
//
//   - decode maps an on-disk name component to its logical form (modUTF7/NFC);
//     an entry is skipped when decode reports ok=false.
//   - isMarkerDir reports directories that are not hierarchy children — the
//     dbox-Mails message store leaf for sdbox — so the walk neither descends
//     into them nor treats them as folders.
//   - selectable receives the slash-joined disk path relative to root and
//     reports whether that directory is a real (selectable) mailbox.
//
// Only selectable folders and the \NoSelect containers on the path to one are
// returned; the containers are derived from the selectable folders' name
// hierarchy, so an empty stray directory (not selectable, no selectable
// descendant) is ignored — the walk still recurses through it to reach any
// selectable child. Parents precede their children in the result.
//
// A missing root yields no entries (not an error): a user with no folders.
func WalkDboxTree(
	root, sep string,
	decode func(string) (string, bool),
	isMarkerDir func(name string) bool,
	selectable func(diskRel string) bool,
) ([]FolderEntry, error) {
	var selNames []string
	isSel := map[string]bool{}
	var walk func(dir, logicalPrefix, diskPrefix string) error
	walk = func(dir, logicalPrefix, diskPrefix string) error {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() || isMarkerDir(e.Name()) {
				continue
			}
			logical, ok := decode(e.Name())
			if !ok {
				continue
			}
			name := logical
			if logicalPrefix != "" {
				name = logicalPrefix + sep + logical
			}
			diskRel := e.Name()
			if diskPrefix != "" {
				diskRel = diskPrefix + "/" + e.Name()
			}
			if selectable(diskRel) {
				selNames = append(selNames, name)
				isSel[name] = true
			}
			if err := walk(filepath.Join(dir, e.Name()), name, diskRel); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, "", ""); err != nil {
		return nil, err
	}

	var out []FolderEntry
	emitted := map[string]bool{}
	for _, name := range selNames {
		// Emit any missing ancestor as a \NoSelect container first so
		// parents precede children in the listing.
		parts := strings.Split(name, sep)
		for i := 1; i < len(parts); i++ {
			anc := strings.Join(parts[:i], sep)
			if isSel[anc] || emitted[anc] {
				continue
			}
			emitted[anc] = true
			out = append(out, FolderEntry{Name: anc, Selectable: false})
		}
		if !emitted[name] {
			emitted[name] = true
			out = append(out, FolderEntry{Name: name, Selectable: true})
		}
	}
	return out, nil
}
