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

	"github.com/emersion/go-sasl"
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
	// Groups is the list of supplementary groups the user belongs to,
	// sourced from the userdb `groups=` extra field. Used by ACL
	// evaluation to match `group=` and `group-override=` entries.
	// Empty when not configured — group= ACL entries have no effect.
	Groups []string

	// QuotaRules is the list of per-user quota rules sourced from the
	// userdb `quota_rule=` extra field. Format: `*:storage=5G`.
	QuotaRules []string
	Proxy      bool
	Host       string
	Port       int

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

// SCRAMSha256Lookup exposes per-user SCRAM-SHA-256 verifiers to
// the session-layer SASL mech. Implementing this interface on the
// configured Authenticator (and on individual passdbs that carry
// verifiers in their backing store) lights up SCRAM-SHA-256 and
// SCRAM-SHA-256-PLUS advertisement in IMAP / POP3 / Submission.
//
// LookupSCRAMSha256 returns:
//
//   - (creds, nil)  — user exists and has a SCRAM-SHA-256 verifier.
//     The SASL mech drives challenge-response from
//     the returned StoredKey / ServerKey without
//     ever seeing a plain password.
//   - (nil, nil)    — user unknown OR stored credential is not a
//     SCRAM verifier. The SASL mech fabricates a
//     fake verifier so the exchange completes with
//     a uniform auth-failed outcome and an attacker
//     cannot probe for user existence via timing.
//   - (nil, err)    — transient backend error. The session
//     surfaces this as temp_fail.
//
// Verifiers are produced by `yarilo-admin auth scram-verifier`
// (or any tool that emits the
// `{SCRAM-SHA-256}iter,salt,stored,server` blob).
type SCRAMSha256Lookup interface {
	LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error)
}

// SCRAMSha1Lookup is the SHA-1 counterpart of SCRAMSha256Lookup.
// Same semantics, same (creds, nil) / (nil, nil) / (nil, err)
// contract; only the digest family of the returned verifier
// differs. Provided for compatibility with legacy clients that
// only speak SCRAM-SHA-1 — new deployments should provision
// SCRAM-SHA-256 verifiers instead.
type SCRAMSha1Lookup interface {
	LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error)
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

// WithAuthenticatorCache attaches an auth cache. When set, every
// Authenticate / AuthenticateMaster call first consults the cache
// (key = `<service>\t<username>`) and short-circuits on hit.
// Misses run the full chain and seed the cache with the result.
// Nil cache = caching disabled (every call goes to the chain).
//
// The cache is keyed by the passdb-side username; in a master-user
// flow the master and target produce independent cache entries.
func WithAuthenticatorCache(c *Cache) AuthenticatorOption {
	return func(a *chainAuthenticator) { a.cache = c }
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

// LookupSCRAMSha256 forwards the SCRAM lookup so the wrapper
// (returned when master-users are disabled) still exposes
// SCRAM-SHA-256 support. SCRAM is orthogonal to master-user
// impersonation — disabling one must not disable the other.
func (p *plainOnlyAuthenticator) LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error) {
	return p.inner.LookupSCRAMSha256(username)
}

// LookupSCRAMSha1 is the SHA-1 counterpart of LookupSCRAMSha256.
func (p *plainOnlyAuthenticator) LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error) {
	return p.inner.LookupSCRAMSha1(username)
}

type chainAuthenticator struct {
	chain               Chain
	userdb              Userdb
	masterdb            []Passdb
	masterUserSeparator string
	masterUsersEnabled  bool
	cache               *Cache
}

func (c *chainAuthenticator) Authenticate(username, password, service string) (*AuthResponse, error) {
	// Cache lookup — positive hit verifies the supplied password
	// against the stored HMAC and short-circuits the chain.
	// Negative hit short-circuits without password check: a
	// previously-failed lookup is authoritative for the neg-TTL
	// window.
	key := MakeCacheKey(service, username)
	if entry, ok := c.cache.Lookup(key, password); ok {
		return responseFromCache(username, entry), nil
	}

	req := &Request{
		Username: username,
		Password: password,
		Service:  service,
		Fields:   NewFields(),
	}
	result, err := RunAuth(c.chain, c.userdb, req)

	// Seed the cache on definitive answers. TempFail is not
	// cached — the next attempt should retry the chain so a
	// transient backend outage does not lock users out for the
	// neg-TTL window.
	if err == nil && (result == ResultOK || result == ResultFail) {
		c.cache.Insert(key, username, password, result, req.Fields)
	}

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
	resp.Groups = extractGroups(req.Fields)
	resp.QuotaRules = extractQuotaRules(req.Fields)
	return resp, err
}

// responseFromCache rebuilds the AuthResponse from a cached
// CacheEntry. Username/Home/MailLoc are pulled from the bag
// (matching the chain's natural population) so a cache hit is
// byte-identical to a fresh chain run for the same input.
func responseFromCache(reqUser string, entry *CacheEntry) *AuthResponse {
	resp := &AuthResponse{
		Result: authResultFromChain(entry.Result),
		Fields: entry.Fields,
	}
	if entry.Fields != nil {
		if v, ok := entry.Fields.Get("user"); ok {
			resp.Username = v
		}
		if v, ok := entry.Fields.Get("home"); ok {
			resp.Home = v
		}
		if v, ok := entry.Fields.Get("mail"); ok {
			resp.MailLoc = v
		}
		resp.Groups = extractGroups(entry.Fields)
		resp.QuotaRules = extractQuotaRules(entry.Fields)
	}
	if resp.Username == "" {
		resp.Username = reqUser
	}
	return resp
}

// AuthenticateMaster implements MasterAuthenticator. Routing
// matches the wire Server.authenticate:
//   - authzid empty (no impersonation) → RunAuth path.
//   - authzid set and == authid → caller asked to log in as self;
//     treat as regular login (RFC 4616 permits this).
//   - authzid set and != authid → RunMasterAuth path.
//
// LookupSCRAMSha256 walks the passdb chain and returns the first
// SCRAM-SHA-256 verifier any chain entry exposes. A passdb that
// does not implement SCRAMSha256Lookup is silently skipped — the
// chain stays mixed-mode (SQL passdbs with PLAIN columns coexist
// with SQL passdbs carrying SCRAM verifiers; only the latter
// contribute to SCRAM authentication).
//
// Returns (nil, nil) when no chain entry has a verifier for the
// user — the session-side SASL server then fabricates a fake
// verifier so the SCRAM exchange completes with a uniform
// auth-failed outcome (defence against user-enumeration via
// timing). Backend errors propagate.
func (c *chainAuthenticator) LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error) {
	for _, db := range c.chain {
		lookup, ok := db.(SCRAMSha256Lookup)
		if !ok {
			continue
		}
		creds, err := lookup.LookupSCRAMSha256(username)
		if err != nil {
			return nil, err
		}
		if creds != nil {
			return creds, nil
		}
	}
	return nil, nil
}

// LookupSCRAMSha1 mirrors LookupSCRAMSha256 for the SHA-1 family.
// Independent of SHA-256 — a passdb may carry SCRAM-SHA-256 for
// one user and SCRAM-SHA-1 for another, and the chain walks both
// without coupling.
func (c *chainAuthenticator) LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error) {
	for _, db := range c.chain {
		lookup, ok := db.(SCRAMSha1Lookup)
		if !ok {
			continue
		}
		creds, err := lookup.LookupSCRAMSha1(username)
		if err != nil {
			return nil, err
		}
		if creds != nil {
			return creds, nil
		}
	}
	return nil, nil
}

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

	// Master-flow caching: key the cache by (service, master,
	// target) tuple so each (master impersonating target) pair
	// caches independently. Cached under the master's name for
	// selective flush — revoking a master's privileges then
	// `auth cache flush <master>` evicts every impersonation
	// entry in one sweep. Stale-privilege risk is bounded by the
	// configured TTL plus the audit log.
	mkey := MakeCacheKey(service, "M:"+master+"\t"+target)
	if entry, ok := c.cache.Lookup(mkey, password); ok {
		return responseFromCache(target, entry), nil
	}

	req := &Request{
		Username: master,
		Password: password,
		Service:  service,
		Fields:   NewFields(),
	}
	result, err := RunMasterAuth(c.chain, Chain(c.masterdb), c.userdb, target, req)

	if err == nil && (result == ResultOK || result == ResultFail) {
		// Cache under master's name (not target) for selective
		// flush — admin revoking a master's privileges flushes
		// by master, and all their impersonation cache entries
		// drop in one sweep.
		c.cache.Insert(mkey, master, password, result, req.Fields)
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

// RunAuth executes the passdb chain and — on ResultOK with a
// userdb configured — also fills the bag with userdb_* fields
// from a userdb.Lookup. Prefetch detection: when the chain has
// already populated any userdb_-prefixed key (an SQL passdb that
// SELECT-ed userdb columns in the same query), userdb.Lookup is
// skipped — the passdb effectively did the work.
//
// userdb errors and misses do NOT downgrade ResultOK — passdb is
// authoritative for the auth decision. They surface in server-side
// logs only; the response carries passdb-only fields. A transient
// userdb outage must not reject an already-verified user.
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
// with the master's username (echoed on the wire OK reply for
// audit), `original_user` + `login_user` + `user` are set to
// target, and req.Username is updated to target so any
// downstream consumer reading the struct sees the
// post-impersonation identity.
//
// Authorisation model (two equivalent paths):
//
//  1. masterdb chain (if non-empty) authenticates the master
//     with its own password. ResultOK → impersonation allowed.
//  2. Otherwise the main passdb chain authenticates the master;
//     the result must carry `master_user=yes` (truthy per the
//     reserved-field bool spelling) — a per-user master flag.
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
	cache                *Cache
	penalty              PenaltyStore
	penaltyToSecs        PenaltyToSecsFunc
	policy               PolicyChecker
	policyMode           PolicyMode
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
// `master_user=yes` field on the result (per-user master flag).
// Either mechanism grants the impersonation.
func WithMasterdb(passdbs []Passdb) ServerOption {
	return func(s *Server) { s.masterdb = passdbs }
}

// WithMasterUserSeparator enables the `target<sep>master`
// workaround for SASL PLAIN clients that cannot supply authzid
// in the standard third-field position. Typical value is `*`.
// Empty disables the workaround — only RFC 4616 authzid is
// honoured.
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
func WithFailureDelay(d time.Duration) ServerOption {
	return func(s *Server) { s.failureDelay = d }
}

// WithInternalFailureDelay sets the delay applied when the
// failure is internal (passdb backend down, SQL refused, etc).
// Separate from WithFailureDelay because operators often want
// internal-error delays shorter than user-facing ones (the user
// will likely retry and another full failure delay would amplify
// outage symptoms).
func WithInternalFailureDelay(d time.Duration) ServerOption {
	return func(s *Server) { s.internalFailureDelay = d }
}

// PolicyChecker is the policy-server hook surface the wire
// Server consults around the chain run. Implemented by
// *policy.Client (auth/policy package); tests can plug stubs.
//
// Decision shape lives here so protocol does not import the
// policy package — single-direction dependency keeps the build
// graph clean.
type PolicyChecker interface {
	CheckBefore(ctx context.Context, req PolicyRequest) (PolicyDecision, error)
	CheckAfter(ctx context.Context, req PolicyRequest, success, policyReject bool) (PolicyDecision, error)
	ReportAfter(ctx context.Context, req PolicyRequest, success, policyReject bool)
}

// PolicyRequest is the input to a policy call. Mirror of the
// shape policy.Client accepts; defined here so the wire Server
// can call without the policy package import.
type PolicyRequest struct {
	Username  string
	Password  string
	RemoteIP  string
	Service   string
	DeviceID  string
	SessionID string
	TLS       bool
	FailType  string
}

// PolicyDecision is the parsed verdict. Continue=true means
// proceed; Reject=true means refuse; TarpitSecs>0 means sleep
// that many seconds then continue.
type PolicyDecision struct {
	Continue   bool
	Reject     bool
	TarpitSecs int
	Message    string
}

// PolicyMode controls which lifecycle hooks fire. CheckBefore
// runs pre-passdb (block the chain on policy refusal);
// CheckAfter runs post-passdb with the outcome known
// (downgrade-success-to-fail use case); ReportAfter is
// fire-and-forget telemetry.
type PolicyMode struct {
	CheckBefore bool
	CheckAfter  bool
	ReportAfter bool
}

// WithPolicy attaches a policy-server hook. The mode controls
// which lifecycle calls fire. Nil checker disables every hook.
func WithPolicy(checker PolicyChecker, mode PolicyMode) ServerOption {
	return func(s *Server) {
		s.policy = checker
		s.policyMode = mode
	}
}

// PenaltyStore is the cross-process auth-fail backoff store the
// wire Server consults pre-passdb. Implemented by *anvil.Conn so
// every yarilo-auth pod shares the same counters; tests can plug
// in a stub. Nil-safe at the call sites — Lookup returns 0 and
// Update is a no-op when the store is not configured.
type PenaltyStore interface {
	PenaltyLookup(ip string) (int, error)
	PenaltyUpdate(ip string, count int) error
}

// PenaltyToSecsFunc maps the penalty counter returned by
// PenaltyStore.Lookup to the sleep duration applied before the
// chain runs. Caller-supplied so a deployment can tune the
// exponential curve without recompiling protocol — typical
// implementations: anvil.PenaltyToSecs (2/4/8/15 cap),
// linear, or capped-linear. Nil falls back to no-sleep.
type PenaltyToSecsFunc func(count int) int

// WithPenalty attaches a cross-process penalty store. When set,
// handleAuth runs the following dance for every request:
//
//  1. Lookup penalty for the remote IP, sleep
//     toSecs(count) seconds (timing-leak protection equalises
//     fast/slow paths regardless of penalty).
//  2. Run the chain.
//  3. On fail → Update(ip, count+1). On success → Update(ip, 0).
//
// Nil store disables; nil toSecs disables the sleep but still
// records counters (useful for telemetry without enforcement).
func WithPenalty(store PenaltyStore, toSecs PenaltyToSecsFunc) ServerOption {
	return func(s *Server) {
		s.penalty = store
		s.penaltyToSecs = toSecs
	}
}

// WithCache attaches an auth cache. handleAuth consults it
// before running the passdb chain — positive hit verifies the
// supplied password against the stored HMAC, negative hit
// short-circuits without password check. Misses run the chain
// and seed the cache with the result.
//
// Nil cache disables caching. Pass the same *Cache instance to
// WithMasterCache on the matching MasterServer to expose
// CACHE-FLUSH for selective eviction.
func WithCache(c *Cache) ServerOption {
	return func(s *Server) { s.cache = c }
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

// connRemoteIP extracts the client IP from a net.Conn.
// Returns "" when the remote address is missing or non-TCP
// (loopback unix-socket harnesses, mock conns in tests).
func connRemoteIP(conn net.Conn) string {
	addr := conn.RemoteAddr()
	if addr == nil {
		return ""
	}
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.IP.String()
	}
	return ""
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
	// Master-user separator workaround (RFC 4616 §2.1 doesn't
	// cover this case): clients that can't supply authzid encode
	// it as `target<sep>master` inside the authid field. Only
	// honoured when master-users are enabled AND no authzid was
	// given AND a separator is configured.
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

	// Auth-penalty pre-check: look up the client IP's current
	// fail-counter, sleep the mapped seconds, then run the chain.
	// Master-user flows are exempt — admin sessions should never
	// be tarpitted (matches the policy-server exemption).
	remoteIP := connRemoteIP(conn)
	penaltyCount := 0
	if s.penalty != nil && remoteIP != "" && target == "" {
		n, perr := s.penalty.PenaltyLookup(remoteIP)
		if perr != nil {
			slog.Warn("auth: penalty lookup failed", "ip", remoteIP, "err", perr)
		} else {
			penaltyCount = n
			if s.penaltyToSecs != nil {
				if secs := s.penaltyToSecs(n); secs > 0 {
					time.Sleep(time.Duration(secs) * time.Second)
				}
			}
		}
	}

	// Policy-server pre-check (check_before_auth). Master flows
	// are exempt (admin sessions bypass policy decisions). On
	// Reject → opaque FAIL like wrong-password. On TarpitSecs →
	// sleep then continue to the chain. Errors honour the
	// policy client's RejectOnFail setting internally; here we
	// only see the resulting Decision.
	policyReq := PolicyRequest{
		Username: master, Password: password, RemoteIP: remoteIP,
		Service: service,
	}
	if s.policy != nil && s.policyMode.CheckBefore && target == "" {
		d, perr := s.policy.CheckBefore(context.Background(), policyReq)
		if perr != nil {
			slog.Warn("auth: policy CheckBefore error", "err", perr)
		}
		if d.Reject {
			slog.Info("auth: policy rejected pre-chain",
				"service", service, "user", master, "msg", d.Message)
			if s.failureDelay > 0 {
				time.Sleep(s.failureDelay)
			}
			fmt.Fprintf(conn, "FAIL\t%s\n", id)
			// Pre-chain reject IS the result — report it so the
			// policy server's telemetry sees its own decision.
			if s.policy != nil && s.policyMode.ReportAfter {
				go s.policy.ReportAfter(context.Background(), policyReq, false, true)
			}
			return
		}
		if d.TarpitSecs > 0 {
			time.Sleep(time.Duration(d.TarpitSecs) * time.Second)
		}
	}

	res, err := s.authenticate(target, master, password, service)

	// Penalty update: success resets to 0; client-visible failure
	// increments by 1 (capped server-side). Internal failures
	// (temp_fail) do NOT update the counter — a passdb outage is
	// not the client's fault and shouldn't lock them out when the
	// backend comes back. Master flows still don't update.
	if s.penalty != nil && remoteIP != "" && target == "" {
		switch {
		case err == nil && res != nil && res.Result == AuthOK:
			if uerr := s.penalty.PenaltyUpdate(remoteIP, 0); uerr != nil {
				slog.Warn("auth: penalty reset failed", "ip", remoteIP, "err", uerr)
			}
		case err == nil && res != nil && res.Result == AuthFail:
			if uerr := s.penalty.PenaltyUpdate(remoteIP, penaltyCount+1); uerr != nil {
				slog.Warn("auth: penalty increment failed", "ip", remoteIP, "err", uerr)
			}
		}
	}

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
		// Policy report on fail too — telemetry needs both sides.
		// policy_reject=false here: any policy-reject path
		// early-returns above with its own ReportAfter call.
		if s.policy != nil && s.policyMode.ReportAfter && target == "" {
			go s.policy.ReportAfter(context.Background(), policyReq, false, false)
		}
		return
	}

	// Policy-server post-chain check (check_after_auth). The
	// policy server now has the success outcome; it may downgrade
	// to fail (e.g. account-takeover detection). Master flows
	// exempt as before. Errors stay non-fatal — CheckAfter's own
	// failover-decision honours RejectOnFail.
	if s.policy != nil && s.policyMode.CheckAfter && target == "" {
		d, perr := s.policy.CheckAfter(context.Background(), policyReq, true, false)
		if perr != nil {
			slog.Warn("auth: policy CheckAfter error", "err", perr)
		}
		if d.Reject {
			slog.Info("auth: policy rejected post-chain",
				"service", service, "user", res.Username, "msg", d.Message)
			if s.failureDelay > 0 {
				time.Sleep(s.failureDelay)
			}
			fmt.Fprintf(conn, "FAIL\t%s\n", id)
			// Report this as a failed login for downstream
			// analytics even though the chain accepted.
			if s.policyMode.ReportAfter {
				go s.policy.ReportAfter(context.Background(), policyReq, false, true)
			}
			return
		}
		if d.TarpitSecs > 0 {
			time.Sleep(time.Duration(d.TarpitSecs) * time.Second)
		}
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

	// Policy report (fire-and-forget post-decision telemetry).
	// Goroutine so the wire reply is not blocked on the report
	// HTTP round-trip. policy_reject=false: any policy-reject
	// path early-returns above with its own ReportAfter call.
	if s.policy != nil && s.policyMode.ReportAfter && target == "" {
		go s.policy.ReportAfter(context.Background(), policyReq, true, false)
	}
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
	if len(res.Groups) > 0 {
		reply += "\tgroups=" + strings.Join(res.Groups, ",")
	}
	if len(res.QuotaRules) > 0 {
		reply += "\tquota_rule=" + strings.Join(res.QuotaRules, ",")
	}
	return reply
}

// extractGroups reads the supplementary groups from an auth Fields
// bag. The userdb path stores them as "userdb_groups=a,b,c"; the
// passdb direct path (pre-AUTH-2 SQL or static) stores them as
// "groups=a,b,c". Both are accepted.
func extractGroups(f *Fields) []string {
	if f == nil {
		return nil
	}
	if v, ok := f.Get("userdb_groups"); ok && v != "" {
		return SplitCSV(v)
	}
	if v, ok := f.Get("groups"); ok && v != "" {
		return SplitCSV(v)
	}
	return nil
}

// extractQuotaRules reads quota_rule entries from the Fields bag.
// The userdb serialises multiple quota_rule= values joined by comma
// under "userdb_quota_rule"; the passdb direct path uses "quota_rule".
func extractQuotaRules(f *Fields) []string {
	if f == nil {
		return nil
	}
	if v, ok := f.Get("userdb_quota_rule"); ok && v != "" {
		return SplitCSV(v)
	}
	if v, ok := f.Get("quota_rule"); ok && v != "" {
		return SplitCSV(v)
	}
	return nil
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
	// Cache key shape mirrors chainAuthenticator so the in-process
	// and wire paths stay interchangeable for the same input.
	// Regular flow: `<service>\t<user>`. Master flow: prefixed
	// with `M:` and the target appended so each (master, target)
	// pair caches independently.
	var key, cacheUser string
	if target != "" && target != master {
		key = MakeCacheKey(service, "M:"+master+"\t"+target)
		cacheUser = master
	} else {
		key = MakeCacheKey(service, master)
		cacheUser = master
	}
	if entry, ok := s.cache.Lookup(key, password); ok {
		retUser := master
		if target != "" {
			retUser = target
		}
		return responseFromCache(retUser, entry), nil
	}

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

	if err == nil && (result == ResultOK || result == ResultFail) {
		s.cache.Insert(key, cacheUser, password, result, req.Fields)
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
// A SASL response of the form `\0target*master\0password` is
// equivalent to the standards-compliant
// `target\0master\0password`.
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
