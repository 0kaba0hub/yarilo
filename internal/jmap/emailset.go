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
	// The state Email/get reports, so a client can compare what it holds
	// against what the account is, and a write against a stale view is refused
	// rather than applied over a change the client has not seen.
	state, serr := s.emailState(h)
	if serr != nil {
		slog.Warn("jmap: Email/set state failed", "account", accountID, "err", serr)
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrServerFail}
	}
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

	// Grouped by folder, and by what the update means: a replacement and the
	// two halves of a delta are three different calls to the store, because
	// one FlagsUpdate carries one mode. Each is still a batch, so a client
	// marking a thread read pays one round of locking rather than one per
	// message.
	type folderWork struct {
		folder  string
		set     map[uint32]mailbox.FlagsUpdate
		add     map[uint32]mailbox.FlagsUpdate
		remove  map[uint32]mailbox.FlagsUpdate
		idOfUID map[uint32]string
	}
	work := map[uint64]*folderWork{}
	workFor := func(ref messageRef) *folderWork {
		w, ok := work[ref.folderID]
		if !ok {
			w = &folderWork{
				folder: ref.folder,
				set:    map[uint32]mailbox.FlagsUpdate{}, add: map[uint32]mailbox.FlagsUpdate{},
				remove: map[uint32]mailbox.FlagsUpdate{}, idOfUID: map[uint32]string{},
			}
			work[ref.folderID] = w
		}
		return w
	}

	for id, patch := range req.Update {
		ref, ok := found[id]
		if !ok {
			resp.NotUpdated[id] = &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
			continue
		}
		plan, serr := patchedKeywords(patch)
		if serr != nil {
			resp.NotUpdated[id] = serr
			continue
		}
		w := workFor(ref)
		w.idOfUID[ref.meta.UID] = id
		if plan.replace != nil {
			flags, custom := splitKeywords(plan.replace)
			w.set[ref.meta.UID] = mailbox.FlagsUpdate{Mode: mailbox.FlagsSet, Flags: flags, Keywords: custom}
			continue
		}
		if len(plan.add) > 0 {
			flags, custom := splitKeywords(plan.add)
			w.add[ref.meta.UID] = mailbox.FlagsUpdate{Mode: mailbox.FlagsAdd, Flags: flags, Keywords: custom}
		}
		if len(plan.remove) > 0 {
			flags, custom := splitKeywords(plan.remove)
			w.remove[ref.meta.UID] = mailbox.FlagsUpdate{Mode: mailbox.FlagsRemove, Flags: flags, Keywords: custom}
		}
		if len(plan.add) == 0 && len(plan.remove) == 0 {
			// An update naming nothing is not an error; it changes nothing and
			// is reported as done, which is what the client asked for.
			resp.Updated[id] = nil
			delete(w.idOfUID, ref.meta.UID)
		}
	}

	for folderID, w := range work {
		failed := map[string]*jmapcore.SetError{}
		applied := map[uint32]bool{}
		// A mixed patch is two calls. They are not one transaction, and that is
		// the price of relative writes: another session can observe the added
		// keyword before the removed one is gone. Nothing is lost either way,
		// which is the property a replacement could not offer.
		for _, batch := range []map[uint32]mailbox.FlagsUpdate{w.set, w.add, w.remove} {
			if len(batch) == 0 {
				continue
			}
			results, err := h.idx.UpdateFlagsMulti(folderID, batch)
			if err != nil {
				slog.Warn("jmap: Email/set write failed", "folder", w.folder, "err", err)
				for uid := range batch {
					failed[w.idOfUID[uid]] = &jmapcore.SetError{
						Type: jmapcore.SetErrForbidden, Description: fmt.Sprintf("could not write flags: %v", err),
					}
				}
				continue
			}
			for uid := range batch {
				if _, ok := results[uid]; !ok {
					// The store skips a UID it no longer has: between the
					// lookup and the write the message was expunged.
					failed[w.idOfUID[uid]] = &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
					continue
				}
				applied[uid] = true
			}
		}
		for uid, id := range w.idOfUID {
			if serr, bad := failed[id]; bad {
				resp.NotUpdated[id] = serr
				continue
			}
			if !applied[uid] {
				resp.NotUpdated[id] = &jmapcore.SetError{Type: jmapcore.SetErrNotFound}
				continue
			}
			// A null value means "updated, with no server-set properties the
			// client did not already know" (§5.3).
			resp.Updated[id] = nil
		}
	}
	// newState is read after the writes rather than assumed to have moved: a
	// call whose every object failed changed nothing, and reporting a new state
	// for it would tell the client to discard a cache that is still correct.
	if len(resp.Updated) > 0 {
		if after, err := s.emailState(h); err == nil {
			resp.NewState = after
		} else {
			slog.Warn("jmap: Email/set could not read the state after writing", "account", accountID, "err", err)
		}
	}
	return resp, nil
}

// keywordPlan is what one update object asks the store to do. The distinction
// is not cosmetic: a whole "keywords" object is the client saying "this is the
// set now", which is a replacement; a "keywords/<name>" patch is the client
// saying "add this one", which is a delta and must stay one.
//
// Resolving a patch into a full set against a snapshot read outside the lock
// re-opens the lost-update the store closed for IMAP (#1282): between the
// lookup and the write another session can add a keyword, and a replacement
// computed from the older snapshot erases it. A delta cannot, because the store
// folds it against the record under its own lock.
type keywordPlan struct {
	// replace is non-nil for the whole-object form and is the complete set.
	replace map[string]bool
	// add and remove are the patch form, and are relative.
	add, remove map[string]bool
}

// patchedKeywords reads one update object into a plan.
//
// Any property other than keywords is refused rather than ignored. Silently
// accepting "mailboxIds" would answer a move with success and leave the message
// where it was -- the client would believe it had moved.
func patchedKeywords(patch json.RawMessage) (keywordPlan, *jmapcore.SetError) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(patch, &fields); err != nil {
		return keywordPlan{}, &jmapcore.SetError{Type: jmapcore.SetErrInvalidPatch, Description: err.Error()}
	}

	plan := keywordPlan{add: map[string]bool{}, remove: map[string]bool{}}
	var unsupported []string
	sawPatchForm := false

	for name, raw := range fields {
		switch {
		case name == "keywords":
			replacement := map[string]bool{}
			if err := json.Unmarshal(raw, &replacement); err != nil {
				return keywordPlan{}, &jmapcore.SetError{
					Type: jmapcore.SetErrInvalidPatch, Properties: []string{"keywords"}, Description: err.Error(),
				}
			}
			plan.replace = map[string]bool{}
			for k, v := range replacement {
				if v {
					plan.replace[strings.ToLower(k)] = true
				}
			}
		case strings.HasPrefix(name, "keywords/"):
			sawPatchForm = true
			kw := strings.ToLower(strings.TrimPrefix(name, "keywords/"))
			if kw == "" {
				return keywordPlan{}, &jmapcore.SetError{
					Type: jmapcore.SetErrInvalidPatch, Properties: []string{name},
					Description: "a keywords patch must name a keyword",
				}
			}
			// True adds; null or false removes -- §5.3 gives null the meaning
			// "remove this property".
			var set *bool
			if err := json.Unmarshal(raw, &set); err != nil {
				return keywordPlan{}, &jmapcore.SetError{
					Type: jmapcore.SetErrInvalidPatch, Properties: []string{name}, Description: err.Error(),
				}
			}
			if set != nil && *set {
				plan.add[kw] = true
			} else {
				plan.remove[kw] = true
			}
		default:
			unsupported = append(unsupported, name)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return keywordPlan{}, &jmapcore.SetError{
			Type:        jmapcore.SetErrNotImplemented,
			Properties:  unsupported,
			Description: "only keywords can be updated; mailboxIds (move) and the rest are not implemented",
		}
	}
	// RFC 8620 §5.3 forbids giving a property directly and by patch in one
	// update: the two say different things about the same set and there is no
	// order that makes both true.
	if plan.replace != nil && sawPatchForm {
		return keywordPlan{}, &jmapcore.SetError{
			Type: jmapcore.SetErrInvalidPatch, Properties: []string{"keywords"},
			Description: "keywords was given both directly and as a patch",
		}
	}
	return plan, nil
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
