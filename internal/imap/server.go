// Package imap wires go-imap/v2 to yarilo's mailbox and index backends.
package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-sasl"
	proxyproto "github.com/pires/go-proxyproto"

	"github.com/yarilomail/yarilo/internal/auth/oauth2"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/internal/auth/scram"
	"github.com/yarilomail/yarilo/internal/connlimit"
	"github.com/yarilomail/yarilo/internal/loginproto"
	"github.com/yarilomail/yarilo/internal/msgcache"
	"github.com/yarilomail/yarilo/internal/quotawarn"
	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	"github.com/yarilomail/yarilo/internal/userstate/acl"
	"github.com/yarilomail/yarilo/internal/userstate/specialuse"
	"github.com/yarilomail/yarilo/internal/userstate/subs"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// Server is the yarilo IMAP server.
type Server struct {
	srv          *imapserver.Server
	opts         Options
	wardenClient *imapWardenClient
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
	// AuthAddr is the yarilo-auth login-protocol address used to verify
	// session tokens forwarded by login pods. When set, connections must
	// carry a valid YARILO preamble.
	AuthAddr string
	AuthTLS  *tls.Config
	// PreambleTLS terminates internal mTLS on the login->backend data path.
	PreambleTLS *tls.Config
	// MasterAddr is the yarilo-auth master-protocol address used for userdb
	// lookups after token verification.
	MasterAddr string
	MasterTLS  *tls.Config
	// MasterPool serves the session userdb lookup from a shared connection
	// instead of dialling one per session (#1419).
	MasterPool         *authclient.Pool
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
	// Zero disables.
	FailureDelay time.Duration

	// OAuth2Enabled advertises OAUTHBEARER/XOAUTH2. Set when at least one
	// OAuth provider is configured; otherwise the mechs are never
	// advertised against a deployment that cannot validate tokens.
	OAuth2Enabled bool

	// Locker is the cross-process write coordinator. When non-nil, each
	// successful write emits an EVENT on the mailbox key so IDLE sessions
	// on other pods wake up immediately. Nil keeps timer-based IDLE.
	Locker locks.Locker

	// SpecialUseDefaults maps folder names to \Sent/\Drafts/etc. for LIST
	// when the per-user special_use file does not override.
	SpecialUseDefaults map[string]string

	// MetadataDict backs RFC 5464 METADATA. When nil the caps are still
	// advertised (the lib needs them to parse the commands) but every op
	// returns "metadata storage disabled".
	MetadataDict dict.Dict

	// SieveEngine runs imapsieve scripts (RFC 6785) on APPEND, COPY/MOVE and
	// flag changes. Nil disables imapsieve.
	SieveEngine *sieve.Engine

	// QuotaEngine enables quota enforcement on saves; APPEND/COPY/MOVE are
	// rejected with OVERQUOTA over the limit. Independent of the IMAP QUOTA
	// extension below.
	QuotaEngine bool
	// QuotaName is the quota-root name in GETQUOTA responses (default "User quota").
	QuotaName string
	// QuotaExceededMessage is the text of the OVERQUOTA rejection.
	QuotaExceededMessage string
	// QuotaMailSize rejects a single message larger than this many bytes
	// (0 = unlimited), independent of the usage limit.
	QuotaMailSize int64
	// QuotaPolicy carries site-wide quota tunables. Grace is not applied on
	// the interactive IMAP path.
	QuotaPolicy quota.Policy
	// QuotaWarner runs quota_warning actions. Nil = warnings only log.
	QuotaWarner *quotawarn.Runner
	// FTS wires full-text search (https://doc.yarilomail.org/FTS §11). Zero value disables it.
	FTS FTSOptions
	// QuotaClone mirrors usage to external dicts. Nil = disabled.
	QuotaClone *quota.Clone
	// QuotaCloneFlushDelay debounces clone writes (one per interval per session).
	QuotaCloneFlushDelay time.Duration

	// IMAPQuota toggles the IMAP QUOTA extension (RFC 9208): capability plus
	// GETQUOTA/GETQUOTAROOT. Query only, no enforcement. When false the
	// capability is not advertised and GETQUOTA returns NO.
	IMAPQuota bool

	// ACLEnabled exposes RFC 4314 server-side ACL. Storage is the
	// per-mailbox yarilo-acl file in each folder's index directory. When
	// false the SessionACL methods return NO; the capability is still
	// advertised since go-imap detects it via interface assertion.
	ACLEnabled bool

	// ACLDefaultsFromInbox makes root-level default ACLs resolve from INBOX
	// for private/shared namespaces.
	ACLDefaultsFromInbox bool

	// ACLGlobal is the operator-configured global ACL merged into every
	// mailbox's effective rights (global takes precedence); nil when none.
	ACLGlobal *acl.Global

	// ACLGlobalsOnly evaluates only the global ACL, ignoring the per-mailbox
	// yarilo-acl files.
	ACLGlobalsOnly bool

	// SharedDict is the owner-discovery registry (#1168): who granted what to
	// whom in owner-templated namespaces, in the reference's shared-boxes key
	// space. Nil disables discovery -- LIST user/* then enumerates nobody.
	SharedDict dict.Dict
	// ACLCacheTTL is how long a parsed per-mailbox ACL is trusted before its
	// file's mtime+size are re-validated. Zero disables caching.
	ACLCacheTTL time.Duration

	// Threads answers FETCH THREADID from the account's threading sidecar.
	// Nil leaves it NIL, which is what an account behaves like before the
	// migration step reaches it (#1425).
	Threads *threads.Cache

	// UserdbLookup resolves an arbitrary (non-authenticating) user's storage
	// identity via the yarilo-auth master -- the same lookup LMTP uses for a
	// recipient. Owner-templated shared namespaces (B1) need it to open the
	// owner's store: the owner is not the session user, and the owner's driver
	// comes only from their userdb mail_location (https://doc.yarilomail.org/OWNER_SHARED_NS 3.3).
	// nil disables owner-templated resolution.
	UserdbLookup func(ctx context.Context, username string) (*mailbox.UserInfo, error)

	// Namespaces drives the IMAP NAMESPACE response (RFC 2342 / RFC 9051
	// §6.3.10). When nil/empty a single personal namespace with separator
	// "/" is assumed.
	Namespaces []NamespaceSpec

	// NamespaceMailboxes carries per-namespace MailboxBackend overrides
	// keyed by namespace prefix, letting operators mix storage drivers
	// across namespaces. Personal namespaces use the global backend unless
	// the per-user MailLoc carries a different driver.
	NamespaceMailboxes map[string]mailbox.MailboxBackend

	// MailboxByDriver returns a MailboxBackend for a driver name ("mdbox",
	// "maildir", …). Used when a user's mail_location driver differs from
	// the global default.
	MailboxByDriver func(driver string) mailbox.MailboxBackend

	// WardenAddr is the yarilo-warden server address. When set and the
	// preamble carried a warden session id, each SELECT/EXAMINE/UNSELECT
	// pushes the selected folder to warden for `who`.
	WardenAddr string
	// WardenTLS optionally wraps the warden dialer with mTLS.
	WardenTLS *tls.Config

	// MaildirSyncOnSelect reconciles the index against the physical mailbox
	// on SELECT/EXAMINE for drivers whose storage can change out of band
	// (maildir). Index-authoritative drivers (dbox) do not implement
	// ProactiveScan and ignore it.
	MaildirSyncOnSelect bool

	// DboxReactiveRebuild enables the sdbox/mdbox reactive auto-rebuild:
	// a read hitting a missing/corrupt message flags the folder, and the
	// next SELECT rebuilds its index from storage. Default true.
	DboxReactiveRebuild bool
}

// NamespaceSpec is the per-namespace data needed to render NAMESPACE
// responses and route mailbox operations. Kept separate from
// config.NamespaceConfig so callers do not depend on pkg/config.
//
// Location is the storage URL ("maildir:/path"). Empty means the namespace
// is wire-declared but not backed by storage; SELECT on it returns NO.
type NamespaceSpec struct {
	Type      NamespaceType
	Prefix    string
	Separator rune
	List      ListMode
	Location  string
	// IgnoreACL bypasses ACL enforcement for this namespace (rights not
	// checked, no lookup-right LIST hiding) even when ACL is enabled.
	IgnoreACL bool
	// Subscriptions is the operator's setting, nil when unset; the answer is
	// resolved by mailbox.NamespaceKeepsSubscriptions (see keepsSubscriptions),
	// so a spec built without it takes the default for its kind.
	Subscriptions *bool
}

// SessionID hands the cross-service correlation id to the imapserver layer:
// its diagnostics otherwise name the login proxy's address with port 0, which
// identifies no connection (#1164). Detected there by interface assertion.
func (s *session) SessionID() string { return s.sid }

// keepsSubscriptions reports whether this namespace holds its own subscription
// file, or delegates to the subscriber's own namespace.
func (spec NamespaceSpec) keepsSubscriptions() bool {
	return mailbox.NamespaceKeepsSubscriptions(string(spec.Type), spec.Prefix, spec.Subscriptions)
}

// ListMode is the namespace's LIST exposure. "yes" lists the namespace node
// and its children; "children" lists only the children -- the node itself is
// not a mailbox, which is what an owner-templated prefix needs: user/%u names
// nothing until an owner is filled in, so it must not appear as a row.
// "no" keeps the namespace addressable without advertising it.
type ListMode string

const (
	ListYes      ListMode = "yes"
	ListChildren ListMode = "children"
	ListNo       ListMode = "no"
)

// listed reports whether the namespace appears in NAMESPACE and contributes
// rows to LIST at all.
func (m ListMode) listed() bool { return m == ListYes || m == ListChildren }

// listsSelf reports whether the namespace's own node is a LIST row.
func (m ListMode) listsSelf() bool { return m == ListYes }

// NamespaceType is the NAMESPACE response slot: Personal / Other / Shared.
type NamespaceType string

const (
	NamespacePersonal NamespaceType = "personal"
	NamespaceOther    NamespaceType = "other"
	NamespaceShared   NamespaceType = "shared"
)

// New creates an IMAP server.
func New(opts Options) *Server {
	// A declared owner-templated namespace that cannot resolve an owner is the
	// #1132 config error one layer up: without UserdbLookup wired, every
	// SELECT user/... would fail at runtime with an internals message. Fail at
	// startup instead, with the reason named -- the pod crashes loudly rather
	// than serving a namespace it can never open (https://doc.yarilomail.org/OWNER_SHARED_NS 3.3).
	for i, ns := range opts.Namespaces {
		if mailbox.PrefixIsOwnerTemplated(ns.Prefix) && opts.UserdbLookup == nil {
			panic(fmt.Sprintf("imap: namespace %q is owner-templated but no userdb lookup is wired; "+
				"owner-templated namespaces need a configured auth master to resolve the owner", ns.Prefix))
		}
		// An unset list mode takes the kind default here, once, so every
		// consumer reads a resolved value (children for owner-templated,
		// yes otherwise -- one rule with config, pkg/mailbox holds it).
		if ns.List == "" {
			mode, _ := mailbox.NamespaceListMode(ns.Prefix, "")
			opts.Namespaces[i].List = ListMode(mode)
		}
	}

	// Memoise the per-driver backend once, so selecting it per handle (personal,
	// each fixed namespace, and every owner handle) shares one write semaphore
	// instead of building a fresh one -- max_concurrent_writes must bound the
	// process, not the connection (#1144, #1149).
	opts.MailboxByDriver = mailbox.MemoizeByDriver(opts.MailboxByDriver)

	s := &Server{
		opts:         opts,
		wardenClient: newImapWardenClient(opts.WardenAddr, opts.WardenTLS),
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
		// IMAP4rev2 (RFC 9051) requires these. ENABLE and SASL-IR are
		// wire-level and handled by go-imap/v2; ESEARCH, SEARCHRES and
		// STATUS=SIZE are implemented in Search/Status below.
		imaplib.CapESearch:          {},
		imaplib.CapSearchRes:        {},
		imaplib.CapEnable:           {},
		imaplib.CapSASLIR:           {},
		imaplib.CapStatusSize:       {},
		imaplib.CapListExtended:     {},
		imaplib.CapListStatus:       {},
		imaplib.CapSpecialUse:       {},
		imaplib.CapCreateSpecialUse: {},
		imaplib.CapObjectID:         {}, // RFC 8474 — MAILBOXID / EMAILID / THREADID
		imaplib.CapNotify:           {}, // RFC 5465 — event notifications
		imaplib.CapBinary:           {},
		imaplib.CapQResync:          {},
	}
	if opts.MetadataDict != nil {
		caps[imaplib.CapMetadata] = struct{}{}
	}
	if opts.ACLEnabled {
		caps[imaplib.CapACL] = struct{}{}
	}
	if opts.IMAPQuota {
		caps[imaplib.CapQuota] = struct{}{}
	}
	// Announced only when there is something to answer with: ID is served by
	// the parser now, and an empty imap_id_send answers NIL.
	if len(parseIDSend(opts.IDSend)) > 0 {
		caps[imaplib.CapID] = struct{}{}
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
	// PreambleListener terminates internal mTLS, so it must wrap the raw
	// stream; plaintext wrappers (maxLineLen, greeting, ID) sit above it on
	// the decrypted IMAP stream. maxLineLen line-scans and would mangle a
	// TLS ClientHello. proxyproto stays innermost: the PROXY header is
	// pre-TLS.
	if s.opts.AuthAddr != "" {
		ln = &loginproto.PreambleListener{
			Listener:        ln,
			AuthAddr:        s.opts.AuthAddr,
			AuthTLS:         s.opts.AuthTLS,
			MasterAddr:      s.opts.MasterAddr,
			MasterTLS:       s.opts.MasterTLS,
			MasterPool:      s.opts.MasterPool,
			ExpectedService: "imap",
			TLSConfig:       s.opts.PreambleTLS,
		}
	}
	if s.opts.MaxLineLength > 0 {
		ln = &maxLineLenListener{Listener: ln, limit: s.opts.MaxLineLength}
	}
	if s.opts.LoginGreeting != "" {
		ln = &greetingListener{Listener: ln, greeting: s.opts.LoginGreeting}
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
			Result:        protocol.AuthOK,
			Username:      pc.Username,
			Home:          pc.Home,
			MailLoc:       pc.MailLoc,
			Groups:        pc.Groups,
			QuotaRules:    pc.QuotaRules,
			QuotaOverFlag: pc.QuotaOverFlag,
			VolatileDir:   pc.VolatileDir,
			MailboxFormat: pc.MailboxFormat,
			IndexDir:      pc.IndexDir,
			ControlDir:    pc.ControlDir,
			AltDir:        pc.AltDir,
			MailPath:      pc.MailPath,
			InboxPath:     pc.InboxPath,
		}); err != nil {
			return nil, nil, err
		}
		return &timedSession{s: sess}, &imapserver.GreetingData{PreAuth: true}, nil
	}
	return &timedSession{s: sess}, &imapserver.GreetingData{PreAuth: false}, nil
}

// unwrapPreambleConn walks the net.Conn wrapper chain looking for a
// *loginproto.PreambleConn via Unwrap().
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
	// box / idx / subs alias the personal namespace handle
	// (s.primary.box / .idx / .subs). Cross-namespace ops route through
	// s.dispatch() and use the resulting handle instead.
	box  mailbox.UserMailbox
	idx  mailbox.UserIndex
	subs *subs.Store

	limitIP string
	folder  *mailbox.Folder

	// maildirSyncTokens overrides the process-wide maildir token cache. Nil in
	// production — the cache outlives every session by design (#1248) — and set
	// only by tests that need an isolated one.
	maildirSyncTokens *syncTokenCache

	// markedCorrupt records folders this session already flagged FSCKD so a
	// FETCH over many corrupt messages marks once, not per message. Keyed
	// by folder ID (stable across mark and clear sites, unlike the name).
	markedCorrupt map[uint64]bool

	// healAttempts counts consecutive reactive-heal failures per folder.
	// After maxHealAttempts we stop auto-retrying (each attempt is a full
	// storage scan) until the marker clears. Reset on a successful heal.
	healAttempts map[uint64]int

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

	// NOTIFY (RFC 5465) state. notifyActive is set by NOTIFY SET and cleared
	// by NOTIFY NONE; while set, unsolicited responses for the selected
	// mailbox are governed by selNew/selExpunge/selFlagChange.
	// selImmediateExpunge distinguishes SELECTED from SELECTED-DELAYED.
	notifyActive        bool
	selNew              bool
	selExpunge          bool
	selFlagChange       bool
	selImmediateExpunge bool
	// notifyWatch monitors non-selected mailboxes named by RFC 5465 §6
	// filters. Nil unless NOTIFY SET requested one. Its activity surfaces
	// as "* STATUS" responses drained by Poll and Idle.
	notifyWatch *notifyWatcher

	// quotaCache is a short-lived cache of the index-derived user quota usage,
	// so a burst of GETQUOTA / APPEND checks does not re-enumerate every folder.
	quotaCacheUsage quota.Usage
	quotaCacheAt    time.Time
	// quotaSnap is the usage captured before the last quota-changing operation,
	// used as the "before" side of quota_warning crossing detection.
	quotaSnap    quota.Usage
	quotaSnapSet bool
	// quota_clone debounce state: mirror at most once per flush delay, deferring
	// the latest usage to a final flush on session close.
	cloneDirty     bool
	cloneDirtyUsg  quota.Usage
	cloneLastFlush time.Time
	// quota_over_status state: run the external over-flag sync once per session.
	overStatusChecked bool
	overStatusLoginAt time.Time

	// namespaces holds the per-namespace storage handles keyed by prefix;
	// the personal namespace has key "". Declared-only namespaces are
	// absent — dispatch() catches them via the wire-spec list.
	namespaces map[string]*nsHandle
	// primary is the personal namespace handle; equals namespaces[""].
	primary *nsHandle
	// ownerHandles caches owner-templated namespace handles built on demand,
	// keyed by prefix + owner. Bounded and closed at teardown (dispatch.go).
	ownerHandles map[string]*nsHandle
	// folderNS is the namespace handle of the selected folder, captured at
	// SELECT so folder-bound ops route without re-parsing the name. Nil
	// means s.primary.
	folderNS *nsHandle

	// savedSearchUIDs is the last SEARCH RETURN SAVE result (RFC 5182);
	// substituted for $ in later commands.
	savedSearchUIDs imaplib.UIDSet

	// specialUse persists per-user RFC 6154 overrides set via CREATE
	// (USE ...) and resolves folder→attr for LIST. Personal namespace only.
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

// emitMailboxChange is fire-and-forget: events are advisory wake-ups for
// IDLE sessions on other pods, the authoritative state is already on disk.
// A 1-second timeout keeps a slow locks server from stalling the command.
// f identifies the folder for the FTS hook, which needs its GUID; the event
// bus needs only the name, so a folder without a resolvable GUID still wakes
// IDLE sessions (#1183).
func (s *session) emitMailboxChange(f *mailbox.Folder, eventType locks.EventType, uid uint32) {
	folder := f.Name
	// delivered/expunged changes storage usage; runs before the Locker
	// guard since it is independent of the event bus.
	if eventType == locks.EventDelivered || eventType == locks.EventExpunged {
		s.ftsNotify(f, eventType == locks.EventExpunged, uid)
		s.quotaChanged()
		// one post-commit read feeds both quota_warning crossing detection
		// and the quota_clone mirror.
		wantWarn := len(s.srv.opts.QuotaPolicy.Warnings) > 0 && s.quotaSnapSet
		wantClone := s.srv.opts.QuotaClone != nil
		if wantWarn || wantClone {
			if after, err := s.countUsage(false); err == nil {
				if wantWarn {
					s.fireQuotaWarnings(after)
				}
				if wantClone {
					s.cloneMirror(after)
				}
			}
		}
	}
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

// emitMailboxList publishes a mailbox-list event (create / delete / rename /
// subscribe) on the user's MailboxListKey. Fire-and-forget, same rationale
// as emitMailboxChange.
func (s *session) emitMailboxList(eventType locks.EventType, payload string) {
	if s.srv.opts.Locker == nil || s.userInfo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.srv.opts.Locker.Emit(ctx, locks.MailboxListKey(s.userInfo.Username), eventType, payload); err != nil {
		slog.Debug("imap: emit list event failed",
			"type", string(eventType), "payload", payload, "err", err)
	}
}

func (s *session) Close() error {
	s.cloneFlushFinal()
	s.stopNotifyWatch()
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
	// tears down every per-namespace box+idx, including the personal handle.
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
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Login")
	res, err := s.srv.opts.Auth.Authenticate(username, password, "imap", remoteIP(s.imapConn.NetConn()))
	if err != nil || res == nil || res.Result != protocol.AuthOK {
		s.delayFailure()
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Invalid credentials"}
	}
	return s.completeLogin(res)
}

// delayFailure sleeps for opts.FailureDelay before an auth failure
// surfaces, so wire timing carries no information about whether the
// user exists.
func (s *session) delayFailure() {
	if d := s.srv.opts.FailureDelay; d > 0 {
		time.Sleep(d)
	}
}

// AuthenticateMechanisms advertises the SASL mechanisms this session
// implements. PLAIN is unconditional; OAUTHBEARER/XOAUTH2 require a
// configured OAuth provider; SCRAM requires a passdb with SCRAM verifiers.
// The PLUS variants additionally require TLS 1.3+ for the RFC 9266
// exporter.
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
// OAUTHBEARER routes the bearer token through the regular Authenticator
// (the OAuth passdb extracts it from the password field). SCRAM variants
// route through the lookup interfaces; no plain password is compared.
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

// completeSCRAMLogin is the OnSuccess hook for SCRAM adapters; the SASL
// server has already verified the user, this runs the regular post-auth
// setup.
func (s *session) completeSCRAMLogin(username string) error {
	return s.completeLogin(&protocol.AuthResponse{
		Result:   protocol.AuthOK,
		Username: username,
	})
}

// tlsExporter returns the 32-byte RFC 9266 exporter output used as
// SCRAM-PLUS channel binding, or nil when the conn is not TLS 1.3+.
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

// authenticateOAuthBearer translates (Username, Token) into the chain's
// Authenticate call. Wire-shape concerns (GS2 parsing, RFC 7628 JSON
// error blob) live inside go-sasl.
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

// authenticateXOAuth2 is the XOAUTH2 callback. Same token validation
// path as OAUTHBEARER; only the wire format differs.
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

// authenticatePlainSASL handles AUTHENTICATE PLAIN. authzid carries the
// impersonation target (master-user model); empty or equal-to-authid
// falls back to the regular login path. Without a MasterAuthenticator a
// distinct authzid is rejected with no detail given to the client.
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

// completeLogin runs the post-auth setup shared by LOGIN and AUTHENTICATE.
// res carries the resolved username and userdb fields needed to open the
// per-namespace storage handles.
func (s *session) completeLogin(res *protocol.AuthResponse) error {
	resolver := s.srv.opts.Resolver
	if resolver == nil {
		resolver = &mailbox.Resolver{}
	}
	userInfo := resolver.UserInfo(res.Username, res.Home)
	userInfo.Groups = res.Groups
	userInfo.ACLUser = res.ACLUser
	userInfo.ACLGroups = res.ACLGroups
	userInfo.QuotaRules = res.QuotaRules
	userInfo.QuotaOverFlag = res.QuotaOverFlag
	userInfo.SessionID = s.sid
	locErr, drvErr := mailbox.ApplyUserdb(userInfo, mailbox.UserdbOverrides{
		VolatileDir:  res.VolatileDir,
		IndexDir:     res.IndexDir,
		ControlDir:   res.ControlDir,
		AltDir:       res.AltDir,
		MailPath:     res.MailPath,
		InboxPath:    res.InboxPath,
		MailLocation: res.MailLoc,
		Driver:       res.MailboxFormat,
	})
	if locErr != nil {
		slog.Warn("imap: mail_location parse failed; using global mailbox backend",
			"user", userInfo.Username, "mail_location", res.MailLoc, "err", locErr)
	}
	if drvErr != nil {
		slog.Warn("imap: userdb named a storage driver we do not have; using the one from mail_location",
			"user", userInfo.Username, "mail_driver", res.MailboxFormat, "err", drvErr)
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

	// quota_over_status: reconcile the external over-flag against actual
	// usage at login (unless lazy).
	s.overStatusLoginAt = time.Now()
	if os := s.srv.opts.QuotaPolicy.OverStatus; os.Mask != "" && !os.LazyCheck {
		if u, uerr := s.countUsage(false); uerr == nil {
			s.evalOverStatus(u)
		}
	}

	owner := fmt.Sprintf("yarilo-imap/%d/%s", os.Getpid(), userInfo.Username)
	s.specialUse = specialuse.New(
		userInfo.Home, userInfo.Username, owner, s.srv.opts.Locker,
		s.srv.opts.SpecialUseDefaults,
	)

	// Audit log. master_user is non-empty only on the impersonation path;
	// emitted for all logins so SIEM sees one log shape.
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
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Select")
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
	if refreshed := s.maildirSyncOnSelect(h, rel, f); refreshed != nil {
		f = refreshed
	}
	if refreshed := s.dboxHealIfCorrupt(h, rel, f); refreshed != nil {
		f = refreshed
	}
	// Mail stored before per-message GUIDs carries none, so stamp it once here.
	// Not fatal: the folder stays pending and every other operation works.
	if err := idxrebuild.BackfillGUIDs(h.box, h.idx, f, rel); err != nil {
		slog.Warn("imap: guid backfill failed", "folder", rel, "err", err)
	}
	s.folder = f
	s.folderNS = h
	// seed a usage baseline so a quota_warning "under" crossing fires even
	// when the session only deletes mail.
	s.seedQuotaWarnSnap()
	tWarden := time.Now()
	s.pushWardenSelect(name)
	slog.Debug("imap: select timing warden_ms", "folder", rel, "warden_ms", time.Since(tWarden).Milliseconds())

	// auto-subscribe on first SELECT so LSUB returns the folder without an
	// explicit SUBSCRIBE.
	if store, keyPrefix, terr := s.subsView(h); terr == nil {
		tSubs := time.Now()
		key := keyPrefix + rel
		if subs, snapErr := store.Snapshot(); snapErr == nil {
			if _, already := subs[key]; !already {
				tAdd := time.Now()
				_ = store.Add(key)
				slog.Debug("imap: select timing subs_add_ms", "folder", rel, "add_ms", time.Since(tAdd).Milliseconds())
			}
		}
		slog.Debug("imap: select timing subs_ms", "folder", rel, "subs_ms", time.Since(tSubs).Milliseconds())
	}

	tGetMsgs := time.Now()
	msgs, err := readMessages(h.idx, f.ID)
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
	if kws, err := readKeywords(h.idx, f.ID); err == nil {
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
		MailboxID:      mailbox.FormatObjectID(f.GUID), // RFC 8474 OBJECTID
	}
	// QRESYNC SELECT (RFC 7162 §3.2): on matching UIDVALIDITY, reply with
	// VANISHED (EARLIER) listing UIDs expunged since the client's modseq.
	// KnownUIDs narrows the response; empty means "tell me everything".
	if opts != nil && opts.QResync != nil && opts.QResync.UIDValidity == f.UIDValidity {
		vanishedUIDs, vErr := readVanished(h.idx, f.ID, opts.QResync.ModSeq)
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
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Unselect")
	s.folder = nil
	s.folderNS = nil
	s.knownMsgs = nil
	s.syncModSeq = 0
	s.hasPendingExpunge = false
	s.knownKeywords = nil
	s.pushWardenSelect("")
	return nil
}

func (s *session) Create(name string, opts *imaplib.CreateOptions) error {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Create")
	h, rel, err := s.dispatch(name)
	if err != nil {
		return err
	}
	if err := s.requireRightOnParent(h, rel, mailbox.RightCreate); err != nil {
		return err
	}
	// quota_mailbox_count: cap the number of mailboxes a user may have.
	if lim := s.srv.opts.QuotaPolicy.MailboxCount; lim > 0 {
		if entries, lerr := h.box.ListFolders(); lerr == nil && int64(len(entries)) >= lim {
			return &imaplib.Error{
				Type: imaplib.StatusResponseTypeNo,
				Code: imaplib.ResponseCode("LIMIT"),
				Text: "Maximum number of mailboxes reached",
			}
		}
	}
	if err := h.box.Create(rel); err != nil {
		return nameError(err)
	}
	// Inheritance is materialised here rather than resolved on every check:
	// the new mailbox gets its own ACL file carrying what it inherited, so the
	// user who just created it -- often holding the create right only at the
	// namespace root -- is named in it before they can issue a SETACL that
	// would otherwise replace the grant they are acting under (#1111).
	if h.acl != nil && s.aclEnforced(h) {
		if err := h.acl.MaterialiseOnCreate(rel); err != nil {
			slog.Warn("imap: acl inheritance not materialised", "folder", name, "err", err)
		}
		if err := s.grantCreatorAdmin(h, rel); err != nil {
			if rbErr := s.rollBackUnadministered(h, rel, name, err); rbErr != nil {
				return rbErr
			}
		}
	}
	// CREATE-SPECIAL-USE (RFC 6154 §3): record the requested use attr for
	// later LIST replies. The RFC allows one attr per folder; honour the
	// first, ignore the rest. Personal namespace only.
	if opts != nil && len(opts.SpecialUse) > 0 && s.specialUse != nil && h == s.primary {
		if err := s.specialUse.Set(rel, opts.SpecialUse[0]); err != nil {
			slog.Warn("imap: special_use persist failed",
				"folder", name, "attr", string(opts.SpecialUse[0]), "err", err)
		}
	}
	s.emitMailboxList(locks.EventMailboxCreate, name)
	return nil
}

// nameError converts a folder-name refusal from the storage layer into a
// protocol answer. Anything the library does not recognise as an *imap.Error
// becomes NO [SERVERBUG] "Internal server error", which tells the client the
// server is broken and sends the operator hunting a crash that never happened
// -- the name was simply invalid (#1072).
func nameError(err error) error {
	if err == nil || !errors.Is(err, mailbox.ErrInvalidFolderName) {
		return err
	}
	return &imaplib.Error{
		Type: imaplib.StatusResponseTypeNo,
		Code: imaplib.ResponseCodeCannot,
		Text: "Invalid mailbox name",
	}
}

func (s *session) Delete(name string) error {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Delete")
	h, rel, err := s.dispatch(name)
	if err != nil {
		return err
	}
	// RFC 9051 6.3.5: DELETE of INBOX is refused. The name is legitimate
	// everywhere else, so the refusal belongs to the destructive verb rather
	// than to name validation -- on maildir INBOX *is* the mail root, and
	// removing it takes every message and the root index (#1063).
	if strings.EqualFold(rel, "INBOX") {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCodeCannot,
			Text: "INBOX cannot be deleted",
		}
	}
	// A mailbox that is not there is refused, not reported as deleted
	// (RFC 9051 §6.3.5, code from RFC 5530). The client asked for it to be
	// gone and it is gone, so the difference looks academic -- but a cleanup
	// that deletes the wrong name reporting success is how a mailbox being
	// destroyed by an unidentified caller stayed unidentified (#1063, #1066).
	//
	// Existence before rights, as SELECT does at line 986: the same answer to
	// the same question, rather than two orders for a client to reason about.
	exists, err := h.box.FolderExists(rel)
	if err != nil {
		return err
	}
	if !exists {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}
	if err := s.requireRight(h, rel, mailbox.RightDeleteMailbox); err != nil {
		return err
	}
	if err := h.box.Delete(rel); err != nil {
		return nameError(err)
	}
	// drop the folder's index state. Non-fatal: the mailbox is already
	// gone; any orphan index dir is reclaimed on next rebuild.
	if err := h.idx.DeleteFolder(rel); err != nil {
		slog.Warn("imap: index delete after DELETE failed", "folder", name, "err", err)
	}
	// drop explicit ACL state (file + namespace-wide index). Non-fatal:
	// the mailbox is already gone.
	if h.acl != nil {
		if err := h.acl.Remove(rel); err != nil {
			slog.Warn("imap: acl remove after DELETE failed", "folder", name, "err", err)
		}
	}
	s.emitMailboxList(locks.EventMailboxDelete, name)
	return nil
}

func (s *session) Rename(oldName, newName string, _ *imaplib.RenameOptions) error {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Rename")
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
	// Same as DELETE: renaming a mailbox that is not there is refused rather
	// than answered OK (RFC 9051 §6.3.6). The two share the lookup and the
	// failure they hide.
	exists, err := hOld.box.FolderExists(relOld)
	if err != nil {
		return err
	}
	if !exists {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}
	// RENAME requires DELETE on the source plus CREATE on the destination's
	// parent, so move rights can be granted without blanket delete.
	if err := s.requireRight(hOld, relOld, mailbox.RightDeleteMailbox); err != nil {
		return err
	}
	if err := s.requireRightOnParent(hNew, relNew, mailbox.RightCreate); err != nil {
		return err
	}
	if err := hOld.box.Rename(relOld, relNew); err != nil {
		return nameError(err)
	}
	if err := hOld.idx.RenameFolder(relOld, relNew); err != nil {
		return err
	}
	// Move the per-mailbox yarilo-acl file and rewrite namespace-wide index
	// entries. Non-fatal: the mailbox has moved; a stale index must not fail
	// the IMAP response.
	if hOld.acl != nil {
		if err := hOld.acl.Rename(relOld, relNew); err != nil {
			slog.Warn("imap: acl rename failed", "from", oldName, "to", newName, "err", err)
		}
	}
	s.emitMailboxList(locks.EventMailboxRename, oldName+renameSep+newName)
	return nil
}

// renameInbox implements RFC 3501 §6.3.5 INBOX rename semantics:
// messages are moved to the new mailbox; INBOX itself is cleared but not deleted.
func (s *session) renameInbox(dest string) error {
	if err := s.box.Create(dest); err != nil {
		// The destination is a name the client chose, so a refusal of it is a
		// client error. Wrapping it plainly made the library report an
		// internal server error, while the source argument one branch up
		// answered correctly -- two answers to the same kind of mistake
		// (#1075).
		return nameError(fmt.Errorf("imap/rename-inbox create: %w", err))
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
		// Relocation, not a new message: the GUID carries over (RFC 8474).
		newFilename, guid, moveErr := s.box.Move("INBOX", dest, m.Filename, m.GUID)
		if moveErr != nil {
			return fmt.Errorf("imap/rename-inbox move: %w", moveErr)
		}
		nm := &mailbox.MessageMeta{
			Filename:     newFilename,
			Flags:        m.Flags,
			Keywords:     m.Keywords,
			Size:         m.Size,
			VSize:        m.VSize,
			InternalDate: m.InternalDate,
			GUID:         guid,
		}
		if err := s.idx.AllocateAndAppend(destFolder.ID, nm); err != nil {
			_ = s.box.Remove(dest, newFilename)
			return fmt.Errorf("imap/rename-inbox record: %w", err)
		}
		s.emitMailboxChange(destFolder, locks.EventDelivered, nm.UID)
		s.idx.ExpungeMessage(srcFolder.ID, m.UID) //nolint:errcheck
		s.emitMailboxChange(srcFolder, locks.EventExpunged, m.UID)
	}
	srcFolder.Messages = 0
	s.idx.SaveFolder(srcFolder) //nolint:errcheck
	return nil
}

func (s *session) Subscribe(name string) error {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Subscribe")
	h, rel, err := s.dispatch(name)
	if err != nil {
		return err
	}
	// Existence is deliberately not checked: RFC 9051 6.3.7 allows subscribing
	// to a mailbox that does not exist yet, and clients rely on it. The name
	// itself still has to be one this server would accept somewhere -- storing
	// ".." produces a subscription no command can ever act on, and LSUB would
	// hand it back to the client as though it meant something (#1075).
	if err := mailbox.CheckName(h.box, rel); err != nil {
		return nameError(err)
	}
	// A subscription is the subscriber's state, so it is kept by the namespace
	// that keeps subscriptions -- normally the caller's own -- under the
	// client-visible name, not in the store of whoever owns the mailbox.
	store, key, err := s.subsTarget(h.visiblePrefix() + rel)
	if err != nil {
		return err
	}
	if err := store.Add(key); err != nil {
		return fmt.Errorf("imap: subscribe %q: %w", name, err)
	}
	s.emitMailboxList(locks.EventMailboxSubscribe, name)
	return nil
}

func (s *session) Unsubscribe(name string) error {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Unsubscribe")
	// Deliberately not through dispatch. Removing a row from the list the caller
	// owns is not learning about anyone else's space, so the owner-templated
	// visibility gate (#1138) must not reach it: gated, a peer's own row would
	// be one nothing could remove. Resolving the storing namespace needs no
	// owner lookup either -- an owner-templated namespace never keeps
	// subscriptions.
	store, key, err := s.subsTarget(s.visibleName(name))
	if err != nil {
		return err
	}
	if err := store.Remove(key); err != nil {
		return fmt.Errorf("imap: unsubscribe %q: %w", name, err)
	}
	s.emitMailboxList(locks.EventMailboxUnsubscribe, name)
	return nil
}

func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, opts *imaplib.ListOptions) error {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "List")
	if s.srv.opts.ClientWorkarounds&workaroundTBExtraMailboxSep != 0 {
		ref = strings.TrimPrefix(ref, "/")
		for i, p := range patterns {
			patterns[i] = strings.TrimPrefix(p, "/")
		}
	}

	// iterate every implemented namespace (personal first, then by prefix);
	// folders are emitted with the namespace prefix re-attached.
	for _, h := range s.orderedHandles() {
		if err := s.listNamespace(w, h, ref, patterns, opts); err != nil {
			return err
		}
	}

	// A pattern naming an owner-templated space with the owner written out
	// (user/alice/*) materialises that owner's namespace and lists it like
	// any other; a wildcard in the owner segment enumerates nobody -- there
	// is no registry of owners to enumerate (#1139). An owner that does not
	// resolve and an owner whose space is ACL-hidden both yield the same
	// silence, so the #1138 oracle stays closed.
	materialised, err := s.listMaterialisedOwners(w, ref, patterns, opts)
	if err != nil {
		return err
	}

	// Owner-templated subscriptions: their namespace opens no handle until
	// referenced, so the loop above never sees them. The rows are the
	// caller's own list and are always shown; the resolver's visibility
	// answer (#1158) decides only what may be said about the mailbox,
	// never whether the row appears -- hiding here would recreate the
	// invisible-and-unremovable trap UNSUBSCRIBE just escaped.
	if opts != nil && opts.SelectSubscribed {
		if err := s.listTemplatedSubscriptions(w, materialised, ref, patterns, opts); err != nil {
			return err
		}
	}

	// emit namespace-root entries for shared/public (\Noselect
	// \HasChildren) so a top-level LIST shows the namespace even before
	// any sub-folder exists.
	for _, spec := range s.namespaceSpecsForList() {
		if spec.Type == NamespacePersonal || !spec.List.listed() {
			continue
		}
		// The node under the ADVERTISED prefix: for an owner-templated
		// namespace that is the literal head (user), never the template --
		// present even in children mode, as the reference presents its
		// truncated node, so a client walking down from the NAMESPACE answer
		// finds a container, not a hole. \HasChildren is truthful: the
		// caller's own space (user/<self>) always exists. A fixed namespace
		// in children mode keeps its node suppressed -- that is what the
		// mode is for.
		if !isOwnerTemplated(spec) && !spec.List.listsSelf() {
			continue
		}
		// declared-only namespaces get a \Noselect entry; SELECT under
		// them returns NO.
		rootName := strings.TrimSuffix(mailbox.AdvertisedPrefix(spec.Prefix), string(spec.Separator))
		if rootName == "" {
			continue
		}
		if !listMatch(rootName, refPatterns(ref, patterns), byte(spec.Separator)) {
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

// aclVisibleEntries filters folders by the RFC 4314 lookup right for a
// non-owned namespace. A no-lookup folder that is an ancestor of a visible
// one is kept as a \NoSelect container; every other no-lookup folder is
// dropped. On an ACL read error the folder fails closed (hidden).
func (s *session) aclVisibleEntries(h *nsHandle, entries []mailbox.FolderEntry, sep string) []mailbox.FolderEntry {
	sepByte := byte(h.spec.Separator)
	aclUser, aclGroups := s.userInfo.ACLIdentity()
	lookup := make(map[string]bool, len(entries))
	for _, e := range entries {
		r, err := h.acl.EffectiveFor(e.Name, aclUser, aclGroups, false, sepByte)
		if err != nil {
			slog.Warn("imap: acl list lookup failed", "ns", h.name, "folder", e.Name, "err", err)
			lookup[e.Name] = false
			continue
		}
		lookup[e.Name] = r.Has(mailbox.RightLookup)
	}
	// mark ancestors of visible folders so they survive as \NoSelect.
	visibleAncestor := make(map[string]bool)
	for name, ok := range lookup {
		if !ok {
			continue
		}
		for sep != "" {
			i := strings.LastIndex(name, sep)
			if i < 0 {
				break
			}
			name = name[:i]
			visibleAncestor[name] = true
		}
	}
	out := make([]mailbox.FolderEntry, 0, len(entries))
	for _, e := range entries {
		switch {
		case lookup[e.Name]:
			out = append(out, e)
		case visibleAncestor[e.Name]:
			out = append(out, mailbox.FolderEntry{Name: e.Name, Selectable: false})
		}
	}
	return out
}

// listNamespace emits LIST replies for one namespace's folders.
// Folder names are wire-encoded with the namespace prefix re-attached.
func (s *session) listNamespace(w *imapserver.ListWriter, h *nsHandle, ref string, patterns []string, opts *imaplib.ListOptions) error {
	tList := time.Now()
	entries, err := h.box.ListFolders()
	slog.Debug("imap: list timing listfolders_ms", "listfolders_ms", time.Since(tList).Milliseconds())
	if err != nil {
		return err
	}
	sep := string(h.spec.Separator)

	// ACL LIST hiding (RFC 4314 lookup right); the owner has full rights so
	// filtering is skipped for the personal namespace.
	if s.aclEnforced(h) && h.acl != nil && !s.isOwner(h) {
		entries = s.aclVisibleEntries(h, entries, sep)
	}

	// flat name slice for the hierarchy helpers.
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}

	// snapshot special-use once per LIST, alongside subscriptions below. Nil
	// for a non-personal namespace, where the attributes do not apply; a nil
	// map reads as empty, so the loop needs no second condition.
	var specialUse map[string]imaplib.MailboxAttr
	if h == s.primary && s.specialUse != nil {
		specialUse = s.specialUse.Attrs()
	}

	// snapshot subscriptions once per LIST so every folder consults the
	// same view.
	// The subscribed set comes from whichever namespace keeps this one's
	// subscriptions, and its keys carry that namespace's prefix -- so a
	// relative name is looked up as subsKeyPrefix+name, not bare.
	var subs map[string]struct{}
	var subsKeyPrefix string
	if store, keyPrefix, terr := s.subsView(h); terr == nil && opts != nil && (opts.SelectSubscribed || opts.ReturnSubscribed) {
		subsKeyPrefix = keyPrefix
		tSubs := time.Now()
		subs, err = store.Snapshot()
		slog.Debug("imap: list timing subs_ms", "subs_ms", time.Since(tSubs).Milliseconds())
		if err != nil {
			slog.Warn("imap: subscription snapshot failed", "ns", h.name, "err", err)
			subs = make(map[string]struct{})
		}
	}

	for _, entry := range entries {
		name := entry.Name
		// wire-protocol name = namespace prefix + relative name. The
		// reference is not part of it: it combines with the pattern (RFC 9051
		// 6.3.9), and gluing it onto results made LIST answer with names that
		// exist nowhere -- a personal folder wearing another namespace's
		// prefix (#1099).
		full := h.fullName(name)
		if !listMatch(full, refPatterns(ref, patterns), byte(h.spec.Separator)) {
			continue
		}
		// SELECT SUBSCRIBED drops folders the user has not subscribed to.
		// RECURSIVEMATCH refinement is not implemented.
		if opts != nil && opts.SelectSubscribed {
			if _, ok := subs[subsKeyPrefix+name]; !ok {
				continue
			}
		}
		attrs := mailboxAttrs(name, names, sep, s.srv.opts.ClientWorkarounds)
		if !entry.Selectable {
			attrs = append(attrs, imaplib.MailboxAttrNoSelect)
		}
		if opts != nil && opts.ReturnChildren {
			attrs = append(attrs, childrenAttr(name, names, sep))
		}
		if opts != nil && opts.ReturnSubscribed {
			if _, ok := subs[subsKeyPrefix+name]; ok {
				attrs = append(attrs, imaplib.MailboxAttrSubscribed)
			}
		}
		// SPECIAL-USE (RFC 6154); personal namespace only. Read once per LIST,
		// for the same reason subscriptions are: the document describes the
		// whole user, so asking per name asked the same question once per
		// folder — and each ask was two round trips to the lock service.
		if attr := specialUse[name]; attr != "" {
			attrs = append(attrs, attr)
		}
		data := &imaplib.ListData{Mailbox: full, Delim: h.spec.Separator, Attrs: attrs}
		// RETURN STATUS (RFC 5819): skip on failure rather than abort the
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

	// Subscriptions whose mailbox is gone are still rows in the caller's own
	// list; dropping them here would leave state that can be seen nowhere and
	// removed only blind.
	if opts != nil && opts.SelectSubscribed && subs != nil {
		existing := make(map[string]bool, len(entries))
		for _, entry := range entries {
			existing[entry.Name] = true
		}
		if err := s.listNamespaceOrphans(w, h, existing, subs, subsKeyPrefix, ref, patterns, opts); err != nil {
			return err
		}
	}
	return nil
}

// orphanAttrs renders a subscribed name whose mailbox does not exist, per
// command form (RFC 5258 §3.1): extended LIST carries \NonExistent, a
// non-extended LIST can only say \Noselect, LSUB says nothing at all.
func orphanAttrs(opts *imaplib.ListOptions) []imaplib.MailboxAttr {
	switch {
	case opts.Lsub:
		return nil
	case opts.SelectSubscribed:
		attrs := []imaplib.MailboxAttr{imaplib.MailboxAttrNonExistent}
		if opts.ReturnSubscribed {
			attrs = append(attrs, imaplib.MailboxAttrSubscribed)
		}
		return attrs
	default:
		return []imaplib.MailboxAttr{imaplib.MailboxAttrNoSelect}
	}
}

// listNamespaceOrphans emits rows for subscriptions in h's view whose mailbox
// is not in the listing. The row is the caller's own state, so it is never
// hidden; existence is judged against the same (ACL-filtered) listing the
// regular rows came from, so an ACL-hidden mailbox reads as nonexistent --
// the answer #1158 already gives, not a new distinguisher.
func (s *session) listNamespaceOrphans(w *imapserver.ListWriter, h *nsHandle, existing map[string]bool, subs map[string]struct{}, subsKeyPrefix, ref string, patterns []string, opts *imaplib.ListOptions) error {
	orphans := make([]string, 0)
	for key := range subs {
		rel, ok := strings.CutPrefix(key, subsKeyPrefix)
		if !ok || rel == "" {
			continue
		}
		// The personal view holds every delegated row too; another
		// namespace's visible name is not a personal folder (B1).
		if h == s.primary && !s.namesPrimaryFolder(rel) {
			continue
		}
		if existing[rel] {
			continue
		}
		full := h.fullName(rel)
		if !listMatch(full, refPatterns(ref, patterns), byte(h.spec.Separator)) {
			continue
		}
		orphans = append(orphans, full)
	}
	sort.Strings(orphans)
	for _, full := range orphans {
		if err := w.WriteList(&imaplib.ListData{Mailbox: full, Delim: h.spec.Separator, Attrs: orphanAttrs(opts)}); err != nil {
			return err
		}
	}
	return nil
}

// listMaterialisedOwners opens the owner namespaces the patterns can reach --
// named outright (user/alice/*) or discovered through the registry when the
// owner segment is wildcarded (#1168) -- and lists each through the regular
// per-namespace path, ACL filtering included. Returns the visible prefixes it
// listed so the subscription pass does not emit their rows twice.
func (s *session) listMaterialisedOwners(w *imapserver.ListWriter, ref string, patterns []string, opts *imaplib.ListOptions) (map[string]bool, error) {
	done := make(map[string]bool)
	for _, spec := range s.namespaceSpecsForList() {
		if !isOwnerTemplated(spec) || !spec.List.listed() {
			continue
		}
		owners := make([]string, 0)
		enumerate := false
		for _, p := range refPatterns(ref, patterns) {
			if owner, _, ok := extractOwner(spec, p); ok && !strings.ContainsAny(owner, "*%") {
				owners = append(owners, owner)
				continue
			}
			if patternTouchesHead(p, mailbox.AdvertisedPrefix(spec.Prefix)) {
				enumerate = true
			}
		}
		// A wildcarded owner segment enumerates the registry: the owners who
		// granted this caller something (their user, their groups, anyone).
		// Rows may be stale -- a revoked grant not yet reconciled -- and each
		// owner still resolves through the same gate as any verb, so a stale
		// row and an invented owner produce the same silence (#1138).
		if enumerate && s.srv.opts.SharedDict != nil && s.userInfo != nil {
			aclUser, aclGroups := s.userInfo.ACLIdentity()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			found, err := acl.OwnersFor(ctx, s.srv.opts.SharedDict, aclUser, aclGroups)
			cancel()
			if err != nil {
				slog.Warn("imap: owner registry scan failed; discovery degraded", "sid", s.sid, "err", err)
			} else {
				owners = append(owners, found...)
			}
		}
		for _, owner := range owners {
			prefix := strings.Replace(spec.Prefix, mailbox.OwnerVar, owner, 1)
			if done[prefix] {
				continue
			}
			h, err := s.ownerHandle(spec, owner)
			if err != nil {
				// Unresolvable owner: the same nothing an ACL-hidden
				// space produces below, so the two cannot be told apart.
				continue
			}
			done[prefix] = true
			if err := s.listNamespace(w, h, ref, patterns, opts); err != nil {
				return done, err
			}
		}
	}
	return done, nil
}

// patternTouchesHead reports whether a LIST pattern can match names under the
// advertised head of a templated namespace: either the pattern descends into
// it literally, or a wildcard opens before the head ends ("*", "us*").
func patternTouchesHead(p, head string) bool {
	if head == "" {
		return false
	}
	if strings.HasPrefix(p, head) {
		return true
	}
	i := strings.IndexAny(p, "*%")
	return i >= 0 && strings.HasPrefix(head, p[:i])
}

// listTemplatedSubscriptions emits rows for subscriptions naming an
// owner-templated namespace. Resolution goes through dispatch like any verb:
// its visibility answer decides only whether the mailbox may be called
// existing -- a hidden or unresolvable owner renders the same orphan row as a
// deleted mailbox, so the #1138 oracle stays closed and the row stays visible.
// Rows under a prefix in materialised were already listed (orphans included)
// by the regular per-namespace path and are skipped here.
func (s *session) listTemplatedSubscriptions(w *imapserver.ListWriter, materialised map[string]bool, ref string, patterns []string, opts *imaplib.ListOptions) error {
	store, keyPrefix, err := s.subsView(s.primary)
	if err != nil {
		return nil
	}
	snap, err := store.Snapshot()
	if err != nil {
		slog.Warn("imap: subscription snapshot failed", "ns", "personal", "err", err)
		return nil
	}
	specs := s.namespaceSpecsForList()
	type row struct {
		name string
		sep  rune
	}
	rows := make([]row, 0)
	for key := range snap {
		rel, ok := strings.CutPrefix(key, keyPrefix)
		if !ok || rel == "" {
			continue
		}
		for _, spec := range specs {
			if !isOwnerTemplated(spec) {
				continue
			}
			owner, _, tok := extractOwner(spec, rel)
			if !tok {
				continue
			}
			if materialised[strings.Replace(spec.Prefix, mailbox.OwnerVar, owner, 1)] {
				break
			}
			rows = append(rows, row{name: rel, sep: spec.Separator})
			break
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	for _, r := range rows {
		if !listMatch(r.name, refPatterns(ref, patterns), byte(r.sep)) {
			continue
		}
		exists := false
		if h, hrel, derr := s.dispatch(r.name); derr == nil {
			if ex, ferr := h.box.FolderExists(hrel); ferr == nil {
				exists = ex
			}
		}
		attrs := orphanAttrs(opts)
		if exists {
			attrs = nil
			if opts.ReturnSubscribed && !opts.Lsub {
				attrs = []imaplib.MailboxAttr{imaplib.MailboxAttrSubscribed}
			}
		}
		if err := w.WriteList(&imaplib.ListData{Mailbox: r.name, Delim: r.sep, Attrs: attrs}); err != nil {
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

// childrenAttr returns \HasChildren when name is a hierarchy prefix of any
// other listed folder, otherwise \HasNoChildren.
func childrenAttr(name string, all []string, sep string) imaplib.MailboxAttr {
	prefix := name + sep
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
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Status")
	h, rel, err := s.dispatch(name)
	if err != nil {
		return nil, err
	}
	// Existence is checked before the index is touched, matching SELECT.
	// Without it OpenFolder *creates* the folder's index, so STATUS on a name
	// that resolves outside the mailbox initialised a fresh index at that
	// path and reported it as an empty mailbox (#1072).
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
	// Reconcile out-of-band deliveries so STATUS (a common new-mail probe)
	// reflects them without a prior SELECT.
	s.reconcileFolder(h, rel)
	f, err := h.idx.OpenFolder(rel, 0)
	if err != nil {
		return nil, err
	}
	// Heal a dbox folder flagged corrupt so STATUS counts exclude ghost records.
	if refreshed := s.dboxHealIfCorrupt(h, rel, f); refreshed != nil {
		f = refreshed
	}
	msgs, err := readMessages(h.idx, f.ID)
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
				totalSize += int64(bm.RFC822Size())
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
	if opts.MailboxID {
		d.MailboxID = mailbox.FormatObjectID(f.GUID) // RFC 8474 OBJECTID
	}
	return d, nil
}

// countingReader tallies the octets read through it, so APPEND can compare what
// the literal delivered against the declared size.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *session) Append(name string, r imaplib.LiteralReader, opts *imaplib.AppendOptions) (*imaplib.AppendData, error) {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Append")
	tAppend := time.Now()
	h, rel, f, err := s.ensureFolderHandle(name)
	if err != nil {
		return nil, tryCreate(err)
	}
	// APPEND requires 'i' (insert), in every namespace. The right depends on
	// the kind of session, not the namespace type: 'p' (post) is the delivery
	// right, set only by the LMTP path, and an IMAP session never carries it
	// (RFC 4314 §4; reference: MAILBOX_FLAG_POST_SESSION). Choosing by namespace
	// type demanded 'p' for APPEND into a shared mailbox, refusing the peer
	// granted the RFC-correct right for the operation (#1119).
	if err := s.requireRight(h, rel, mailbox.RightInsert); err != nil {
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
	// Count the octets the literal actually delivers. box.Save copies to EOF and
	// does not check the count against size, so an under-delivered literal (a
	// mid-body EOF) would be stored truncated and, being errorless, answered OK
	// (#1129). This is the invariant #1137's "stored -> OK" rests on, made
	// explicit: OK only for a fully delivered literal.
	counted := &countingReader{r: r}
	filename, vsize, guid, err := h.box.Save(rel, counted, 0, size, flagList, [16]byte{})
	if err != nil {
		return nil, err
	}
	if counted.n != size {
		// A short literal is not the malformed-tail case (#1137, a complete
		// literal with garbage after it) -- it is an incomplete message. Remove
		// it rather than keep mangled mail, and refuse.
		if rmErr := h.box.Remove(rel, filename); rmErr != nil {
			// The truncated file survived: it is exactly the orphan
			// ReconcileIndex would import with a fresh UID -- the #1129 outcome,
			// inside the branch that exists to prevent it. BAD would claim nothing
			// ran while something is on disk, so answer NO [SERVERBUG] instead and
			// log the orphan for cleanup.
			slog.Error("imap: APPEND under-delivered literal; cleanup failed, truncated file orphaned",
				"user", s.userInfo.Username, "folder", rel, "file", filename, "err", rmErr)
			return nil, &imaplib.Error{
				Type: imaplib.StatusResponseTypeNo,
				Code: imaplib.ResponseCodeServerBug,
				Text: "APPEND literal under-delivered and cleanup failed; message may be partially stored",
			}
		}
		return nil, &imaplib.Error{
			Type: imaplib.StatusResponseTypeBad,
			Code: imaplib.ResponseCodeClientBug,
			Text: fmt.Sprintf("APPEND literal under-delivered: %d of %d octets", counted.n, size),
		}
	}
	tIndex := time.Now()
	internalDate := time.Now()
	if opts != nil && !opts.Time.IsZero() {
		internalDate = opts.Time
	}
	m := &mailbox.MessageMeta{
		Filename: filename, Flags: flagList, Keywords: kwList, Size: uint32(size), VSize: vsize,
		InternalDate: internalDate, GUID: guid,
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
	s.emitMailboxChange(f, locks.EventDelivered, m.UID)

	// imapsieve (RFC 6785): run scripts bound to this mailbox on the APPEND
	// event; may refile, discard, or reflag the message just stored.
	s.runImapSieveEvent("APPEND", name, rel, h, f, m.UID, filename, m.AltTier, "", nil)

	return &imaplib.AppendData{UIDValidity: f.UIDValidity, UID: imaplib.UID(m.UID)}, nil
}

// Notify configures NOTIFY (RFC 5465) for this session. options == nil is
// NOTIFY NONE. Once NOTIFY SET is issued the selected mailbox's unsolicited
// responses are suppressed unless a SELECTED or SELECTED-DELAYED filter
// re-enables specific events (RFC 5465 §5).
func (s *session) Notify(w *imapserver.UpdateWriter, options *imaplib.NotifyOptions) error {
	s.notifyActive = false
	s.selNew, s.selExpunge, s.selFlagChange, s.selImmediateExpunge = false, false, false, false
	s.stopNotifyWatch()
	if options == nil {
		return nil
	}
	s.notifyActive = true
	s.startNotifyWatch(options.Items)
	for _, it := range options.Items {
		if it.MailboxSpec != imaplib.NotifyMailboxSpecSelected &&
			it.MailboxSpec != imaplib.NotifyMailboxSpecSelectedDelayed {
			continue
		}
		if it.MailboxSpec == imaplib.NotifyMailboxSpecSelected {
			s.selImmediateExpunge = true
		}
		for _, e := range it.Events {
			switch e {
			case imaplib.NotifyEventMessageNew:
				s.selNew = true
			case imaplib.NotifyEventMessageExpunge:
				s.selExpunge = true
			case imaplib.NotifyEventFlagChange:
				s.selFlagChange = true
			}
		}
	}
	return nil
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
// Fast-path: OpenFolder reads only the folder header for HighestModSeq;
// GetMessages (full index scan) runs only when the modseq advanced or
// pending expunges remain to deliver.
func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Poll")
	// Non-selected NOTIFY activity is independent of the selected mailbox and
	// must flush even when nothing is selected.
	if err := s.drainNotifyStatus(w); err != nil {
		return err
	}
	if s.folder == nil || s.knownMsgs == nil {
		return nil
	}

	// Reconcile out-of-band deliveries into the selected folder so an IDLE /
	// NOOP client sees new mail. Token-gated, so a quiescent folder costs one
	// stat. A change bumps HighestModSeq, which the modseq check below picks up
	// and the diff loop turns into EXISTS / EXPUNGE updates.
	if s.folderNS != nil {
		s.reconcileFolder(s.folderNS, s.folder.Name)
	}

	// Cheap modseq check — skip full scan when nothing changed and no
	// pending expunges are waiting for an allowExpunge=true window.
	refreshed, err := s.folderIdx().OpenFolder(s.folder.Name, s.folder.UIDValidity)
	if err != nil {
		return nil
	}
	// Heal a dbox folder flagged corrupt (by this or another session's read) so
	// an IDLE/NOOP client sees the ghost records expunged. The heal bumps
	// HighestModSeq, which the diff below turns into EXPUNGE updates.
	if s.folderNS != nil && refreshed.Fsckd {
		if r2 := s.dboxHealIfCorrupt(s.folderNS, s.folder.Name, refreshed); r2 != nil {
			refreshed = r2
		}
	}
	if refreshed.HighestModSeq == s.syncModSeq && !s.hasPendingExpunge {
		return nil
	}

	current, err := readMessages(s.folderIdx(), s.folder.ID)
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

	// NOTIFY (RFC 5465): once NOTIFY SET is active the selected mailbox emits
	// only the events the client re-enabled via SELECTED / SELECTED-DELAYED.
	// Without NOTIFY, all three are on (default IMAP behavior). A suppressed
	// expunge is left in knownMsgs (RFC 5465 §5) so the client's sequence view
	// stays consistent until it re-selects.
	notifyExpunge := !s.notifyActive || s.selExpunge
	notifyFlags := !s.notifyActive || s.selFlagChange
	notifyNew := !s.notifyActive || s.selNew

	// Phase 1: expunges — descending seq so each WriteExpungeUID seq number
	// remains valid as earlier entries are removed from knownMsgs.
	if notifyExpunge {
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
	}

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
		if notifyFlags && len(newKws) > 0 {
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
			// Advance the known modseq even when suppressing the FETCH, so the
			// change is not re-detected on the next poll.
			s.knownMsgs[p.seq-1].modseq = p.ci.modseq
			if !notifyFlags {
				continue
			}
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
		}
	}

	// Phase 3: new messages — UIDs in current that are not yet in knownMsgs.
	// Suppressed (not added to knownMsgs, no EXISTS) when the client turned
	// MessageNew off for the selected mailbox; it learns of them on re-select.
	if notifyNew {
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
	}

	// Only advance syncModSeq when we have fully delivered all expunges.
	// If hasPendingExpunge is true, leave syncModSeq unchanged so the next
	// call (with allowExpunge=true) re-reads the index and retries.
	if !s.hasPendingExpunge {
		s.syncModSeq = refreshed.HighestModSeq
	}
	return nil
}

// announceNewKeywords checks keywords against knownKeywords and, if any are
// new, writes an untagged "* FLAGS (...)" response via w before the caller
// sends per-message FETCH responses. This satisfies RFC 3501 §7.2.6, which
// requires the server to announce keyword flags before including them in
// FETCH data items.
func (s *session) announceNewKeywords(w *imapserver.FetchWriter, keywords []string) error {
	var newKws []string
	for _, kw := range keywords {
		if _, known := s.knownKeywords[kw]; !known {
			newKws = append(newKws, kw)
		}
	}
	if len(newKws) == 0 {
		return nil
	}
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
	return w.WriteMailboxFlags(mbFlags)
}

func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Idle")
	interval := s.srv.opts.IdleNotifyInterval

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Flush any non-selected NOTIFY activity that queued before IDLE began,
	// then wake on the watcher's signal for the rest of the IDLE window.
	if err := s.drainNotifyStatus(w); err != nil {
		return err
	}
	var wake <-chan struct{}
	if s.notifyWatch != nil {
		wake = s.notifyWatch.wake
	}

	// Cross-pod event subscription: an EVENT from another process writing this
	// user's folder refreshes the count and pushes EXISTS immediately, without
	// waiting for the timer. Selected mailbox only; non-selected mailboxes use
	// the NOTIFY watcher above.
	var events <-chan locks.Event
	var tickC <-chan time.Time
	if s.folder != nil {
		if s.srv.opts.Locker != nil && s.userInfo != nil {
			ch, err := s.srv.opts.Locker.Subscribe(ctx, locks.MailboxKey(s.userInfo.Username, s.folder.Name))
			if err != nil {
				slog.Debug("imap: idle subscribe failed; falling back to timer-only", "err", err)
			} else {
				events = ch
			}
		}
		// Heartbeat tick — a liveness signal for misbehaving clients, useful
		// even when the subscription is up.
		if interval > 0 {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			tickC = ticker.C
		}
	}

	if events == nil && tickC == nil && wake == nil {
		// Nothing to watch — purely passive IDLE.
		<-stop
		return nil
	}

	for {
		select {
		case <-stop:
			return nil
		case <-wake:
			if err := s.drainNotifyStatus(w); err != nil {
				return err
			}
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
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Expunge")
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
	// Capture the pre-expunge usage as the quota_warning "before" baseline right
	// here — the per-message expunge events below supply "after", so an "under"
	// crossing fires on a delete-only session regardless of SELECT-time seeding.
	s.captureQuotaSnap()
	refs := newBodyRefs(msgs)
	// Each expunge shifts later sequence numbers down by one, so track and
	// adjust seqNum as we go rather than using the static GetMessages index.
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
		// Free storage before dropping the index record. Storage-first so a
		// crash in the window leaves a dangling index record (reactive heal
		// expunges it) rather than an unreclaimable orphan. Best-effort: on
		// failure the index is still expunged, leak reclaimable by rebuild+purge.
		if !refs.release(m.Filename) {
			slog.Warn("imap: expunge kept the body, another record still points at it",
				"user", s.userInfo.Username, "folder", s.folder.Name, "uid", m.UID, "file", m.Filename)
		} else if rerr := s.folderBox().Remove(s.folder.Name, m.Filename); rerr != nil {
			slog.Warn("imap: expunge storage remove failed (index still expunged; leak reclaimable by rebuild+purge)",
				"user", s.userInfo.Username, "folder", s.folder.Name, "uid", m.UID, "file", m.Filename, "err", rerr)
		}
		idx.ExpungeMessage(s.folder.ID, m.UID) //nolint:errcheck
		s.emitMailboxChange(s.folder, locks.EventExpunged, m.UID)
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
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Search")
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

	msgs, err := readMessages(s.folderIdx(), s.folder.ID)
	if err != nil {
		return nil, err
	}

	needsBody := len(criteria.Header) > 0 || len(criteria.Body) > 0 || len(criteria.Text) > 0 ||
		!criteria.SentSince.IsZero() || !criteria.SentBefore.IsZero() || searchNeedsBodyRecurse(criteria.Not, criteria.Or)

	// Full-text path: answer Body/Text/Header criteria from the index and
	// scan only the candidates (https://doc.yarilomail.org/FTS §11). nil = sequential scan.
	ftsF, ftsErr := s.prepareFTSSearch(criteria, msgs)
	if ftsErr != nil {
		return nil, ftsErr
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
		// matchedOrder is every hit's UID in the same ascending enumeration
		// order as uidHits/seqHits (msgs is already scanned seq-ascending),
		// used only to build the RELEVANCY list — see relevancyScores below.
		matchedOrder []uint32
		// unreadable collects the messages whose bytes the scan could not
		// read. A body criterion cannot match what was never read, so without
		// this an unreadable mailbox and an empty one answer identically
		// (#1283).
		unreadable  []uint32
		lastReadErr error
	)
	for i, m := range msgs {
		seqNum := uint32(i + 1)

		matchCrit, needRaw := criteria, needsBody
		if ftsF != nil {
			if !ftsF.covered[m.UID] {
				continue
			}
			if ftsF.verify[m.UID] {
				needRaw = true
			} else {
				matchCrit, needRaw = ftsF.stripped, ftsF.strippedNeedsBody
			}
		}
		var rawMsg []byte
		if needRaw && m.Filename != "" {
			rc, err := s.fetchSelected(m)
			if err == nil {
				rawMsg, err = io.ReadAll(rc)
				rc.Close()
			}
			if err != nil {
				// MatchMessage answers TRUE for a BODY/TEXT criterion when it
				// is given no message: with the read error dropped, every
				// message the scan could not read matched every body search
				// (#1283). Excluded and counted instead -- a message nobody
				// looked at is not a message that matched.
				unreadable = append(unreadable, m.UID)
				lastReadErr = err
				continue
			}
		}

		imapFlags := make([]imaplib.Flag, len(m.Flags)+len(m.Keywords))
		for j, f := range m.Flags {
			imapFlags[j] = imaplib.Flag(f)
		}
		for j, k := range m.Keywords {
			imapFlags[len(m.Flags)+j] = imaplib.Flag(k)
		}

		if !imapserver.MatchMessage(seqNum, imaplib.UID(m.UID), m.InternalDate, int64(m.RFC822Size()), imapFlags, rawMsg, matchCrit) {
			continue
		}

		// CONDSTORE SEARCH MODSEQ filter (RFC 7162 §3.1.5). Per-attribute
		// modseq tracking is unimplemented: the attribute qualifier is ignored
		// and criteria.ModSeq compared at message level. Strictly more
		// permissive (extra matches), which is RFC-acceptable.
		if criteria.ModSeq != nil && m.ModSeq < criteria.ModSeq.ModSeq {
			continue
		}
		hitCount++
		matchedOrder = append(matchedOrder, m.UID)
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

	// A message the scan could not read did not "not match" -- it was never
	// looked at. Reported once per SEARCH with counts and one example error,
	// never per message: a mailbox whose storage is gone would otherwise write
	// a line per record. WARN because the answer the client is about to get is
	// incomplete and nothing else says so.
	if len(unreadable) > 0 {
		metricSearchUnreadable.Add(float64(len(unreadable)))
		slog.Warn("imap: search could not read some messages; the result is incomplete",
			"user", s.userInfo.Username,
			"folder", s.folder.Name,
			"unreadable", len(unreadable),
			"records_scanned", len(msgs),
			"matched", hitCount,
			"first_unreadable_uid", unreadable[0],
			"err", lastReadErr,
		)
	}

	// On a zero-match search, log the scanned record count and UID range so a
	// delivery/visibility mismatch is diagnosable. DEBUG-gated, counts only.
	if hitCount == 0 {
		var uidMin, uidMax uint32
		for _, m := range msgs {
			if uidMin == 0 || m.UID < uidMin {
				uidMin = m.UID
			}
			if m.UID > uidMax {
				uidMax = m.UID
			}
		}
		slog.Debug("imap: search matched no messages",
			"user", s.userInfo.Username,
			"folder", s.folder.Name,
			"records_scanned", len(msgs),
			"uid_min", uidMin,
			"uid_max", uidMax,
			"kind", kind,
			"unreadable", len(unreadable),
		)
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
	// CONDSTORE (RFC 7162 §3.1.5): surface the max matched modseq so the client
	// can persist its highest-seen modseq, regardless of RETURN items requested.
	if highestMod > 0 {
		data.ModSeq = highestMod
	}
	// RELEVANCY (RFC 4731/6203): only available when FTS engaged and returned
	// scores. A sequential scan has no ranking signal; nil omits the item
	// from the response rather than erroring.
	if opts != nil && opts.ReturnRelevancy && ftsF != nil && ftsF.scores != nil {
		data.Relevancy = relevancyScores(ftsF.scores, matchedOrder)
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
				matchCrit, needRaw := criteria, needsBody
				if ftsF != nil {
					if !ftsF.covered[m.UID] {
						continue
					}
					if ftsF.verify[m.UID] {
						needRaw = true
					} else {
						matchCrit, needRaw = ftsF.stripped, ftsF.strippedNeedsBody
					}
				}
				var raw []byte
				if needRaw && m.Filename != "" {
					if rc, err := s.fetchSelected(m); err == nil {
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
				if imapserver.MatchMessage(uint32(i+1), imaplib.UID(m.UID), m.InternalDate, int64(m.RFC822Size()), imapFlags, raw, matchCrit) {
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
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Fetch")
	if s.folder == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	if err := s.requireRightOnSelected(mailbox.RightRead); err != nil {
		return err
	}
	idx := s.folderIdx()
	backendMsgs, err := readMessages(idx, s.folder.ID)
	if err != nil {
		return err
	}
	// UID FETCH (CHANGEDSINCE N VANISHED) — RFC 7162 §3.2.10. Emit
	// VANISHED (EARLIER) for UIDs expunged since the supplied modseq.
	// VANISHED on a sequence-number FETCH is invalid; the patched lib
	// rejects that at parse time so we do not need to re-check here.
	if opts.Vanished && opts.ChangedSince > 0 {
		vanishedUIDs, vErr := readVanished(idx, s.folder.ID, opts.ChangedSince)
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

	type fetchEntry struct {
		seqNum uint32
		msg    *mailbox.MessageMeta
	}
	// Report sequence numbers from the client's knownMsgs view (both seq and
	// UID FETCH), avoiding "UID changed" errors when another session expunged
	// messages and the backend positions shifted before the client got the
	// EXPUNGE. Messages expunged since the last Poll are skipped; Poll delivers
	// their EXPUNGE after the tagged response.
	uidToClientSeq := make(map[uint32]uint32, len(s.knownMsgs))
	for i, km := range s.knownMsgs {
		uidToClientSeq[km.uid] = uint32(i + 1)
	}
	numSet = resolveStar(numSet, backendMsgs, uidToClientSeq)

	var fetchList []fetchEntry
	if _, isUID := numSet.(imaplib.UIDSet); isUID {
		for _, m := range backendMsgs {
			seqNum, ok := uidToClientSeq[m.UID]
			if !ok {
				// UID appended after the pre-OK poll: not yet in the client's
				// sequence view. Skip; the client learns of it on the next Poll.
				slog.Debug("imap: fetch skipped uid absent from client view",
					"user", s.userInfo.Username,
					"folder", s.folder.Name,
					"uid", m.UID,
					"known_msgs", len(s.knownMsgs),
				)
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
	// The index cache serves the listing's hot path without opening message
	// files (#1030): ENVELOPE and BODYSTRUCTURE alike, which is also what
	// removes the SECOND open a FETCH (ENVELOPE BODYSTRUCTURE) used to pay.
	// Opened once per FETCH, misses parsed and written back, offsets stamped
	// in one batch on close. nil-safe: any cache trouble degrades to
	// parsing, never to a client error.
	var envCache *msgcache.Handle
	if opts.Envelope || opts.BodyStructure != nil {
		envCache = msgcache.Open(s.folderIdx(), s.folder.ID, msgcache.Options{
			Locker:  s.srv.opts.Locker,
			User:    s.userInfo.Username,
			Folder:  s.folder.Name,
			TraceID: s.sid,
		})
		defer envCache.Close()
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
			// Add the flag, do not declare the set. The flags in hand came from
			// the read this response is built on; writing them back as an
			// absolute list drops anything another session set in between
			// (#1250).
			if uerr := idx.AddFlags(s.folder.ID, m.UID, []string{`\Seen`}, nil); uerr == nil {
				m.Flags = append(append([]string(nil), m.Flags...), `\Seen`)
				seenJustSet = true
				s.emitMailboxChange(s.folder, locks.EventChanged, m.UID)
			}
		}
		if opts.Flags || seenJustSet {
			if err := s.announceNewKeywords(w, m.Keywords); err != nil {
				return err
			}
			mw.WriteFlags(toImapFlags(append(m.Flags, m.Keywords...)))
		}
		if opts.UID {
			mw.WriteUID(imaplib.UID(m.UID))
		}
		if opts.InternalDate {
			mw.WriteInternalDate(m.InternalDate)
		}
		if opts.RFC822Size {
			// Virtual (CRLF) size: the octet count actually transmitted.
			// Physical size varies with stored line endings.
			size := m.RFC822Size()
			// Records with VSize==0 predate Save() returning virtual size and
			// fall back to physical size; recompute from the body on read so
			// the reported size stays stable.
			if m.VSize == 0 && m.Filename != "" {
				if rc, ferr := s.fetchSelected(m); ferr == nil {
					if raw, rerr := io.ReadAll(rc); rerr == nil {
						size = virtualSizeFromRaw(raw)
					}
					rc.Close()
				}
			}
			mw.WriteRFC822Size(int64(size))
		}
		if opts.ModSeq && m.ModSeq > 0 {
			mw.WriteModSeq(m.ModSeq)
		}
		if opts.EmailID {
			mw.WriteEmailID(mailbox.FormatObjectID(m.GUID)) // RFC 8474 OBJECTID
		}
		if opts.ThreadID {
			// RFC 8474: NIL means the server cannot determine a thread, which
			// is exactly true for an account the migration step has not
			// reached -- and is what this answered for every message before
			// threading existed.
			mw.WriteThreadID(s.threadIDOf(m))
		}
		if opts.Envelope && m.Filename != "" {
			if env := envCache.Envelope(m); env != nil {
				mw.WriteEnvelope(env)
			} else if rc, ferr := s.fetchSelected(m); ferr == nil {
				hdr, _ := textproto.ReadHeader(bufio.NewReader(rc))
				rc.Close()
				env := imapserver.ExtractEnvelope(hdr)
				mw.WriteEnvelope(env)
				envCache.StoreEnvelope(m, env)
			}
		}
		if opts.BodyStructure != nil && m.Filename != "" {
			if bs := envCache.BodyStructure(m); bs != nil {
				mw.WriteBodyStructure(bs)
			} else if rc, ferr := s.fetchSelected(m); ferr == nil {
				bs := imapserver.ExtractBodyStructure(rc)
				rc.Close()
				mw.WriteBodyStructure(bs)
				envCache.StoreBodyStructure(m, bs)
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
			rc, ferr := s.fetchSelected(m)
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
				// The section could not be produced: since #1377 the library
				// serves a message whose header it cannot parse, so what is
				// left is a part spec that needs a parse the message does not
				// support. An empty literal is the only thing the protocol
				// lets us put here, so the reason goes to the log rather than
				// nowhere -- a client seeing {0} has no way to tell "empty"
				// from "we could not".
				slog.Warn("imap: fetch produced no data for a body section",
					"user", s.userInfo.Username, "folder", s.folder.Name,
					"uid", m.UID, "file", m.Filename,
					"specifier", string(section.Specifier), "part", section.Part)
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
			rc, ferr := s.fetchSelected(m)
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
			rc, ferr := s.fetchSelected(m)
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
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Store")
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
	numSet = resolveStar(numSet, msgs, uidToClientSeq)

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
		filename string
		altTier  bool
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
		// FLAGS (...) declares the set, so the set is what we send. +FLAGS and
		// -FLAGS are deltas by definition: send the named flags and let the
		// index resolve them against the record under its own lock. Sending the
		// computed set instead would carry this read forward and overwrite
		// whatever another session changed in between (#1250).
		upd := mailbox.FlagsUpdate{Flags: newFlags, Keywords: newKW}
		switch storeFlags.Op {
		case imaplib.StoreFlagsAdd:
			upd = storeDelta(storeFlags, mailbox.FlagsAdd)
		case imaplib.StoreFlagsDel:
			upd = storeDelta(storeFlags, mailbox.FlagsRemove)
		}
		pending = append(pending, pendingStore{seqNum, m.UID, newFlags, newKW, m.Filename, m.AltTier})
		batchUpdates[m.UID] = upd
	}

	// Pass 2: single lock/reload/flush for all flag updates.
	var results map[uint32]mailbox.FlagsResult
	if len(batchUpdates) > 0 {
		var err error
		results, err = idx.UpdateFlagsMulti(s.folder.ID, batchUpdates)
		if err != nil {
			// Classified before it leaves: an unwrapped error becomes
			// NO [SERVERBUG] in the library, which tells the client this
			// server is broken when the lock service is merely restarting.
			return dependencyError(err)
		}
		// Under a delta the resulting set is only known after the write, so the
		// untagged FETCH reports what the index holds rather than what this
		// command predicted from its own read.
		for i := range pending {
			if r, ok := results[pending[i].uid]; ok {
				pending[i].newFlags, pending[i].newKW = r.Flags, r.Keywords
			}
		}
		s.emitMailboxChange(s.folder, locks.EventChanged, 0)
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
		if r, ok := results[s.knownMsgs[i].uid]; ok {
			s.knownMsgs[i].modseq = r.ModSeq
		}
	}
	if !storeFlags.Silent {
		// Collect all new keywords across pending messages and announce once.
		var allNewKWs []string
		seen := make(map[string]struct{})
		for _, p := range pending {
			for _, kw := range p.newKW {
				if _, dup := seen[kw]; !dup {
					seen[kw] = struct{}{}
					allNewKWs = append(allNewKWs, kw)
				}
			}
		}
		if err := s.announceNewKeywords(w, allNewKWs); err != nil {
			return err
		}
		for _, p := range pending {
			mw := w.CreateMessage(p.seqNum)
			mw.WriteFlags(toImapFlags(append(p.newFlags, p.newKW...)))
			mw.WriteUID(imaplib.UID(p.uid))
			if r := results[p.uid]; r.ModSeq > 0 {
				mw.WriteModSeq(r.ModSeq)
			}
			mw.Close() //nolint:errcheck
		}
	}

	// imapsieve (RFC 6785): after the STORE responses are sent, the FLAG cause
	// fires on the selected mailbox for each message whose flags changed; the
	// script may refile / discard / reflag it. Gated on a bound script (or
	// globals) so a bulk STORE with no imapsieve script fetches nothing.
	if eng := s.srv.opts.SieveEngine; eng != nil && eng.ImapSieveEnabled() && storeFlags != nil && len(pending) > 0 {
		scriptName := s.imapSieveScriptName(s.folderNS, s.folder.Name, s.folder.GUID)
		if scriptName != "" || eng.HasImapGlobals() {
			changed := make([]string, 0, len(storeFlags.Flags))
			for _, fl := range storeFlags.Flags {
				changed = append(changed, string(fl))
			}
			for _, p := range pending {
				s.runImapSieveEvent("FLAG", s.folder.Name, s.folder.Name, s.folderNS, s.folder, p.uid, p.filename, p.altTier, "", changed)
			}
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
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Copy")
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
		return nil, tryCreate(err)
	}
	if err := s.requireRight(destH, destRel, mailbox.RightInsert); err != nil {
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
	numSet = resolveStar(numSet, msgs, copyUIDToClientSeq)
	var srcUIDs, dstUIDs imaplib.UIDSet
	var saveTotalMs, indexTotalMs int64
	var count int
	type copiedMsg struct {
		uid      uint32
		filename string
	}
	var copied []copiedMsg
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
		// COPY yields a distinct message, so a fresh GUID is generated (RFC 8474);
		// only MOVE preserves the source identity.
		newFilename, vsize, guid, saveErr := destH.box.Save(destRel, bytes.NewReader(data), 0, int64(len(data)), m.Flags, [16]byte{})
		if saveErr != nil {
			return nil, fmt.Errorf("imap/copy save: %w", saveErr)
		}
		saveTotalMs += time.Since(tSave).Milliseconds()
		nm := &mailbox.MessageMeta{
			Filename:     newFilename,
			Flags:        m.Flags,
			Keywords:     m.Keywords,
			Size:         uint32(len(data)),
			VSize:        vsize,
			InternalDate: m.InternalDate,
			GUID:         guid,
		}
		tIndex := time.Now()
		if err := destH.idx.AllocateAndAppend(destFolder.ID, nm); err != nil {
			_ = destH.box.Remove(destRel, newFilename)
			return nil, fmt.Errorf("imap/copy record: %w", err)
		}
		indexTotalMs += time.Since(tIndex).Milliseconds()
		count++
		s.emitMailboxChange(destFolder, locks.EventDelivered, nm.UID)
		srcUIDs.AddNum(imaplib.UID(m.UID))
		dstUIDs.AddNum(imaplib.UID(nm.UID))
		copied = append(copied, copiedMsg{uid: nm.UID, filename: newFilename})
	}
	// imapsieve (RFC 6785): COPY cause fires on the destination mailbox for each
	// copied message, after the copy is recorded (so COPYUID reflects the initial
	// landing). Scripts may refile / discard / reflag the copy.
	if s.srv.opts.SieveEngine != nil {
		for _, cm := range copied {
			s.runImapSieveEvent("COPY", dest, destRel, destH, destFolder, cm.uid, cm.filename, false, s.folder.Name, nil)
		}
	}
	slog.Debug("imap: copy timing",
		"user", s.userInfo.Username, "src", s.folder.Name, "dst", dest,
		"count", count, "save_ms", saveTotalMs, "index_ms", indexTotalMs,
		"total_ms", time.Since(tCopy).Milliseconds())
	// COPYUID needs at least one pair; the encoder rejects an empty set and
	// would truncate the reply mid-line. A zero-match COPY is a plain OK.
	if count == 0 {
		return nil, nil
	}
	return &imaplib.CopyData{
		UIDValidity: destFolder.UIDValidity,
		SourceUIDs:  srcUIDs,
		DestUIDs:    dstUIDs,
	}, nil
}

func (s *session) Namespace() (*imaplib.NamespaceData, error) {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Namespace")
	specs := s.srv.opts.Namespaces
	if len(specs) == 0 {
		specs = defaultNamespaces
	}
	var data imaplib.NamespaceData
	for _, ns := range specs {
		if !ns.List.listed() {
			continue
		}
		// An owner-templated prefix is advertised truncated at the variable
		// (user/%u/ -> user/): the prefix is what a client prepends to a
		// name, and the template prepends to nothing (#1171).
		desc := imaplib.NamespaceDescriptor{Prefix: mailbox.AdvertisedPrefix(ns.Prefix), Delim: ns.Separator}
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
	{Type: NamespacePersonal, Prefix: "", Separator: '.', List: ListYes},
}

// GetMetadata implements RFC 5464 GETMETADATA. mailbox == "" requests
// server-scope annotations (stored under INBOX's GUID with a vendor
// prefix so they cannot collide with INBOX's own mailbox attributes).
// Per RFC 5464, options.Depth controls whether entries below the
// requested name are included (0 = exact, 1 = direct children,
// infinity = whole subtree); options.MaxSize lets the server elide
// entries larger than the supplied byte cap.
func (s *session) GetMetadata(folder string, entries []string, opts *imaplib.GetMetadataOptions) (*imaplib.GetMetadataData, error) {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "GetMetadata")
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
	slog.Debug("imap: command", "sid", s.sid, "cmd", "SetMetadata")
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
	// RFC 5464 mailbox METADATA is gated by ACL: the accessing user needs
	// the lookup right plus one access right. Server-scope (folder == "")
	// has no mailbox to gate. Owner / ACL-disabled short-circuit inside.
	if folder != "" {
		if err := s.requireMetadataAccess(h, rel); err != nil {
			return nil, [16]byte{}, err
		}
	}
	// The last OpenFolder outside ensureFolderHandle, and it carried the same
	// defect: METADATA on a name that resolves outside the mailbox answered OK
	// and left a complete index behind for a mailbox that does not exist
	// (#1072).
	exists, err := h.box.FolderExists(rel)
	if err != nil {
		return nil, [16]byte{}, err
	}
	if !exists {
		return nil, [16]byte{}, &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
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
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Move")
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
		return tryCreate(err)
	}
	if err := s.requireRight(destH, destRel, mailbox.RightInsert); err != nil {
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
	numSet = resolveStar(numSet, msgs, moveUIDToClientSeq)

	type matched struct {
		seqNum   uint32
		srcUID   uint32
		filename string
		destUID  uint32
		destFile string
		moved    bool // body relocated in place; source file is already gone
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
		// MOVE keeps one identity across folders (RFC 8474), so the source GUID is
		// carried into the destination instead of a fresh one being generated.
		var newFilename string
		var guid [16]byte
		vsize := m.VSize
		size := m.Size
		tSave := time.Now()
		if srcBox == destH.box {
			var moveErr error
			newFilename, guid, moveErr = srcBox.Move(s.folder.Name, destRel, m.Filename, m.GUID)
			if moveErr != nil {
				return fmt.Errorf("imap/move relocate: %w", moveErr)
			}
		} else {
			// Cross-namespace: no shared storage to relocate within, so copy the
			// body over and hand the source GUID to Save, which stores it verbatim.
			rc, fetchErr := srcBox.Fetch(s.folder.Name, m.Filename, m.AltTier)
			if fetchErr != nil {
				return fmt.Errorf("imap/move fetch: %w", fetchErr)
			}
			data, readErr := io.ReadAll(rc)
			rc.Close()
			if readErr != nil {
				return fmt.Errorf("imap/move read: %w", readErr)
			}
			var saveErr error
			newFilename, vsize, guid, saveErr = destH.box.Save(destRel, bytes.NewReader(data), 0, int64(len(data)), m.Flags, m.GUID)
			if saveErr != nil {
				return fmt.Errorf("imap/move save: %w", saveErr)
			}
			size = uint32(len(data))
		}
		saveTotalMs += time.Since(tSave).Milliseconds()
		nm := &mailbox.MessageMeta{
			Filename:     newFilename,
			Flags:        m.Flags,
			Keywords:     m.Keywords,
			Size:         size,
			VSize:        vsize,
			InternalDate: m.InternalDate,
			GUID:         guid,
		}
		tIndex := time.Now()
		if err := destH.idx.AllocateAndAppend(destFolder.ID, nm); err != nil {
			if srcBox == destH.box {
				_, _, _ = srcBox.Move(destRel, s.folder.Name, newFilename, guid)
			} else {
				_ = destH.box.Remove(destRel, newFilename)
			}
			return fmt.Errorf("imap/move record: %w", err)
		}
		indexTotalMs += time.Since(tIndex).Milliseconds()
		s.emitMailboxChange(destFolder, locks.EventDelivered, nm.UID)
		srcUIDs.AddNum(imaplib.UID(m.UID))
		dstUIDs.AddNum(imaplib.UID(nm.UID))
		hits = append(hits, matched{seqNum: seqNum, srcUID: m.UID, filename: m.Filename, destUID: nm.UID, destFile: newFilename, moved: srcBox == destH.box})
	}

	// COPYUID needs at least one pair; the encoder rejects an empty set and
	// would truncate the reply mid-line. A zero-match MOVE is a plain OK.
	if len(hits) > 0 {
		if err := w.WriteCopyData(&imaplib.CopyData{
			UIDValidity: destFolder.UIDValidity,
			SourceUIDs:  srcUIDs,
			DestUIDs:    dstUIDs,
		}); err != nil {
			return err
		}
	}

	// Expunge source in descending seq order (RFC 6851 §3.3).
	for i := len(hits) - 1; i >= 0; i-- {
		h := hits[i]
		if !h.moved {
			srcBox.Remove(s.folder.Name, h.filename) //nolint:errcheck
		}
		srcIdx.ExpungeMessage(s.folder.ID, h.srcUID) //nolint:errcheck
		s.emitMailboxChange(s.folder, locks.EventExpunged, h.srcUID)
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
	// imapsieve (RFC 6785): a MOVE lands each message in the destination — the
	// COPY cause fires there after the move completes; scripts may refile/discard.
	if s.srv.opts.SieveEngine != nil {
		src := s.folder.Name
		for _, h := range hits {
			s.runImapSieveEvent("COPY", dest, destRel, destH, destFolder, h.destUID, h.destFile, false, src, nil)
		}
	}
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
// errFolderNotFound reports a mailbox that does not exist. Each caller answers
// with its own response code -- APPEND, COPY and MOVE owe the client TRYCREATE
// (RFC 9051), which NONEXISTENT would not tell it.
var errFolderNotFound = errors.New("imap: no such mailbox")

func (s *session) ensureFolderHandle(name string) (*nsHandle, string, *mailbox.Folder, error) {
	h, rel, err := s.dispatch(name)
	if err != nil {
		return nil, "", nil, err
	}
	if s.folder != nil && s.folder.Name == rel && s.folderNS == h {
		return h, rel, s.folder, nil
	}
	// Existence is checked before the index is opened, not after. OpenFolder
	// creates what it is asked for with a fresh UIDVALIDITY, so every caller
	// that checked afterwards had already made index state for a mailbox that
	// does not exist -- the same defect STATUS had, reached through APPEND,
	// COPY and MOVE (#1072).
	exists, err := h.box.FolderExists(rel)
	if err != nil {
		return nil, "", nil, err
	}
	if !exists {
		return nil, "", nil, errFolderNotFound
	}
	f, err := h.idx.OpenFolder(rel, uint32(time.Now().Unix()))
	if err != nil {
		return nil, "", nil, err
	}
	return h, rel, f, nil
}

// tryCreate turns the sentinel into the answer APPEND, COPY and MOVE owe a
// client naming a mailbox that is not there, and a storage name refusal into
// CANNOT. Anything else the library reports as an internal server error.
func tryCreate(err error) error {
	switch {
	case errors.Is(err, errFolderNotFound):
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCodeTryCreate,
			Text: "No such mailbox",
		}
	default:
		return nameError(err)
	}
}

// refPatterns combines the LIST reference with each pattern, which is what the
// reference is for: "the reference argument and the mailbox name argument are
// concatenated" (RFC 9051 6.3.9). It selects what to match, not what to
// return.
func refPatterns(ref string, patterns []string) []string {
	if ref == "" {
		return patterns
	}
	out := make([]string, len(patterns))
	for i, p := range patterns {
		out[i] = ref + p
	}
	return out
}

// listMatch reports whether name matches any of the LIST patterns, with the
// wildcards RFC 9051 6.3.9 defines: "*" spans hierarchy levels, "%" stops at
// the separator.
//
// It used to accept only a bare "*" or "%" or an exact name, so every other
// pattern matched nothing at all: LIST "" "Shared/*" and LIST "" "W*" both
// answered empty, and "%" behaved like "*". A client that lists a subtree --
// which is the ordinary way to explore a namespace -- was told it is empty
// (#1099).
func listMatch(name string, patterns []string, sep byte) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if matchPattern(name, p, sep) {
			return true
		}
	}
	return false
}

// matchPattern is the RFC 9051 6.3.9 wildcard match.
//
// Case-sensitive, except that INBOX is matched case-insensitively -- the one
// name RFC 9051 5.1 says is. Folding everything would have been a quiet
// behaviour change riding in with the rewrite: mailbox names are
// case-sensitive here and ValidateFolderName says so, so LIST "" "work" must
// not return "Work".
//
// sep == 0 makes "%" equivalent to "*": with no hierarchy separator there is no
// level for it to stop at.
func matchPattern(name, pattern string, sep byte) bool {
	if strings.EqualFold(name, "INBOX") && strings.EqualFold(pattern, "INBOX") {
		return true
	}

	var match func(n, p int) bool
	match = func(n, p int) bool {
		for p < len(pattern) {
			switch pattern[p] {
			case '*':
				// Spans everything, including separators.
				for i := n; i <= len(name); i++ {
					if match(i, p+1) {
						return true
					}
				}
				return false
			case '%':
				// Stops at the separator: try every split up to the next one.
				for i := n; i <= len(name); i++ {
					if i > n && sep != 0 && name[i-1] == sep {
						break
					}
					if match(i, p+1) {
						return true
					}
				}
				return false
			default:
				if n >= len(name) || name[n] != pattern[p] {
					return false
				}
				n++
				p++
			}
		}
		return n == len(name)
	}
	return match(0, 0)
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

// virtualSizeFromRaw returns the octet count the message occupies once every
// bare LF is normalised to CRLF — the RFC822.SIZE a client observes on the
// wire. It mirrors the counting the storage backends do at Save() time and is
// the fallback for legacy index records that never recorded a virtual size.
func virtualSizeFromRaw(raw []byte) uint32 {
	n := uint32(len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\n' && (i == 0 || raw[i-1] != '\r') {
			n++
		}
	}
	return n
}

// bodyRefs counts how many index records name each file. Mailboxes damaged
// before the reconcile guard hold several records for one file, and unlinking
// on the first expunge would strip the body from the ones still live.
type bodyRefs map[string]int

func newBodyRefs(msgs []*mailbox.MessageMeta) bodyRefs {
	r := make(bodyRefs, len(msgs))
	for _, m := range msgs {
		if m.Filename != "" {
			r[m.Filename]++
		}
	}
	return r
}

// release drops one reference and reports whether the body can now be freed.
func (r bodyRefs) release(filename string) bool {
	if filename == "" {
		return false
	}
	r[filename]--
	return r[filename] <= 0
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

// resolveStar rewrites a bare "*" as the largest number in the mailbox
// (RFC 3501 §9). It is encoded {0,0} and Range.Contains requires a non-zero
// Start, so it matches nothing unresolved; "n:*" is {n,0} and already works.
func resolveStar(numSet imaplib.NumSet, msgs []*mailbox.MessageMeta, uidToSeq map[uint32]uint32) imaplib.NumSet {
	var maxUID, maxSeq uint32
	for _, m := range msgs {
		if m.UID > maxUID {
			maxUID = m.UID
		}
		if seq, ok := uidToSeq[m.UID]; ok && seq > maxSeq {
			maxSeq = seq
		}
	}
	switch ns := numSet.(type) {
	case imaplib.UIDSet:
		if maxUID == 0 {
			return ns
		}
		out := make(imaplib.UIDSet, len(ns))
		copy(out, ns)
		for i := range out {
			if out[i].Start == 0 && out[i].Stop == 0 {
				out[i] = imaplib.UIDRange{Start: imaplib.UID(maxUID), Stop: imaplib.UID(maxUID)}
			}
		}
		return out
	case imaplib.SeqSet:
		if maxSeq == 0 {
			return ns
		}
		out := make(imaplib.SeqSet, len(ns))
		copy(out, ns)
		for i := range out {
			if out[i].Start == 0 && out[i].Stop == 0 {
				out[i] = imaplib.SeqRange{Start: maxSeq, Stop: maxSeq}
			}
		}
		return out
	}
	return numSet
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

// storeDelta turns a +FLAGS / -FLAGS command into an index update that names
// only the flags it changes.
func storeDelta(store *imaplib.StoreFlags, mode mailbox.FlagsMode) mailbox.FlagsUpdate {
	upd := mailbox.FlagsUpdate{Mode: mode}
	for _, f := range store.Flags {
		if strings.HasPrefix(string(f), `\`) {
			upd.Flags = append(upd.Flags, string(f))
		} else {
			upd.Keywords = append(upd.Keywords, string(f))
		}
	}
	return upd
}

// unlockedReader is the optional capability an index has when its files can
// prove their own freshness: a read that only answers a client can skip the
// cross-process lock. Declared here rather than on mailbox.UserIndex so an
// index without the property is simply an index without the method.
// unlockedReader adds the two reads beyond the message list that IMAP serves
// straight to the client. The message list itself goes through
// mailbox.ReadMessages, which four packages share.
type unlockedReader interface {
	VanishedUnlocked(folderID uint64, sinceModSeq uint64) ([]uint32, error)
	KeywordsUnlocked(folderID uint64) ([]string, error)
}

// readMessages is the read for handlers whose answer goes to the client and
// decides nothing on disk. Callers whose answer drives a write or a delete --
// STORE, EXPUNGE, COPY, MOVE -- must keep using GetMessages, which is why this
// is a separate function rather than a swap inside one (#1249).
func readMessages(idx mailbox.UserIndex, folderID uint64) ([]*mailbox.MessageMeta, error) {
	return mailbox.ReadMessages(idx, folderID, mailbox.SeqSet{})
}

// readVanished and readKeywords are the same contract for the other two reads
// that only answer the client: QRESYNC/CHANGEDSINCE and the SELECT keyword
// list.
func readVanished(idx mailbox.UserIndex, folderID uint64, sinceModSeq uint64) ([]uint32, error) {
	if u, ok := idx.(unlockedReader); ok {
		return u.VanishedUnlocked(folderID, sinceModSeq)
	}
	return idx.Vanished(folderID, sinceModSeq)
}

func readKeywords(idx mailbox.UserIndex, folderID uint64) ([]string, error) {
	if u, ok := idx.(unlockedReader); ok {
		return u.KeywordsUnlocked(folderID)
	}
	return idx.Keywords(folderID)
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
