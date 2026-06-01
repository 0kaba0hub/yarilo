package mdboxmap

import (
	"fmt"
)

// MovedRecord describes one map record that purge has copied
// from an old m.<file_id> into a fresh one. The original map_uid
// is preserved so every per-folder index that references it
// stays valid; only the physical {file_id, offset} change.
type MovedRecord struct {
	UID    uint32 // map_uid, unchanged
	FileID uint32 // new file_id
	Offset uint32 // new offset
	Size   uint32 // unchanged
}

// AppendMove is the purge-driver primitive. It atomically:
//
//   - rewrites map records for every UID in `moved`, pointing
//     them at their new physical location ({FileID, Offset})
//   - expunges records for every UID in `expunged`
//
// Both lists may reference UIDs scattered across any number of
// source m.<N> files — this is just the index-side bookkeeping.
// The actual bytes-on-disk work (writing the new m.<N> file,
// unlinking the old ones) is the purge driver's job and lives in
// Phase 6.
//
// Atomic from a sibling process's view: holds the map X lock
// for the whole rewrite + Recreate.
func (m *Map) AppendMove(moved []MovedRecord, expunged []uint32) error {
	if len(moved) == 0 && len(expunged) == 0 {
		return nil
	}
	return m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		// 1) Rewrite physical pointers for moved records.
		for _, mv := range moved {
			idx, ok := m.byMapUID[mv.UID]
			if !ok {
				return fmt.Errorf("mdboxmap/move: map_uid %d not found", mv.UID)
			}
			rec := m.f.Records[idx]
			rec.Ext[extMap] = encodeMapExt(mv.FileID, mv.Offset, mv.Size)
			if mv.FileID > m.highestFileID {
				m.highestFileID = mv.FileID
			}
		}
		// 2) Expunge zero-ref records. Build a set so we can
		//    rebuild the records slice in one pass; preserves
		//    relative order for everything else.
		if len(expunged) > 0 {
			drop := make(map[uint32]bool, len(expunged))
			for _, uid := range expunged {
				if _, ok := m.byMapUID[uid]; !ok {
					return fmt.Errorf("mdboxmap/move: expunge target %d not found", uid)
				}
				drop[uid] = true
			}
			kept := m.f.Records[:0]
			for _, rec := range m.f.Records {
				if drop[rec.UID] {
					continue
				}
				kept = append(kept, rec)
			}
			m.f.Records = kept
		}
		// 3) Rebuild byMapUID since indexes shifted.
		m.byMapUID = make(map[uint32]int, len(m.f.Records))
		for i, rec := range m.f.Records {
			m.byMapUID[rec.UID] = i
		}
		return m.flushLocked()
	})
}

// CompactGarbage walks the live record set and returns the list
// of map_uids whose refcount is zero. Convenience for purge
// drivers that want to enumerate everything that should be
// expunged in the next AppendMove call.
func (m *Map) CompactGarbage() []uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.f == nil {
		return nil
	}
	var out []uint32
	for _, rec := range m.f.Records {
		if decodeRefExt(rec.Ext[extRef]) == 0 {
			out = append(out, rec.UID)
		}
	}
	return out
}
