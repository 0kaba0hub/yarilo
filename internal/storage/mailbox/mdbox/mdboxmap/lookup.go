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

// Lookup resolves one map_uid to its location and refcount, under m.mu only: a
// sibling's concurrent append can leave the view briefly stale, never torn,
// since the base is replaced atomically.
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

// Records returns every record in map order, for pairing a whole map against
// another one. The slice is a copy; the caller may keep it.
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

// LookupByGUID finds the map_uid for a message GUID -- the rebuild's preferred
// match, since a GUID survives compaction where an offset does not. A zero GUID
// always returns false, or it would mass-match every pre-GUID record.
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
