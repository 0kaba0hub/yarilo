package backendapi

import (
	"encoding/hex"
	"net/http"
	"sort"
)

// registerFolderRoutes wires the folder admin surface — purely
// read-only operations against fileindex + the storage driver.
//
// Mutating folder ops (create/delete/rename/expunge) are deferred to
// a later phase: they need IMAP-level ACL context, proper event
// emission to active sessions, and per-protocol lock semantics that
// the admin path does not yet model. See TODO.md.
func (s *Server) registerFolderRoutes() {
	s.mux.Handle("POST /api/backend/folder/list", s.middleware(s.handleFolderList))
	s.mux.Handle("POST /api/backend/folder/info", s.middleware(s.handleFolderInfo))
	s.mux.Handle("POST /api/backend/folder/guid", s.middleware(s.handleFolderGUID))
	s.mux.Handle("POST /api/backend/folder/stats", s.middleware(s.handleFolderStats))
	s.mux.Handle("POST /api/backend/folder/repair", s.middleware(s.handleFolderRepair))
}

type folderRequest struct {
	User      string `json:"user"`
	Folder    string `json:"folder"`
	Namespace string `json:"namespace"`
}

type folderInfoOut struct {
	Name          string `json:"name"`
	GUID          string `json:"guid"`
	UIDValidity   uint32 `json:"uid_validity"`
	NextUID       uint32 `json:"next_uid"`
	Messages      uint32 `json:"messages"`
	Unseen        uint32 `json:"unseen"`
	HighestModSeq uint64 `json:"highest_modseq"`
}

func (s *Server) handleFolderList(w http.ResponseWriter, r *http.Request) {
	var req folderRequest
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
	names, err := bundle.box.ListFolders()
	if err != nil {
		apiError(w, "list folders: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Strings(names)
	apiJSON(w, map[string]any{"folders": names})
}

func (s *Server) handleFolderInfo(w http.ResponseWriter, r *http.Request) {
	out, status, err := s.folderInfoCommon(w, r)
	if err != nil {
		apiError(w, err.Error(), status)
		return
	}
	apiJSON(w, out)
}

func (s *Server) handleFolderGUID(w http.ResponseWriter, r *http.Request) {
	out, status, err := s.folderInfoCommon(w, r)
	if err != nil {
		apiError(w, err.Error(), status)
		return
	}
	apiJSON(w, map[string]any{"folder": out.Name, "guid": out.GUID})
}

func (s *Server) handleFolderStats(w http.ResponseWriter, r *http.Request) {
	var req folderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Folder == "" {
		apiError(w, "folder required", http.StatusBadRequest)
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
	exists, err := bundle.box.FolderExists(req.Folder)
	if err != nil {
		apiError(w, "folder exists: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		apiError(w, "folder not found", http.StatusNotFound)
		return
	}
	folder, err := bundle.idx.OpenFolder(req.Folder, 0)
	if err != nil {
		apiError(w, "open folder: "+err.Error(), http.StatusInternalServerError)
		return
	}
	msgs, err := bundle.box.List(req.Folder)
	if err != nil {
		apiError(w, "list messages: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var sizeBytes uint64
	for _, m := range msgs {
		sizeBytes += uint64(m.Size)
	}
	apiJSON(w, map[string]any{
		"name":           folder.Name,
		"guid":           hex.EncodeToString(folder.GUID[:]),
		"uid_validity":   folder.UIDValidity,
		"next_uid":       folder.NextUID,
		"messages":       folder.Messages,
		"unseen":         folder.Unseen,
		"highest_modseq": folder.HighestModSeq,
		"size_bytes":     sizeBytes,
		"on_disk_count":  uint32(len(msgs)),
	})
}

// folderInfoCommon reads (req, opens) the folder and returns the
// metadata. Used by both info and guid endpoints.
func (s *Server) folderInfoCommon(w http.ResponseWriter, r *http.Request) (*folderInfoOut, int, error) {
	var req folderRequest
	if !decodeJSON(w, r, &req) {
		return nil, 0, errDecode
	}
	if req.Folder == "" {
		return nil, http.StatusBadRequest, errFolderRequired
	}
	uc, err := s.openUserContext(req.User)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	defer uc.Close()

	bundle, err := uc.ns(s, req.Namespace)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	exists, err := bundle.box.FolderExists(req.Folder)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !exists {
		return nil, http.StatusNotFound, errFolderNotFound
	}
	folder, err := bundle.idx.OpenFolder(req.Folder, 0)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return &folderInfoOut{
		Name:          folder.Name,
		GUID:          hex.EncodeToString(folder.GUID[:]),
		UIDValidity:   folder.UIDValidity,
		NextUID:       folder.NextUID,
		Messages:      folder.Messages,
		Unseen:        folder.Unseen,
		HighestModSeq: folder.HighestModSeq,
	}, http.StatusOK, nil
}
