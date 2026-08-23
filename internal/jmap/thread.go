package jmap

import (
	"context"
	"encoding/json"

	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// threadGet implements Thread/get (RFC 8621 §3). Threading is not computed yet,
// so every message is its own thread and a thread id is the id of its only
// message — the same model Mailbox's thread counts and Email.threadId already
// report. Answering with that is consistent; answering notFound would make a
// client believe the message has no thread at all.
func (s *Server) threadGet(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.GetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	if req.IDs == nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments,
			Description: "Thread/get requires ids; a null ids would select every thread"}
	}
	if unknown := jmapcore.UnknownProperties(jmapcore.Thread{}, propsOfGet(req)); len(unknown) > 0 {
		return nil, jmapcore.InvalidProperties(unknown)
	}
	want := make(map[string]bool, len(*req.IDs))
	for _, id := range *req.IDs {
		want[id] = true
	}
	found, err := s.findMessages(h, want)
	if err != nil {
		return nil, storeFailure("Thread/get", accountID, err)
	}
	members, err := h.threadMembers(*req.IDs)
	if err != nil {
		return nil, storeFailure("Thread/get", accountID, err)
	}
	props := propsOfGet(req)
	resp := jmapcore.GetResponse[any]{
		AccountID: accountID,
		State:     "0",
		List:      []any{},
		NotFound:  []string{},
	}
	for _, id := range *req.IDs {
		ids := members[id]
		if len(ids) == 0 {
			// No sidecar, or none for this id: the account behaves as it did
			// before threading, where a thread is the message it names. A
			// notFound would tell the client the message has no thread at all,
			// which is a different and wrong answer.
			if _, ok := found[id]; !ok {
				resp.NotFound = append(resp.NotFound, id)
				continue
			}
			ids = []string{id}
		}
		resp.List = append(resp.List, jmapcore.Project(
			jmapcore.Thread{ID: id, EmailIDs: ids}, props, nil))
	}
	return resp, nil
}

// threadMembers reads the conversations the requested ids belong to.
//
// One Read for the whole request: a reader that took the state per id could
// answer from two states -- a message's thread read before a merge, its
// members read after -- and hand the client a conversation that never existed.
//
// An account with no sidecar answers nothing here, and the caller falls back
// to the pre-threading shape. That is the state of every account the migration
// step has not reached, by decision (#1425), so it is the common path rather
// than an error.
func (h *userHandle) threadMembers(ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	if h.threads == nil {
		return out, nil
	}
	path := threads.PathFor(h.info)
	if path == "" {
		return out, nil
	}
	state, err := h.threads.Get(h.info.Username, path)
	if err != nil {
		return nil, err
	}
	state.Read(func(v threads.View) {
		for _, id := range ids {
			threadID, ok := v.ThreadOf(id)
			if !ok {
				continue
			}
			out[id] = v.Members(threadID)
		}
	})
	return out, nil
}

// threadOf reports the conversation one message belongs to, falling back to
// the message's own id.
//
// The fallback is the pre-threading answer and stays correct: an account the
// migration step has not reached has every message in a conversation of its
// own, and saying so is what the server did before this existed. Returning
// nothing instead would strip threadId from a response that requires it.
func (h *userHandle) threadOf(emailID string) string {
	if h.threads == nil {
		return emailID
	}
	path := threads.PathFor(h.info)
	if path == "" {
		return emailID
	}
	state, err := h.threads.Get(h.info.Username, path)
	if err != nil {
		// A sidecar that cannot be read is not a reason to fail a mail read:
		// the conversation is metadata, the message is not.
		return emailID
	}
	if id, ok := state.ThreadOfGUID(emailID); ok {
		return id
	}
	return emailID
}
