package jmaplogin

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// wardenCall records one accounting call so a test can assert the sequence.
type wardenCall struct {
	op   string // "connect" | "disconnect"
	id   string
	user string
	ip   string
}

type fakeWarden struct {
	mu    sync.Mutex
	calls []wardenCall
	// refuse makes Connect fail, standing in for a reached limit.
	refuse bool
}

func (f *fakeWarden) Connect(id, user, ip, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, wardenCall{"connect", id, user, ip})
	if f.refuse {
		return errors.New("limit reached")
	}
	return nil
}

func (f *fakeWarden) Disconnect(id, user, ip, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, wardenCall{"disconnect", id, user, ip})
	return nil
}

func (f *fakeWarden) snapshot() []wardenCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wardenCall(nil), f.calls...)
}

func (f *fakeWarden) countOp(op string) int {
	n := 0
	for _, c := range f.snapshot() {
		if c.op == op {
			n++
		}
	}
	return n
}

type fakeAuth struct{ users map[string]string }

func (f fakeAuth) Authenticate(username, password, _, _, _ string) (string, error) {
	if pw, ok := f.users[username]; ok && pw == password {
		return username, nil
	}
	return "", errors.New("bad credentials")
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// startProxy runs the proxy against a stub backend and returns its base URL.
func startProxy(t *testing.T, opts Options) string {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the contract headers so the proxy tests can read them back.
		for _, h := range []string{hdrForwarded, hdrSessionID, hdrProxyTTL, hdrUser} {
			w.Header().Set("Echo-"+h, r.Header.Get(h))
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	if opts.Router == nil {
		opts.Router = StaticRouter{Addr: backend.Listener.Addr().String()}
	}
	if opts.Auth == nil {
		opts.Auth = fakeAuth{users: map[string]string{"u1": "pw", "u2": "pw"}}
	}

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatalf("probe close: %v", err)
	}
	opts.Addr = addr

	s := New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after cancel")
		}
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close() //nolint:errcheck
			return "http://" + addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("proxy at %s never accepted", addr)
	return ""
}

// keepAliveClient reuses one TCP connection, which is what makes the
// per-connection accounting observable.
func keepAliveClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 1, DisableKeepAlives: false},
		Timeout:   5 * time.Second,
	}
}

func get(t *testing.T, c *http.Client, base, authz string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/.well-known/jmap", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// The unit of accounting is the connection, not the request: several requests
// over one keep-alive must not multiply the count.
func TestWardenCountsTheConnectionNotTheRequest(t *testing.T) {
	wd := &fakeWarden{}
	base := startProxy(t, Options{Warden: wd})
	c := keepAliveClient()

	for i := 0; i < 3; i++ {
		resp := get(t, c, base, basic("u1", "pw"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
		resp.Body.Close() //nolint:errcheck
	}
	if n := wd.countOp("connect"); n != 1 {
		t.Errorf("3 requests on one connection produced %d Connect calls, want 1", n)
	}
}

// HTTP permits a second user on the same keep-alive. That is re-accounted, not
// refused and not counted twice, or warden's view drifts from reality.
func TestWardenReaccountsWhenTheUserChanges(t *testing.T) {
	wd := &fakeWarden{}
	base := startProxy(t, Options{Warden: wd})
	c := keepAliveClient()

	for _, u := range []string{"u1", "u2"} {
		resp := get(t, c, base, basic(u, "pw"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", u, resp.StatusCode)
		}
		resp.Body.Close() //nolint:errcheck
	}

	calls := wd.snapshot()
	var seq []string
	for _, c := range calls {
		seq = append(seq, fmt.Sprintf("%s:%s", c.op, c.user))
	}
	want := []string{"connect:u1", "disconnect:u1", "connect:u2"}
	if len(seq) < len(want) {
		t.Fatalf("got %v, want it to start with %v", seq, want)
	}
	for i, w := range want {
		if seq[i] != w {
			t.Fatalf("call %d = %s, want %s (full: %v)", i, seq[i], w, seq)
		}
	}
	// Both users must never be accounted at once.
	if wd.countOp("connect")-wd.countOp("disconnect") > 1 {
		t.Errorf("more than one user accounted at a time: %v", seq)
	}
}

// A connection that never authenticates is never accounted. Brute force on it
// is the auth penalty's problem, not a connection count's.
func TestWardenIgnoresAConnectionThatNeverAuthenticated(t *testing.T) {
	wd := &fakeWarden{}
	base := startProxy(t, Options{Warden: wd})
	c := keepAliveClient()

	for _, authz := range []string{"", basic("u1", "wrong"), basic("ghost", "pw"), "Bearer nope"} {
		resp := get(t, c, base, authz)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%q: status %d, want 401", authz, resp.StatusCode)
		}
		resp.Body.Close() //nolint:errcheck
	}
	if n := len(wd.snapshot()); n != 0 {
		t.Errorf("an unauthenticated connection produced %d warden calls: %v", n, wd.snapshot())
	}
}

// Closing the connection releases the account, or the count only ever grows.
func TestWardenReleasesOnConnectionClose(t *testing.T) {
	wd := &fakeWarden{}
	base := startProxy(t, Options{Warden: wd})
	tr := &http.Transport{}
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	resp := get(t, c, base, basic("u1", "pw"))
	resp.Body.Close() //nolint:errcheck
	tr.CloseIdleConnections()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if wd.countOp("disconnect") == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("close produced no Disconnect: %v", wd.snapshot())
}

// A refused Connect fails the request closed by default: the limit means
// something only if it is enforced.
func TestWardenRefusalIsFailClosed(t *testing.T) {
	wd := &fakeWarden{refuse: true}
	base := startProxy(t, Options{Warden: wd})
	resp := get(t, keepAliveClient(), base, basic("u1", "pw"))
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// With fail-open the same refusal lets the request through: a warden blip must
// not lock every user out.
func TestWardenRefusalCanFailOpen(t *testing.T) {
	wd := &fakeWarden{refuse: true}
	base := startProxy(t, Options{Warden: wd, WardenFailOpen: true})
	resp := get(t, keepAliveClient(), base, basic("u1", "pw"))
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
