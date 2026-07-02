// Package imap wires go-imap/v2 to yarilo's mailbox and index backends.
package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-sasl"
	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/internal/auth/oauth2"
	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/auth/scram"
	"github.com/0kaba0hub/yarilo/internal/connlimit"
	"github.com/0kaba0hub/yarilo/internal/loginproto"
	"github.com/0kaba0hub/yarilo/internal/userstate/specialuse"
	"github.com/0kaba0hub/yarilo/internal/userstate/subs"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Server is the yarilo IMAP server.
type Server struct {
	srv         *imapserver.Server
	opts        Options
	anvilClient *imapAnvilClient
}

// Options configures the IMAP server.
type Options struct {
	Addr               string
	AddrPlain          string
	TLSConfig          *tls.Config
	Mailbox            mailbox.MailboxBackend
	Index              mailbox.IndexBackend
	Resolver           *mailbox.Resolver
	Auth               protocol.Authenticator
	ProxyProtocol      bool
	HAProxyTimeout     time.Duration
	HAProxyTrustedNets []*net.IPNet
	// AuthAddr is the host:port of yarilo-auth login protocol used by the
	// PreambleListener to verify session tokens forwarded by login pods.
	// When set, connections must carry a valid YARILO preamble.
	AuthAddr string
	AuthTLS  *tls.Config
	// MasterAddr is the host:port of yarilo-auth master protocol used by
	// the PreambleListener to perform userdb lookups after token verification.
	MasterAddr         string
	MasterTLS          *tls.Config
	DisablePlainAuth   bool
	IdleNotifyInterval time.Duration
	MaxLineLength      int
	ConnLimit          *connlimit.Limiter
	IDSend             string
	LoginGreeting      string
	LogoutFormat       string
	ClientWorkarounds  imapWorkarounds

	// FailureDelay is the timing-leak mitigation hold applied
	// before returning an auth-failure error to the client.
	// Mirrors Dovecot's auth_failure_delay. Zero disables.
	FailureDelay time.Duration

	// OAuth2Enabled flips advertisement of the OAUTHBEARER SASL
	// mechanism in the AuthenticateMechanisms reply. Set by the
	// backend / yarilo-auth wiring when at least one OAuth provider
	// is configured under auth.oauth2; otherwise the mech stays
	// off the wire so a client never sees it advertised against a
	// deployment that cannot validate tokens.
	OAuth2Enabled bool

	// Locker is the cross-process write coordinator. When non-nil, each
	// successful write (APPEND/COPY/MOVE/STORE/EXPUNGE) emits an EVENT on
	// the mailbox key so IMAP IDLE sessions on other pods are woken up
	// without waiting for the heartbeat interval. Nil keeps the legacy
	// timer-based IDLE behaviour.
	Locker locks.Locker

	// SpecialUseDefaults is the folder-name → \Sent/\Drafts/etc. mapping
	// applied by LIST when the on-disk per-user special_use file does not
	// override. Driven by protocol.imap.imap_special_use_defaults in
	// yarilo.yaml.
	SpecialUseDefaults map[string]string

	// MetadataDict backs RFC 5464 METADATA (GETMETADATA / SETMETADATA).
	// When nil, the server still advertises METADATA / METADATA-SERVER
	// (the lib needs the caps to parse the commands) but every op
	// returns "metadata storage disabled". Operators wire this from
	// cfg.Dicts["metadata"] in yarilo.yaml.
	MetadataDict dict.Dict

	// QuotaDict backs RFC 9208 QUOTA (GETQUOTAROOT / GETQUOTA). When
	// non-nil, the server advertises the QUOTA capability, enforces
	// storage limits on APPEND/COPY/MOVE, and updates per-user counters
	// (priv/quota/storage, priv/quota/messages) atomically. Nil disables
	// quota entirely. Operators wire this from cfg.Dicts["quota"].
	QuotaDict dict.Dict

	// ACLEnabled exposes RFC 4314 server-side ACL (GETACL / SETACL /
	// DELETEACL / MYRIGHTS / LISTRIGHTS) when true. Storage is the
	// per-mailbox `yarilo-acl` file in each folder's index directory
	// — no extra backend wiring required. When false, the SessionACL
	// methods on *session return NO("ACL extension disabled by
	// operator"); the capability is still advertised since go-imap
	// detects it via interface assertion.
	ACLEnabled bool

	// Namespaces drives the IMAP NAMESPACE response (RFC 2342 / RFC
	// 9051 §6.3.10). When nil/empty the server falls back to a single
	// personal namespace with separator "/" — backwards-compatible
	// with pre-v1.20 single-namespace deployments.
	Namespaces []NamespaceSpec

	// NamespaceMailboxes (optional) carries per-namespace
	// MailboxBackend overrides keyed by namespace prefix. When a
	// namespace has an entry here, openHandle uses it instead of the
	// global Mailbox backend — letting operators mix storage drivers
	// across namespaces (e.g. personal=maildir + shared=mdbox).
	// Personal namespaces always use the global Mailbox backend.
	NamespaceMailboxes map[string]mailbox.MailboxBackend

	// AnvilAddr is the yarilo-anvil server address. When non-empty
	// and the session arrived with XCLIENT SESSION=<id>, each
	// SELECT / EXAMINE / UNSELECT pushes a SELECT command to anvil
	// so `who` can render the currently-SELECTed folder.
	AnvilAddr string
	// AnvilTLS optionally wraps the anvil dialer with mTLS.
	AnvilTLS *tls.Config
}

// NamespaceSpec is the per-namespace data the IMAP server needs to
// render NAMESPACE responses and route mailbox operations. Mirrors
// the relevant subset of config.NamespaceConfig; kept separate so
// callers (backend, tests) can construct it without depending on
// pkg/config.
//
// Location is the storage URL ("maildir:/path") for this namespace.
// Empty means the namespace is wire-declared but not backed by
// storage — SELECT etc. on it returns NO. Personal namespaces
// inherit their storage from cfg.Storage.MailHomeTemplate and may
// leave Location empty.
type NamespaceSpec struct {
	Type      NamespaceType
	Prefix    string
	Separator rune
	List      bool
	Location  string
}

// NamespaceType classifies a namespace into the three slots of the
// IMAP NAMESPACE response: Personal / Other / Shared.
type NamespaceType string

const (
	NamespacePersonal NamespaceType = "personal"
	NamespaceOther    NamespaceType = "other"
	NamespaceShared   NamespaceType = "shared"
)

// New creates an IMAP server.
func New(opts Options) *Server {
	s := &Server{
		opts:        opts,
		anvilClient: newImapAnvilClient(opts.AnvilAddr, opts.AnvilTLS),
	}

	caps := imaplib.CapSet{
		imaplib.CapIMAP4rev1:   {},
		imaplib.CapIMAP4rev2:   {},
		imaplib.CapIdle:        {},
		imaplib.CapMove:        {},
		imaplib.CapCondStore:   {},
		imaplib.CapUIDPlus:     {},
		imaplib.CapNamespace:   {},
		imaplib.CapUnselect:    {},
		imaplib.CapLiteralPlus: {},
		// IMAP4rev2 (RFC 9051) requires these. Some are wire-level (ENABLE,
		// SASL-IR) — declaring them is enough; go-imap/v2 handles the
		// protocol mechanics. The rest (ESEARCH, SEARCHRES, STATUS=SIZE)
		// require semantic implementation in Search/Status below.
		imaplib.CapESearch:          {},
		imaplib.CapSearchRes:        {},
		imaplib.CapEnable:           {},
		imaplib.CapSASLIR:           {},
		imaplib.CapStatusSize:       {},
		imaplib.CapListExtended:     {},
		imaplib.CapListStatus:       {},
		imaplib.CapSpecialUse:       {},
		imaplib.CapCreateSpecialUse: {},
		imaplib.CapBinary:           {},
		imaplib.CapQResync:          {},
	}
	if opts.MetadataDict != nil {
		caps[imaplib.CapMetadata] = struct{}{}
	}
	if opts.ACLEnabled {
		caps[imaplib.CapACL] = struct{}{}
	}
	if opts.QuotaDict != nil {
		caps[imaplib.CapQuota] = struct{}{}
	}

	s.srv = imapserver.New(&imapserver.Options{
		NewSession:   s.newSession,
		Caps:         caps,
		TLSConfig:    opts.TLSConfig,
		InsecureAuth: !opts.DisablePlainAuth,
		Logger:       &slogLogger{},
	})
	return s
}

func (s *Server) ListenAndServeTLS() error {
	if s.opts.TLSConfig == nil {
		return fmt.Errorf("imap: TLS config required for IMAPS")
	}
	ln, err := tls.Listen("tcp", s.opts.Addr, s.opts.TLSConfig)
	if err != nil {
		return err
	}
	slog.Info("imap: listening (TLS)", "addr", s.opts.Addr)
	return s.srv.Serve(s.wrapProxy(ln))
}

func (s *Server) Serve(ln net.Listener) error {
	return s.srv.Serve(s.wrapProxy(ln))
}

func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.opts.AddrPlain)
	if err != nil {
		return err
	}
	slog.Info("imap: listening (STARTTLS)", "addr", s.opts.AddrPlain)
	return s.srv.Serve(s.wrapProxy(ln))
}

func (s *Server) wrapProxy(ln net.Listener) net.Listener {
	if s.opts.ProxyProtocol {
		timeout := s.opts.HAProxyTimeout
		if timeout == 0 {
			timeout = 3 * time.Second
		}
		ln = &proxyproto.Listener{
			Listener:          ln,
			ReadHeaderTimeout: timeout,
			Policy:            proxyPolicy(s.opts.HAProxyTrustedNets),
		}
	}
	if s.opts.MaxLineLength > 0 {
		ln = &maxLineLenListener{Listener: ln, limit: s.opts.MaxLineLength}
	}
	if s.opts.AuthAddr != "" {
		ln = &loginproto.PreambleListener{
			Listener:        ln,
			AuthAddr:        s.opts.AuthAddr,
			AuthTLS:         s.opts.AuthTLS,
			MasterAddr:      s.opts.MasterAddr,
			MasterTLS:       s.opts.MasterTLS,
			ExpectedService: "imap",
		}
	}
	if s.opts.LoginGreeting != "" {
		ln = &greetingListener{Listener: ln, greeting: s.opts.LoginGreeting}
	}
	if s.opts.IDSend != "" {
		ln = newIDListener(ln, s.opts.IDSend)
	}
	return ln
}

func proxyPolicy(nets []*net.IPNet) func(net.Addr) (proxyproto.Policy, error) {
	return func(upstream net.Addr) (proxyproto.Policy, error) {
		if len(nets) == 0 {
			return proxyproto.IGNORE, nil
		}
		tcp, ok := upstream.(*net.TCPAddr)
		if !ok {
			return proxyproto.IGNORE, nil
		}
		for _, n := range nets {
			if n.Contains(tcp.IP) {
				return proxyproto.USE, nil
			}
		}
		return proxyproto.IGNORE, nil
	}
}

func (s *Server) newSession(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
	sess := &session{srv: s, imapConn: c}
	if pc := unwrapPreambleConn(c.NetConn()); pc != nil {
		sess.sid = pc.SessionID
		if err := sess.completeLogin(&protocol.AuthResponse{
			Result:      protocol.AuthOK,
			Username:    pc.Username,
			Home:        pc.Home,
			MailLoc:     pc.MailLoc,
			Groups:      pc.Groups,
			QuotaRules:  pc.QuotaRules,
			VolatileDir: pc.VolatileDir,
			IndexDir:    pc.IndexDir,
			ControlDir:  pc.ControlDir,
			AltDir:      pc.AltDir,
		}); err != nil {
			return nil, nil, err
		}
		return sess, &imapserver.GreetingData{PreAuth: true}, nil
	}
	return sess, &imapserver.GreetingData{PreAuth: false}, nil
}

// unwrapPreambleConn walks the net.Conn wrapper chain looking for a
// *loginproto.PreambleConn. Wrapper types (greetingConn, idImapConn) expose
// Unwrap() net.Conn so they can be peeled off transparently.
func unwrapPreambleConn(c net.Conn) *loginproto.PreambleConn {
	type unwrapper interface{ Unwrap() net.Conn }
	for c != nil {
		if pc, ok := c.(*loginproto.PreambleConn); ok {
			return pc
		}
		uw, ok := c.(unwrapper)
		if !ok {
			return nil
		}
		c = uw.Unwrap()
	}
	return nil
}

// ---- session ---------------------------------------------------------------

type session struct {
	srv      *Server
	imapConn *imapserver.Conn
	userInfo *mailbox.UserInfo
	sid      string // cross-service correlation ID from login-proxy
	// box / idx / subs are convenience aliases for the personal
	// namespace handle (== s.primary.box / .idx / .subs). They keep
	// the pre-NS-1b single-namespace code path readable for the
	// common case (INBOX + personal folders). Cross-namespace ops
	// (SELECT under "Shared/", LIST traversal, GETMETADATA on a
	// shared mailbox) explicitly route through s.dispatch() and use
	// the resulting handle's box/idx/subs.
	box  mailbox.UserMailbox
	idx  mailbox.UserIndex
	subs *subs.Store

	limitIP string
	folder  *mailbox.Folder

	// knownMsgs is the server's copy of the client's sequence→message state
	// for the selected folder. Each entry records uid and modseq; the slice
	// index+1 is the IMAP sequence number. Populated at SELECT, updated by
	// Poll, Expunge, and Move. Nil when no folder is selected.
	knownMsgs []sessionMsg
	// syncModSeq is the HighestModSeq seen at the last successful full
	// GetMessages diff. Used as a cheap fast-path: when the index has not
	// advanced past this value and hasPendingExpunge is false, Poll skips
	// the GetMessages call entirely.
	syncModSeq uint64
	// hasPendingExpunge is set when Poll found expunged UIDs but could not
	// deliver * EXPUNGE because allowExpunge was false. The flag bypasses
	// the syncModSeq fast-path on the next Poll call so the expunges are
	// retried as soon as allowExpunge becomes true.
	hasPendingExpunge bool
	// knownKeywords tracks keyword flags announced to the client in SELECT or
	// via subsequent * FLAGS responses; used to detect new keywords during Poll.
	// Nil when no folder is selected.
	knownKeywords map[string]struct{}

	// namespaces holds the per-namespace storage handles, keyed by
	// the namespace prefix. The personal namespace always has key "".
	// Empty / declared-only namespaces (Other Users in NS-1b) are
	// absent — dispatch() catches them via the wire-spec list.
	namespaces map[string]*nsHandle
	// primary is the personal namespace handle; pointer-equal to
	// namespaces[""].
	primary *nsHandle
	// folderNS is the namespace handle for the currently-SELECTed
	// folder. Captured at SELECT time so folder-bound ops (FETCH,
	// STORE, EXPUNGE, etc.) route to the right backend without
	// re-parsing the mailbox name. When nil, s.primary is assumed.
	folderNS *nsHandle

	// savedSearchUIDs holds the most recent SEARCH result that was issued
	// with RETURN SAVE (RFC 5182). Subsequent commands that reference $ get
	// this set substituted in via go-imap/v2's IsSearchRes detection.
	savedSearchUIDs imaplib.UIDSet

	// specialUse persists per-user RFC 6154 overrides set via CREATE
	// (USE ...) and resolves folder→attr for LIST. Only personal —
	// RFC 6154 \Sent/\Drafts/etc. semantics do not extend to shared
	// or public namespaces.
	specialUse *specialuse.Store

	statsDeleted    int
	statsExpunged   int
	statsFetchHdr   int
	statsFetchHdrB  int64
	statsFetchBody  int
	statsFetchBodyB int64
}

// folderBox returns the UserMailbox backing s.folder. Returns s.box
// (personal alias) when no folder is selected or when the selected
// folder happens to be in the personal namespace.
func (s *session) folderBox() mailbox.UserMailbox {
	if s.folderNS != nil {
		return s.folderNS.box
	}
	return s.box
}

// folderIdx returns the UserIndex backing s.folder.
func (s *session) folderIdx() mailbox.UserIndex {
	if s.folderNS != nil {
		return s.folderNS.idx
	}
	return s.idx
}

var _ imapserver.SessionIMAP4rev2 = (*session)(nil)

// emitMailboxChange is fire-and-forget — events are advisory wake-ups for
// subscribed IMAP IDLE sessions on other pods. Errors are logged at debug
// level and never surfaced to the caller because the authoritative state
// already lives in the just-written index/uidlist files. A 1-second timeout
// keeps a sluggish locks server from stalling the IMAP command.
func (s *session) emitMailboxChange(folder string, eventType locks.EventType, uid uint32) {
	if s.srv.opts.Locker == nil || s.userInfo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	payload := strconv.FormatUint(uint64(uid), 10)
	if err := s.srv.opts.Locker.Emit(ctx, locks.MailboxKey(s.userInfo.Username, folder), eventType, payload); err != nil {
		slog.Debug("imap: emit event failed",
			"folder", folder, "type", string(eventType), "err", err)
	}
}

func (s *session) Close() error {
	if s.srv.opts.ConnLimit != nil && s.userInfo != nil {
		s.srv.opts.ConnLimit.Release(s.userInfo.Username, s.limitIP)
	}
	if s.srv.opts.LogoutFormat != "" && s.userInfo != nil {
		msg := formatLogoutMsg(s.srv.opts.LogoutFormat, map[string]string{
			"deleted":          strconv.Itoa(s.statsDeleted),
			"expunged":         strconv.Itoa(s.statsExpunged),
			"fetch_hdr_count":  strconv.Itoa(s.statsFetchHdr),
			"fetch_hdr_bytes":  strconv.FormatInt(s.statsFetchHdrB, 10),
			"fetch_body_count": strconv.Itoa(s.statsFetchBody),
			"fetch_body_bytes": strconv.FormatInt(s.statsFetchBodyB, 10),
		})
		slog.Info("imap: logout", "sid", s.sid, "user", s.userInfo.Username, "stats", msg)
	}
	// closeHandles tears down every per-namespace box+idx; s.box/s.idx
	// aliases pointed at s.primary so the personal handle is included.
	s.closeHandles()
	return nil
}

func formatLogoutMsg(format string, vars map[string]string) string {
	var sb strings.Builder
	i := 0
	for i < len(format) {
		if format[i] != '%' || i+2 >= len(format) || format[i+1] != '{' {
			sb.WriteByte(format[i])
			i++
			continue
		}
		end := strings.IndexByte(format[i:], '}')
		if end < 0 {
			sb.WriteByte(format[i])
			i++
			continue
		}
		key := format[i+2 : i+end]
		if v, ok := vars[key]; ok {
			sb.WriteString(v)
		} else {
			sb.WriteString(format[i : i+end+1])
		}
		i += end + 1
	}
	return sb.String()
}

func (s *session) Login(username, password string) error {
	res, err := s.srv.opts.Auth.Authenticate(username, password, "imap", remoteIP(s.imapConn.NetConn()))
	if err != nil || res == nil || res.Result != protocol.AuthOK {
		s.delayFailure()
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Invalid credentials"}
	}
	return s.completeLogin(res)
}

// delayFailure holds the calling goroutine for opts.FailureDelay
// before letting an auth-failure surface to the client. Mirrors
// Dovecot's auth_failure_delay — equalises wall-clock between
// success and every failure-cause so the wire timing carries no
// information about whether the user exists.
func (s *session) delayFailure() {
	if d := s.srv.opts.FailureDelay; d > 0 {
		time.Sleep(d)
	}
}

// AuthenticateMechanisms advertises the SASL mechanisms our session
// implements itself (overriding go-imap's built-in PLAIN handler).
// PLAIN is unconditional; OAUTHBEARER lights up when an OAuth
// provider is configured; SCRAM-SHA-256 + SCRAM-SHA-256-PLUS light
// up when the configured Authenticator implements
// SCRAMSha256Lookup (i.e. at least one passdb carries SCRAM
// verifiers). The PLUS variant additionally requires the
// connection to be over TLS 1.3+ so the RFC 9266 exporter is
// available.
func (s *session) AuthenticateMechanisms() []string {
	out := []string{sasl.Plain}
	if s.srv.opts.OAuth2Enabled {
		out = append(out, sasl.OAuthBearer)
		out = append(out, sasl.XOAuth2)
	}
	if _, ok := s.srv.opts.Auth.(protocol.SCRAMSha256Lookup); ok {
		out = append(out, sasl.ScramSha256)
		if s.tlsExporter() != nil {
			out = append(out, sasl.ScramSha256Plus)
		}
	}
	if _, ok := s.srv.opts.Auth.(protocol.SCRAMSha1Lookup); ok {
		out = append(out, sasl.ScramSha1)
		if s.tlsExporter() != nil {
			out = append(out, sasl.ScramSha1Plus)
		}
	}
	return out
}

// Authenticate returns the SASL server for the requested mechanism.
// Custom PLAIN handler is used when the configured Authenticator
// implements protocol.MasterAuthenticator — otherwise we delegate
// the response shape (no authzid) to the standard Login path so
// stubs / non-master backends keep working. OAUTHBEARER routes the
// bearer token through the regular Authenticator surface (the OAuth
// passdb downstream extracts the token from req.Password). SCRAM
// variants route through the SCRAMSha256Lookup interface — the
// verifier never crosses the wire and no plain password is ever
// compared.
func (s *session) Authenticate(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(s.authenticatePlainSASL), nil
	case sasl.OAuthBearer:
		if !s.srv.opts.OAuth2Enabled {
			return nil, &imaplib.Error{
				Type: imaplib.StatusResponseTypeNo,
				Text: "SASL mechanism not supported",
			}
		}
		return oauth2.NewOAuthBearerSASLServer(s.authenticateOAuthBearer), nil
	case sasl.XOAuth2:
		if !s.srv.opts.OAuth2Enabled {
			return nil, &imaplib.Error{
				Type: imaplib.StatusResponseTypeNo,
				Text: "SASL mechanism not supported",
			}
		}
		return oauth2.NewXOAuth2SASLServer(s.authenticateXOAuth2), nil
	case sasl.ScramSha256:
		lookup, ok := s.srv.opts.Auth.(protocol.SCRAMSha256Lookup)
		if !ok {
			return nil, &imaplib.Error{
				Type: imaplib.StatusResponseTypeNo, Text: "SASL mechanism not supported",
			}
		}
		return scram.NewSha256(lookup, s.completeSCRAMLogin), nil
	case sasl.ScramSha256Plus:
		lookup, ok := s.srv.opts.Auth.(protocol.SCRAMSha256Lookup)
		if !ok {
			return nil, &imaplib.Error{
				Type: imaplib.StatusResponseTypeNo, Text: "SASL mechanism not supported",
			}
		}
		cb := s.tlsExporter()
		if cb == nil {
			return nil, &imaplib.Error{
				Type: imaplib.StatusResponseTypeNo,
				Text: "Channel binding unavailable",
			}
		}
		return scram.NewSha256Plus(lookup, cb, s.completeSCRAMLogin), nil
	case sasl.ScramSha1:
		lookup, ok := s.srv.opts.Auth.(protocol.SCRAMSha1Lookup)
		if !ok {
			return nil, &imaplib.Error{
				Type: imaplib.StatusResponseTypeNo, Text: "SASL mechanism not supported",
			}
		}
		return scram.NewSha1(lookup, s.completeSCRAMLogin), nil
	case sasl.ScramSha1Plus:
		lookup, ok := s.srv.opts.Auth.(protocol.SCRAMSha1Lookup)
		if !ok {
			return nil, &imaplib.Error{
				Type: imaplib.StatusResponseTypeNo, Text: "SASL mechanism not supported",
			}
		}
		cb := s.tlsExporter()
		if cb == nil {
			return nil, &imaplib.Error{
				Type: imaplib.StatusResponseTypeNo,
				Text: "Channel binding unavailable",
			}
		}
		return scram.NewSha1Plus(lookup, cb, s.completeSCRAMLogin), nil
	}
	return nil, &imaplib.Error{
		Type: imaplib.StatusResponseTypeNo,
		Text: "SASL mechanism not supported",
	}
}

// completeSCRAMLogin is the OnSuccess hook for SCRAM session
// adapters. The SCRAM SASL server has already verified the user
// (via stored StoredKey + ClientProof); here we run the regular
// post-auth setup so the IMAP session lands fully initialised.
func (s *session) completeSCRAMLogin(username string) error {
	return s.completeLogin(&protocol.AuthResponse{
		Result:   protocol.AuthOK,
		Username: username,
	})
}

// tlsExporter returns the RFC 9266 TLS exporter output for the
// session's underlying TLS connection, or nil when the conn is
// not TLS 1.3+. The 32-byte exporter is the channel-binding
// material for SCRAM-SHA-256-PLUS.
func (s *session) tlsExporter() []byte {
	netConn := s.imapConn.NetConn()
	if netConn == nil {
		return nil
	}
	tc, ok := netConn.(*tls.Conn)
	if !ok {
		return nil
	}
	state := tc.ConnectionState()
	if state.Version < tls.VersionTLS13 {
		return nil
	}
	out, err := state.ExportKeyingMaterial("EXPORTER-Channel-Binding", nil, 32)
	if err != nil {
		return nil
	}
	return out
}

// authenticateOAuthBearer is the OAuthBearerAuthenticator callback
// invoked by go-sasl after it has parsed the GS2 envelope. Wire-
// shape concerns (gs2-header parsing, RFC 7628 JSON error blob on
// failure) live entirely inside go-sasl; here we only translate
// (Username, Token) into the chain's Authenticate(user, password,
// service) call and surface a Bearer JSON error on rejection.
func (s *session) authenticateOAuthBearer(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
	res, err := s.srv.opts.Auth.Authenticate(opts.Username, opts.Token, "imap", remoteIP(s.imapConn.NetConn()))
	if err != nil || res == nil || res.Result != protocol.AuthOK {
		s.delayFailure()
		return &sasl.OAuthBearerError{
			Status:  "invalid_token",
			Schemes: "bearer",
		}
	}
	if err := s.completeLogin(res); err != nil {
		return &sasl.OAuthBearerError{
			Status:  "invalid_token",
			Schemes: "bearer",
		}
	}
	return nil
}

// authenticateXOAuth2 is the XOAUTH2 callback. Same token
// validation path as OAUTHBEARER — only the wire format differs.
func (s *session) authenticateXOAuth2(opts sasl.XOAuth2Options) *sasl.OAuthBearerError {
	res, err := s.srv.opts.Auth.Authenticate(opts.Username, opts.Token, "imap", remoteIP(s.imapConn.NetConn()))
	if err != nil || res == nil || res.Result != protocol.AuthOK {
		s.delayFailure()
		return &sasl.OAuthBearerError{
			Status:  "invalid_token",
			Schemes: "bearer",
		}
	}
	if err := s.completeLogin(res); err != nil {
		return &sasl.OAuthBearerError{
			Status:  "invalid_token",
			Schemes: "bearer",
		}
	}
	return nil
}

// authenticatePlainSASL is the PlainAuthenticator callback used by
// our SessionSASL handler. authzid carries the impersonation target
// (Dovecot's master-user model); empty / equal-to-authid disables
// impersonation and falls back to the regular login path.
//
// When the configured Authenticator does not implement
// MasterAuthenticator, a non-empty distinct authzid is rejected
// (same opacity as the wire FAIL — no detail given to the client).
func (s *session) authenticatePlainSASL(authzid, authid, password string) error {
	invalid := &imaplib.Error{
		Type: imaplib.StatusResponseTypeNo,
		Text: "Invalid credentials",
	}
	ip := remoteIP(s.imapConn.NetConn())
	if authzid == "" || authzid == authid {
		res, err := s.srv.opts.Auth.Authenticate(authid, password, "imap", ip)
		if err != nil || res == nil || res.Result != protocol.AuthOK {
			s.delayFailure()
			return invalid
		}
		return s.completeLogin(res)
	}
	master, ok := s.srv.opts.Auth.(protocol.MasterAuthenticator)
	if !ok {
		s.delayFailure()
		return invalid
	}
	res, err := master.AuthenticateMaster(authzid, authid, password, "imap", ip)
	if err != nil || res == nil || res.Result != protocol.AuthOK {
		s.delayFailure()
		return invalid
	}
	return s.completeLogin(res)
}

// completeLogin runs the post-auth session setup shared between the
// IMAP LOGIN command path and the AUTHENTICATE PLAIN SASL path.
// res carries the resolved username (target, for master flows) and
// the userdb-enriched fields needed to open the per-namespace
// storage handles.
func (s *session) completeLogin(res *protocol.AuthResponse) error {
	resolver := s.srv.opts.Resolver
	if resolver == nil {
		resolver = &mailbox.Resolver{}
	}
	userInfo := resolver.UserInfo(res.Username, res.Home)
	userInfo.Groups = res.Groups
	userInfo.QuotaRules = res.QuotaRules
	userInfo.SessionID = s.sid
	if res.VolatileDir != "" {
		vd := strings.ReplaceAll(res.VolatileDir, "%h", userInfo.Home)
		userInfo.VolatileDir = mailbox.ExpandVars(vd, res.Username)
	}
	if res.IndexDir != "" {
		id := strings.ReplaceAll(res.IndexDir, "%h", userInfo.Home)
		userInfo.IndexDir = mailbox.ExpandVars(id, res.Username)
	}
	if res.ControlDir != "" {
		cd := strings.ReplaceAll(res.ControlDir, "%h", userInfo.Home)
		userInfo.ControlDir = mailbox.ExpandVars(cd, res.Username)
	}
	if res.AltDir != "" {
		ad := strings.ReplaceAll(res.AltDir, "%h", userInfo.Home)
		userInfo.AltDir = mailbox.ExpandVars(ad, res.Username)
	}

	if lim := s.srv.opts.ConnLimit; lim != nil {
		ip := remoteIP(s.imapConn.NetConn())
		if !lim.Acquire(userInfo.Username, ip) {
			slog.Warn("imap: connection limit reached", "sid", s.sid, "user", userInfo.Username, "ip", ip, "result", "fail")
			return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Too many simultaneous connections"}
		}
		s.limitIP = ip
	}

	s.userInfo = userInfo

	handles, primary, err := s.openHandles(userInfo)
	if err != nil {
		slog.Error("imap: namespace handle init failed", "user", userInfo.Username, "err", err)
		if s.srv.opts.ConnLimit != nil {
			s.srv.opts.ConnLimit.Release(userInfo.Username, s.limitIP)
		}
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Internal error"}
	}
	s.namespaces = handles
	s.primary = primary
	s.box = primary.box
	s.idx = primary.idx
	s.subs = primary.subs

	owner := fmt.Sprintf("yarilo-imap/%d/%s", os.Getpid(), userInfo.Username)
	s.specialUse = specialuse.New(
		userInfo.Home, userInfo.Username, owner, s.srv.opts.Locker,
		s.srv.opts.SpecialUseDefaults,
	)

	// Audit log. master_user is non-empty only on the impersonation
	// path (AUTH-3); mirrors Dovecot's per-event `master_user=`
	// field that surfaces in every subsequent log line for the
	// session. Always emitted for ALL logins so SIEM can correlate
	// regular and master-user sessions through one log shape.
	master, _ := res.Fields.Get("master_user")
	slog.Info("imap: login",
		"sid", s.sid,
		"user", userInfo.Username,
		"master_user", master,
		"remoteIP", remoteIP(s.imapConn.NetConn()),
		"result", "ok",
	)
	return nil
}

func remoteIP(c net.Conn) string {
	if c == nil {
		return ""
	}
	if tcp, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		return tcp.IP.String()
	}
	return c.RemoteAddr().String()
}

func (s *session) Select(name string, opts *imaplib.SelectOptions) (*imaplib.SelectData, error) {
	tSelect := time.Now()
	h, rel, err := s.dispatch(name)
	if err != nil {
		return nil, err
	}
	exists, err := h.box.FolderExists(rel)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}
	if err := s.requireRight(h, rel, mailbox.RightRead); err != nil {
		return nil, err
	}
	tOpen := time.Now()
	f, err := h.idx.OpenFolder(rel, uint32(time.Now().Unix()))
	if err != nil {
		return nil, err
	}
	slog.Debug("imap: select timing open_ms", "folder", rel, "open_ms", time.Since(tOpen).Milliseconds())
	s.folder = f
	s.folderNS = h
	tAnvil := time.Now()
	s.pushAnvilSelect(name)
	slog.Debug("imap: select timing anvil_ms", "folder", rel, "anvil_ms", time.Since(tAnvil).Milliseconds())

	// Auto-subscribe the folder on first SELECT so LSUB returns it
	// without requiring an explicit SUBSCRIBE command from the client.
	if h.subs != nil {
		tSubs := time.Now()
		if subs, snapErr := h.subs.Snapshot(); snapErr == nil {
			if _, already := subs[rel]; !already {
				tAdd := time.Now()
				_ = h.subs.Add(rel)
				slog.Debug("imap: select timing subs_add_ms", "folder", rel, "add_ms", time.Since(tAdd).Milliseconds())
			}
		}
		slog.Debug("imap: select timing subs_ms", "folder", rel, "subs_ms", time.Since(tSubs).Milliseconds())
	}

	tGetMsgs := time.Now()
	msgs, err := h.idx.GetMessages(f.ID, mailbox.SeqSet{})
	slog.Debug("imap: select timing getmsgs_ms", "folder", rel, "getmsgs_ms", time.Since(tGetMsgs).Milliseconds(), "total_ms", time.Since(tSelect).Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("imap: select getmsgs %s: %w", rel, err)
	}
	s.knownMsgs = make([]sessionMsg, len(msgs))
	for i, m := range msgs {
		s.knownMsgs[i] = sessionMsg{uid: m.UID, modseq: m.ModSeq}
	}
	s.syncModSeq = f.HighestModSeq
	s.hasPendingExpunge = false
	sysFlags := []imaplib.Flag{
		imaplib.FlagAnswered, imaplib.FlagFlagged,
		imaplib.FlagDeleted, imaplib.FlagSeen, imaplib.FlagDraft,
	}
	allFlags := sysFlags
	s.knownKeywords = make(map[string]struct{})
	if kws, err := h.idx.Keywords(f.ID); err == nil {
		for _, kw := range kws {
			allFlags = append(allFlags, imaplib.Flag(kw))
			s.knownKeywords[kw] = struct{}{}
		}
	}
	data := &imaplib.SelectData{
		Flags:          allFlags,
		PermanentFlags: append(allFlags, imaplib.Flag(`\*`)),
		NumMessages:    uint32(len(msgs)),
		UIDValidity:    f.UIDValidity,
		UIDNext:        imaplib.UID(f.NextUID),
		HighestModSeq:  f.HighestModSeq,
	}
	// QRESYNC SELECT (RFC 7162 §3.2): when the client supplies (UIDVALIDITY
	// <v> <last-known-modseq> [<known-uids>]) and the UIDVALIDITY matches,
	// reply with VANISHED (EARLIER) listing UIDs expunged since the client's
	// modseq. The KnownUIDs filter narrows the response to UIDs the client
	// actually remembers; an empty set means "tell me everything".
	if opts != nil && opts.QResync != nil && opts.QResync.UIDValidity == f.UIDValidity {
		vanishedUIDs, vErr := h.idx.Vanished(f.ID, opts.QResync.ModSeq)
		if vErr == nil && len(vanishedUIDs) > 0 {
			var vset imaplib.UIDSet
			if len(opts.QResync.KnownUIDs) == 0 {
				for _, uid := range vanishedUIDs {
					vset.AddNum(imaplib.UID(uid))
				}
			} else {
				for _, uid := range vanishedUIDs {
					if opts.QResync.KnownUIDs.Contains(imaplib.UID(uid)) {
						vset.AddNum(imaplib.UID(uid))
					}
				}
			}
			if len(vset) > 0 {
				data.Vanished = vset
			}
		}
	}
	return data, nil
}

func (s *session) Unselect() error {
	s.folder = nil
	s.folderNS = nil
	s.knownMsgs = nil
	s.syncModSeq = 0
	s.hasPendingExpunge = false
	s.knownKeywords = nil
	s.pushAnvilSelect("")
	return nil
}

func (s *session) Create(name string, opts *imaplib.CreateOptions) error {
	h, rel, err := s.dispatch(name)
	if err != nil {
		return err
	}
	if err := s.requireRightOnParent(h, rel, mailbox.RightCreate); err != nil {
		return err
	}
	if err := h.box.Create(rel); err != nil {
		return err
	}
	// CREATE-SPECIAL-USE (RFC 6154 §3): record the requested use attr so
	// subsequent LIST replies advertise it. RFC permits multiple USE attrs
	// in the request but forbids carrying more than one on the folder; we
	// honour the first one in the supplied slice and ignore the rest.
	// Special-use semantics are personal-namespace-only — shared/public
	// folders with \Sent/\Drafts would be confusing across users.
	if opts != nil && len(opts.SpecialUse) > 0 && s.specialUse != nil && h == s.primary {
		if err := s.specialUse.Set(rel, opts.SpecialUse[0]); err != nil {
			slog.Warn("imap: special_use persist failed",
				"folder", name, "attr", string(opts.SpecialUse[0]), "err", err)
		}
	}
	return nil
}

func (s *session) Delete(name string) error {
	h, rel, err := s.dispatch(name)
	if err != nil {
		return err
	}
	if err := s.requireRight(h, rel, mailbox.RightDeleteMailbox); err != nil {
		return err
	}
	if err := h.box.Delete(rel); err != nil {
		return err
	}
	// Drop any explicit ACL state — file + namespace-wide index.
	// Errors are non-fatal (mailbox is already gone); log and
	// proceed so the client sees a clean DELETE result.
	if h.acl != nil {
		if err := h.acl.Remove(rel); err != nil {
			slog.Warn("imap: acl remove after DELETE failed", "folder", name, "err", err)
		}
	}
	return nil
}

func (s *session) Rename(oldName, newName string, _ *imaplib.RenameOptions) error {
	if strings.EqualFold(oldName, "INBOX") {
		return s.renameInbox(newName)
	}
	hOld, relOld, err := s.dispatch(oldName)
	if err != nil {
		return err
	}
	hNew, relNew, err := s.dispatch(newName)
	if err != nil {
		return err
	}
	if hOld != hNew {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Text: "RENAME across namespaces is not supported",
		}
	}
	// RENAME requires DELETE on the source mailbox plus CREATE on
	// the destination's parent — matches Dovecot acl-mailbox.c
	// rename semantics and lets shared-mailbox admin grant move
	// rights without granting blanket delete.
	if err := s.requireRight(hOld, relOld, mailbox.RightDeleteMailbox); err != nil {
		return err
	}
	if err := s.requireRightOnParent(hNew, relNew, mailbox.RightCreate); err != nil {
		return err
	}
	if err := hOld.box.Rename(relOld, relNew); err != nil {
		return err
	}
	if err := hOld.idx.RenameFolder(relOld, relNew); err != nil {
		return err
	}
	// Move the per-mailbox yarilo-acl file (if any) and rewrite the
	// namespace-wide index entries. Errors are non-fatal — the
	// mailbox itself has moved; we log and keep the IMAP response
	// successful so clients are not blocked by a stale index.
	if hOld.acl != nil {
		if err := hOld.acl.Rename(relOld, relNew); err != nil {
			slog.Warn("imap: acl rename failed", "from", oldName, "to", newName, "err", err)
		}
	}
	return nil
}

// renameInbox implements RFC 3501 §6.3.5 INBOX rename semantics:
// messages are moved to the new mailbox; INBOX itself is cleared but not deleted.
func (s *session) renameInbox(dest string) error {
	if err := s.box.Create(dest); err != nil {
		return fmt.Errorf("imap/rename-inbox create: %w", err)
	}
	srcFolder, err := s.idx.OpenFolder("INBOX", 0)
	if err != nil {
		return err
	}
	msgs, err := s.idx.GetMessages(srcFolder.ID, mailbox.SeqSet{})
	if err != nil {
		return err
	}
	destFolder, err := s.idx.OpenFolder(dest, uint32(time.Now().Unix()))
	if err != nil {
		return err
	}
	for _, m := range msgs {
		rc, fetchErr := s.box.Fetch("INBOX", m.Filename, m.AltTier)
		if fetchErr != nil {
			return fmt.Errorf("imap/rename-inbox fetch: %w", fetchErr)
		}
		data, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			return fmt.Errorf("imap/rename-inbox read: %w", readErr)
		}
		newFilename, saveErr := s.box.Save(dest, bytes.NewReader(data), 0, int64(len(data)), m.Flags)
		if saveErr != nil {
			return fmt.Errorf("imap/rename-inbox save: %w", saveErr)
		}
		nm := &mailbox.MessageMeta{
			Filename:     newFilename,
			Flags:        m.Flags,
			Keywords:     m.Keywords,
			Size:         uint32(len(data)),
			InternalDate: m.InternalDate,
		}
		if err := s.idx.AllocateAndAppend(destFolder.ID, nm); err != nil {
			_ = s.box.Remove(dest, newFilename)
			return fmt.Errorf("imap/rename-inbox record: %w", err)
		}
		s.emitMailboxChange(dest, locks.EventDelivered, nm.UID)
		s.box.Remove("INBOX", m.Filename)         //nolint:errcheck
		s.idx.ExpungeMessage(srcFolder.ID, m.UID) //nolint:errcheck
		s.emitMailboxChange("INBOX", locks.EventExpunged, m.UID)
	}
	srcFolder.Messages = 0
	s.idx.SaveFolder(srcFolder) //nolint:errcheck
	return nil
}

func (s *session) Subscribe(name string) error {
	h, rel, err := s.dispatch(name)
	if err != nil {
		return err
	}
	if h.subs == nil {
		return nil
	}
	if err := h.subs.Add(rel); err != nil {
		return fmt.Errorf("imap: subscribe %q: %w", name, err)
	}
	return nil
}

func (s *session) Unsubscribe(name string) error {
	h, rel, err := s.dispatch(name)
	if err != nil {
		return err
	}
	if h.subs == nil {
		return nil
	}
	if err := h.subs.Remove(rel); err != nil {
		return fmt.Errorf("imap: unsubscribe %q: %w", name, err)
	}
	return nil
}

func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, opts *imaplib.ListOptions) error {
	if s.srv.opts.ClientWorkarounds&workaroundTBExtraMailboxSep != 0 {
		ref = strings.TrimPrefix(ref, "/")
		for i, p := range patterns {
			patterns[i] = strings.TrimPrefix(p, "/")
		}
	}

	// Iterate every implemented namespace (personal first, then by
	// prefix). Each handle's folders are emitted with the namespace
	// prefix re-attached so the wire-protocol name is the full path
	// the client sent / would send back.
	for _, h := range s.orderedHandles() {
		if err := s.listNamespace(w, h, ref, patterns, opts); err != nil {
			return err
		}
	}

	// Also emit "namespace root" entries for shared/public (\Noselect
	// \HasChildren) so a top-level LIST returns the namespace as a
	// visible folder even before any sub-folder exists. Personal's
	// root is INBOX-based and not emitted separately.
	for _, spec := range s.namespaceSpecsForList() {
		if spec.Type == NamespacePersonal || !spec.List {
			continue
		}
		// Skip namespaces with no implemented handle (Other Users
		// declared-only). Emit a \Noselect entry so clients see the
		// namespace exists; SELECT under it returns NO.
		rootName := strings.TrimSuffix(spec.Prefix, string(spec.Separator))
		if rootName == "" {
			continue
		}
		if !listMatch(rootName, patterns) {
			continue
		}
		attrs := []imaplib.MailboxAttr{
			imaplib.MailboxAttrNoSelect,
			imaplib.MailboxAttrHasChildren,
		}
		if err := w.WriteList(&imaplib.ListData{
			Mailbox: rootName,
			Delim:   spec.Separator,
			Attrs:   attrs,
		}); err != nil {
			return err
		}
	}
	return nil
}

// listNamespace emits LIST replies for one namespace's folders.
// Folder names are wire-encoded with the namespace prefix re-attached.
func (s *session) listNamespace(w *imapserver.ListWriter, h *nsHandle, ref string, patterns []string, opts *imaplib.ListOptions) error {
	tList := time.Now()
	folders, err := h.box.ListFolders()
	slog.Debug("imap: list timing listfolders_ms", "listfolders_ms", time.Since(tList).Milliseconds())
	if err != nil {
		return err
	}

	// Snapshot subscriptions once per LIST — every folder's ReturnSubscribed
	// / SelectSubscribed decision consults the same view, even if a sibling
	// session SUBSCRIBE'd mid-iteration.
	var subs map[string]struct{}
	if h.subs != nil && (opts != nil && (opts.SelectSubscribed || opts.ReturnSubscribed)) {
		tSubs := time.Now()
		subs, err = h.subs.Snapshot()
		slog.Debug("imap: list timing subs_ms", "subs_ms", time.Since(tSubs).Milliseconds())
		if err != nil {
			slog.Warn("imap: subscription snapshot failed", "ns", h.name, "err", err)
			subs = make(map[string]struct{})
		}
	}

	for _, name := range folders {
		// Wire-protocol name = namespace prefix + namespace-relative name.
		full := ref + h.fullName(name)
		if !listMatch(full, patterns) {
			continue
		}
		// SELECT SUBSCRIBED — drop folders the user has not subscribed to.
		// RECURSIVEMATCH refinement (return parent even if only a child is
		// subscribed) is out of scope for IMAP-B; clients that need it must
		// LIST without the filter and post-filter on their side.
		if opts != nil && opts.SelectSubscribed {
			if _, ok := subs[name]; !ok {
				continue
			}
		}
		attrs := mailboxAttrs(name, folders, s.srv.opts.ClientWorkarounds)
		// RETURN CHILDREN — annotate \HasChildren / \HasNoChildren.
		if opts != nil && opts.ReturnChildren {
			attrs = append(attrs, childrenAttr(name, folders))
		}
		// RETURN SUBSCRIBED — annotate \Subscribed when applicable.
		if opts != nil && opts.ReturnSubscribed {
			if _, ok := subs[name]; ok {
				attrs = append(attrs, imaplib.MailboxAttrSubscribed)
			}
		}
		// SPECIAL-USE (RFC 6154). Only personal namespace has
		// special-use semantics — \Sent/\Drafts/etc. on a shared
		// folder is confusing across users.
		if h == s.primary && s.specialUse != nil {
			if attr := s.specialUse.Get(name); attr != "" {
				attrs = append(attrs, attr)
			}
		}
		data := &imaplib.ListData{Mailbox: full, Delim: h.spec.Separator, Attrs: attrs}
		// RETURN STATUS (RFC 5819 / IMAP4rev2) — per-folder Status response
		// embedded in the LIST reply. Skip on failure rather than abort the
		// whole LIST.
		if opts != nil && opts.ReturnStatus != nil {
			if status, statErr := s.Status(full, opts.ReturnStatus); statErr == nil {
				data.Status = status
			}
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}

// namespaceSpecsForList returns the wire-protocol spec list used for
// emitting root entries. Falls back to defaultNamespaces when the
// operator did not configure any.
func (s *session) namespaceSpecsForList() []NamespaceSpec {
	specs := s.srv.opts.Namespaces
	if len(specs) == 0 {
		specs = defaultNamespaces
	}
	return specs
}

// childrenAttr returns \HasChildren when `name` is a prefix of any other
// listed folder, otherwise \HasNoChildren. Cheap O(n) scan — acceptable
// because LIST already loaded the slice.
func childrenAttr(name string, all []string) imaplib.MailboxAttr {
	prefix := name + "/"
	for _, other := range all {
		if other == name {
			continue
		}
		if strings.HasPrefix(other, prefix) {
			return imaplib.MailboxAttrHasChildren
		}
	}
	return imaplib.MailboxAttrHasNoChildren
}

func (s *session) Status(name string, opts *imaplib.StatusOptions) (*imaplib.StatusData, error) {
	h, rel, err := s.dispatch(name)
	if err != nil {
		return nil, err
	}
	if err := s.requireRight(h, rel, mailbox.RightRead); err != nil {
		return nil, err
	}
	f, err := h.idx.OpenFolder(rel, 0)
	if err != nil {
		return nil, err
	}
	msgs, err := h.idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		return nil, fmt.Errorf("imap: status getmsgs %s: %w", rel, err)
	}
	var (
		unseen    uint32
		deleted   uint32
		totalSize int64
	)
	for _, m := range msgs {
		if !hasFlag(m.Flags, `\Seen`) {
			unseen++
		}
		if hasFlag(m.Flags, `\Deleted`) {
			deleted++
		}
	}
	// STATUS=SIZE (RFC 8438, also IMAP4rev2 required) — the FileIndex
	// record does not carry message size; pull it from the maildir/dbox
	// filename via box.List which extracts the ",S=<phys>" suffix.
	// Only walked when the client asked for SIZE so the common STATUS
	// path stays cheap.
	if opts.Size {
		boxMsgs, listErr := h.box.List(rel)
		if listErr == nil {
			for _, bm := range boxMsgs {
				totalSize += int64(bm.Size)
			}
		}
	}
	d := &imaplib.StatusData{Mailbox: name}
	if opts.NumMessages {
		n := uint32(len(msgs))
		d.NumMessages = &n
	}
	if opts.UIDNext {
		d.UIDNext = imaplib.UID(f.NextUID)
	}
	if opts.UIDValidity {
		d.UIDValidity = f.UIDValidity
	}
	if opts.NumUnseen {
		d.NumUnseen = &unseen
	}
	if opts.NumDeleted {
		d.NumDeleted = &deleted
	}
	if opts.Size {
		d.Size = &totalSize
	}
	if opts.HighestModSeq {
		d.HighestModSeq = f.HighestModSeq
	}
	if opts.NumRecent {
		var n uint32 // RECENT tracking not implemented; RFC 9051 permits 0
		d.NumRecent = &n
	}
	return d, nil
}

func (s *session) Append(name string, r imaplib.LiteralReader, opts *imaplib.AppendOptions) (*imaplib.AppendData, error) {
	tAppend := time.Now()
	h, rel, f, err := s.ensureFolderHandle(name)
	if err != nil {
		return nil, err
	}
	if err := s.requireRight(h, rel, insertRight(h.spec)); err != nil {
		return nil, err
	}

	var flagList, kwList []string
	if opts != nil {
		for _, fl := range opts.Flags {
			fs := string(fl)
			if strings.HasPrefix(fs, `\`) {
				flagList = append(flagList, fs)
			} else {
				kwList = append(kwList, fs)
			}
		}
	}

	size := r.Size()

	// Enforce quota before allocating UID so the slot isn't wasted.
	if err := s.quotaCheckAppend(context.Background(), name, size); err != nil {
		return nil, err
	}

	tSave := time.Now()
	filename, err := h.box.Save(rel, r, 0, size, flagList)
	if err != nil {
		return nil, err
	}
	tIndex := time.Now()
	internalDate := time.Now()
	if opts != nil && !opts.Time.IsZero() {
		internalDate = opts.Time
	}
	m := &mailbox.MessageMeta{
		Filename: filename, Flags: flagList, Keywords: kwList, Size: uint32(size),
		InternalDate: internalDate,
	}
	if err := h.idx.AllocateAndAppend(f.ID, m); err != nil {
		_ = h.box.Remove(rel, filename)
		return nil, fmt.Errorf("imap/append record: %w", err)
	}
	tDone := time.Now()
	slog.Debug("imap: append timing",
		"user", s.userInfo.Username, "folder", rel, "size", size,
		"save_ms", tIndex.Sub(tSave).Milliseconds(),
		"index_ms", tDone.Sub(tIndex).Milliseconds(),
		"total_ms", tDone.Sub(tAppend).Milliseconds(),
	)
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		if rc, ferr := h.box.Fetch(rel, filename, false); ferr == nil {
			raw, _ := io.ReadAll(rc)
			rc.Close()
			sum := md5.Sum(raw)
			slog.Debug("imap: append saved",
				"user", s.userInfo.Username,
				"folder", rel,
				"uid", m.UID,
				"file", filename,
				"size", len(raw),
				"md5", fmt.Sprintf("%x", sum),
			)
		}
	}
	s.emitMailboxChange(name, locks.EventDelivered, m.UID)

	return &imaplib.AppendData{UIDValidity: f.UIDValidity, UID: imaplib.UID(m.UID)}, nil
}

// Poll delivers pending mailbox updates to the client between commands
// (RFC 3501 §5.2). Three classes of update are handled:
//
//   - * N EXPUNGE — UIDs in knownMsgs that are no longer in the index
//     (expunged by another session). Emitted in descending sequence order
//     (RFC 3501 §7.4.1). Withheld when allowExpunge is false; hasPendingExpunge
//     ensures the next allowExpunge=true call retries them.
//   - * N FETCH (FLAGS ...) — UIDs still present but with a changed modseq,
//     meaning flags were altered by another session.
//   - * N EXISTS — new messages appended to the folder since last poll.
//
// Fast-path: OpenFolder reads only the folder header (tiny file) to get
// HighestModSeq. GetMessages (full index scan) is only called when the
// modseq advanced or there are pending expunges to deliver.
func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.folder == nil || s.knownMsgs == nil {
		return nil
	}

	// Cheap modseq check — skip full scan when nothing changed and no
	// pending expunges are waiting for an allowExpunge=true window.
	refreshed, err := s.folderIdx().OpenFolder(s.folder.Name, s.folder.UIDValidity)
	if err != nil {
		return nil
	}
	if refreshed.HighestModSeq == s.syncModSeq && !s.hasPendingExpunge {
		return nil
	}

	current, err := s.folderIdx().GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return nil
	}
	type curInfo struct {
		modseq uint64
		flags  []string
		kw     []string
	}
	curMap := make(map[uint32]curInfo, len(current))
	for _, m := range current {
		curMap[m.UID] = curInfo{m.ModSeq, m.Flags, m.Keywords}
	}

	// Phase 1: expunges — descending seq so each WriteExpungeUID seq number
	// remains valid as earlier entries are removed from knownMsgs.
	hadExpunges := false
	for i := len(s.knownMsgs) - 1; i >= 0; i-- {
		uid := s.knownMsgs[i].uid
		if _, exists := curMap[uid]; exists {
			continue
		}
		hadExpunges = true
		if !allowExpunge {
			continue
		}
		if err := w.WriteExpungeUID(uint32(i+1), imaplib.UID(uid)); err != nil {
			return err
		}
		s.knownMsgs = append(s.knownMsgs[:i], s.knownMsgs[i+1:]...)
	}
	s.hasPendingExpunge = hadExpunges && !allowExpunge

	// Phase 2: flag updates — skipped entirely when hasPendingExpunge is true
	// because the pre-expunge sequence numbers would be wrong on the client side.
	if !s.hasPendingExpunge {
		type flagsUpdate struct {
			seq uint32
			uid uint32
			ci  curInfo
		}
		var pending []flagsUpdate
		newKwSet := make(map[string]struct{})
		var newKws []string
		for i, km := range s.knownMsgs {
			ci, exists := curMap[km.uid]
			if !exists || ci.modseq == km.modseq {
				continue
			}
			pending = append(pending, flagsUpdate{uint32(i + 1), km.uid, ci})
			for _, kw := range ci.kw {
				if _, known := s.knownKeywords[kw]; !known {
					if _, dup := newKwSet[kw]; !dup {
						newKws = append(newKws, kw)
						newKwSet[kw] = struct{}{}
					}
				}
			}
		}
		if len(newKws) > 0 {
			sysFlags := []imaplib.Flag{
				imaplib.FlagAnswered, imaplib.FlagFlagged,
				imaplib.FlagDeleted, imaplib.FlagSeen, imaplib.FlagDraft,
			}
			mbFlags := make([]imaplib.Flag, len(sysFlags), len(sysFlags)+len(s.knownKeywords)+len(newKws))
			copy(mbFlags, sysFlags)
			for kw := range s.knownKeywords {
				mbFlags = append(mbFlags, imaplib.Flag(kw))
			}
			for _, kw := range newKws {
				mbFlags = append(mbFlags, imaplib.Flag(kw))
				s.knownKeywords[kw] = struct{}{}
			}
			if err := w.WriteMailboxFlags(mbFlags); err != nil {
				return err
			}
		}
		for _, p := range pending {
			allFlags := make([]imaplib.Flag, 0, len(p.ci.flags)+len(p.ci.kw))
			for _, f := range p.ci.flags {
				allFlags = append(allFlags, imaplib.Flag(f))
			}
			for _, k := range p.ci.kw {
				allFlags = append(allFlags, imaplib.Flag(k))
			}
			if err := w.WriteMessageFlags(p.seq, imaplib.UID(p.uid), allFlags); err != nil {
				return err
			}
			s.knownMsgs[p.seq-1].modseq = p.ci.modseq
		}
	}

	// Phase 3: new messages — UIDs in current that are not yet in knownMsgs.
	knownSet := make(map[uint32]struct{}, len(s.knownMsgs))
	for _, km := range s.knownMsgs {
		knownSet[km.uid] = struct{}{}
	}
	added := 0
	var newKwsFromAppend []string
	newKwSetFromAppend := make(map[string]struct{})
	for _, m := range current {
		if _, seen := knownSet[m.UID]; seen {
			continue
		}
		s.knownMsgs = append(s.knownMsgs, sessionMsg{uid: m.UID, modseq: m.ModSeq})
		added++
		for _, kw := range m.Keywords {
			if _, known := s.knownKeywords[kw]; known {
				continue
			}
			if _, dup := newKwSetFromAppend[kw]; dup {
				continue
			}
			newKwsFromAppend = append(newKwsFromAppend, kw)
			newKwSetFromAppend[kw] = struct{}{}
		}
	}
	if len(newKwsFromAppend) > 0 {
		sysFlags := []imaplib.Flag{
			imaplib.FlagAnswered, imaplib.FlagFlagged,
			imaplib.FlagDeleted, imaplib.FlagSeen, imaplib.FlagDraft,
		}
		mbFlags := make([]imaplib.Flag, len(sysFlags), len(sysFlags)+len(s.knownKeywords)+len(newKwsFromAppend))
		copy(mbFlags, sysFlags)
		for kw := range s.knownKeywords {
			mbFlags = append(mbFlags, imaplib.Flag(kw))
		}
		for _, kw := range newKwsFromAppend {
			mbFlags = append(mbFlags, imaplib.Flag(kw))
			s.knownKeywords[kw] = struct{}{}
		}
		if err := w.WriteMailboxFlags(mbFlags); err != nil {
			return err
		}
	}
	if added > 0 {
		if err := w.WriteNumMessages(uint32(len(s.knownMsgs))); err != nil {
			return err
		}
	}

	// Only advance syncModSeq when we have fully delivered all expunges.
	// If hasPendingExpunge is true, leave syncModSeq unchanged so the next
	// call (with allowExpunge=true) re-reads the index and retries.
	if !s.hasPendingExpunge {
		s.syncModSeq = refreshed.HighestModSeq
	}
	return nil
}

func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	interval := s.srv.opts.IdleNotifyInterval
	if s.folder == nil {
		<-stop
		return nil
	}

	// Cross-pod event subscription: when another process (LMTP delivery, an
	// IMAP session on a sibling pod, etc.) writes to this user's folder, we
	// get an EVENT, refresh the message count from disk, and push the
	// EXISTS notification immediately — no waiting for the timer.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var events <-chan locks.Event
	if s.srv.opts.Locker != nil && s.userInfo != nil {
		ch, err := s.srv.opts.Locker.Subscribe(ctx, locks.MailboxKey(s.userInfo.Username, s.folder.Name))
		if err != nil {
			slog.Debug("imap: idle subscribe failed; falling back to timer-only", "err", err)
		} else {
			events = ch
		}
	}

	// Heartbeat tick — required only when there is no event channel; the
	// timer is still useful as a liveness signal for misbehaving clients
	// even when the subscription is up.
	var tickC <-chan time.Time
	if interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		tickC = ticker.C
	}

	if events == nil && tickC == nil {
		// Locker disabled and no heartbeat configured — purely passive IDLE.
		<-stop
		return nil
	}

	for {
		select {
		case <-stop:
			return nil
		case _, ok := <-events:
			if !ok {
				events = nil // subscription dropped; keep heartbeat going
				continue
			}
			if err := s.refreshIdleCount(w); err != nil {
				return err
			}
		case <-tickC:
			if err := w.WriteNumMessages(s.folder.Messages); err != nil {
				return err
			}
		}
	}
}

// refreshIdleCount re-reads the selected folder's message count from the
// index and writes EXISTS. Used by IDLE after a cross-pod EVENT — the
// in-memory s.folder.Messages may be stale if another process appended.
func (s *session) refreshIdleCount(w *imapserver.UpdateWriter) error {
	if s.folder == nil {
		return nil
	}
	refreshed, err := s.folderIdx().OpenFolder(s.folder.Name, s.folder.UIDValidity)
	if err != nil {
		// Best-effort: report what we have. Authoritative state lives on disk
		// and the next user command will re-read it.
		return w.WriteNumMessages(s.folder.Messages)
	}
	s.folder.Messages = refreshed.Messages
	return w.WriteNumMessages(refreshed.Messages)
}

func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imaplib.UIDSet) error {
	tExpunge := time.Now()
	if s.folder == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	if err := s.requireRightOnSelected(mailbox.RightExpunge); err != nil {
		return err
	}
	idx := s.folderIdx()
	msgs, err := idx.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return err
	}
	// Track the current sequence number separately: each expunged message
	// shifts all subsequent sequence numbers down by one, so we must adjust
	// as we go rather than using the static position from GetMessages.
	seqNum := uint32(len(msgs))
	var expunge_count int
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if !hasFlag(m.Flags, `\Deleted`) {
			seqNum--
			continue
		}
		if uids != nil && !uids.Contains(imaplib.UID(m.UID)) {
			seqNum--
			continue
		}
		idx.ExpungeMessage(s.folder.ID, m.UID) //nolint:errcheck
		s.emitMailboxChange(s.folder.Name, locks.EventExpunged, m.UID)
		s.statsExpunged++
		expunge_count++
		if err := w.WriteExpunge(seqNum); err != nil {
			return err
		}
		// Remove from knownMsgs so Poll does not re-deliver this expunge.
		kIdx := int(seqNum) - 1
		if kIdx >= 0 && kIdx < len(s.knownMsgs) {
			s.knownMsgs = append(s.knownMsgs[:kIdx], s.knownMsgs[kIdx+1:]...)
		}
		seqNum--
	}
	slog.Debug("imap: expunge timing",
		"user", s.userInfo.Username, "folder", s.folder.Name,
		"count", expunge_count, "total_ms", time.Since(tExpunge).Milliseconds())
	return nil
}

func (s *session) Search(kind imapserver.NumKind, criteria *imaplib.SearchCriteria, opts *imaplib.SearchOptions) (*imaplib.SearchData, error) {
	if s.folder == nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	if err := s.requireRightOnSelected(mailbox.RightRead); err != nil {
		return nil, err
	}

	// SearchRes ($) substitution: when the client passes "$" as a UID set,
	// go-imap/v2 surfaces it as an imaplib.SearchRes()-tagged entry in
	// criteria.UID. We swap it for the saved set from the previous RETURN
	// SAVE so the matcher sees a concrete UID list.
	criteria = s.substituteSearchRes(criteria)

	msgs, err := s.folderIdx().GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return nil, err
	}

	needsBody := len(criteria.Header) > 0 || len(criteria.Body) > 0 || len(criteria.Text) > 0 ||
		!criteria.SentSince.IsZero() || !criteria.SentBefore.IsZero() || searchNeedsBodyRecurse(criteria.Not, criteria.Or)

	// Collect both representations — clients may want UID set OR sequence
	// numbers via RETURN ALL, while MIN/MAX/COUNT always operate on the
	// kind requested.
	var (
		uidHits    imaplib.UIDSet
		seqHits    imaplib.SeqSet
		first      uint32
		last       uint32
		hitCount   uint32
		highestMod uint64 // CONDSTORE MODSEQ across all matched messages
	)
	for i, m := range msgs {
		seqNum := uint32(i + 1)

		var rawMsg []byte
		if needsBody && m.Filename != "" {
			if rc, err := s.folderBox().Fetch(s.folder.Name, m.Filename, m.AltTier); err == nil {
				rawMsg, _ = io.ReadAll(rc)
				rc.Close()
			}
		}

		imapFlags := make([]imaplib.Flag, len(m.Flags)+len(m.Keywords))
		for j, f := range m.Flags {
			imapFlags[j] = imaplib.Flag(f)
		}
		for j, k := range m.Keywords {
			imapFlags[len(m.Flags)+j] = imaplib.Flag(k)
		}

		if !imapserver.MatchMessage(seqNum, imaplib.UID(m.UID), m.InternalDate, int64(m.Size), imapFlags, rawMsg, criteria) {
			continue
		}

		// CONDSTORE SEARCH MODSEQ filter — RFC 7162 §3.1.5.
		// MetadataName/MetadataType narrow which attribute's modseq to compare;
		// per-attribute modseq tracking is future work, so we treat any
		// criteria.ModSeq as "message-level modseq" and ignore the attribute
		// qualifier — strictly more permissive (returns extra matches), which
		// is RFC-acceptable.
		if criteria.ModSeq != nil && m.ModSeq < criteria.ModSeq.ModSeq {
			continue
		}
		hitCount++
		var current uint32
		if kind == imapserver.NumKindUID {
			current = m.UID
			uidHits.AddNum(imaplib.UID(m.UID))
		} else {
			current = seqNum
			seqHits.AddNum(seqNum)
		}
		if first == 0 || current < first {
			first = current
		}
		if current > last {
			last = current
		}
		if m.ModSeq > highestMod {
			highestMod = m.ModSeq
		}
	}

	data := &imaplib.SearchData{}
	// ESEARCH RETURN handling. Per RFC 4731: when RETURN is given, only the
	// requested data items are sent. When RETURN is omitted, send ALL by
	// default (legacy SEARCH response).
	wantAll := opts == nil ||
		(!opts.ReturnMin && !opts.ReturnMax && !opts.ReturnCount && !opts.ReturnAll && !opts.ReturnSave)
	if wantAll || (opts != nil && opts.ReturnAll) {
		if kind == imapserver.NumKindUID {
			data.All = uidHits
		} else {
			data.All = seqHits
		}
	}
	if opts != nil && opts.ReturnMin {
		data.Min = first
	}
	if opts != nil && opts.ReturnMax {
		data.Max = last
	}
	if opts != nil && opts.ReturnCount {
		data.Count = hitCount
	}
	// CONDSTORE — RFC 7162 §3.1.5. When any matched message carries a
	// modseq, surface the maximum so the client can persist its
	// "highest-seen modseq" for the folder. Emitted regardless of which
	// RETURN items were requested (the spec considers MODSEQ implicit
	// when SEARCH MODSEQ criteria are used, but it is also useful for
	// non-modseq searches against a CONDSTORE-enabled mailbox).
	if highestMod > 0 {
		data.ModSeq = highestMod
	}
	// SEARCHRES (RFC 5182): RETURN SAVE pins the hit set for later $ refs.
	// The spec says the saved set is always the UID-typed result; convert
	// from sequence numbers if SEARCH was issued in sequence-number mode.
	if opts != nil && opts.ReturnSave {
		if kind == imapserver.NumKindUID {
			s.savedSearchUIDs = uidHits
		} else {
			// Sequence-number SEARCH still saves UIDs (RFC 5182 §2.1).
			var saved imaplib.UIDSet
			for i, m := range msgs {
				var raw []byte
				if needsBody && m.Filename != "" {
					if rc, err := s.folderBox().Fetch(s.folder.Name, m.Filename, m.AltTier); err == nil {
						raw, _ = io.ReadAll(rc)
						rc.Close()
					}
				}
				imapFlags := make([]imaplib.Flag, len(m.Flags)+len(m.Keywords))
				for j, f := range m.Flags {
					imapFlags[j] = imaplib.Flag(f)
				}
				for j, k := range m.Keywords {
					imapFlags[len(m.Flags)+j] = imaplib.Flag(k)
				}
				if imapserver.MatchMessage(uint32(i+1), imaplib.UID(m.UID), m.InternalDate, int64(m.Size), imapFlags, raw, criteria) {
					saved.AddNum(imaplib.UID(m.UID))
				}
			}
			s.savedSearchUIDs = saved
		}
	}
	return data, nil
}

// substituteSearchRes walks criteria.UID looking for the SearchRes ($)
// marker and replaces it with the in-memory saved set from a previous
// RETURN SAVE. Returns the original criteria unchanged if no marker is
// present, so non-$ SEARCH calls pay no cost.
func (s *session) substituteSearchRes(criteria *imaplib.SearchCriteria) *imaplib.SearchCriteria {
	if criteria == nil {
		return criteria
	}
	needsSub := false
	for _, u := range criteria.UID {
		if imaplib.IsSearchRes(u) {
			needsSub = true
			break
		}
	}
	if !needsSub {
		return criteria
	}
	clone := *criteria
	clone.UID = make([]imaplib.UIDSet, 0, len(criteria.UID))
	for _, u := range criteria.UID {
		if imaplib.IsSearchRes(u) {
			clone.UID = append(clone.UID, s.savedSearchUIDs)
			continue
		}
		clone.UID = append(clone.UID, u)
	}
	return &clone
}

func (s *session) Fetch(w *imapserver.FetchWriter, numSet imaplib.NumSet, opts *imaplib.FetchOptions) error {
	if s.folder == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	if err := s.requireRightOnSelected(mailbox.RightRead); err != nil {
		return err
	}
	idx := s.folderIdx()
	box := s.folderBox()
	backendMsgs, err := idx.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return err
	}
	// UID FETCH (CHANGEDSINCE N VANISHED) — RFC 7162 §3.2.10. Emit
	// VANISHED (EARLIER) for UIDs expunged since the supplied modseq.
	// VANISHED on a sequence-number FETCH is invalid; the patched lib
	// rejects that at parse time so we do not need to re-check here.
	if opts.Vanished && opts.ChangedSince > 0 {
		vanishedUIDs, vErr := idx.Vanished(s.folder.ID, opts.ChangedSince)
		if vErr == nil && len(vanishedUIDs) > 0 {
			var vset imaplib.UIDSet
			for _, uid := range vanishedUIDs {
				vset.AddNum(imaplib.UID(uid))
			}
			if writeErr := w.WriteVanished(vset); writeErr != nil {
				return writeErr
			}
		}
	}

	// Build a UID→message lookup from the backend so we can resolve by UID
	// independently of the backend's current sequence numbering.
	backendByUID := make(map[uint32]*mailbox.MessageMeta, len(backendMsgs))
	for _, m := range backendMsgs {
		backendByUID[m.UID] = m
	}

	// For sequence-number FETCH: iterate knownMsgs (the client's current
	// seq→UID view). This prevents UID-changed mismatches when another
	// session has expunged messages and the backend's sequence numbers have
	// shifted. Messages expunged since the last Poll are silently skipped
	// here; pollExpunge delivers the EXPUNGE notifications after the tagged
	// response so the client can reconcile its view.
	//
	// For UID FETCH: use the backend's sequence numbering directly — UIDs
	// are stable identifiers and the client matches by UID, not seq.
	type fetchEntry struct {
		seqNum uint32
		msg    *mailbox.MessageMeta
	}
	// Build uid→client-seqNum from knownMsgs so both seq-number and UID
	// FETCH report sequence numbers that match the client's current view.
	// This avoids "UID changed" errors when another session has expunged
	// messages and the backend's positions have shifted before the client
	// received the EXPUNGE notifications.
	uidToClientSeq := make(map[uint32]uint32, len(s.knownMsgs))
	for i, km := range s.knownMsgs {
		uidToClientSeq[km.uid] = uint32(i + 1)
	}

	var fetchList []fetchEntry
	if _, isUID := numSet.(imaplib.UIDSet); isUID {
		for _, m := range backendMsgs {
			seqNum, ok := uidToClientSeq[m.UID]
			if !ok {
				// New message appended after the pre-OK poll; skip it
				// — client will learn about it on the next Poll cycle.
				continue
			}
			fetchList = append(fetchList, fetchEntry{seqNum, m})
		}
	} else {
		for i, km := range s.knownMsgs {
			m, ok := backendByUID[km.uid]
			if !ok {
				continue
			}
			fetchList = append(fetchList, fetchEntry{uint32(i + 1), m})
		}
	}

	// RFC 3501 §6.4.5 — BODY[] without .PEEK implicitly sets \Seen.
	// Compute once per FETCH command (it's a property of the request, not each message).
	markSeen := false
	for _, sec := range opts.BodySection {
		if !sec.Peek {
			markSeen = true
			break
		}
	}
	for _, fe := range fetchList {
		m := fe.msg
		// CHANGEDSINCE filter — skip messages whose modseq has not moved
		// past the client's last-known value (RFC 4551 §3.3.1).
		if opts.ChangedSince > 0 && m.ModSeq <= opts.ChangedSince {
			continue
		}
		seqNum := fe.seqNum
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		mw := w.CreateMessage(seqNum)
		// Implicit \Seen: update index before writing FLAGS so the response
		// carries the new flag set (whether or not the client asked for FLAGS).
		seenJustSet := false
		if markSeen && !hasFlag(m.Flags, `\Seen`) {
			newFlags := append([]string(nil), m.Flags...)
			newFlags = append(newFlags, `\Seen`)
			if uerr := idx.UpdateFlags(s.folder.ID, m.UID, newFlags, m.Keywords); uerr == nil {
				m.Flags = newFlags
				seenJustSet = true
				s.emitMailboxChange(s.folder.Name, locks.EventChanged, m.UID)
			}
		}
		if opts.Flags || seenJustSet {
			mw.WriteFlags(toImapFlags(append(m.Flags, m.Keywords...)))
		}
		if opts.UID {
			mw.WriteUID(imaplib.UID(m.UID))
		}
		if opts.InternalDate {
			mw.WriteInternalDate(m.InternalDate)
		}
		if opts.RFC822Size {
			mw.WriteRFC822Size(int64(m.Size))
		}
		if opts.ModSeq && m.ModSeq > 0 {
			mw.WriteModSeq(m.ModSeq)
		}
		if opts.Envelope && m.Filename != "" {
			if rc, ferr := box.Fetch(s.folder.Name, m.Filename, m.AltTier); ferr == nil {
				hdr, _ := textproto.ReadHeader(bufio.NewReader(rc))
				rc.Close()
				mw.WriteEnvelope(imapserver.ExtractEnvelope(hdr))
			}
		}
		if opts.BodyStructure != nil && m.Filename != "" {
			if rc, ferr := box.Fetch(s.folder.Name, m.Filename, m.AltTier); ferr == nil {
				bs := imapserver.ExtractBodyStructure(rc)
				rc.Close()
				mw.WriteBodyStructure(bs)
			}
		}
		for _, section := range opts.BodySection {
			if m.Filename == "" {
				if slog.Default().Enabled(context.Background(), slog.LevelDebug) &&
					section.Specifier == imaplib.PartSpecifierNone && len(section.Part) == 0 {
					slog.Debug("imap: fetch body[] no filename",
						"user", s.userInfo.Username,
						"folder", s.folder.Name,
						"uid", m.UID,
					)
				}
				break
			}
			rc, ferr := box.Fetch(s.folder.Name, m.Filename, m.AltTier)
			if ferr != nil {
				if slog.Default().Enabled(context.Background(), slog.LevelDebug) &&
					section.Specifier == imaplib.PartSpecifierNone && len(section.Part) == 0 {
					slog.Debug("imap: fetch body[] file error",
						"user", s.userInfo.Username,
						"folder", s.folder.Name,
						"uid", m.UID,
						"file", m.Filename,
						"err", ferr,
					)
				}
				break
			}
			extracted := imapserver.ExtractBodySection(rc, section)
			rc.Close()
			if extracted == nil {
				extracted = []byte{}
			}
			if slog.Default().Enabled(context.Background(), slog.LevelDebug) &&
				section.Specifier == imaplib.PartSpecifierNone && len(section.Part) == 0 {
				sum := md5.Sum(extracted)
				slog.Debug("imap: fetch body[]",
					"user", s.userInfo.Username,
					"folder", s.folder.Name,
					"uid", m.UID,
					"file", m.Filename,
					"size", len(extracted),
					"md5", fmt.Sprintf("%x", sum),
				)
			}
			switch section.Specifier {
			case imaplib.PartSpecifierHeader, imaplib.PartSpecifierMIME:
				s.statsFetchHdr++
				s.statsFetchHdrB += int64(len(extracted))
			default:
				s.statsFetchBody++
				s.statsFetchBodyB += int64(len(extracted))
			}
			bw := mw.WriteBodySection(section, int64(len(extracted)))
			io.Copy(bw, bytes.NewReader(extracted)) //nolint:errcheck
			bw.Close()
		}
		// BINARY[] (RFC 3516) — decode Content-Transfer-Encoding (base64,
		// quoted-printable) so the client gets the raw bytes. Without a
		// part spec we decode message-level CTE; multipart-walk (BINARY[1])
		// returns the section unchanged when MIME parsing is non-trivial.
		for _, section := range opts.BinarySection {
			if m.Filename == "" {
				break
			}
			rc, ferr := box.Fetch(s.folder.Name, m.Filename, m.AltTier)
			if ferr != nil {
				break
			}
			body, _ := io.ReadAll(rc)
			rc.Close()
			decoded := decodeBinarySection(body, section.Part)
			s.statsFetchBody++
			s.statsFetchBodyB += int64(len(decoded))
			bw := mw.WriteBinarySection(section, int64(len(decoded)))
			io.Copy(bw, bytes.NewReader(decoded)) //nolint:errcheck
			bw.Close()
		}
		// BINARY.SIZE[] — same decode, return size only.
		for _, section := range opts.BinarySectionSize {
			if m.Filename == "" {
				break
			}
			rc, ferr := box.Fetch(s.folder.Name, m.Filename, m.AltTier)
			if ferr != nil {
				break
			}
			body, _ := io.ReadAll(rc)
			rc.Close()
			decoded := decodeBinarySection(body, section.Part)
			mw.WriteBinarySectionSize(section, uint32(len(decoded)))
		}
		mw.Close() //nolint:errcheck
	}
	return nil
}

func (s *session) Store(w *imapserver.FetchWriter, numSet imaplib.NumSet, storeFlags *imaplib.StoreFlags, opts *imaplib.StoreOptions) error {
	tStore := time.Now()
	if s.folder == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	if storeFlags != nil {
		if err := s.requireAllRightsOnSelected(storeFlagRights(storeFlags.Flags)); err != nil {
			return err
		}
	}
	idx := s.folderIdx()
	msgs, err := idx.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return err
	}
	tUpdate := time.Now()

	uidToClientSeq := make(map[uint32]uint32, len(s.knownMsgs))
	for i, km := range s.knownMsgs {
		uidToClientSeq[km.uid] = uint32(i + 1)
	}

	// CONDSTORE STORE (UNCHANGEDSINCE N) — RFC 7162 §3.1.3.
	// Any message whose current modseq is greater than the client's
	// last-known value is *skipped* (no flag update, no FETCH response).
	// The list of skipped UIDs is returned as the MODIFIED response code
	// after STORE completes so the client can re-sync those messages.
	unchangedSince := uint64(0)
	if opts != nil {
		unchangedSince = opts.UnchangedSince
	}
	var modifiedUIDs imaplib.UIDSet

	// Pass 1: determine which messages to update and compute new flag sets.
	type pendingStore struct {
		seqNum   uint32
		uid      uint32
		newFlags []string
		newKW    []string
	}
	var pending []pendingStore
	batchUpdates := make(map[uint32]mailbox.FlagsUpdate)

	for _, m := range msgs {
		seqNum, ok := uidToClientSeq[m.UID]
		if !ok {
			continue
		}
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		if unchangedSince > 0 && m.ModSeq > unchangedSince {
			modifiedUIDs.AddNum(imaplib.UID(m.UID))
			continue
		}
		current := append(m.Flags, m.Keywords...)
		wasDeleted := hasFlag(current, `\Deleted`)
		allNew := applyStoreFlags(current, storeFlags)
		if !wasDeleted && hasFlag(allNew, `\Deleted`) {
			s.statsDeleted++
		}
		var newFlags, newKW []string
		for _, f := range allNew {
			if strings.HasPrefix(f, `\`) {
				newFlags = append(newFlags, f)
			} else {
				newKW = append(newKW, f)
			}
		}
		pending = append(pending, pendingStore{seqNum, m.UID, newFlags, newKW})
		batchUpdates[m.UID] = mailbox.FlagsUpdate{Flags: newFlags, Keywords: newKW}
	}

	// Pass 2: single lock/reload/flush for all flag updates.
	var newModSeqs map[uint32]uint64
	if len(batchUpdates) > 0 {
		var err error
		newModSeqs, err = idx.UpdateFlagsMulti(s.folder.ID, batchUpdates)
		if err != nil {
			return err
		}
		s.emitMailboxChange(s.folder.Name, locks.EventChanged, 0)
	}
	slog.Debug("imap: store timing",
		"user", s.userInfo.Username, "folder", s.folder.Name, "count", len(batchUpdates),
		"getmsgs_ms", tUpdate.Sub(tStore).Milliseconds(),
		"update_ms", time.Since(tUpdate).Milliseconds(),
		"total_ms", time.Since(tStore).Milliseconds(),
	)

	// Pass 3: send FETCH responses using modseqs returned from the batch.
	// Also update knownMsgs.modseq so the post-command Poll skips these
	// messages — without this Poll would see modseq changed and emit a
	// second duplicate * FETCH for every STOREd message.
	for i := range s.knownMsgs {
		if ms, ok := newModSeqs[s.knownMsgs[i].uid]; ok {
			s.knownMsgs[i].modseq = ms
		}
	}
	if !storeFlags.Silent {
		for _, p := range pending {
			mw := w.CreateMessage(p.seqNum)
			mw.WriteFlags(toImapFlags(append(p.newFlags, p.newKW...)))
			mw.WriteUID(imaplib.UID(p.uid))
			if ms := newModSeqs[p.uid]; ms > 0 {
				mw.WriteModSeq(ms)
			}
			mw.Close() //nolint:errcheck
		}
	}

	if len(modifiedUIDs) > 0 {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeOK,
			Code: imaplib.ResponseCode(string(imaplib.ResponseCodeModified) + " " + modifiedUIDs.String()),
			Text: "Some messages had a modseq greater than the supplied UNCHANGEDSINCE value",
		}
	}
	return nil
}

func (s *session) Copy(numSet imaplib.NumSet, dest string) (*imaplib.CopyData, error) {
	tCopy := time.Now()
	if s.folder == nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	if err := s.requireRightOnSelected(mailbox.RightRead); err != nil {
		return nil, err
	}
	srcIdx := s.folderIdx()
	srcBox := s.folderBox()
	destH, destRel, destFolder, err := s.ensureFolderHandle(dest)
	if err != nil {
		return nil, err
	}
	exists, err := destH.box.FolderExists(destRel)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCodeTryCreate, Text: "No such mailbox"}
	}
	if err := s.requireRight(destH, destRel, insertRight(destH.spec)); err != nil {
		return nil, err
	}
	msgs, err := srcIdx.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return nil, err
	}
	copyUIDToClientSeq := make(map[uint32]uint32, len(s.knownMsgs))
	for i, km := range s.knownMsgs {
		copyUIDToClientSeq[km.uid] = uint32(i + 1)
	}
	var srcUIDs, dstUIDs imaplib.UIDSet
	var saveTotalMs, indexTotalMs int64
	var count int
	for _, m := range msgs {
		seqNum, ok := copyUIDToClientSeq[m.UID]
		if !ok {
			continue
		}
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		rc, fetchErr := srcBox.Fetch(s.folder.Name, m.Filename, m.AltTier)
		if fetchErr != nil {
			return nil, fmt.Errorf("imap/copy fetch: %w", fetchErr)
		}
		data, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			return nil, fmt.Errorf("imap/copy read: %w", readErr)
		}
		tSave := time.Now()
		newFilename, saveErr := destH.box.Save(destRel, bytes.NewReader(data), 0, int64(len(data)), m.Flags)
		if saveErr != nil {
			return nil, fmt.Errorf("imap/copy save: %w", saveErr)
		}
		saveTotalMs += time.Since(tSave).Milliseconds()
		nm := &mailbox.MessageMeta{
			Filename:     newFilename,
			Flags:        m.Flags,
			Keywords:     m.Keywords,
			Size:         uint32(len(data)),
			InternalDate: m.InternalDate,
		}
		tIndex := time.Now()
		if err := destH.idx.AllocateAndAppend(destFolder.ID, nm); err != nil {
			_ = destH.box.Remove(destRel, newFilename)
			return nil, fmt.Errorf("imap/copy record: %w", err)
		}
		indexTotalMs += time.Since(tIndex).Milliseconds()
		count++
		s.emitMailboxChange(dest, locks.EventDelivered, nm.UID)
		srcUIDs.AddNum(imaplib.UID(m.UID))
		dstUIDs.AddNum(imaplib.UID(nm.UID))
	}
	slog.Debug("imap: copy timing",
		"user", s.userInfo.Username, "src", s.folder.Name, "dst", dest,
		"count", count, "save_ms", saveTotalMs, "index_ms", indexTotalMs,
		"total_ms", time.Since(tCopy).Milliseconds())
	return &imaplib.CopyData{
		UIDValidity: destFolder.UIDValidity,
		SourceUIDs:  srcUIDs,
		DestUIDs:    dstUIDs,
	}, nil
}

func (s *session) Namespace() (*imaplib.NamespaceData, error) {
	specs := s.srv.opts.Namespaces
	if len(specs) == 0 {
		specs = defaultNamespaces
	}
	var data imaplib.NamespaceData
	for _, ns := range specs {
		if !ns.List {
			continue
		}
		desc := imaplib.NamespaceDescriptor{Prefix: ns.Prefix, Delim: ns.Separator}
		switch ns.Type {
		case NamespacePersonal:
			data.Personal = append(data.Personal, desc)
		case NamespaceOther:
			data.Other = append(data.Other, desc)
		case NamespaceShared:
			data.Shared = append(data.Shared, desc)
		}
	}
	return &data, nil
}

// defaultNamespaces is the backwards-compatible fallback applied when
// Options.Namespaces is empty: a single personal namespace with the
// "/" separator, matching pre-v1.20 single-namespace behaviour.
var defaultNamespaces = []NamespaceSpec{
	{Type: NamespacePersonal, Prefix: "", Separator: '/', List: true},
}

// GetMetadata implements RFC 5464 GETMETADATA. mailbox == "" requests
// server-scope annotations (stored under INBOX's GUID with a vendor
// prefix so they cannot collide with INBOX's own mailbox attributes).
// Per RFC 5464, options.Depth controls whether entries below the
// requested name are included (0 = exact, 1 = direct children,
// infinity = whole subtree); options.MaxSize lets the server elide
// entries larger than the supplied byte cap.
func (s *session) GetMetadata(folder string, entries []string, opts *imaplib.GetMetadataOptions) (*imaplib.GetMetadataData, error) {
	if s.srv.opts.MetadataDict == nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata storage not configured"}
	}
	h, guid, err := s.metadataResolve(folder)
	if err != nil {
		return nil, err
	}
	depth := imaplib.GetMetadataDepthZero
	var maxSize *uint32
	if opts != nil {
		depth = opts.Depth
		maxSize = opts.MaxSize
	}
	out := map[string]*[]byte{}
	for _, entry := range entries {
		scope, attrName, err := mailbox.ParseAttrEntry(entry)
		if err != nil {
			return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeBad, Text: err.Error()}
		}
		if err := s.collectMetadata(h, folder, guid, scope, attrName, entry, depth, maxSize, out); err != nil {
			return nil, err
		}
	}
	return &imaplib.GetMetadataData{Mailbox: folder, Entries: out}, nil
}

// collectMetadata pulls either an exact key or a prefix iteration into out.
// Depth 0 = the entry itself; Depth 1 = entry + immediate children;
// Depth Infinity = entry + whole subtree.
func (s *session) collectMetadata(
	h *nsHandle, folder string, guid [16]byte,
	scope mailbox.AttrScope, attrName, requestedEntry string,
	depth imaplib.GetMetadataDepth, maxSize *uint32,
	out map[string]*[]byte,
) error {
	ctx := context.Background()
	ops := s.metadataOps()

	exactKey := s.metadataKey(h, folder, guid, scope, attrName)
	exactVals, found, err := s.srv.opts.MetadataDict.Lookup(ctx, ops, exactKey)
	if err != nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata lookup failed: " + err.Error()}
	}
	if found && len(exactVals) > 0 {
		v := exactVals[0]
		if maxSize == nil || uint32(len(v)) <= *maxSize {
			out[requestedEntry] = &v
		}
	}

	if depth == imaplib.GetMetadataDepthZero {
		return nil
	}

	flags := dict.IterSortByKey
	if depth == imaplib.GetMetadataDepthInfinity {
		flags |= dict.IterRecurse
	}
	prefix := s.metadataPrefix(h, folder, guid, scope) + attrName + "/"
	it, err := s.srv.opts.MetadataDict.Iterate(ctx, ops, prefix, flags)
	if err != nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata iterate failed: " + err.Error()}
	}
	defer it.Close() //nolint:errcheck
	stripPrefix := s.metadataPrefix(h, folder, guid, scope)
	for it.Next() {
		k := it.Key()
		vs := it.Values()
		if len(vs) == 0 {
			continue
		}
		v := vs[0]
		if maxSize != nil && uint32(len(v)) > *maxSize {
			continue
		}
		entryName := mailbox.TrimAttrPrefix(k, stripPrefix)
		if entryName == "" {
			continue
		}
		out[mailbox.FormatAttrEntry(scope, entryName)] = &v
	}
	return it.Err()
}

// SetMetadata implements RFC 5464 SETMETADATA. A nil value in entries
// means "remove that attribute" (per the spec). Server-scope ops use
// mailbox == "".
func (s *session) SetMetadata(folder string, entries map[string]*[]byte) error {
	if s.srv.opts.MetadataDict == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata storage not configured"}
	}
	h, guid, err := s.metadataResolve(folder)
	if err != nil {
		return err
	}
	ctx := context.Background()
	tx, err := s.srv.opts.MetadataDict.Begin(ctx, s.metadataOps())
	if err != nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata begin failed: " + err.Error()}
	}
	for entry, value := range entries {
		scope, attrName, err := mailbox.ParseAttrEntry(entry)
		if err != nil {
			_ = tx.Rollback()
			return &imaplib.Error{Type: imaplib.StatusResponseTypeBad, Text: err.Error()}
		}
		key := s.metadataKey(h, folder, guid, scope, attrName)
		if value == nil {
			if err := tx.Unset(key); err != nil {
				_ = tx.Rollback()
				return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata unset failed: " + err.Error()}
			}
			continue
		}
		if err := tx.Set(key, *value); err != nil {
			_ = tx.Rollback()
			return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata set failed: " + err.Error()}
		}
	}
	res, err := tx.Commit()
	if err != nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata commit failed: " + err.Error()}
	}
	if res != dict.CommitOK {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata commit returned " + strconv.Itoa(int(res))}
	}
	return nil
}

// metadataResolve returns the namespace handle and folder GUID used
// for keying the requested folder's attributes. Server-scope ops
// (folder == "") hash under the personal INBOX's GUID — server-scope
// state is per-user, never per-namespace.
func (s *session) metadataResolve(folder string) (*nsHandle, [16]byte, error) {
	target := folder
	if target == "" {
		target = "INBOX"
	}
	h, rel, err := s.dispatch(target)
	if err != nil {
		return nil, [16]byte{}, err
	}
	f, err := h.idx.OpenFolder(rel, uint32(time.Now().Unix()))
	if err != nil {
		return nil, [16]byte{}, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Mailbox lookup failed: " + err.Error()}
	}
	if f.GUID == ([16]byte{}) {
		return nil, [16]byte{}, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Mailbox missing GUID"}
	}
	return h, f.GUID, nil
}

func (s *session) metadataKey(h *nsHandle, folder string, guid [16]byte, scope mailbox.AttrScope, attrName string) string {
	if folder == "" {
		return mailbox.ServerAttrKey(scope, guid, attrName)
	}
	if h == nil || h == s.primary {
		return mailbox.AttrKey(scope, guid, attrName)
	}
	// Shared / public namespaces — priv/ keys carry an accessing-user
	// dimension so users do not see each other's private annotations
	// on the same shared folder. shared/ keys are global to the
	// folder regardless of accessing user.
	return mailbox.SharedAttrKey(scope, guid, s.userInfo.Username, attrName)
}

func (s *session) metadataPrefix(h *nsHandle, folder string, guid [16]byte, scope mailbox.AttrScope) string {
	if folder == "" {
		return mailbox.ServerAttrPrefix(scope, guid)
	}
	if h == nil || h == s.primary {
		return mailbox.AttrPrefix(scope, guid)
	}
	return mailbox.SharedAttrPrefix(scope, guid, s.userInfo.Username)
}

func (s *session) metadataOps() *dict.OpSettings {
	if s.userInfo == nil {
		return nil
	}
	return &dict.OpSettings{
		Username: s.userInfo.Username,
		HomeDir:  s.userInfo.Home,
	}
}

func (s *session) Move(w *imapserver.MoveWriter, numSet imaplib.NumSet, dest string) error {
	tMove := time.Now()
	if s.folder == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	// MOVE = COPY + STORE \Deleted + EXPUNGE on the source, so the
	// caller must hold r on the source (to read the message), t (to
	// delete it), and e (to expunge it); plus i/p on the destination.
	if err := s.requireAllRightsOnSelected([]rune{
		mailbox.RightRead, mailbox.RightDeleteMessage, mailbox.RightExpunge,
	}); err != nil {
		return err
	}
	srcIdx := s.folderIdx()
	srcBox := s.folderBox()
	destH, destRel, destFolder, err := s.ensureFolderHandle(dest)
	if err != nil {
		return err
	}
	exists, err := destH.box.FolderExists(destRel)
	if err != nil {
		return err
	}
	if !exists {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCodeTryCreate, Text: "No such mailbox"}
	}
	if err := s.requireRight(destH, destRel, insertRight(destH.spec)); err != nil {
		return err
	}
	msgs, err := srcIdx.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return err
	}

	moveUIDToClientSeq := make(map[uint32]uint32, len(s.knownMsgs))
	for i, km := range s.knownMsgs {
		moveUIDToClientSeq[km.uid] = uint32(i + 1)
	}

	type matched struct {
		seqNum   uint32
		srcUID   uint32
		filename string
	}
	var hits []matched
	var srcUIDs, dstUIDs imaplib.UIDSet
	var saveTotalMs, indexTotalMs int64

	for _, m := range msgs {
		seqNum, ok := moveUIDToClientSeq[m.UID]
		if !ok {
			continue
		}
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		rc, fetchErr := srcBox.Fetch(s.folder.Name, m.Filename, m.AltTier)
		if fetchErr != nil {
			return fmt.Errorf("imap/move fetch: %w", fetchErr)
		}
		data, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			return fmt.Errorf("imap/move read: %w", readErr)
		}
		tSave := time.Now()
		newFilename, saveErr := destH.box.Save(destRel, bytes.NewReader(data), 0, int64(len(data)), m.Flags)
		if saveErr != nil {
			return fmt.Errorf("imap/move save: %w", saveErr)
		}
		saveTotalMs += time.Since(tSave).Milliseconds()
		nm := &mailbox.MessageMeta{
			Filename:     newFilename,
			Flags:        m.Flags,
			Keywords:     m.Keywords,
			Size:         uint32(len(data)),
			InternalDate: m.InternalDate,
		}
		tIndex := time.Now()
		if err := destH.idx.AllocateAndAppend(destFolder.ID, nm); err != nil {
			_ = destH.box.Remove(destRel, newFilename)
			return fmt.Errorf("imap/move record: %w", err)
		}
		indexTotalMs += time.Since(tIndex).Milliseconds()
		s.emitMailboxChange(dest, locks.EventDelivered, nm.UID)
		srcUIDs.AddNum(imaplib.UID(m.UID))
		dstUIDs.AddNum(imaplib.UID(nm.UID))
		hits = append(hits, matched{seqNum: seqNum, srcUID: m.UID, filename: m.Filename})
	}

	if err := w.WriteCopyData(&imaplib.CopyData{
		UIDValidity: destFolder.UIDValidity,
		SourceUIDs:  srcUIDs,
		DestUIDs:    dstUIDs,
	}); err != nil {
		return err
	}

	// Expunge source in descending seq order (RFC 6851 §3.3).
	for i := len(hits) - 1; i >= 0; i-- {
		h := hits[i]
		srcBox.Remove(s.folder.Name, h.filename)     //nolint:errcheck
		srcIdx.ExpungeMessage(s.folder.ID, h.srcUID) //nolint:errcheck
		s.emitMailboxChange(s.folder.Name, locks.EventExpunged, h.srcUID)
		if err := w.WriteExpunge(h.seqNum); err != nil {
			return err
		}
		kIdx := int(h.seqNum) - 1
		if kIdx >= 0 && kIdx < len(s.knownMsgs) {
			s.knownMsgs = append(s.knownMsgs[:kIdx], s.knownMsgs[kIdx+1:]...)
		}
	}
	s.folder.Messages -= uint32(len(hits))
	srcIdx.SaveFolder(s.folder) //nolint:errcheck
	slog.Debug("imap: move timing",
		"user", s.userInfo.Username, "src", s.folder.Name, "dst", dest,
		"count", len(hits), "save_ms", saveTotalMs, "index_ms", indexTotalMs,
		"total_ms", time.Since(tMove).Milliseconds())
	return nil
}

// ---- session message tracker -----------------------------------------------

// sessionMsg is the server's copy of one message's state as last communicated
// to the IMAP client. The slice position+1 is the RFC 3501 sequence number.
type sessionMsg struct {
	uid    uint32
	modseq uint64
}

type slogLogger struct{}

func (l *slogLogger) Printf(format string, args ...interface{}) {
	slog.Error(fmt.Sprintf(format, args...))
}

// ensureFolderHandle resolves a wire-protocol mailbox name to its
// namespace handle, the namespace-relative folder name, and the
// opened *Folder. Used by ops that need to know which backend the
// folder lives on (APPEND, COPY, MOVE, METADATA). Re-uses the
// currently-SELECTed folder when name matches, to avoid re-OpenFolder
// round-trips inside short-lived ops.
func (s *session) ensureFolderHandle(name string) (*nsHandle, string, *mailbox.Folder, error) {
	h, rel, err := s.dispatch(name)
	if err != nil {
		return nil, "", nil, err
	}
	if s.folder != nil && s.folder.Name == rel && s.folderNS == h {
		return h, rel, s.folder, nil
	}
	f, err := h.idx.OpenFolder(rel, uint32(time.Now().Unix()))
	if err != nil {
		return nil, "", nil, err
	}
	return h, rel, f, nil
}

func listMatch(name string, patterns []string) bool {
	for _, p := range patterns {
		if p == "*" || p == "%" {
			return true
		}
		if strings.EqualFold(name, p) {
			return true
		}
	}
	return len(patterns) == 0
}

func hasFlag(flags []string, f string) bool {
	for _, fl := range flags {
		if fl == f {
			return true
		}
	}
	return false
}

func toImapFlags(flags []string) []imaplib.Flag {
	out := make([]imaplib.Flag, len(flags))
	for i, f := range flags {
		out[i] = imaplib.Flag(f)
	}
	return out
}

func numSetContains(numSet imaplib.NumSet, seqNum uint32, uid imaplib.UID) bool {
	switch ns := numSet.(type) {
	case imaplib.SeqSet:
		return ns.Contains(seqNum)
	case imaplib.UIDSet:
		return ns.Contains(uid)
	}
	return false
}

// searchNeedsBodyRecurse reports whether any criteria in the Not/Or lists
// requires the raw message body (Header, Body, Text, SentSince, SentBefore).
func searchNeedsBodyRecurse(not []imaplib.SearchCriteria, or [][2]imaplib.SearchCriteria) bool {
	for i := range not {
		if searchCriteriaHasBody(&not[i]) {
			return true
		}
	}
	for i := range or {
		if searchCriteriaHasBody(&or[i][0]) || searchCriteriaHasBody(&or[i][1]) {
			return true
		}
	}
	return false
}

func searchCriteriaHasBody(c *imaplib.SearchCriteria) bool {
	return len(c.Header) > 0 || len(c.Body) > 0 || len(c.Text) > 0 ||
		!c.SentSince.IsZero() || !c.SentBefore.IsZero() ||
		searchNeedsBodyRecurse(c.Not, c.Or)
}

func applyStoreFlags(current []string, store *imaplib.StoreFlags) []string {
	newFlags := make([]string, len(store.Flags))
	for i, f := range store.Flags {
		newFlags[i] = string(f)
	}
	switch store.Op {
	case imaplib.StoreFlagsSet:
		return newFlags
	case imaplib.StoreFlagsAdd:
		result := make([]string, len(current))
		copy(result, current)
		for _, f := range newFlags {
			if !hasFlag(result, f) {
				result = append(result, f)
			}
		}
		return result
	case imaplib.StoreFlagsDel:
		var result []string
		for _, cf := range current {
			if !hasFlag(newFlags, cf) {
				result = append(result, cf)
			}
		}
		return result
	}
	return current
}
