package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Foo/changes answers "what moved since the state I hold". It is computed by
// diffing the client's description against the account as it is now -- which is
// why the state carries a description rather than a digest (#1216).
//
// Every way of not knowing ends in cannotCalculateChanges, never in a confident
// empty answer: a state from another format version, a state for another object
// type, and a window whose expunge history has been folded away all take the
// same exit. An empty destroyed list means "nothing was deleted", and saying
// that when we cannot see is how a client keeps listing messages that are gone.
func (s *Server) emailChanges(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.ChangesRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	old, merr := parseSince(req.SinceState, jmapcore.KindEmail)
	if merr != nil {
		return nil, merr
	}

	folders, err := s.folderMarks(h)
	if err != nil {
		return nil, storeFailure("Email/changes state failed", accountID, err)
	}
	newState := folders.description().String()

	resp := &jmapcore.ChangesResponse{
		AccountID: accountID, OldState: req.SinceState, NewState: newState,
		Created: []string{}, Updated: []string{}, Destroyed: []string{},
	}
	if req.SinceState == newState {
		return resp, nil
	}

	oldByKey := map[[8]byte]jmapcore.StateEntry{}
	for _, e := range old.Entries {
		oldByKey[e.Key] = e
	}

	for _, f := range folders {
		prev, known := oldByKey[f.key]
		if !known {
			// A folder the client has never seen: everything in it is new.
			ids, err := s.folderMessageIDs(h, f, 0, 0)
			if err != nil {
				return nil, storeFailure("Email/changes read", accountID, err)
			}
			resp.Created = append(resp.Created, ids.created...)
			continue
		}
		delete(oldByKey, f.key)
		prevUIDValidity, prevModSeq, prevNextUID := fieldsOf(prev)
		if prevUIDValidity != uint64(f.folder.UIDValidity) {
			// The folder was recreated under the same identity: nothing the
			// client holds for it is valid any more.
			return nil, cannotCalculate("a mailbox was recreated; its messages must be refetched")
		}
		if prevModSeq == f.folder.HighestModSeq {
			continue // nothing happened here
		}
		// The gate: expunges older than the floor were folded away, so this
		// folder cannot answer "what vanished since". One such folder refuses
		// the WHOLE call -- a partial answer carrying a fresh state would tell
		// the client it is current, which is the same silence with a newer
		// label.
		floor, err := h.idx.ExpungeFloor(f.folder.ID)
		if err != nil {
			return nil, storeFailure("Email/changes floor", accountID, err)
		}
		if floor > prevModSeq {
			return nil, cannotCalculate("the expunge history for this account no longer reaches that state")
		}
		ids, err := s.folderMessageIDs(h, f, prevModSeq, uint32(prevNextUID))
		if err != nil {
			return nil, storeFailure("Email/changes read", accountID, err)
		}
		resp.Created = append(resp.Created, ids.created...)
		resp.Updated = append(resp.Updated, ids.updated...)

		guids, complete, err := h.idx.VanishedGUIDs(f.folder.ID, prevModSeq)
		if err != nil {
			return nil, storeFailure("Email/changes vanished", accountID, err)
		}
		if !complete {
			// Some expunge in range cannot be named -- a record written before
			// the field carried the message id. Reporting the rest would be a
			// destroyed list that is missing entries, which a client reads as
			// "those still exist".
			return nil, cannotCalculate("some deletions in that window cannot be identified")
		}
		for _, g := range guids {
			resp.Destroyed = append(resp.Destroyed, mailbox.FormatObjectID(g))
		}
	}

	// Folders in the old description and not in the new one are gone. Their
	// messages went with them, and the expunge records went with the folder, so
	// there is nothing left to enumerate: the client is told to refetch rather
	// than handed a destroyed list we cannot build.
	if len(oldByKey) > 0 {
		return nil, cannotCalculate("a mailbox was deleted; its messages cannot be enumerated after the fact")
	}
	if merr := capChanges(req.MaxChanges, len(resp.Created)+len(resp.Updated)+len(resp.Destroyed)); merr != nil {
		return nil, merr
	}
	return resp, nil
}

// mailboxChanges is the same question for mailboxes, where the whole answer is
// in the two descriptions: an entry that appeared is created, one that changed
// its digest is updated, and one that is gone is DESTROYED -- named explicitly,
// because a client that only sees an entry stop appearing never learns the
// mailbox was deleted.
func (s *Server) mailboxChanges(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.ChangesRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	old, merr := parseSince(req.SinceState, jmapcore.KindMailbox)
	if merr != nil {
		return nil, merr
	}
	list, err := s.mailboxList(h)
	if err != nil {
		return nil, storeFailure("Mailbox/changes failed", accountID, err)
	}

	resp := &jmapcore.ChangesResponse{
		AccountID: accountID, OldState: req.SinceState, NewState: mailboxState(list),
		Created: []string{}, Updated: []string{}, Destroyed: []string{},
	}
	oldByKey := map[[8]byte][]uint64{}
	for _, e := range old.Entries {
		if len(e.Fields) > 0 {
			oldByKey[e.Key] = e.Fields
		}
	}
	for _, mb := range list {
		key := mailboxKeyOf(mb.ID)
		fields, known := oldByKey[key]
		delete(oldByKey, key)
		switch {
		case !known:
			resp.Created = append(resp.Created, mb.ID)
		case fields[0] != mailboxDigest(mb):
			resp.Updated = append(resp.Updated, mb.ID)
		}
	}
	// What is left in the old description no longer exists, and has to be said
	// out loud: a client that merely stops seeing an entry never learns the
	// mailbox was deleted, and keeps it in its list for ever.
	for key, fields := range oldByKey {
		id, ok := destroyedMailboxID(key, fields)
		if !ok {
			return nil, cannotCalculate("a mailbox this server cannot name was deleted")
		}
		resp.Destroyed = append(resp.Destroyed, id)
	}
	if merr := capChanges(req.MaxChanges, len(resp.Created)+len(resp.Updated)+len(resp.Destroyed)); merr != nil {
		return nil, merr
	}
	return resp, nil
}

// parseSince turns the client's state into a description, or into the one
// answer that is always safe.
func parseSince(state string, kind byte) (jmapcore.Description, *jmapcore.MethodError) {
	if state == "" {
		return jmapcore.Description{}, &jmapcore.MethodError{
			Type: jmapcore.ErrInvalidArguments, Description: "sinceState is required",
		}
	}
	desc, err := jmapcore.ParseDescription(state, kind)
	if err == nil {
		return desc, nil
	}
	// Both failures mean the same thing to a client -- refetch -- and neither
	// may become a diff. ErrStateVersion is the expected one: a state written
	// by another build of this encoding, which we must not try to read.
	switch {
	case errors.Is(err, jmapcore.ErrStateVersion):
		return jmapcore.Description{}, cannotCalculate("that state was written in another format version")
	default:
		return jmapcore.Description{}, cannotCalculate("that state is not one this server issued")
	}
}

// cannotCalculate is the honest refusal: the client refetches, which RFC 8620
// §5.2 provides for exactly this.
func cannotCalculate(why string) *jmapcore.MethodError {
	return &jmapcore.MethodError{Type: jmapcore.ErrCannotCalculateChanges, Description: why}
}

// storeFailure is where a Go error becomes a method error, and therefore the
// only place that can classify it. jmapcore cannot: it must stay free of yarilo
// imports, so a wrapper around the dispatch would only ever see a MethodError
// that has already thrown the cause away.
//
// A dependency being restarted is temporary and the client should retry;
// serverFail says "something went wrong here", which a client treats as final
// and reports to its user (#1339).
func storeFailure(what, accountID string, err error) *jmapcore.MethodError {
	if errors.Is(err, locks.ErrUnavailable) {
		slog.Warn("jmap: "+what+" unavailable", "account", accountID, "err", err)
		return &jmapcore.MethodError{
			Type:        jmapcore.ErrServerUnavailable,
			Description: "the mail store is temporarily unavailable, try again",
		}
	}
	slog.Warn("jmap: "+what+" failed", "account", accountID, "err", err)
	return &jmapcore.MethodError{Type: jmapcore.ErrServerFail}
}

// capChanges enforces maxChanges. RFC 8620 §5.2 says a server that cannot
// answer within the limit returns tooManyChanges rather than a truncated list
// with a new state, which would tell the client it had seen everything.
func capChanges(maxChanges *uint, total int) *jmapcore.MethodError {
	if maxChanges == nil || *maxChanges == 0 || total <= int(*maxChanges) {
		return nil
	}
	return &jmapcore.MethodError{
		Type:        jmapcore.ErrTooManyChanges,
		Description: "more changes than maxChanges; ask again with a larger limit or refetch",
	}
}

func fieldsOf(e jmapcore.StateEntry) (uidValidity, modseq, nextUID uint64) {
	if len(e.Fields) > 0 {
		uidValidity = e.Fields[0]
	}
	if len(e.Fields) > 1 {
		modseq = e.Fields[1]
	}
	if len(e.Fields) > 2 {
		nextUID = e.Fields[2]
	}
	return uidValidity, modseq, nextUID
}

// mailboxQueryChanges is the same standing refusal for Mailbox/queryChanges,
// and for the same reason: a client that reached Mailbox/query will reach for
// this next, and an absent method answers unknownMethod, which reads as "this
// server is broken" rather than "run the query again".
func (s *Server) mailboxQueryChanges(_ context.Context, _ *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.ChangesRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	return nil, cannotCalculate("this server does not track query results; run the query again")
}

// emailQueryChanges implements Email/queryChanges (RFC 8620 §5.6) as a standing
// refusal, which is a decision rather than a gap.
//
// A query result depends on its filter and its sort, so saying which ids
// entered or left a window needs the previous result set. This server does not
// keep one -- that is per-client state on a mailbox that several clients and
// two protocols share -- so the honest answer is that changes cannot be
// calculated, which §5.6 provides for and every client handles by running the
// query again.
//
// It is implemented rather than left absent on purpose. An unimplemented method
// answers unknownMethod, which tells a client the server is broken or the
// capability was mis-advertised; cannotCalculateChanges tells it exactly what to
// do next, and costs one query instead of a failed sync.
func (s *Server) emailQueryChanges(_ context.Context, _ *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.ChangesRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	return nil, cannotCalculate("this server does not track query results; run the query again")
}
