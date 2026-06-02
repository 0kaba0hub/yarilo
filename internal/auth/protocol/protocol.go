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

// NewAuthenticator wraps one or more Passdb drivers into the
// session-friendly Authenticator surface. Optionally attach a
// userdb via WithAuthenticatorUserdb so the bag returned by every
// successful Authenticate also carries userdb_* fields the
// session path needs for mail-storage setup.
func NewAuthenticator(passdbs []Passdb, opts ...AuthenticatorOption) Authenticator {
	a := &chainAuthenticator{chain: Chain(passdbs)}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

type chainAuthenticator struct {
	chain  Chain
	userdb Userdb
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
// responses with userdb_* fields (Phase AUTH-2 PR 3).
type Server struct {
	passdbs []Passdb
	userdb  Userdb
	connUID atomic.Uint64
	pid     int
	cookie  string
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

	username, password, ok := parsePlain(mech, resp)
	if !ok {
		fmt.Fprintf(conn, "FAIL\t%s\treason=bad-credentials\n", id)
		return
	}

	res, err := s.authenticate(username, password, service)
	if err != nil || res == nil || res.Result != AuthOK {
		if err != nil || (res != nil && res.Result == AuthTempFail) {
			fmt.Fprintf(conn, "FAIL\t%s\ttemp_fail\n", id)
		} else {
			fmt.Fprintf(conn, "FAIL\t%s\n", id)
		}
		return
	}

	reply := buildAuthOK(id, res)
	fmt.Fprintln(conn, reply)
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

// authenticate runs the configured passdb chain against the
// supplied credentials and projects the chain-internal Result onto
// the wire-shaped AuthResponse handleAuth knows how to serialise.
// All bag fields the chain accumulated land on resp.Fields; the
// typed Username / Home / MailLoc members are mirrored from the
// bag so the pre-PR-2 buildAuthOK fallback path stays usable for
// any future caller that bypasses the bag.
func (s *Server) authenticate(username, password, service string) (*AuthResponse, error) {
	req := &Request{
		Username: username,
		Password: password,
		Service:  service,
		Fields:   NewFields(),
	}
	result, err := RunAuth(Chain(s.passdbs), s.userdb, req)
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

// parsePlain decodes PLAIN SASL or treats resp as "user\0pass" (LOGIN).
func parsePlain(mech, resp string) (username, password string, ok bool) {
	// PLAIN: authzid\0authid\0passwd (base64 already decoded by client field)
	if mech == "PLAIN" || mech == "LOGIN" {
		parts := strings.SplitN(resp, "\x00", 3)
		if len(parts) == 3 {
			return parts[1], parts[2], true
		}
		if len(parts) == 2 {
			return parts[0], parts[1], true
		}
	}
	return "", "", false
}
