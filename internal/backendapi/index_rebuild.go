package backendapi

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// rebuildRequest is the wire body for /api/backend/index/rebuild.
// reset_uids would reissue UIDs and bump UIDVALIDITY; unsupported in v1
// (operator must DELETE+CREATE instead).
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
	req.Folder = mailbox.NormalizeName(req.Folder, s.skipNFC())
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

	// A folder-agnostic driver (mdbox) has a storage-wide Scan: the per-folder
	// rebuild would import every stored message into this folder with fresh UIDs
	// (cross-folder pollution + duplicates). Reject; use the storage-wide rebuild.
	if fa, ok := mailbox.Driver(bundle.box).(mailbox.FolderAgnosticStorage); ok && fa.FolderAgnosticScan() {
		return nil, http.StatusNotImplemented, errMdboxRebuildUnsupported
	}

	start := time.Now()

	// Cross-process lock so concurrent writers cannot race the rebuild.
	// Acquired before any Scan/OpenFolder so the snapshot we read is the
	// snapshot we rewrite.
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

	rstats, err := idxrebuild.RebuildFolder(bundle.box, bundle.idx, folder)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	// An operator rebuild is a superset of the reactive heal; drop any FSCKD
	// marker so the next SELECT does not run a redundant reactive heal.
	if cm, ok := bundle.idx.(mailbox.CorruptionMarker); ok {
		if err := cm.ClearFolderCorrupt(folder.ID); err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("clear corrupt marker: %w", err)
		}
	}

	// Invalidate FTS documents for the dropped records; otherwise they linger as
	// ghost documents until the next fts rescan.
	s.ftsExpunge(uc, folder.Name, rstats.ExpungedUIDs)

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
	// RestoreOrphans re-files unreferenced messages carrying an ORIG_MAILBOX tag
	// back into their home folder. Default false: unreferenced messages are left
	// zero-ref for the next purge, never resurrected.
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
// every folder index, adopt orphans into INBOX, drop vanished map records.
// Non-storage-wide drivers are rejected; those use per-folder /rebuild.
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
	rb, ok := mailbox.Driver(bundle.box).(mailbox.StorageWideRebuilder)
	if !ok {
		apiError(w, "storage-wide rebuild is only for folder-agnostic drivers (mdbox); use /api/backend/index/rebuild per folder", http.StatusBadRequest)
		return
	}

	start := time.Now()
	// The rebuild takes the storage (map) lock itself; the per-folder lock is
	// taken inside idx.ResetFolder.
	st, err := rb.RebuildStorage(bundle.idx, req.RestoreOrphans)
	if err != nil {
		apiError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Invalidate FTS documents for every record the rebuild dropped, per folder;
	// otherwise they linger as ghost documents until the next fts rescan.
	for folderName, uids := range st.ExpungedUIDs {
		s.ftsExpunge(uc, folderName, uids)
	}

	note := "run with the user's mailboxes quiesced — no concurrent delivery or folder ops (operator repair tool). Unreferenced messages are set zero-ref for the next purge; orphan restore requires restore_orphans=true AND an ORIG_MAILBOX tag"
	if req.RestoreOrphans {
		note = "restore_orphans=true: unreferenced messages with an ORIG_MAILBOX tag were re-filed into their home folder (default flags — flags are not recoverable for an orphan); the rest are zero-ref for purge. Run with the user's mailboxes quiesced"
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
	// All folds every folder of the account. Without it a caller wanting the
	// whole account writes the loop itself -- which is how a seeding script
	// ends up doing eleven full rebuild scans to get the effect of eleven
	// folds.
	All bool `json:"all"`
}

type optimizeStats struct {
	Folder     string `json:"folder"`
	DurationMs int64  `json:"duration_ms"`
}

// optimizeAccountStats is what folding a whole account reports: one line per
// folder, plus what failed. A folder that could not be folded does not stop the
// rest -- the operation is per folder and independent, and a caller who asked
// for the account wants the ten that worked rather than an error about the one
// that did not.
type optimizeAccountStats struct {
	User    string            `json:"user"`
	Folders []optimizeStats   `json:"folders"`
	Failed  map[string]string `json:"failed,omitempty"`
	// MapFolded reports whether the driver's per-user map was folded too. Only
	// mdbox has one; for the others the field is absent, which is the honest
	// answer rather than "false".
	MapFolded *bool `json:"map_folded,omitempty"`
	TotalMs   int64 `json:"total_ms"`
}

// mapCompactor is the optional capability a driver has when it keeps a per-user
// structure that is replayed at open time beside the folder indexes. mdbox has
// one; maildir and sdbox do not, so they are simply drivers without the method.
type mapCompactor interface {
	CompactMap() error
}

func (s *Server) handleIndexOptimize(w http.ResponseWriter, r *http.Request) {
	var req optimizeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.All {
		stats, status, err := s.optimizeAccount(r.Context(), req)
		if err != nil {
			apiError(w, err.Error(), status)
			return
		}
		apiJSON(w, stats)
		return
	}
	if req.Folder == "" {
		apiError(w, errFolderRequired.Error(), http.StatusBadRequest)
		return
	}
	req.Folder = mailbox.NormalizeName(req.Folder, s.skipNFC())
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

// optimizeAccount folds every folder of one account, one at a time. Sequential
// on purpose: each fold takes that folder's cross-process lock, and running
// them together would queue a user's own sessions behind their own maintenance.
func (s *Server) optimizeAccount(ctx context.Context, req optimizeRequest) (*optimizeAccountStats, int, error) {
	uc, err := s.openUserContext(req.User)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	bundle, err := uc.ns(s, req.Namespace)
	if err != nil {
		uc.Close()
		return nil, http.StatusBadRequest, err
	}
	entries, err := bundle.box.ListFolders()
	if err != nil {
		uc.Close()
		return nil, http.StatusInternalServerError, fmt.Errorf("list folders: %w", err)
	}
	// The per-user map, where the driver keeps one. Folded before the folder
	// indexes so a failure here is visible in the same call rather than
	// discovered later as "the folders are clean but opening is still slow".
	var mapFolded *bool
	if mc, ok := bundle.box.(mapCompactor); ok {
		ok := mc.CompactMap() == nil
		mapFolded = &ok
	}
	uc.Close()

	out := &optimizeAccountStats{User: req.User, MapFolded: mapFolded}
	start := time.Now()
	for _, e := range entries {
		if !e.Selectable {
			continue
		}
		one := optimizeRequest{User: req.User, Folder: e.Name, Namespace: req.Namespace}
		st, _, ferr := s.optimizeFolder(ctx, one)
		if ferr != nil {
			if out.Failed == nil {
				out.Failed = map[string]string{}
			}
			out.Failed[e.Name] = ferr.Error()
			continue
		}
		out.Folders = append(out.Folders, *st)
	}
	out.TotalMs = time.Since(start).Milliseconds()
	slog.Info("backendapi: account index fold",
		"user", req.User, "folded", len(out.Folders), "failed", len(out.Failed), "total_ms", out.TotalMs)
	return out, http.StatusOK, nil
}

// ---- folder repair --------------------------------------------------------

// handleFolderRepair runs Rebuild then index compaction on one folder.
// Returns a combined stats object. If Rebuild fails, Optimize is skipped
// and the Rebuild error propagates.
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
	req.Folder = mailbox.NormalizeName(req.Folder, s.skipNFC())
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
