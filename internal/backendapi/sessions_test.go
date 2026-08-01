package backendapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/warden"
)

// startKickWarden spins an in-process warden server, returns its
// address and a subscription channel on "kick:imap" that the
// test asserts against.
func startKickWarden(t *testing.T) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv := warden.NewServer(0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.ListenAndServe(ctx, addr, nil)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	subConn, err := warden.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("dial sub: %v", err)
	}
	subCtx, subCancel := context.WithCancel(context.Background())
	ch, err := subConn.Subscribe(subCtx, "kick:imap")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	t.Cleanup(func() {
		subCancel()
		cancel()
		<-done
	})
	return addr, ch
}

func TestKickEndpoint_EmitsBroadcast(t *testing.T) {
	addr, ch := startKickWarden(t)

	s := New(Options{WardenAddr: addr})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{
		"session_id": "sess-123",
		"user":       "alice@example.com",
		"protocols":  []string{"imap"},
	})
	resp := mustPostKick(t, ts.URL, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	select {
	case payload := <-ch:
		if payload != "sess-123" {
			t.Errorf("event payload = %q, want sess-123", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no kick event on kick:imap within 2s")
	}
}

func TestKickEndpoint_RejectsMissingSessionID(t *testing.T) {
	s := New(Options{WardenAddr: "127.0.0.1:1"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp := mustPostKick(t, ts.URL, []byte(`{"user":"alice@example.com"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestKickEndpoint_RejectsMissingWarden(t *testing.T) {
	s := New(Options{}) // no WardenAddr
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp := mustPostKick(t, ts.URL, []byte(`{"session_id":"sess-1"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// mustPostKick issues POST /api/backend/sessions/kick with a
// caller-supplied JSON body. Uses NewRequestWithContext so it
// passes golangci-lint's noctx check; behaviour is otherwise
// identical to a bare http.Post.
func mustPostKick(t *testing.T, baseURL string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		baseURL+"/api/backend/sessions/kick", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}
