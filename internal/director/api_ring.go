package director

import (
	"net/http"
)

// apiPeerList returns this replica's ring-status view as this director sees it:
// computed left/right neighbors, per-neighbor live-edge state and uptime, the
// per-member (origin,seq) dedup watermark, and tombstones. A structured object
// (not a bare address list) so a gate can assert topology programmatically.
func (s *Server) apiPeerList(w http.ResponseWriter, _ *http.Request) {
	apiJSON(w, s.membership.Status())
}

// apiPeerAdd/apiPeerRemove: the ring is self-organizing, so there is nothing for
// an admin to force. Membership is driven by DIRECTOR-JOIN against a seed and
// DIRECTOR-REMOVE on detected death. Routes stay registered (rather than 404ing)
// so `yarctl director ring add/remove` gets a clear explanation, not a bare error.
func (s *Server) apiPeerAdd(w http.ResponseWriter, _ *http.Request) {
	apiError(w, "ring membership is self-organizing (#750) — join a node to the ring by pointing its director_service.peers at a seed; there is no manual add", http.StatusGone)
}

func (s *Server) apiPeerRemove(w http.ResponseWriter, _ *http.Request) {
	apiError(w, "ring membership is self-organizing (#750) — a member is removed automatically when its right-neighbor dial fails; there is no manual remove", http.StatusGone)
}
