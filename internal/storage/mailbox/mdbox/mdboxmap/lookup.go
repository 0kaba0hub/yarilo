package mdboxmap

import (
	"fmt"
	"sync"
	"time"
)

// lockRead takes the in-process map mutex and records the wait. A read needs no
// cross-process lock, but it queues behind a writer that is waiting for one --
// which is the whole question in #1205, and unanswerable without this number.
func lockRead(mu *sync.Mutex) {
	start := time.Now()
	mu.Lock()
	metricMapReadBlocked.Observe(time.Since(start).Seconds())
}

// Lookup resolves one map_uid to its on-disk location and current
// refcount. Returns (entry, true, nil) on success, (_, false, nil)
// when the UID is not present. Reads under m.mu only — no
// cross-process lock — the caller may see a brief stale view
// when a sibling process appends concurrently, but never a torn
// record (the base is replaced atomically by .tmp+rename).
func (m *Map) Lookup(mapUID uint32) (MapEntry, bool, error) {
	lockRead(&m.mu)
	defer m.mu.Unlock()
	if e, ok := m.lookupLocked(mapUID); ok {
		return e, true, nil
	}
	// Miss: a sibling process may have appended this map_uid after we cached the
	// map. Refresh (incremental log replay) and retry before reporting absence.
	if rerr := m.reloadLocked(); rerr != nil {
		return MapEntry{}, false, rerr
	}
	e, ok := m.lookupLocked(mapUID)
	return e, ok, nil
}

func (m *Map) lookupLocked(mapUID uint32) (MapEntry, bool) {
	i, ok := m.findLocked(mapUID)
	if !ok {
		return MapEntry{}, false
	}
	return m.st.at(i), true
}

// Records returns every record the map currently holds, in map order.
//
// For pairing a whole map against another one, where a lookup per record would
// take the lock once per record and still have to be told what to look for. The
// slice is a copy; the caller may keep it.
func (m *Map) Records() []MapEntry {
	lockRead(&m.mu)
	defer m.mu.Unlock()
	out := make([]MapEntry, 0, m.st.count())
	for i, n := 0, m.st.count(); i < n; i++ {
		out = append(out, m.st.at(i))
	}
	return out
}

// LookupMany resolves a batch of map_uids in a single lock hop.
// Returns one MapEntry per requested UID; entries that did not
// resolve carry UID=0. The result slice mirrors the input order.
func (m *Map) LookupMany(mapUIDs []uint32) ([]MapEntry, error) {
	lockRead(&m.mu)
	defer m.mu.Unlock()
	out := make([]MapEntry, len(mapUIDs))
	refreshed := false
	for i, uid := range mapUIDs {
		e, ok := m.lookupLocked(uid)
		if !ok && !refreshed {
			// Refresh once on the first miss to pick up a sibling's appends.
			if rerr := m.reloadLocked(); rerr != nil {
				return nil, fmt.Errorf("mdboxmap/lookup refresh: %w", rerr)
			}
			refreshed = true
			e, ok = m.lookupLocked(uid)
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
	for i, n := 0, m.st.count(); i < n; i++ {
		if e := m.st.at(i); e.GUID == guid {
			return e, true, nil
		}
	}
	return MapEntry{}, false, nil
}
