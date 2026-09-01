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

// AdoptNames brings the folder directories under root/mailboxes to this
// deployment's encoding, which makes the store one-way from the first open
// (#1586). Bottom-up so a renamed parent cannot invalidate a child's path, and
// each rename is fsynced; a name that does not decode is left alone.
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
			// The message directory, not a folder name.
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
			// Source gone: a twin or a crash halfway already renamed. Source
			// still there: two folders would become one, which nothing undoes (#1609).
			if _, serr := os.Stat(dir); os.IsNotExist(serr) {
				continue
			}
			return renamed, fmt.Errorf("dboxconv: renaming %s to %s: the target already exists", dir, target)
		}
		if err := os.Rename(dir, target); err != nil {
			if os.IsNotExist(err) {
				// Renamed by a twin between the walk and here.
				if _, serr := os.Stat(target); serr == nil {
					continue
				}
			}
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
		// The encodings agree on ASCII, except for "&": the modified UTF-7
		// escape, so a name carrying one differs between them.
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
	// A name unchanged by decode-and-re-encode is already theirs: re-encoding
	// would escape its "&" into the double encoding this removes.
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
