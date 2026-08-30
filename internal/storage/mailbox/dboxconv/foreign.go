// Package dboxconv converts another implementation's dbox store into ours, in
// place: the messages are not copied, moved or rewritten. Their map entries
// become ours, pointing at the same storage files at the same offsets, and
// their folder indexes become ours beside them; then theirs are removed.
//
// The conversion is one-way and destructive by design (#1524). Once a folder is
// converted the original server can no longer open it: it would find its index
// missing and rebuild from the records, losing flags and keywords. That is
// stated where an operator reads it, not implied.
//
// Layering: this package reads their format and builds our message metadata. It
// does not write our folder index -- that belongs to the index backend, which
// calls this.
package dboxconv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
)

// Their file names. Kept in one place because the removal step has to name
// exactly the same set the reading step understood: a file left behind that
// they would still read makes a stale foreign index look authoritative.
const dboxMailsDir = "dbox-Mails"

const (
	foreignIndex    = "dovecot.index"
	foreignLog      = "dovecot.index.log"
	foreignLogPrev  = "dovecot.index.log.2"
	foreignCache    = "dovecot.index.cache"
	foreignMapIndex = "dovecot.map.index"
	foreignMapLog   = "dovecot.map.index.log"
)

// StoreRoot is where an mdbox store lives, the same rule the driver applies:
// mail_path when the deployment sets one, and <home>/mdbox when it does not.
// Stated here rather than assumed, because the conversion has to read their
// files from the directory the driver will later write ours into -- computing it
// differently by one level puts the map somewhere nothing looks.
func StoreRoot(home, mailPath string) string {
	if mailPath != "" {
		return mailPath
	}
	return filepath.Join(home, "mdbox")
}

// HasForeignFolder reports whether dir holds another implementation's folder
// index: either a base index or a log, since a folder written but not yet
// flushed has only the second (#1564).
func HasForeignFolder(dir string) bool {
	for _, name := range []string{foreignIndex, foreignLog} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// HasForeignMap reports whether a store's storage directory holds their map.
func HasForeignMap(storageDir string) bool {
	_, err := os.Stat(filepath.Join(storageDir, foreignMapLog))
	return err == nil
}

// AnyForeignFolderLeft reports whether the store still holds a folder of theirs
// that has not been converted.
//
// The walk is over directories rather than over their mailbox list index: a
// folder is theirs while its index files are on disk, and that is the same
// thing the conversion path decides on. Reading their list instead would answer
// a different question -- what they believed existed -- and the two disagree
// exactly when it matters, on a store somebody has been converting.
func AnyForeignFolderLeft(mailboxesDir string) (bool, error) {
	found := false
	err := filepath.WalkDir(mailboxesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if found {
			return fs.SkipAll
		}
		if !d.IsDir() || filepath.Base(p) != dboxMailsDir {
			return nil
		}
		if HasForeignFolder(p) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("dboxconv: walk %s: %w", mailboxesDir, err)
	}
	return found, nil
}

// DropForeignMapIfDone removes their map once no folder of theirs is left, and
// reports whether it did.
//
// Separate from a folder's conversion on purpose. The map is one for the whole
// store, and a folder that has not been converted still addresses its mail
// through their map uids: removing it early makes the rest of the store
// unreadable by either implementation. So the last folder to convert is what
// ends the store's conversion, and until then their map stays (#1569).
//
// The check and the removal are one section under the map's lock. What that
// orders is not two deleters -- a second one finds the files gone and is happy
// -- but a deleter against an importer, which reads exactly the files being
// removed.
//
// A store where some folder is never opened keeps their map indefinitely, and
// that is the honest outcome rather than a failure: the folder still needs it.
func DropForeignMapIfDone(storageDir, mailboxesDir string, ours *mdboxmap.Map) (bool, error) {
	dropped := false
	err := ours.WithLock(func() error {
		left, err := AnyForeignFolderLeft(mailboxesDir)
		if err != nil {
			return err
		}
		if left {
			return nil
		}
		for _, name := range []string{foreignMapIndex, foreignMapLog} {
			if err := os.Remove(filepath.Join(storageDir, name)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("dboxconv: remove %s: %w", name, err)
			}
		}
		dropped = true
		return nil
	})
	return dropped, err
}

// ErrReadOnly says a store cannot be converted because it cannot be written to.
var ErrReadOnly = errors.New("store is read-only; conversion deletes the foreign index and cannot run -- mount it writable or convert it offline")

// CheckWritable reports whether a conversion could finish in dir, by writing
// there rather than by reasoning about permissions.
//
// Asked before any work, not after. A conversion ends by unlinking their index
// files, so a directory that refuses writes fails on the last step -- having
// read their index, built ours and paid for all of it, on every open, for as
// long as the store stays read-only. The probe costs two syscalls and turns
// that into one refusal.
//
// It writes: mode bits, mount options and whatever else a filesystem decides by
// are not one question that can be asked, and a check that reasoned about them
// would be wrong on the case that matters -- a read-only mount under a
// directory whose bits say 0700.
func CheckWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".yarilo-convert-probe-*")
	if err != nil {
		return fmt.Errorf("dboxconv: %s: %w (%v)", dir, ErrReadOnly, err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("dboxconv: %s: %w (%v)", dir, ErrReadOnly, err)
	}
	return nil
}

// ReadForeignMap reads their map: the base when there is one, then its log, and
// the log alone when there is not. The second is not an edge case -- their map
// base is written only once a rewrite threshold is passed, so a store that has
// not reached it has no base at all.
func ReadForeignMap(storageDir string) ([]dboxindex.MapEntry, error) {
	var seed []dboxindex.Extension
	if raw, err := os.ReadFile(filepath.Join(storageDir, foreignMapIndex)); err == nil {
		h, herr := dboxindex.ParseHeader(raw)
		if herr != nil {
			return nil, fmt.Errorf("dboxconv: map index: %w", herr)
		}
		seed, herr = dboxindex.ParseExtensions(raw, h)
		if herr != nil {
			return nil, fmt.Errorf("dboxconv: map extensions: %w", herr)
		}
	}

	raw, err := os.ReadFile(filepath.Join(storageDir, foreignMapLog))
	if err != nil {
		return nil, fmt.Errorf("dboxconv: map log: %w", err)
	}
	lh, err := dboxindex.ParseLogHeader(raw)
	if err != nil {
		return nil, fmt.Errorf("dboxconv: map log header: %w", err)
	}
	return dboxindex.ReadMap(raw, int(lh.HeaderSize), seed)
}

// Folder is one of their folders as read from disk: what it holds, how to
// locate it, and the two header fields that say which UID space it is.
type Folder struct {
	Records []dboxindex.Record
	Exts    []dboxindex.Extension
	Header  dboxindex.HeaderState
}

// ReadForeignFolder reads one folder's state: their base index plus its log
// from log_file_tail_offset, or the log from its start when there is no base.
//
// A folder with neither is not this function's business and is an error here:
// an empty return would show as a folder that exists and holds nothing, which
// is the one answer nobody checks.
func ReadForeignFolder(dir string) (Folder, error) {
	logPath := filepath.Join(dir, foreignLog)
	raw, err := os.ReadFile(filepath.Join(dir, foreignIndex))
	if err != nil {
		if !os.IsNotExist(err) {
			return Folder{}, fmt.Errorf("dboxconv: read index %s: %w", dir, err)
		}
		tail, terr := os.ReadFile(logPath)
		if terr != nil {
			return Folder{}, fmt.Errorf("dboxconv: %s has no index and no log: %w", dir, terr)
		}
		return folderFromLog(dir, tail)
	}

	h, err := dboxindex.ParseHeader(raw)
	if err != nil {
		return Folder{}, fmt.Errorf("dboxconv: index %s: %w", dir, err)
	}
	recs, err := dboxindex.ParseRecords(raw, h)
	if err != nil {
		return Folder{}, fmt.Errorf("dboxconv: records %s: %w", dir, err)
	}
	exts, err := dboxindex.ParseExtensions(raw, h)
	if err != nil {
		return Folder{}, fmt.Errorf("dboxconv: extensions %s: %w", dir, err)
	}

	var names []string
	if kw, ok := dboxindex.Find(exts, "keywords"); ok {
		names, err = dboxindex.KeywordNames(kw)
		if err != nil {
			return Folder{}, fmt.Errorf("dboxconv: keywords %s: %w", dir, err)
		}
		for i := range recs {
			recs[i].Keywords = dboxindex.KeywordsOf(recs[i].Raw, kw, names)
		}
	}
	state := dboxindex.HeaderState{UIDValidity: h.UIDValidity, NextUID: h.NextUID}
	if tail, terr := os.ReadFile(logPath); terr == nil {
		changes, cerr := dboxindex.ReadChanges(tail, int(h.LogFileTailOffset), exts)
		if cerr != nil {
			return Folder{}, fmt.Errorf("dboxconv: log %s: %w", dir, cerr)
		}
		state = dboxindex.ApplyHeader(state, changes)
		recs = dboxindex.Apply(recs, changes, names)
	}
	return Folder{Records: recs, Exts: exts, Header: state}, nil
}

// folderFromLog builds a folder out of its log alone, for one whose base index
// has not been written yet. The extensions come from the log's own intro
// records: an mdbox message is found through the map uid its extension carries,
// and with no base there is nothing else to name that extension.
func folderFromLog(dir string, tail []byte) (Folder, error) {
	h, err := dboxindex.ParseLogHeader(tail)
	if err != nil {
		return Folder{}, fmt.Errorf("dboxconv: log %s: %w", dir, err)
	}
	changes, exts, err := dboxindex.ReadChangesAndExtensions(tail, int(h.HeaderSize), nil)
	if err != nil {
		return Folder{}, fmt.Errorf("dboxconv: log %s: %w", dir, err)
	}
	// No keyword table: their base holds one and there is no base. A keyword
	// set in the log names itself, so those arrive; one carried as a bitmask
	// alone has nothing to resolve against and does not.
	return Folder{
		Records: dboxindex.Apply(nil, changes, nil),
		Exts:    exts,
		Header:  dboxindex.ApplyHeader(dboxindex.HeaderState{}, changes),
	}, nil
}
