package mdboxmap

import "fmt"

// AppendBatch is one in-flight save transaction:
//
//  1. Map.AppendBatch() starts it.
//  2. b.Next(size) per message returns the (file_id, offset) the
//     caller must write the body to at <storage>/m.<file_id>.
//  3. b.Finish() takes the cross-process map lock, allocates
//     map_uids, persists the records, and returns them in
//     Next-allocation order.
//
// Finish MUST be called exactly once. Dropping a batch without
// Finish leaks no on-disk state; only in-flight body bytes are wasted.
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

// AppendRecord allocates a fresh map_uid for one already-written
// record under the map X lock. guid must match the GUID in the dbox
// trailer. Returns the assigned map_uid.
func (m *Map) AppendRecord(fileID, offset, size uint32, guid [16]byte) (uint32, error) {
	uids, err := m.AppendRecords([]RecordLayout{{FileID: fileID, Offset: offset, Size: size, GUID: guid}})
	if err != nil {
		return 0, err
	}
	return uids[0], nil
}

// RecordLayout is one (file_id, offset, size, guid) tuple for
// AppendRecords, describing an already-written body. GUID must match
// the dbox trailer so rebuild can pair physical records with map
// entries by GUID.
type RecordLayout struct {
	FileID uint32
	Offset uint32
	Size   uint32
	GUID   [16]byte
}

// AppendRecords is the batch variant of AppendRecord: one
// cross-process lock hop for all entries, map_uids returned in input
// order. Bumps highest_file_id to max(input.FileID, current) so later
// calls won't re-pick a file_id already in use.
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

// ImportOnce appends the records fn produces, but only if this map is still
// empty -- and it decides that under the map lock, in the same section as the
// append.
//
// For importing another implementation's map into an empty one of ours. Doing
// the same with Records() followed by AppendRecords leaves a window: the check
// is per caller and the append is per caller, so two sessions opening two
// different folders at the same time both see an empty map and both import it.
// The result is every record twice, with a doubled refcount and a
// correspondence that resolves to whichever copy came first.
//
// fn is called only when the import will happen, so a caller that has to read
// another file to build the layouts does not read it on the common path where
// the map is already populated. Returns how many records were appended, and
// zero when somebody else had already done it.
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

// AllocFileID reserves and persists a fresh m.<N> file_id under the
// map X lock. Used by purge/rebuild for compacted bodies; concurrent
// AppendBatch peers see the bumped highest_file_id and route their
// next Save into a higher id. The caller creates the file on disk;
// AllocFileID only advances the in-index counter.
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

// Next reserves a (file_id, offset) for one message of size bytes.
// The first Next in a batch picks file_id = highestFileID (or 1) and
// offset 0. Writes pack into the same file_id until cumulative offset
// would exceed rotateSize, then roll to file_id+1.
//
// Callers needing a different rotation policy drive it themselves by
// interleaving FinishAndFlush() calls.
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

// SetLastGUID records the GUID for the slot from the last Next call.
// Must be called after Next and before the next Next or Finish. GUID
// must match the dbox trailer so rebuild can pair physical records
// with map entries by GUID.
func (b *AppendBatch) SetLastGUID(guid [16]byte) {
	if len(b.pending) == 0 {
		return
	}
	b.pending[len(b.pending)-1].guid = guid
}

// Finish takes the cross-process map lock, assigns map_uids, appends
// one record per Next call, advances highest_file_id, and persists.
// Returns the map_uids in Next-allocation order.
//
// Concurrent peers serialise here: the first to grab the X lock takes
// the lower map_uids; the next reloads and gets the higher range.
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

		// Recompute offsets against the freshly-loaded highestFileID.
		// The caller's relative layout is kept (one continuous block
		// per batch, rotating every rotateSize bytes); only the base
		// file_id shifts.
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
