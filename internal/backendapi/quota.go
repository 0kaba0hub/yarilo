package backendapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
	"github.com/0kaba0hub/yarilo/pkg/quota"
)

func (s *Server) registerQuotaRoutes() {
	s.mux.Handle("GET /api/backend/quota/show", s.middleware(s.handleQuotaShow))
	s.mux.Handle("POST /api/backend/quota/recalc", s.middleware(s.handleQuotaRecalc))
}

// userCountUsage sums the authoritative index-derived usage across a user's
// personal-namespace folders (the count backend). Shared by /show and /recalc.
func (s *Server) userCountUsage(user, namespace string) (quota.Usage, quota.Limits, error) {
	uc, err := s.openUserContext(user)
	if err != nil {
		return quota.Usage{}, quota.Limits{}, err
	}
	defer uc.Close()
	bundle, err := uc.ns(s, namespace)
	if err != nil {
		return quota.Usage{}, quota.Limits{}, err
	}
	folders, err := bundle.box.ListFolders()
	if err != nil {
		return quota.Usage{}, quota.Limits{}, err
	}
	var limits quota.Limits
	if s.opts.AuthClient != nil {
		if pui, uerr := s.opts.AuthClient.Userdb(context.Background(), user); uerr == nil && pui != nil {
			limits = quota.ParseRules(pui.QuotaRules)
		}
	}
	return quota.CountUsage(bundle.idx, mailbox.SelectableNames(folders), limits), limits, nil
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
	user := r.URL.Query().Get("user")
	if user == "" {
		apiError(w, "missing user parameter", http.StatusBadRequest)
		return
	}
	u, limits, err := s.userCountUsage(user, "")
	if err != nil {
		apiError(w, "quota show: "+err.Error(), http.StatusInternalServerError)
		return
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

// handleQuotaRecalc returns the authoritative usage summed from the index
// aggregate (self-healing hdr-vsize). In the count model the index is always
// the source of truth, so this is a read; it exists for admin verification
// after migrations.
func (s *Server) handleQuotaRecalc(w http.ResponseWriter, r *http.Request) {
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
	folders, err := bundle.box.ListFolders()
	if err != nil {
		apiError(w, "recalc: list folders: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Force a rebuild of each folder's aggregate from records, then sum.
	names := mailbox.SelectableNames(folders)
	for _, name := range names {
		f, oerr := bundle.idx.OpenFolder(name, 0)
		if oerr != nil {
			continue
		}
		if rerr := bundle.idx.RecomputeVSize(f.ID); rerr != nil {
			slog.Warn("quota recalc: rebuild failed", "user", req.User, "folder", name, "err", rerr)
		}
	}
	var limits quota.Limits
	if s.opts.AuthClient != nil {
		if pui, uerr := s.opts.AuthClient.Userdb(context.Background(), req.User); uerr == nil && pui != nil {
			limits = quota.ParseRules(pui.QuotaRules)
		}
	}
	u := quota.CountUsage(bundle.idx, names, limits)
	apiJSON(w, quotaRecalcResponse{
		User:         req.User,
		StorageBytes: u.StorageBytes,
		Messages:     u.Messages,
	})
}
