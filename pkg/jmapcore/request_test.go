package jmapcore

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func declared() []string { return []string{CapCore, CapMail} }

// The envelope is rejected as a whole for a request-level fault: the client
// gets a problem document, not a batch of failures.
func TestParseRequestRejections(t *testing.T) {
	lim := testLimits()
	tests := []struct {
		name, body string
		wantType   string
		wantLimit  string
	}{
		{
			name:     "not JSON",
			body:     `{"using":[`,
			wantType: ProblemNotJSON,
		},
		{
			name:     "no methodCalls",
			body:     `{"using":["urn:ietf:params:jmap:core"]}`,
			wantType: ProblemNotRequest,
		},
		{
			name:     "malformed invocation",
			body:     `{"using":[],"methodCalls":[["Core/echo",{}]]}`,
			wantType: ProblemNotRequest,
		},
		{
			name:     "unknown capability",
			body:     `{"using":["urn:example:nope"],"methodCalls":[]}`,
			wantType: ProblemUnknownCapability,
		},
		{
			name:      "too many calls",
			body:      manyCalls(lim.MaxCallsInRequest + 1),
			wantType:  ProblemLimit,
			wantLimit: "maxCallsInRequest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, rerr := ParseRequest([]byte(tt.body), declared(), lim)
			if rerr == nil {
				t.Fatalf("accepted %s", tt.body)
			}
			if rerr.Type != tt.wantType {
				t.Errorf("type = %s, want %s", rerr.Type, tt.wantType)
			}
			if rerr.Limit != tt.wantLimit {
				t.Errorf("limit = %q, want %q", rerr.Limit, tt.wantLimit)
			}
		})
	}
}

// The URN a client asked for has to appear in the problem, or an operator
// cannot tell which capability was missing.
func TestUnknownCapabilityNamesTheURN(t *testing.T) {
	_, rerr := ParseRequest([]byte(`{"using":["urn:example:nope"],"methodCalls":[]}`), declared(), testLimits())
	if rerr == nil {
		t.Fatal("accepted an unknown capability")
	}
	if !strings.Contains(rerr.Detail, "urn:example:nope") {
		t.Errorf("detail = %q, want it to name the URN", rerr.Detail)
	}
}

// §3.6.1 requires the limit member on a limit problem: "too big" is useless
// without knowing which bound was hit.
func TestLimitProblemCarriesTheLimitMember(t *testing.T) {
	_, rerr := ParseRequest([]byte(manyCalls(testLimits().MaxCallsInRequest+1)), declared(), testLimits())
	if rerr == nil {
		t.Fatal("accepted an over-long batch")
	}
	w := httptest.NewRecorder()
	rerr.Write(w)
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["limit"] != "maxCallsInRequest" {
		t.Errorf("limit member = %v", got["limit"])
	}
	if got["type"] != ProblemLimit {
		t.Errorf("type = %v", got["type"])
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// Core is not implicit: a method whose capability the client did not declare is
// answered exactly like one that does not exist (RFC 8620 §3.2).
func TestUndeclaredCapabilityHidesTheMethod(t *testing.T) {
	tests := []struct {
		name    string
		using   []string
		wantErr bool
	}{
		{name: "declared", using: []string{CapCore}},
		{name: "empty using", using: []string{}, wantErr: true},
		{name: "other capability", using: []string{CapMail}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &Request{Using: tt.using, MethodCalls: []Invocation{
				{Name: "Core/echo", Args: json.RawMessage(`{"a":1}`), CallID: "c0"},
			}}
			resp := Execute(context.Background(), req, CoreRegistry(), testLimits())
			got := resp.MethodResponses[0]
			if tt.wantErr {
				if got.Name != "error" {
					t.Fatalf("response = %s, want error", got.Name)
				}
				var e MethodError
				if err := json.Unmarshal(got.Args, &e); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if e.Type != ErrUnknownMethod {
					t.Errorf("error type = %s, want %s", e.Type, ErrUnknownMethod)
				}
				return
			}
			if got.Name != "Core/echo" {
				t.Errorf("response = %s", got.Name)
			}
		})
	}
}

// A failed call must not abort the batch, and its call id must still be
// answered: the client matches responses by id, not by position.
func TestBatchKeepsShapeOnFailure(t *testing.T) {
	req := &Request{Using: []string{CapCore}, MethodCalls: []Invocation{
		{Name: "Core/echo", Args: json.RawMessage(`{"n":1}`), CallID: "c0"},
		{Name: "Nope/method", Args: json.RawMessage(`{}`), CallID: "c1"},
		{Name: "Core/echo", Args: json.RawMessage(`{"n":2}`), CallID: "c2"},
	}}
	resp := Execute(context.Background(), req, CoreRegistry(), testLimits())
	if len(resp.MethodResponses) != 3 {
		t.Fatalf("got %d responses for 3 calls", len(resp.MethodResponses))
	}
	want := []struct{ name, callID string }{
		{"Core/echo", "c0"}, {"error", "c1"}, {"Core/echo", "c2"},
	}
	for i, w := range want {
		if resp.MethodResponses[i].Name != w.name || resp.MethodResponses[i].CallID != w.callID {
			t.Errorf("response %d = %s/%s, want %s/%s", i,
				resp.MethodResponses[i].Name, resp.MethodResponses[i].CallID, w.name, w.callID)
		}
	}
}

// The batch is what makes a back-reference useful: the second call reads the
// first one's result without a round trip.
func TestBatchResolvesBackReference(t *testing.T) {
	req := &Request{Using: []string{CapCore}, MethodCalls: []Invocation{
		{Name: "Core/echo", Args: json.RawMessage(`{"ids":["m1","m2"]}`), CallID: "c0"},
		{Name: "Core/echo", Args: json.RawMessage(
			`{"#ids":{"resultOf":"c0","name":"Core/echo","path":"/ids"}}`), CallID: "c1"},
	}}
	resp := Execute(context.Background(), req, CoreRegistry(), testLimits())
	if n := len(resp.MethodResponses); n != 2 {
		t.Fatalf("got %d responses", n)
	}
	second := resp.MethodResponses[1]
	if second.Name != "Core/echo" {
		t.Fatalf("second response = %s: %s", second.Name, second.Args)
	}
	if !jsonEqual(t, second.Args, `{"ids":["m1","m2"]}`) {
		t.Errorf("back-reference produced %s", second.Args)
	}
}

// sessionState comes from the same derivation the session resource uses, or a
// client would be told its session went stale by one value and not the other.
func TestResponseSessionStateMatchesTheSessionResource(t *testing.T) {
	lim := testLimits()
	resp := Execute(context.Background(),
		&Request{Using: []string{CapCore}, MethodCalls: []Invocation{}}, CoreRegistry(), lim)
	if resp.SessionState != BuildSession(lim, "u1@example.com").State {
		t.Errorf("sessionState = %q, session resource = %q",
			resp.SessionState, BuildSession(lim, "u1@example.com").State)
	}
}

func manyCalls(n int) string {
	calls := make([]Invocation, 0, n)
	for i := 0; i < n; i++ {
		calls = append(calls, Invocation{Name: "Core/echo", Args: json.RawMessage(`{}`), CallID: "c"})
	}
	body, err := json.Marshal(Request{Using: []string{CapCore}, MethodCalls: calls})
	if err != nil {
		panic(err)
	}
	return string(body)
}
