package protocol

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// TestParsePlain_AllShapes covers every PLAIN/LOGIN encoding the
// server is expected to accept and every malformed input it must
// reject.
func TestParsePlain_AllShapes(t *testing.T) {
	tests := []struct {
		name       string
		mech       string
		resp       string
		wantAuthz  string
		wantAuthid string
		wantPass   string
		wantOK     bool
	}{
		{
			name:       "PLAIN three-field with authzid",
			mech:       "PLAIN",
			resp:       "target\x00master\x00pass",
			wantAuthz:  "target",
			wantAuthid: "master",
			wantPass:   "pass",
			wantOK:     true,
		},
		{
			name:       "PLAIN three-field with empty authzid",
			mech:       "PLAIN",
			resp:       "\x00alice\x00secret",
			wantAuthz:  "",
			wantAuthid: "alice",
			wantPass:   "secret",
			wantOK:     true,
		},
		{
			name:       "PLAIN two-field (legacy clients omit authzid)",
			mech:       "PLAIN",
			resp:       "alice\x00secret",
			wantAuthz:  "",
			wantAuthid: "alice",
			wantPass:   "secret",
			wantOK:     true,
		},
		{
			name:       "LOGIN two-field",
			mech:       "LOGIN",
			resp:       "alice\x00secret",
			wantAuthz:  "",
			wantAuthid: "alice",
			wantPass:   "secret",
			wantOK:     true,
		},
		{
			name:       "PLAIN with empty password (still well-formed)",
			mech:       "PLAIN",
			resp:       "\x00alice\x00",
			wantAuthz:  "",
			wantAuthid: "alice",
			wantPass:   "",
			wantOK:     true,
		},
		{
			name:       "PLAIN password contains NUL (SplitN keeps tail)",
			mech:       "PLAIN",
			resp:       "\x00alice\x00pa\x00ss",
			wantAuthz:  "",
			wantAuthid: "alice",
			wantPass:   "pa\x00ss",
			wantOK:     true,
		},
		{
			name:   "single field rejected",
			mech:   "PLAIN",
			resp:   "alice",
			wantOK: false,
		},
		{
			name:   "empty response rejected",
			mech:   "PLAIN",
			resp:   "",
			wantOK: false,
		},
		{
			name:   "unknown mechanism rejected",
			mech:   "CRAM-MD5",
			resp:   "alice\x00secret",
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authz, authid, pass, ok := parsePlain(tc.mech, tc.resp)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if authz != tc.wantAuthz || authid != tc.wantAuthid || pass != tc.wantPass {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)",
					authz, authid, pass, tc.wantAuthz, tc.wantAuthid, tc.wantPass)
			}
		})
	}
}

// TestSplitMasterFromAuthid covers Dovecot's separator workaround
// for clients that cannot supply an authzid: the third PLAIN field
// is absent and the master/target identities are packed into the
// authid as `target<sep>master`.
func TestSplitMasterFromAuthid(t *testing.T) {
	tests := []struct {
		name       string
		authid     string
		sep        string
		wantMaster string
		wantTarget string
	}{
		{
			name:       "default star separator",
			authid:     "alice*admin",
			sep:        "*",
			wantMaster: "admin",
			wantTarget: "alice",
		},
		{
			name:       "multi-byte separator",
			authid:     "alice::admin",
			sep:        "::",
			wantMaster: "admin",
			wantTarget: "alice",
		},
		{
			name:       "target contains @ (mail address)",
			authid:     "alice@example.com*admin",
			sep:        "*",
			wantMaster: "admin",
			wantTarget: "alice@example.com",
		},
		{
			name:       "empty separator disables splitting",
			authid:     "alice*admin",
			sep:        "",
			wantMaster: "alice*admin",
			wantTarget: "",
		},
		{
			name:       "separator absent — authid passes through unchanged",
			authid:     "alice",
			sep:        "*",
			wantMaster: "alice",
			wantTarget: "",
		},
		{
			name:       "bare separator with empty target — no split",
			authid:     "*admin",
			sep:        "*",
			wantMaster: "*admin",
			wantTarget: "",
		},
		{
			name:       "bare separator with empty master — no split",
			authid:     "alice*",
			sep:        "*",
			wantMaster: "alice*",
			wantTarget: "",
		},
		{
			name:       "multiple separators — first wins (Dovecot behaviour)",
			authid:     "alice*ad*min",
			sep:        "*",
			wantMaster: "ad*min",
			wantTarget: "alice",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotMaster, gotTarget := SplitMasterFromAuthid(tc.authid, tc.sep)
			if gotMaster != tc.wantMaster || gotTarget != tc.wantTarget {
				t.Errorf("(%q,%q), want (%q,%q)",
					gotMaster, gotTarget, tc.wantMaster, tc.wantTarget)
			}
		})
	}
}

// --- RunMasterAuth core test helpers -----------------------------------

// targetUserdb records the username it was asked to look up and
// returns a preset UserInfo. Used to confirm RunMasterAuth runs
// userdb against TARGET, not against the master.
type targetUserdb struct {
	wantUser string
	got      string
	ret      *UserInfo
	err      error
}

func (u *targetUserdb) Lookup(username string) (*UserInfo, error) {
	u.got = username
	return u.ret, u.err
}

// TestRunMasterAuth_MasterdbHitGrantsImpersonation drives the
// happy path through the dedicated masterdb chain.
func TestRunMasterAuth_MasterdbHitGrantsImpersonation(t *testing.T) {
	masterdb := Chain{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			// masterdb may carry its own metadata — e.g. an
			// administrative group; this MUST be visible on the
			// final response so callers can audit the master.
			f.Set("master_admin_group", "ops")
		},
	}}
	passdb := Chain{} // not consulted on masterdb hit
	udb := &targetUserdb{
		wantUser: "alice",
		ret: &UserInfo{
			Username: "alice",
			Home:     "/mail/alice",
		},
	}
	req := &Request{Username: "admin", Password: "p", Service: "imap", Fields: NewFields()}

	res, err := RunMasterAuth(passdb, masterdb, udb, "alice", req)
	if err != nil {
		t.Fatal(err)
	}
	if res != ResultOK {
		t.Fatalf("result = %v, want ResultOK", res)
	}
	if req.Username != "alice" {
		t.Errorf("req.Username = %q, want alice (switched to target)", req.Username)
	}
	if udb.got != "alice" {
		t.Errorf("userdb looked up %q, want alice", udb.got)
	}
	wantSet := map[string]string{
		"master_user":        "admin",
		"original_user":      "alice",
		"login_user":         "alice",
		"user":               "alice",
		"master_admin_group": "ops",
		// VisitFields emits only typed `extras` — Username is the
		// lookup key, not echoed as userdb_user. `user` (above)
		// carries the target identity for the wire reply.
		"userdb_home": "/mail/alice",
	}
	for k, v := range wantSet {
		got, ok := req.Fields.Get(k)
		if !ok {
			t.Errorf("missing field %q", k)
			continue
		}
		if got != v {
			t.Errorf("field %q = %q, want %q", k, got, v)
		}
	}
}

// TestRunMasterAuth_MasterdbFailRejectsImpersonation — when the
// masterdb authoritatively rejects the password it MUST NOT fall
// through to the passdb master_user flag. Allowing fall-through
// would let any user impersonate by guessing the per-user
// master_user flag set on their own account.
func TestRunMasterAuth_MasterdbFailRejectsImpersonation(t *testing.T) {
	masterdb := Chain{&stubPassdb{result: ResultFail}}
	passdb := Chain{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) { f.Set("master_user", "yes") },
	}}
	udb := &targetUserdb{}
	req := &Request{Username: "admin", Password: "wrong", Fields: NewFields()}

	res, err := RunMasterAuth(passdb, masterdb, udb, "alice", req)
	if err != nil {
		t.Fatal(err)
	}
	if res != ResultFail {
		t.Errorf("result = %v, want ResultFail", res)
	}
	if udb.got != "" {
		t.Errorf("userdb consulted (%q) on master failure", udb.got)
	}
	if req.Username != "admin" {
		t.Errorf("req.Username = %q, want admin (no switch on fail)", req.Username)
	}
	if v, ok := req.Fields.Get("master_user"); ok {
		t.Errorf("master_user leaked to bag on fail: %q", v)
	}
}

// TestRunMasterAuth_MasterdbNextFallsThroughToPassdbFlag — when
// the masterdb does not know the user, the main passdb is
// consulted; impersonation is granted only when its result carries
// master_user=yes.
func TestRunMasterAuth_MasterdbNextFallsThroughToPassdbFlag(t *testing.T) {
	masterdb := Chain{&stubPassdb{result: ResultNext}}
	passdb := Chain{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			f.Set("master_user", "yes")
			f.Set("admin_origin", "ldap")
		},
	}}
	udb := &targetUserdb{wantUser: "alice"}
	req := &Request{Username: "admin", Password: "p", Fields: NewFields()}

	res, _ := RunMasterAuth(passdb, masterdb, udb, "alice", req)
	if res != ResultOK {
		t.Fatalf("result = %v, want ResultOK", res)
	}
	if req.Username != "alice" {
		t.Errorf("req.Username = %q, want alice", req.Username)
	}
	if v, _ := req.Fields.Get("master_user"); v != "admin" {
		t.Errorf("master_user = %q, want admin (master's name)", v)
	}
	if v, _ := req.Fields.Get("admin_origin"); v != "ldap" {
		t.Errorf("passdb mutations dropped after master_user gate: admin_origin = %q", v)
	}
}

// TestRunMasterAuth_NoMasterdbConfigured — when masterdb is empty
// the master_user=yes flag in the main passdb is the sole gate.
func TestRunMasterAuth_NoMasterdbConfigured(t *testing.T) {
	passdb := Chain{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) { f.Set("master_user", "yes") },
	}}
	udb := &targetUserdb{}
	req := &Request{Username: "admin", Password: "p", Fields: NewFields()}

	res, _ := RunMasterAuth(passdb, nil, udb, "alice", req)
	if res != ResultOK {
		t.Fatalf("result = %v, want ResultOK", res)
	}
	if req.Username != "alice" {
		t.Errorf("req.Username = %q, want alice", req.Username)
	}
}

// TestRunMasterAuth_PassdbFlagFalseRejects — without master_user=yes
// the user is just a regular user, not a master.
func TestRunMasterAuth_PassdbFlagFalseRejects(t *testing.T) {
	passdb := Chain{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			f.Set("home", "/mail/admin")
			f.Set("master_user", "no")
		},
	}}
	udb := &targetUserdb{}
	req := &Request{Username: "admin", Password: "p", Fields: NewFields()}

	res, _ := RunMasterAuth(passdb, nil, udb, "alice", req)
	if res != ResultFail {
		t.Fatalf("result = %v, want ResultFail (no master flag)", res)
	}
	// Passdb mutations must be rolled back so a failed impersonation
	// does not leak the master's own profile fields onto the wire.
	if req.Fields.Len() != 0 {
		t.Errorf("bag dirty after failed impersonation: %v", req.Fields)
	}
	if udb.got != "" {
		t.Errorf("userdb consulted on failed impersonation")
	}
}

// TestRunMasterAuth_PassdbFlagAbsentRejects — passdb authenticates
// the user but does not declare them a master. Same outcome as
// master_user=no.
func TestRunMasterAuth_PassdbFlagAbsentRejects(t *testing.T) {
	passdb := Chain{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) { f.Set("home", "/mail/admin") },
	}}
	req := &Request{Username: "admin", Password: "p", Fields: NewFields()}
	res, _ := RunMasterAuth(passdb, nil, nil, "alice", req)
	if res != ResultFail {
		t.Errorf("result = %v, want ResultFail (no master_user field at all)", res)
	}
	if req.Fields.Len() != 0 {
		t.Errorf("bag dirty after fail: %v", req.Fields)
	}
}

// TestRunMasterAuth_PassdbNextRejects — master is unknown in both
// the masterdb chain and the main passdb chain.
func TestRunMasterAuth_PassdbNextRejects(t *testing.T) {
	passdb := Chain{&stubPassdb{result: ResultNext}}
	req := &Request{Username: "ghost", Password: "p", Fields: NewFields()}
	res, _ := RunMasterAuth(passdb, nil, nil, "alice", req)
	if res != ResultFail {
		t.Errorf("result = %v, want ResultFail", res)
	}
	if req.Fields.Len() != 0 {
		t.Errorf("bag dirty after fail: %v", req.Fields)
	}
}

// TestRunMasterAuth_MasterdbErrorPropagatesAsTempFail — backend
// errors during the masterdb pass must surface as ResultTempFail
// (not Fail) so anvil / penalty / cache can distinguish a real
// outage from a credential mismatch.
func TestRunMasterAuth_MasterdbErrorPropagatesAsTempFail(t *testing.T) {
	wantErr := errors.New("ldap unreachable")
	masterdb := Chain{&stubPassdb{result: ResultTempFail, err: wantErr}}
	passdb := Chain{} // never reached
	req := &Request{Username: "admin", Password: "p", Fields: NewFields()}

	res, err := RunMasterAuth(passdb, masterdb, nil, "alice", req)
	if res != ResultTempFail {
		t.Errorf("result = %v, want ResultTempFail", res)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrapping %v", err, wantErr)
	}
	if req.Fields.Len() != 0 {
		t.Errorf("bag dirty after temp_fail: %v", req.Fields)
	}
}

// TestRunMasterAuth_PassdbErrorPropagatesAsTempFail — same for the
// fall-through passdb chain.
func TestRunMasterAuth_PassdbErrorPropagatesAsTempFail(t *testing.T) {
	wantErr := errors.New("sql connect refused")
	masterdb := Chain{&stubPassdb{result: ResultNext}}
	passdb := Chain{&stubPassdb{result: ResultTempFail, err: wantErr}}
	req := &Request{Username: "admin", Password: "p", Fields: NewFields()}

	res, err := RunMasterAuth(passdb, masterdb, nil, "alice", req)
	if res != ResultTempFail {
		t.Errorf("result = %v, want ResultTempFail", res)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrapping %v", err, wantErr)
	}
}

// TestRunMasterAuth_UserdbMissDoesNotDowngrade — userdb hole for
// the TARGET must not flip a successful master auth to fail. The
// caller (login pod) decides what to do with a master-authenticated
// but unknown target.
func TestRunMasterAuth_UserdbMissDoesNotDowngrade(t *testing.T) {
	masterdb := Chain{&stubPassdb{result: ResultOK}}
	udb := &targetUserdb{ret: nil} // miss
	req := &Request{Username: "admin", Password: "p", Fields: NewFields()}

	res, err := RunMasterAuth(nil, masterdb, udb, "alice", req)
	if err != nil {
		t.Fatal(err)
	}
	if res != ResultOK {
		t.Errorf("result = %v, want ResultOK despite userdb miss", res)
	}
	if v, _ := req.Fields.Get("master_user"); v != "admin" {
		t.Errorf("master_user = %q, want admin", v)
	}
}

// TestRunMasterAuth_UserdbErrorDoesNotDowngrade — same for userdb
// backend errors.
func TestRunMasterAuth_UserdbErrorDoesNotDowngrade(t *testing.T) {
	masterdb := Chain{&stubPassdb{result: ResultOK}}
	udb := &targetUserdb{err: errors.New("ldap timeout")}
	req := &Request{Username: "admin", Password: "p", Fields: NewFields()}

	res, _ := RunMasterAuth(nil, masterdb, udb, "alice", req)
	if res != ResultOK {
		t.Errorf("result = %v, want ResultOK despite userdb error", res)
	}
}

// TestRunMasterAuth_PrefetchSkipsUserdbLookup — masterdb that
// pre-loads userdb_* fields for the target should suppress the
// userdb call entirely. Matches userdb-prefetch semantics from
// the regular auth path.
func TestRunMasterAuth_PrefetchSkipsUserdbLookup(t *testing.T) {
	masterdb := Chain{&stubPassdb{
		result: ResultOK,
		setBag: func(f *Fields) {
			f.Set("userdb_user", "alice")
			f.Set("userdb_home", "/mail/alice")
			f.Set("userdb_uid", "1001")
		},
	}}
	udb := &targetUserdb{}
	req := &Request{Username: "admin", Password: "p", Fields: NewFields()}

	res, _ := RunMasterAuth(nil, masterdb, udb, "alice", req)
	if res != ResultOK {
		t.Fatal("expected OK")
	}
	if udb.got != "" {
		t.Errorf("userdb consulted (%q) despite prefetch", udb.got)
	}
}

// TestRunMasterAuth_PassdbFailIsAuthoritative — when the dedicated
// masterdb is empty and the passdb chain returns ResultFail (master
// is known, password is wrong), the call MUST reject without
// falling further. There is nowhere left to fall to anyway, but
// the test pins the contract.
func TestRunMasterAuth_PassdbFailIsAuthoritative(t *testing.T) {
	passdb := Chain{&stubPassdb{result: ResultFail}}
	req := &Request{Username: "admin", Password: "wrong", Fields: NewFields()}
	res, _ := RunMasterAuth(passdb, nil, nil, "alice", req)
	if res != ResultFail {
		t.Errorf("result = %v, want ResultFail", res)
	}
	if req.Fields.Len() != 0 {
		t.Errorf("bag dirty after fail: %v", req.Fields)
	}
}

// TestRunMasterAuth_MasterFieldsNeverLeakOnFail — the spec requires
// that a failed impersonation produces a response indistinguishable
// from a regular failed login at the wire level. The bag is the
// thing that drives the wire reply, so it must be empty.
func TestRunMasterAuth_MasterFieldsNeverLeakOnFail(t *testing.T) {
	// Driver pretends to authenticate the master with a rich field
	// set, then says ResultFail.
	bad := &stubPassdb{
		result: ResultFail,
		setBag: func(f *Fields) {
			f.Set("home", "/mail/admin")
			f.Set("uid", "1000")
			f.Set("admin_secret", "leaked-on-fail-is-a-bug")
		},
	}
	req := &Request{Username: "admin", Password: "wrong", Fields: NewFields()}
	res, _ := RunMasterAuth(Chain{bad}, nil, nil, "alice", req)
	if res != ResultFail {
		t.Fatalf("want ResultFail")
	}
	req.Fields.Each(func(k, v string) bool {
		t.Errorf("field leaked on fail: %s=%s", k, v)
		return true
	})
}

// --- wire-level tests ---------------------------------------------------

// masterCredPassdb mirrors credPassdb but sets `master_user=yes`
// on its bag — i.e. this user is annotated as a master in the
// passdb. Used for the wire-level master flow without a separate
// masterdb chain.
type masterCredPassdb struct{ user, pass string }

func (c *masterCredPassdb) Authenticate(req *Request) (Result, error) {
	if req.Username == c.user && req.Password == c.pass {
		req.Fields.Set("user", req.Username)
		req.Fields.Set("master_user", "yes")
		return ResultOK, nil
	}
	return ResultNext, nil
}

// targetUserdbForWire injects a known UserInfo for the target so
// the OK wire reply carries userdb_* tokens we can grep.
type targetUserdbForWire struct{}

func (targetUserdbForWire) Lookup(username string) (*UserInfo, error) {
	if username == "alice" {
		return &UserInfo{Username: "alice", Home: "/mail/alice"}, nil
	}
	return nil, nil
}

func dialAndHandshake(t *testing.T, addr string) (net.Conn, *bufio.Scanner) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	sc := bufio.NewScanner(conn)
	readHandshake(t, sc)
	return conn, sc
}

// TestWire_MasterUser_Authzid drives the standards-compliant
// SASL PLAIN encoding: `target\0master\0password`. The OK reply
// must carry user=target plus `master_user=master` (Dovecot
// echoes the master's identity on the wire for audit).
func TestWire_MasterUser_Authzid(t *testing.T) {
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "userpass"}}, // regular passdb knows alice (target userdb backstop)
		WithMasterUsers(true),
		WithMasterdb([]Passdb{&credPassdb{"admin", "masterpass"}}),
		WithUserdb(targetUserdbForWire{}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	fmt.Fprintf(conn, "AUTH\t9\tPLAIN\tservice=imap\tresp=alice\x00admin\x00masterpass\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	got := sc.Text()
	if !strings.HasPrefix(got, "OK\t9\tuser=alice") {
		t.Errorf("reply does not impersonate target: %q", got)
	}
	if !strings.Contains(got, "master_user=admin") {
		t.Errorf("master_user= missing or wrong on OK reply: %q", got)
	}
	if !strings.Contains(got, "userdb_home=/mail/alice") {
		t.Errorf("userdb fields for target missing: %q", got)
	}
}

// TestWire_MasterUser_Separator drives Dovecot's workaround for
// clients that cannot supply authzid: the SASL response is
// `\0alice*admin\0masterpass`. WithMasterUserSeparator("*") splits
// it into (target=alice, master=admin) inside handleAuth.
func TestWire_MasterUser_Separator(t *testing.T) {
	srv := NewServer(
		nil,
		WithMasterUsers(true),
		WithMasterdb([]Passdb{&credPassdb{"admin", "masterpass"}}),
		WithMasterUserSeparator("*"),
		WithUserdb(targetUserdbForWire{}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	fmt.Fprintf(conn, "AUTH\t11\tPLAIN\tservice=imap\tresp=\x00alice*admin\x00masterpass\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	got := sc.Text()
	if !strings.HasPrefix(got, "OK\t11\tuser=alice") {
		t.Errorf("separator workaround did not impersonate target: %q", got)
	}
	if !strings.Contains(got, "master_user=admin") {
		t.Errorf("master_user= missing on OK reply: %q", got)
	}
}

// TestWire_MasterUser_PassdbFlagPath — no dedicated masterdb;
// the master is authenticated by the main passdb which annotates
// the row with `master_user=yes`. RunMasterAuth recognises the
// flag and grants impersonation.
func TestWire_MasterUser_PassdbFlagPath(t *testing.T) {
	srv := NewServer(
		[]Passdb{&masterCredPassdb{"admin", "masterpass"}},
		WithMasterUsers(true),
		WithUserdb(targetUserdbForWire{}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	fmt.Fprintf(conn, "AUTH\t14\tPLAIN\tservice=imap\tresp=alice\x00admin\x00masterpass\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	got := sc.Text()
	if !strings.HasPrefix(got, "OK\t14\tuser=alice") {
		t.Errorf("passdb-flag path did not impersonate target: %q", got)
	}
	if !strings.Contains(got, "master_user=admin") {
		t.Errorf("master_user= missing on OK reply: %q", got)
	}
}

// TestWire_MasterUser_FailIsOpaque pins the security contract: a
// failed master auth must look identical on the wire to a regular
// failed login. No reason= token, no master_user trace, no field
// echoes from the master's row.
func TestWire_MasterUser_FailIsOpaque(t *testing.T) {
	srv := NewServer(
		[]Passdb{&masterCredPassdb{"admin", "masterpass"}},
		WithMasterUsers(true),
		WithMasterdb([]Passdb{&credPassdb{"admin", "masterpass"}}),
		WithUserdb(targetUserdbForWire{}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	// Wrong master password.
	fmt.Fprintf(conn, "AUTH\t21\tPLAIN\tservice=imap\tresp=alice\x00admin\x00WRONG\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	got := sc.Text()
	if got != "FAIL\t21" {
		t.Errorf("FAIL reply leaks detail: %q (must be exactly 'FAIL\\t21')", got)
	}
}

// TestWire_NoImpersonation_AuthzidEqualsAuthid — RFC 4616 says
// authzid may be supplied even when it equals authid (it's a
// no-op in that case). The server must treat this as a regular
// login, not a master flow.
func TestWire_NoImpersonation_AuthzidEqualsAuthid(t *testing.T) {
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "secret"}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	fmt.Fprintf(conn, "AUTH\t30\tPLAIN\tservice=imap\tresp=alice\x00alice\x00secret\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	got := sc.Text()
	if !strings.HasPrefix(got, "OK\t30\tuser=alice") {
		t.Errorf("authzid==authid should behave as regular login: %q", got)
	}
	if strings.Contains(got, "master_user=") {
		t.Errorf("regular login wrote master_user=: %q", got)
	}
}

// TestWire_MasterUser_DisabledByDefault — opt-in security check.
// Server is constructed WITHOUT WithMasterUsers(true) but with
// a fully-populated masterdb + separator. A SASL PLAIN response
// carrying a distinct authzid MUST be ignored: the server routes
// the request as a plain login of AUTHID. The masterdb password
// is admin's, but the regular passdb expects alice's — so the
// auth fails (admin is unknown to the regular passdb).
func TestWire_MasterUser_DisabledByDefault(t *testing.T) {
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "alicepass"}},
		// Note: NO WithMasterUsers(true) — default is disabled.
		WithMasterdb([]Passdb{&credPassdb{"admin", "masterpass"}}),
		WithMasterUserSeparator("*"),
		WithUserdb(targetUserdbForWire{}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	// authzid=alice authid=admin password=masterpass. Master flow
	// would normally succeed (masterdb authenticates admin, target
	// alice on userdb backstop). Because master-users are disabled
	// the authzid is dropped and the server tries to authenticate
	// admin via the regular passdb, which only knows alice.
	fmt.Fprintf(conn, "AUTH\t40\tPLAIN\tservice=imap\tresp=alice\x00admin\x00masterpass\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	got := sc.Text()
	if got != "FAIL\t40" {
		t.Errorf("master flow leaked while disabled: %q (want FAIL\\t40)", got)
	}
}

// TestAuthenticator_MasterUsersDisabledByDefault — NewAuthenticator
// without WithAuthenticatorMasterUsers(true) must NOT implement
// MasterAuthenticator. Type assertion at every session boundary
// (IMAP, POP3, Submission) relies on this for opt-in.
func TestAuthenticator_MasterUsersDisabledByDefault(t *testing.T) {
	auth := NewAuthenticator(
		[]Passdb{&credPassdb{"alice", "secret"}},
		// WithAuthenticatorMasterdb + Separator are set even though
		// the toggle is off — they MUST be inert.
		WithAuthenticatorMasterdb([]Passdb{&credPassdb{"admin", "masterpass"}}),
		WithAuthenticatorMasterUserSeparator("*"),
	)
	if _, ok := auth.(MasterAuthenticator); ok {
		t.Errorf("NewAuthenticator surfaces MasterAuthenticator while disabled")
	}
	// And Authenticate itself still works for regular logins.
	res, err := auth.Authenticate("alice", "secret", "imap", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != AuthOK {
		t.Errorf("regular login broken when master disabled: %v", res.Result)
	}
}

// TestAuthenticator_MasterUsersEnabledSurfaces — flipping the
// toggle exposes MasterAuthenticator.
func TestAuthenticator_MasterUsersEnabledSurfaces(t *testing.T) {
	auth := NewAuthenticator(
		[]Passdb{&credPassdb{"alice", "secret"}},
		WithAuthenticatorMasterUsers(true),
		WithAuthenticatorMasterdb([]Passdb{&credPassdb{"admin", "masterpass"}}),
	)
	master, ok := auth.(MasterAuthenticator)
	if !ok {
		t.Fatalf("NewAuthenticator hides MasterAuthenticator while enabled")
	}
	res, err := master.AuthenticateMaster("alice", "admin", "masterpass", "imap", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != AuthOK {
		t.Errorf("master flow failed when enabled: %v", res.Result)
	}
	if res.Username != "alice" {
		t.Errorf("master flow did not switch identity: %q", res.Username)
	}
}

// TestWire_MasterUser_DisabledIgnoresSeparator — the
// `target<sep>master` workaround MUST be inert while master-users
// are disabled. authid `alice*admin` is treated literally as the
// username `alice*admin`, not split into target+master.
func TestWire_MasterUser_DisabledIgnoresSeparator(t *testing.T) {
	srv := NewServer(
		// Regular passdb literally knows the user `alice*admin`
		// (operator-defined login name); separator splitting must
		// not interfere with such a username while disabled.
		[]Passdb{&credPassdb{"alice*admin", "literalpass"}},
		WithMasterdb([]Passdb{&credPassdb{"admin", "masterpass"}}),
		WithMasterUserSeparator("*"),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	fmt.Fprintf(conn, "AUTH\t41\tPLAIN\tservice=imap\tresp=\x00alice*admin\x00literalpass\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	got := sc.Text()
	if !strings.HasPrefix(got, "OK\t41\tuser=alice*admin") {
		t.Errorf("separator split while disabled: %q", got)
	}
}
