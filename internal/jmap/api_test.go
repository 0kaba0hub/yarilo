package jmap

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// apiRequest is a batch arriving from the login layer.
func apiRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, apiPath, strings.NewReader(body))
	r.RemoteAddr = trustedPeer
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(hdrUser, "u1@example.com")
	r.Header.Set(hdrSessionID, "deadbeefdeadbeef")
	r.Header.Set(hdrProxyTTL, "4")
	r.Header.Set(hdrForwarded, `for="203.0.113.7:51234";proto=https;by="10.1.2.3"`)
	return r
}

func postAPI(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	trustedServer(t).Handler().ServeHTTP(w, apiRequest(body))
	return w
}

// The batch runs and answers every call by id, which is what a client matches
// on.
func TestAPIRunsABatch(t *testing.T) {
	w := postAPI(t, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[
		["Core/echo",{"ids":["m1","m2"]},"c0"],
		["Core/echo",{"#ids":{"resultOf":"c0","name":"Core/echo","path":"/ids"}},"c1"]
	]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp struct {
		MethodResponses []json.RawMessage `json:"methodResponses"`
		SessionState    string            `json:"sessionState"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.MethodResponses) != 2 {
		t.Fatalf("got %d responses: %s", len(resp.MethodResponses), w.Body)
	}
	var second jmapcore.Invocation
	if err := json.Unmarshal(resp.MethodResponses[1], &second); err != nil {
		t.Fatalf("second response: %v", err)
	}
	if second.Name != "Core/echo" || second.CallID != "c1" {
		t.Fatalf("second response = %s/%s: %s", second.Name, second.CallID, second.Args)
	}
	// The back-reference must have produced the first call's ids.
	var args struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(second.Args, &args); err != nil {
		t.Fatalf("second args: %v", err)
	}
	if len(args.IDs) != 2 || args.IDs[0] != "m1" {
		t.Errorf("back-reference produced %v", args.IDs)
	}
	if resp.SessionState == "" {
		t.Error("response carries no sessionState")
	}
}

// The trust gate covers the API endpoint too: there is no route that reads the
// asserted user without it.
func TestAPIIsBehindTheTrustGate(t *testing.T) {
	s := New(Options{Trust: ResolveTrust(false, false, nil), Limits: testLimits()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, apiRequest(`{"using":[],"methodCalls":[]}`))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

// A request-level fault is a problem document, and the batch does not run.
func TestAPIRequestLevelProblems(t *testing.T) {
	tests := []struct {
		name, body string
		wantStatus int
		wantType   string
	}{
		{
			name:       "not JSON",
			body:       `{"using":[`,
			wantStatus: http.StatusBadRequest,
			wantType:   jmapcore.ProblemNotJSON,
		},
		{
			name:       "not a request",
			body:       `{"using":[]}`,
			wantStatus: http.StatusBadRequest,
			wantType:   jmapcore.ProblemNotRequest,
		},
		{
			name:       "unknown capability",
			body:       `{"using":["urn:example:nope"],"methodCalls":[]}`,
			wantStatus: http.StatusBadRequest,
			wantType:   jmapcore.ProblemUnknownCapability,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := postAPI(t, tt.body)
			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q", ct)
			}
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got["type"] != tt.wantType {
				t.Errorf("type = %v, want %s", got["type"], tt.wantType)
			}
		})
	}
}

// An oversized body is a JMAP limit problem naming the bound, not a bare 413:
// the client has to be able to parse what it got back.
func TestAPIOversizedBodyIsALimitProblem(t *testing.T) {
	s := New(Options{
		Trust:  ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits: jmapcore.Limits{MaxSizeRequest: 64, MaxCallsInRequest: 16},
	})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, apiRequest(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[`+
		strings.Repeat(`["Core/echo",{"pad":"xxxxxxxxxxxxxxxx"},"c0"],`, 20)+`["Core/echo",{},"cz"]]}`))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", w.Code, w.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["type"] != jmapcore.ProblemLimit {
		t.Errorf("type = %v, want %s", got["type"], jmapcore.ProblemLimit)
	}
	if got["limit"] != "maxSizeRequest" {
		t.Errorf("limit = %v, want maxSizeRequest", got["limit"])
	}
}

// A broken mail store must not take out the methods that do not need it.
// Core/echo exists to answer when other things cannot, so it is the one method
// that must never fail on a storage dependency (#994).
func TestBrokenStoreLeavesStorelessMethodsWorking(t *testing.T) {
	s := New(Options{
		Trust:  ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits: testLimits(),
		// Wired but unusable: a resolver that always fails stands in for an
		// auth listener that answers the wrong protocol.
		Storage: &Storage{
			Mailbox:     nil,
			Index:       nil,
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return nil, errors.New("userdb down") },
		},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, apiRequest(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls":[
			["Core/echo",{"alive":true},"c0"],
			["Mailbox/get",{"accountId":"u1@example.com"},"c1"],
			["Core/echo",{"still":true},"c2"]]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a broken store must not fail the request: %s", w.Code, w.Body)
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.MethodResponses) != 3 {
		t.Fatalf("got %d responses for 3 calls", len(resp.MethodResponses))
	}
	want := []string{"Core/echo", "error", "Core/echo"}
	for i, wantName := range want {
		var name string
		if err := json.Unmarshal(resp.MethodResponses[i][0], &name); err != nil {
			t.Fatalf("response %d name: %v", i, err)
		}
		if name != wantName {
			t.Errorf("response %d = %s, want %s", i, name, wantName)
		}
	}
}
