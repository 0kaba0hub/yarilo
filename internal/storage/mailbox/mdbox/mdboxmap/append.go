package mdboxmap

import "fmt"

// AppendBatch is one in-flight save: Next(size) returns where to write a body and
// Finish allocates the map_uids. Finish MUST be called exactly once.
type AppendBatch struct {
	m        *Map
	pending  []pendingEntry
	finished bool
}

type pendingEntry struct {
	fileID uint32
	offset uint32
	size   uint32
	guid   [16]byte
}

// AppendBatch begins a new save batch. The returned batch is
// goroutine-affine: do not share between goroutines.
func (m *Map) AppendBatch() *AppendBatch {
	return &AppendBatch{m: m}
}

// AppendRecord allocates a map_uid for one already-written record under the map
// lock. guid must match the dbox trailer.
func (m *Map) AppendRecord(fileID, offset, size uint32, guid [16]byte) (uint32, error) {
	uids, err := m.AppendRecords([]RecordLayout{{FileID: fileID, Offset: offset, Size: size, GUID: guid}})
	if err != nil {
		return 0, err
	}
	return uids[0], nil
}

// RecordLayout describes one already-written body for AppendRecords. GUID must
// match the dbox trailer, or a rebuild cannot pair it with its map entry.
type RecordLayout struct {
	FileID uint32
	Offset uint32
	Size   uint32
	GUID   [16]byte
}

// AppendRecords is AppendRecord for a batch: one lock hop, map_uids in input
// order, and highest_file_id raised so a later call cannot re-pick a used id.
func (m *Map) AppendRecords(layouts []RecordLayout) ([]uint32, error) {
	if len(layouts) == 0 {
		return nil, nil
	}
	out := make([]uint32, len(layouts))
	err := m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		added := make([]MapEntry, 0, len(layouts))
		for i, l := range layouts {
			e := MapEntry{UID: m.nextMapUID, FileID: l.FileID, Offset: l.Offset, Size: l.Size, RefCount: 1, GUID: l.GUID}
			m.st.insert(e)
			added = append(added, e)
			m.nextMapUID++
			if l.FileID > m.highestFileID {
				m.highestFileID = l.FileID
			}
			out[i] = e.UID
		}
		return m.commitAppendLocked(added)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ImportOnce appends what fn produces only if the map is still empty, decided in
// the same locked section: taken separately, two sessions both import.
func (m *Map) ImportOnce(fn func() ([]RecordLayout, error)) (int, error) {
	var n int
	err := m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		if m.st.count() > 0 {
			return nil
		}
		layouts, err := fn()
		if err != nil {
			return err
		}
		if len(layouts) == 0 {
			return nil
		}
		added := make([]MapEntry, 0, len(layouts))
		for _, l := range layouts {
			e := MapEntry{UID: m.nextMapUID, FileID: l.FileID, Offset: l.Offset, Size: l.Size, RefCount: 1, GUID: l.GUID}
			m.st.insert(e)
			added = append(added, e)
			m.nextMapUID++
			if l.FileID > m.highestFileID {
				m.highestFileID = l.FileID
			}
		}
		n = len(added)
		return m.commitAppendLocked(added)
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// WithLock runs fn under the map lock, for work that must not interleave with an
// append -- removing a map a concurrent import would be reading (#1569).
func (m *Map) WithLock(fn func() error) error { return m.withMapLock(fn) }

// AllocFileID persists a fresh file_id under the map lock so a peer routes its
// next Save higher. The caller creates the file.
func (m *Map) AllocFileID() (uint32, error) {
	var assigned uint32
	err := m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		next := m.highestFileID + 1
		if next == 0 {
			next = 1
		}
		m.highestFileID = next
		assigned = next
		return m.flushLocked()
	})
	return assigned, err
}

// Next reserves a (file_id, offset), packing into one file until rotateSize would
// be passed. Another rotation policy is driven by interleaving FinishAndFlush.
func (b *AppendBatch) Next(size uint32) (fileID, offset uint32) {
	b.m.mu.Lock()
	defer b.m.mu.Unlock()
	// First slot of the batch: start at the current high water.
	if len(b.pending) == 0 {
		fileID = b.m.highestFileID
		if fileID == 0 {
			fileID = 1
		}
		offset = 0
	} else {
		last := b.pending[len(b.pending)-1]
		fileID = last.fileID
		offset = last.offset + last.size
		if offset+size > b.m.rotateSizeOrDefault() {
			fileID++
			offset = 0
		}
	}
	b.pending = append(b.pending, pendingEntry{
		fileID: fileID,
		offset: offset,
		size:   size,
		// guid is set by SetLastGUID after the caller writes the body.
	})
	return fileID, offset
}

// SetLastGUID records the GUID for the slot the last Next returned, between that
// Next and the following one. It must match the dbox trailer.
func (b *AppendBatch) SetLastGUID(guid [16]byte) {
	if len(b.pending) == 0 {
		return
	}
	b.pending[len(b.pending)-1].guid = guid
}

// Finish assigns and persists the map_uids under the lock, in allocation order.
// Peers serialise here: the first to take the lock gets the lower range.
func (b *AppendBatch) Finish() ([]uint32, error) {
	if b.finished {
		return nil, fmt.Errorf("mdboxmap/finish: batch already finished")
	}
	b.finished = true
	if len(b.pending) == 0 {
		return nil, nil
	}
	var mapUIDs []uint32
	err := b.m.withMapLock(func() error {
		// Refresh: a sibling process may have burned the file_id /
		// map_uid range we reserved on Next() while writing bodies.
		if err := b.m.reloadLocked(); err != nil {
			return err
		}

		// Offsets recomputed against the reloaded highestFileID; the caller's
		// relative layout is kept and only the base file_id shifts.
		baseDelta := int64(0)
		if b.pending[0].fileID > 0 {
			baseDelta = int64(b.m.highestFileID) - int64(b.pending[0].fileID)
			if b.m.highestFileID == 0 {
				baseDelta = 1 - int64(b.pending[0].fileID)
			}
		}
		// Append records under freshly-assigned map_uids.
		mapUIDs = make([]uint32, len(b.pending))
		added := make([]MapEntry, 0, len(b.pending))
		maxFileID := b.m.highestFileID
		if maxFileID == 0 {
			maxFileID = 1
		}
		for i, p := range b.pending {
			fileID := uint32(int64(p.fileID) + baseDelta)
			if fileID == 0 {
				fileID = 1
			}
			if fileID > maxFileID {
				maxFileID = fileID
			}
			e := MapEntry{UID: b.m.nextMapUID, FileID: fileID, Offset: p.offset, Size: p.size, RefCount: 1, GUID: p.guid}
			b.m.st.insert(e)
			added = append(added, e)
			b.m.nextMapUID++
			mapUIDs[i] = e.UID
		}
		b.m.highestFileID = maxFileID
		return b.m.commitAppendLocked(added)
	})
	if err != nil {
		return nil, err
	}
	return mapUIDs, nil
}
