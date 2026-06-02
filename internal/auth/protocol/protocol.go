// Package protocol implements the yarilo-auth TCP+mTLS protocol.
// Server → Client handshake, then AUTH/CONT/CANCEL commands.
package protocol

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	protoName = "yarilo-auth"
	majorVer  = 1
	minorVer  = 0
	maxLine   = 16384
)

// AuthResult is the outcome of a single authentication attempt.
type AuthResult int

const (
	AuthOK AuthResult = iota
	AuthFail
	AuthTempFail
)

// AuthResponse is the final response sent back to the client session.
//
// Phase AUTH-2 PR 1 adds Fields alongside the legacy typed members.
// When a passdb populates Fields, handleAuth emits the OK reply
// from the bag (with prefix gating: auth_* stripped, userdb_*
// passed through). When Fields is nil the typed fallback path runs
// — that's the pre-AUTH-2 wire shape, byte-compatible for the
// existing SQL passdb until PR 2 swaps the Passdb interface to
// take a shared Fields instance directly.
type AuthResponse struct {
	Result   AuthResult
	Username string
	Home     string
	MailLoc  string
	Proxy    bool
	Host     string
	Port     int

	// Fields carries the passdb result as a key/value bag with
	// prefix-derived scoping (see fields.go). Populated by passdb
	// implementations that opt into Phase AUTH-2's wire surface;
	// nil falls back to the typed-fields wire path.
	Fields *Fields
}

// Request bundles every input a passdb needs to authenticate plus
// the shared Fields bag the chain mutates as it walks. Phase AUTH-2
// PR 2 introduces this struct so passdb backends can write directly
// into the bag (the chain isolates each driver's mutations via
// Snapshot / Rollback on ResultNext). PR 3 will extend Request with
// userdb-prefetch metadata.
type Request struct {
	Username string
	Password string
	Service  string

	// Fields is the chain-wide bag that accumulates passdb output.
	// Driver implementations MAY assume non-nil; the canonical
	// caller (Chain.Authenticate) allocates it when the Server-side
	// authenticate entry-point starts the chain.
	Fields *Fields
}

// Result classifies a passdb outcome. Separate from AuthResult
// (which is wire-shaped on AuthResponse) because ResultNext is a
// chain-internal signal — it never reaches the client wire.
type Result int

const (
	// ResultOK — user verified, Fields populated, chain stops.
	ResultOK Result = iota
	// ResultFail — credentials rejected (password mismatch,
	// disabled account, expired credential). Chain stops; the
	// wire returns FAIL.
	ResultFail
	// ResultNext — user unknown in this driver; chain rolls back
	// any partial Fields mutations and tries the next driver.
	ResultNext
	// ResultTempFail — backend technical failure (DB down,
	// network). Chain stops with a wire-level FAIL temp_fail.
	// Always accompanies a non-nil error so the server-side log
	// has the underlying cause.
	ResultTempFail
)

// Passdb is the interface that passdb backends implement. Drivers
// receive a chain-wide Request whose Fields bag they mutate
// directly; the returned Result selects the chain's next move.
// error is reserved for unexpected technical failures that
// accompany ResultTempFail — the wire never carries the error
// text, only the temp_fail marker, so a driver-level "connection
// refused" is invisible to the login pod.
type Passdb interface {
	Authenticate(req *Request) (Result, error)
}

// Authenticator is the simpler legacy surface session paths
// (yarilo-imap, yarilo-pop3, etc.) use directly when they need to
// verify credentials in-process (without going over the
// yarilo-auth wire). Wraps a Passdb chain and projects the chain-
// internal Result back onto a wire-shaped AuthResponse so the
// caller does not need to know about Request / Result.
type Authenticator interface {
	Authenticate(username, password, service string) (*AuthResponse, error)
}

// MasterAuthenticator extends Authenticator with the SASL PLAIN
// authzid surface — i.e. master-user impersonation. Session-level
// callers (IMAP / POP3 / Submission) type-assert opts.Auth into
// this interface to decide whether to honour authzid; backends
// that only implement Authenticator (or stub test doubles) keep
// working unchanged but fall back to "authzid must equal authid"
// behaviour at the SASL layer.
//
//   - authzid — target identity the caller wants to log in AS.
//     When empty (or equal to authid), this is a regular login and
//     callers should typically dispatch via Authenticate instead.
//   - authid  — the user supplying the password (the master in
//     an impersonation request).
//   - password — the master's password.
//   - service — login service tag (imap / pop3 / submission /
//     lmtp). Logged + forwarded to the chain unmodified.
type MasterAuthenticator interface {
	AuthenticateMaster(authzid, authid, password, service string) (*AuthResponse, error)
}

// AuthenticatorOption tunes a NewAuthenticator construction.
type AuthenticatorOption func(*chainAuthenticator)

// WithAuthenticatorUserdb attaches a userdb backend to the
// Authenticator. When set, a successful passdb result is enriched
// with the userdb lookup (with prefetch detection — see RunAuth).
// The userdb fields land in the response bag with the `userdb_`
// prefix so the caller can distinguish passdb-only fields from
// userdb-enriched ones.
func WithAuthenticatorUserdb(u Userdb) AuthenticatorOption {
	return func(a *chainAuthenticator) { a.userdb = u }
}

// WithAuthenticatorMasterdb attaches a dedicated masterdb chain.
// Mirrors WithMasterdb on the wire Server: AuthenticateMaster
// consults this chain first, falling through to the main passdb's
// per-user `master_user=yes` flag only when no masterdb entry
// knows the master.
func WithAuthenticatorMasterdb(passdbs []Passdb) AuthenticatorOption {
	return func(a *chainAuthenticator) { a.masterdb = passdbs }
}

// WithAuthenticatorMasterUserSeparator enables the
// `target<sep>master` SASL workaround for legacy clients that
// cannot send authzid. Empty disables it. Mirrors
// WithMasterUserSeparator on the wire Server.
func WithAuthenticatorMasterUserSeparator(sep string) AuthenticatorOption {
	return func(a *chainAuthenticator) { a.masterUserSeparator = sep }
}

// WithAuthenticatorMasterUsers flips the top-level master-user
// opt-in. When false (the default), NewAuthenticator returns an
// Authenticator-only wrapper — type-asserts to
// MasterAuthenticator at every protocol entry point fail, so any
// distinct SASL PLAIN authzid is rejected before the chain is
// consulted. When true, the chainAuthenticator's
// AuthenticateMaster method is exposed and the masterdb /
// separator options take effect.
func WithAuthenticatorMasterUsers(enabled bool) AuthenticatorOption {
	return func(a *chainAuthenticator) { a.masterUsersEnabled = enabled }
}

// NewAuthenticator wraps one or more Passdb drivers into the
// session-friendly Authenticator surface. Optionally attach a
// userdb via WithAuthenticatorUserdb so the bag returned by every
// successful Authenticate also carries userdb_* fields the
// session path needs for mail-storage setup.
//
// Master-user impersonation is opt-in via
// WithAuthenticatorMasterUsers(true). When disabled (the default)
// the returned Authenticator deliberately does NOT implement
// MasterAuthenticator — so session-level type assertions in
// IMAP/POP3/Submission fail and any distinct SASL PLAIN authzid
// is rejected. Operators must explicitly flip the toggle (and
// configure masterdb / separator) to enable the feature.
func NewAuthenticator(passdbs []Passdb, opts ...AuthenticatorOption) Authenticator {
	a := &chainAuthenticator{chain: Chain(passdbs)}
	for _, opt := range opts {
		opt(a)
	}
	if !a.masterUsersEnabled {
		// Hide AuthenticateMaster behind a wrapper that exposes
		// only the Authenticator surface. This makes the
		// type-assert in session.Auth(PLAIN) fail cleanly.
		return &plainOnlyAuthenticator{inner: a}
	}
	return a
}

// plainOnlyAuthenticator wraps chainAuthenticator to hide its
// AuthenticateMaster method when master-users are disabled.
// Implements Authenticator but NOT MasterAuthenticator on
// purpose — Go method-set lookup walks the embedded type and
// would expose AuthenticateMaster if we just embedded the inner
// pointer, so the field is named and the wrapper redeclares
// only the methods it wants to expose.
type plainOnlyAuthenticator struct{ inner *chainAuthenticator }

func (p *plainOnlyAuthenticator) Authenticate(username, password, service string) (*AuthResponse, error) {
	return p.inner.Authenticate(username, password, service)
}

type chainAuthenticator struct {
	chain               Chain
	userdb              Userdb
	masterdb            []Passdb
	masterUserSeparator string
	masterUsersEnabled  bool
}

func (c *chainAuthenticator) Authenticate(username, password, service string) (*AuthResponse, error) {
	req := &Request{
		Username: username,
		Password: password,
		Service:  service,
		Fields:   NewFields(),
	}
	result, err := RunAuth(c.chain, c.userdb, req)
	resp := &AuthResponse{
		Result: authResultFromChain(result),
		Fields: req.Fields,
	}
	if v, ok := req.Fields.Get("user"); ok {
		resp.Username = v
	} else {
		resp.Username = username
	}
	if v, ok := req.Fields.Get("home"); ok {
		resp.Home = v
	}
	if v, ok := req.Fields.Get("mail"); ok {
		resp.MailLoc = v
	}
	return resp, err
}

// AuthenticateMaster implements MasterAuthenticator. Routing
// matches the wire Server.authenticate:
//   - authzid empty (no impersonation) → RunAuth path.
//   - authzid set and == authid → caller asked to log in as self;
//     treat as regular login (RFC 4616 permits this).
//   - authzid set and != authid → RunMasterAuth path.
//
// The `target<sep>master` separator workaround is applied when
// authzid is empty AND masterUserSeparator is non-empty.
func (c *chainAuthenticator) AuthenticateMaster(authzid, authid, password, service string) (*AuthResponse, error) {
	// Defence-in-depth: even if a caller obtains the
	// chainAuthenticator directly (bypassing the wrapper
	// NewAuthenticator hands out when master-users are disabled),
	// honour the toggle here too. Distinct authzid against a
	// disabled chain is treated as a regular login of the
	// AUTHID — the safe fallback that never grants impersonation.
	if !c.masterUsersEnabled {
		return c.Authenticate(authid, password, service)
	}
	target := authzid
	master := authid
	if target == "" && c.masterUserSeparator != "" {
		if m, t := SplitMasterFromAuthid(authid, c.masterUserSeparator); t != "" {
			master = m
			target = t
		}
	}
	if target == "" || target == master {
		return c.Authenticate(master, password, service)
	}
	req := &Request{
		Username: master,
		Password: password,
		Service:  service,
		Fields:   NewFields(),
	}
	result, err := RunMasterAuth(c.chain, Chain(c.masterdb), c.userdb, target, req)
	resp := &AuthResponse{
		Result: authResultFromChain(result),
		Fields: req.Fields,
	}
	if v, ok := req.Fields.Get("user"); ok {
		resp.Username = v
	} else {
		resp.Username = req.Username
	}
	if v, ok := req.Fields.Get("home"); ok {
		resp.Home = v
	}
	if v, ok := req.Fields.Get("mail"); ok {
		resp.MailLoc = v
	}
	return resp, err
}

// RunAuth executes the passdb chain and — on ResultOK with a
// userdb configured — also fills the bag with userdb_* fields
// from a userdb.Lookup. Prefetch detection: when the chain has
// already populated any userdb_-prefixed key (an SQL passdb that
// SELECT-ed userdb columns in the same query), userdb.Lookup is
// skipped (mirrors Dovecot's userdb-prefetch driver behaviour).
//
// userdb errors and misses do NOT downgrade ResultOK — passdb is
// authoritative for the auth decision. They surface in server-side
// logs only; the response carries passdb-only fields. The
// rationale matches Dovecot: a temporary userdb outage should not
// reject a verified user since the wire response also doubles as
// the only credential check.
//
// Allocates req.Fields when the caller passed nil so the userdb
// writer never trips a nil-pointer.
func RunAuth(chain Chain, userdb Userdb, req *Request) (Result, error) {
	if req.Fields == nil {
		req.Fields = NewFields()
	}
	result, err := chain.Authenticate(req)
	if result != ResultOK || userdb == nil {
		return result, err
	}
	if hasUserdbFields(req.Fields) {
		return result, err
	}
	info, ulerr := userdb.Lookup(req.Username)
	if ulerr != nil {
		slog.Warn("auth/protocol: userdb lookup failed after passdb OK",
			"user", req.Username, "err", ulerr)
		return result, err
	}
	if info == nil {
		slog.Warn("auth/protocol: userdb miss for passdb-verified user",
			"user", req.Username)
		return result, err
	}
	writeUserdbFields(req.Fields, info)
	return result, err
}

// hasUserdbFields reports whether the bag already carries at
// least one userdb_-prefixed key — the marker for a prefetched
// userdb result that a downstream userdb.Lookup must not overwrite.
func hasUserdbFields(f *Fields) bool {
	if f == nil {
		return false
	}
	var found bool
	f.Each(func(k, _ string) bool {
		if strings.HasPrefix(k, "userdb_") {
			found = true
			return false
		}
		return true
	})
	return found
}

// writeUserdbFields visits every populated UserInfo field and
// writes it into f with the `userdb_` prefix. Internal-only
// fields (Password, CertName, PolicyResponse) are stripped at
// VisitFields construction, so they cannot leak even when a
// buggy backend populates them.
func writeUserdbFields(f *Fields, ui *UserInfo) {
	ui.VisitFields(func(k, v string) {
		f.Set("userdb_"+k, v)
	})
}

// RunMasterAuth handles a master-user impersonation request:
// the master's password is verified, the request switches
// identity to target, and userdb runs against target.
//
// req.Username MUST be the master when the function is called.
// On success, req.Fields is mutated: `master_user` is overwritten
// with the master's username (Dovecot semantics — same field name
// is reused on the wire for audit), `original_user` + `login_user`
// + `user` are set to target, and req.Username is updated to
// target so any downstream consumer reading the struct sees the
// post-impersonation identity.
//
// Authorisation model (matches Dovecot's two equivalent paths):
//
//  1. masterdb chain (if non-empty) authenticates the master
//     with its own password. ResultOK → impersonation allowed.
//  2. Otherwise the main passdb chain authenticates the master;
//     the result must carry `master_user=yes` (truthy per the
//     reserved-field bool spelling) — that's the per-user master
//     flag Dovecot's `auth_master_user_passdb` mode uses.
//
// On either failure (no masterdb hit AND no `master_user=yes`
// flag from passdb) the request is rejected with ResultFail.
// req.Fields is rolled back to the pre-call snapshot so the
// caller sees an empty bag, not the master's verified passdb
// fields leaking into a failed impersonation response.
//
// userdb runs for TARGET, not for the master. Errors / misses
// behave like in RunAuth — they do not downgrade ResultOK.
func RunMasterAuth(passdbs, masterdb Chain, userdb Userdb, target string, req *Request) (Result, error) {
	if req.Fields == nil {
		req.Fields = NewFields()
	}
	preSnap := req.Fields.Snapshot()

	// Phase 1: try the dedicated masterdb chain. Walked
	// per-driver (not via Chain.Authenticate) because the
	// distinction matters here:
	//   - any driver returns ResultOK → impersonation allowed.
	//   - any driver returns ResultFail → STOP, do not fall
	//     through to the main passdb. The driver knows the
	//     master and rejected the password; falling through
	//     would let the same master succeed via its per-user
	//     `master_user=yes` flag in passdb on the wrong
	//     password.
	//   - all drivers return ResultNext → masterdb does not
	//     know this user; fall through to the per-user flag
	//     in the main passdb.
	// Chain.Authenticate collapses Next-exhaust and explicit
	// Fail into the same ResultFail, which would break the
	// "stop on Fail, fall through on all-Next" contract above.
	masterOK := false
	if len(masterdb) > 0 {
		for _, db := range masterdb {
			snap := req.Fields.Snapshot()
			result, err := db.Authenticate(req)
			if err != nil {
				req.Fields.Rollback(preSnap)
				return ResultTempFail, err
			}
			switch result {
			case ResultOK:
				masterOK = true
			case ResultFail:
				req.Fields.Rollback(preSnap)
				return ResultFail, nil
			case ResultTempFail:
				req.Fields.Rollback(preSnap)
				return ResultTempFail, err
			case ResultNext:
				req.Fields.Rollback(snap)
				continue
			}
			break
		}
	}

	// Phase 2: per-user `master_user=yes` flag in main passdb.
	// Only consulted when masterdb did not authoritatively
	// reject (Next, or no masterdb configured).
	if !masterOK {
		snap := req.Fields.Snapshot()
		result, err := passdbs.Authenticate(req)
		if err != nil {
			req.Fields.Rollback(preSnap)
			return ResultTempFail, err
		}
		if result == ResultOK {
			if v, _ := req.Fields.Get("master_user"); IsTruthy(v) {
				masterOK = true
			}
		}
		if !masterOK {
			req.Fields.Rollback(snap)
		}
	}

	if !masterOK {
		req.Fields.Rollback(preSnap)
		return ResultFail, nil
	}

	// Master authenticated. Switch identity to target.
	masterName := req.Username
	req.Username = target
	req.Fields.Set("master_user", masterName)
	req.Fields.Set("original_user", target)
	req.Fields.Set("login_user", target)
	req.Fields.Set("user", target)

	// userdb lookup for TARGET — not the master.
	if userdb != nil && !hasUserdbFields(req.Fields) {
		info, ulerr := userdb.Lookup(target)
		if ulerr != nil {
			slog.Warn("auth/protocol: userdb lookup for master target failed",
				"master", masterName, "target", target, "err", ulerr)
		} else if info == nil {
			slog.Warn("auth/protocol: userdb miss for master target",
				"master", masterName, "target", target)
		} else {
			writeUserdbFields(req.Fields, info)
		}
	}

	return ResultOK, nil
}

// Chain composes multiple Passdb backends with first-hit-wins
// semantics. Each entry runs under a per-entry Fields.Snapshot so
// a driver's partial mutations are rolled back on ResultNext.
// Wire surface preserved from pre-PR-2: chain exhaust without a
// hit returns ResultFail (matches the pre-refactor Chain that
// returned AuthFail when every driver answered nil).
type Chain []Passdb

// Authenticate walks the chain. Snapshot before each driver,
// rollback on ResultNext, propagate immediately on OK / Fail /
// TempFail. Allocates the Fields bag on demand when the caller
// passed nil so drivers can always mutate without a nil-check.
func (c Chain) Authenticate(req *Request) (Result, error) {
	if req.Fields == nil {
		req.Fields = NewFields()
	}
	for _, db := range c {
		snap := req.Fields.Snapshot()
		result, err := db.Authenticate(req)
		switch result {
		case ResultNext:
			req.Fields.Rollback(snap)
			continue
		case ResultTempFail:
			req.Fields.Rollback(snap)
			return result, err
		}
		// ResultOK / ResultFail — driver's mutations kept,
		// chain stops.
		return result, err
	}
	return ResultFail, nil
}

// authResultFromChain maps the chain-internal Result onto the
// wire-shape AuthResult. ResultNext never reaches this path —
// Chain.Authenticate converts a chain exhaust to ResultFail before
// returning.
func authResultFromChain(r Result) AuthResult {
	switch r {
	case ResultOK:
		return AuthOK
	case ResultTempFail:
		return AuthTempFail
	default:
		return AuthFail
	}
}

// Server is the yarilo-auth TCP+mTLS server. Holds the passdb
// chain plus an optional userdb that enriches successful auth
// responses with userdb_* fields (Phase AUTH-2 PR 3) plus an
// optional masterdb chain + separator for master-user
// impersonation (Phase AUTH-3).
type Server struct {
	passdbs              []Passdb
	userdb               Userdb
	masterdb             []Passdb
	masterUserSeparator  string
	masterUsersEnabled   bool
	failureDelay         time.Duration
	internalFailureDelay time.Duration
	connUID              atomic.Uint64
	pid                  int
	cookie               string
}

// ServerOption tunes a NewServer construction.
type ServerOption func(*Server)

// WithUserdb attaches a userdb backend to the Server. When set,
// every successful passdb result is enriched with userdb fields
// (with prefetch detection — see RunAuth). The userdb fields land
// in the response with the `userdb_` prefix preserved on the wire,
// so login pods can use them to set up the mail session without a
// separate master-protocol round-trip.
func WithUserdb(u Userdb) ServerOption {
	return func(s *Server) { s.userdb = u }
}

// WithMasterdb attaches a dedicated masterdb chain. Master-user
// impersonation requests (PLAIN SASL responses where authzid is
// set and differs from authid) authenticate the authid against
// the masterdb first; if absent or not configured, the request
// falls through to the regular passdb and looks for a
// `master_user=yes` field on the result (Dovecot's per-user master
// flag — see auth/auth-request-fields.c). Either mechanism grants
// the impersonation.
func WithMasterdb(passdbs []Passdb) ServerOption {
	return func(s *Server) { s.masterdb = passdbs }
}

// WithMasterUserSeparator enables Dovecot's `target<sep>master`
// workaround for SASL PLAIN clients that cannot supply authzid
// in the standard third-field position. Typical value is `*`
// (Dovecot's `auth_master_user_separator` default). Empty
// disables the workaround — only RFC 4616 authzid is honoured.
func WithMasterUserSeparator(sep string) ServerOption {
	return func(s *Server) { s.masterUserSeparator = sep }
}

// WithMasterUsers is the top-level opt-in for master-user
// impersonation on the wire AUTH path. While false (the default)
// handleAuth ignores any SASL PLAIN authzid the client sends
// AND skips the separator workaround — every request routes
// through the regular passdb chain, indistinguishable from a
// build without master support. Set to true to expose the
// master flow at the wire level.
func WithMasterUsers(enabled bool) ServerOption {
	return func(s *Server) { s.masterUsersEnabled = enabled }
}

// WithFailureDelay sets the timing-leak mitigation delay for
// client-visible auth failures (wrong password, unknown user,
// malformed SASL). The reply is held back by d before writing
// to the wire. Zero disables the delay (mostly for tests).
// Mirrors Dovecot's `auth_failure_delay`.
func WithFailureDelay(d time.Duration) ServerOption {
	return func(s *Server) { s.failureDelay = d }
}

// WithInternalFailureDelay sets the delay applied when the
// failure is internal (passdb backend down, SQL refused, etc).
// Separate from WithFailureDelay because operators often want
// internal-error delays shorter than user-facing ones (the user
// will likely retry and another full failure_delay would amplify
// outage symptoms). Mirrors Dovecot's `auth_internal_failure_delay`.
func WithInternalFailureDelay(d time.Duration) ServerOption {
	return func(s *Server) { s.internalFailureDelay = d }
}

// NewServer creates an auth server with the given passdb chain.
// Optional userdb is attached via WithUserdb.
func NewServer(passdbs []Passdb, opts ...ServerOption) *Server {
	cookie := make([]byte, 16)
	rand.Read(cookie) //nolint:errcheck
	s := &Server{
		passdbs: passdbs,
		pid:     os.Getpid(),
		cookie:  hex.EncodeToString(cookie),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ListenAndServe starts the auth TCP server. When tlsCfg is non-nil the
// listener uses mTLS (TLS 1.3, RequireAndVerifyClientCert). Blocks until ctx
// is cancelled; active sessions drain before the function returns.
func (s *Server) ListenAndServe(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	var ln net.Listener
	var err error
	if tlsCfg != nil {
		ln, err = tls.Listen("tcp", addr, tlsCfg)
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("auth: listen %s: %w", addr, err)
	}

	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			wg.Wait()
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("auth: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	cuid := s.connUID.Add(1)
	rd := bufio.NewReaderSize(conn, maxLine)

	// Server → Client handshake
	fmt.Fprintf(conn, "VERSION\t%s\t%d\t%d\n", protoName, majorVer, minorVer)
	fmt.Fprintf(conn, "MECH\tPLAIN\t\n")
	fmt.Fprintf(conn, "MECH\tLOGIN\t\n")
	fmt.Fprintf(conn, "SPID\t%d\n", s.pid)
	fmt.Fprintf(conn, "CUID\t%d\n", cuid)
	fmt.Fprintf(conn, "COOKIE\t%s\n", s.cookie)
	fmt.Fprintf(conn, "DONE\n")

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				_ = err
			}
			return
		}
		line = strings.TrimRight(line, "\n")
		fields := strings.Split(line, "\t")
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "CPID":
			// client pid — ignore for now
		case "AUTH":
			s.handleAuth(conn, fields)
		case "CONT":
			// SASL continuation — not needed for PLAIN
		case "CANCEL":
			// cancel pending auth
		}
	}
}

func (s *Server) handleAuth(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		return
	}
	id := fields[1]
	mech := fields[2]

	var service, resp string
	for _, f := range fields[3:] {
		if strings.HasPrefix(f, "service=") {
			service = strings.TrimPrefix(f, "service=")
		}
		if strings.HasPrefix(f, "resp=") {
			resp = strings.TrimPrefix(f, "resp=")
		}
	}
	_ = service

	authzid, authid, password, ok := parsePlain(mech, resp)
	if !ok {
		fmt.Fprintf(conn, "FAIL\t%s\treason=bad-credentials\n", id)
		return
	}
	// Master-user separator workaround (RFC 4616 §2.1 doesn't cover
	// this case; Dovecot's `auth_master_user_separator` does):
	// clients that can't supply authzid encode it as
	// `target<sep>master` inside the authid field. Only honoured
	// when master-users are enabled AND no authzid was given AND
	// a separator is configured.
	//
	// When master-users are disabled at the server level, BOTH
	// the authzid and the separator workaround are ignored — the
	// request routes through the regular passdb chain as if the
	// client had sent a plain `authid\0password` PLAIN response.
	// Indistinguishable from a build without master support.
	target := ""
	master := authid
	if s.masterUsersEnabled {
		target = authzid
		if target == "" && s.masterUserSeparator != "" {
			if m, t := SplitMasterFromAuthid(authid, s.masterUserSeparator); t != "" {
				master = m
				target = t
			}
		}
	}

	res, err := s.authenticate(target, master, password, service)
	if err != nil || res == nil || res.Result != AuthOK {
		// Timing-leak mitigation: hold the FAIL reply for a
		// configured duration so unknown-user / wrong-password /
		// malformed-SASL paths all surface in the same wall-clock
		// time. Internal failures use a separate (typically
		// shorter) delay so a passdb outage doesn't amplify into
		// user-facing latency.
		isInternal := err != nil || (res != nil && res.Result == AuthTempFail)
		delay := s.failureDelay
		if isInternal {
			delay = s.internalFailureDelay
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		if isInternal {
			fmt.Fprintf(conn, "FAIL\t%s\ttemp_fail\n", id)
		} else {
			fmt.Fprintf(conn, "FAIL\t%s\n", id)
		}
		// Audit log on failure — kept server-side only; the wire
		// reply already stripped any reason text so the attacker
		// cannot distinguish unknown-user from wrong-password
		// through the protocol. Logs ARE allowed to be specific
		// because they don't reach the client.
		slog.Info("auth: fail",
			"service", service,
			"user", master,
			"master_user_target", target,
			"err", err,
		)
		return
	}

	reply := buildAuthOK(id, res)
	fmt.Fprintln(conn, reply)

	// Audit log on success. master_user is empty for a regular
	// login, set to the master's identity on impersonation.
	var loggedMaster string
	if res.Fields != nil {
		loggedMaster, _ = res.Fields.Get("master_user")
	}
	slog.Info("auth: ok",
		"service", service,
		"user", res.Username,
		"master_user", loggedMaster,
	)
}

// buildAuthOK renders the OK response. When res.Fields is set, the
// reply is emitted from the bag with prefix gating — auth_* entries
// are stripped, userdb_* entries are passed through verbatim, the
// rest land as `key=value` tokens. When Fields is nil, the legacy
// typed-fields path runs so pre-AUTH-2 SQL passdbs see no wire
// change.
//
// Phase AUTH-2 PR 2 will switch the SQL passdb to populate Fields
// directly and drop the typed-fields fallback; this function is
// the single place that needs to change at that point.
func buildAuthOK(id string, res *AuthResponse) string {
	reply := fmt.Sprintf("OK\t%s\tuser=%s", id, res.Username)
	if res.Fields != nil && res.Fields.Len() > 0 {
		for _, tok := range res.Fields.WireForm() {
			if strings.HasPrefix(tok, "user=") {
				// res.Username already emitted; the bag carrying
				// `user=` is the authoritative source if it
				// disagrees, but suppressing the second emission
				// keeps the wire well-formed.
				continue
			}
			reply += "\t" + tok
		}
		return reply
	}
	if res.Home != "" {
		reply += "\thome=" + res.Home
	}
	if res.MailLoc != "" {
		reply += "\tmail=" + res.MailLoc
	}
	return reply
}

// authenticate runs the configured auth chains against the
// supplied credentials and projects the chain-internal Result onto
// the wire-shaped AuthResponse handleAuth knows how to serialise.
//
// When target is empty (no impersonation) the call goes through
// RunAuth: passdb chain → userdb enrich. When target is non-empty
// and ≠ master (impersonation request) the call routes through
// RunMasterAuth: masterdb / passdb master_user flag → switch
// identity → userdb for target.
func (s *Server) authenticate(target, master, password, service string) (*AuthResponse, error) {
	req := &Request{
		Username: master,
		Password: password,
		Service:  service,
		Fields:   NewFields(),
	}
	var (
		result Result
		err    error
	)
	if target != "" && target != master {
		result, err = RunMasterAuth(Chain(s.passdbs), Chain(s.masterdb), s.userdb, target, req)
	} else {
		result, err = RunAuth(Chain(s.passdbs), s.userdb, req)
	}
	resp := &AuthResponse{
		Result: authResultFromChain(result),
		Fields: req.Fields,
	}
	if v, ok := req.Fields.Get("user"); ok {
		resp.Username = v
	} else {
		resp.Username = req.Username
	}
	if v, ok := req.Fields.Get("home"); ok {
		resp.Home = v
	}
	if v, ok := req.Fields.Get("mail"); ok {
		resp.MailLoc = v
	}
	return resp, err
}

// parsePlain decodes a SASL PLAIN response (RFC 4616) into its
// three logical fields. The wire format is
// `authzid\0authid\0passwd` — base64 already decoded by the
// caller (yarilo-auth's AUTH command transports the response
// pre-decoded inside the `resp=` field).
//
//   - authzid — the user the caller wants to log in AS. When
//     non-empty and different from authid, this is a master-user
//     impersonation request — see RunMasterAuth.
//   - authid  — the user supplying the password (the master in
//     an impersonation request, the regular user otherwise).
//   - password — the master's / user's password.
//
// LOGIN mech (legacy) does not carry authzid; both the two-field
// and three-field PLAIN shapes are accepted so a client that
// elides the empty leading authzid still works.
func parsePlain(mech, resp string) (authzid, authid, password string, ok bool) {
	if mech == "PLAIN" || mech == "LOGIN" {
		parts := strings.SplitN(resp, "\x00", 3)
		if len(parts) == 3 {
			return parts[0], parts[1], parts[2], true
		}
		if len(parts) == 2 {
			return "", parts[0], parts[1], true
		}
	}
	return "", "", "", false
}

// SplitMasterFromAuthid extracts a (master, target) pair from an
// authid that uses the `target<sep>master` workaround for clients
// that cannot supply authzid in the PLAIN response (older Outlook,
// some mobile clients). Returns ("", authid) when sep is empty or
// not found — authid passes through unchanged.
//
// Mirrors Dovecot's `auth_master_user_separator` (default `*`)
// from auth/auth-request-fields.c: a SASL response of the form
// `\0target*master\0password` is equivalent to the standards-
// compliant `target\0master\0password`.
func SplitMasterFromAuthid(authid, sep string) (master, target string) {
	if sep == "" {
		return authid, ""
	}
	idx := strings.Index(authid, sep)
	if idx < 0 {
		return authid, ""
	}
	target = authid[:idx]
	master = authid[idx+len(sep):]
	if target == "" || master == "" {
		// Bare separator or empty side — treat as no split.
		return authid, ""
	}
	return master, target
}
