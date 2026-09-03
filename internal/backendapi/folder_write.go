package backendapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/userstate/acl"
	"github.com/yarilomail/yarilo/internal/userstate/specialuse"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// folderEventTimeout bounds the fire-and-forget event emission so a sluggish
// yarilo-locks pub/sub cannot stall the admin HTTP request.
const folderEventTimeout = time.Second

// registerFolderWriteRoutes wires the mutating folder admin endpoints.
//
// Authorisation model: admin bypass. backend-api is already gated by Token +
// AllowedNets + mTLS, so the handler does NOT consult ACL.Effective even when
// an ACL file exists on the target mailbox. The ACL store IS still updated:
// DELETE drops the yarilo-acl file + global index entries and RENAME moves
// them, leaving no orphan ACL state behind.
func (s *Server) registerFolderWriteRoutes() {
	s.mux.Handle("POST /api/backend/folder/create", s.middleware(s.handleFolderCreate))
	s.mux.Handle("POST /api/backend/folder/delete", s.middleware(s.handleFolderDelete))
	s.mux.Handle("POST /api/backend/folder/rename", s.middleware(s.handleFolderRename))
	s.mux.Handle("POST /api/backend/folder/expunge", s.middleware(s.handleFolderExpunge))
}

// folderCreateRequest is decoded by handleFolderCreate. SpecialUse is optional;
// when set, the folder is registered with that RFC 6154 attribute so a
// subsequent LIST surfaces it. Personal-namespace only: specialuse is per-user
// and has no semantics on shared / public namespaces.
type folderCreateRequest struct {
	User       string `json:"user"`
	Folder     string `json:"folder"`
	Namespace  string `json:"namespace"`
	SpecialUse string `json:"special_use"`
}

// folderRenameRequest carries the old + new folder names. Renaming across
// namespaces is not supported.
type folderRenameRequest struct {
	User      string `json:"user"`
	OldFolder string `json:"old_folder"`
	NewFolder string `json:"new_folder"`
	Namespace string `json:"namespace"`
}

// folderExpungeRequest narrows the EXPUNGE to a specific UID set when UIDs is
// non-nil; when empty, every \Deleted-flagged message is removed.
type folderExpungeRequest struct {
	User      string   `json:"user"`
	Folder    string   `json:"folder"`
	Namespace string   `json:"namespace"`
	UIDs      []uint32 `json:"uids,omitempty"`
}

func (s *Server) handleFolderCreate(w http.ResponseWriter, r *http.Request) {
	var req folderCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Folder == "" {
		apiError(w, "folder required", http.StatusBadRequest)
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
	// One owner of NFC on the admin surface too, so a decomposed name from a
	// tool addresses the same folder a client created (#1113).
	req.Folder = mailbox.NormalizeName(req.Folder, bundle.info.SkipNFCNormalize)

	exists, err := bundle.box.FolderExists(req.Folder)
	if err != nil {
		apiError(w, "folder exists check: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if exists {
		apiError(w, "folder already exists", http.StatusConflict)
		return
	}
	if err := bundle.box.Create(req.Folder); err != nil {
		apiError(w, "create: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.materialiseFolderACL(bundle, req.Folder); err != nil {
		apiError(w, "materialise acl: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Only meaningful on personal; shared / public folders do not carry
	// per-user RFC 6154 attrs.
	if req.SpecialUse != "" && bundle.spec.Type == "personal" {
		store := specialuse.New(
			bundle.folderHome(),
			uc.info.Username,
			uc.lockOwner(),
			s.opts.Locker,
			s.opts.SpecialUseDefaults,
		)
		if err := store.Set(req.Folder, imaplib.MailboxAttr(req.SpecialUse)); err != nil {
			// Folder is created; rolling back is worse than reporting the
			// partial state. Surface as 200 with a warning.
			slog.Warn("backendapi/folder: special_use set failed",
				"user", req.User, "folder", req.Folder, "attr", req.SpecialUse, "err", err)
			apiJSON(w, map[string]any{
				"status":            "ok",
				"special_use_error": err.Error(),
			})
			return
		}
	}

	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleFolderDelete(w http.ResponseWriter, r *http.Request) {
	var req folderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Folder == "" {
		apiError(w, "folder required", http.StatusBadRequest)
		return
	}
	// On maildir INBOX is the mail root, so removing it takes every message
	// and the root index with it. The Rename sibling already refuses; this
	// path did not, and an admin call naming the wrong folder was
	// indistinguishable from a successful cleanup (#1063).
	if strings.EqualFold(req.Folder, "INBOX") {
		apiError(w, "INBOX cannot be deleted", http.StatusBadRequest)
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
	// One owner of NFC on the admin surface too, so a decomposed name from a
	// tool addresses the same folder a client created (#1113).
	req.Folder = mailbox.NormalizeName(req.Folder, bundle.info.SkipNFCNormalize)

	exists, err := bundle.box.FolderExists(req.Folder)
	if err != nil {
		apiError(w, "folder exists check: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		apiError(w, "folder does not exist", http.StatusNotFound)
		return
	}

	if err := bundle.box.Delete(req.Folder); err != nil {
		apiError(w, "delete: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Drop the per-folder yarilo-acl file + namespace-wide list entries.
	// Non-fatal: the mailbox is already gone; warn on ACL-state drift.
	if err := s.dropFolderACL(bundle, req.Folder); err != nil {
		slog.Warn("backendapi/folder: acl cleanup after delete failed",
			"user", req.User, "folder", req.Folder, "err", err)
	}

	s.emitFolderEvent(uc, req.Folder, locks.EventExpunged, 0)
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleFolderRename(w http.ResponseWriter, r *http.Request) {
	var req folderRenameRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.OldFolder == "" || req.NewFolder == "" {
		apiError(w, "old_folder and new_folder required", http.StatusBadRequest)
		return
	}
	if strings.EqualFold(req.OldFolder, "INBOX") {
		// INBOX rename has move-messages semantics the admin path does not
		// yet implement (it would need to touch every message). Reject rather
		// than silently doing the wrong thing.
		apiError(w, "rename of INBOX is not supported via backend-api", http.StatusBadRequest)
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

	req.OldFolder = mailbox.NormalizeName(req.OldFolder, bundle.info.SkipNFCNormalize)
	req.NewFolder = mailbox.NormalizeName(req.NewFolder, bundle.info.SkipNFCNormalize)
	srcExists, err := bundle.box.FolderExists(req.OldFolder)
	if err != nil {
		apiError(w, "src exists check: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !srcExists {
		apiError(w, "old_folder does not exist", http.StatusNotFound)
		return
	}
	dstExists, err := bundle.box.FolderExists(req.NewFolder)
	if err != nil {
		apiError(w, "dst exists check: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if dstExists {
		apiError(w, "new_folder already exists", http.StatusConflict)
		return
	}

	if err := bundle.box.Rename(req.OldFolder, req.NewFolder); err != nil {
		apiError(w, "rename: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := bundle.idx.RenameFolder(req.OldFolder, req.NewFolder); err != nil {
		// On-disk box rename succeeded; index disagreed. Log rather than
		// roll back; the operator can run repair.
		slog.Warn("backendapi/folder: idx rename failed after box rename",
			"user", req.User, "from", req.OldFolder, "to", req.NewFolder, "err", err)
	}
	if err := s.renameFolderACL(bundle, req.OldFolder, req.NewFolder); err != nil {
		slog.Warn("backendapi/folder: acl rename failed",
			"user", req.User, "from", req.OldFolder, "to", req.NewFolder, "err", err)
	}

	s.emitFolderEvent(uc, req.OldFolder, locks.EventExpunged, 0)
	s.emitFolderEvent(uc, req.NewFolder, locks.EventDelivered, 0)
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleFolderExpunge(w http.ResponseWriter, r *http.Request) {
	var req folderExpungeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Folder == "" {
		apiError(w, "folder required", http.StatusBadRequest)
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
	// One owner of NFC on the admin surface too, so a decomposed name from a
	// tool addresses the same folder a client created (#1113).
	req.Folder = mailbox.NormalizeName(req.Folder, bundle.info.SkipNFCNormalize)

	exists, err := bundle.box.FolderExists(req.Folder)
	if err != nil {
		apiError(w, "folder exists check: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		apiError(w, "folder does not exist", http.StatusNotFound)
		return
	}

	folder, err := bundle.idx.OpenFolder(req.Folder, 0)
	if err != nil {
		apiError(w, "open folder: "+err.Error(), http.StatusInternalServerError)
		return
	}
	msgs, err := bundle.idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		apiError(w, "list messages: "+err.Error(), http.StatusInternalServerError)
		return
	}

	uidSet := make(map[uint32]struct{}, len(req.UIDs))
	for _, u := range req.UIDs {
		uidSet[u] = struct{}{}
	}
	scoped := len(req.UIDs) > 0

	var expunged []uint32
	for _, m := range msgs {
		if !hasDeletedFlag(m.Flags) {
			continue
		}
		if scoped {
			if _, ok := uidSet[m.UID]; !ok {
				continue
			}
		}
		if err := bundle.idx.ExpungeMessage(folder.ID, m.UID); err != nil {
			slog.Warn("backendapi/folder: expunge message failed",
				"user", req.User, "folder", req.Folder, "uid", m.UID, "err", err)
			continue
		}
		if err := bundle.box.Remove(req.Folder, m.Filename); err != nil {
			slog.Warn("backendapi/folder: remove blob failed",
				"user", req.User, "folder", req.Folder, "filename", m.Filename, "err", err)
		}
		expunged = append(expunged, m.UID)
		s.emitFolderEvent(uc, req.Folder, locks.EventExpunged, m.UID)
	}

	apiJSON(w, map[string]any{
		"status":   "ok",
		"expunged": expunged,
		"count":    len(expunged),
	})
}

// hasDeletedFlag reports whether flags contain \Deleted.
func hasDeletedFlag(flags []string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, `\Deleted`) {
			return true
		}
	}
	return false
}

// materialiseFolderACL writes what a newly created mailbox inherits into its
// own ACL file, the same way the IMAP CREATE path does -- otherwise a mailbox
// made through the admin API would resolve its rights differently from one made
// by a client (#1111).
func (s *Server) materialiseFolderACL(bundle *nsBundle, folder string) error {
	store := acl.New(
		bundle.folderHome(),
		bundle.info.MailPath,
		bundle.info.Driver,
		bundle.info.Separator,
		bundle.info.StorageEscapeChar,
		bundle.info.Username,
		locks.Owner(bundle.info.Username, bundle.info.LockID()),
		acl.Policy{},
		s.opts.Locker,
	)
	return store.MaterialiseOnCreate(folder)
}

// dropFolderACL removes the per-folder yarilo-acl file and its namespace-wide
// list entry. Idempotent: acl.Store.Remove is a no-op when no file exists.
func (s *Server) dropFolderACL(bundle *nsBundle, folder string) error {
	store := acl.New(
		bundle.folderHome(),
		bundle.info.MailPath,
		bundle.info.Driver,
		bundle.info.Separator,
		bundle.info.StorageEscapeChar,
		bundle.info.Username,
		locks.Owner(bundle.info.Username, bundle.info.LockID()),
		acl.Policy{},
		s.opts.Locker,
	)
	return store.Remove(folder)
}

// renameFolderACL moves the per-folder yarilo-acl file across index dirs and
// rewrites the namespace-wide list entries. Idempotent.
func (s *Server) renameFolderACL(bundle *nsBundle, oldFolder, newFolder string) error {
	store := acl.New(
		bundle.folderHome(),
		bundle.info.MailPath,
		bundle.info.Driver,
		bundle.info.Separator,
		bundle.info.StorageEscapeChar,
		bundle.info.Username,
		locks.Owner(bundle.info.Username, bundle.info.LockID()),
		acl.Policy{},
		s.opts.Locker,
	)
	return store.Rename(oldFolder, newFolder)
}

// emitFolderEvent pushes an advisory wake-up to IDLE sessions on other pods.
// Fire-and-forget: errors are logged at Debug only, since the authoritative
// state already lives on disk and the next poll picks it up.
func (s *Server) emitFolderEvent(uc *userContext, folder string, eventType locks.EventType, uid uint32) {
	if s.opts.Locker == nil || uc.info == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), folderEventTimeout)
	defer cancel()
	payload := uintPayload(uid)
	key := locks.MailboxKey(uc.info.Username, folder)
	if err := s.opts.Locker.Emit(ctx, key, eventType, payload); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			slog.Debug("backendapi/folder: event emit timed out",
				"folder", folder, "type", string(eventType))
			return
		}
		slog.Debug("backendapi/folder: event emit failed",
			"folder", folder, "type", string(eventType), "err", err)
	}
}

func uintPayload(u uint32) string {
	if u == 0 {
		return ""
	}
	const digits = "0123456789"
	var buf [10]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = digits[u%10]
		u /= 10
	}
	return string(buf[i:])
}
