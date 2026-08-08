package backendapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	_ "github.com/yarilomail/yarilo/pkg/dict/memory"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// stubIteratorUserdb mirrors the shape pkg/authclient's tests use —
// the backend-api lookups exercise the master protocol end-to-end,
// so the same in-process master server hangs off this stub.
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

// spawnAuthMaster brings up an in-process master listener backed by
// the supplied stub and returns a connected authclient.Client. The
// listener and client both teardown via t.Cleanup so each test
// starts with a fresh wire.
func spawnAuthMaster(t *testing.T, userdb protocol.Userdb) *authclient.Client {
	t.Helper()
	srv := protocol.NewMasterServer(userdb)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck

	// Wait for the rebind to land before dialling.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	c, err := authclient.Dial(addr, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// authTestServer is the parallel of storageTestServer in storage_test.go
// — backendapi.Server wired with everything storage_test.go expects
// PLUS an authclient.Client connected to an in-process master
// listener.
func authTestServer(t *testing.T, userdb protocol.Userdb) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	mb := maildir.New()
	idx := file.New()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("open memory dict: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	s := New(Options{
		Dicts:   map[string]dict.Dict{"metadata": d},
		Mailbox: mb,
		Index:   idx,
		Resolver: &mailbox.Resolver{
			Root:         root,
			HomeTemplate: "%d/%n",
		},
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true},
		},
		MetadataDict: d,
		AuthClient:   spawnAuthMaster(t, userdb),
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, root
}

func TestUserInfo_EnrichedWithUserdb(t *testing.T) {
	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{
		"alice@example.com": {
			Username:     "alice@example.com",
			UID:          1001,
			GID:          1001,
			Home:         "/h/alice",
			MailLocation: "maildir:/m/alice",
			Groups:       []string{"staff", "mail"},
			QuotaRules:   []string{"*:storage=5G"},
			AllowNets:    []string{"10.0.0.0/8"},
			Extra:        map[string]string{"tier": "gold"},
		},
	}}
	ts, _ := authTestServer(t, udb)

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/user/info", "",
		map[string]any{"user": "alice@example.com"})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp struct {
		Username     string `json:"username"`
		UserdbStatus string `json:"userdb_status"`
		Userdb       *struct {
			UID          uint32            `json:"uid"`
			GID          uint32            `json:"gid"`
			Home         string            `json:"home"`
			MailLocation string            `json:"mail_location"`
			Groups       []string          `json:"groups"`
			QuotaRule    []string          `json:"quota_rule"`
			AllowNets    []string          `json:"allow_nets"`
			Extra        map[string]string `json:"extra"`
		} `json:"userdb"`
	}
	decodeJSONBody(t, body, &resp)

	if resp.Username != "alice@example.com" {
		t.Errorf("username = %q", resp.Username)
	}
	if resp.Userdb == nil {
		t.Fatalf("userdb block missing")
	}
	if resp.Userdb.UID != 1001 || resp.Userdb.GID != 1001 {
		t.Errorf("UID/GID = %d/%d", resp.Userdb.UID, resp.Userdb.GID)
	}
	if resp.Userdb.MailLocation != "maildir:/m/alice" {
		t.Errorf("mail_location = %q", resp.Userdb.MailLocation)
	}
	if len(resp.Userdb.Groups) != 2 {
		t.Errorf("groups = %v", resp.Userdb.Groups)
	}
	if resp.Userdb.Extra["tier"] != "gold" {
		t.Errorf("extra[tier] = %q", resp.Userdb.Extra["tier"])
	}
}

func TestUserInfo_UserdbMissReturnsErrorBody(t *testing.T) {
	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{}}
	ts, _ := authTestServer(t, udb)

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/user/info", "",
		map[string]any{"user": "ghost@example.com"})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp map[string]any
	decodeJSONBody(t, body, &resp)
	if _, ok := resp["error"]; !ok {
		t.Errorf("expected error field in body: %s", body)
	}
}

func TestUserInfo_UserdbErrorReturns503(t *testing.T) {
	// Close the auth conn so the next lookup surfaces ErrClosed.
	// The handler must return 503.
	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{}}
	root := t.TempDir()
	mb := maildir.New()
	idx := file.New()
	d, _ := dict.Open(dict.Config{Driver: "memory"})
	t.Cleanup(func() { _ = d.Close() })

	cl := spawnAuthMaster(t, udb)
	if err := cl.Close(); err != nil {
		t.Fatalf("Close authclient: %v", err)
	}

	s := New(Options{
		Dicts:   map[string]dict.Dict{"metadata": d},
		Mailbox: mb,
		Index:   idx,
		Resolver: &mailbox.Resolver{
			Root:         root,
			HomeTemplate: "%d/%n",
		},
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true},
		},
		MetadataDict: d,
		AuthClient:   cl, // closed — every call returns ErrClosed
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/user/info", "",
		map[string]any{"user": "alice@example.com"})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", status, body)
	}
}

func TestUserInfo_NoAuthClientOmitsUserdbBlock(t *testing.T) {
	// The legacy storageTestServer wires no AuthClient — the
	// /user/info response keeps its pre-AUTH-1 shape.
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"

	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/user/info", "",
		map[string]any{"user": user})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp map[string]any
	decodeJSONBody(t, body, &resp)
	if _, ok := resp["userdb"]; ok {
		t.Errorf("userdb key present when AuthClient is nil: %+v", resp)
	}
	if _, ok := resp["userdb_status"]; ok {
		t.Errorf("userdb_status key present when AuthClient is nil")
	}
}

func TestUserIterate_HappyPath(t *testing.T) {
	udb := &stubIteratorUserdb{all: []string{"carol@example.com", "alice@example.com", "bob@example.com"}}
	ts, _ := authTestServer(t, udb)

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/user/iterate", "",
		map[string]any{})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp struct {
		Users []string `json:"users"`
	}
	decodeJSONBody(t, body, &resp)
	want := []string{"alice@example.com", "bob@example.com", "carol@example.com"}
	if len(resp.Users) != len(want) {
		t.Fatalf("got %d users, want %d: %v", len(resp.Users), len(want), resp.Users)
	}
	for i, u := range want {
		if resp.Users[i] != u {
			t.Errorf("[%d] = %q, want %q (response should be sorted)", i, resp.Users[i], u)
		}
	}
}

func TestUserIterate_MissingAuthClientReturns503(t *testing.T) {
	ts, _ := storageTestServer(t) // no AuthClient wired
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/user/iterate", "",
		map[string]any{})
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: body=%s", status, body)
	}
}

func TestUserInfo_StripsInternalFieldsThroughTheWire(t *testing.T) {
	// Defence-in-depth: even if a future backend stuffs Password
	// into UserInfo, the master-protocol wire layer strips it.
	// Confirm the backend-api response carries nothing sensitive.
	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{
		"alice@example.com": {
			Username:       "alice@example.com",
			UID:            1001,
			Password:       "must-not-leak",
			CertName:       "internal-cn",
			PolicyResponse: "internal-policy",
		},
	}}
	ts, _ := authTestServer(t, udb)

	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/user/info", "",
		map[string]any{"user": "alice@example.com"})
	for _, leak := range []string{"must-not-leak", "internal-cn", "internal-policy",
		`"password"`, `"cert_name"`, `"policy_response"`} {
		if containsByteSlice(body, leak) {
			t.Errorf("response leaks %q in body: %s", leak, body)
		}
	}
}

func containsByteSlice(b []byte, sub string) bool {
	bs := string(b)
	return len(sub) > 0 && len(bs) >= len(sub) && stringContains(bs, sub)
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
