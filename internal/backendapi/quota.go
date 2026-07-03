package backendapi

import (
	"net/http"

	"github.com/0kaba0hub/yarilo/pkg/quota"
)

func (s *Server) registerQuotaRoutes() {
	s.mux.Handle("GET /api/backend/quota/show", s.middleware(s.handleQuotaShow))
	s.mux.Handle("POST /api/backend/quota/recalc", s.middleware(s.handleQuotaRecalc))
	s.mux.Handle("POST /api/backend/quota/set", s.middleware(s.handleQuotaSet))
}

type quotaShowResponse struct {
	User         string `json:"user"`
	StorageBytes int64  `json:"storage_bytes"`
	Messages     int64  `json:"messages"`
	LimitBytes   int64  `json:"limit_bytes"`    // 0 = unlimited
	LimitMsgs    int64  `json:"limit_messages"` // 0 = unlimited
}

// handleQuotaShow returns the current usage for a user.
// GET /api/backend/quota/show?user=alice@example.com
// Limits are 0 (unlimited) unless the auth client can provide
// the userdb QuotaRules — use yarilo-admin auth passdb to look
// those up separately if needed.
func (s *Server) handleQuotaShow(w http.ResponseWriter, r *http.Request) {
	if s.opts.QuotaDict == nil {
		apiError(w, "quota not configured (set dicts.quota in yarilo.yaml)", http.StatusServiceUnavailable)
		return
	}
	user := r.URL.Query().Get("user")
	if user == "" {
		apiError(w, "missing user parameter", http.StatusBadRequest)
		return
	}
	ctr := quota.NewCounter(s.opts.QuotaDict, user)
	u, err := ctr.Get(r.Context())
	if err != nil {
		apiError(w, "quota show: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, quotaShowResponse{
		User:         user,
		StorageBytes: u.StorageBytes,
		Messages:     u.Messages,
	})
}

type quotaRecalcRequest struct {
	User      string `json:"user"`
	Namespace string `json:"namespace"`
}

type quotaRecalcResponse struct {
	User         string `json:"user"`
	StorageBytes int64  `json:"storage_bytes"`
	Messages     int64  `json:"messages"`
}

// handleQuotaRecalc scans all folder fileindexes for the user, sums
// message sizes, and overwrites the dict counters. Use after manual
// migrations or when counters drift.
func (s *Server) handleQuotaRecalc(w http.ResponseWriter, r *http.Request) {
	if s.opts.QuotaDict == nil {
		apiError(w, "quota not configured", http.StatusServiceUnavailable)
		return
	}
	var req quotaRecalcRequest
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

	// Walk all folders, sum message sizes from fileindex.
	folders, err := bundle.box.ListFolders()
	if err != nil {
		apiError(w, "recalc: list folders: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var totalBytes int64
	var totalMsgs int64
	for _, folder := range folders {
		f, err := bundle.idx.OpenFolder(folder, 0)
		if err != nil || f == nil {
			continue
		}
		msgs, err := bundle.idx.GetMessages(f.ID, nil)
		if err != nil {
			continue
		}
		for _, m := range msgs {
			totalBytes += int64(m.Size)
			totalMsgs++
		}
	}

	u := quota.Usage{StorageBytes: totalBytes, Messages: totalMsgs}
	ctr := quota.NewCounter(s.opts.QuotaDict, req.User)
	if err := ctr.Set(r.Context(), u); err != nil {
		apiError(w, "recalc: set counters: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, quotaRecalcResponse{
		User:         req.User,
		StorageBytes: totalBytes,
		Messages:     totalMsgs,
	})
}

type quotaSetRequest struct {
	User         string `json:"user"`
	StorageBytes *int64 `json:"storage_bytes,omitempty"`
	Messages     *int64 `json:"messages,omitempty"`
}

// handleQuotaSet directly overwrites counter values (admin override).
// Use when you need to manually adjust counts without a full rescan.
func (s *Server) handleQuotaSet(w http.ResponseWriter, r *http.Request) {
	if s.opts.QuotaDict == nil {
		apiError(w, "quota not configured", http.StatusServiceUnavailable)
		return
	}
	var req quotaSetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.User == "" {
		apiError(w, "missing user", http.StatusBadRequest)
		return
	}
	ctr := quota.NewCounter(s.opts.QuotaDict, req.User)
	cur, err := ctr.Get(r.Context())
	if err != nil {
		apiError(w, "quota set: get: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if req.StorageBytes != nil {
		cur.StorageBytes = *req.StorageBytes
	}
	if req.Messages != nil {
		cur.Messages = *req.Messages
	}
	if err := ctr.Set(r.Context(), cur); err != nil {
		apiError(w, "quota set: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, quotaRecalcResponse{
		User:         req.User,
		StorageBytes: cur.StorageBytes,
		Messages:     cur.Messages,
	})
}
