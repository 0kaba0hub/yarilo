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

	"github.com/0kaba0hub/yarilo/internal/anvil"
)

// startKickAnvil spins an in-process anvil server, returns its
// address and a subscription channel on "kick:imap" that the
// test asserts against.
func startKickAnvil(t *testing.T) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv := anvil.NewServer(0)
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

	subConn, err := anvil.Dial(addr, nil, time.Second)
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
	addr, ch := startKickAnvil(t)

	s := New(Options{AnvilAddr: addr})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{
		"session_id": "sess-123",
		"user":       "alice@example.com",
		"protocols":  []string{"imap"},
	})
	resp, err := http.Post(ts.URL+"/api/backend/sessions/kick", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
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
	s := New(Options{AnvilAddr: "127.0.0.1:1"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/backend/sessions/kick", "application/json",
		strings.NewReader(`{"user":"alice@example.com"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestKickEndpoint_RejectsMissingAnvil(t *testing.T) {
	s := New(Options{}) // no AnvilAddr
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/backend/sessions/kick", "application/json",
		strings.NewReader(`{"session_id":"sess-1"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}
