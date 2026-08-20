package director

import "sync"

// Ring delivery is redundant on purpose: in a cycle every member is reachable
// by two paths, so one dead link does not lose an event. Dedup exists to stop
// the copies from being applied twice -- and it has to mean "I have seen THIS
// event", not "I have seen something newer than it". A high-water mark means
// the second thing, and it throws away the very redundancy the ring is built
// for: the backup copy of an event always arrives after the direct copy of the
// next one (#1359).
//
// seenSeqs is the first half of the correction: the sequence numbers already
// applied, remembered per origin and bounded in total.
//
// Two bounds, and they are different things. The staleness threshold is PER
// ORIGIN: an event more than seenWindow behind that origin's highest is treated
// as seen. The memory is bounded ACROSS origins: seenWindow entries in the live
// generation, after which the older generation is dropped whole. Both are
// stated limits rather than accidents -- a member that far behind has a larger
// problem than one missing envelope.
const seenWindow = 4096

type seenSeqs struct {
	mu sync.Mutex
	// per origin: the highest sequence applied, and the ones applied below it.
	// Two generations rather than one map with eviction: the swap bounds memory
	// without walking the map on every insert.
	high map[string]uint64
	cur  map[string]map[uint64]bool
	prev map[string]map[uint64]bool
	n    int
}

func newSeenSeqs() *seenSeqs {
	return &seenSeqs{
		high: map[string]uint64{},
		cur:  map[string]map[uint64]bool{},
		prev: map[string]map[uint64]bool{},
	}
}

// admit reports whether this (origin, seq) has not been applied yet, recording
// it when it has not. A duplicate -- by either path -- is refused; an event that
// arrives after a newer one is admitted, which is the case a high-water mark
// silently dropped.
func (s *seenSeqs) admit(origin string, seq uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if high, ok := s.high[origin]; ok && seq+seenWindow <= high {
		return false // older than the window: assumed seen
	}
	if s.cur[origin][seq] || s.prev[origin][seq] {
		return false
	}
	if s.cur[origin] == nil {
		s.cur[origin] = map[uint64]bool{}
	}
	s.cur[origin][seq] = true
	s.n++
	if seq > s.high[origin] {
		s.high[origin] = seq
	}
	if s.n > seenWindow {
		// Generation swap: the previous set is forgotten whole, so the memory
		// is bounded without a per-insert eviction scan. What is forgotten is
		// older than a full window, which the check above already treats as
		// seen.
		s.prev, s.cur, s.n = s.cur, map[string]map[uint64]bool{}, 0
	}
	return true
}

// orderGuard is the second half. Dedup by identity restores the ring's
// redundancy, but it also removes something the high-water mark was providing
// by accident: events from one origin were applied in ascending order. They no
// longer are -- a recovered copy of event 5 now lands after event 6 -- so every
// handler must either be commutative or refuse an event older than the state it
// would overwrite.
//
// This is the refusal, keyed by the object an event is about rather than by its
// origin: a session id, a user, a kill, a member. Two originators never issue
// events about the same object in a way that would race here (a session belongs
// to one login pod's director, a kill to the member that started it), so one
// counter per object is enough.
type orderGuard struct {
	mu   sync.Mutex
	last map[string]uint64
	prev map[string]uint64
}

func newOrderGuard() *orderGuard {
	return &orderGuard{last: map[string]uint64{}, prev: map[string]uint64{}}
}

// admit reports whether seq is newer than the last event applied for key.
func (g *orderGuard) admit(key string, seq uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if last, ok := g.last[key]; ok && seq <= last {
		return false
	}
	if last, ok := g.prev[key]; ok && seq <= last {
		return false
	}
	g.last[key] = seq
	if len(g.last) > seenWindow {
		g.prev, g.last = g.last, map[string]uint64{}
	}
	return true
}
