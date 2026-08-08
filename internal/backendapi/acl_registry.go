package backendapi

import (
	"net/http"

	"github.com/yarilomail/yarilo/internal/userstate/acl"
)

// registerACLRegistryRoutes wires the owner-discovery registry admin surface
// (#1168): list answers "which owners can this caller discover", rebuild
// reprojects one owner's rows from their yarilo-acl-list index.
func (s *Server) registerACLRegistryRoutes() {
	s.mux.Handle("POST /api/backend/acl/registry/list", s.middleware(s.handleACLRegistryList))
	s.mux.Handle("POST /api/backend/acl/registry/rebuild", s.middleware(s.handleACLRegistryRebuild))
}

func (s *Server) handleACLRegistryList(w http.ResponseWriter, r *http.Request) {
	var req aclRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.User == "" {
		apiError(w, errUserRequired.Error(), http.StatusBadRequest)
		return
	}
	if s.opts.SharedDict == nil {
		apiError(w, "acl_shared_dict is not configured; owner discovery is disabled", http.StatusBadRequest)
		return
	}
	// Groups are a session-side identity (from the userdb at login); the admin
	// surface answers for the bare user plus anyone, and says so.
	owners, err := acl.OwnersFor(r.Context(), s.opts.SharedDict, req.User, nil)
	if err != nil {
		apiError(w, "registry scan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if owners == nil {
		owners = []string{}
	}
	apiJSON(w, map[string]any{
		"status": "ok",
		"user":   req.User,
		"owners": owners,
		"note":   "user and anyone grants only; group grants resolve per session identity",
	})
}

func (s *Server) handleACLRegistryRebuild(w http.ResponseWriter, r *http.Request) {
	store, req, _, _, err := s.openACLStore(w, r)
	if err != nil {
		return
	}
	if s.opts.SharedDict == nil {
		apiError(w, "acl_shared_dict is not configured; owner discovery is disabled", http.StatusBadRequest)
		return
	}
	if req.Folder != "" || len(req.Folders) > 0 || req.All {
		apiError(w, "registry rebuild reprojects the whole namespace index; do not send folders", http.StatusBadRequest)
		return
	}
	if err := store.RegistrySync(); err != nil {
		apiError(w, "registry sync: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]string{"status": "ok"})
}
