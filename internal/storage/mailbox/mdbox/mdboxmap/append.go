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
	})
	return fileID, offset
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
					extMap: encodeMapExt(fileID, p.offset, p.size),
					extRef: encodeRefExt(1),
				},
			}
			b.m.f.Records = append(b.m.f.Records, rec)
			b.m.byMapUID[mapUID] = len(b.m.f.Records) - 1
			b.m.nextMapUID++
			mapUIDs[i] = mapUID
		}
		b.m.highestFileID = maxFileID
		return b.m.flushLocked()
	})
	if err != nil {
		return nil, err
	}
	return mapUIDs, nil
}
