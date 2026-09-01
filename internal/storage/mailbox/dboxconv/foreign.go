// Package dboxconv converts another implementation's dbox store into ours in
// place: their map entries become ours, pointing at the same files at the same
// offsets. One-way by design -- afterwards the original server would rebuild
// and lose flags and keywords (#1524).
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

// Their file names, in one place: the removal step must name exactly the set
// the reading step understood.
const dboxMailsDir = "dbox-Mails"

const foreignSubscriptions = "subscriptions"

const (
	foreignIndex      = "dovecot.index"
	foreignLog        = "dovecot.index.log"
	foreignLogPrev    = "dovecot.index.log.2"
	foreignCache      = "dovecot.index.cache"
	foreignMapIndex   = "dovecot.map.index"
	foreignMapLog     = "dovecot.map.index.log"
	foreignMapLogPrev = "dovecot.map.index.log.2"
	// The message-file prefix both implementations write for sdbox.
	sdboxPrefix = "u."
)

// StoreRoot is where a dbox store lives, the rule the drivers apply: mail_path
// when set, else <home>/<driver>. One level out puts the map where nothing looks.
func StoreRoot(home, mailPath, driver string) string {
	if mailPath != "" {
		return mailPath
	}
	// The drivers disagree about which name they root under.
	switch driver {
	case "sdbox", "dbox":
		return filepath.Join(home, "sdbox")
	default:
		return filepath.Join(home, "mdbox")
	}
}

// HasForeignFolder reports whether dir holds their folder index. Either file:
// a folder written but not yet flushed has only the log (#1564).
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

// AnyForeignFolderLeft reports whether an unconverted folder of theirs is left.
// It walks directories, not their list index, which answers a different
// question -- what they believed existed.
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
		// Every directory, not only the dbox-Mails leaves: with a separate
		// index root their index sits in the mailbox directory itself (#1583).
		if !d.IsDir() {
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

// DropForeignMapIfDone removes their map once no folder of theirs is left. An
// unconverted folder still addresses its mail through their uids, so only the
// last folder may end it (#1569); check and removal are one section under the
// map's lock, which orders a deleter against an importer.
func DropForeignMapIfDone(storageDir, mailboxesDir, mailRoot string, ours *mdboxmap.Map) (bool, error) {
	dropped := false
	err := ours.WithLock(func() error {
		left, err := AnyForeignFolderLeft(mailboxesDir)
		if err != nil {
			return err
		}
		if left {
			return nil
		}
		// The rotated log too: a file of theirs left behind is one a tool of
		// theirs would still read, and read as authoritative.
		for _, name := range []string{foreignMapIndex, foreignMapLog, foreignMapLogPrev} {
			if err := os.Remove(filepath.Join(storageDir, name)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("dboxconv: remove %s: %w", name, err)
			}
		}
		// Their subscription file is store-wide too, and its contents are in
		// ours by now -- carried on the first folder that converted.
		if err := os.Remove(filepath.Join(mailRoot, foreignSubscriptions)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("dboxconv: remove %s: %w", foreignSubscriptions, err)
		}
		dropped = true
		return nil
	})
	return dropped, err
}

// ErrReadOnly says a store cannot be converted because it cannot be written to.
var ErrReadOnly = errors.New("store is read-only; conversion deletes the foreign index and cannot run -- mount it writable or convert it offline")

// CheckWritable reports whether a conversion could finish, by writing to every
// directory it would write to -- before any work, and by writing rather than
// reasoning, since a read-only mount under 0700 bits defeats a permission check.
// Every directory: a mixed mount would otherwise fail halfway.
func CheckWritable(dirs ...string) error {
	for _, dir := range dirs {
		if err := checkWritable(dir); err != nil {
			return err
		}
	}
	return nil
}

func checkWritable(dir string) error {
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

// HasForeignSubscriptions reports whether the store carries their subscription
// file.
func HasForeignSubscriptions(mailRoot string) bool {
	_, err := os.Stat(filepath.Join(mailRoot, foreignSubscriptions))
	return err == nil
}

// ReadForeignMap reads their map: base then log, or the log alone -- their base
// is written only past a rewrite threshold, so a young store has none.
func ReadForeignMap(storageDir string) ([]dboxindex.MapEntry, error) {
	var (
		seed     []dboxindex.MapEntry
		exts     []dboxindex.Extension
		offset   int
		baseSeq  uint32
		baseTail uint32
	)
	if raw, err := os.ReadFile(filepath.Join(storageDir, foreignMapIndex)); err == nil {
		h, herr := dboxindex.ParseHeader(raw)
		if herr != nil {
			return nil, fmt.Errorf("dboxconv: map index: %w", herr)
		}
		exts, herr = dboxindex.ParseExtensions(raw, h)
		if herr != nil {
			return nil, fmt.Errorf("dboxconv: map extensions: %w", herr)
		}
		// Base plus log-from-tail: after a rotation the base is the only place
		// the older half exists (#1583).
		seed, herr = dboxindex.ParseMapRecords(raw, h, exts)
		if herr != nil {
			return nil, fmt.Errorf("dboxconv: map records: %w", herr)
		}
		offset = int(h.LogFileTailOffset)
		baseSeq, baseTail = h.LogFileSeq, h.LogFileTailOffset
	}

	raw, err := os.ReadFile(filepath.Join(storageDir, foreignMapLog))
	if err != nil {
		return nil, fmt.Errorf("dboxconv: map log: %w", err)
	}
	lh, err := dboxindex.ParseLogHeader(raw)
	if err != nil {
		return nil, fmt.Errorf("dboxconv: map log header: %w", err)
	}
	// The tail offset addresses the log named by its sequence, not whichever is
	// on disk now: after a rotation it lands anywhere. Used only when they agree.
	if baseSeq != 0 && baseSeq != lh.FileSeq {
		offset = int(lh.HeaderSize)
	}
	// No base: the log from its start, which is the whole state then.
	if offset < int(lh.HeaderSize) {
		offset = int(lh.HeaderSize)
	}

	// What the rotation moved out lives in the rotated log, from the base's
	// tail on. Read first, so the current log lands on top.
	if baseSeq != 0 && baseSeq != lh.FileSeq {
		if prev, perr := os.ReadFile(filepath.Join(storageDir, foreignMapLogPrev)); perr == nil {
			ph, pherr := dboxindex.ParseLogHeader(prev)
			if pherr != nil {
				return nil, fmt.Errorf("dboxconv: rotated map log header: %w", pherr)
			}
			at := int(ph.HeaderSize)
			if ph.FileSeq == baseSeq && int(baseTail) > at {
				at = int(baseTail)
			}
			seed, err = dboxindex.ReadMapOnto(seed, prev, at, exts)
			if err != nil {
				return nil, fmt.Errorf("dboxconv: rotated map log: %w", err)
			}
		}
	}
	return dboxindex.ReadMapOnto(seed, raw, offset, exts)
}

// Folder is one of their folders as read from disk: what it holds, how to
// locate it, and the two header fields that say which UID space it is.
type Folder struct {
	Records []dboxindex.Record
	Exts    []dboxindex.Extension
	Header  dboxindex.HeaderState
}

// ReadForeignFolder reads one folder's state: their base plus its log from
// log_file_tail_offset, or the log alone. Neither is an error, not an empty
// result that would read as a folder holding nothing.
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

// folderFromLog builds a folder from its log alone, for one with no base yet.
// Extensions come from the log's intro records: nothing else names the map uid
// an mdbox message is found through.
func folderFromLog(dir string, tail []byte) (Folder, error) {
	h, err := dboxindex.ParseLogHeader(tail)
	if err != nil {
		return Folder{}, fmt.Errorf("dboxconv: log %s: %w", dir, err)
	}
	changes, exts, err := dboxindex.ReadChangesAndExtensions(tail, int(h.HeaderSize), nil)
	if err != nil {
		return Folder{}, fmt.Errorf("dboxconv: log %s: %w", dir, err)
	}
	// No keyword table without a base: a keyword named in the log arrives, one
	// carried as a bitmask alone has nothing to resolve against.
	return Folder{
		Records: dboxindex.Apply(nil, changes, nil),
		Exts:    exts,
		Header:  dboxindex.ApplyHeader(dboxindex.HeaderState{}, changes),
	}, nil
}
