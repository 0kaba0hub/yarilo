package jmap

import (
	"context"
	"encoding/json"

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
	want := make(map[string]bool, len(*req.IDs))
	for _, id := range *req.IDs {
		want[id] = true
	}
	found, err := s.findMessages(h, want)
	if err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrServerFail}
	}
	resp := jmapcore.GetResponse[jmapcore.Thread]{
		AccountID: accountID,
		State:     "0",
		List:      []jmapcore.Thread{},
		NotFound:  []string{},
	}
	for _, id := range *req.IDs {
		if _, ok := found[id]; !ok {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		resp.List = append(resp.List, jmapcore.Thread{ID: id, EmailIDs: []string{id}})
	}
	return resp, nil
}
