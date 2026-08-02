package backendapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/warden"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func startWardenForTest(t *testing.T) (string, context.CancelFunc) {
	t.Helper()
	srv := warden.NewServer(10)
	ctx, cancel := context.WithCancel(context.Background())
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(30 * time.Millisecond)
	t.Cleanup(cancel)

	// Seed two sessions via the wire protocol.
	for _, sess := range []struct {
		id, user, ip, service string
	}{
		{"s1", "alice@example.com", "1.1.1.1", "imap"},
		{"s2", "bob@example.com", "2.2.2.2", "pop3"},
	} {
		c, err := warden.Dial(addr, nil, time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := c.Connect(sess.id, sess.user, sess.ip, sess.service); err != nil {
			c.Close()
			t.Fatalf("connect %s: %v", sess.id, err)
		}
		// Keep the conn open until cleanup so the session stays
		// registered.
		t.Cleanup(c.Close)
	}
	return addr, cancel
}

func whoTestServer(t *testing.T, wardenAddr string) *httptest.Server {
	t.Helper()
	s := New(Options{
		WardenAddr: wardenAddr,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestWhoReturnsAllSessionsGroupedByUser(t *testing.T) {
	wardenAddr, _ := startWardenForTest(t)
	ts := whoTestServer(t, wardenAddr)

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/who", "", map[string]any{})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp struct {
		Total  int `json:"total"`
		Groups []struct {
			User     string `json:"user"`
			Total    int    `json:"total"`
			Sessions []struct {
				ID      string `json:"id"`
				IP      string `json:"ip"`
				Service string `json:"service"`
			} `json:"sessions"`
		} `json:"groups"`
	}
	decodeJSONBody(t, body, &resp)
	if resp.Total != 2 {
		t.Errorf("total=%d want 2", resp.Total)
	}
	if len(resp.Groups) != 2 {
		t.Fatalf("groups=%d want 2", len(resp.Groups))
	}
}

func TestWhoProtocolFilter(t *testing.T) {
	wardenAddr, _ := startWardenForTest(t)
	ts := whoTestServer(t, wardenAddr)

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/who", "", map[string]any{
		"service": "imap",
	})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp struct {
		Total  int `json:"total"`
		Groups []struct {
			User string `json:"user"`
		} `json:"groups"`
	}
	decodeJSONBody(t, body, &resp)
	if resp.Total != 1 || len(resp.Groups) != 1 || resp.Groups[0].User != "alice@example.com" {
		t.Fatalf("imap-only filter returned %+v", resp)
	}
}

func TestWhoFlatGrouping(t *testing.T) {
	wardenAddr, _ := startWardenForTest(t)
	ts := whoTestServer(t, wardenAddr)

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/who", "", map[string]any{
		"group_by": "none",
	})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp struct {
		Total    int `json:"total"`
		Sessions []struct {
			User    string `json:"user"`
			Service string `json:"service"`
		} `json:"sessions"`
	}
	decodeJSONBody(t, body, &resp)
	if len(resp.Sessions) != 2 {
		t.Fatalf("sessions=%d want 2: %+v", len(resp.Sessions), resp)
	}
}

func TestWhoCountTotal(t *testing.T) {
	wardenAddr, _ := startWardenForTest(t)
	ts := whoTestServer(t, wardenAddr)

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/who/count", "", map[string]any{})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp struct {
		Total int `json:"total"`
	}
	decodeJSONBody(t, body, &resp)
	if resp.Total != 2 {
		t.Errorf("total=%d want 2", resp.Total)
	}
}

func TestWhoCountByProtocol(t *testing.T) {
	wardenAddr, _ := startWardenForTest(t)
	ts := whoTestServer(t, wardenAddr)

	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/who/count", "", map[string]any{
		"by": "protocol",
	})
	var resp struct {
		Total      int            `json:"total"`
		ByProtocol map[string]int `json:"by_protocol"`
	}
	decodeJSONBody(t, body, &resp)
	if resp.ByProtocol["imap"] != 1 || resp.ByProtocol["pop3"] != 1 {
		t.Errorf("by_protocol=%v want imap=1 pop3=1", resp.ByProtocol)
	}
}

func TestWhoCountByUser(t *testing.T) {
	wardenAddr, _ := startWardenForTest(t)
	ts := whoTestServer(t, wardenAddr)

	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/who/count", "", map[string]any{
		"by": "user",
	})
	var resp struct {
		Total  int            `json:"total"`
		ByUser map[string]int `json:"by_user"`
	}
	decodeJSONBody(t, body, &resp)
	if resp.ByUser["alice@example.com"] != 1 || resp.ByUser["bob@example.com"] != 1 {
		t.Errorf("by_user=%v want alice=1 bob=1", resp.ByUser)
	}
}

func TestWhoCountWithServiceFilter(t *testing.T) {
	wardenAddr, _ := startWardenForTest(t)
	ts := whoTestServer(t, wardenAddr)

	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/who/count", "", map[string]any{
		"service": "imap",
	})
	var resp struct {
		Total int `json:"total"`
	}
	decodeJSONBody(t, body, &resp)
	if resp.Total != 1 {
		t.Errorf("total imap=%d want 1", resp.Total)
	}
}

func TestWhoReturns501WhenWardenNotConfigured(t *testing.T) {
	ts := whoTestServer(t, "")
	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/who", "", map[string]any{})
	if status != http.StatusNotImplemented {
		t.Errorf("status=%d want 501 when warden not wired", status)
	}
}
