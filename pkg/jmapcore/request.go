package jmapcore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// Request is the API request envelope (RFC 8620 §3.3).
type Request struct {
	// Using lists the capabilities this request relies on. Core is not
	// implicit: a method whose capability is absent is answered with
	// unknownMethod, which is what lets a server add capabilities without
	// changing how older clients behave.
	Using       []string          `json:"using"`
	MethodCalls []Invocation      `json:"methodCalls"`
	CreatedIDs  map[string]string `json:"createdIds,omitempty"`
}

// Response is the API response envelope (RFC 8620 §3.4).
type Response struct {
	MethodResponses []Invocation      `json:"methodResponses"`
	CreatedIDs      map[string]string `json:"createdIds,omitempty"`
	// SessionState is the same value the session resource carries, so a client
	// can tell its cached session went stale without refetching it.
	SessionState string `json:"sessionState"`
}

// RequestError is a request-level failure (RFC 8620 §3.6.1): the batch is not
// run at all and the client gets a problem document rather than an envelope.
type RequestError struct {
	Type   string
	Status int
	Detail string
	// Limit names the exceeded limit. §3.6.1 requires it on a limit problem,
	// since "too big" is useless without knowing which bound was hit.
	Limit string
}

func (e *RequestError) Error() string { return e.Type + ": " + e.Detail }

// Write emits the problem document.
func (e *RequestError) Write(w http.ResponseWriter) {
	body := map[string]any{"type": e.Type, "status": e.Status, "detail": e.Detail}
	if e.Limit != "" {
		body["limit"] = e.Limit
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(e.Status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("jmapcore: problem write failed", "err", err)
	}
}

// Method runs one call. args have had their back-references resolved already.
// A returned MethodError becomes an "error" response for this call and leaves
// the rest of the batch running.
type Method func(ctx context.Context, args json.RawMessage) (any, *MethodError)

// MethodEntry binds a method to the capability a client must have declared in
// "using" to reach it.
type MethodEntry struct {
	Capability string
	Fn         Method
}

// Registry maps method names to their implementations.
type Registry map[string]MethodEntry

// ParseRequest decodes and validates the envelope. declared is the set of
// capability URNs this server advertises; it is passed in rather than read from
// anywhere, so this package stays free of a server's configuration.
func ParseRequest(body []byte, declared []string, lim Limits) (*Request, *RequestError) {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		// A malformed invocation triple is reported as notRequest: the JSON
		// parsed, it just was not a request.
		if !json.Valid(body) {
			return nil, &RequestError{Type: ProblemNotJSON, Status: http.StatusBadRequest,
				Detail: "The request body is not valid JSON."}
		}
		return nil, &RequestError{Type: ProblemNotRequest, Status: http.StatusBadRequest,
			Detail: err.Error()}
	}
	if req.MethodCalls == nil {
		return nil, &RequestError{Type: ProblemNotRequest, Status: http.StatusBadRequest,
			Detail: "The request has no methodCalls array."}
	}
	for _, urn := range req.Using {
		if !contains(declared, urn) {
			return nil, &RequestError{Type: ProblemUnknownCapability, Status: http.StatusBadRequest,
				Detail: fmt.Sprintf("The server does not support %s.", urn)}
		}
	}
	if n := lim.MaxCallsInRequest; n > 0 && len(req.MethodCalls) > n {
		return nil, &RequestError{Type: ProblemLimit, Status: http.StatusBadRequest,
			Limit:  "maxCallsInRequest",
			Detail: fmt.Sprintf("The request has %d method calls; the limit is %d.", len(req.MethodCalls), n)}
	}
	return &req, nil
}

// Execute runs the batch in order and returns the envelope. Order is what makes
// back-references well-defined: a call can only see results already produced.
func Execute(ctx context.Context, req *Request, reg Registry, lim Limits) *Response {
	resp := &Response{
		MethodResponses: make([]Invocation, 0, len(req.MethodCalls)),
		CreatedIDs:      req.CreatedIDs,
		SessionState:    SessionState(lim),
	}
	for _, call := range req.MethodCalls {
		resp.MethodResponses = append(resp.MethodResponses, invoke(ctx, call, req.Using, reg, resp.MethodResponses))
	}
	return resp
}

func invoke(ctx context.Context, call Invocation, using []string, reg Registry, done []Invocation) Invocation {
	entry, ok := reg[call.Name]
	// A method the client did not declare the capability for is indistinguishable
	// from one the server does not implement, by design (RFC 8620 §3.2).
	if !ok || (entry.Capability != "" && !contains(using, entry.Capability)) {
		return errorInvocation(&MethodError{Type: ErrUnknownMethod}, call.CallID)
	}
	args, merr := resolveBackRefs(call.Args, done)
	if merr != nil {
		return errorInvocation(merr, call.CallID)
	}
	result, merr := entry.Fn(ctx, args)
	if merr != nil {
		return errorInvocation(merr, call.CallID)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return errorInvocation(&MethodError{Type: ErrServerFail, Description: err.Error()}, call.CallID)
	}
	return Invocation{Name: call.Name, Args: raw, CallID: call.CallID}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
