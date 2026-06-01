package mdboxmap

import (
	"fmt"
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
// A refcount that would go negative is clamped at 0 — matches
// Dovecot's behaviour and prevents underflow on double-expunge
// from sloppy callers. Callers should not rely on the clamp;
// audit the call site if it triggers.
func (m *Map) UpdateRefcounts(mapUIDs []uint32, delta int16) error {
	if len(mapUIDs) == 0 {
		return nil
	}
	return m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		for _, uid := range mapUIDs {
			idx, ok := m.byMapUID[uid]
			if !ok {
				return fmt.Errorf("mdboxmap/refcount: map_uid %d not found", uid)
			}
			rec := m.f.Records[idx]
			cur := int32(decodeRefExt(rec.Ext[extRef]))
			next := cur + int32(delta)
			if next < 0 {
				next = 0
			}
			if next > 0xFFFF {
				next = 0xFFFF
			}
			rec.Ext[extRef] = encodeRefExt(uint16(next))
		}
		return m.flushLocked()
	})
}

// GetZeroRefFiles returns the set of distinct file_ids that
// contain at least one record with refcount == 0. These are the
// candidate files for purge; the caller picks one (or several)
// and feeds the AppendMove primitive to compact live records
// into a fresh file_id.
func (m *Map) GetZeroRefFiles() ([]uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.f == nil {
		return nil, nil
	}
	seen := make(map[uint32]bool)
	for _, rec := range m.f.Records {
		if decodeRefExt(rec.Ext[extRef]) != 0 {
			continue
		}
		fileID, _, _, err := decodeMapExt(rec.Ext[extMap])
		if err != nil {
			return nil, fmt.Errorf("mdboxmap/zero-ref scan uid=%d: %w", rec.UID, err)
		}
		seen[fileID] = true
	}
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
	if m.f == nil {
		return nil, nil
	}
	var out []MapEntry
	for _, rec := range m.f.Records {
		e, err := recordToEntry(rec)
		if err != nil {
			return nil, fmt.Errorf("mdboxmap/records-in-file: %w", err)
		}
		if e.FileID != fileID {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
