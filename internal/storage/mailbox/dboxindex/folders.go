package dboxindex

import (
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mboxenc"
)

// Folder is one mailbox found on disk: Name as a client sees it, Path relative
// to the mailboxes directory.
type Folder struct {
	Name string
	Path string
}

const dboxMailsDir = "dbox-Mails"

// WalkFolders finds every folder under a store's mailboxes directory: a
// directory holding a dbox-Mails. Any other layout comes back empty rather than
// half-read, since nothing outside this shape is a folder to this walk.
func WalkFolders(dir fs.FS) ([]Folder, error) {
	var out []Folder

	err := fs.WalkDir(dir, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || p == "." {
			return nil
		}
		if path.Base(p) == dboxMailsDir {
			// The message directory of the folder above; the test below would
			// reject it anyway. Skipped to avoid walking every one.
			return fs.SkipDir
		}
		if _, statErr := fs.Stat(dir, path.Join(p, dboxMailsDir)); statErr != nil {
			// Holds folders but is not one: walked into, not listed.
			return nil
		}
		name, nerr := decodeFolderPath(p)
		if nerr != nil {
			return fmt.Errorf("dboxindex: folder %q: %w", p, nerr)
		}
		out = append(out, Folder{Name: name, Path: p})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// decodeFolderPath turns a path on disk into the name a client sees. Decoded
// whole: modified base64 spells '/' as ',', so a separator never falls inside
// an encoded run.
func decodeFolderPath(p string) (string, error) {
	return mboxenc.FromModUTF7(p)
}
