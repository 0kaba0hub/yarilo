package backendapi

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/yarilomail/yarilo/internal/userstate/acl"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// registerACLRoutes wires the RFC 4314 ACL admin surface. Reuses
// internal/userstate/acl.Store — same on-disk format (yarilo-acl
// per-mailbox + yarilo-acl-list namespace-wide index) and the same
// locks as the IMAP SETACL / DELETEACL paths, so concurrent IMAP
// sessions see admin writes immediately.
//
// Endpoints:
//
//	POST /api/backend/acl/list    — every mailbox with an explicit ACL
//	POST /api/backend/acl/get     — parsed ACL for one mailbox
//	POST /api/backend/acl/set     — replace ACL for one mailbox
//	POST /api/backend/acl/delete  — drop ACL for one mailbox
//	POST /api/backend/acl/materialise — write inherited entries into the ACL
//	                                of each mailbox that lacks them (repair for
//	                                mailboxes created before inheritance was
//	                                materialised at creation); dry_run unless
//	                                asked otherwise
//	POST /api/backend/acl/rebuild — reseed namespace-wide index from
//	                                per-mailbox files (folders arg
//	                                supplied by caller)
func (s *Server) registerACLRoutes() {
	s.mux.Handle("POST /api/backend/acl/list", s.middleware(s.handleACLList))
	s.mux.Handle("POST /api/backend/acl/get", s.middleware(s.handleACLGet))
	s.mux.Handle("POST /api/backend/acl/set", s.middleware(s.handleACLSet))
	s.mux.Handle("POST /api/backend/acl/delete", s.middleware(s.handleACLDelete))
	s.mux.Handle("POST /api/backend/acl/rebuild", s.middleware(s.handleACLRebuild))
	s.mux.Handle("POST /api/backend/acl/materialise", s.middleware(s.handleACLMaterialise))
}

// aclRequest is the common request body for the admin endpoints.
// Folder is required by get / set / delete; ignored by list. ACL is
// required by set. Folders is required by rebuild.
type aclRequest struct {
	User      string         `json:"user"`
	Namespace string         `json:"namespace"`
	Folder    string         `json:"folder"`
	ACL       []aclEntryJSON `json:"acl,omitempty"`
	Folders   []string       `json:"folders,omitempty"`
	// Root addresses the namespace-root ACL, the one a shared namespace needs
	// before anyone can create a mailbox in it.
	//
	// An explicit field rather than "an empty folder now means the root":
	// folder is required everywhere else, so a typo that dropped it would
	// otherwise become a legitimate grant on the root of the namespace
	// (#1091).
	Root bool `json:"root,omitempty"`
	// Apply turns a materialise run from a dry run into a write. Absent means
	// dry run: the operation changes who can reach mail, so "show me what you
	// would do" is how it is meant to be run first, not a flag remembered
	// afterwards.
	Apply bool `json:"apply,omitempty"`
}

// aclEntryJSON is the wire-format representation of a single ACL
// entry. Identifier carries the leading '-' for negatives so JSON
// stays symmetric with the on-disk format; the API layer splits it
// out into the Negative flag before persisting.
type aclEntryJSON struct {
	Identifier string `json:"identifier"`
	Rights     string `json:"rights"`
	Negative   bool   `json:"negative,omitempty"`
}

func (s *Server) handleACLList(w http.ResponseWriter, r *http.Request) {
	store, _, _, err := s.openACLStore(w, r)
	if err != nil {
		return
	}
	entries, err := store.ListSnapshot()
	if err != nil {
		apiError(w, "list snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]any{"entries": entriesToJSON(entries)})
}

func (s *Server) handleACLGet(w http.ResponseWriter, r *http.Request) {
	store, req, _, err := s.openACLStore(w, r)
	if err != nil {
		return
	}
	if req.Folder == "" && !req.Root {
		apiError(w, `folder required (or "root": true for the namespace root)`, http.StatusBadRequest)
		return
	}
	parsed, err := store.Get(req.Folder)
	if err != nil {
		apiError(w, "get: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]any{
		"folder": req.Folder,
		"acl":    aclToJSON(parsed),
	})
}

func (s *Server) handleACLSet(w http.ResponseWriter, r *http.Request) {
	store, req, _, err := s.openACLStore(w, r)
	if err != nil {
		return
	}
	if req.Folder == "" && !req.Root {
		apiError(w, `folder required (or "root": true for the namespace root)`, http.StatusBadRequest)
		return
	}
	parsed, err := jsonToACL(req.ACL)
	if err != nil {
		apiError(w, "acl: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := store.Set(req.Folder, parsed); err != nil {
		apiError(w, "set: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleACLDelete(w http.ResponseWriter, r *http.Request) {
	store, req, _, err := s.openACLStore(w, r)
	if err != nil {
		return
	}
	if req.Folder == "" && !req.Root {
		apiError(w, `folder required (or "root": true for the namespace root)`, http.StatusBadRequest)
		return
	}
	if err := store.Remove(req.Folder); err != nil {
		apiError(w, "delete: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleACLRebuild(w http.ResponseWriter, r *http.Request) {
	store, req, present, err := s.openACLStore(w, r)
	if err != nil {
		return
	}
	if len(req.Folders) == 0 {
		apiError(w, "folders required", http.StatusBadRequest)
		return
	}
	// Unlike set, rebuild does not create anything: it reseeds the index from
	// files already on disk, so a name that is not there contributes nothing
	// and cannot become a mailbox with permissions. It is also the repair
	// tool, run precisely when the state is already inconsistent, so refusing
	// the whole batch over one stale name would fail on the state it repairs.
	//
	// Permissive, then, but not silent: the count used to be len(req.Folders),
	// so a batch of three names of which two did not exist answered
	// {"folders":3,"status":"ok"} -- an operator who misspelt one got a success
	// with the number they expected and went away believing the index reseeded.
	rebuilt := make([]string, 0, len(req.Folders))
	skipped := make([]map[string]string, 0)
	err = store.ListRebuild(req.Folders, func(folder string) (mailbox.ACL, error) {
		if !present[folder] {
			skipped = append(skipped, map[string]string{"folder": folder, "reason": "folder not found"})
			return nil, nil
		}
		acl, err := store.Get(folder)
		if err != nil {
			return nil, err
		}
		if len(acl) == 0 {
			skipped = append(skipped, map[string]string{"folder": folder, "reason": "no ACL"})
			return nil, nil
		}
		rebuilt = append(rebuilt, folder)
		return acl, nil
	})
	if err != nil {
		apiError(w, "rebuild: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]any{
		"status":  "ok",
		"folders": len(rebuilt),
		"rebuilt": rebuilt,
		"skipped": skipped,
	})
}

// openACLStore decodes the common request body, resolves the
// per-namespace bundle, and returns the acl.Store. Mirrors
// openSubsStore / openSpecialUseStore in this package.
func (s *Server) openACLStore(w http.ResponseWriter, r *http.Request) (*acl.Store, *aclRequest, map[string]bool, error) {
	var req aclRequest
	if !decodeJSON(w, r, &req) {
		return nil, nil, nil, errDecode
	}
	if req.User == "" {
		apiError(w, errUserRequired.Error(), http.StatusBadRequest)
		return nil, nil, nil, errUserRequired
	}
	uc, err := s.openUserContext(req.User)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return nil, nil, nil, err
	}
	defer uc.Close()

	nsName := req.Namespace
	if nsName == "" {
		nsName = "personal"
	}
	bundle, err := uc.ns(s, nsName)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return nil, nil, nil, err
	}
	// Admin surface manages explicit entries, not effective-with-default
	// resolution, so acl_defaults_from_inbox does not apply here.
	// The admin path writes the files the IMAP commands read, so a name IMAP
	// refuses must not be writable here. It was: "/" and "." were accepted and
	// stored (#1091). Checked through the same configured rules the session
	// servers use, rather than a second list that could drift from them.
	//
	// The empty name is left to each handler: it means "the namespace root" to
	// some of them and nothing to others.
	if req.Root && req.Folder != "" {
		apiError(w, `"root" addresses the namespace root; do not send "folder" with it`, http.StatusBadRequest)
		return nil, nil, nil, errRootWithFolder
	}
	if req.Folder != "" {
		// Same NFC owner as every other admin entry: address the folder the
		// client created, not a decomposed spelling of it (#1113).
		req.Folder = mailbox.NormalizeName(req.Folder, bundle.info.SkipNFCNormalize)
		if err := mailbox.CheckName(bundle.box, req.Folder); err != nil {
			apiError(w, err.Error(), http.StatusBadRequest)
			return nil, nil, nil, err
		}
		// RFC 4314 3.3, the rule #1075 put on the IMAP side: the ACL commands
		// answer for a mailbox that is there. This path never checked, so
		// setting an ACL on a misspelt name was not an error -- the store
		// created the directory and wrote the file, and a typo became a
		// mailbox with permissions and no messages.
		//
		// Asked here rather than in each handler, for the reason
		// resolveACLHandle gives: four copies is how one of them ends up
		// without. The root is exempt by construction -- it carries no folder
		// name, so it never reaches this branch (#1096).
		exists, err := bundle.box.FolderExists(req.Folder)
		if err != nil {
			apiError(w, "folder exists: "+err.Error(), http.StatusInternalServerError)
			return nil, nil, nil, err
		}
		if !exists {
			apiError(w, "folder not found", http.StatusNotFound)
			return nil, nil, nil, errFolderNotFound
		}
	}
	// Existence for the rebuild batch, resolved here because the storage
	// handle is closed when this returns. rebuild does not refuse an absent
	// name -- see the handler -- but it must not report it as rebuilt either.
	var present map[string]bool
	if len(req.Folders) > 0 {
		present = make(map[string]bool, len(req.Folders))
		for _, f := range req.Folders {
			exists, ferr := bundle.box.FolderExists(f)
			if ferr != nil {
				apiError(w, "folder exists: "+ferr.Error(), http.StatusInternalServerError)
				return nil, nil, nil, ferr
			}
			present[f] = exists
		}
	}
	store := acl.New(bundle.folderHome(), bundle.info.MailPath, bundle.info.Driver, bundle.info.Separator, bundle.info.StorageEscapeChar, uc.info.Username, uc.lockOwner(), acl.Policy{}, s.opts.Locker)
	return store, &req, present, nil
}

// aclToJSON / jsonToACL bridge the in-memory ACL representation and
// the API wire format. The on-disk Negative flag is surfaced on the
// wire as a '-' prefix on the identifier so a get → set round-trip
// preserves type without an extra negative field on every entry.

func aclToJSON(acl mailbox.ACL) []aclEntryJSON {
	out := make([]aclEntryJSON, 0, len(acl))
	for _, e := range acl {
		id := e.Identifier.String()
		if e.Negative {
			id = "-" + id
		}
		out = append(out, aclEntryJSON{
			Identifier: id,
			Rights:     e.Rights.String(),
			Negative:   e.Negative,
		})
	}
	return out
}

func jsonToACL(in []aclEntryJSON) (mailbox.ACL, error) {
	out := make(mailbox.ACL, 0, len(in))
	for _, e := range in {
		idStr := e.Identifier
		negative := e.Negative
		if len(idStr) > 0 && idStr[0] == '-' {
			negative = true
			idStr = idStr[1:]
		}
		id, err := parseAdminIdentifier(idStr)
		if err != nil {
			return nil, err
		}
		rights, err := mailbox.ParseRights(e.Rights)
		if err != nil {
			return nil, err
		}
		out = append(out, mailbox.Entry{
			Identifier: id,
			Rights:     rights,
			Negative:   negative,
		})
	}
	return out, nil
}

// parseAdminIdentifier accepts the disk-canonical forms (anyone /
// authenticated / owner / user= / group= / group-override=) and a
// bare username (same convention as internal/imap.identifierFromIMAP).
func parseAdminIdentifier(s string) (mailbox.Identifier, error) {
	switch s {
	case "anyone":
		return mailbox.Identifier{Type: mailbox.IDAnyone}, nil
	case "authenticated":
		return mailbox.Identifier{Type: mailbox.IDAuthenticated}, nil
	case "owner":
		return mailbox.Identifier{Type: mailbox.IDOwner}, nil
	}
	if len(s) == 0 {
		return mailbox.Identifier{}, fmt.Errorf("backendapi/acl: empty identifier")
	}
	// Disk-canonical prefixed forms go through ParseIdentifier so
	// validation and Name extraction stay in one place.
	if hasAnyPrefix(s, "user=", "group=", "group-override=") {
		return mailbox.ParseIdentifier(s)
	}
	return mailbox.Identifier{Type: mailbox.IDUser, Name: s}, nil
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if len(s) >= len(p) && s[:len(p)] == p {
			return true
		}
	}
	return false
}

// entriesToJSON serialises a yarilo-acl-list snapshot for the wire,
// sorted by (mailbox, identifier) so the response is deterministic
// across calls.
func entriesToJSON(in []acl.ListEntry) []map[string]any {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Mailbox != in[j].Mailbox {
			return in[i].Mailbox < in[j].Mailbox
		}
		return in[i].Identifier.String() < in[j].Identifier.String()
	})
	out := make([]map[string]any, 0, len(in))
	for _, e := range in {
		id := e.Identifier.String()
		if e.Negative {
			id = "-" + id
		}
		out = append(out, map[string]any{
			"mailbox":    e.Mailbox,
			"identifier": id,
			"rights":     e.Rights.String(),
			"negative":   e.Negative,
		})
	}
	return out
}

// handleACLMaterialise writes what each mailbox inherits into its own ACL,
// for mailboxes that have an ACL of their own and do not name those
// identifiers.
//
// It repairs what copy-at-create cannot: mailboxes created before it, whose
// file replaced the inherited grant outright and can therefore name a peer and
// nobody able to administer the mailbox (#1111).
//
// A dry run unless "apply": true. It only adds, never rewrites an entry that is
// already there -- an existing entry is an explicit statement, and a mailbox
// whose ACL deliberately leaves out an identifier the root names is
// indistinguishable on disk from one orphaned by the old rule. That is also why
// this is an operator action and not something a resolver does on read.
func (s *Server) handleACLMaterialise(w http.ResponseWriter, r *http.Request) {
	store, req, _, err := s.openACLStore(w, r)
	if err != nil {
		return
	}
	if req.Folder != "" || req.Root {
		apiError(w, "materialise runs over a whole namespace; do not send a folder", http.StatusBadRequest)
		return
	}
	folders := req.Folders
	if len(folders) == 0 {
		apiError(w, "folders required", http.StatusBadRequest)
		return
	}
	rep, err := store.MaterialiseExisting(folders, !req.Apply)
	if err != nil {
		apiError(w, "materialise: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]any{
		"status":  "ok",
		"applied": req.Apply,
		"added":   rep.Added,
		"skipped": rep.Skipped,
	})
}
