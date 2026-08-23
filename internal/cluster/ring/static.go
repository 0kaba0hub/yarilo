package ring

import "fmt"

// Static is the director's ring, built from a fixed list and never told
// anything else.
//
// It exists so a deployment without a director can place users the way the
// director would: the same hash, the same folding, the same vhost weights, and
// therefore the same backend for the same user. Adding a director later must
// move nobody, and it cannot if both sides compute placement from one
// implementation -- which is why this is a constructor over Ring rather than a
// second one beside it (#1415).
//
// It has NO health surface, deliberately, and that absence is the design:
//
//	Re-routing a user is a change of mailbox owner. Without shared state, two
//	frontends that disagree about which backend is alive are two owners of one
//	mailbox -- with FTS write handles and index writers on both sides, and the
//	lock service as the only thing between them. A better liveness heuristic
//	only shortens the window in which they disagree; the way not to have the
//	failure is not to create it.
//
// A user whose backend is silent is therefore unavailable, and is told so.
// Anyone who wants a user moved when a backend dies runs a director: the ring
// is what makes that move safe.
type Static struct {
	ring *Ring
}

// NewStatic builds the ring from a fixed list. Every entry is Up: liveness is
// not an input here, and an entry that is present is a placement target.
//
// An empty list is an error rather than an empty ring, because an empty ring
// answers every lookup with "nowhere" -- which reads as a routing decision and
// is a misconfiguration.
func NewStatic(hf HashFormat, backends []Backend) (*Static, error) {
	if len(backends) == 0 {
		return nil, fmt.Errorf("ring: static routing needs at least one backend")
	}
	r := New(hf)
	for i := range backends {
		b := backends[i]
		if b.IP == "" {
			return nil, fmt.Errorf("ring: static backend %d has no address", i)
		}
		b.Up = true
		r.AddBackend(&b)
	}
	return &Static{ring: r}, nil
}

// Lookup returns the backend that owns username, and false when the ring is
// empty. The answer does not depend on anything this process has observed:
// two frontends with the same list answer identically, always.
func (s *Static) Lookup(username string) (Backend, bool) {
	b := s.ring.LookupBackend(username)
	if b == nil {
		return Backend{}, false
	}
	return *b, true
}

// Size reports how many backends the list placed, for a startup log line that
// says what routing was built rather than leaving it to be inferred.
func (s *Static) Size() int { return len(s.ring.backends) }
