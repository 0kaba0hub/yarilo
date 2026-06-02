package authclient_test

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/pkg/authclient"
)

// stubIteratorUserdb is the same shape as the master-protocol tests
// use. Reused here so the client round-trip exercises the real wire
// path on an in-process server.
type stubIteratorUserdb struct {
	users   map[string]*protocol.UserInfo
	all     []string
	lookErr error
	iterErr error
}

func (s *stubIteratorUserdb) Lookup(username string) (*protocol.UserInfo, error) {
	if s.lookErr != nil {
		return nil, s.lookErr
	}
	return s.users[username], nil
}

func (s *stubIteratorUserdb) Iterate() ([]string, error) {
	if s.iterErr != nil {
		return nil, s.iterErr
	}
	return s.all, nil
}

type nonIteratorUserdb struct{ users map[string]*protocol.UserInfo }

func (n *nonIteratorUserdb) Lookup(username string) (*protocol.UserInfo, error) {
	return n.users[username], nil
}

// spawnMaster brings up a yarilo-auth master server on a random
// local port and returns its address. The server runs until the
// test finishes via t.Cleanup.
func spawnMaster(t *testing.T, userdb protocol.Userdb) string {
	t.Helper()
	srv := protocol.NewMasterServer(userdb)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck

	// Wait briefly for the real listener to come up after the
	// probe-close above released the port.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(cancel)
	return addr
}

func dial(t *testing.T, addr string) *authclient.Client {
	t.Helper()
	c, err := authclient.Dial(addr, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestClient_DialConsumesHandshake(t *testing.T) {
	addr := spawnMaster(t, nil)
	c, err := authclient.Dial(addr, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	// Reaching this point already proves the handshake parser drained
	// VERSION/SPID/CUID/COOKIE/DONE without choking on the wire.
}

func TestClient_UserdbHit(t *testing.T) {
	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{
		"alice": {
			Username:     "alice",
			UID:          1001,
			GID:          1001,
			Home:         "/h/alice",
			MailLocation: "maildir:/m/alice",
			QuotaRules:   []string{"*:storage=5G", "Trash:storage=+1G"},
			AllowNets:    []string{"10.0.0.0/8", "192.168.0.0/16"},
			Extra:        map[string]string{"custom_tier": "gold"},
		},
	}}
	c := dial(t, spawnMaster(t, udb))

	got, err := c.Userdb(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Userdb: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil UserInfo")
	}
	if got.Username != "alice" {
		t.Errorf("Username = %q, want alice", got.Username)
	}
	if got.UID != 1001 || got.GID != 1001 {
		t.Errorf("UID/GID = %d/%d, want 1001/1001", got.UID, got.GID)
	}
	if got.Home != "/h/alice" || got.MailLocation != "maildir:/m/alice" {
		t.Errorf("Home/Mail = %q / %q", got.Home, got.MailLocation)
	}
	if !reflect.DeepEqual(got.QuotaRules, []string{"*:storage=5G", "Trash:storage=+1G"}) {
		t.Errorf("QuotaRules = %v", got.QuotaRules)
	}
	if !reflect.DeepEqual(got.AllowNets, []string{"10.0.0.0/8", "192.168.0.0/16"}) {
		t.Errorf("AllowNets = %v", got.AllowNets)
	}
	if got.Extra["custom_tier"] != "gold" {
		t.Errorf("Extra[custom_tier] = %q, want gold", got.Extra["custom_tier"])
	}
}

func TestClient_UserdbMissReturnsNilNil(t *testing.T) {
	c := dial(t, spawnMaster(t, &stubIteratorUserdb{users: map[string]*protocol.UserInfo{}}))
	got, err := c.Userdb(context.Background(), "ghost")
	if err != nil {
		t.Errorf("Userdb on miss: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for miss, got %+v", got)
	}
}

func TestClient_UserdbBackendErrorSurfacesReason(t *testing.T) {
	udb := &stubIteratorUserdb{lookErr: errors.New("db down")}
	c := dial(t, spawnMaster(t, udb))

	_, err := c.Userdb(context.Background(), "alice")
	if err == nil {
		t.Fatal("expected error from backend failure")
	}
	if msg := err.Error(); !contains(msg, "db down") {
		t.Errorf("error message %q should carry server reason 'db down'", msg)
	}
}

func TestClient_UserdbInternalFieldsNeverArriveOverWire(t *testing.T) {
	// Server-side guarantee: Password / CertName / PolicyResponse
	// are stripped by marshalUserInfo. This test confirms the
	// guarantee end-to-end — even when the backend stuffs them,
	// the client must observe nothing.
	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{
		"alice": {
			Username:       "alice",
			Password:       "should-never-reach-client",
			CertName:       "internal-cn",
			PolicyResponse: "internal-policy",
			Home:           "/h/alice",
		},
	}}
	c := dial(t, spawnMaster(t, udb))

	got, _ := c.Userdb(context.Background(), "alice")
	if got.Password != "" {
		t.Errorf("Password leaked over wire: %q", got.Password)
	}
	if got.CertName != "" {
		t.Errorf("CertName leaked over wire: %q", got.CertName)
	}
	if got.PolicyResponse != "" {
		t.Errorf("PolicyResponse leaked over wire: %q", got.PolicyResponse)
	}
}

func TestClient_PassdbReturnsErrNotImplemented(t *testing.T) {
	c := dial(t, spawnMaster(t, &stubIteratorUserdb{}))
	_, err := c.PassdbLookup(context.Background(), "alice")
	if !errors.Is(err, authclient.ErrNotImplemented) {
		t.Errorf("got %v, want ErrNotImplemented", err)
	}
}

func TestClient_IterateUsersStreaming(t *testing.T) {
	udb := &stubIteratorUserdb{all: []string{"alice", "bob", "carol"}}
	c := dial(t, spawnMaster(t, udb))

	users, err := c.IterateUsers(context.Background())
	if err != nil {
		t.Fatalf("IterateUsers: %v", err)
	}
	sort.Strings(users)
	want := []string{"alice", "bob", "carol"}
	if !reflect.DeepEqual(users, want) {
		t.Errorf("got %v, want %v", users, want)
	}
}

func TestClient_IterateUsersEmptyStream(t *testing.T) {
	c := dial(t, spawnMaster(t, &stubIteratorUserdb{all: nil}))
	users, err := c.IterateUsers(context.Background())
	if err != nil {
		t.Fatalf("IterateUsers: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("got %v, want empty", users)
	}
}

func TestClient_IterateUsersNoIteratorFails(t *testing.T) {
	c := dial(t, spawnMaster(t, &nonIteratorUserdb{users: map[string]*protocol.UserInfo{}}))
	_, err := c.IterateUsers(context.Background())
	if err == nil {
		t.Fatal("expected error when backend does not support enumeration")
	}
	if !contains(err.Error(), "does not support enumeration") {
		t.Errorf("error %q should surface the server reason", err)
	}
}

func TestClient_ConcurrentCallsSerialiseSafely(t *testing.T) {
	// Multiple goroutines hammering the same client must not
	// interleave wire bytes; the internal mutex serialises them.
	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{
		"alice": {Username: "alice", UID: 1001},
		"bob":   {Username: "bob", UID: 1002},
		"carol": {Username: "carol", UID: 1003},
	}}
	c := dial(t, spawnMaster(t, udb))

	const goroutines = 8
	const each = 20
	errCh := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			users := []string{"alice", "bob", "carol"}
			target := users[i%3]
			for j := 0; j < each; j++ {
				info, err := c.Userdb(context.Background(), target)
				if err != nil {
					errCh <- err
					return
				}
				if info == nil || info.Username != target {
					errCh <- errors.New("wrong UserInfo")
					return
				}
			}
			errCh <- nil
		}(i)
	}
	for i := 0; i < goroutines; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("goroutine error: %v", err)
		}
	}
}

func TestClient_CloseIsIdempotent(t *testing.T) {
	c := dial(t, spawnMaster(t, &stubIteratorUserdb{}))
	if err := c.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := c.Userdb(context.Background(), "alice"); !errors.Is(err, authclient.ErrClosed) {
		t.Errorf("post-close Userdb: got %v, want ErrClosed", err)
	}
}

func TestClient_ContextDeadlinePropagates(t *testing.T) {
	// Cancel the context before the call — the client must
	// surface the ctx error without writing to the conn.
	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{
		"alice": {Username: "alice"},
	}}
	c := dial(t, spawnMaster(t, udb))

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
	defer cancel()
	_, err := c.Userdb(ctx, "alice")
	if err == nil {
		t.Fatal("expected error on already-expired context")
	}
}

func TestClient_DialRejectsBadAddress(t *testing.T) {
	_, err := authclient.Dial("127.0.0.1:1", nil) // port 1 — refused
	if err == nil {
		t.Fatal("expected Dial to fail against a port that refuses connections")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
