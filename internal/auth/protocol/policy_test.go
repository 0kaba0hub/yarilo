package protocol

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubPolicy records every Check / Report call. Returns a
// preset Decision from each Check method.
type stubPolicy struct {
	mu             sync.Mutex
	beforeCalls    []PolicyRequest
	afterCalls     []afterCall
	reportCalls    []reportCall
	beforeDecision PolicyDecision
	beforeErr      error
	afterDecision  PolicyDecision
	afterErr       error
}

type afterCall struct {
	req     PolicyRequest
	success bool
	reject  bool
}

type reportCall struct {
	req     PolicyRequest
	success bool
	reject  bool
}

func (p *stubPolicy) CheckBefore(_ context.Context, req PolicyRequest) (PolicyDecision, error) {
	p.mu.Lock()
	p.beforeCalls = append(p.beforeCalls, req)
	d, e := p.beforeDecision, p.beforeErr
	p.mu.Unlock()
	return d, e
}

func (p *stubPolicy) CheckAfter(_ context.Context, req PolicyRequest, success, reject bool) (PolicyDecision, error) {
	p.mu.Lock()
	p.afterCalls = append(p.afterCalls, afterCall{req, success, reject})
	d, e := p.afterDecision, p.afterErr
	p.mu.Unlock()
	return d, e
}

func (p *stubPolicy) ReportAfter(_ context.Context, req PolicyRequest, success, reject bool) {
	p.mu.Lock()
	p.reportCalls = append(p.reportCalls, reportCall{req, success, reject})
	p.mu.Unlock()
}

func newStubPolicy(beforeAllow bool) *stubPolicy {
	d := PolicyDecision{}
	if beforeAllow {
		d.Continue = true
	}
	return &stubPolicy{
		beforeDecision: d,
		afterDecision:  PolicyDecision{Continue: true},
	}
}

// TestWire_Policy_CheckBeforeContinues — Continue decision lets
// the chain run normally.
func TestWire_Policy_CheckBeforeContinues(t *testing.T) {
	p := newStubPolicy(true)
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "secret"}},
		WithPolicy(p, PolicyMode{CheckBefore: true}),
	)
	addr, cancel := startTestServer(t, srv)
	defer cancel()

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()
	fmt.Fprintf(conn, "AUTH\t1\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n")
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "OK") {
		t.Fatalf("expected OK, got %q", sc.Text())
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.beforeCalls) != 1 {
		t.Errorf("CheckBefore called %d times, want 1", len(p.beforeCalls))
	}
}

// TestWire_Policy_CheckBeforeRejectsPreChain — Reject pre-chain
// MUST surface as opaque FAIL and the passdb MUST NOT run.
func TestWire_Policy_CheckBeforeRejectsPreChain(t *testing.T) {
	p := &stubPolicy{
		beforeDecision: PolicyDecision{Reject: true, Message: "abuse"},
		afterDecision:  PolicyDecision{Continue: true},
	}
	sentinel := &stubPassdb{
		result: ResultFail,
		setBag: func(f *Fields) {
			t.Errorf("passdb ran after CheckBefore Reject")
		},
	}
	srv := NewServer(
		[]Passdb{sentinel},
		WithPolicy(p, PolicyMode{CheckBefore: true}),
	)
	addr, cancel := startTestServer(t, srv)
	defer cancel()

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()
	fmt.Fprintf(conn, "AUTH\t2\tPLAIN\tservice=imap\tresp=\x00alice\x00x\n")
	if !sc.Scan() {
		t.Fatal("no reply")
	}
	if sc.Text() != "FAIL\t2" {
		t.Errorf("policy reject leaked details: %q (want FAIL\\t2)", sc.Text())
	}
}

// TestWire_Policy_CheckAfterDowngradesSuccess — passdb says OK
// but CheckAfter rejects → wire returns FAIL.
func TestWire_Policy_CheckAfterDowngradesSuccess(t *testing.T) {
	p := &stubPolicy{
		beforeDecision: PolicyDecision{Continue: true},
		afterDecision:  PolicyDecision{Reject: true, Message: "takeover"},
	}
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "secret"}},
		WithPolicy(p, PolicyMode{CheckBefore: true, CheckAfter: true}),
	)
	addr, cancel := startTestServer(t, srv)
	defer cancel()

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()
	fmt.Fprintf(conn, "AUTH\t3\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n")
	if !sc.Scan() {
		t.Fatal("no reply")
	}
	if sc.Text() != "FAIL\t3" {
		t.Errorf("after-reject did not downgrade OK: %q", sc.Text())
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.afterCalls) != 1 || !p.afterCalls[0].success {
		t.Errorf("CheckAfter success-flag wrong: %+v", p.afterCalls)
	}
}

// TestWire_Policy_ReportAfter_FiresOnOK — successful auth fires
// the report telemetry.
func TestWire_Policy_ReportAfter_FiresOnOK(t *testing.T) {
	p := newStubPolicy(true)
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "secret"}},
		WithPolicy(p, PolicyMode{CheckBefore: true, ReportAfter: true}),
	)
	addr, cancel := startTestServer(t, srv)
	defer cancel()

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()
	fmt.Fprintf(conn, "AUTH\t4\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n")
	sc.Scan()
	time.Sleep(50 * time.Millisecond) // wait for goroutine

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reportCalls) != 1 || !p.reportCalls[0].success {
		t.Errorf("Report on OK wrong: %+v", p.reportCalls)
	}
}

// TestWire_Policy_ReportAfter_FiresOnFail — failed auth also
// reports (telemetry needs both sides).
func TestWire_Policy_ReportAfter_FiresOnFail(t *testing.T) {
	p := newStubPolicy(true)
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "right"}},
		WithPolicy(p, PolicyMode{CheckBefore: true, ReportAfter: true}),
	)
	addr, cancel := startTestServer(t, srv)
	defer cancel()

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()
	fmt.Fprintf(conn, "AUTH\t5\tPLAIN\tservice=imap\tresp=\x00alice\x00WRONG\n")
	sc.Scan()
	time.Sleep(50 * time.Millisecond)

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reportCalls) != 1 || p.reportCalls[0].success {
		t.Errorf("Report on fail wrong: %+v", p.reportCalls)
	}
}

// TestWire_Policy_MasterFlowExempt — master-user impersonation
// MUST bypass policy entirely (admin sessions are not subject to
// abuse policy).
func TestWire_Policy_MasterFlowExempt(t *testing.T) {
	p := newStubPolicy(true)
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "userpass"}},
		WithMasterUsers(true),
		WithMasterdb([]Passdb{&credPassdb{"admin", "masterpass"}}),
		WithUserdb(targetUserdbForWire{}),
		WithPolicy(p, PolicyMode{CheckBefore: true, CheckAfter: true, ReportAfter: true}),
	)
	addr, cancel := startTestServer(t, srv)
	defer cancel()

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()
	fmt.Fprintf(conn, "AUTH\t6\tPLAIN\tservice=imap\tresp=alice\x00admin\x00masterpass\n")
	sc.Scan()
	time.Sleep(50 * time.Millisecond)

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.beforeCalls)+len(p.afterCalls)+len(p.reportCalls) != 0 {
		t.Errorf("master flow consulted policy: before=%v after=%v report=%v",
			p.beforeCalls, p.afterCalls, p.reportCalls)
	}
}

// TestWire_Policy_TarpitDelaysPreChain — TarpitSecs>0 from
// CheckBefore inserts a sleep before the chain runs.
func TestWire_Policy_TarpitDelaysPreChain(t *testing.T) {
	p := &stubPolicy{
		beforeDecision: PolicyDecision{Continue: true, TarpitSecs: 1},
		afterDecision:  PolicyDecision{Continue: true},
	}
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "secret"}},
		WithPolicy(p, PolicyMode{CheckBefore: true}),
	)
	addr, cancel := startTestServer(t, srv)
	defer cancel()

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	t0 := time.Now()
	fmt.Fprintf(conn, "AUTH\t7\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n")
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "OK") {
		t.Fatalf("expected OK, got %q", sc.Text())
	}
	if elapsed := time.Since(t0); elapsed < time.Second {
		t.Errorf("tarpit not applied (%v)", elapsed)
	}
}

// startTestServer is a shared helper for policy + penalty tests.
func startTestServer(t *testing.T, srv *Server) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)
	return addr, cancel
}
