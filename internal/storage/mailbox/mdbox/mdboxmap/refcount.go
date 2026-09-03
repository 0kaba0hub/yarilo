package mdboxmap

import (
	"fmt"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// UpdateRefcounts applies a delta to every listed map_uid under one lock hop. A
// missing uid is an error; a negative result is clamped, and reaching the clamp
// is a bug at the call site.
func (m *Map) UpdateRefcounts(mapUIDs []uint32, delta int16) error {
	if len(mapUIDs) == 0 {
		return nil
	}
	return m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		deltas := make([]mailindex.TxExtAtomicInc, 0, len(mapUIDs))
		for _, uid := range mapUIDs {
			i, ok := m.findLocked(uid)
			if !ok {
				return fmt.Errorf("mdboxmap/refcount: map_uid %d not found", uid)
			}
			e := m.st.at(i)
			e.RefCount = clampRef(int32(e.RefCount) + int32(delta))
			m.st.setAt(i, e)
			deltas = append(deltas, mailindex.TxExtAtomicInc{UID: uid, Diff: int32(delta)})
		}
		// Appended, not rewritten: every save and delete changes a refcount, so
		// a base rewrite here priced one operation at a full file (#1205).
		if err := m.appendRefcountLogLocked(deltas); err != nil {
			// The records changed but the write did not happen, and nothing on
			// disk moved -- so the next reload fast-paths and keeps the phantom
			// value, which for a decrement is what the purge scan reads.
			m.invalidateLocked()
			return err
		}
		return nil
	})
}

// SetRefcountsFromReferences rewrites every refcount from refs, zero for a record
// it does not name, so an unreferenced one is reclaimed rather than resurrected.
func (m *Map) SetRefcountsFromReferences(refs map[uint32]int) (int, error) {
	zeroed := 0
	err := m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		zeroed = 0
		for i, count := 0, m.st.count(); i < count; i++ {
			e := m.st.at(i)
			n := refs[e.UID]
			if n < 0 {
				n = 0
			}
			if n > 0xFFFF {
				n = 0xFFFF
			}
			if n == 0 {
				zeroed++
			}
			e.RefCount = uint16(n)
			m.st.setAt(i, e)
		}
		return m.flushLocked()
	})
	if err != nil {
		return 0, err
	}
	return zeroed, nil
}

// SetGUIDs stamps a GUID onto records carrying none, taking it from the folder
// index of a converted store rather than from a walk over every storage file. A
// record that already has one is left alone.
func (m *Map) SetGUIDs(guids map[uint32][16]byte) (int, error) {
	if len(guids) == 0 {
		return 0, nil
	}
	var zero [16]byte
	written := 0
	err := m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		written = 0
		for i, count := 0, m.st.count(); i < count; i++ {
			e := m.st.at(i)
			g, ok := guids[e.UID]
			if !ok || g == zero || e.GUID != zero {
				continue
			}
			e.GUID = g
			m.st.setAt(i, e)
			written++
		}
		if written == 0 {
			return nil
		}
		return m.flushLocked()
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// GetZeroRefFiles returns the file_ids holding at least one zero-refcount
// record -- purge candidates, which the caller compacts through AppendMove.
func (m *Map) GetZeroRefFiles() ([]uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		return nil, nil
	}
	// Refcounts live in the log until it is compacted, and this answer decides
	// what purge physically deletes: reading the base alone would offer a file
	// whose count was raised moments ago.
	if err := m.reloadLocked(); err != nil {
		return nil, fmt.Errorf("mdboxmap/zero-ref: %w", err)
	}
	seen := make(map[uint32]bool)
	m.st.each(func(e MapEntry) {
		if e.RefCount == 0 {
			seen[e.FileID] = true
		}
	})
	out := make([]uint32, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out, nil
}

// RecordsInFile returns every entry living in one physical file, for purge to
// split into what it copies forward and what it expunges.
func (m *Map) RecordsInFile(fileID uint32) ([]MapEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		return nil, nil
	}
	// Same reason as GetZeroRefFiles: purge copies forward what is referenced
	// and drops the rest, from these counts.
	if err := m.reloadLocked(); err != nil {
		return nil, fmt.Errorf("mdboxmap/records-in-file: %w", err)
	}
	var out []MapEntry
	m.st.each(func(e MapEntry) {
		if e.FileID == fileID {
			out = append(out, e)
		}
	})
	return out, nil
}
