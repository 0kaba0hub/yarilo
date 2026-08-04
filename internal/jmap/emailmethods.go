package jmap

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// emailGet implements Email/get (RFC 8621 §4.2).
// propsOf returns the requested property names, or nil when the client named
// none — which asks for all of them.
func propsOf(req jmapcore.EmailGetRequest) []string {
	if req.Properties == nil {
		return nil
	}
	return *req.Properties
}

func (s *Server) emailGet(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.EmailGetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	// A null ids would mean every message the account has, which for mail is
	// unbounded. RFC 8621 §4.2 allows a server to refuse it, and refusing is
	// the only answer that keeps the request's cost bounded.
	if req.IDs == nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments,
			Description: "Email/get requires ids; a null ids would select every message"}
	}
	if n := s.opts.Limits.MaxObjectsInGet; n > 0 && len(*req.IDs) > n {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrRequestTooLarge,
			Description: "more ids than maxObjectsInGet"}
	}

	want := make(map[string]bool, len(*req.IDs))
	for _, id := range *req.IDs {
		want[id] = true
	}
	found, err := s.findMessages(h, want)
	if err != nil {
		slog.Warn("jmap: Email/get lookup failed", "account", accountID, "err", err)
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrServerFail}
	}

	// Projected, not the object itself: a response must carry the properties
	// the client asked for and no others (RFC 8620 §5.1). Returning the whole
	// object states every field as fact, including the ones this request never
	// computed.
	resp := jmapcore.GetResponse[any]{
		AccountID: accountID,
		State:     "0", // Email state tracking arrives with Email/changes.
		List:      []any{},
		NotFound:  []string{},
	}
	// Answer in the order the client asked, which is what it renders.
	for _, id := range *req.IDs {
		ref, ok := found[id]
		if !ok {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		email, headerFields, err := s.buildEmail(h, ref, req, s.opts.MaxBodyValueBytes)
		if err != nil {
			// One unreadable message must not fail the whole call: the client
			// asked for several and the rest are answerable.
			slog.Warn("jmap: Email/get build failed", "account", accountID, "id", id, "err", err)
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		resp.List = append(resp.List, jmapcore.Project(email, propsOf(req), headerFields))
	}
	return resp, nil
}
