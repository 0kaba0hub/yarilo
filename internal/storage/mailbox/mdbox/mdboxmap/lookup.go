package mdboxmap

import (
	"fmt"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// Lookup resolves one map_uid to its on-disk location and current
// refcount. Returns (entry, true, nil) on success, (_, false, nil)
// when the UID is not present. Reads under m.mu only — no
// cross-process lock — the caller may see a brief stale view
// when a sibling process appends concurrently, but never a torn
// record (mailindex Recreate is atomic .tmp+rename).
func (m *Map) Lookup(mapUID uint32) (MapEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok, err := m.lookupLocked(mapUID)
	if err != nil || ok {
		return entry, ok, err
	}
	// Miss: a sibling process may have appended this map_uid after we cached the
	// map. Refresh (incremental log replay) and retry before reporting absence.
	if rerr := m.reloadLocked(); rerr != nil {
		return MapEntry{}, false, rerr
	}
	return m.lookupLocked(mapUID)
}

func (m *Map) lookupLocked(mapUID uint32) (MapEntry, bool, error) {
	if m.byMapUID == nil {
		return MapEntry{}, false, nil
	}
	idx, ok := m.byMapUID[mapUID]
	if !ok {
		return MapEntry{}, false, nil
	}
	rec := m.f.Records[idx]
	entry, err := recordToEntry(rec)
	if err != nil {
		return MapEntry{}, false, err
	}
	return entry, true, nil
}

// LookupMany resolves a batch of map_uids in a single lock hop.
// Returns one MapEntry per requested UID; entries that did not
// resolve carry UID=0. The result slice mirrors the input order.
func (m *Map) LookupMany(mapUIDs []uint32) ([]MapEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MapEntry, len(mapUIDs))
	refreshed := false
	for i, uid := range mapUIDs {
		e, ok, err := m.lookupLocked(uid)
		if err != nil {
			return nil, fmt.Errorf("mdboxmap/lookup uid=%d: %w", uid, err)
		}
		if !ok && !refreshed {
			// Refresh once on the first miss to pick up a sibling's appends.
			if rerr := m.reloadLocked(); rerr != nil {
				return nil, fmt.Errorf("mdboxmap/lookup refresh: %w", rerr)
			}
			refreshed = true
			e, ok, err = m.lookupLocked(uid)
			if err != nil {
				return nil, fmt.Errorf("mdboxmap/lookup uid=%d: %w", uid, err)
			}
		}
		if ok {
			out[i] = e
		}
	}
	return out, nil
}

// LookupByGUID finds the map_uid for a 128-bit message GUID. Used
// by the rebuild path as the preferred matching strategy: more
// robust than offset matching because GUIDs survive file compaction.
// Returns (entry, true, nil) on success; (_, false, nil) when no
// record carries that GUID (e.g. pre-GUID records have zero GUIDs).
// A zero GUID argument always returns false to prevent accidental
// mass-matches against pre-GUID records.
func (m *Map) LookupByGUID(guid [16]byte) (MapEntry, bool, error) {
	if guid == ([16]byte{}) {
		return MapEntry{}, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.f.Records {
		if decodeGUIDExt(rec.Ext[extGUID]) == guid {
			entry, err := recordToEntry(rec)
			if err != nil {
				return MapEntry{}, false, err
			}
			return entry, true, nil
		}
	}
	return MapEntry{}, false, nil
}

// recordToEntry unpacks a mailindex Record into the typed
// MapEntry surfaced through the Map API.
func recordToEntry(rec *mailindex.Record) (MapEntry, error) {
	fileID, offset, size, err := decodeMapExt(rec.Ext[extMap])
	if err != nil {
		return MapEntry{}, fmt.Errorf("decode map ext: %w", err)
	}
	return MapEntry{
		UID:      rec.UID,
		FileID:   fileID,
		Offset:   offset,
		Size:     size,
		RefCount: decodeRefExt(rec.Ext[extRef]),
		GUID:     decodeGUIDExt(rec.Ext[extGUID]),
	}, nil
}
