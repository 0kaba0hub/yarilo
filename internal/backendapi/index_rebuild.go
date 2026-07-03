package backendapi

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// rebuildRequest is the wire body for /api/backend/index/rebuild.
// `reset_uids` flips the default preserve-UIDs semantics to a
// nuke-and-reissue (UIDVALIDITY would need to bump too; not
// supported in v1 — operator must DELETE+CREATE instead).
type rebuildRequest struct {
	User      string `json:"user"`
	Folder    string `json:"folder"`
	Namespace string `json:"namespace"`
	ResetUIDs bool   `json:"reset_uids"`
}

type rebuildStats struct {
	Folder         string `json:"folder"`
	FolderGUID     string `json:"folder_guid"`
	Scanned        int    `json:"scanned"`
	UIDsPreserved  int    `json:"uids_preserved"`
	UIDsAssigned   int    `json:"uids_assigned"`
	OrphansDropped int    `json:"orphans_dropped"`
	DurationMs     int64  `json:"duration_ms"`
}

func (s *Server) handleIndexRebuild(w http.ResponseWriter, r *http.Request) {
	var req rebuildRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Folder == "" {
		apiError(w, errFolderRequired.Error(), http.StatusBadRequest)
		return
	}
	if req.ResetUIDs {
		apiError(w, "reset_uids=true not supported in v1 — DELETE+CREATE the folder via IMAP instead", http.StatusNotImplemented)
		return
	}
	stats, status, err := s.rebuildFolder(r.Context(), req)
	if err != nil {
		apiError(w, err.Error(), status)
		return
	}
	apiJSON(w, stats)
}

func (s *Server) rebuildFolder(ctx context.Context, req rebuildRequest) (*rebuildStats, int, error) {
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
		return nil, http.StatusInternalServerError, fmt.Errorf("folder exists: %w", err)
	}
	if !exists {
		return nil, http.StatusNotFound, errFolderNotFound
	}

	start := time.Now()

	// Cross-process lock so concurrent IMAP writers cannot race the
	// rebuild. Acquired before any Scan/OpenFolder work so the snapshot
	// we read is the snapshot we rewrite.
	if s.opts.Locker != nil {
		key := locks.MailboxKey(uc.info.Username, req.Folder)
		lockCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		lk, err := locks.Acquire(lockCtx, s.opts.Locker, key, uc.lockOwner(), 90*time.Second)
		if err != nil {
			return nil, http.StatusServiceUnavailable, fmt.Errorf("acquire mailbox lock: %w", err)
		}
		defer func() { _ = s.opts.Locker.Unlock(context.Background(), lk.ID) }()
	}

	scanned, scanErr := bundle.box.Scan(req.Folder)
	if scanErr != nil {
		status := http.StatusInternalServerError
		if strings.Contains(scanErr.Error(), "not yet implemented") {
			status = http.StatusNotImplemented
		}
		return nil, status, fmt.Errorf("scan: %w", scanErr)
	}

	folder, err := bundle.idx.OpenFolder(req.Folder, 0)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("open folder: %w", err)
	}
	existing, err := bundle.idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("get messages: %w", err)
	}

	byFilename := make(map[string]*mailbox.MessageMeta, len(existing))
	for _, m := range existing {
		if m.Filename != "" {
			byFilename[m.Filename] = m
		}
	}

	stats := &rebuildStats{
		Folder:     folder.Name,
		FolderGUID: hex.EncodeToString(folder.GUID[:]),
	}
	stats.Scanned = len(scanned)

	nextUID := folder.NextUID
	if nextUID == 0 {
		nextUID = 1
	}
	rebuilt := make([]*mailbox.MessageMeta, 0, len(scanned))
	for i := range scanned {
		rec := &scanned[i]
		if rec.Filename == "" {
			stats.OrphansDropped++
			continue
		}
		newMeta := &mailbox.MessageMeta{
			Filename:     rec.Filename,
			Size:         rec.Size,
			VSize:        rec.VSize,
			InternalDate: rec.InternalDate,
			GUID:         rec.GUID,
		}
		if old, ok := byFilename[rec.Filename]; ok {
			newMeta.UID = old.UID
			// Driver-provided flags (maildir) win since the filename
			// trailer is the source of truth there; dbox returns empty
			// so the index keeps its prior flag set unchanged.
			if len(rec.Flags) > 0 {
				newMeta.Flags = rec.Flags
				newMeta.Keywords = nil
			} else {
				newMeta.Flags = old.Flags
				newMeta.Keywords = old.Keywords
			}
			// Preserve GUID when the driver did not stamp one (maildir).
			var zero [16]byte
			if newMeta.GUID == zero {
				newMeta.GUID = old.GUID
			}
			stats.UIDsPreserved++
		} else {
			newMeta.UID = nextUID
			nextUID++
			newMeta.Flags = rec.Flags
			stats.UIDsAssigned++
		}
		rebuilt = append(rebuilt, newMeta)
	}

	// Deterministic on-disk order so two consecutive rebuilds with the
	// same input produce byte-identical .index files (helps diff-based
	// integrity checks across replicas).
	sort.Slice(rebuilt, func(i, j int) bool { return rebuilt[i].UID < rebuilt[j].UID })

	if err := bundle.idx.ResetFolder(folder.ID, rebuilt); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("reset folder: %w", err)
	}
	stats.DurationMs = time.Since(start).Milliseconds()
	return stats, http.StatusOK, nil
}

// ---- optimize -------------------------------------------------------------

type optimizeRequest struct {
	User      string `json:"user"`
	Folder    string `json:"folder"`
	Namespace string `json:"namespace"`
}

type optimizeStats struct {
	Folder     string `json:"folder"`
	DurationMs int64  `json:"duration_ms"`
}

func (s *Server) handleIndexOptimize(w http.ResponseWriter, r *http.Request) {
	var req optimizeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Folder == "" {
		apiError(w, errFolderRequired.Error(), http.StatusBadRequest)
		return
	}
	stats, status, err := s.optimizeFolder(r.Context(), req)
	if err != nil {
		apiError(w, err.Error(), status)
		return
	}
	apiJSON(w, stats)
}

func (s *Server) optimizeFolder(ctx context.Context, req optimizeRequest) (*optimizeStats, int, error) {
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
		return nil, http.StatusInternalServerError, fmt.Errorf("folder exists: %w", err)
	}
	if !exists {
		return nil, http.StatusNotFound, errFolderNotFound
	}
	start := time.Now()
	if s.opts.Locker != nil {
		key := locks.MailboxKey(uc.info.Username, req.Folder)
		lockCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		lk, err := locks.Acquire(lockCtx, s.opts.Locker, key, uc.lockOwner(), 90*time.Second)
		if err != nil {
			return nil, http.StatusServiceUnavailable, fmt.Errorf("acquire mailbox lock: %w", err)
		}
		defer func() { _ = s.opts.Locker.Unlock(context.Background(), lk.ID) }()
	}
	folder, err := bundle.idx.OpenFolder(req.Folder, 0)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("open folder: %w", err)
	}
	if err := bundle.idx.OptimizeIndex(folder.ID); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("optimize index: %w", err)
	}
	return &optimizeStats{
		Folder:     folder.Name,
		DurationMs: time.Since(start).Milliseconds(),
	}, http.StatusOK, nil
}

// ---- folder repair --------------------------------------------------------

// handleFolderRepair runs Rebuild then Optimize back-to-back on
// one folder under a single per-folder lock. Operator's "one knob
// to fix whatever is wrong" — Rebuild followed by index compaction.
//
// Returns a combined stats object so the operator sees what each
// step did. If Rebuild fails Optimize is skipped and the error
// from Rebuild propagates.
func (s *Server) handleFolderRepair(w http.ResponseWriter, r *http.Request) {
	var req rebuildRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Folder == "" {
		apiError(w, errFolderRequired.Error(), http.StatusBadRequest)
		return
	}
	if req.ResetUIDs {
		apiError(w, "reset_uids=true not supported in v1 — DELETE+CREATE the folder via IMAP instead", http.StatusNotImplemented)
		return
	}
	rb, status, err := s.rebuildFolder(r.Context(), req)
	if err != nil {
		apiError(w, err.Error(), status)
		return
	}
	opt, status, err := s.optimizeFolder(r.Context(), optimizeRequest{
		User:      req.User,
		Folder:    req.Folder,
		Namespace: req.Namespace,
	})
	if err != nil {
		apiError(w, "rebuilt OK but optimize failed: "+err.Error(), status)
		return
	}
	apiJSON(w, map[string]any{
		"rebuild":  rb,
		"optimize": opt,
	})
}
