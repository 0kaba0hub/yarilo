package mdboxmap

import "sort"

// store is the v2 base in memory: the record area as raw bytes sorted by map_uid,
// read through offset arithmetic, so loading the map is a read and nothing else.
type store struct {
	recs []byte
}

func (s *store) count() int { return len(s.recs) / baseRecordLen }

func (s *store) at(i int) MapEntry { return getRecord(s.recs[i*baseRecordLen:]) }

func (s *store) setAt(i int, e MapEntry) { putRecord(s.recs[i*baseRecordLen:], e) }

// find returns the index of uid, or the index it would be inserted at.
func (s *store) find(uid uint32) (int, bool) {
	n := s.count()
	i := sort.Search(n, func(i int) bool { return recordUID(s.recs[i*baseRecordLen:]) >= uid })
	if i < n && recordUID(s.recs[i*baseRecordLen:]) == uid {
		return i, true
	}
	return i, false
}

// insert places e in map_uid order. Appends are the norm — map_uids are
// allocated monotonically under the map lock — so the tail case is the fast
// one, but the ordered insert is what the binary search actually depends on.
func (s *store) insert(e MapEntry) {
	buf := make([]byte, baseRecordLen)
	putRecord(buf, e)
	n := s.count()
	if n == 0 || recordUID(s.recs[(n-1)*baseRecordLen:]) < e.UID {
		s.recs = append(s.recs, buf...)
		return
	}
	i, found := s.find(e.UID)
	if found {
		copy(s.recs[i*baseRecordLen:], buf)
		return
	}
	s.recs = append(s.recs, make([]byte, baseRecordLen)...)
	copy(s.recs[(i+1)*baseRecordLen:], s.recs[i*baseRecordLen:len(s.recs)-baseRecordLen])
	copy(s.recs[i*baseRecordLen:], buf)
}

// removeFunc drops every record for which drop reports true, preserving order.
// Returns how many were dropped.
func (s *store) removeFunc(drop func(uid uint32) bool) int {
	n := s.count()
	kept := 0
	for i := 0; i < n; i++ {
		if drop(recordUID(s.recs[i*baseRecordLen:])) {
			continue
		}
		if kept != i {
			copy(s.recs[kept*baseRecordLen:(kept+1)*baseRecordLen], s.recs[i*baseRecordLen:(i+1)*baseRecordLen])
		}
		kept++
	}
	s.recs = s.recs[:kept*baseRecordLen]
	return n - kept
}

// each walks every record in map_uid order.
func (s *store) each(fn func(e MapEntry)) {
	for i, n := 0, s.count(); i < n; i++ {
		fn(s.at(i))
	}
}
