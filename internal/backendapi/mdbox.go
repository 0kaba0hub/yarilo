package backendapi

import (
	"net/http"

	mdboxdriver "github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
)

// registerMdboxRoutes wires the mdbox-specific admin surface.
// Only meaningful for users whose storage backend is mdbox; for
// any other driver the endpoint surfaces 400 with the actual
// driver name so the caller can audit configuration.
func (s *Server) registerMdboxRoutes() {
	s.mux.Handle("POST /api/backend/mdbox/purge", s.middleware(s.handleMdboxPurge))
}

type mdboxPurgeRequest struct {
	User      string `json:"user"`
	Namespace string `json:"namespace"`
}

type mdboxPurgeResponse struct {
	FilesScanned    int   `json:"files_scanned"`
	FilesRewritten  int   `json:"files_rewritten"`
	FilesUnlinked   int   `json:"files_unlinked"`
	RecordsKept     int   `json:"records_kept"`
	RecordsExpunged int   `json:"records_expunged"`
	BytesReclaimed  int64 `json:"bytes_reclaimed"`
}

// handleMdboxPurge runs the mdbox driver's Purge() against the
// supplied user. Reclaims disk by compacting every m.<N> that
// holds at least one zero-ref record; the global map is rewritten
// atomically so per-folder indexes referencing live map_uids
// continue to work without per-folder I/O.
func (s *Server) handleMdboxPurge(w http.ResponseWriter, r *http.Request) {
	var req mdboxPurgeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	uc, err := s.openUserContext(req.User)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer uc.Close()

	bundle, err := uc.ns(s, req.Namespace)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}

	type purger interface {
		Purge() (mdboxdriver.PurgeStats, error)
	}
	p, ok := bundle.box.(purger)
	if !ok {
		apiError(w, "purge: storage driver does not implement mdbox purge (only mdbox does)", http.StatusBadRequest)
		return
	}

	stats, err := p.Purge()
	if err != nil {
		apiError(w, "purge: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, mdboxPurgeResponse{
		FilesScanned:    stats.FilesScanned,
		FilesRewritten:  stats.FilesRewritten,
		FilesUnlinked:   stats.FilesUnlinked,
		RecordsKept:     stats.RecordsKept,
		RecordsExpunged: stats.RecordsExpunged,
		BytesReclaimed:  stats.BytesReclaimed,
	})
}
