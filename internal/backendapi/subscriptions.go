package backendapi

import (
	"net/http"
	"sort"

	"github.com/0kaba0hub/yarilo/internal/userstate/subs"
)

// registerSubscriptionRoutes registers IMAP subscription routes.
// Uses subs.Store with the same on-disk format and lock key as
// IMAP sessions.
func (s *Server) registerSubscriptionRoutes() {
	s.mux.Handle("POST /api/backend/subscriptions/list", s.middleware(s.handleSubsList))
	s.mux.Handle("POST /api/backend/subscriptions/add", s.middleware(s.handleSubsAdd))
	s.mux.Handle("POST /api/backend/subscriptions/remove", s.middleware(s.handleSubsRemove))
}

type subsRequest struct {
	User      string `json:"user"`
	Folder    string `json:"folder"`
	Namespace string `json:"namespace"`
}

func (s *Server) handleSubsList(w http.ResponseWriter, r *http.Request) {
	store, _, err := s.openSubsStore(w, r)
	if err != nil {
		return
	}
	set, err := store.Snapshot()
	if err != nil {
		apiError(w, "snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	apiJSON(w, map[string]any{"subscriptions": out})
}

func (s *Server) handleSubsAdd(w http.ResponseWriter, r *http.Request) {
	store, folder, err := s.openSubsStore(w, r)
	if err != nil {
		return
	}
	if folder == "" {
		apiError(w, "folder required", http.StatusBadRequest)
		return
	}
	if err := store.Add(folder); err != nil {
		apiError(w, "add: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleSubsRemove(w http.ResponseWriter, r *http.Request) {
	store, folder, err := s.openSubsStore(w, r)
	if err != nil {
		return
	}
	if folder == "" {
		apiError(w, "folder required", http.StatusBadRequest)
		return
	}
	if err := store.Remove(folder); err != nil {
		apiError(w, "remove: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]string{"status": "ok"})
}

// openSubsStore decodes the request and returns the namespace's
// subs.Store. On error the HTTP response is already written.
// The store holds no long-lived handles, so uc can be closed here.
func (s *Server) openSubsStore(w http.ResponseWriter, r *http.Request) (*subs.Store, string, error) {
	var req subsRequest
	if !decodeJSON(w, r, &req) {
		return nil, "", errDecode
	}
	if req.User == "" {
		apiError(w, "user required", http.StatusBadRequest)
		return nil, "", errUserRequired
	}
	uc, err := s.openUserContext(req.User)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return nil, "", err
	}
	defer uc.Close()

	nsName := req.Namespace
	if nsName == "" {
		nsName = "personal"
	}
	bundle, err := uc.ns(s, nsName)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return nil, "", err
	}
	store := subs.New(
		bundle.folderControlRoot(),
		subsFileFor(bundle.spec),
		uc.info.Username,
		uc.lockOwner(),
		s.opts.Locker,
	)
	return store, req.Folder, nil
}
