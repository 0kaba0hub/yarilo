package mdboxmap

import (
	"fmt"

	"github.com/0kaba0hub/yarilo/internal/storage/mailindex"
)

// AppendBatch is one in-flight save transaction. Callers:
//
//  1. Call Map.AppendBatch() to start.
//  2. For every new message they want to write to the storage
//     tree, call b.Next(size). Next returns the (file_id, offset)
//     the message body must be written to. The caller is
//     responsible for actually copying bytes to
//     <storage>/m.<file_id> at offset.
//  3. After all bodies are on disk, call b.Finish(). Finish takes
//     the cross-process map lock, allocates map_uids, persists
//     the records, and returns the slice of assigned map_uids
//     in the order they were Next-allocated.
//
// Finish MUST be called exactly once. An AppendBatch that is
// dropped without Finish leaks no on-disk state — only the
// caller's in-flight m.<N> body bytes are wasted.
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

// rotateSize is the per-m.<N> physical-file size cap before
// the next Next() call rolls to a fresh file_id. 2 MiB matches
// the canonical mdbox_rotate_size default.
const rotateSize uint32 = 2 * 1024 * 1024

// AppendBatch begins a new save batch. The returned batch is
// goroutine-affine: do not share between goroutines.
func (m *Map) AppendBatch() *AppendBatch {
	return &AppendBatch{m: m}
}

// AppendRecord allocates a fresh map_uid for one record under
// the map X lock. The caller has already written the record to
// m.<fileID> at offset and now needs the index to learn about
// it. guid must match the GUID written into the dbox trailer.
// Returns the assigned map_uid.
func (m *Map) AppendRecord(fileID, offset, size uint32, guid [16]byte) (uint32, error) {
	uids, err := m.AppendRecords([]RecordLayout{{FileID: fileID, Offset: offset, Size: size, GUID: guid}})
	if err != nil {
		return 0, err
	}
	return uids[0], nil
}

// RecordLayout is one (file_id, offset, size, guid) tuple passed to
// AppendRecords. The caller has already written the body at this
// location; the map records the pointer + GUID and assigns a map_uid.
// GUID must be the same 128-bit value written into the dbox trailer
// so rebuild can pair physical records with map entries via GUID match.
type RecordLayout struct {
	FileID uint32
	Offset uint32
	Size   uint32
	GUID   [16]byte
}

// AppendRecords is the batch variant of AppendRecord — same
// semantics, one cross-process lock hop for all entries. Returns
// the assigned map_uids in input order. Bumps highest_file_id
// to max(input.FileID, current) so later calls won't re-pick a
// file_id already in use.
func (m *Map) AppendRecords(layouts []RecordLayout) ([]uint32, error) {
	if len(layouts) == 0 {
		return nil, nil
	}
	out := make([]uint32, len(layouts))
	err := m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		for i, l := range layouts {
			mapUID := m.nextMapUID
			rec := &mailindex.Record{
				UID: mapUID,
				Ext: map[string][]byte{
					extMap:  encodeMapExt(l.FileID, l.Offset, l.Size),
					extRef:  encodeRefExt(1),
					extGUID: encodeGUIDExt(l.GUID),
				},
			}
			m.f.Records = append(m.f.Records, rec)
			m.byMapUID[mapUID] = len(m.f.Records) - 1
			m.nextMapUID++
			if l.FileID > m.highestFileID {
				m.highestFileID = l.FileID
			}
			out[i] = mapUID
		}
		return m.commitAppendLocked(len(layouts))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AllocFileID reserves and persists a fresh m.<N> physical
// file_id under the map X lock. Used by purge / rebuild to write
// compacted bodies into a known-unique file_id; concurrent
// AppendBatch peers see the bumped highest_file_id and route
// their next Save into an even higher id.
//
// The caller is responsible for actually creating the
// m.<returned id> file on disk; AllocFileID only updates the
// in-index counter.
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

// Next reserves a (file_id, offset) for one message of `size`
// bytes. The first Next call in a batch picks file_id =
// highestFileID (or 1 if zero) and offset = current size of that
// file (looked up by the caller via os.Stat on the actual
// m.<file_id> file). For simplicity in this Phase-4 helper we
// pack writes into the SAME file_id until cumulative offset
// would exceed rotateSize; then we roll to file_id+1.
//
// Callers that need fancier rotation policies (e.g. respect a
// caller-supplied per-physical-file size cap) should drive the
// rotation themselves by interleaving FinishAndFlush() calls.
func (b *AppendBatch) Next(size uint32) (fileID, offset uint32) {
	b.m.mu.Lock()
	defer b.m.mu.Unlock()
	// First slot of the batch — start at the current high water.
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
		if offset+size > rotateSize {
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

// SetLastGUID records the GUID for the most recently reserved slot
// (the slot returned by the last Next call). Must be called after
// Next and before the next Next or Finish. The GUID is the same
// 128-bit value written into the dbox trailer so rebuild can pair
// physical records with map entries via GUID match.
func (b *AppendBatch) SetLastGUID(guid [16]byte) {
	if len(b.pending) == 0 {
		return
	}
	b.pending[len(b.pending)-1].guid = guid
}

// Finish takes the cross-process map lock, assigns map_uids,
// appends one map record per Next call, advances
// highest_file_id, and persists. Returns the slice of map_uids
// in Next-allocation order.
//
// Concurrent peers serialise here: the first writer to grab the
// X lock takes the lower map_uids; the next reloads the fresh
// state and gets the higher range.
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
		// Refresh the on-disk view — a sibling process may have
		// raced and burned the file_id / map_uid range we
		// reserved on Next() while we were writing bodies.
		if err := b.m.reloadLocked(); err != nil {
			return err
		}

		// Recompute physical offsets against the freshly-loaded
		// highestFileID. We keep the relative layout the caller
		// built (one continuous block per batch, rotating every
		// rotateSize bytes); only the base file_id shifts.
		baseDelta := int64(0)
		if b.pending[0].fileID > 0 {
			baseDelta = int64(b.m.highestFileID) - int64(b.pending[0].fileID)
			if b.m.highestFileID == 0 {
				baseDelta = 1 - int64(b.pending[0].fileID)
			}
		}
		// Append records under freshly-assigned map_uids.
		mapUIDs = make([]uint32, len(b.pending))
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
			mapUID := b.m.nextMapUID
			rec := &mailindex.Record{
				UID: mapUID,
				Ext: map[string][]byte{
					extMap:  encodeMapExt(fileID, p.offset, p.size),
					extRef:  encodeRefExt(1),
					extGUID: encodeGUIDExt(p.guid),
				},
			}
			b.m.f.Records = append(b.m.f.Records, rec)
			b.m.byMapUID[mapUID] = len(b.m.f.Records) - 1
			b.m.nextMapUID++
			mapUIDs[i] = mapUID
		}
		b.m.highestFileID = maxFileID
		return b.m.commitAppendLocked(len(b.pending))
	})
	if err != nil {
		return nil, err
	}
	return mapUIDs, nil
}
