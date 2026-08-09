package mdboxmap

import (
	"fmt"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// UpdateRefcounts applies a signed delta to the refcount of every
// listed map_uid. Used for IMAP COPY (+1) and EXPUNGE (-1).
//
// All UIDs are updated under a single cross-process lock hop, so
// the operation is atomic from a sibling process's point of view:
// either every update is visible or none.
//
// Missing UIDs are reported as an error — a refcount update for a
// non-existent map_uid is always a caller bug (stale folder
// record pointing at a purged map entry).
//
// A refcount that would go negative is clamped at 0 to prevent
// underflow on double-expunge from sloppy callers. Callers
// should not rely on the clamp; audit the call site if it
// triggers.
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
		// Appended, not rewritten. Every save and every delete changes a
		// refcount, so rewriting the whole base index here made a full file
		// rewrite the price of one operation -- and every sibling process then
		// had to re-open the base it invalidated (#1205).
		if err := m.appendRefcountLogLocked(deltas); err != nil {
			// The records above are already changed, and the write that would
			// have made them durable did not happen: neither the base mtime
			// nor the log size moved, so the next reload would take the fast
			// path and keep the phantom value. For a decrement that value is
			// below the truth, and the purge scan reads exactly this state.
			m.invalidateLocked()
			return err
		}
		return nil
	})
}

// SetRefcountsFromReferences rewrites every live map record's refcount to the
// number of folder references reported in refs (map_uid → reference count),
// defaulting to 0 for any record not present in refs. This is the map-side of a
// storage-wide rebuild's "recompute refcounts from actual references" step:
// after the folder indexes are reconciled, a record
// referenced by no folder gets refcount 0 so the next purge reclaims it — no
// stale refcount>0 lingers to trip the next rebuild, and no unreferenced message
// is silently resurrected. Returns the number of records set to 0 (the
// unreferenced-but-still-present count) for reporting. Runs under the map X-lock.
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

// GetZeroRefFiles returns the set of distinct file_ids that
// contain at least one record with refcount == 0. These are the
// candidate files for purge; the caller picks one (or several)
// and feeds the AppendMove primitive to compact live records
// into a fresh file_id.
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

// RecordsInFile returns every MapEntry whose data lives in the
// given physical m.<file_id>. Used by the purge driver to decide
// which records to copy forward (refcount > 0) and which to
// expunge (refcount == 0). Read-only.
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
