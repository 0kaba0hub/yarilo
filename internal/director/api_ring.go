package director

import (
	"net/http"
)

// apiPeerList returns this replica's rich ring-status view (#833): computed
// left/right neighbors, per-neighbor live-edge state + uptime, the sparse
// (origin,seq) dedup watermark per member, and tombstones — all from the
// self-organized membership state (#750), as THIS director sees it. A
// structured object (not a bare address list) so a gate can assert topology
// programmatically.
func (s *Server) apiPeerList(w http.ResponseWriter, _ *http.Request) {
	apiJSON(w, s.membership.Status())
}

// apiPeerAdd/apiPeerRemove previously forced a peer into/out of a static
// full-mesh list. The ring is self-organizing now (#750): membership is
// driven by DIRECTOR-JOIN against a seed and DIRECTOR-REMOVE on detected
// death — there is nothing left for an admin to force. Routes stay
// registered (rather than 404ing) so `yarilo-admin director ring add/remove`
// gets a clear explanation instead of a bare connection/method error.
func (s *Server) apiPeerAdd(w http.ResponseWriter, _ *http.Request) {
	apiError(w, "ring membership is self-organizing (#750) — join a node to the ring by pointing its director_service.peers at a seed; there is no manual add", http.StatusGone)
}

func (s *Server) apiPeerRemove(w http.ResponseWriter, _ *http.Request) {
	apiError(w, "ring membership is self-organizing (#750) — a member is removed automatically when its right-neighbor dial fails; there is no manual remove", http.StatusGone)
}
