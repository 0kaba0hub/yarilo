package backendapi

import (
	"encoding/hex"
	"net/http"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// registerIndexRoutes registers index routes: dump (read-only),
// rebuild (per-folder resync; 501 for mdbox), rebuild-storage
// (storage-wide, for mdbox), optimize (compact .index.log).
func (s *Server) registerIndexRoutes() {
	s.mux.Handle("POST /api/backend/index/dump", s.middleware(s.handleIndexDump))
	s.mux.Handle("POST /api/backend/index/rebuild", s.middleware(s.handleIndexRebuild))
	s.mux.Handle("POST /api/backend/index/rebuild-storage", s.middleware(s.handleStorageRebuild))
	s.mux.Handle("POST /api/backend/index/optimize", s.middleware(s.handleIndexOptimize))
}

type indexDumpRequest struct {
	User      string `json:"user"`
	Folder    string `json:"folder"`
	Namespace string `json:"namespace"`
	Limit     int    `json:"limit"`
}

type indexRecordOut struct {
	UID      uint32   `json:"uid"`
	Filename string   `json:"filename"`
	Flags    []string `json:"flags,omitempty"`
	Keywords []string `json:"keywords,omitempty"`
	ModSeq   uint64   `json:"modseq"`
	Size     uint32   `json:"size"`
	VSize    uint32   `json:"vsize"`
	GUID     string   `json:"guid,omitempty"`
}

func (s *Server) handleIndexDump(w http.ResponseWriter, r *http.Request) {
	var req indexDumpRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Folder == "" {
		apiError(w, "folder required", http.StatusBadRequest)
		return
	}
	if req.Limit < 0 {
		req.Limit = 0
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
	req.Folder = mailbox.NormalizeName(req.Folder, bundle.info.SkipNFCNormalize)
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
	all, err := bundle.idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		apiError(w, "get messages: "+err.Error(), http.StatusInternalServerError)
		return
	}
	truncated := false
	if req.Limit > 0 && len(all) > req.Limit {
		all = all[:req.Limit]
		truncated = true
	}
	out := make([]indexRecordOut, 0, len(all))
	for _, m := range all {
		var guidStr string
		var zero [16]byte
		if m.GUID != zero {
			guidStr = hex.EncodeToString(m.GUID[:])
		}
		out = append(out, indexRecordOut{
			UID:      m.UID,
			Filename: m.Filename,
			Flags:    m.Flags,
			Keywords: m.Keywords,
			ModSeq:   m.ModSeq,
			Size:     m.Size,
			VSize:    m.VSize,
			GUID:     guidStr,
		})
	}
	apiJSON(w, map[string]any{
		"folder":         folder.Name,
		"folder_guid":    hex.EncodeToString(folder.GUID[:]),
		"uid_validity":   folder.UIDValidity,
		"next_uid":       folder.NextUID,
		"highest_modseq": folder.HighestModSeq,
		"records":        out,
		"truncated":      truncated,
	})
}
