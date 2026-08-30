package dboxconv

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mboxenc"
)

// AdoptNames brings the folder directories under root/mailboxes to the encoding
// this deployment writes, and reports how many it renamed.
//
// A store the other implementation left spells its folder names in modified
// UTF-7. Ours spells them the way mailbox_list_utf8 says. Where the two differ,
// every name that is not plain ASCII is unreadable: listed as mojibake, and not
// selectable under any name a client can send (#1586). Neither side can be
// asked to bend at read time -- a store half in one encoding and half in the
// other is a store neither implementation can read whole -- so adoption brings
// the disk to what the running configuration says, once.
//
// Consequences, stated rather than discovered:
//
//   - the store becomes one-way at the first open, not folder by folder. A
//     renamed directory is one the other implementation will not find, whether
//     or not that folder has been converted yet;
//   - a name that does not decode is left exactly as it is. It may be what a
//     user typed;
//   - plain ASCII is skipped, because the two encodings agree on it.
//
// Bottom-up, so a parent renamed first cannot invalidate the path of a child
// still to be renamed. Each rename is followed by an fsync of the directory
// holding it, so a crash leaves a tree that is part renamed rather than a
// rename that reached no disk: the next open finds the trigger still in place
// and finishes the pass.
func AdoptNames(mailboxesDir string, utf8 bool) (int, error) {
	var dirs []string
	err := filepath.WalkDir(mailboxesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() || p == mailboxesDir {
			return nil
		}
		if filepath.Base(p) == dboxMailsDir {
			// The message directory, not a folder name. Its own children are
			// message files, so there is nothing below it to walk either.
			return fs.SkipDir
		}
		dirs = append(dirs, p)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("dboxconv: walk %s: %w", mailboxesDir, err)
	}
	// Deepest first.
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], string(filepath.Separator)) > strings.Count(dirs[j], string(filepath.Separator))
	})

	renamed := 0
	for _, dir := range dirs {
		parent, name := filepath.Split(dir)
		want, ok := adoptedName(name, utf8)
		if !ok || want == name {
			continue
		}
		target := filepath.Join(parent, want)
		if _, err := os.Stat(target); err == nil {
			// Something already carries the name we would move to. Refused
			// rather than merged: two folders becoming one is a loss no later
			// step can undo, and a store in that shape needs a person.
			return renamed, fmt.Errorf("dboxconv: renaming %s to %s: the target already exists", dir, target)
		}
		if err := os.Rename(dir, target); err != nil {
			return renamed, fmt.Errorf("dboxconv: rename %s to %s: %w", dir, target, err)
		}
		if err := fsyncDir(filepath.Clean(parent)); err != nil {
			return renamed, err
		}
		renamed++
	}
	return renamed, nil
}

// adoptedName returns the name this deployment would write for a directory
// currently named name, and whether it could tell.
func adoptedName(name string, utf8 bool) (string, bool) {
	if isASCII(name) && !strings.Contains(name, "&") {
		// The two encodings agree, and there is nothing to do. The ampersand is
		// the exception: it is the escape in modified UTF-7, so a name carrying
		// one is not the same string in both.
		return name, true
	}
	if utf8 {
		decoded, err := mboxenc.FromModUTF7(name)
		if err != nil {
			// Not their encoding, so nothing to bring across.
			return name, false
		}
		return decoded, true
	}
	// Encoding an already-encoded name escapes its ampersand and produces
	// &-BBIER... -- the double encoding this whole change exists to remove,
	// reintroduced by the fix. A name that survives a decode and re-encode
	// unchanged is already in their encoding and is left alone.
	if decoded, err := mboxenc.FromModUTF7(name); err == nil && mboxenc.ToModUTF7(decoded) == name {
		return name, true
	}
	return mboxenc.ToModUTF7(name), true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// fsyncDir flushes a directory entry, so a rename inside it survives a crash
// rather than merely having been asked for.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("dboxconv: fsync %s: %w", dir, err)
	}
	defer d.Close() //nolint:errcheck
	if err := d.Sync(); err != nil {
		return fmt.Errorf("dboxconv: fsync %s: %w", dir, err)
	}
	return nil
}
