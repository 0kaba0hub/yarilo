package backendapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// cachePurger is asserted at use: a backend without a cache answers 501.
type cachePurger interface {
	PurgeCache(folderID uint64) (carried int, reclaimed int64, err error)
}

type cachePurgeStats struct {
	Folder     string `json:"folder"`
	Carried    int    `json:"carried"`
	Reclaimed  int64  `json:"reclaimed_bytes"`
	DurationMs int64  `json:"duration_ms"`
}

// handleIndexCachePurge reclaims a folder's yarilo.index.cache (#1030).
// Operator-triggered in v1: the file is append-only and has no threshold
// trigger yet, so nothing shrinks it on its own (BACKEND-API.md).
func (s *Server) handleIndexCachePurge(w http.ResponseWriter, r *http.Request) {
	var req optimizeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Folder == "" {
		apiError(w, errFolderRequired.Error(), http.StatusBadRequest)
		return
	}
	req.Folder = mailbox.NormalizeName(req.Folder, s.skipNFC())

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
		apiError(w, errFolderNotFound.Error(), http.StatusNotFound)
		return
	}
	purger, ok := bundle.idx.(cachePurger)
	if !ok {
		apiError(w, "this index backend keeps no cache to purge", http.StatusNotImplemented)
		return
	}

	// A purge moves every offset, so it takes the lock the session-side
	// cache window takes.
	start := time.Now()
	if s.opts.Locker != nil {
		key := locks.MailboxKey(uc.info.Username, req.Folder)
		ctx, cancel := context.WithTimeout(locks.WithSite(r.Context(), "admin-cache-purge"), 60*time.Second)
		defer cancel()
		lk, lerr := locks.Acquire(ctx, s.opts.Locker, key, uc.lockOwner(), 90*time.Second)
		if lerr != nil {
			apiError(w, fmt.Sprintf("acquire mailbox lock: %v", lerr), http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = s.opts.Locker.Unlock(context.Background(), lk.ID) }()
	}
	folder, err := bundle.idx.OpenFolder(req.Folder, 0)
	if err != nil {
		apiError(w, "open folder: "+err.Error(), http.StatusInternalServerError)
		return
	}
	carried, reclaimed, err := purger.PurgeCache(folder.ID)
	if err != nil {
		apiError(w, "purge cache: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, cachePurgeStats{
		Folder:     folder.Name,
		Carried:    carried,
		Reclaimed:  reclaimed,
		DurationMs: time.Since(start).Milliseconds(),
	})
}
