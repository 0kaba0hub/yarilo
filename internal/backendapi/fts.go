package backendapi

import (
	"encoding/hex"
	"net/http"

	"github.com/0kaba0hub/yarilo/pkg/fts"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// registerFTSRoutes wires the operator surface for full-text search. Every
// endpoint dials the yarilo-fts service over ftsproto — backend-api resolves
// the folder's GUID/UIDVALIDITY from the authoritative index so the operator
// only ever names user + folder. Disabled (501) when no FTS client is set.
func (s *Server) registerFTSRoutes() {
	s.mux.Handle("GET /api/backend/fts/status", s.middleware(s.handleFTSStatus))
	s.mux.Handle("POST /api/backend/fts/rescan", s.middleware(s.handleFTSRescan))
	s.mux.Handle("POST /api/backend/fts/optimize", s.middleware(s.handleFTSOptimize))
}

// ftsMailboxRef resolves a folder name to the full wire identity via the
// user's index. UIDVALIDITY and GUID come from the authoritative folder
// record so the service keys the same mailbox the IMAP path does.
func (s *Server) ftsMailboxRef(uc *userContext, folder string) (fts.MailboxRef, error) {
	bundle, err := uc.ns(s, "")
	if err != nil {
		return fts.MailboxRef{}, err
	}
	f, err := bundle.idx.OpenFolder(folder, 0)
	if err != nil {
		return fts.MailboxRef{}, err
	}
	return fts.MailboxRef{
		Name:        f.Name,
		GUID:        hex.EncodeToString(f.GUID[:]),
		UIDValidity: f.UIDValidity,
	}, nil
}

type ftsStatusResponse struct {
	User             string `json:"user"`
	Folder           string `json:"folder"`
	LastIndexedUID   uint32 `json:"last_indexed_uid"`
	SettingsChecksum uint32 `json:"settings_checksum"`
}

// handleFTSStatus reports the per-mailbox indexing checkpoint.
// GET /api/backend/fts/status?user=&folder=
func (s *Server) handleFTSStatus(w http.ResponseWriter, r *http.Request) {
	if s.opts.FTSClient == nil {
		apiError(w, "fts not configured on this backend-api", http.StatusNotImplemented)
		return
	}
	user := r.URL.Query().Get("user")
	folder := r.URL.Query().Get("folder")
	if user == "" {
		apiError(w, errUserRequired.Error(), http.StatusBadRequest)
		return
	}
	if folder == "" {
		apiError(w, errFolderRequired.Error(), http.StatusBadRequest)
		return
	}
	uc, err := s.openUserContext(user)
	if err != nil {
		apiError(w, "fts status: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer uc.Close()
	mbox, err := s.ftsMailboxRef(uc, folder)
	if err != nil {
		apiError(w, "fts status: "+err.Error(), http.StatusInternalServerError)
		return
	}
	last, checksum, err := s.opts.FTSClient.Status(user, mbox)
	if err != nil {
		apiError(w, "fts status: "+err.Error(), http.StatusBadGateway)
		return
	}
	apiJSON(w, ftsStatusResponse{
		User: user, Folder: folder,
		LastIndexedUID: last, SettingsChecksum: checksum,
	})
}

type ftsRescanResponse struct {
	User    string   `json:"user"`
	Folders []string `json:"folders"` // folders that were rescanned
}

// handleFTSRescan reconciles the index against the mailbox. With ?folder=
// only that folder is rescanned; without it, every selectable folder is.
// POST /api/backend/fts/rescan?user=&folder=
func (s *Server) handleFTSRescan(w http.ResponseWriter, r *http.Request) {
	if s.opts.FTSClient == nil {
		apiError(w, "fts not configured on this backend-api", http.StatusNotImplemented)
		return
	}
	user := r.URL.Query().Get("user")
	folder := r.URL.Query().Get("folder")
	if user == "" {
		apiError(w, errUserRequired.Error(), http.StatusBadRequest)
		return
	}
	uc, err := s.openUserContext(user)
	if err != nil {
		apiError(w, "fts rescan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer uc.Close()

	var names []string
	if folder != "" {
		names = []string{folder}
	} else {
		bundle, err := uc.ns(s, "")
		if err != nil {
			apiError(w, "fts rescan: "+err.Error(), http.StatusInternalServerError)
			return
		}
		folders, err := bundle.box.ListFolders()
		if err != nil {
			apiError(w, "fts rescan: "+err.Error(), http.StatusInternalServerError)
			return
		}
		names = mailbox.SelectableNames(folders)
	}

	done := make([]string, 0, len(names))
	for _, name := range names {
		mbox, err := s.ftsMailboxRef(uc, name)
		if err != nil {
			apiError(w, "fts rescan: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.opts.FTSClient.Rescan(user, mbox); err != nil {
			apiError(w, "fts rescan "+name+": "+err.Error(), http.StatusBadGateway)
			return
		}
		done = append(done, name)
	}
	apiJSON(w, ftsRescanResponse{User: user, Folders: done})
}

// handleFTSOptimize compacts every index owned by the user.
// POST /api/backend/fts/optimize?user=
func (s *Server) handleFTSOptimize(w http.ResponseWriter, r *http.Request) {
	if s.opts.FTSClient == nil {
		apiError(w, "fts not configured on this backend-api", http.StatusNotImplemented)
		return
	}
	user := r.URL.Query().Get("user")
	if user == "" {
		apiError(w, errUserRequired.Error(), http.StatusBadRequest)
		return
	}
	if err := s.opts.FTSClient.Optimize(user); err != nil {
		apiError(w, "fts optimize: "+err.Error(), http.StatusBadGateway)
		return
	}
	apiJSON(w, map[string]string{"user": user, "status": "ok"})
}
