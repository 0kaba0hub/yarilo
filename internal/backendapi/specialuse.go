package backendapi

import (
	"net/http"
	"sort"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/0kaba0hub/yarilo/internal/userstate/specialuse"
)

// registerSpecialUseRoutes registers RFC 6154 special-use override
// routes. Uses specialuse.Store with the same on-disk format and
// lock key as IMAP CREATE (USE ...). Personal namespace only.
func (s *Server) registerSpecialUseRoutes() {
	s.mux.Handle("POST /api/backend/specialuse/list", s.middleware(s.handleSpecialUseList))
	s.mux.Handle("POST /api/backend/specialuse/get", s.middleware(s.handleSpecialUseGet))
	s.mux.Handle("POST /api/backend/specialuse/set", s.middleware(s.handleSpecialUseSet))
	s.mux.Handle("POST /api/backend/specialuse/delete", s.middleware(s.handleSpecialUseDelete))
}

type specialUseRequest struct {
	User   string `json:"user"`
	Folder string `json:"folder"`
	Attr   string `json:"attr"`
}

func (s *Server) handleSpecialUseList(w http.ResponseWriter, r *http.Request) {
	store, _, err := s.openSpecialUseStore(w, r)
	if err != nil {
		return
	}
	overrides, err := store.Snapshot()
	if err != nil {
		apiError(w, "snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ovKeys := make([]string, 0, len(overrides))
	for k := range overrides {
		ovKeys = append(ovKeys, k)
	}
	sort.Strings(ovKeys)
	ovOut := make(map[string]string, len(overrides))
	for _, k := range ovKeys {
		ovOut[k] = string(overrides[k])
	}
	defaults := store.Defaults()
	defOut := make(map[string]string, len(defaults))
	for k, v := range defaults {
		defOut[k] = string(v)
	}
	apiJSON(w, map[string]any{
		"overrides": ovOut,
		"defaults":  defOut,
	})
}

func (s *Server) handleSpecialUseGet(w http.ResponseWriter, r *http.Request) {
	store, req, err := s.openSpecialUseStoreReq(w, r)
	if err != nil {
		return
	}
	if req.Folder == "" {
		apiError(w, "folder required", http.StatusBadRequest)
		return
	}
	overrides, err := store.Snapshot()
	if err != nil {
		apiError(w, "snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	source := "none"
	var attr imaplib.MailboxAttr
	if v, ok := overrides[req.Folder]; ok {
		attr = v
		source = "override"
	} else {
		defaults := store.Defaults()
		if v, ok := defaults[req.Folder]; ok {
			attr = v
			source = "default"
		}
	}
	apiJSON(w, map[string]any{
		"folder": req.Folder,
		"attr":   string(attr),
		"source": source,
	})
}

func (s *Server) handleSpecialUseSet(w http.ResponseWriter, r *http.Request) {
	store, req, err := s.openSpecialUseStoreReq(w, r)
	if err != nil {
		return
	}
	if req.Folder == "" {
		apiError(w, "folder required", http.StatusBadRequest)
		return
	}
	if req.Attr == "" {
		apiError(w, errAttrRequired.Error(), http.StatusBadRequest)
		return
	}
	if err := store.Set(req.Folder, imaplib.MailboxAttr(req.Attr)); err != nil {
		apiError(w, "set: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleSpecialUseDelete(w http.ResponseWriter, r *http.Request) {
	store, req, err := s.openSpecialUseStoreReq(w, r)
	if err != nil {
		return
	}
	if req.Folder == "" {
		apiError(w, "folder required", http.StatusBadRequest)
		return
	}
	if err := store.Delete(req.Folder); err != nil {
		apiError(w, "delete: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) openSpecialUseStore(w http.ResponseWriter, r *http.Request) (*specialuse.Store, *specialUseRequest, error) {
	return s.openSpecialUseStoreReq(w, r)
}

func (s *Server) openSpecialUseStoreReq(w http.ResponseWriter, r *http.Request) (*specialuse.Store, *specialUseRequest, error) {
	var req specialUseRequest
	if !decodeJSON(w, r, &req) {
		return nil, nil, errDecode
	}
	if req.User == "" {
		apiError(w, errUserRequired.Error(), http.StatusBadRequest)
		return nil, nil, errUserRequired
	}
	uc, err := s.openUserContext(req.User)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return nil, nil, err
	}
	defer uc.Close()

	bundle, err := uc.ns(s, "personal")
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return nil, nil, err
	}
	store := specialuse.New(
		bundle.folderHome(),
		uc.info.Username,
		uc.lockOwner(),
		s.opts.Locker,
		s.opts.SpecialUseDefaults,
	)
	return store, &req, nil
}
