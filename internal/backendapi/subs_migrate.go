package backendapi

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/internal/userstate/subs"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// handleSubsMigrate folds a namespace's old per-namespace subscription file into
// the subscriber's own, which is where subscriptions live now: they are the
// subscriber's state, not the mailbox owner's.
//
// The rows being folded were written into the OWNER's store, and every one of
// them names a mailbox in the owner's own space -- so folding them restores the
// owner's own subscriptions exactly, at the cost of the owner also inheriting
// rows a peer created, all of which point at mailboxes the owner already sees.
// Nothing foreign is disclosed and nothing of the owner's is lost.
//
// Authorship was never recorded, so a peer's subscription cannot be returned to
// the peer: the migration restores the owner's subscriptions and does not
// restore anyone else's. Peers re-subscribe themselves.
//
// Dry run unless "apply": the operation changes what a user's client shows.
func (s *Server) handleSubsMigrate(w http.ResponseWriter, r *http.Request) {
	var req subsMigrateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.User == "" {
		apiError(w, errUserRequired.Error(), http.StatusBadRequest)
		return
	}
	if req.Namespace == "" {
		apiError(w, "namespace required (the one that no longer keeps its own subscriptions)", http.StatusBadRequest)
		return
	}
	spec, ok := s.namespaceByName(req.Namespace)
	if !ok {
		apiError(w, "namespace "+req.Namespace+" not configured", http.StatusBadRequest)
		return
	}
	if spec.KeepsSubscriptions() {
		apiError(w, "namespace "+req.Namespace+" keeps its own subscriptions; nothing to migrate", http.StatusBadRequest)
		return
	}
	uc, err := s.openUserContext(req.User)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer uc.Close()
	uc.setActor(req.Actor)

	bundle, err := uc.ns(s, req.Namespace)
	if err != nil {
		apiError(w, err.Error(), http.StatusBadRequest)
		return
	}
	personal, ok := uc.handles["personal"]
	if !ok || personal == nil {
		apiError(w, "no personal namespace to hold the subscriptions", http.StatusInternalServerError)
		return
	}

	// Both names this namespace's file has had: the current one, and the
	// pre-#1159 form where the slug was taken as a path. The older one holds
	// the owner's rows too, so it is folded, not dropped.
	root := bundle.folderControlRoot()
	oldFiles := []string{
		mailbox.NamespaceSubsFile(spec.Prefix, spec.Separator, spec.Type),
		"subscriptions-" + strings.ToLower(strings.TrimSuffix(spec.Prefix, "/")),
	}

	dest := subs.New(personal.folderControlRoot(), mailbox.NamespaceSubsFile(personal.spec.Prefix, personal.spec.Separator, personal.spec.Type),
		uc.info.Username, uc.lockOwner(), s.opts.Locker)
	existing, err := dest.Snapshot()
	if err != nil {
		apiError(w, "read destination: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// The visible prefix: an owner-templated namespace is addressed with the
	// owner expanded, and that is the key the session writes today.
	visiblePrefix := spec.Prefix
	if mailbox.PrefixIsOwnerTemplated(spec.Prefix) {
		visiblePrefix = strings.Replace(spec.Prefix, mailbox.OwnerVar, uc.info.Username, 1)
	}

	folded := make([]string, 0)
	already := make([]string, 0)
	seen := make(map[string]bool)
	sources := make([]string, 0, len(oldFiles))
	for _, name := range oldFiles {
		path := filepath.Join(root, name)
		st, statErr := os.Stat(path)
		if statErr != nil || st.IsDir() {
			// A directory here is the pre-#1159 form of the OTHER name: the
			// slug was taken as a path, so "subscriptions-user" is the
			// directory holding "subscriptions-user/%u". The file inside is the
			// source; the directory itself is not.
			continue
		}
		sources = append(sources, name)
		src := subs.New(root, name, uc.info.Username, uc.lockOwner(), s.opts.Locker)
		rows, rerr := src.Snapshot()
		if rerr != nil {
			apiError(w, "read "+name+": "+rerr.Error(), http.StatusInternalServerError)
			return
		}
		for rel := range rows {
			key := visiblePrefix + rel
			if seen[key] {
				continue
			}
			seen[key] = true
			if _, dup := existing[key]; dup {
				already = append(already, key)
				continue
			}
			folded = append(folded, key)
		}
	}
	sort.Strings(folded)
	sort.Strings(already)

	if req.Apply {
		for _, key := range folded {
			if aerr := dest.Add(key); aerr != nil {
				apiError(w, "write destination: "+aerr.Error(), http.StatusInternalServerError)
				return
			}
		}
		// Remove the old files only after every row is in the destination, so a
		// failure leaves the source intact and the run repeatable.
		for _, name := range sources {
			if rerr := os.Remove(filepath.Join(root, name)); rerr != nil {
				apiError(w, "remove "+name+": "+rerr.Error(), http.StatusInternalServerError)
				return
			}
			// The pre-#1159 name is a path, so removing the file leaves its
			// directory behind -- and that directory carries the name the
			// current file wants, so it would block writing one. Remove it when
			// it is empty; a non-empty one is left alone (Remove refuses it).
			if dir := filepath.Dir(name); dir != "." {
				_ = os.Remove(filepath.Join(root, dir))
			}
		}
	}
	apiJSON(w, map[string]any{
		"status":  "ok",
		"applied": req.Apply,
		"sources": sources,
		"folded":  folded,
		"already": already,
	})
}

type subsMigrateRequest struct {
	User      string `json:"user"`
	Namespace string `json:"namespace"`
	Actor     string `json:"actor,omitempty"`
	Apply     bool   `json:"apply,omitempty"`
}
