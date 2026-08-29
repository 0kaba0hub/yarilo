package dboxindex

import (
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mboxenc"
)

// Folder is one mailbox found on disk.
//
// Name is what a client sees, with '/' between levels; Path is where its index
// files are, relative to the mailboxes directory. The two differ by more than
// encoding for a nested folder, because every level of the path is separately
// encoded.
type Folder struct {
	Name string
	Path string
}

const dboxMailsDir = "dbox-Mails"

// WalkFolders finds every folder under a store's mailboxes directory.
//
// One layout only: the reference's default for dbox, where a folder is a
// directory under <home>/mdbox/mailboxes and its messages live in a dbox-Mails
// beneath it. Anything else -- a Maildir++ tree with dotted names, or a
// deployment whose folder list lives in an index rather than in directories --
// is not read here, and is not silently half-read either: nothing outside this
// shape looks like a folder to this walk, so such a store comes back empty
// rather than wrong. Said out loud because somebody will point the importer at
// one.
//
// A folder is a directory containing dbox-Mails; that directory is where its
// index and its messages live, and the folder's own children sit beside it.
// So the walk cannot stop at the first hit and cannot treat every directory as
// a folder either -- a level that only holds children has no dbox-Mails and is
// not selectable, and a client is told so rather than shown an empty mailbox.
//
// Names on disk are modified UTF-7. The whole path is decoded at once, which is
// safe rather than lucky: modified base64 uses ',' where base64 uses '/', so a
// separator can never appear inside an encoded run and the levels stay whole.
// This started as a loop decoding each level, justified by a corruption that
// cannot happen -- a mutation collapsing it to one call passed, which is how
// the claim was found to be wrong.
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
			// The message directory of the folder above it. Skipped as an
			// optimisation and not for correctness: it holds no dbox-Mails of
			// its own, so the test below would reject it anyway. What this
			// avoids is walking every message directory in the account.
			return fs.SkipDir
		}
		if _, statErr := fs.Stat(dir, path.Join(p, dboxMailsDir)); statErr != nil {
			// A container: it holds folders but is not one. Walked into, not
			// listed.
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

// decodeFolderPath turns a path on disk into the name a client sees, decoding
// each level separately.
func decodeFolderPath(p string) (string, error) {
	return mboxenc.FromModUTF7(p)
}
