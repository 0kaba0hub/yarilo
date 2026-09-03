package mdboxmap

import (
	"fmt"
)

// MovedRecord is one record purge copied into a fresh file. The map_uid is
// preserved so every folder index referencing it stays valid, and GUID must
// carry the original so the map is still queryable by it after compaction.
type MovedRecord struct {
	UID    uint32   // map_uid, unchanged
	FileID uint32   // new file_id
	Offset uint32   // new offset
	Size   uint32   // unchanged
	GUID   [16]byte // original message GUID; zero for pre-GUID records
}

// AppendMove is purge's index-side bookkeeping: repoint the moved, expunge the
// rest, under the map lock for the whole rewrite so a sibling sees all or none.
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

// ExpungeVanished drops every record a storage scan could not read, under the
// map lock the caller already holds -- which is what keeps set and drop consistent.
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

// CompactGarbage returns every map_uid at refcount zero, for a purge driver
// enumerating what the next AppendMove should expunge.
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
