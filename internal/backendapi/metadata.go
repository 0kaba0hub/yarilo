package backendapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// registerMetadataRoutes wires the RFC 5464 METADATA admin surface.
//
// Backend-api keeps the same dict layout the IMAP server already
// uses (pkg/mailbox/attribute.go) — keys are GUID-namespaced under
// priv/box/<guid>/... or shared/box/<guid>/... — so admin writes are
// visible to the next IMAP GETMETADATA round-trip. Empty folder name
// targets server-scope (vendor-prefixed under INBOX's GUID).
//
// Shared/public namespaces follow SharedAttrKey semantics:
// /private/ scope embeds a per-accessing-user hash so each user's
// private annotation on a shared folder stays isolated. The admin
// caller MUST pass the accessing user via the as_user field to
// target the right per-user slice.
func (s *Server) registerMetadataRoutes() {
	s.mux.Handle("POST /api/backend/metadata/list", s.middleware(s.handleMetadataList))
	s.mux.Handle("POST /api/backend/metadata/get", s.middleware(s.handleMetadataGet))
	s.mux.Handle("POST /api/backend/metadata/set", s.middleware(s.handleMetadataSet))
	s.mux.Handle("POST /api/backend/metadata/delete", s.middleware(s.handleMetadataDelete))
}

type metadataRequest struct {
	User      string `json:"user"`
	Folder    string `json:"folder"`
	Namespace string `json:"namespace"`
	Scope     string `json:"scope"`
	Entry     string `json:"entry"`
	Value     string `json:"value"`
	AsUser    string `json:"as_user"`
}

type metadataContext struct {
	uc       *userContext
	bundle   *nsBundle
	scope    mailbox.AttrScope
	guid     [16]byte
	shared   bool
	asUser   string
	folder   string
	rawScope string
}

func (s *Server) handleMetadataList(w http.ResponseWriter, r *http.Request) {
	mc, err := s.openMetadataContext(w, r, false)
	if err != nil {
		return
	}
	defer mc.uc.Close()
	d := s.opts.MetadataDict
	if d == nil {
		apiError(w, "metadata dict not configured", http.StatusNotImplemented)
		return
	}
	prefix := mc.prefix()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	op := &dict.OpSettings{Username: mc.uc.info.Username}
	iter, err := d.Iterate(ctx, op, prefix, dict.IterRecurse)
	if err != nil {
		apiError(w, "iterate: "+err.Error(), http.StatusInternalServerError)
		return
	}
	entries := make(map[string]string)
	for iter.Next() {
		entry := mailbox.TrimAttrPrefix(iter.Key(), prefix)
		if entry == "" {
			continue
		}
		values := iter.Values()
		var encoded string
		if len(values) > 0 {
			encoded = base64.StdEncoding.EncodeToString(values[0])
		}
		entries[mailbox.FormatAttrEntry(mc.scope, entry)] = encoded
	}
	if err := iter.Err(); err != nil {
		_ = iter.Close()
		apiError(w, "iterate: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := iter.Close(); err != nil {
		apiError(w, "iterate close: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]any{
		"folder":  mc.folder,
		"scope":   mc.rawScope,
		"entries": entries,
	})
}

func (s *Server) handleMetadataGet(w http.ResponseWriter, r *http.Request) {
	mc, req, err := s.openMetadataContextReq(w, r, true)
	if err != nil {
		return
	}
	defer mc.uc.Close()
	d := s.opts.MetadataDict
	if d == nil {
		apiError(w, "metadata dict not configured", http.StatusNotImplemented)
		return
	}
	scope, attrName, err := mailbox.ParseAttrEntry(req.Entry)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	mc.scope = scope
	mc.rawScope = scopeWire(scope)
	key := mc.key(attrName)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	values, found, err := d.Lookup(ctx, &dict.OpSettings{Username: mc.uc.info.Username}, key)
	if err != nil {
		apiError(w, "lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]any{
		"folder": mc.folder,
		"entry":  req.Entry,
		"found":  found,
	}
	if found && len(values) > 0 {
		resp["value"] = base64.StdEncoding.EncodeToString(values[0])
	}
	apiJSON(w, resp)
}

func (s *Server) handleMetadataSet(w http.ResponseWriter, r *http.Request) {
	mc, req, err := s.openMetadataContextReq(w, r, true)
	if err != nil {
		return
	}
	defer mc.uc.Close()
	d := s.opts.MetadataDict
	if d == nil {
		apiError(w, "metadata dict not configured", http.StatusNotImplemented)
		return
	}
	scope, attrName, err := mailbox.ParseAttrEntry(req.Entry)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	mc.scope = scope
	mc.rawScope = scopeWire(scope)
	key := mc.key(attrName)

	raw, err := base64.StdEncoding.DecodeString(req.Value)
	if err != nil {
		apiError(w, "value: invalid base64: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tx, err := d.Begin(ctx, &dict.OpSettings{Username: mc.uc.info.Username})
	if err != nil {
		apiError(w, "transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Set(key, raw); err != nil {
		_ = tx.Rollback()
		apiError(w, "set: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.Commit(); err != nil {
		apiError(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleMetadataDelete(w http.ResponseWriter, r *http.Request) {
	mc, req, err := s.openMetadataContextReq(w, r, true)
	if err != nil {
		return
	}
	defer mc.uc.Close()
	d := s.opts.MetadataDict
	if d == nil {
		apiError(w, "metadata dict not configured", http.StatusNotImplemented)
		return
	}
	scope, attrName, err := mailbox.ParseAttrEntry(req.Entry)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	mc.scope = scope
	mc.rawScope = scopeWire(scope)
	key := mc.key(attrName)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tx, err := d.Begin(ctx, &dict.OpSettings{Username: mc.uc.info.Username})
	if err != nil {
		apiError(w, "transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Unset(key); err != nil {
		_ = tx.Rollback()
		apiError(w, "unset: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.Commit(); err != nil {
		apiError(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apiJSON(w, map[string]string{"status": "ok"})
}

// openMetadataContext is for list (entry not required), set the
// scope from req.Scope. For get/set/delete (where the entry name
// carries the scope) use openMetadataContextReq with requireEntry.
func (s *Server) openMetadataContext(w http.ResponseWriter, r *http.Request, requireEntry bool) (*metadataContext, error) {
	mc, _, err := s.openMetadataContextReq(w, r, requireEntry)
	return mc, err
}

func (s *Server) openMetadataContextReq(w http.ResponseWriter, r *http.Request, requireEntry bool) (*metadataContext, *metadataRequest, error) {
	var req metadataRequest
	if !decodeJSON(w, r, &req) {
		return nil, nil, errDecode
	}
	if req.User == "" {
		apiError(w, errUserRequired.Error(), http.StatusBadRequest)
		return nil, nil, errUserRequired
	}
	if requireEntry && req.Entry == "" {
		apiError(w, errEntryRequired.Error(), http.StatusBadRequest)
		return nil, nil, errEntryRequired
	}

	uc, err := s.openUserContext(req.User)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return nil, nil, err
	}
	bundle, err := uc.ns(s, req.Namespace)
	if err != nil {
		uc.Close()
		apiError(w, err.Error(), http.StatusBadRequest)
		return nil, nil, err
	}
	// One owner of NFC on the admin surface too (#1113).
	req.Folder = mailbox.NormalizeName(req.Folder, bundle.info.SkipNFCNormalize)

	scope := mailbox.AttrPrivate
	if strings.EqualFold(req.Scope, "shared") {
		scope = mailbox.AttrShared
	}

	mc := &metadataContext{
		uc:       uc,
		bundle:   bundle,
		scope:    scope,
		rawScope: scopeWire(scope),
		shared:   bundle.spec.Type != "personal",
		asUser:   req.AsUser,
		folder:   req.Folder,
	}
	if mc.asUser == "" {
		mc.asUser = uc.info.Username
	}
	// Resolve target GUID: empty folder → INBOX GUID (server scope);
	// otherwise the requested folder's own GUID. OpenFolder errors
	// when the folder does not exist (or, for INBOX on a fresh user,
	// initialises it via the same code path the IMAP server uses).
	target := req.Folder
	if target == "" {
		target = "INBOX"
	}
	if exists, err := bundle.box.FolderExists(target); err != nil {
		uc.Close()
		apiError(w, "folder exists: "+err.Error(), http.StatusInternalServerError)
		return nil, nil, err
	} else if !exists {
		uc.Close()
		apiError(w, errFolderNotFound.Error(), http.StatusNotFound)
		return nil, nil, errFolderNotFound
	}
	folder, err := bundle.idx.OpenFolder(target, 0)
	if err != nil {
		uc.Close()
		apiError(w, "open folder: "+err.Error(), http.StatusInternalServerError)
		return nil, nil, err
	}
	mc.guid = folder.GUID
	return mc, &req, nil
}

// key returns the dict key for (attrName) in this metadata context.
// For empty folder (server scope) it routes through
// mailbox.ServerAttrKey; otherwise the per-folder helper variant
// matching personal vs shared/public namespace.
func (mc *metadataContext) key(attrName string) string {
	if mc.folder == "" {
		return mailbox.ServerAttrKey(mc.scope, mc.guid, attrName)
	}
	if mc.shared {
		return mailbox.SharedAttrKey(mc.scope, mc.guid, mc.asUser, attrName)
	}
	return mailbox.AttrKey(mc.scope, mc.guid, attrName)
}

// prefix returns the dict iterate prefix for "every attribute of
// this folder under this scope".
func (mc *metadataContext) prefix() string {
	if mc.folder == "" {
		return mailbox.ServerAttrPrefix(mc.scope, mc.guid)
	}
	if mc.shared {
		return mailbox.SharedAttrPrefix(mc.scope, mc.guid, mc.asUser)
	}
	return mailbox.AttrPrefix(mc.scope, mc.guid)
}

func scopeWire(s mailbox.AttrScope) string {
	if s == mailbox.AttrShared {
		return "shared"
	}
	return "private"
}
