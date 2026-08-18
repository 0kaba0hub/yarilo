package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// jmapToIMAPFlag is the inverse of the systemFlags table in keywordsOf. The two
// directions are written once each rather than derived from one another, and a
// test walks the pair, because a keyword that can be read and not written is
// the failure this method exists to avoid.
var jmapToIMAPFlag = map[string]string{
	"$seen":     `\Seen`,
	"$answered": `\Answered`,
	"$flagged":  `\Flagged`,
	"$draft":    `\Draft`,
	"$deleted":  `\Deleted`,
}

// emailSet implements Email/set (RFC 8621 §4.6), keywords only.
//
// Scope is deliberate and visible to the client rather than silent: creating,
// destroying and moving between mailboxes answer with a SetError naming what is
// not built, so a client learns it from the response instead of from a message
// that never appears. Keywords come first because they are the operation a mail
// client cannot work without -- marking a message read -- and because the store
// beneath them already journals keyword changes (#1281), so the write is
// visible to every other session without a base rewrite.
func (s *Server) emailSet(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.SetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	// The state is still the placeholder Email/get reports, so the only honest
	// thing ifInState can do is refuse a value that does not match it. Once the
	// composite state lands this becomes a real comparison without the callers
	// changing.
	const state = "0"
	if req.IfInState != nil && *req.IfInState != state {
		return nil, &jmapcore.MethodError{
			Type:        jmapcore.ErrStateMismatch,
			Description: "the account has changed since the state the client holds",
		}
	}

	resp := jmapcore.NewSetResponse(accountID, state)

	for id := range req.Create {
		resp.NotCreated[id] = &jmapcore.SetError{
			Type:        jmapcore.SetErrNotImplemented,
			Description: "Email/set create is not implemented; append over IMAP or deliver over LMTP",
		}
	}
	for _, id := range req.Destroy {
		resp.NotDestroyed[id] = &jmapcore.SetError{
			Type:        jmapcore.SetErrNotImplemented,
			Description: "Email/set destroy is not implemented; expunge over IMAP",
		}
	}
	if len(req.Update) == 0 {
		return resp, nil
	}

	want := make(map[string]bool, len(req.Update))
	for id := range req.Update {
		want[id] = true
	}
	found, err := s.findMessages(h, want)
	if err != nil {
		slog.Warn("jmap: Email/set lookup failed", "account", accountID, "err", err)
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrServerFail}
	}

	// Grouped by folder: the store applies a batch under one lock, so a client
	// marking a thread read pays one round of locking rather than one per
	// message.
	byFolder := map[uint64]map[uint32]mailbox.FlagsUpdate{}
	folderOf := map[uint64]string{}
	idOfUID := map[uint64]map[uint32]string{}

	for id, patch := range req.Update {
		ref, ok := found[id]
		if !ok {
			resp.NotUpdated[id] = &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
			continue
		}
		keywords, serr := patchedKeywords(keywordsOf(ref.meta), patch)
		if serr != nil {
			resp.NotUpdated[id] = serr
			continue
		}
		flags, custom := splitKeywords(keywords)
		if byFolder[ref.folderID] == nil {
			byFolder[ref.folderID] = map[uint32]mailbox.FlagsUpdate{}
			idOfUID[ref.folderID] = map[uint32]string{}
			folderOf[ref.folderID] = ref.folder
		}
		byFolder[ref.folderID][ref.meta.UID] = mailbox.FlagsUpdate{
			Mode: mailbox.FlagsSet, Flags: flags, Keywords: custom,
		}
		idOfUID[ref.folderID][ref.meta.UID] = id
	}

	for folderID, updates := range byFolder {
		results, err := h.idx.UpdateFlagsMulti(folderID, updates)
		if err != nil {
			// The folder failed as a whole, so every id in it is reported as
			// failed. Leaving them out of both maps would read as success.
			slog.Warn("jmap: Email/set write failed", "folder", folderOf[folderID], "err", err)
			for _, id := range idOfUID[folderID] {
				resp.NotUpdated[id] = &jmapcore.SetError{
					Type: jmapcore.SetErrForbidden, Description: fmt.Sprintf("could not write flags: %v", err),
				}
			}
			continue
		}
		for uid, id := range idOfUID[folderID] {
			if _, ok := results[uid]; !ok {
				// The store skips a UID it no longer has; between the lookup
				// and the write the message was expunged.
				resp.NotUpdated[id] = &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
				continue
			}
			// A null value means "updated, with no server-set properties the
			// client did not already know" (§5.3).
			resp.Updated[id] = nil
		}
	}
	return resp, nil
}

// patchedKeywords resolves one update object into the keyword set the message
// should end up with. Both forms of §5.3 patching are accepted: a whole
// "keywords" object replaces the set, and "keywords/<name>" entries add or
// remove one at a time.
//
// Any other property is refused rather than ignored. Silently accepting
// "mailboxIds" would answer a move with success and leave the message where it
// was -- the client would believe it had moved.
func patchedKeywords(current map[string]bool, patch json.RawMessage) (map[string]bool, *jmapcore.SetError) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(patch, &fields); err != nil {
		return nil, &jmapcore.SetError{Type: jmapcore.SetErrInvalidPatch, Description: err.Error()}
	}

	out := make(map[string]bool, len(current))
	for k := range current {
		out[k] = true
	}

	var unsupported []string
	for name, raw := range fields {
		switch {
		case name == "keywords":
			replacement := map[string]bool{}
			if err := json.Unmarshal(raw, &replacement); err != nil {
				return nil, &jmapcore.SetError{
					Type: jmapcore.SetErrInvalidPatch, Properties: []string{"keywords"}, Description: err.Error(),
				}
			}
			out = map[string]bool{}
			for k, v := range replacement {
				if v {
					out[strings.ToLower(k)] = true
				}
			}
		case strings.HasPrefix(name, "keywords/"):
			kw := strings.ToLower(strings.TrimPrefix(name, "keywords/"))
			if kw == "" {
				return nil, &jmapcore.SetError{
					Type: jmapcore.SetErrInvalidPatch, Properties: []string{name},
					Description: "a keywords patch must name a keyword",
				}
			}
			// A patch is true to add, or null/false to remove; §5.3 gives null
			// the meaning "remove this property".
			var set *bool
			if err := json.Unmarshal(raw, &set); err != nil {
				return nil, &jmapcore.SetError{
					Type: jmapcore.SetErrInvalidPatch, Properties: []string{name}, Description: err.Error(),
				}
			}
			if set != nil && *set {
				out[kw] = true
			} else {
				delete(out, kw)
			}
		default:
			unsupported = append(unsupported, name)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return nil, &jmapcore.SetError{
			Type:        jmapcore.SetErrNotImplemented,
			Properties:  unsupported,
			Description: "only keywords can be updated; mailboxIds (move) and the rest are not implemented",
		}
	}
	return out, nil
}

// splitKeywords maps a JMAP keyword set onto what the store takes: IMAP system
// flags for the five with a standard meaning, and the rest as keywords in their
// own right.
func splitKeywords(keywords map[string]bool) (flags, custom []string) {
	for kw := range keywords {
		if f, ok := jmapToIMAPFlag[strings.ToLower(kw)]; ok {
			flags = append(flags, f)
			continue
		}
		custom = append(custom, kw)
	}
	// Sorted so a write is reproducible and a test can compare without
	// caring about map order.
	sort.Strings(flags)
	sort.Strings(custom)
	return flags, custom
}
