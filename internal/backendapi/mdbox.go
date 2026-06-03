package backendapi

import (
	"net/http"
	"time"

	mdboxdriver "github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
)

// registerMdboxRoutes wires the mdbox-specific admin surface.
// Only meaningful for users whose storage backend is mdbox; for
// any other driver the endpoint surfaces 400 with the actual
// driver name so the caller can audit configuration.
func (s *Server) registerMdboxRoutes() {
	s.mux.Handle("POST /api/backend/mdbox/purge", s.middleware(s.handleMdboxPurge))
	s.mux.Handle("POST /api/backend/mdbox/altmove", s.middleware(s.handleMdboxAltMove))
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

type mdboxAltMoveRequest struct {
	User      string `json:"user"`
	Namespace string `json:"namespace"`
	// Before is an RFC 3339 timestamp; messages with InternalDate
	// strictly before this time are eligible for alt-move.
	// Empty string means "all messages".
	Before  string `json:"before"`
	Reverse bool   `json:"reverse"`
}

type mdboxAltMoveResponse struct {
	Candidates    int   `json:"candidates"`
	Moved         int   `json:"moved"`
	FilesCreated  int   `json:"files_created"`
	FilesUnlinked int   `json:"files_unlinked"`
	BytesMoved    int64 `json:"bytes_moved"`
}

// handleMdboxAltMove is the yarilo equivalent of `doveadm altmove`.
// It moves messages from primary storage to alt (cold) storage or
// vice versa (Reverse=true), filtered by InternalDate < Before.
// Mirrors Dovecot's doveadm-mail-altmove.c flow: mark + purge in
// one atomic call per file.
func (s *Server) handleMdboxAltMove(w http.ResponseWriter, r *http.Request) {
	var req mdboxAltMoveRequest
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

	type altMover interface {
		AltMove(mdboxdriver.AltMoveQuery) (mdboxdriver.AltMoveStats, error)
		AltEnabled() bool
	}
	am, ok := bundle.box.(altMover)
	if !ok {
		apiError(w, "altmove: storage driver does not implement mdbox altmove", http.StatusBadRequest)
		return
	}
	if !am.AltEnabled() {
		apiError(w, "altmove: alt storage not configured (set storage.mdbox_alt_storage_path)", http.StatusBadRequest)
		return
	}

	q := mdboxdriver.AltMoveQuery{Reverse: req.Reverse}
	if req.Before != "" {
		t, err := time.Parse(time.RFC3339, req.Before)
		if err != nil {
			apiError(w, "altmove: invalid before timestamp (use RFC3339): "+err.Error(), http.StatusBadRequest)
			return
		}
		q.Before = t
	}

	stats, err := am.AltMove(q)
	if err != nil {
		apiError(w, "altmove: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, mdboxAltMoveResponse{
		Candidates:    stats.Candidates,
		Moved:         stats.Moved,
		FilesCreated:  stats.FilesCreated,
		FilesUnlinked: stats.FilesUnlinked,
		BytesMoved:    stats.BytesMoved,
	})
}
