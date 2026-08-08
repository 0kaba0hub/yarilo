// The owner registry: who granted what to whom in owner-templated
// namespaces, kept in a dict so LIST user/* can enumerate the owners the
// caller may see (#1168). Key space is the reference's, verbatim, so a
// dict_import migration is mechanical:
//
//	shared/shared-boxes/user/<seer>/<owner>   = 1
//	shared/shared-boxes/group/<group>/<owner> = 1
//	shared/shared-boxes/anyone/<owner>        = 1
//
// The second, reverse layout answers "what has this owner granted so far"
// as a prefix scan instead of a scan of the whole key space (the reference
// gates it behind acl_dict_index for historical reasons; we have no history,
// so both layouts are always written -- the cost is one extra set per grant):
//
//	shared/shared-user-boxes-rev/<owner>/user/<seer>   = 1
//	shared/shared-user-boxes-rev/<owner>/group/<group> = 1
//	shared/shared-user-boxes-rev/<owner>/anyone        = 1
//
// The registry is a projection of the yarilo-acl-list index, which is itself
// a projection of the ACL files: it is synced where the index is written, in
// the same critical section, so there is one derivation chain, not two
// independent ones from one source (the #1147/#1152/#1160 shape).
package acl

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const (
	fwdPrefix = "shared/shared-boxes/"
	revPrefix = "shared/shared-user-boxes-rev/"
)

// Registry projects one owner's grants into the shared dict.
type Registry struct {
	dict  dict.Dict
	owner string
}

// NewRegistry binds the shared dict to one owner's namespace store. d may be
// nil (registry disabled); every method is then a no-op.
func NewRegistry(d dict.Dict, owner string) *Registry {
	if d == nil {
		return nil
	}
	return &Registry{dict: d, owner: owner}
}

// identPath maps a positive ACL identifier to its registry path segment, or
// "" for identifiers that grant nobody discovery (owner: implicit; invalid:
// nothing). authenticated folds into anyone, as in the reference.
func identPath(id mailbox.Identifier) string {
	switch id.Type {
	case mailbox.IDUser:
		return "user/" + dict.Escape(id.Name)
	case mailbox.IDGroup, mailbox.IDGroupOverride:
		return "group/" + dict.Escape(id.Name)
	case mailbox.IDAnyone, mailbox.IDAuthenticated:
		return "anyone"
	}
	return ""
}

// SyncFromList reconciles the registry with the identifier set derived from a
// full index snapshot. complete says the snapshot is a full successful read;
// a partial one only ADDS -- removing on incomplete data breaks spaces the
// walker could not see (the reference's no_removes rule).
func (r *Registry) SyncFromList(entries []ListEntry, complete bool) error {
	if r == nil {
		return nil
	}
	wanted := make(map[string]bool)
	for _, e := range entries {
		if e.Negative || e.Rights == "" {
			continue // a denial or an empty grant lets nobody discover the space
		}
		if p := identPath(e.Identifier); p != "" {
			wanted[p] = true
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	current := make(map[string]bool)
	haveCurrent := false
	it, err := r.dict.Iterate(ctx, nil, revPrefix+dict.Escape(r.owner)+"/", dict.IterRecurse|dict.IterNoValue)
	if err == nil {
		iterOK := true
		for it.Next() {
			key := strings.TrimPrefix(it.Key(), revPrefix+dict.Escape(r.owner)+"/")
			current[key] = true
		}
		if cerr := it.Close(); cerr != nil {
			iterOK = false
		}
		haveCurrent = iterOK
	}
	if !haveCurrent {
		// Every wanted row is re-set and nothing is removed -- correct
		// (no_removes) but worth a trace, or a persistently failing dict
		// looks like a healthy one doing full writes.
		slog.Debug("userstate/acl: registry current-rows read failed; add-only sync", "owner", r.owner)
	}

	tx, err := r.dict.Begin(ctx, nil)
	if err != nil {
		return fmt.Errorf("userstate/acl: registry begin: %w", err)
	}
	one := []byte("1")
	for p := range wanted {
		if !current[p] {
			if err := tx.Set(r.fwdKey(p), one); err != nil {
				return fmt.Errorf("userstate/acl: registry set: %w", err)
			}
			if err := tx.Set(revPrefix+dict.Escape(r.owner)+"/"+p, one); err != nil {
				return fmt.Errorf("userstate/acl: registry set: %w", err)
			}
		}
	}
	// Removals only from a complete snapshot AND a successful current read:
	// deleting against partial knowledge is how someone else's visible space
	// goes dark.
	if complete && haveCurrent {
		for p := range current {
			if !wanted[p] {
				if err := tx.Unset(r.fwdKey(p)); err != nil {
					return fmt.Errorf("userstate/acl: registry unset: %w", err)
				}
				if err := tx.Unset(revPrefix + dict.Escape(r.owner) + "/" + p); err != nil {
					return fmt.Errorf("userstate/acl: registry unset: %w", err)
				}
			}
		}
	}
	if res, err := tx.Commit(); err != nil || res != dict.CommitOK {
		return fmt.Errorf("userstate/acl: registry commit: result=%v err=%w", res, err)
	}
	return nil
}

// fwdKey turns a rev-layout path segment (user/<seer>, group/<g>, anyone)
// into the forward key ending with the owner.
func (r *Registry) fwdKey(identPath string) string {
	return fwdPrefix + identPath + "/" + dict.Escape(r.owner)
}

// OwnersFor returns the owners whose spaces the caller may discover: a
// prefix scan per identity (their user, each group, anyone), last key
// component is the owner -- the reference's iterate shape. Rows may be stale
// (a revoked grant not yet reconciled); the caller resolves each owner
// through the same visibility gate as any verb, so a stale row and an
// invented owner produce the same silence.
func OwnersFor(ctx context.Context, d dict.Dict, user string, groups []string) ([]string, error) {
	if d == nil {
		return nil, nil
	}
	paths := []string{fwdPrefix + "user/" + dict.Escape(user) + "/", fwdPrefix + "anyone/"}
	for _, g := range groups {
		paths = append(paths, fwdPrefix+"group/"+dict.Escape(g)+"/")
	}
	seen := make(map[string]bool)
	owners := make([]string, 0)
	for _, p := range paths {
		it, err := d.Iterate(ctx, nil, p, dict.IterRecurse|dict.IterNoValue)
		if err != nil {
			return nil, fmt.Errorf("userstate/acl: registry scan %s: %w", p, err)
		}
		for it.Next() {
			key := it.Key()
			owner := dict.Unescape(key[strings.LastIndex(key, "/")+1:])
			if owner != "" && !seen[owner] {
				seen[owner] = true
				owners = append(owners, owner)
			}
		}
		if err := it.Close(); err != nil {
			return nil, fmt.Errorf("userstate/acl: registry scan %s: %w", p, err)
		}
	}
	return owners, nil
}

// RegistrySync reprojects the registry from the current index snapshot -- the
// admin repair verb. A no-op without an attached registry.
func (s *Store) RegistrySync() error {
	if s.registry == nil {
		return nil
	}
	entries, err := s.ListSnapshot()
	if err != nil {
		return fmt.Errorf("userstate/acl: registry sync snapshot: %w", err)
	}
	return s.registry.SyncFromList(entries, true)
}
