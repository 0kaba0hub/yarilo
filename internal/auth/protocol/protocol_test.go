package protocol

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// stubPassdb is a chain-control fake: returns a preset Result +
// error, optionally mutating req.Fields beforehand. Used to drive
// Chain through every branch without standing up a real SQL store.
type stubPassdb struct {
	result Result
	err    error
	setBag func(f *Fields)
}

func (s *stubPassdb) Authenticate(req *Request) (Result, error) {
	if s.setBag != nil {
		s.setBag(req.Fields)
	}
	return s.result, s.err
}

func TestChain_FirstWinsKeepsMutations(t *testing.T) {
	ok := &stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			f.Set("user", "alice")
			f.Set("home", "/mail/alice")
		},
	}
	never := &stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			t.Errorf("second driver invoked after first returned OK")
		},
	}
	req := &Request{Username: "alice", Password: "pass", Service: "imap", Fields: NewFields()}
	got, err := Chain{ok, never}.Authenticate(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != ResultOK {
		t.Errorf("Result = %v, want ResultOK", got)
	}
	if v, _ := req.Fields.Get("home"); v != "/mail/alice" {
		t.Errorf("home = %q, want /mail/alice", v)
	}
}

func TestChain_SkipNextRollsBackPartialMutation(t *testing.T) {
	// First driver writes some fields then returns ResultNext —
	// Chain must Rollback those mutations so the second driver
	// (and any caller) sees a clean bag.
	noisy := &stubPassdb{
		result: ResultNext,
		setBag: func(f *Fields) {
			f.Set("home", "/wrong-home")
			f.Set("mail", "wrong-mail")
		},
	}
	ok := &stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			if v, _ := f.Get("home"); v != "" {
				t.Errorf("second driver saw rolled-back home=%q", v)
			}
			f.Set("user", "bob")
			f.Set("home", "/mail/bob")
		},
	}
	req := &Request{Username: "bob", Password: "pass", Service: "imap", Fields: NewFields()}
	got, _ := Chain{noisy, ok}.Authenticate(req)
	if got != ResultOK {
		t.Errorf("Result = %v, want ResultOK", got)
	}
	if v, _ := req.Fields.Get("home"); v != "/mail/bob" {
		t.Errorf("home = %q, want /mail/bob", v)
	}
}

func TestChain_AllNextReturnsFail(t *testing.T) {
	req := &Request{Username: "nobody", Password: "p", Fields: NewFields()}
	got, err := Chain{
		&stubPassdb{result: ResultNext},
		&stubPassdb{result: ResultNext},
	}.Authenticate(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != ResultFail {
		t.Errorf("Result = %v, want ResultFail (chain exhaust)", got)
	}
}

func TestChain_TempFailStopsChainAndRollsBack(t *testing.T) {
	fail := &stubPassdb{
		result: ResultTempFail,
		err:    fmt.Errorf("db connection refused"),
		setBag: func(f *Fields) {
			f.Set("home", "/should-be-rolled-back")
		},
	}
	never := &stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			t.Errorf("second driver invoked after first TempFailed")
		},
	}
	req := &Request{Username: "x", Password: "p", Fields: NewFields()}
	got, err := Chain{fail, never}.Authenticate(req)
	if got != ResultTempFail {
		t.Errorf("Result = %v, want ResultTempFail", got)
	}
	if err == nil || !strings.Contains(err.Error(), "db connection refused") {
		t.Errorf("error not propagated: %v", err)
	}
	if req.Fields.Has("home") {
		t.Errorf("TempFail driver's mutations not rolled back: %+v", req.Fields)
	}
}

func TestChain_FailStopsChainKeepsMutations(t *testing.T) {
	// Distinct from TempFail: Fail means "credentials wrong /
	// disabled" — the driver KNOWS the user, just rejects the
	// password. Mutations stay (e.g. for audit logs) and the
	// chain stops.
	fail := &stubPassdb{
		result: ResultFail,
		setBag: func(f *Fields) {
			f.Set("user", "alice")
		},
	}
	never := &stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			t.Errorf("second driver invoked after first ResultFail")
		},
	}
	req := &Request{Username: "alice", Password: "wrong", Fields: NewFields()}
	got, _ := Chain{fail, never}.Authenticate(req)
	if got != ResultFail {
		t.Errorf("Result = %v, want ResultFail", got)
	}
	if v, _ := req.Fields.Get("user"); v != "alice" {
		t.Errorf("ResultFail driver's mutations dropped: user=%q", v)
	}
}

func TestChain_Empty(t *testing.T) {
	req := &Request{Username: "x", Fields: NewFields()}
	got, err := Chain{}.Authenticate(req)
	if err != nil || got != ResultFail {
		t.Fatalf("empty chain should return ResultFail, got %v err=%v", got, err)
	}
}

func TestChain_AllocatesFieldsWhenNil(t *testing.T) {
	// Defensive: a caller that forgets to pre-allocate Fields
	// gets a working bag rather than a nil-pointer panic.
	mutator := &stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			if f == nil {
				t.Error("driver saw nil Fields")
				return
			}
			f.Set("user", "alice")
		},
	}
	req := &Request{Username: "alice"}
	got, _ := Chain{mutator}.Authenticate(req)
	if got != ResultOK {
		t.Errorf("Result = %v, want ResultOK", got)
	}
	if req.Fields == nil {
		t.Fatal("Chain did not allocate Fields")
	}
	if v, _ := req.Fields.Get("user"); v != "alice" {
		t.Errorf("user = %q, want alice", v)
	}
}

func TestNewAuthenticator_WrapsChainIntoLegacyShape(t *testing.T) {
	// The Authenticator wrapper is what session-side callers
	// (IMAP/POP3/Submission) use — projects the chain-internal
	// (Result, error) onto the wire-shaped (*AuthResponse, error).
	a := NewAuthenticator([]Passdb{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			f.Set("user", "carol")
			f.Set("home", "/mail/carol")
			f.Set("mail", "maildir:/m/carol")
		},
	}})
	resp, err := a.Authenticate("carol", "pw", "imap")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result != AuthOK {
		t.Errorf("Result = %v, want AuthOK", resp.Result)
	}
	if resp.Username != "carol" {
		t.Errorf("Username = %q, want carol", resp.Username)
	}
	if resp.Home != "/mail/carol" {
		t.Errorf("Home = %q, want /mail/carol", resp.Home)
	}
	if resp.MailLoc != "maildir:/m/carol" {
		t.Errorf("MailLoc = %q, want maildir:/m/carol", resp.MailLoc)
	}
	if resp.Fields == nil {
		t.Fatal("Fields bag dropped on Authenticator projection")
	}
}

// simpleUserdb is a minimal Userdb fake for RunAuth tests. Returns
// (info, nil) when the username matches, (nil, nil) for miss, or
// (nil, err) when err is set.
type simpleUserdb struct {
	user string
	info *UserInfo
	err  error
}

func (s *simpleUserdb) Lookup(username string) (*UserInfo, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.user != "" && username != s.user {
		return nil, nil
	}
	return s.info, nil
}

func TestRunAuth_PassdbOnlyWhenUserdbNil(t *testing.T) {
	chain := Chain{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			f.Set("user", "alice")
			f.Set("home", "/h/alice")
		},
	}}
	req := &Request{Username: "alice", Password: "p", Fields: NewFields()}
	got, err := RunAuth(chain, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	if got != ResultOK {
		t.Errorf("Result = %v, want ResultOK", got)
	}
	if v, _ := req.Fields.Get("home"); v != "/h/alice" {
		t.Errorf("home = %q", v)
	}
	if req.Fields.Has("userdb_uid") {
		t.Error("userdb_uid set without userdb configured")
	}
}

func TestRunAuth_UserdbEnrichesAfterPassdbOK(t *testing.T) {
	chain := Chain{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			f.Set("user", "alice")
			f.Set("home", "/h/alice")
		},
	}}
	udb := &simpleUserdb{
		user: "alice",
		info: &UserInfo{
			Username: "alice",
			UID:      1001,
			GID:      1001,
			Home:     "/h/alice",
			Groups:   []string{"staff", "mail"},
		},
	}
	req := &Request{Username: "alice", Password: "p", Fields: NewFields()}
	got, _ := RunAuth(chain, udb, req)
	if got != ResultOK {
		t.Errorf("Result = %v, want ResultOK", got)
	}
	if v, _ := req.Fields.Get("userdb_uid"); v != "1001" {
		t.Errorf("userdb_uid = %q, want 1001", v)
	}
	if v, _ := req.Fields.Get("userdb_gid"); v != "1001" {
		t.Errorf("userdb_gid = %q, want 1001", v)
	}
	if v, _ := req.Fields.Get("userdb_groups"); v != "staff,mail" {
		t.Errorf("userdb_groups = %q, want staff,mail", v)
	}
	// Passdb's home remains untouched (no userdb_ prefix).
	if v, _ := req.Fields.Get("home"); v != "/h/alice" {
		t.Errorf("home = %q (passdb field must survive)", v)
	}
}

func TestRunAuth_PrefetchSkipsUserdbLookup(t *testing.T) {
	// Passdb already SELECT-ed userdb columns and set userdb_*
	// fields itself — RunAuth must NOT invoke userdb.Lookup.
	udb := &simpleUserdb{
		user: "alice",
		info: &UserInfo{Username: "alice", UID: 9999}, // would overwrite if called
	}
	udbCalled := false
	wrappedUdb := userdbHook{wrapped: udb, onLookup: func() { udbCalled = true }}

	chain := Chain{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			f.Set("user", "alice")
			f.Set("userdb_uid", "1001") // prefetch marker
			f.Set("userdb_home", "/h/prefetched")
		},
	}}
	req := &Request{Username: "alice", Password: "p", Fields: NewFields()}
	_, _ = RunAuth(chain, wrappedUdb, req)

	if udbCalled {
		t.Error("userdb.Lookup invoked despite passdb prefetch")
	}
	if v, _ := req.Fields.Get("userdb_uid"); v != "1001" {
		t.Errorf("prefetched userdb_uid overwritten: %q", v)
	}
}

func TestRunAuth_UserdbErrorDoesNotDowngrade(t *testing.T) {
	chain := Chain{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) { f.Set("user", "alice") },
	}}
	udb := &simpleUserdb{err: fmt.Errorf("userdb backend down")}
	req := &Request{Username: "alice", Password: "p", Fields: NewFields()}
	got, _ := RunAuth(chain, udb, req)
	if got != ResultOK {
		t.Errorf("userdb error should not downgrade passdb OK; got %v", got)
	}
	if req.Fields.Has("userdb_uid") {
		t.Error("userdb error should not leave partial userdb_* fields")
	}
}

func TestRunAuth_UserdbMissDoesNotDowngrade(t *testing.T) {
	chain := Chain{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) { f.Set("user", "alice") },
	}}
	udb := &simpleUserdb{user: "bob"} // alice unknown to this userdb
	req := &Request{Username: "alice", Password: "p", Fields: NewFields()}
	got, _ := RunAuth(chain, udb, req)
	if got != ResultOK {
		t.Errorf("userdb miss should not downgrade passdb OK; got %v", got)
	}
}

func TestRunAuth_FailSkipsUserdb(t *testing.T) {
	chain := Chain{&stubPassdb{result: ResultFail}}
	udbCalled := false
	udb := userdbHook{onLookup: func() { udbCalled = true }}
	req := &Request{Username: "alice", Password: "wrong", Fields: NewFields()}
	got, _ := RunAuth(chain, udb, req)
	if got != ResultFail {
		t.Errorf("Result = %v, want ResultFail", got)
	}
	if udbCalled {
		t.Error("userdb.Lookup invoked despite ResultFail")
	}
}

// userdbHook wraps a Userdb to record whether Lookup was called.
type userdbHook struct {
	wrapped  Userdb
	onLookup func()
}

func (u userdbHook) Lookup(username string) (*UserInfo, error) {
	if u.onLookup != nil {
		u.onLookup()
	}
	if u.wrapped != nil {
		return u.wrapped.Lookup(username)
	}
	return nil, nil
}

// credPassdb accepts only a fixed username+password pair. Migrated
// to the new Passdb interface for use by server-side tests.
type credPassdb struct{ user, pass string }

func (c *credPassdb) Authenticate(req *Request) (Result, error) {
	if req.Username == c.user && req.Password == c.pass {
		req.Fields.Set("user", req.Username)
		req.Fields.Set("home", "/mail/"+req.Username)
		return ResultOK, nil
	}
	return ResultNext, nil
}

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

// readHandshake drains server handshake lines until "DONE" and returns all
// lines as a single string. Uses a bufio.Scanner so partial TCP reads are
// handled correctly.
func readHandshake(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	var sb strings.Builder
	for sc.Scan() {
		line := sc.Text()
		sb.WriteString(line)
		sb.WriteByte('\n')
		if line == "DONE" {
			break
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner error reading handshake: %v", err)
	}
	return sb.String()
}

func TestListenAndServe_Handshake(t *testing.T) {
	srv := NewServer(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))

	sc := bufio.NewScanner(conn)
	hs := readHandshake(t, sc)
	if !strings.Contains(hs, "VERSION\tyarilo-auth") {
		t.Errorf("missing VERSION line in handshake: %q", hs)
	}
	if !strings.Contains(hs, "DONE") {
		t.Errorf("missing DONE line in handshake: %q", hs)
	}
}

func TestListenAndServe_AuthOK(t *testing.T) {
	srv := NewServer([]Passdb{&credPassdb{"alice", "secret"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))

	sc := bufio.NewScanner(conn)
	readHandshake(t, sc)

	fmt.Fprintf(conn, "AUTH\t1\tPLAIN\tservice=imap\tresp=alice\x00alice\x00secret\n")

	if !sc.Scan() {
		t.Fatalf("no auth response line: %v", sc.Err())
	}
	resp := sc.Text()
	if !strings.HasPrefix(resp, "OK\t1\t") {
		t.Errorf("expected OK response, got: %q", resp)
	}
	if !strings.Contains(resp, "user=alice") {
		t.Errorf("expected user=alice in response, got: %q", resp)
	}
}

func TestListenAndServe_AuthFail(t *testing.T) {
	srv := NewServer([]Passdb{&credPassdb{"alice", "secret"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))

	sc := bufio.NewScanner(conn)
	readHandshake(t, sc)

	fmt.Fprintf(conn, "AUTH\t2\tPLAIN\tservice=imap\tresp=alice\x00alice\x00wrongpass\n")

	if !sc.Scan() {
		t.Fatalf("no auth response line: %v", sc.Err())
	}
	resp := sc.Text()
	if !strings.HasPrefix(resp, "FAIL\t2") {
		t.Errorf("expected FAIL response, got: %q", resp)
	}
}

func TestListenAndServe_GracefulShutdown(t *testing.T) {
	srv := NewServer(nil)
	ctx, cancel := context.WithCancel(context.Background())

	addr := freeAddr(t)
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, addr, nil) }()
	time.Sleep(20 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ListenAndServe returned error after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("ListenAndServe did not return after context cancel")
	}
}
