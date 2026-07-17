package backendapi

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/0kaba0hub/yarilo/internal/storage/idxrebuild"
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

	// A folder-agnostic driver (mdbox) has a storage-wide Scan: running the
	// per-folder rebuild would import every stored message into this folder with
	// fresh UIDs (cross-folder pollution + duplicates). Reject until the
	// storage-wide rebuild lands (#594 Phase 2b) — never fall through to
	// RebuildFolder.
	if fa, ok := bundle.box.(mailbox.FolderAgnosticStorage); ok && fa.FolderAgnosticScan() {
		return nil, http.StatusNotImplemented, errMdboxRebuildUnsupported
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

	folder, err := bundle.idx.OpenFolder(req.Folder, 0)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("open folder: %w", err)
	}

	// Folder-agnostic drivers were rejected above, so any error here is a genuine
	// rebuild failure — no "not yet implemented" special-casing.
	rstats, err := idxrebuild.RebuildFolder(bundle.box, bundle.idx, folder)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	// An operator rebuild is a superset of the reactive heal, so drop any FSCKD
	// marker — otherwise the next SELECT would run a redundant reactive heal.
	if cm, ok := bundle.idx.(mailbox.CorruptionMarker); ok {
		if err := cm.ClearFolderCorrupt(folder.ID); err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("clear corrupt marker: %w", err)
		}
	}

	stats := &rebuildStats{
		Folder:         folder.Name,
		FolderGUID:     hex.EncodeToString(folder.GUID[:]),
		Scanned:        rstats.Scanned,
		UIDsPreserved:  rstats.UIDsPreserved,
		UIDsAssigned:   rstats.UIDsAssigned,
		OrphansDropped: rstats.OrphansDropped,
		DurationMs:     time.Since(start).Milliseconds(),
	}
	return stats, http.StatusOK, nil
}

// ---- storage-wide rebuild (mdbox) -----------------------------------------

type storageRebuildRequest struct {
	User      string `json:"user"`
	Namespace string `json:"namespace"`
	// RestoreOrphans opts in to re-filing unreferenced messages that carry an
	// ORIG_MAILBOX tag back into their home folder. Default false: unreferenced
	// messages are left zero-ref for the next purge, never resurrected.
	RestoreOrphans bool `json:"restore_orphans"`
}

type storageRebuildStats struct {
	Scanned             int    `json:"scanned"`
	FoldersRebuilt      int    `json:"folders_rebuilt"`
	Expunged            int    `json:"expunged"`
	UnreferencedZeroref int    `json:"unreferenced_zeroref"`
	OrphansRestored     int    `json:"orphans_restored"`
	RebuildCount        uint32 `json:"rebuild_count"`
	DurationMs          int64  `json:"duration_ms"`
	Note                string `json:"note"`
}

// handleStorageRebuild runs the storage-wide rebuild for a folder-agnostic
// driver (mdbox): reconcile the shared map against the physical files, reset
// every folder index, adopt orphans into INBOX, drop vanished map records. A
// driver that is not storage-wide is rejected — those use per-folder /rebuild.
func (s *Server) handleStorageRebuild(w http.ResponseWriter, r *http.Request) {
	var req storageRebuildRequest
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
	rb, ok := bundle.box.(mailbox.StorageWideRebuilder)
	if !ok {
		apiError(w, "storage-wide rebuild is only for folder-agnostic drivers (mdbox); use /api/backend/index/rebuild per folder", http.StatusBadRequest)
		return
	}

	start := time.Now()
	// The rebuild takes the storage (map) lock itself; no folder lock is acquired
	// here — it is taken per folder inside idx.ResetFolder.
	st, err := rb.RebuildStorage(bundle.idx, req.RestoreOrphans)
	if err != nil {
		apiError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	note := "run with delivery to this user quiesced (operator repair tool). Unreferenced messages are set zero-ref for the next purge; orphan restore requires restore_orphans=true AND an ORIG_MAILBOX tag"
	if req.RestoreOrphans {
		note = "restore_orphans=true: unreferenced messages with an ORIG_MAILBOX tag were re-filed into their home folder; the rest are zero-ref for purge. Run with delivery quiesced"
	}
	apiJSON(w, storageRebuildStats{
		Scanned:             st.Scanned,
		FoldersRebuilt:      st.FoldersRebuilt,
		Expunged:            st.Expunged,
		UnreferencedZeroref: st.UnreferencedZeroref,
		OrphansRestored:     st.OrphansRestored,
		RebuildCount:        st.RebuildCount,
		DurationMs:          time.Since(start).Milliseconds(),
		Note:                note,
	})
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
