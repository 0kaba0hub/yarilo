package mdboxmap

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// One-time conversion of a v1 base (a mailindex file with per-record extensions)
// into the fixed-width v2 base. It exists because the map is not derivable
// state: map_uid lives only here, the dbox trailer does not carry it, and every
// per-folder index references messages by it. Rebuilding from the storage files
// would mint new identities and orphan every folder record — so the old bytes
// must be read exactly once rather than discarded.
//
// This file is deleted before beta ends. Nothing else may depend on it.

// isForeignBase reports whether err says the file does not carry the v2 magic
// at all — the only case worth trying to convert. A file that does carry the
// magic under a version this binary does not know is refused, never converted
// and never rebuilt: a newer version's records are not ours to reinterpret.
func isForeignBase(err error) bool {
	var e errUnknownBaseVersion
	return errors.As(err, &e) && !e.magicOK
}

// convertV1Locked reads the v1 base plus its append log and writes the v2 base
// in their place. map_uids and refcounts carry over unchanged.
//
// Written to .tmp and renamed, so v1 stays intact until v2 is complete: an
// interrupted conversion leaves the old file readable and the next open simply
// converts again. The log is left alone and its replay offset recorded, so a
// transaction appended while this ran is still applied afterwards.
func (m *Map) convertV1Locked() error {
	f, err := mailindex.Open(m.path)
	if err != nil {
		// Not v1 either: the file names a format this binary cannot read. It
		// decides which bytes belong to which message and which file a purge may
		// unlink, so guessing is mail loss — refuse and leave it untouched.
		return fmt.Errorf("mdboxmap/convert: base is neither v2 nor v1, refusing to guess: %w", err)
	}

	entries := make([]MapEntry, 0, len(f.Records))
	var maxUID, maxFileID uint32
	for _, rec := range f.Records {
		fileID, offset, size, derr := decodeMapExt(rec.Ext[extMap])
		if derr != nil {
			return fmt.Errorf("mdboxmap/convert: uid=%d: %w", rec.UID, derr)
		}
		entries = append(entries, MapEntry{
			UID:      rec.UID,
			FileID:   fileID,
			Offset:   offset,
			Size:     size,
			RefCount: decodeRefExt(rec.Ext[extRef]),
			GUID:     decodeGUIDExt(rec.Ext[extGUID]),
		})
		if rec.UID > maxUID {
			maxUID = rec.UID
		}
		if fileID > maxFileID {
			maxFileID = fileID
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].UID < entries[j].UID })

	m.st = store{recs: make([]byte, len(entries)*baseRecordLen)}
	for i, e := range entries {
		m.st.setAt(i, e)
	}
	m.loaded = true

	m.indexID = f.Header.IndexID
	m.nextMapUID = f.Header.NextUID
	if maxUID+1 > m.nextMapUID {
		m.nextMapUID = maxUID + 1
	}
	if m.nextMapUID == 0 {
		m.nextMapUID = 1
	}
	if ext := findExt(f.Extensions, extMap); ext != nil {
		m.highestFileID, m.rebuildCount, m.createFileID, m.createTime = decodeMapHeader(ext.HdrData)
	}
	if maxFileID > m.highestFileID {
		m.highestFileID = maxFileID
	}

	seq, _, ierr := m.logIdentityLocked()
	if ierr != nil {
		return ierr
	}
	applied, rerr := m.replayLogInnerLocked(mailindex.LogHeaderSize)
	if rerr != nil && !errors.Is(rerr, errLogIndexMismatch) {
		return fmt.Errorf("mdboxmap/convert: replay log: %w", rerr)
	}
	m.logSeq = seq
	m.logReplayOffset = applied
	m.logSize = applied

	if err := m.writeBaseLocked(); err != nil {
		return fmt.Errorf("mdboxmap/convert: %w", err)
	}
	slog.Warn("mdbox: map index converted to format v2",
		"user", m.username, "path", m.path, "records", len(entries), "next_map_uid", m.nextMapUID)
	return nil
}

// findExt returns a pointer to the named extension in the slice, or nil if not
// found.
func findExt(exts []mailindex.Extension, name string) *mailindex.Extension {
	for i := range exts {
		if exts[i].Name == name {
			return &exts[i]
		}
	}
	return nil
}
