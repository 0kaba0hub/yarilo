package mdboxmap

import (
	"fmt"
)

// MovedRecord describes one map record that purge has copied
// from an old m.<file_id> into a fresh one. The original map_uid
// is preserved so every per-folder index that references it
// stays valid; only the physical {file_id, offset} change.
// GUID must carry the original message GUID (from the dbox trailer)
// so the index remains queryable by GUID after compaction.
type MovedRecord struct {
	UID    uint32   // map_uid, unchanged
	FileID uint32   // new file_id
	Offset uint32   // new offset
	Size   uint32   // unchanged
	GUID   [16]byte // original message GUID; zero for pre-GUID records
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
		// 1) Rewrite physical pointers and GUID for moved records.
		for _, mv := range moved {
			i, ok := m.findLocked(mv.UID)
			if !ok {
				return fmt.Errorf("mdboxmap/move: map_uid %d not found", mv.UID)
			}
			e := m.st.at(i)
			e.FileID, e.Offset, e.Size = mv.FileID, mv.Offset, mv.Size
			if mv.GUID != ([16]byte{}) {
				e.GUID = mv.GUID
			}
			m.st.setAt(i, e)
			if mv.FileID > m.highestFileID {
				m.highestFileID = mv.FileID
			}
		}
		// 2) Expunge zero-ref records.
		if len(expunged) > 0 {
			drop := make(map[uint32]bool, len(expunged))
			for _, uid := range expunged {
				if _, ok := m.findLocked(uid); !ok {
					return fmt.Errorf("mdboxmap/move: expunge target %d not found", uid)
				}
				drop[uid] = true
			}
			m.st.removeFunc(func(uid uint32) bool { return drop[uid] })
		}
		return m.flushLocked()
	})
}

// ExpungeVanished drops every map record whose map_uid is absent from
// presentUIDs — the set of map_uids a physical storage scan could actually read.
// It is the map-side of a storage-wide rebuild: a record the scan could not find
// points at a message that no longer exists on disk, so its pointer is removed
// (the message bytes, if any linger, are reclaimed by the next purge). Runs the
// one-pass drop + byMapUID rebuild + atomic flush under the map X-lock, mirroring
// AppendMove's expunge path. Returns the number of records dropped.
//
// The caller (mdbox storage-wide rebuild) already holds the map lock, so the
// re-entrant withMapLock keeps the present-set and the drop consistent.
func (m *Map) ExpungeVanished(presentUIDs map[uint32]bool) (int, error) {
	dropped := 0
	err := m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		dropped = m.st.removeFunc(func(uid uint32) bool { return !presentUIDs[uid] })
		if dropped == 0 {
			return nil
		}
		return m.flushLocked()
	})
	if err != nil {
		return 0, err
	}
	return dropped, nil
}

// CompactGarbage walks the live record set and returns the list
// of map_uids whose refcount is zero. Convenience for purge
// drivers that want to enumerate everything that should be
// expunged in the next AppendMove call.
func (m *Map) CompactGarbage() []uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		return nil
	}
	var out []uint32
	m.st.each(func(e MapEntry) {
		if e.RefCount == 0 {
			out = append(out, e.UID)
		}
	})
	return out
}
