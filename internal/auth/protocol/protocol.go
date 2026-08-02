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

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const (
	majorVer = 1
	minorVer = 1
	maxLine  = 16384
)

// AuthResult is the outcome of a single authentication attempt.
type AuthResult int

const (
	AuthOK AuthResult = iota
	AuthFail
	AuthTempFail
)

// AuthResponse is the final response sent back to the client session.
// When Fields is set the OK reply is emitted from the bag; when nil,
// the legacy typed members drive the wire output.
type AuthResponse struct {
	Result   AuthResult
	Username string
	Home     string
	MailLoc  string
	// Groups is the supplementary group list from the userdb
	// `groups=` field, used by ACL group= matching.
	Groups []string

	// ACLUser / ACLGroups override the identity used for ACL
	// evaluation. Empty ACLUser means evaluate as Username / Groups.
	ACLUser   string
	ACLGroups []string

	// QuotaRules holds per-user quota rules (`*:storage=5G`).
	QuotaRules []string

	// QuotaOverFlag is the external "over quota" marker compared
	// against quota_over_status_mask at login.
	QuotaOverFlag string

	// DirectorTag is the per-user director backend tag. Empty means
	// the login component's static director_tag applies.
	DirectorTag string

	// VolatileDir is the VOLATILEDIR mail-location modifier, raw
	// template (%u/%n/%d/%h unexpanded).
	VolatileDir string

	// IndexDir is the INDEX= mail-location modifier, raw template.
	// When set, per-folder index files live here.
	IndexDir string

	// ControlDir is the CONTROL= mail-location modifier, raw template.
	// When set, per-folder control files live here.
	ControlDir string

	// AltDir is the ALT= mail-location modifier, raw template.
	// When set, cold-tiered messages live under it.
	AltDir string

	// MailPath is the base path of the mail storage tree, raw
	// template. Empty falls back to Home.
	MailPath string

	// InboxPath overrides the INBOX location, raw template.
	// Empty defaults to MailPath.
	InboxPath string

	Proxy bool
	Host  string
	Port  int

	// Fields carries the passdb result as a key/value bag with
	// prefix-derived scoping (see fields.go). Nil falls back to
	// the typed-fields wire path.
	Fields *Fields
}

// Request bundles the passdb inputs plus the shared Fields bag the
// chain mutates. The chain isolates each driver's mutations via
// Snapshot / Rollback on ResultNext.
type Request struct {
	Username string
	Password string
	Service  string
	// RemoteIP is the client IP (no port); may be empty. Drivers
	// enforcing allow_nets must skip the check when empty.
	RemoteIP string

	// Fields accumulates passdb output. Drivers may assume non-nil;
	// Chain.Authenticate allocates it.
	Fields *Fields
}

// Result classifies a passdb outcome. Separate from AuthResult
// because ResultNext is chain-internal and never reaches the wire.
type Result int

const (
	// ResultOK — user verified, chain stops.
	ResultOK Result = iota
	// ResultFail — credentials rejected, chain stops.
	ResultFail
	// ResultNext — user unknown here; roll back and try next driver.
	ResultNext
	// ResultTempFail — backend failure; always with a non-nil error.
	ResultTempFail
)

// Passdb is implemented by passdb backends. Drivers mutate
// req.Fields directly; error accompanies ResultTempFail only.
// The wire never carries error text, only the temp_fail marker.
type Passdb interface {
	Authenticate(req *Request) (Result, error)
}

// Authenticator is the in-process surface session processes use to
// verify credentials without going over the wire. Wraps a Passdb
// chain and returns a wire-shaped AuthResponse.
type Authenticator interface {
	Authenticate(username, password, service, remoteIP string) (*AuthResponse, error)
}

// MasterAuthenticator extends Authenticator with master-user
// impersonation (SASL PLAIN authzid). Sessions type-assert into
// this interface; backends without it fall back to requiring
// authzid == authid. authzid is the target identity, authid the
// user supplying the password.
type MasterAuthenticator interface {
	AuthenticateMaster(authzid, authid, password, service, remoteIP string) (*AuthResponse, error)
}

// SCRAMSha256Lookup exposes per-user SCRAM-SHA-256 verifiers to the
// SASL mech; implementing it enables SCRAM-SHA-256(-PLUS)
// advertisement. Returns (creds, nil) when a verifier exists,
// (nil, nil) when the user is unknown or has no SCRAM verifier —
// the mech then fabricates a fake verifier so user existence can't
// be probed — and (nil, err) on transient backend error.
type SCRAMSha256Lookup interface {
	LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error)
}

// SCRAMSha1Lookup is the SHA-1 counterpart of SCRAMSha256Lookup,
// for legacy clients that only speak SCRAM-SHA-1.
type SCRAMSha1Lookup interface {
	LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error)
}

// AuthenticatorOption tunes a NewAuthenticator construction.
type AuthenticatorOption func(*chainAuthenticator)

// WithAuthenticatorMasterdb attaches a dedicated masterdb chain,
// consulted before the main passdb's per-user `master_user=yes` flag.
func WithAuthenticatorMasterdb(passdbs []Passdb) AuthenticatorOption {
	return func(a *chainAuthenticator) { a.masterdb = passdbs }
}

// WithAuthenticatorMasterUserSeparator enables the `target<sep>master`
// workaround for clients that cannot send authzid. Empty disables it.
func WithAuthenticatorMasterUserSeparator(sep string) AuthenticatorOption {
	return func(a *chainAuthenticator) { a.masterUserSeparator = sep }
}

// WithAuthenticatorMasterUsers is the master-user opt-in. When
// false (the default) NewAuthenticator returns an Authenticator-only
// wrapper, so MasterAuthenticator type-asserts fail and any distinct
// authzid is rejected before the chain runs.
func WithAuthenticatorMasterUsers(enabled bool) AuthenticatorOption {
	return func(a *chainAuthenticator) { a.masterUsersEnabled = enabled }
}

// WithAuthenticatorCache attaches an auth cache, consulted before
// the chain; misses seed it with the result. Nil disables caching.
func WithAuthenticatorCache(c *Cache) AuthenticatorOption {
	return func(a *chainAuthenticator) { a.cache = c }
}

// NewAuthenticator wraps Passdb drivers into the Authenticator
// surface. Passdb-only; userdb lookups happen via the master
// protocol. Unless WithAuthenticatorMasterUsers(true) is given the
// result deliberately does not implement MasterAuthenticator.
func NewAuthenticator(passdbs []Passdb, opts ...AuthenticatorOption) Authenticator {
	a := &chainAuthenticator{chain: Chain(passdbs)}
	for _, opt := range opts {
		opt(a)
	}
	if !a.masterUsersEnabled {
		// hide AuthenticateMaster so session-side type-asserts fail
		return &plainOnlyAuthenticator{inner: a}
	}
	return a
}

// plainOnlyAuthenticator hides AuthenticateMaster when master-users
// are disabled. The inner field is named, not embedded, so the
// method set exposes only what is redeclared here.
type plainOnlyAuthenticator struct{ inner *chainAuthenticator }

func (p *plainOnlyAuthenticator) Authenticate(username, password, service, remoteIP string) (*AuthResponse, error) {
	return p.inner.Authenticate(username, password, service, remoteIP)
}

// LookupSCRAMSha256 forwards the SCRAM lookup: SCRAM is orthogonal
// to master-user impersonation and must survive the wrapper.
func (p *plainOnlyAuthenticator) LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error) {
	return p.inner.LookupSCRAMSha256(username)
}

// LookupSCRAMSha1 is the SHA-1 counterpart of LookupSCRAMSha256.
func (p *plainOnlyAuthenticator) LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error) {
	return p.inner.LookupSCRAMSha1(username)
}

type chainAuthenticator struct {
	chain               Chain
	masterdb            []Passdb
	masterUserSeparator string
	masterUsersEnabled  bool
	cache               *Cache
}

func (c *chainAuthenticator) Authenticate(username, password, service, remoteIP string) (*AuthResponse, error) {
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
		RemoteIP: remoteIP,
		Fields:   NewFields(),
	}
	result, err := RunAuth(c.chain, req)

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
	resp.ACLUser = extractACLUser(req.Fields)
	resp.ACLGroups = extractACLGroups(req.Fields)
	resp.QuotaRules = extractQuotaRules(req.Fields)
	resp.QuotaOverFlag = extractQuotaOverFlag(req.Fields)
	resp.DirectorTag = extractDirectorTag(req.Fields)
	resp.VolatileDir = extractVolatileDir(req.Fields)
	resp.IndexDir = extractIndexDir(req.Fields)
	resp.ControlDir = extractControlDir(req.Fields)
	resp.AltDir = extractAltDir(req.Fields)
	resp.MailPath = extractMailPath(req.Fields)
	resp.InboxPath = extractInboxPath(req.Fields)
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
		resp.ACLUser = extractACLUser(entry.Fields)
		resp.ACLGroups = extractACLGroups(entry.Fields)
		resp.QuotaRules = extractQuotaRules(entry.Fields)
		resp.QuotaOverFlag = extractQuotaOverFlag(entry.Fields)
		resp.DirectorTag = extractDirectorTag(entry.Fields)
		resp.VolatileDir = extractVolatileDir(entry.Fields)
		resp.IndexDir = extractIndexDir(entry.Fields)
		resp.ControlDir = extractControlDir(entry.Fields)
		resp.AltDir = extractAltDir(entry.Fields)
		resp.MailPath = extractMailPath(entry.Fields)
		resp.InboxPath = extractInboxPath(entry.Fields)
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
func (c *chainAuthenticator) AuthenticateMaster(authzid, authid, password, service, remoteIP string) (*AuthResponse, error) {
	// Defence-in-depth: even if a caller obtains the
	// chainAuthenticator directly (bypassing the wrapper
	// NewAuthenticator hands out when master-users are disabled),
	// honour the toggle here too. Distinct authzid against a
	// disabled chain is treated as a regular login of the
	// AUTHID — the safe fallback that never grants impersonation.
	if !c.masterUsersEnabled {
		return c.Authenticate(authid, password, service, remoteIP)
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
		return c.Authenticate(master, password, service, remoteIP)
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
		RemoteIP: remoteIP,
		Fields:   NewFields(),
	}
	result, err := RunMasterAuth(c.chain, Chain(c.masterdb), target, req)

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
	resp.VolatileDir = extractVolatileDir(req.Fields)
	resp.IndexDir = extractIndexDir(req.Fields)
	resp.ControlDir = extractControlDir(req.Fields)
	resp.AltDir = extractAltDir(req.Fields)
	resp.MailPath = extractMailPath(req.Fields)
	resp.InboxPath = extractInboxPath(req.Fields)
	return resp, err
}

// RunAuth executes the passdb chain. passdb is authoritative for
// the auth decision; userdb is never called here. Session processes
// obtain home/mail/quota via a separate USER command on the master
// socket after a successful VERIFY.
func RunAuth(chain Chain, req *Request) (Result, error) {
	if req.Fields == nil {
		req.Fields = NewFields()
	}
	return chain.Authenticate(req)
}

// hasUserdbFields reports whether the bag already carries at
// least one userdb_-prefixed key — the marker for a prefetched
// userdb result that a downstream userdb.Lookup must not overwrite.
// RunMasterAuth handles a master-user impersonation request.
// req.Username MUST be the master when the function is called.
// On success req.Fields carries master_user/original_user/user.
// userdb is not consulted here — session processes call it via
// the master socket after VERIFY.
func RunMasterAuth(passdbs, masterdb Chain, target string, req *Request) (Result, error) {
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
		start := time.Now()
		result, err := db.Authenticate(req)
		observePassdb(driverLabel(db), resultLabel(result, err), start)
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
	tokenStore           TokenStore
	connUID              atomic.Uint64
	pid                  int
	cookie               string
	defaultMailPath      string
	defaultInboxPath     string
	// maxConcurrentRequests bounds in-flight commands per connection (#887).
	// 0 selects DefaultMaxConcurrentRequests.
	maxConcurrentRequests int
}

// ServerOption tunes a NewServer construction.
// DefaultMaxConcurrentRequests bounds how many commands one connection may have
// in flight at once (#887). Reached only under a burst; the read loop then applies
// backpressure by not reading the next command, which is preferable to spawning
// unbounded goroutines.
const DefaultMaxConcurrentRequests = 256

type ServerOption func(*Server)

// WithMaxConcurrentRequests overrides DefaultMaxConcurrentRequests — the number
// of commands a single connection may process concurrently. Values <= 0 restore
// the default.
func WithMaxConcurrentRequests(n int) ServerOption {
	return func(s *Server) {
		if n > 0 {
			s.maxConcurrentRequests = n
		}
	}
}

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
// wire Server consults pre-passdb. Implemented by *warden.Conn so
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
// implementations: warden.PenaltyToSecs (2/4/8/15 cap),
// linear, or capped-linear. Nil falls back to no-sleep.
type PenaltyToSecsFunc func(count int) int

// TokenStore is implemented by authtoken.Store. Abstracted as an interface
// so tests can provide a stub without importing the concrete package.
type TokenStore interface {
	Issue(username, sessionID, service string) (string, error)
	Validate(tok string) (username, sessionID, service string, ok bool)
}

// WithTokenStore attaches the session token store. When set, every successful
// AUTH response carries a "token=<hex>" field that the backend must present
// via VERIFY before entering authenticated state. When nil, no token is issued
// and the VERIFY command always returns FAIL (standalone / test deployments
// that do not need the token handshake).
func WithTokenStore(ts TokenStore) ServerOption {
	return func(s *Server) { s.tokenStore = ts }
}

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

// WithDefaultMailPath sets the cluster-wide mail_path default applied
// when the userdb result carries no explicit mail_path. Supports ~/
// and %u/%n/%d/%h expansion against the resolved home.
func WithDefaultMailPath(p string) ServerOption {
	return func(s *Server) { s.defaultMailPath = p }
}

// WithDefaultInboxPath sets the cluster-wide mail_inbox_path default
// applied when the userdb result carries no explicit mail_inbox_path.
func WithDefaultInboxPath(p string) ServerOption {
	return func(s *Server) { s.defaultInboxPath = p }
}

// applyMailPathDefaults fills MailPath/InboxPath on resp using the
// server-wide defaults when the userdb did not supply per-user values.
func (s *Server) applyMailPathDefaults(resp *AuthResponse) {
	if resp.MailLoc != "" {
		resp.MailLoc = mailbox.ExpandVars(resp.MailLoc, resp.Username)
	}
	if resp.MailPath == "" && s.defaultMailPath != "" {
		mp := mailbox.ExpandHome(s.defaultMailPath, resp.Home)
		mp = strings.ReplaceAll(mp, "%h", resp.Home)
		resp.MailPath = mailbox.ExpandVars(mp, resp.Username)
	}
	if resp.InboxPath == "" && s.defaultInboxPath != "" {
		ip := mailbox.ExpandHome(s.defaultInboxPath, resp.Home)
		ip = strings.ReplaceAll(ip, "%h", resp.Home)
		resp.InboxPath = mailbox.ExpandVars(ip, resp.Username)
	}
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
// Listen binds addr and returns the listener, so a caller can report readiness
// only once the port is accepting. ListenAndServe binds and serves in one call,
// which forces the caller to run it in a goroutine and therefore to announce
// readiness before knowing whether the bind succeeded.
func (s *Server) Listen(addr string, tlsCfg *tls.Config) (net.Listener, error) {
	if tlsCfg != nil {
		ln, err := tls.Listen("tcp", addr, tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("auth: listen %s (tls): %w", addr, err)
		}
		return ln, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("auth: listen %s: %w", addr, err)
	}
	return ln, nil
}

// ListenAndServe binds addr and serves it. Kept for callers that do not need to
// separate the two.
func (s *Server) ListenAndServe(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	ln, err := s.Listen(addr, tlsCfg)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve serves an already-bound listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {

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

// handleConn serves one client connection. Commands are dispatched to their own
// goroutine (#887): a client multiplexes many requests over a single connection
// (#878/#885), and handling them one at a time made the connection a queue.
// Worse, the AUTH path deliberately sleeps — auth-penalty tarpit, policy tarpit,
// timing-leak failure delay — so a single tarpitted request would stall every
// unrelated login queued behind it.
//
// Out-of-order completion is exactly what the wire protocol expects: every reply
// carries the request id it answers, and the client demultiplexes on it.
func (s *Server) handleConn(conn net.Conn) {
	cuid := s.connUID.Add(1)
	connections.Inc()
	connectionsTotal.Inc()
	defer connections.Dec()
	rd := bufio.NewReaderSize(conn, maxLine)

	// Handlers now run concurrently, so every reply must be written atomically
	// or two replies could interleave mid-line on the wire.
	sc := &syncConn{Conn: conn}

	var wg sync.WaitGroup
	// Close only after every in-flight handler has finished, otherwise a
	// handler still holding the tarpit sleep would write to a closed conn.
	defer func() {
		wg.Wait()
		_ = conn.Close()
	}()

	limit := s.maxConcurrentRequests
	if limit <= 0 {
		limit = DefaultMaxConcurrentRequests
	}
	sem := make(chan struct{}, limit)

	// Server → Client handshake. Written before any handler exists, so the
	// plain conn is safe here.
	fmt.Fprintf(conn, "VERSION\t%d\t%d\n", majorVer, minorVer)
	fmt.Fprintf(conn, "MECH\tPLAIN\tplaintext\n")
	fmt.Fprintf(conn, "MECH\tLOGIN\tplaintext\n")
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
		case "AUTH", "VERIFY":
			verb := fields[0]
			// strings.Split allocated a fresh slice for this line, so the
			// goroutine owns it outright.
			args := fields
			// Blocking here is deliberate backpressure: at the bound the read
			// loop stops consuming commands instead of spawning more goroutines.
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				start := time.Now()
				if verb == "AUTH" {
					observeRequest("AUTH", s.handleAuth(sc, args), start)
					return
				}
				observeRequest("VERIFY", s.handleVerify(sc, args), start)
			}()
		case "CONT":
			// SASL continuation — not needed for PLAIN
		case "CANCEL":
			// cancel pending auth
		}
	}
}

// syncConn serialises writes so concurrent handlers cannot interleave two
// replies within one line. Handlers emit a whole line per fmt.Fprintf/Fprintln
// call, which is one Write, so a mutex around Write is all the atomicity the
// wire format needs.
type syncConn struct {
	net.Conn
	mu sync.Mutex
}

func (c *syncConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn.Write(b)
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

// handleAuth processes one AUTH command and returns the metric result label
// for it ("ok" | "fail" | "tempfail" | "bad_request"). The label is returned
// rather than observed here so the caller times the whole verb, including the
// deliberate delays this function applies.
func (s *Server) handleAuth(conn net.Conn, fields []string) string {
	if len(fields) < 3 {
		return "bad_request"
	}
	id := fields[1]
	mech := fields[2]

	var service, resp, ripAttr, sessionID string
	for _, f := range fields[3:] {
		switch {
		case strings.HasPrefix(f, "service="):
			service = strings.TrimPrefix(f, "service=")
		case strings.HasPrefix(f, "resp="):
			resp = strings.TrimPrefix(f, "resp=")
		case strings.HasPrefix(f, "rip="):
			ripAttr = strings.TrimPrefix(f, "rip=")
		case strings.HasPrefix(f, "session="):
			sessionID = strings.TrimPrefix(f, "session=")
		}
	}
	_ = service

	// rip= carries the actual mail-client IP forwarded by the login pod.
	// Use it for penalty tracking instead of the TCP peer (login pod) IP.
	remoteIP := ripAttr
	if remoteIP == "" {
		remoteIP = connRemoteIP(conn)
	}

	authzid, authid, password, ok := parsePlain(mech, resp)
	if !ok {
		slog.Debug("auth: bad SASL response format", "id", id, "mech", mech)
		fmt.Fprintf(conn, "FAIL\t%s\treason=bad-credentials\n", id)
		return "bad_request"
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
				"sid", sessionID, "proto", service, "user", master, "msg", d.Message, "result", "fail")
			if s.failureDelay > 0 {
				time.Sleep(s.failureDelay)
			}
			fmt.Fprintf(conn, "FAIL\t%s\n", id)
			// Pre-chain reject IS the result — report it so the
			// policy server's telemetry sees its own decision.
			if s.policy != nil && s.policyMode.ReportAfter {
				go s.policy.ReportAfter(context.Background(), policyReq, false, true)
			}
			return "fail"
		}
		if d.TarpitSecs > 0 {
			time.Sleep(time.Duration(d.TarpitSecs) * time.Second)
		}
	}

	res, err := s.authenticate(target, master, password, service, remoteIP)

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
			fmt.Fprintf(conn, "FAIL\t%s\tcode=temp_fail\n", id)
		} else {
			fmt.Fprintf(conn, "FAIL\t%s\n", id)
		}
		// Audit log on failure — kept server-side only; the wire
		// reply already stripped any reason text so the attacker
		// cannot distinguish unknown-user from wrong-password
		// through the protocol. Logs ARE allowed to be specific
		// because they don't reach the client.
		slog.Info("auth: fail",
			"sid", sessionID,
			"proto", service,
			"user", master,
			"master_user_target", target,
			"err", err,
			"result", "fail",
		)
		// Policy report on fail too — telemetry needs both sides.
		// policy_reject=false here: any policy-reject path
		// early-returns above with its own ReportAfter call.
		if s.policy != nil && s.policyMode.ReportAfter && target == "" {
			go s.policy.ReportAfter(context.Background(), policyReq, false, false)
		}
		if isInternal {
			return "tempfail"
		}
		return "fail"
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
				"sid", sessionID, "proto", service, "user", res.Username, "msg", d.Message, "result", "fail")
			if s.failureDelay > 0 {
				time.Sleep(s.failureDelay)
			}
			fmt.Fprintf(conn, "FAIL\t%s\n", id)
			// Report this as a failed login for downstream
			// analytics even though the chain accepted.
			if s.policyMode.ReportAfter {
				go s.policy.ReportAfter(context.Background(), policyReq, false, true)
			}
			return "fail"
		}
		if d.TarpitSecs > 0 {
			time.Sleep(time.Duration(d.TarpitSecs) * time.Second)
		}
	}

	reply := buildAuthOK(id, res)
	if s.tokenStore != nil && sessionID != "" {
		if tok, terr := s.tokenStore.Issue(res.Username, sessionID, service); terr == nil {
			reply += "\ttoken=" + tok
		} else {
			slog.Warn("auth: token issue failed", "err", terr)
		}
	}
	fmt.Fprintln(conn, reply)

	// Audit log on success. master_user is empty for a regular
	// login, set to the master's identity on impersonation.
	var loggedMaster string
	if res.Fields != nil {
		loggedMaster, _ = res.Fields.Get("master_user")
	}
	slog.Info("auth: ok",
		"sid", sessionID,
		"proto", service,
		"user", res.Username,
		"master_user", loggedMaster,
		"result", "ok",
	)

	// Policy report (fire-and-forget post-decision telemetry).
	// Goroutine so the wire reply is not blocked on the report
	// HTTP round-trip. policy_reject=false: any policy-reject
	// path early-returns above with its own ReportAfter call.
	if s.policy != nil && s.policyMode.ReportAfter && target == "" {
		go s.policy.ReportAfter(context.Background(), policyReq, true, false)
	}
	return "ok"
}

// handleVerify processes a VERIFY command from a backend pod.
//
// Wire format:
//
//	VERIFY\t<id>\t<token>\tuser=<username>\tsession=<sessionID>\n
//	→ OK\t<id>\tuser=<username>\tsession=<sessionID>\tservice=<service>\n
//	→ FAIL\t<id>\n
//
// The token is one-time. user= and session= are verified against the
// stored token — a mismatch is treated as a FAIL.
// The metric result label is returned for the caller to observe, mirroring
// handleAuth.
func (s *Server) handleVerify(conn net.Conn, fields []string) string {
	if len(fields) < 3 {
		if len(fields) >= 2 {
			fmt.Fprintf(conn, "FAIL\t%s\treason=bad-request\n", fields[1])
		}
		return "bad_request"
	}
	id := fields[1]
	tok := fields[2]

	var claimedUser, claimedSession string
	for _, f := range fields[3:] {
		switch {
		case strings.HasPrefix(f, "user="):
			claimedUser = f[len("user="):]
		case strings.HasPrefix(f, "session="):
			claimedSession = f[len("session="):]
		}
	}

	if s.tokenStore == nil {
		fmt.Fprintf(conn, "FAIL\t%s\treason=not-configured\n", id)
		return "error"
	}
	username, sessionID, service, ok := s.tokenStore.Validate(tok)
	if !ok {
		slog.Info("auth: verify failed: invalid token", "id", id)
		fmt.Fprintf(conn, "FAIL\t%s\n", id)
		return "fail"
	}
	if claimedUser != "" && claimedUser != username {
		slog.Warn("auth: verify failed: user mismatch", "claimed", claimedUser, "token_user", username)
		fmt.Fprintf(conn, "FAIL\t%s\n", id)
		return "fail"
	}
	if claimedSession != "" && claimedSession != sessionID {
		slog.Warn("auth: verify failed: session mismatch", "claimed", claimedSession, "token_session", sessionID)
		fmt.Fprintf(conn, "FAIL\t%s\n", id)
		return "fail"
	}
	slog.Info("auth: verify ok", "user", username, "session", sessionID)
	fmt.Fprintf(conn, "OK\t%s\tuser=%s\tsession=%s\tservice=%s\n", id, username, sessionID, service)
	return "ok"
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
	if res.QuotaOverFlag != "" {
		reply += "\tquota_over_flag=" + res.QuotaOverFlag
	}
	if res.DirectorTag != "" {
		reply += "\tdirector_tag=" + res.DirectorTag
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

// extractACLUser reads the acl_user override from the Fields bag.
func extractACLUser(f *Fields) string {
	if f == nil {
		return ""
	}
	if v, ok := f.Get("userdb_acl_user"); ok && v != "" {
		return v
	}
	if v, ok := f.Get("acl_user"); ok && v != "" {
		return v
	}
	return ""
}

// extractACLGroups reads the acl_groups override from the Fields bag.
func extractACLGroups(f *Fields) []string {
	if f == nil {
		return nil
	}
	if v, ok := f.Get("userdb_acl_groups"); ok && v != "" {
		return SplitCSV(v)
	}
	if v, ok := f.Get("acl_groups"); ok && v != "" {
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

// extractQuotaOverFlag reads the quota_over_flag value from the Fields bag.
// Priority: userdb-scoped userdb_quota_over_flag → bare quota_over_flag.
func extractQuotaOverFlag(f *Fields) string {
	if f == nil {
		return ""
	}
	if v, ok := f.Get("userdb_quota_over_flag"); ok && v != "" {
		return v
	}
	if v, ok := f.Get("quota_over_flag"); ok && v != "" {
		return v
	}
	return ""
}

// extractDirectorTag reads the director_tag value from the Fields bag.
// Priority: userdb-scoped userdb_director_tag → bare director_tag.
func extractDirectorTag(f *Fields) string {
	if f == nil {
		return ""
	}
	if v, ok := f.Get("userdb_director_tag"); ok && v != "" {
		return v
	}
	if v, ok := f.Get("director_tag"); ok && v != "" {
		return v
	}
	return ""
}

// extractVolatileDir reads the VOLATILEDIR value from the Fields bag.
// Priority: explicit volatile_dir= field → VOLATILEDIR= modifier inside
// the mail= location string. Returns the raw template (not yet expanded).
func extractVolatileDir(f *Fields) string {
	if f == nil {
		return ""
	}
	for _, key := range []string{"userdb_volatile_dir", "volatile_dir"} {
		if v, ok := f.Get(key); ok && v != "" {
			return v
		}
	}
	for _, key := range []string{"userdb_mail", "mail"} {
		if v, ok := f.Get(key); ok && v != "" {
			if vd := parseMailLocationMod(v, "VOLATILEDIR"); vd != "" {
				return vd
			}
		}
	}
	return ""
}

// extractIndexDir reads the INDEX= value from the Fields bag.
// Priority: explicit index_dir= field → INDEX= modifier inside the
// mail= location string. Returns the raw template (not yet expanded).
func extractIndexDir(f *Fields) string {
	if f == nil {
		return ""
	}
	for _, key := range []string{"userdb_index_dir", "index_dir"} {
		if v, ok := f.Get(key); ok && v != "" {
			return v
		}
	}
	for _, key := range []string{"userdb_mail", "mail"} {
		if v, ok := f.Get(key); ok && v != "" {
			if id := parseMailLocationMod(v, "INDEX"); id != "" {
				return id
			}
		}
	}
	return ""
}

// extractControlDir reads the CONTROL= value from the Fields bag.
// Priority: explicit control_dir= field → CONTROL= modifier inside
// the mail= location string. Returns the raw template (not yet expanded).
func extractControlDir(f *Fields) string {
	if f == nil {
		return ""
	}
	for _, key := range []string{"userdb_control_dir", "control_dir"} {
		if v, ok := f.Get(key); ok && v != "" {
			return v
		}
	}
	for _, key := range []string{"userdb_mail", "mail"} {
		if v, ok := f.Get(key); ok && v != "" {
			if cd := parseMailLocationMod(v, "CONTROL"); cd != "" {
				return cd
			}
		}
	}
	return ""
}

// extractMailPath reads the mail_path value from the Fields bag.
// Priority: explicit mail_path= field → base path extracted from the
// mail= location string ("driver:PATH[:modifiers]"). Returns the raw
// template (not yet expanded).
func extractMailPath(f *Fields) string {
	if f == nil {
		return ""
	}
	for _, key := range []string{"userdb_mail_path", "mail_path"} {
		if v, ok := f.Get(key); ok && v != "" {
			return v
		}
	}
	for _, key := range []string{"userdb_mail", "mail"} {
		if v, ok := f.Get(key); ok && v != "" {
			if parts := strings.SplitN(v, ":", 3); len(parts) >= 2 && parts[1] != "" {
				return parts[1]
			}
		}
	}
	return ""
}

// extractInboxPath reads the mail_inbox_path value from the Fields bag.
func extractInboxPath(f *Fields) string {
	if f == nil {
		return ""
	}
	for _, key := range []string{"userdb_mail_inbox_path", "mail_inbox_path"} {
		if v, ok := f.Get(key); ok && v != "" {
			return v
		}
	}
	return ""
}

// extractAltDir reads the ALT= value from the Fields bag.
// Priority: explicit alt_dir= field → ALT= modifier inside the
// mail= location string. Returns the raw template (not yet expanded).
func extractAltDir(f *Fields) string {
	if f == nil {
		return ""
	}
	for _, key := range []string{"userdb_alt_dir", "alt_dir"} {
		if v, ok := f.Get(key); ok && v != "" {
			return v
		}
	}
	for _, key := range []string{"userdb_mail", "mail"} {
		if v, ok := f.Get(key); ok && v != "" {
			if ad := parseMailLocationMod(v, "ALT"); ad != "" {
				return ad
			}
		}
	}
	return ""
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
func (s *Server) authenticate(target, master, password, service, remoteIP string) (*AuthResponse, error) {
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
		resp := responseFromCache(retUser, entry)
		s.applyMailPathDefaults(resp)
		return resp, nil
	}

	req := &Request{
		Username: master,
		Password: password,
		Service:  service,
		RemoteIP: remoteIP,
		Fields:   NewFields(),
	}
	var (
		result Result
		err    error
	)
	if target != "" && target != master {
		result, err = RunMasterAuth(Chain(s.passdbs), Chain(s.masterdb), target, req)
	} else {
		result, err = RunAuth(Chain(s.passdbs), req)
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
	resp.MailPath = extractMailPath(req.Fields)
	resp.InboxPath = extractInboxPath(req.Fields)
	s.applyMailPathDefaults(resp)
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
