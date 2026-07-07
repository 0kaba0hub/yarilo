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
	User           string `json:"user"`
	StorageValue   int64  `json:"storage_value"`   // used storage in KiB (RFC 9208 STORAGE unit)
	StorageLimit   int64  `json:"storage_limit"`   // limit in KiB; -1 = unlimited
	StoragePercent int    `json:"storage_percent"` // 0 when unlimited
	MessageValue   int64  `json:"message_value"`   // used message count
	MessageLimit   int64  `json:"message_limit"`   // -1 = unlimited
	MessagePercent int    `json:"message_percent"` // 0 when unlimited
}

// handleQuotaShow returns the current usage and configured limits for a user.
// GET /api/backend/quota/show?user=alice@example.com
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
	var limits quota.Limits
	if s.opts.AuthClient != nil {
		if pui, err := s.opts.AuthClient.Userdb(r.Context(), user); err == nil && pui != nil {
			limits = quota.ParseRules(pui.QuotaRules)
		}
	}
	storageKiB := quota.StorageBytesToKiB(u.StorageBytes)
	limitKiB := int64(-1)
	storagePct := 0
	if limits.StorageBytes > 0 {
		limitKiB = int64(quota.StorageBytesToKiB(limits.StorageBytes))
		if limitKiB > 0 {
			storagePct = int(int64(storageKiB) * 100 / limitKiB)
		}
	}
	msgLimit := int64(-1)
	msgPct := 0
	if limits.Messages > 0 {
		msgLimit = limits.Messages
		if msgLimit > 0 {
			msgPct = int(u.Messages * 100 / msgLimit)
		}
	}
	apiJSON(w, quotaShowResponse{
		User:           user,
		StorageValue:   int64(storageKiB),
		StorageLimit:   limitKiB,
		StoragePercent: storagePct,
		MessageValue:   u.Messages,
		MessageLimit:   msgLimit,
		MessagePercent: msgPct,
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
