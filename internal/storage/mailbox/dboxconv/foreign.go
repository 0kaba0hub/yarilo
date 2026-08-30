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
	"fmt"
	"os"
	"path/filepath"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
)

// Their file names. Kept in one place because the removal step has to name
// exactly the same set the reading step understood: a file left behind that
// they would still read makes a stale foreign index look authoritative.
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

// ReadForeignFolder reads one folder's state: their base index plus its log
// from log_file_tail_offset, or the log from its start when there is no base.
//
// A folder with neither is not this function's business and is an error here:
// an empty return would show as a folder that exists and holds nothing, which
// is the one answer nobody checks.
func ReadForeignFolder(dir string) ([]dboxindex.Record, []dboxindex.Extension, error) {
	logPath := filepath.Join(dir, foreignLog)
	raw, err := os.ReadFile(filepath.Join(dir, foreignIndex))
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("dboxconv: read index %s: %w", dir, err)
		}
		tail, terr := os.ReadFile(logPath)
		if terr != nil {
			return nil, nil, fmt.Errorf("dboxconv: %s has no index and no log: %w", dir, terr)
		}
		return folderFromLog(dir, tail)
	}

	h, err := dboxindex.ParseHeader(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("dboxconv: index %s: %w", dir, err)
	}
	recs, err := dboxindex.ParseRecords(raw, h)
	if err != nil {
		return nil, nil, fmt.Errorf("dboxconv: records %s: %w", dir, err)
	}
	exts, err := dboxindex.ParseExtensions(raw, h)
	if err != nil {
		return nil, nil, fmt.Errorf("dboxconv: extensions %s: %w", dir, err)
	}

	var names []string
	if kw, ok := dboxindex.Find(exts, "keywords"); ok {
		names, err = dboxindex.KeywordNames(kw)
		if err != nil {
			return nil, nil, fmt.Errorf("dboxconv: keywords %s: %w", dir, err)
		}
		for i := range recs {
			recs[i].Keywords = dboxindex.KeywordsOf(recs[i].Raw, kw, names)
		}
	}
	if tail, terr := os.ReadFile(logPath); terr == nil {
		changes, cerr := dboxindex.ReadChanges(tail, int(h.LogFileTailOffset), exts)
		if cerr != nil {
			return nil, nil, fmt.Errorf("dboxconv: log %s: %w", dir, cerr)
		}
		recs = dboxindex.Apply(recs, changes, names)
	}
	return recs, exts, nil
}

// folderFromLog builds a folder out of its log alone, for one whose base index
// has not been written yet. The extensions come from the log's own intro
// records: an mdbox message is found through the map uid its extension carries,
// and with no base there is nothing else to name that extension.
func folderFromLog(dir string, tail []byte) ([]dboxindex.Record, []dboxindex.Extension, error) {
	h, err := dboxindex.ParseLogHeader(tail)
	if err != nil {
		return nil, nil, fmt.Errorf("dboxconv: log %s: %w", dir, err)
	}
	changes, exts, err := dboxindex.ReadChangesAndExtensions(tail, int(h.HeaderSize), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("dboxconv: log %s: %w", dir, err)
	}
	// No keyword table: their base holds one and there is no base. A keyword
	// set in the log names itself, so those arrive; one carried as a bitmask
	// alone has nothing to resolve against and does not.
	return dboxindex.Apply(nil, changes, nil), exts, nil
}
