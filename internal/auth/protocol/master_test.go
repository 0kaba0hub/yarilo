package protocol

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// stubIteratorUserdb is a Userdb + UserdbIterator combo for the
// LIST tests. Returning iterErr from Iterate exercises the FAIL path.
type stubIteratorUserdb struct {
	users   map[string]*UserInfo
	all     []string
	lookErr error
	iterErr error
}

func (s *stubIteratorUserdb) Lookup(username string) (*UserInfo, error) {
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

// nonIteratorUserdb implements only Userdb, NOT UserdbIterator —
// drives the LIST-not-supported path.
type nonIteratorUserdb struct {
	users map[string]*UserInfo
}

func (n *nonIteratorUserdb) Lookup(username string) (*UserInfo, error) {
	return n.users[username], nil
}

// startMaster spins a MasterServer on a random local port and returns
// the dialer. Cleanup teardown is registered with t.
//
// Port-allocation pattern (mirrors authclient/client_test.go and
// backendapi/userdb_test.go): bind a probe listener, capture its
// address, CLOSE the probe BEFORE starting the server goroutine,
// then race the server's own ListenAndServe against any other
// process that might steal the freed port. The wait loop below
// covers the loopback race window.
//
// The bug this comment-block warns against: an earlier version of
// this helper started the server goroutine BEFORE closing the
// probe ln. Under load (self-hosted CI runner saturated), the
// scheduler could run the goroutine first, observe the probe ln
// still holding the port, fail ListenAndServe silently (the
// errcheck nolint above lets it die), and leave nothing listening.
// Subsequent dial() then failed with "connection refused" and the
// test (TestMaster_PipelinedRequestsSerialiseInOrder) flaked.
func startMaster(t *testing.T, userdb Userdb) func() (net.Conn, *bufio.Reader) {
	t.Helper()
	srv := NewMasterServer(userdb)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck

	// Wait briefly for the real listener to come up.
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
	return func() (net.Conn, *bufio.Reader) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn, bufio.NewReader(conn)
	}
}

// readMasterHandshake consumes the VERSION / SPID / CUID / COOKIE / DONE
// lines the server emits on connect. Returns the parsed version
// fields so tests can assert the wire constants.
func readMasterHandshake(t *testing.T, r *bufio.Reader) (proto string, major, minor string) {
	t.Helper()
	saw := map[string]bool{}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("handshake read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "DONE" {
			break
		}
		parts := strings.Split(line, "\t")
		switch parts[0] {
		case "VERSION":
			if len(parts) != 4 {
				t.Fatalf("VERSION malformed: %q", line)
			}
			proto, major, minor = parts[1], parts[2], parts[3]
		case "SPID", "CUID", "COOKIE":
			saw[parts[0]] = true
		}
	}
	for _, k := range []string{"SPID", "CUID", "COOKIE"} {
		if !saw[k] {
			t.Errorf("handshake missing %s", k)
		}
	}
	return
}

func send(t *testing.T, c net.Conn, line string) {
	t.Helper()
	if _, err := c.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

func TestMaster_HandshakeAdvertisesProtocol(t *testing.T) {
	dial := startMaster(t, nil)
	_, r := dial()
	proto, major, minor := readMasterHandshake(t, r)
	if proto != "yarilo-auth-master" || major != "1" || minor != "0" {
		t.Errorf("VERSION = %s %s.%s, want yarilo-auth-master 1.0", proto, major, minor)
	}
}

func TestMaster_UserHit(t *testing.T) {
	udb := &stubIteratorUserdb{users: map[string]*UserInfo{
		"alice": {
			Username:     "alice",
			UID:          1001,
			GID:          1001,
			Home:         "/h/alice",
			MailLocation: "maildir:/m/alice",
			QuotaRules:   []string{"*:storage=5G", "Trash:storage=+1G"},
		},
	}}
	dial := startMaster(t, udb)
	c, r := dial()
	readMasterHandshake(t, r)

	send(t, c, "USER\t7\talice")
	got := readLine(t, r)
	// Expected line begins with "USER\t7\talice\t" followed by fields
	// in marshalUserInfo's canonical order.
	if !strings.HasPrefix(got, "USER\t7\talice\t") {
		t.Fatalf("response = %q, want USER 7 alice prefix", got)
	}
	for _, want := range []string{
		"uid=1001",
		"gid=1001",
		"home=/h/alice",
		"mail=maildir:/m/alice",
		"quota_rule=*:storage=5G,Trash:storage=+1G",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("response %q missing %q", got, want)
		}
	}
}

func TestMaster_UserMiss(t *testing.T) {
	dial := startMaster(t, &stubIteratorUserdb{users: map[string]*UserInfo{}})
	c, r := dial()
	readMasterHandshake(t, r)

	send(t, c, "USER\t1\tghost")
	got := readLine(t, r)
	if got != "NOTFOUND\t1" {
		t.Errorf("response = %q, want NOTFOUND 1", got)
	}
}

func TestMaster_UserNilUserdb(t *testing.T) {
	dial := startMaster(t, nil)
	c, r := dial()
	readMasterHandshake(t, r)

	send(t, c, "USER\t1\talice")
	got := readLine(t, r)
	if got != "NOTFOUND\t1" {
		t.Errorf("response = %q, want NOTFOUND 1 when userdb is nil", got)
	}
}

func TestMaster_UserBackendError(t *testing.T) {
	udb := &stubIteratorUserdb{lookErr: errors.New("db down")}
	dial := startMaster(t, udb)
	c, r := dial()
	readMasterHandshake(t, r)

	send(t, c, "USER\t9\talice")
	got := readLine(t, r)
	if !strings.HasPrefix(got, "FAIL\t9\treason=db down") {
		t.Errorf("response = %q, want FAIL 9 reason=db down", got)
	}
}

func TestMaster_UserMalformedFrame(t *testing.T) {
	dial := startMaster(t, &stubIteratorUserdb{})
	c, r := dial()
	readMasterHandshake(t, r)

	send(t, c, "USER\t3") // missing username
	got := readLine(t, r)
	if !strings.HasPrefix(got, "FAIL\t3\treason=") {
		t.Errorf("response = %q, want FAIL 3 reason=...", got)
	}
}

func TestMaster_PassReturnsNotImplemented(t *testing.T) {
	dial := startMaster(t, &stubIteratorUserdb{})
	c, r := dial()
	readMasterHandshake(t, r)

	send(t, c, "PASS\t5\talice")
	got := readLine(t, r)
	if !strings.HasPrefix(got, "FAIL\t5\treason=PASS not implemented") {
		t.Errorf("response = %q, want PASS not implemented FAIL", got)
	}
}

func TestMaster_ListStreamsThenDone(t *testing.T) {
	udb := &stubIteratorUserdb{
		users: map[string]*UserInfo{},
		all:   []string{"alice", "bob", "carol"},
	}
	dial := startMaster(t, udb)
	c, r := dial()
	readMasterHandshake(t, r)

	send(t, c, "LIST\t11")
	wantStream := []string{
		"LIST\t11\talice",
		"LIST\t11\tbob",
		"LIST\t11\tcarol",
		"DONE\t11",
	}
	for i, want := range wantStream {
		got := readLine(t, r)
		if got != want {
			t.Errorf("[%d] = %q, want %q", i, got, want)
		}
	}
}

func TestMaster_ListNoIteratorFailsCleanly(t *testing.T) {
	dial := startMaster(t, &nonIteratorUserdb{users: map[string]*UserInfo{}})
	c, r := dial()
	readMasterHandshake(t, r)

	send(t, c, "LIST\t2")
	got := readLine(t, r)
	if !strings.HasPrefix(got, "FAIL\t2\treason=userdb does not support enumeration") {
		t.Errorf("response = %q, want enumeration-not-supported FAIL", got)
	}
}

func TestMaster_ListIteratorErrorPropagates(t *testing.T) {
	udb := &stubIteratorUserdb{iterErr: errors.New("scan failed")}
	dial := startMaster(t, udb)
	c, r := dial()
	readMasterHandshake(t, r)

	send(t, c, "LIST\t4")
	got := readLine(t, r)
	if !strings.HasPrefix(got, "FAIL\t4\treason=scan failed") {
		t.Errorf("response = %q, want FAIL 4 reason=scan failed", got)
	}
}

func TestMaster_UnknownCommandFails(t *testing.T) {
	dial := startMaster(t, &stubIteratorUserdb{})
	c, r := dial()
	readMasterHandshake(t, r)

	send(t, c, "FROBNICATE\t8\twhatever")
	got := readLine(t, r)
	if !strings.HasPrefix(got, "FAIL\t8\treason=unknown command") {
		t.Errorf("response = %q, want unknown-command FAIL", got)
	}
}

func TestMaster_PipelinedRequestsSerialiseInOrder(t *testing.T) {
	udb := &stubIteratorUserdb{users: map[string]*UserInfo{
		"alice": {Username: "alice", UID: 1001},
		"bob":   {Username: "bob", UID: 1002},
	}}
	dial := startMaster(t, udb)
	c, r := dial()
	readMasterHandshake(t, r)

	// Send several commands back-to-back without reading between
	// them. The server processes commands serially per connection,
	// so responses arrive in the order they were sent.
	for _, q := range []string{"USER\t1\talice", "USER\t2\tbob", "USER\t3\tghost"} {
		send(t, c, q)
	}
	want := []string{"USER\t1\talice", "USER\t2\tbob", "NOTFOUND\t3"}
	for i, prefix := range want {
		got := readLine(t, r)
		if !strings.HasPrefix(got, prefix) {
			t.Errorf("[%d] = %q, want prefix %q", i, got, prefix)
		}
	}
}

func TestMarshalUserInfo_SkipsInternalFields(t *testing.T) {
	ui := &UserInfo{
		Username:       "alice",
		UID:            1001,
		Password:       "should-not-cross-wire",
		CertName:       "internal-cert-cn",
		PolicyResponse: "internal-policy-blob",
	}
	got := marshalUserInfo(ui)
	for _, leak := range []string{"should-not-cross-wire", "internal-cert-cn", "internal-policy-blob",
		"password=", "cert_name=", "policy_response="} {
		if strings.Contains(got, leak) {
			t.Errorf("marshaled output leaks internal data: %q (full: %q)", leak, got)
		}
	}
}

func TestMarshalUserInfo_DeterministicMapOrder(t *testing.T) {
	ui := &UserInfo{
		Username: "alice",
		Extra:    map[string]string{"zeta": "z", "alpha": "a", "mid": "m"},
		Forward:  map[string]string{"session": "abc", "origin_ip": "1.2.3.4"},
	}
	first := marshalUserInfo(ui)
	for i := 0; i < 5; i++ {
		if got := marshalUserInfo(ui); got != first {
			t.Errorf("non-deterministic output:\n first=%q\n  now=%q", first, got)
		}
	}
	// Verify alphabetical order within each group.
	alphaIdx := strings.Index(first, "alpha=a")
	midIdx := strings.Index(first, "mid=m")
	zetaIdx := strings.Index(first, "zeta=z")
	if !(alphaIdx < midIdx && midIdx < zetaIdx) {
		t.Errorf("Extra keys not lexicographically ordered: %q", first)
	}
}

func TestMarshalUserInfo_BooleanOnlyWhenTrue(t *testing.T) {
	ui := &UserInfo{Username: "alice", NoLogin: true, NoDelay: false, Proxy: true, ProxyMaybe: false}
	got := marshalUserInfo(ui)
	if !strings.Contains(got, "nologin=yes") {
		t.Errorf("missing nologin=yes in %q", got)
	}
	if !strings.Contains(got, "proxy=yes") {
		t.Errorf("missing proxy=yes in %q", got)
	}
	if strings.Contains(got, "nodelay=") {
		t.Errorf("false bool should be omitted, got %q", got)
	}
	if strings.Contains(got, "proxy_maybe=") {
		t.Errorf("false bool should be omitted, got %q", got)
	}
}

func TestMarshalUserInfo_EscapesTabAndNewline(t *testing.T) {
	ui := &UserInfo{
		Username: "alice",
		Home:     "/home/with\ttab/and\nnewline",
	}
	got := marshalUserInfo(ui)
	if !strings.Contains(got, `home=/home/with\ttab/and\nnewline`) {
		t.Errorf("escapes not applied: %q", got)
	}
}

func TestMarshalUserInfo_EmptyReturnsEmpty(t *testing.T) {
	if got := marshalUserInfo(&UserInfo{}); got != "" {
		t.Errorf("empty UserInfo produces %q, want empty string", got)
	}
	if got := marshalUserInfo(nil); got != "" {
		t.Errorf("nil UserInfo produces %q, want empty string", got)
	}
}
