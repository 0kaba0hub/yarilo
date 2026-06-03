// Package imap wires go-imap/v2 to yarilo's mailbox and index backends.
package imap

import (
	"bytes"
	"context"
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
	"github.com/emersion/go-sasl"
	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/internal/auth/oauth2"
	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/connlimit"
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
	XClient            bool
	XClientTrustedNets []*net.IPNet
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
		imaplib.CapMetadata:         {},
		// CapMetadataServer not in opts.Caps: the fork's capability
		// allow-list does not echo it back, but SessionMetadata's
		// mailbox=="" path handles server-scope ops anyway, so the
		// behaviour is preserved without the wire-level cap atom.
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
	if s.opts.XClient {
		ln = &xclientImapListener{Listener: ln, trustedNets: s.opts.XClientTrustedNets}
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
	return sess, &imapserver.GreetingData{PreAuth: false}, nil
}

// ---- session ---------------------------------------------------------------

type session struct {
	srv      *Server
	imapConn *imapserver.Conn
	userInfo *mailbox.UserInfo
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
		slog.Info("imap: logout", "user", s.userInfo.Username, "stats", msg)
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
	res, err := s.srv.opts.Auth.Authenticate(username, password, "imap")
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
// PLAIN is unconditional; OAUTHBEARER lights up when the operator
// configured at least one OAuth provider — see Options.OAuth2Enabled.
func (s *session) AuthenticateMechanisms() []string {
	out := []string{sasl.Plain}
	if s.srv.opts.OAuth2Enabled {
		out = append(out, sasl.OAuthBearer)
	}
	return out
}

// Authenticate returns the SASL server for the requested mechanism.
// Custom PLAIN handler is used when the configured Authenticator
// implements protocol.MasterAuthenticator — otherwise we delegate
// the response shape (no authzid) to the standard Login path so
// stubs / non-master backends keep working. OAUTHBEARER routes the
// bearer token through the regular Authenticator surface (the OAuth
// passdb downstream extracts the token from req.Password).
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
	}
	return nil, &imaplib.Error{
		Type: imaplib.StatusResponseTypeNo,
		Text: "SASL mechanism not supported",
	}
}

// authenticateOAuthBearer is the OAuthBearerAuthenticator callback
// invoked by go-sasl after it has parsed the GS2 envelope. Wire-
// shape concerns (gs2-header parsing, RFC 7628 JSON error blob on
// failure) live entirely inside go-sasl; here we only translate
// (Username, Token) into the chain's Authenticate(user, password,
// service) call and surface a Bearer JSON error on rejection.
func (s *session) authenticateOAuthBearer(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
	res, err := s.srv.opts.Auth.Authenticate(opts.Username, opts.Token, "imap")
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
	if authzid == "" || authzid == authid {
		res, err := s.srv.opts.Auth.Authenticate(authid, password, "imap")
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
	res, err := master.AuthenticateMaster(authzid, authid, password, "imap")
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

	if lim := s.srv.opts.ConnLimit; lim != nil {
		ip := remoteIP(s.imapConn.NetConn())
		if !lim.Acquire(userInfo.Username, ip) {
			slog.Warn("imap: connection limit reached", "user", userInfo.Username, "ip", ip)
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
		"user", userInfo.Username,
		"master_user", master,
		"remoteIP", remoteIP(s.imapConn.NetConn()),
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
	f, err := h.idx.OpenFolder(rel, uint32(time.Now().Unix()))
	if err != nil {
		return nil, err
	}
	s.folder = f
	s.folderNS = h
	s.pushAnvilSelect(name)

	msgs, _ := h.idx.GetMessages(f.ID, mailbox.SeqSet{})
	data := &imaplib.SelectData{
		Flags: []imaplib.Flag{
			imaplib.FlagAnswered, imaplib.FlagFlagged,
			imaplib.FlagDeleted, imaplib.FlagSeen, imaplib.FlagDraft,
		},
		PermanentFlags: []imaplib.Flag{
			imaplib.FlagAnswered, imaplib.FlagFlagged,
			imaplib.FlagDeleted, imaplib.FlagSeen, imaplib.FlagDraft,
			imaplib.Flag(`\*`),
		},
		NumMessages:   uint32(len(msgs)),
		UIDValidity:   f.UIDValidity,
		UIDNext:       imaplib.UID(f.NextUID),
		HighestModSeq: f.HighestModSeq,
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
		rc, fetchErr := s.box.Fetch("INBOX", m.Filename)
		if fetchErr != nil {
			return fmt.Errorf("imap/rename-inbox fetch: %w", fetchErr)
		}
		data, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			return fmt.Errorf("imap/rename-inbox read: %w", readErr)
		}
		newUID, allocErr := s.idx.AllocateUID(destFolder.ID)
		if allocErr != nil {
			return fmt.Errorf("imap/rename-inbox allocate: %w", allocErr)
		}
		modseq, _ := s.idx.NextModSeq(destFolder.ID)
		newFilename, saveErr := s.box.Save(dest, bytes.NewReader(data), newUID, int64(len(data)), m.Flags)
		if saveErr != nil {
			return fmt.Errorf("imap/rename-inbox save: %w", saveErr)
		}
		if err := s.idx.AppendMessage(destFolder.ID, &mailbox.MessageMeta{
			UID:          newUID,
			Filename:     newFilename,
			Flags:        m.Flags,
			Keywords:     m.Keywords,
			ModSeq:       modseq,
			Size:         uint32(len(data)),
			InternalDate: m.InternalDate,
		}); err != nil {
			_ = s.box.Remove(dest, newFilename)
			return fmt.Errorf("imap/rename-inbox record: %w", err)
		}
		s.emitMailboxChange(dest, locks.EventDelivered, newUID)
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
	folders, err := h.box.ListFolders()
	if err != nil {
		return err
	}

	// Snapshot subscriptions once per LIST — every folder's ReturnSubscribed
	// / SelectSubscribed decision consults the same view, even if a sibling
	// session SUBSCRIBE'd mid-iteration.
	var subs map[string]struct{}
	if h.subs != nil && (opts != nil && (opts.SelectSubscribed || opts.ReturnSubscribed)) {
		subs, err = h.subs.Snapshot()
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
	msgs, _ := h.idx.GetMessages(f.ID, mailbox.SeqSet{})
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
	return d, nil
}

func (s *session) Append(name string, r imaplib.LiteralReader, opts *imaplib.AppendOptions) (*imaplib.AppendData, error) {
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
	uid, err := h.idx.AllocateUID(f.ID)
	if err != nil {
		return nil, fmt.Errorf("imap/append allocate: %w", err)
	}
	modseq, _ := h.idx.NextModSeq(f.ID)

	filename, err := h.box.Save(rel, r, uid, size, flagList)
	if err != nil {
		return nil, err
	}
	if err := h.idx.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: uid, Filename: filename, Flags: flagList, Keywords: kwList, ModSeq: modseq, Size: uint32(size),
	}); err != nil {
		_ = h.box.Remove(rel, filename)
		return nil, fmt.Errorf("imap/append record: %w", err)
	}
	s.emitMailboxChange(name, locks.EventDelivered, uid)

	return &imaplib.AppendData{UIDValidity: f.UIDValidity, UID: imaplib.UID(uid)}, nil
}

func (s *session) Poll(_ *imapserver.UpdateWriter, _ bool) error { return nil }

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
	for _, m := range msgs {
		if !hasFlag(m.Flags, `\Deleted`) {
			continue
		}
		if uids != nil && !uids.Contains(imaplib.UID(m.UID)) {
			continue
		}
		idx.ExpungeMessage(s.folder.ID, m.UID) //nolint:errcheck
		s.emitMailboxChange(s.folder.Name, locks.EventExpunged, m.UID)
		s.statsExpunged++
		if err := w.WriteExpunge(m.UID); err != nil {
			return err
		}
	}
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
		if !matchesCriteriaSeq(m, seqNum, criteria) {
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
				if matchesCriteriaSeq(m, uint32(i+1), criteria) {
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
	msgs, err := idx.GetMessages(s.folder.ID, mailbox.SeqSet{})
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
	for i, m := range msgs {
		// CHANGEDSINCE filter — skip messages whose modseq has not moved
		// past the client's last-known value (RFC 4551 §3.3.1).
		if opts.ChangedSince > 0 && m.ModSeq <= opts.ChangedSince {
			continue
		}
		seqNum := uint32(i + 1)
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		mw := w.CreateMessage(seqNum)
		if opts.Flags {
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
		for _, section := range opts.BodySection {
			if m.Filename == "" {
				break
			}
			rc, ferr := box.Fetch(s.folder.Name, m.Filename)
			if ferr != nil {
				break
			}
			body, _ := io.ReadAll(rc)
			rc.Close()
			switch section.Specifier {
			case imaplib.PartSpecifierHeader, imaplib.PartSpecifierMIME:
				s.statsFetchHdr++
				s.statsFetchHdrB += int64(len(body))
			default:
				s.statsFetchBody++
				s.statsFetchBodyB += int64(len(body))
			}
			bw := mw.WriteBodySection(section, int64(len(body)))
			io.Copy(bw, bytes.NewReader(body)) //nolint:errcheck
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
			rc, ferr := box.Fetch(s.folder.Name, m.Filename)
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
			rc, ferr := box.Fetch(s.folder.Name, m.Filename)
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

	for i, m := range msgs {
		seqNum := uint32(i + 1)
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
		idx.UpdateFlags(s.folder.ID, m.UID, newFlags, newKW) //nolint:errcheck
		s.emitMailboxChange(s.folder.Name, locks.EventChanged, m.UID)

		// Re-read modseq after UpdateFlags so the FETCH response carries
		// the bumped value (CONDSTORE clients use it to update their
		// last-known state).
		newModSeq := m.ModSeq
		if updated, err := idx.GetMessages(s.folder.ID, mailbox.SeqSet{{From: m.UID, To: m.UID}}); err == nil && len(updated) > 0 {
			newModSeq = updated[0].ModSeq
		}

		if !storeFlags.Silent {
			mw := w.CreateMessage(seqNum)
			mw.WriteFlags(toImapFlags(append(newFlags, newKW...)))
			mw.WriteUID(imaplib.UID(m.UID))
			if newModSeq > 0 {
				mw.WriteModSeq(newModSeq)
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
	var srcUIDs, dstUIDs imaplib.UIDSet
	for i, m := range msgs {
		seqNum := uint32(i + 1)
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		rc, fetchErr := srcBox.Fetch(s.folder.Name, m.Filename)
		if fetchErr != nil {
			return nil, fmt.Errorf("imap/copy fetch: %w", fetchErr)
		}
		data, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			return nil, fmt.Errorf("imap/copy read: %w", readErr)
		}
		newUID, allocErr := destH.idx.AllocateUID(destFolder.ID)
		if allocErr != nil {
			return nil, fmt.Errorf("imap/copy allocate: %w", allocErr)
		}
		modseq, _ := destH.idx.NextModSeq(destFolder.ID)
		newFilename, saveErr := destH.box.Save(destRel, bytes.NewReader(data), newUID, int64(len(data)), m.Flags)
		if saveErr != nil {
			return nil, fmt.Errorf("imap/copy save: %w", saveErr)
		}
		if err := destH.idx.AppendMessage(destFolder.ID, &mailbox.MessageMeta{
			UID:          newUID,
			Filename:     newFilename,
			Flags:        m.Flags,
			Keywords:     m.Keywords,
			ModSeq:       modseq,
			Size:         uint32(len(data)),
			InternalDate: m.InternalDate,
		}); err != nil {
			_ = destH.box.Remove(destRel, newFilename)
			return nil, fmt.Errorf("imap/copy record: %w", err)
		}
		s.emitMailboxChange(dest, locks.EventDelivered, newUID)
		srcUIDs.AddNum(imaplib.UID(m.UID))
		dstUIDs.AddNum(imaplib.UID(newUID))
	}
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

	type matched struct {
		seqNum   uint32
		srcUID   uint32
		filename string
	}
	var hits []matched
	var srcUIDs, dstUIDs imaplib.UIDSet

	for i, m := range msgs {
		seqNum := uint32(i + 1)
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		rc, fetchErr := srcBox.Fetch(s.folder.Name, m.Filename)
		if fetchErr != nil {
			return fmt.Errorf("imap/move fetch: %w", fetchErr)
		}
		data, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			return fmt.Errorf("imap/move read: %w", readErr)
		}
		newUID, allocErr := destH.idx.AllocateUID(destFolder.ID)
		if allocErr != nil {
			return fmt.Errorf("imap/move allocate: %w", allocErr)
		}
		modseq, _ := destH.idx.NextModSeq(destFolder.ID)
		newFilename, saveErr := destH.box.Save(destRel, bytes.NewReader(data), newUID, int64(len(data)), m.Flags)
		if saveErr != nil {
			return fmt.Errorf("imap/move save: %w", saveErr)
		}
		if err := destH.idx.AppendMessage(destFolder.ID, &mailbox.MessageMeta{
			UID:          newUID,
			Filename:     newFilename,
			Flags:        m.Flags,
			Keywords:     m.Keywords,
			ModSeq:       modseq,
			Size:         uint32(len(data)),
			InternalDate: m.InternalDate,
		}); err != nil {
			_ = destH.box.Remove(destRel, newFilename)
			return fmt.Errorf("imap/move record: %w", err)
		}
		s.emitMailboxChange(dest, locks.EventDelivered, newUID)
		srcUIDs.AddNum(imaplib.UID(m.UID))
		dstUIDs.AddNum(imaplib.UID(newUID))
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
	}
	s.folder.Messages -= uint32(len(hits))
	srcIdx.SaveFolder(s.folder) //nolint:errcheck
	return nil
}

// ---- helpers ---------------------------------------------------------------

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

// matchesCriteriaSeq evaluates a SearchCriteria against a single message.
// seqNum supplies the message's 1-based sequence number; 0 means "unknown"
// and any criteria.SeqNum filter then short-circuits to not-matched.
func matchesCriteriaSeq(m *mailbox.MessageMeta, seqNum uint32, criteria *imaplib.SearchCriteria) bool {
	if criteria == nil {
		return true
	}
	for _, f := range criteria.Flag {
		if !hasFlag(m.Flags, string(f)) {
			return false
		}
	}
	for _, f := range criteria.NotFlag {
		if hasFlag(m.Flags, string(f)) {
			return false
		}
	}
	// Each entry in criteria.UID / criteria.SeqNum is an AND condition (the
	// message must fall within EVERY listed set). Per RFC 9051 §6.4.4.
	for _, set := range criteria.UID {
		if !set.Contains(imaplib.UID(m.UID)) {
			return false
		}
	}
	for _, set := range criteria.SeqNum {
		if seqNum == 0 || !set.Contains(seqNum) {
			return false
		}
	}
	// CONDSTORE SEARCH MODSEQ filter — RFC 7162 §3.1.5.
	// Match only messages whose modseq is >= the supplied value.
	// MetadataName/MetadataType narrow which attribute's modseq to compare;
	// per-attribute modseq tracking is future work, so we treat any
	// criteria.ModSeq as "message-level modseq" and ignore the attribute
	// qualifier — strictly more permissive (returns extra matches), which
	// is RFC-acceptable.
	if criteria.ModSeq != nil && m.ModSeq < criteria.ModSeq.ModSeq {
		return false
	}
	return true
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
