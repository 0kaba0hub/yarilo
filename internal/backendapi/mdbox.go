package backendapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	mdboxdriver "github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// registerMdboxRoutes registers mdbox-specific admin routes.
// Non-mdbox drivers get 400.
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

// handleMdboxPurge runs mdbox Purge(): compacts m.<N> files with
// zero-ref records; the map rewrite is atomic so live map_uids
// stay valid.
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
	p, ok := mailbox.Driver(bundle.box).(purger)
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
	// Before (RFC 3339): only messages with InternalDate strictly
	// before this are moved. Empty = all messages.
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

// handleMdboxAltMove moves messages between primary and alt storage
// (Reverse=true for alt→primary), filtered by InternalDate < Before.
// Mark + purge in one atomic call per file.
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
	am, ok := mailbox.Driver(bundle.box).(altMover)
	if !ok {
		apiError(w, "altmove: storage driver does not implement mdbox altmove", http.StatusBadRequest)
		return
	}
	if !am.AltEnabled() {
		apiError(w, "altmove: alt storage not configured (set storage.alt_dir or storage.mdbox_alt_storage_path)", http.StatusBadRequest)
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

	// Update AltTier in every folder's index so Fetch() can skip the
	// primary open(). SetAltTier takes the mailbox lock itself.
	if len(stats.MovedFilenames) > 0 {
		if flagErr := s.setAltTierAllFolders(bundle, stats.MovedFilenames, !req.Reverse); flagErr != nil {
			slog.Warn("altmove: failed to update alt-tier index flags",
				"user", req.User, "moved", len(stats.MovedFilenames), "err", flagErr)
		}
	}

	apiJSON(w, mdboxAltMoveResponse{
		Candidates:    stats.Candidates,
		Moved:         stats.Moved,
		FilesCreated:  stats.FilesCreated,
		FilesUnlinked: stats.FilesUnlinked,
		BytesMoved:    stats.BytesMoved,
	})
}

// setAltTierAllFolders calls SetAltTier on every selectable folder
// to keep the index flag in sync with the m.<N> file location.
func (s *Server) setAltTierAllFolders(bundle *nsBundle, filenames []string, altTier bool) error {
	entries, err := bundle.box.ListFolders()
	if err != nil {
		return fmt.Errorf("altmove/flag: list folders: %w", err)
	}
	var firstErr error
	for _, entry := range entries {
		if !entry.Selectable {
			continue
		}
		folder := entry.Name
		f, ferr := bundle.idx.OpenFolder(folder, 0)
		if ferr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("altmove/flag: open %q: %w", folder, ferr)
			}
			continue
		}
		if serr := bundle.idx.SetAltTier(f.ID, filenames, altTier); serr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("altmove/flag: set tier %q: %w", folder, serr)
			}
		}
	}
	return firstErr
}
