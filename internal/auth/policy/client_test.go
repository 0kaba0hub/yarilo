package policy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// captured holds what a test policy server saw on the last call.
type captured struct {
	method   string
	url      string
	headers  http.Header
	body     []byte
	bodyJSON map[string]interface{}
}

func newPolicyServer(t *testing.T, status int, respBody string) (*httptest.Server, *captured) {
	t.Helper()
	cap := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.url = r.URL.String()
		cap.headers = r.Header.Clone()
		cap.body, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(cap.body, &cap.bodyJSON)
		if status != 0 {
			w.WriteHeader(status)
		}
		if respBody != "" {
			w.Write([]byte(respBody)) //nolint:errcheck
		}
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

// TestNew_DisabledURL — empty URL returns (nil, nil) so callers
// can skip without nil-checking everywhere.
func TestNew_DisabledURL(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Errorf("expected nil client for empty URL, got %+v", c)
	}
	// nil client's methods are safe.
	d, err := c.CheckBefore(context.Background(), Request{})
	if err != nil || !d.Continue {
		t.Errorf("nil client CheckBefore: d=%+v err=%v", d, err)
	}
}

// TestNew_RequiresNonce — set URL, no nonce → error.
func TestNew_RequiresNonce(t *testing.T) {
	_, err := New(Config{URL: "http://x"})
	if err == nil {
		t.Error("expected error when URL set but nonce empty")
	}
}

// TestCheckBefore_AllowsOnZeroStatus — status==0 → Continue.
func TestCheckBefore_AllowsOnZeroStatus(t *testing.T) {
	srv, cap := newPolicyServer(t, 200, `{"status":0}`)
	c, err := New(Config{URL: srv.URL, HashNonce: "salt"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.CheckBefore(context.Background(), Request{
		Username: "alice", Password: "p", RemoteIP: "1.2.3.4",
		Service: "imap", TLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Continue || d.Reject || d.TarpitSecs != 0 {
		t.Errorf("status=0 decision = %+v", d)
	}
	if !strings.HasSuffix(cap.url, "?command=allow") {
		t.Errorf("unexpected URL: %s", cap.url)
	}
	if cap.method != "POST" {
		t.Errorf("method = %s, want POST", cap.method)
	}
	// Payload presence checks.
	for _, k := range []string{"device_id", "fail_type", "login",
		"protocol", "pwhash", "remote", "session_id", "tls"} {
		if _, ok := cap.bodyJSON[k]; !ok {
			t.Errorf("payload missing key %q: %v", k, cap.bodyJSON)
		}
	}
	if cap.bodyJSON["login"] != "alice" || cap.bodyJSON["remote"] != "1.2.3.4" {
		t.Errorf("payload identity wrong: %v", cap.bodyJSON)
	}
	// CheckBefore must NOT include success / policy_reject.
	if _, ok := cap.bodyJSON["success"]; ok {
		t.Errorf("CheckBefore payload should omit success: %v", cap.bodyJSON)
	}
}

// TestCheckBefore_RejectsOnNegativeStatus — status<0 → Reject.
func TestCheckBefore_RejectsOnNegativeStatus(t *testing.T) {
	srv, _ := newPolicyServer(t, 200, `{"status":-1,"msg":"too noisy"}`)
	c, _ := New(Config{URL: srv.URL, HashNonce: "salt"})
	d, err := c.CheckBefore(context.Background(), Request{Username: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Reject || d.Continue {
		t.Errorf("status<0 decision = %+v", d)
	}
	if d.Message != "too noisy" {
		t.Errorf("msg lost: %q", d.Message)
	}
}

// TestCheckBefore_TarpitsOnPositiveStatus — status>0 → Continue
// with TarpitSecs set.
func TestCheckBefore_TarpitsOnPositiveStatus(t *testing.T) {
	srv, _ := newPolicyServer(t, 200, `{"status":5}`)
	c, _ := New(Config{URL: srv.URL, HashNonce: "salt"})
	d, err := c.CheckBefore(context.Background(), Request{Username: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Continue || d.Reject || d.TarpitSecs != 5 {
		t.Errorf("status>0 decision = %+v", d)
	}
}

// TestCheckAfter_IncludesSuccessFields — CheckAfter payload has
// success + policy_reject keys; CheckBefore does not.
func TestCheckAfter_IncludesSuccessFields(t *testing.T) {
	srv, cap := newPolicyServer(t, 200, `{"status":0}`)
	c, _ := New(Config{URL: srv.URL, HashNonce: "salt"})
	c.CheckAfter(context.Background(), Request{Username: "x"}, true, false) //nolint:errcheck
	if v, ok := cap.bodyJSON["success"]; !ok || v != true {
		t.Errorf("success field missing/wrong: %v", cap.bodyJSON)
	}
	if v, ok := cap.bodyJSON["policy_reject"]; !ok || v != false {
		t.Errorf("policy_reject field missing/wrong: %v", cap.bodyJSON)
	}
}

// TestFailoverDecision_DefaultAllow — server unreachable AND
// RejectOnFail=false → Continue.
func TestFailoverDecision_DefaultAllow(t *testing.T) {
	c, _ := New(Config{URL: "http://127.0.0.1:1", HashNonce: "salt", Timeout: 50 * time.Millisecond})
	d, err := c.CheckBefore(context.Background(), Request{})
	if err == nil {
		t.Error("expected error on unreachable server")
	}
	if !d.Continue || d.Reject {
		t.Errorf("default-allow failover broken: %+v", d)
	}
}

// TestFailoverDecision_RejectOnFail — opposite stance.
func TestFailoverDecision_RejectOnFail(t *testing.T) {
	c, _ := New(Config{
		URL: "http://127.0.0.1:1", HashNonce: "salt",
		Timeout: 50 * time.Millisecond, RejectOnFail: true,
	})
	d, _ := c.CheckBefore(context.Background(), Request{})
	if !d.Reject {
		t.Errorf("RejectOnFail decision = %+v", d)
	}
}

// TestLogOnly_IgnoresReject — log-only mode treats Reject as
// continue (but caller's log surfaces the would-have-rejected).
func TestLogOnly_IgnoresReject(t *testing.T) {
	srv, _ := newPolicyServer(t, 200, `{"status":-1,"msg":"abuse"}`)
	c, _ := New(Config{URL: srv.URL, HashNonce: "salt", LogOnly: true})
	d, err := c.CheckBefore(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Continue || d.Reject {
		t.Errorf("log-only failed to ignore reject: %+v", d)
	}
}

// TestAPIHeader_FormatKeyValue — "Key: value" → custom header.
func TestAPIHeader_FormatKeyValue(t *testing.T) {
	srv, cap := newPolicyServer(t, 200, `{"status":0}`)
	c, _ := New(Config{URL: srv.URL, HashNonce: "salt", APIHeader: "X-Wforce: secret"})
	c.CheckBefore(context.Background(), Request{}) //nolint:errcheck
	if cap.headers.Get("X-Wforce") != "secret" {
		t.Errorf("custom header missing: %v", cap.headers)
	}
}

// TestAPIHeader_FormatBare — bare value → X-API-Key.
func TestAPIHeader_FormatBare(t *testing.T) {
	srv, cap := newPolicyServer(t, 200, `{"status":0}`)
	c, _ := New(Config{URL: srv.URL, HashNonce: "salt", APIHeader: "secret"})
	c.CheckBefore(context.Background(), Request{}) //nolint:errcheck
	if cap.headers.Get("X-API-Key") != "secret" {
		t.Errorf("default header missing: %v", cap.headers)
	}
}

// TestHash_DifferentNonceProducesDifferentHash — two nonces
// produce different pwhashes for the same user+password.
func TestHash_DifferentNonceProducesDifferentHash(t *testing.T) {
	c1, _ := New(Config{URL: "http://x", HashNonce: "n1"})
	c2, _ := New(Config{URL: "http://x", HashNonce: "n2"})
	h1 := c1.hashPassword("alice", "secret")
	h2 := c2.hashPassword("alice", "secret")
	if h1 == h2 {
		t.Errorf("nonce ignored: %q == %q", h1, h2)
	}
}

// TestHash_Truncate12Bits — 12 bits → 3 hex chars after the
// non-significant nibble is masked off. Verify length is correct
// (2 bytes hex = 4 chars, top 12 bits keep all 4).
func TestHash_Truncate12Bits(t *testing.T) {
	c, _ := New(Config{URL: "http://x", HashNonce: "salt", HashTruncateBits: 12})
	h := c.hashPassword("alice", "secret")
	// 12 bits → 2 bytes (keepBytes=1, remBits=4) → hex = 4 chars
	if len(h) != 4 {
		t.Errorf("12-bit truncated hash hex len = %d, want 4: %q", len(h), h)
	}
}

// TestHash_TruncateFull — 0 → no truncation, full sha256 = 64 hex.
func TestHash_TruncateFull(t *testing.T) {
	// 0 in Config goes through New's default to 12; pass explicit
	// large value to disable.
	c, _ := New(Config{URL: "http://x", HashNonce: "salt", HashTruncateBits: 256})
	h := c.hashPassword("alice", "secret")
	if len(h) != 64 {
		t.Errorf("untruncated sha256 hex = %d chars: %q", len(h), h)
	}
}

// TestReportAfter_FireAndForget — report mode swallows non-OK
// responses without surfacing errors.
func TestReportAfter_FireAndForget(t *testing.T) {
	srv, cap := newPolicyServer(t, 500, "broken")
	c, _ := New(Config{URL: srv.URL, HashNonce: "salt"})
	c.ReportAfter(context.Background(), Request{Username: "x"}, true, false)
	if !strings.HasSuffix(cap.url, "?command=report") {
		t.Errorf("report URL wrong: %s", cap.url)
	}
}

// TestURLAmpersandPreserved — URL ending in `&` extends the
// query-string instead of starting one.
func TestURLAmpersandPreserved(t *testing.T) {
	if got := joinCommand("http://x?tenant=42&", "allow"); got != "http://x?tenant=42&command=allow" {
		t.Errorf("&-suffix joinCommand wrong: %q", got)
	}
	if got := joinCommand("http://x/api", "allow"); got != "http://x/api?command=allow" {
		t.Errorf("plain joinCommand wrong: %q", got)
	}
}

// TestConcurrency_NoDataRace — hammer the client from many
// goroutines to flush out shared-state bugs.
func TestConcurrency_NoDataRace(t *testing.T) {
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		w.Write([]byte(`{"status":0}`)) //nolint:errcheck
	}))
	defer srv.Close()
	c, _ := New(Config{URL: srv.URL, HashNonce: "salt"})
	const N = 50
	done := make(chan struct{}, N)
	for i := 0; i < N; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			c.CheckBefore(context.Background(), Request{Username: "alice"}) //nolint:errcheck
		}()
	}
	for i := 0; i < N; i++ {
		<-done
	}
	if atomic.LoadInt64(&count) != N {
		t.Errorf("got %d requests, want %d", count, N)
	}
}
