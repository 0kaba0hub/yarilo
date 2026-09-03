package mdboxmap

import (
	"fmt"
	"sort"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// Format v1 is the mailindex-backed base, kept because the format on disk decides
// what a rolled-back binary can read. It replays from the start, and a replayed
// refcount delta does not dedupe.

// readBaseV1Locked loads a v1 base into the record area.
func (m *Map) readBaseV1Locked() error {
	f, err := mailindex.Open(m.path)
	if err != nil {
		return fmt.Errorf("mdboxmap/load v1: %w", err)
	}
	entries := make([]MapEntry, 0, len(f.Records))
	var maxUID, maxFileID uint32
	for _, rec := range f.Records {
		fileID, offset, size, derr := decodeMapExt(rec.Ext[extMap])
		if derr != nil {
			return fmt.Errorf("mdboxmap/load v1: uid=%d: %w", rec.UID, derr)
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
	m.onDisk = FormatV1

	m.indexID = f.Header.IndexID
	m.nextMapUID = f.Header.NextUID
	if maxUID+1 > m.nextMapUID {
		m.nextMapUID = maxUID + 1
	}
	if m.nextMapUID == 0 {
		m.nextMapUID = 1
	}
	m.highestFileID, m.rebuildCount, m.createFileID, m.createTime = 0, 0, 0, 0
	if ext := findExt(f.Extensions, extMap); ext != nil {
		m.highestFileID, m.rebuildCount, m.createFileID, m.createTime = decodeMapHeader(ext.HdrData)
	}
	if maxFileID > m.highestFileID {
		m.highestFileID = maxFileID
	}
	// v1 has nowhere to record which log it folded, so a reader can only replay
	// from the start.
	m.lineage, m.foldedLineage, m.foldedOffset = 0, 0, 0
	return nil
}

// writeBaseV1Locked rewrites the whole base in v1. mailindex.Recreate is
// atomic (.tmp + rename), the same guarantee the v2 writer gives.
func (m *Map) writeBaseV1Locked() error {
	f, err := mailindex.NewFile(m.indexID, defaultExtensions(m.highestFileID))
	if err != nil {
		return fmt.Errorf("mdboxmap/write v1: NewFile: %w", err)
	}
	m.st.each(func(e MapEntry) {
		f.Records = append(f.Records, &mailindex.Record{
			UID: e.UID,
			Ext: map[string][]byte{
				extMap:  encodeMapExt(e.FileID, e.Offset, e.Size),
				extRef:  encodeRefExt(e.RefCount),
				extGUID: encodeGUIDExt(e.GUID),
			},
		})
	})
	ext := findExt(f.Extensions, extMap)
	if ext == nil {
		return fmt.Errorf("mdboxmap/write v1: missing %q extension", extMap)
	}
	ext.HdrData = encodeMapHeader(m.highestFileID, m.rebuildCount, m.createFileID, m.createTime)
	ext.HdrSize = uint32(len(ext.HdrData))
	layout, err := mailindex.ComputeRecordLayout(f.Extensions)
	if err != nil {
		return fmt.Errorf("mdboxmap/write v1: layout: %w", err)
	}
	extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
	if err != nil {
		return fmt.Errorf("mdboxmap/write v1: encode ext headers: %w", err)
	}
	f.Extensions = layout.Extensions
	f.Layout = layout
	f.Header.RecordSize = layout.RecordSize
	f.Header.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	f.Header.NextUID = m.nextMapUID
	f.Header.MessagesCount = uint32(m.st.count())
	if _, err := mailindex.Recreate(f.ToRecreateInput(m.path)); err != nil {
		return fmt.Errorf("mdboxmap/write v1: %w", err)
	}
	m.onDisk = FormatV1
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
