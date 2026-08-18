package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// mailboxRegistry binds the Mailbox methods for one authenticated user. The
// handle is per request, so the closures capture it rather than a session.
func (s *Server) mailboxRegistry(lazy *lazyStore, accountID string) jmapcore.Registry {
	return jmapcore.Registry{
		"Mailbox/get": {Capability: jmapcore.CapMail, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.mailboxGet(ctx, h, accountID, args)
			})
		}},
		"Mailbox/query": {Capability: jmapcore.CapMail, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.mailboxQuery(ctx, h, accountID, args)
			})
		}},
		"Thread/get": {Capability: jmapcore.CapMail, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.threadGet(ctx, h, accountID, args)
			})
		}},
		"Email/query": {Capability: jmapcore.CapMail, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.emailQuery(ctx, h, accountID, args)
			})
		}},
		"Email/get": {Capability: jmapcore.CapMail, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.emailGet(ctx, h, accountID, args)
			})
		}},
		"Mailbox/changes": {Capability: jmapcore.CapMail, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.mailboxChanges(ctx, h, accountID, args)
			})
		}},
		"Email/changes": {Capability: jmapcore.CapMail, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.emailChanges(ctx, h, accountID, args)
			})
		}},
		"Email/set": {Capability: jmapcore.CapMail, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.emailSet(ctx, h, accountID, args)
			})
		}},
		"SearchSnippet/get": {Capability: jmapcore.CapMail, Fn: func(ctx context.Context, args json.RawMessage) (any, *jmapcore.MethodError) {
			return s.withStore(lazy, func(h *userHandle) (any, *jmapcore.MethodError) {
				return s.searchSnippet(ctx, h, accountID, args)
			})
		}},
	}
}

// withStore opens the user's mail on first use and turns a failure into a
// method-level error. The batch keeps its shape: the calls that need no store
// still answer, and each call that does gets told why it could not.
func (s *Server) withStore(lazy *lazyStore, fn func(*userHandle) (any, *jmapcore.MethodError)) (any, *jmapcore.MethodError) {
	h, err := lazy.get()
	if err != nil {
		slog.Warn("jmap: mail store unavailable", "err", err)
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrServerFail, Description: "mail store unavailable"}
	}
	return fn(h)
}

// mailboxGet implements Mailbox/get (RFC 8621 §2.1).
func (s *Server) mailboxGet(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.GetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	// Property names are checked before any work: an unknown one is refused
	// rather than ignored, so a client can tell its own typo from a property
	// yarilo has not implemented (§5.1).
	if unknown := jmapcore.UnknownProperties(jmapcore.Mailbox{}, propsOfGet(req)); len(unknown) > 0 {
		return nil, jmapcore.InvalidProperties(unknown)
	}
	all, err := s.mailboxList(h)
	if err != nil {
		slog.Warn("jmap: Mailbox/get failed", "account", accountID, "err", err)
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrServerFail}
	}
	props := propsOfGet(req)
	resp := jmapcore.GetResponse[any]{
		AccountID: accountID,
		State:     mailboxState(all),
		List:      []any{},
		NotFound:  []string{},
	}
	// A null ids means every mailbox; an empty array means none. The two are
	// different requests, which is why GetRequest.IDs is a pointer (§5.1).
	if req.IDs == nil {
		for _, mb := range all {
			resp.List = append(resp.List, jmapcore.Project(mb, props, nil))
		}
		return resp, nil
	}
	byID := make(map[string]jmapcore.Mailbox, len(all))
	for _, mb := range all {
		byID[mb.ID] = mb
	}
	for _, id := range *req.IDs {
		if mb, ok := byID[id]; ok {
			resp.List = append(resp.List, jmapcore.Project(mb, props, nil))
			continue
		}
		resp.NotFound = append(resp.NotFound, id)
	}
	return resp, nil
}

// mailboxQuery implements Mailbox/query (RFC 8621 §2.3).
func (s *Server) mailboxQuery(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.MailboxQueryRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	if req.Filter != nil && req.Filter.Operator != "" {
		// AND/OR/NOT nodes are a later phase. Refusing is the only honest
		// answer: matching everything would look like a working filter.
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrUnsupportedFilter,
			Description: "filter operators are not supported yet"}
	}
	all, err := s.mailboxList(h)
	if err != nil {
		slog.Warn("jmap: Mailbox/query failed", "account", accountID, "err", err)
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrServerFail}
	}

	matched := make([]jmapcore.Mailbox, 0, len(all))
	for _, mb := range all {
		if matchesMailbox(mb, req.Filter) {
			matched = append(matched, mb)
		}
	}
	if merr := sortMailboxes(matched, req.Sort); merr != nil {
		return nil, merr
	}

	ids := make([]string, 0, len(matched))
	for _, mb := range matched {
		ids = append(ids, mb.ID)
	}
	total := uint(len(ids))
	pos := req.Position
	if pos < 0 {
		// A negative position counts back from the end (§5.5).
		pos = len(ids) + pos
		if pos < 0 {
			pos = 0
		}
	}
	if pos > len(ids) {
		pos = len(ids)
	}
	ids = ids[pos:]
	if req.Limit != nil && uint(len(ids)) > *req.Limit {
		ids = ids[:*req.Limit]
	}

	resp := jmapcore.QueryResponse{
		AccountID:  accountID,
		QueryState: mailboxState(all),
		// The query state is a digest of the whole mailbox set, not a change
		// log: it tells a client the result may have moved, never how. Until
		// Mailbox/changes lands there is nothing to calculate from.
		CanCalculateChanges: false,
		Position:            pos,
		IDs:                 ids,
		Limit:               req.Limit,
	}
	if req.CalculateTotal {
		resp.Total = &total
	}
	return resp, nil
}

// checkAccount refuses a request aimed at another account. One user has one
// account here, so anything else is a client error, not an empty result.
func checkAccount(got, want string) *jmapcore.MethodError {
	if got == "" || got == want {
		return nil
	}
	return &jmapcore.MethodError{Type: jmapcore.ErrAccountNotFound}
}

func matchesMailbox(mb jmapcore.Mailbox, f *jmapcore.MailboxFilter) bool {
	if f == nil {
		return true
	}
	if f.ParentID != nil {
		if mb.ParentID == nil || *mb.ParentID != *f.ParentID {
			return false
		}
	}
	if f.Name != nil && !strings.Contains(strings.ToLower(mb.Name), strings.ToLower(*f.Name)) {
		return false
	}
	if f.Role != nil {
		if mb.Role == nil || *mb.Role != *f.Role {
			return false
		}
	}
	if f.HasAnyRole != nil && (mb.Role != nil) != *f.HasAnyRole {
		return false
	}
	if f.IsSubscribed != nil && mb.IsSubscribed != *f.IsSubscribed {
		return false
	}
	return true
}

// sortMailboxes applies the client's comparators. An unknown property is
// refused rather than ignored: a client that asked for an order and got another
// one silently renders the wrong list.
func sortMailboxes(list []jmapcore.Mailbox, cmps []jmapcore.Comparator) *jmapcore.MethodError {
	for _, c := range cmps {
		switch c.Property {
		case "sortOrder", "name", "parentId":
		default:
			return &jmapcore.MethodError{Type: jmapcore.ErrUnsupportedSort,
				Description: fmt.Sprintf("cannot sort on %q", c.Property)}
		}
	}
	if len(cmps) == 0 {
		return nil
	}
	sort.SliceStable(list, func(i, j int) bool {
		for _, c := range cmps {
			a, b := mailboxKey(list[i], c.Property), mailboxKey(list[j], c.Property)
			if a == b {
				continue
			}
			if c.Ascending() {
				return a < b
			}
			return a > b
		}
		return false
	})
	return nil
}

func mailboxKey(mb jmapcore.Mailbox, property string) string {
	switch property {
	case "name":
		return mb.Name
	case "parentId":
		if mb.ParentID == nil {
			return ""
		}
		return *mb.ParentID
	default:
		// sortOrder is uniform until a client can set it, so the name breaks
		// the tie and the order stays repeatable.
		return mb.Name
	}
}
