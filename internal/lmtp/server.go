// Package lmtp implements an LMTP server (RFC 2033) for local mail delivery.
package lmtp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/internal/loginproto"
	"github.com/yarilomail/yarilo/internal/quotawarn"
	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/internal/userstate/acl"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// Options configures the LMTP server.
type Options struct {
	Hostname string
	Config   config.LMTPProtocolConfig
	Mailbox  mailbox.MailboxBackend
	Index    mailbox.IndexBackend
	Resolver *mailbox.Resolver

	// TLSConfig enables STARTTLS on the LMTP listener.
	// For immediate TLS (ssl mode), wrap the listener before calling Serve().
	TLSConfig *tls.Config

	// Router resolves recipient usernames to backend IPs. Non-nil on director
	// nodes activates proxy mode; nil on backend nodes means local delivery.
	Router UserRouter
	// BackendPort is the LMTP port on backend pods. Default: 24.
	BackendPort int

	// Locker is the cross-process write coordinator. Non-nil emits a
	// `delivered` EVENT after each delivery so IMAP IDLE on other pods
	// wakes up; nil disables cross-pod notifications.
	Locker locks.Locker

	// FTSClient, when set with FTSAutoindex, queues an INDEX after each
	// accepted delivery. Best-effort: rescan heals lost hooks.
	FTSClient    ftsproto.Client
	FTSAutoindex bool
	FTSMaxRecent int

	// UserdbLookup fetches per-user storage config before accepting RCPT TO.
	// (nil, nil) = user not found → 550; (nil, err) = transient → 451.
	// Nil skips the check (unit tests / director-only nodes).
	UserdbLookup func(ctx context.Context, username string) (*mailbox.UserInfo, error)

	// QuotaEngine enables quota enforcement on delivery: over-quota messages
	// are rejected with 452 4.2.2. Usage is summed from the recipient's index.
	QuotaEngine bool

	// QuotaExceededMessage is the 452 text when a delivery is over quota.
	QuotaExceededMessage string
	// QuotaMailSize rejects a single message larger than this many bytes
	// (0 = unlimited), independent of the usage limit.
	QuotaMailSize int64
	// QuotaPolicy carries the site-wide quota tunables. On this inbound-delivery
	// path storage grace IS applied (LMTP/LDA overshoot).
	QuotaPolicy quota.Policy
	// QuotaWarner runs quota_warning actions. Nil = warnings only log.
	QuotaWarner *quotawarn.Runner
	// QuotaClone mirrors usage to external dicts after delivery. Nil = disabled.
	QuotaClone *quota.Clone

	// MetadataDict backs the mboxmetadata / servermetadata Sieve tests
	// (RFC 5490 §4); same dict IMAP uses for RFC 5464 METADATA. Nil
	// reports every annotation as absent.
	MetadataDict dict.Dict

	// AuthAddr is the yarilo-auth client-protocol address used to verify
	// session tokens forwarded by lmtp-login. Empty skips preamble
	// verification (unit tests, or lmtp-login not in the path).
	AuthAddr string
	// AuthTLS optionally wraps the auth-client dialer with mTLS.
	AuthTLS     *tls.Config
	PreambleTLS *tls.Config // internal mTLS on the data path (#824)

	// SieveEngine executes per-user Sieve scripts during local delivery.
	// Nil disables filtering; messages go to the default folder.
	SieveEngine *sieve.Engine

	// MailboxByDriver, when non-nil, returns a MailboxBackend for a driver
	// name, selecting per-user storage when the user's mail_location driver
	// differs from the global default.
	MailboxByDriver func(driver string) mailbox.MailboxBackend

	// Namespaces routes a namespace-prefixed target folder (e.g. "Public/News")
	// to that namespace's storage. Empty = everything lands in the recipient's
	// personal store.
	Namespaces []config.NamespaceConfig

	// ACL enforcement for cross-namespace delivery: shared/public targets
	// require the 'p' (post) right; a denial falls back to INBOX (implicit
	// keep). The personal store is never ACL-checked. The ACL* fields mirror
	// the IMAP resolution so both paths agree.
	ACLEnabled           bool
	ACLGlobal            *acl.Global
	ACLGlobalsOnly       bool
	ACLDefaultsFromInbox bool
	ACLCacheTTL          time.Duration
}

// Server is an LMTP server backed by a MailboxBackend and IndexBackend.
type Server struct {
	srv    *goSmtp.Server
	opts   Options
	router *proxyRouter // non-nil when proxy mode is active
}

// New creates an LMTP server from Options.
func New(opts Options) *Server {
	var router *proxyRouter
	if opts.Router != nil {
		timeout := time.Duration(opts.Config.Proxy.Timeout) * time.Second
		if timeout == 0 {
			timeout = 125 * time.Second
		}
		router = newProxyRouter(opts.Hostname, opts.Router, opts.BackendPort, timeout)
	}

	s := &Server{opts: opts, router: router}
	be := &backend{opts: opts, router: router, srv: s}

	srv := goSmtp.NewServer(be)
	srv.Domain = opts.Hostname
	srv.LMTP = true
	srv.TLSConfig = opts.TLSConfig
	srv.ReadTimeout = time.Duration(opts.Config.ReadTimeout) * time.Second
	srv.WriteTimeout = time.Duration(opts.Config.WriteTimeout) * time.Second

	s.srv = srv
	return s
}

// Serve starts accepting LMTP connections on ln. When AuthAddr is set, the
// listener is wrapped with a PreambleListener that verifies the session token
// forwarded by lmtp-login before passing the connection to the LMTP backend.
func (s *Server) Serve(ln net.Listener) error {
	slog.Info("lmtp: listening", "addr", ln.Addr().String(),
		"preamble", s.opts.AuthAddr != "",
		"proxy_mode", s.opts.Router != nil,
	)
	if s.opts.AuthAddr != "" {
		ln = &loginproto.PreambleListener{
			Listener:        ln,
			AuthAddr:        s.opts.AuthAddr,
			AuthTLS:         s.opts.AuthTLS,
			ExpectedService: "lmtp",
			TLSConfig:       s.opts.PreambleTLS,
		}
	}
	if wa := parseWorkarounds(s.opts.Config.ClientWorkarounds); wa != 0 {
		ln = &lmtpWorkaroundListener{Listener: ln, workarounds: wa}
	}
	return s.srv.Serve(ln)
}

// ---- backend ----------------------------------------------------------------

type backend struct {
	opts   Options
	router *proxyRouter
	srv    *Server
}

func (b *backend) NewSession(c *goSmtp.Conn) (goSmtp.Session, error) {
	peerIP := ""
	var mtaConn net.Conn
	if c != nil {
		mtaConn = c.Conn()
		if mtaConn != nil {
			if host, _, err := net.SplitHostPort(mtaConn.RemoteAddr().String()); err == nil {
				peerIP = host
			} else {
				peerIP = mtaConn.RemoteAddr().String()
			}
		}
	}
	return &session{opts: b.opts, router: b.router, srv: b.srv, peerIP: peerIP, mtaConn: mtaConn, connID: nextConnID()}, nil
}

// connIDSeq is a per-process monotonic counter identifying one LMTP
// connection (one upstream MTA session, potentially many MAIL/RCPT/DATA
// cycles) — distinct from deliverOne's per-delivery call_id, so a
// connection's full command sequence greps as one thread.
var connIDSeq atomic.Uint64

func nextConnID() uint64 { return connIDSeq.Add(1) }

// ---- session ----------------------------------------------------------------

type session struct {
	opts       Options
	router     *proxyRouter
	srv        *Server  // back-reference
	peerIP     string   // upstream MTA IP, captured at NewSession
	mtaConn    net.Conn // raw TCP conn from the upstream MTA
	from       string
	rcpts      []string            // local recipients
	proxyRcpts map[string][]string // backend addr → []rcpt (proxy mode)

	// rcptUserInfo caches per-recipient UserInfo fetched at RCPT TO time
	// so LMTPData can use correct Home and QuotaRules without re-querying.
	rcptUserInfo map[string]*mailbox.UserInfo

	// connID identifies this LMTP connection (see nextConnID).
	connID uint64
}

// folderMessageCount returns folder's current message count from the index
// (the authoritative count backend). ok is false when the folder is unavailable.
func folderMessageCount(idx quota.FolderVSizer, folder string) (int64, bool) {
	f, err := idx.OpenFolder(folder, 0)
	if err != nil {
		return 0, false
	}
	_, msgs, err := idx.FolderVSize(f.ID)
	if err != nil {
		return 0, false
	}
	return int64(msgs), true
}

// quotaExceededMessage is the 452 text for an over-quota delivery.
func (s *session) quotaExceededMessage() string {
	if m := s.opts.QuotaExceededMessage; m != "" {
		return m
	}
	return "Mailbox full"
}

func (s *session) Mail(from string, _ *goSmtp.MailOptions) error {
	slog.Debug("lmtp: command", "conn_id", s.connID, "cmd", "MAIL", "from", from)
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *goSmtp.RcptOptions) error {
	slog.Debug("lmtp: command", "conn_id", s.connID, "cmd", "RCPT", "to", to)
	if s.router != nil {
		return s.rcptProxy(to)
	}
	return s.rcptLocal(to)
}

func (s *session) rcptLocal(to string) error {
	user, _, err := resolveMailbox(to)
	if err != nil {
		return &goSmtp.SMTPError{Code: 501, EnhancedCode: goSmtp.EnhancedCode{5, 1, 3}, Message: "Bad recipient address"}
	}
	resolver := s.opts.Resolver
	if resolver == nil {
		resolver = &mailbox.Resolver{}
	}

	// Userdb lookup: verify recipient exists and get per-user storage config.
	// Done before opening the mailbox so Home and QuotaRules are correct.
	var userInfo *mailbox.UserInfo
	if s.opts.UserdbLookup != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ui, cherr := s.opts.UserdbLookup(ctx, user)
		cancel()
		if cherr != nil {
			slog.Error("lmtp: userdb lookup failed", "user", user, "err", cherr)
			return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Temporary user lookup error"}
		}
		if ui == nil {
			return &goSmtp.SMTPError{Code: 550, EnhancedCode: goSmtp.EnhancedCode{5, 1, 1}, Message: "No such user here"}
		}
		userInfo = ui
	} else {
		userInfo = resolver.UserInfo(user, "")
	}
	if s.rcptUserInfo == nil {
		s.rcptUserInfo = make(map[string]*mailbox.UserInfo)
	}
	s.rcptUserInfo[to] = userInfo

	box := s.opts.Mailbox.OpenUser(userInfo)
	defer box.Close() //nolint:errcheck

	// Quota is enforced at delivery time from the index (see LMTPData), the
	// authoritative source — no RCPT-time dict pre-check.

	exists, err := box.FolderExists("INBOX")
	if err != nil {
		slog.Error("lmtp: mailbox check failed", "user", user, "err", err)
		return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Temporary user lookup error"}
	}
	if !exists {
		if err := box.Init(); err != nil {
			slog.Error("lmtp: auto-provision failed", "user", user, "err", err)
			return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Mailbox provisioning failed"}
		}
		slog.Info("lmtp: provisioned mailbox", "user", user)
	}

	// Per-(IP, mailbox) rate limit (cluster-wide via
	// pkg/locks COUNTER-INC). Counter unavailable is non-fatal —
	// log + accept so a locks outage cannot block delivery.
	if rl := s.opts.Config.RateLimit; rl.Enabled && s.opts.Locker != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := checkRecipientRate(ctx, s.opts.Locker, s.peerIP, to,
			rl.PerRecipientBurst, rl.PerRecipientWindowSeconds)
		cancel()
		if errors.Is(err, ErrRateLimited) {
			slog.Warn("lmtp: recipient rate limit exceeded", "ip", s.peerIP, "rcpt", to,
				"burst", rl.PerRecipientBurst, "window_seconds", rl.PerRecipientWindowSeconds)
			return &goSmtp.SMTPError{Code: 421, EnhancedCode: goSmtp.EnhancedCode{4, 7, 0}, Message: "Rate limit exceeded for recipient"}
		}
		if err != nil {
			slog.Warn("lmtp: rate-limit counter unavailable, accepting", "ip", s.peerIP, "rcpt", to, "err", err)
		}
	}

	s.rcpts = append(s.rcpts, to)
	return nil
}

func (s *session) rcptProxy(to string) error {
	user, _, err := resolveMailbox(to)
	if err != nil {
		return &goSmtp.SMTPError{Code: 501, EnhancedCode: goSmtp.EnhancedCode{5, 1, 3}, Message: "Bad recipient address"}
	}
	addr, err := s.router.route(user)
	if err != nil {
		slog.Error("lmtp: proxy route failed", "user", user, "err", err)
		return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Temporary routing error"}
	}
	if s.proxyRcpts == nil {
		s.proxyRcpts = make(map[string][]string)
	}
	s.proxyRcpts[addr] = append(s.proxyRcpts[addr], to)
	return nil
}

// Data is never called in LMTP mode — LMTPData handles DATA instead.
func (s *session) Data(_ io.Reader) error { return nil }

// prependHeaders prepends Received and Delivered-To headers to data.
// rcpt is the original RCPT TO address; finalRcpt is after detail stripping.
func (s *session) prependHeaders(data []byte, rcpt, finalRcpt string) []byte {
	var hdrs []byte
	switch s.opts.Config.HdrDeliveryAddress {
	case "final":
		hdrs = append(hdrs, ("Delivered-To: " + finalRcpt + "\r\n")...)
	case "original":
		hdrs = append(hdrs, ("Delivered-To: " + rcpt + "\r\n")...)
	}
	if s.opts.Config.AddReceivedHeader {
		hdrs = append(hdrs, buildReceivedHeader(s.from)...)
	}
	if len(hdrs) == 0 {
		return data
	}
	return append(hdrs, data...)
}

// matchNamespace returns the configured namespace whose (non-empty) prefix is
// the longest prefix of folder, or nil when the folder belongs to the personal
// store. Longest-prefix match so nested namespaces resolve to the most specific.
func (s *session) matchNamespace(folder string) *config.NamespaceConfig {
	var best *config.NamespaceConfig
	for i := range s.opts.Namespaces {
		n := &s.opts.Namespaces[i]
		if n.Prefix == "" || n.Location == "" {
			continue
		}
		if strings.HasPrefix(folder, n.Prefix) {
			if best == nil || len(n.Prefix) > len(best.Prefix) {
				best = n
			}
		}
	}
	return best
}

// deliveryTarget resolves a delivery folder through the recipient's namespaces.
// A namespace-prefixed folder routes to that namespace's storage with the prefix stripped;
// everything else goes to the recipient's own store. Returns the target
// box/idx, the namespace-relative folder, and a close func for any store this
// call opened (a no-op for the personal store, which the caller owns).
//
// When enforcePost is set, delivery into a shared / public namespace requires
// the recipient to hold the 'p' (post) right on the target folder; a denial
// falls back to the recipient's INBOX (implicit keep). The recipient's own
// personal store is never ACL-checked (IGNORE_ACLS semantics).
// folderByMailboxID resolves a MAILBOXID (RFC 8474 objectid) to the name of the
// personal-namespace folder carrying it, backing fileinto :mailboxid and
// mailboxidexists (RFC 9042). It walks the user's selectable folders and matches
// the requested id against each folder's stable GUID. Returns ("", false) when
// no folder matches or the folder tree cannot be read.
func (s *session) folderByMailboxID(rcptBox mailbox.UserMailbox, rcptIdx mailbox.UserIndex, id string) (string, bool) {
	if id == "" {
		return "", false
	}
	entries, err := rcptBox.ListFolders()
	if err != nil {
		slog.Warn("lmtp: mailboxid lookup: list folders failed", "err", err)
		return "", false
	}
	for _, e := range entries {
		if !e.Selectable {
			continue
		}
		f, err := rcptIdx.OpenFolder(e.Name, 0)
		if err != nil {
			continue
		}
		if mailbox.FormatObjectID(f.GUID) == id {
			return e.Name, true
		}
	}
	return "", false
}

// mailboxMetadata reads a mailbox-scoped IMAP METADATA annotation (RFC 5464) on
// mbox for the recipient, backing the mboxmetadata Sieve tests (RFC 5490 §4). It
// reads the same personal-namespace dict keys the IMAP server writes. Returns
// ("", false, nil) when the dict is unconfigured, the entry name is malformed,
// the folder is unknown, or the annotation is absent.
func (s *session) mailboxMetadata(ctx context.Context, userInfo *mailbox.UserInfo, idx mailbox.UserIndex, mbox, annotation string) (string, bool, error) {
	if s.opts.MetadataDict == nil {
		return "", false, nil
	}
	scope, attrName, err := mailbox.ParseAttrEntry(annotation)
	if err != nil {
		return "", false, nil
	}
	f, err := idx.OpenFolder(mbox, 0)
	if err != nil {
		return "", false, nil
	}
	return s.lookupMetadata(ctx, userInfo, mailbox.AttrKey(scope, f.GUID, attrName))
}

// serverMetadata reads a server-scoped IMAP METADATA annotation, backing the
// servermetadata Sieve tests. Server-scope entries live under INBOX's GUID with
// the vendor prefix, matching the IMAP server's key derivation.
func (s *session) serverMetadata(ctx context.Context, userInfo *mailbox.UserInfo, idx mailbox.UserIndex, annotation string) (string, bool, error) {
	if s.opts.MetadataDict == nil {
		return "", false, nil
	}
	scope, attrName, err := mailbox.ParseAttrEntry(annotation)
	if err != nil {
		return "", false, nil
	}
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		return "", false, nil
	}
	return s.lookupMetadata(ctx, userInfo, mailbox.ServerAttrKey(scope, f.GUID, attrName))
}

func (s *session) lookupMetadata(ctx context.Context, userInfo *mailbox.UserInfo, key string) (string, bool, error) {
	ops := &dict.OpSettings{Username: userInfo.Username, HomeDir: userInfo.Home}
	vals, found, err := s.opts.MetadataDict.Lookup(ctx, ops, key)
	if err != nil {
		return "", false, fmt.Errorf("lmtp/metadata lookup: %w", err)
	}
	if !found || len(vals) == 0 {
		return "", false, nil
	}
	return string(vals[0]), true, nil
}

func (s *session) deliveryTarget(userInfo *mailbox.UserInfo, rcptBox mailbox.UserMailbox, rcptIdx mailbox.UserIndex, folder string, enforcePost bool) (mailbox.UserMailbox, mailbox.UserIndex, string, func()) {
	noop := func() {}
	ns := s.matchNamespace(folder)
	if ns == nil {
		return rcptBox, rcptIdx, folder, noop
	}
	loc, ok, err := mailbox.ParseLocation(ns.Location, nil)
	if err != nil || !ok {
		slog.Warn("lmtp: namespace location parse failed, using personal store",
			"prefix", ns.Prefix, "location", ns.Location, "err", err)
		return rcptBox, rcptIdx, folder, noop
	}
	rel := strings.TrimPrefix(folder, ns.Prefix)
	if rel == "" {
		rel = "INBOX" // delivery to a bare namespace prefix → its INBOX
	}
	ui := &mailbox.UserInfo{
		Username:    userInfo.Username,
		Home:        loc.Path,
		MailPath:    loc.Path,
		Driver:      loc.Driver,
		IndexDir:    loc.IndexDir,
		VolatileDir: loc.VolatileDir,
		ControlDir:  loc.ControlDir,
		AltDir:      loc.AltDir,
		Separator:   ns.Separator,
		Groups:      userInfo.Groups,
		ACLUser:     userInfo.ACLUser,
		ACLGroups:   userInfo.ACLGroups,
	}
	if enforcePost && !s.postAllowed(ui, ns, rel) {
		slog.Warn("lmtp: post right denied, falling back to INBOX",
			"rcpt", userInfo.Username, "prefix", ns.Prefix, "folder", rel)
		return rcptBox, rcptIdx, "INBOX", noop
	}
	mb := s.opts.Mailbox
	if f := s.opts.MailboxByDriver; f != nil && loc.Driver != "" {
		mb = f(loc.Driver)
	}
	box := mb.OpenUser(ui)
	if err := box.Init(); err != nil {
		slog.Warn("lmtp: namespace store init failed, using personal store",
			"prefix", ns.Prefix, "err", err)
		box.Close() //nolint:errcheck
		return rcptBox, rcptIdx, folder, noop
	}
	idx := s.opts.Index.OpenUser(ui)
	return box, idx, rel, func() {
		box.Close() //nolint:errcheck
		idx.Close() //nolint:errcheck
	}
}

// postAllowed reports whether the recipient holds the 'p' (post) right on the
// target folder of a shared / public namespace. It mirrors the IMAP ACL
// resolution (global rules, globals-only, defaults-from-inbox) so both paths
// agree. ACL read errors fail closed (no post) → the caller falls back to the
// recipient's INBOX rather than silently delivering past an unreadable ACL.
func (s *session) postAllowed(ui *mailbox.UserInfo, ns *config.NamespaceConfig, rel string) bool {
	if !s.opts.ACLEnabled {
		return true
	}
	// acl_defaults_from_inbox applies to private / shared namespaces only.
	defaultsFromInbox := s.opts.ACLDefaultsFromInbox && ns.Type != "public"
	lockOwner := fmt.Sprintf("yarilo-lmtp/%d/%s", os.Getpid(), ui.Username)
	store := acl.New(ui.Home, ui.MailPath, ui.Driver, ui.Separator, ui.Username, lockOwner, acl.Policy{
		DefaultsFromInbox: defaultsFromInbox,
		GlobalsOnly:       s.opts.ACLGlobalsOnly,
		Global:            s.opts.ACLGlobal,
		CacheTTL:          s.opts.ACLCacheTTL,
	}, s.opts.Locker)
	var sep byte = '/'
	if ns.Separator != "" {
		sep = ns.Separator[0]
	}
	aclUser, aclGroups := ui.ACLIdentity()
	rights, err := store.EffectiveFor(rel, aclUser, aclGroups, false, sep)
	if err != nil {
		slog.Warn("lmtp: post-right ACL read failed, denying", "folder", rel, "err", err)
		return false
	}
	return rights.Has(mailbox.RightPost)
}

// LMTPData delivers the message and reports per-recipient status via status.SetStatus.
// resolveRcptUserInfo returns the UserInfo for a recipient at delivery time.
// The RCPT-time lookup normally caches it (with UserInfo.Driver stamped via the
// userdb); on a cache miss it re-runs the userdb lookup so Driver is still set,
// because the personal-backend selection hangs entirely on that field — a dbox
// recipient must not fall through to the global (maildir) store. Only when there
// is no userdb to consult does it fall back to the bare resolver (no per-user
// driver, so the global backend is correct).
func (s *session) resolveRcptUserInfo(rcpt, username string) *mailbox.UserInfo {
	if ui := s.rcptUserInfo[rcpt]; ui != nil {
		return ui
	}
	if s.opts.UserdbLookup != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ui, err := s.opts.UserdbLookup(ctx, username)
		cancel()
		if err == nil && ui != nil {
			return ui
		}
	}
	resolver := s.opts.Resolver
	if resolver == nil {
		resolver = &mailbox.Resolver{}
	}
	return resolver.UserInfo(username, "")
}

func (s *session) LMTPData(r io.Reader, status goSmtp.StatusCollector) error {
	slog.Debug("lmtp: command", "conn_id", s.connID, "cmd", "DATA", "from", s.from, "rcpts", len(s.rcpts))
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	// For proxy mode, build message with common headers (no per-rcpt Delivered-To).
	proxyData := data
	if s.opts.Config.AddReceivedHeader {
		proxyData = append([]byte(buildReceivedHeader(s.from)), data...)
	}

	// Proxy recipients: fan-out to backends in parallel.
	if len(s.proxyRcpts) > 0 {
		results := s.router.proxyFanOut(s.proxyRcpts, s.from, proxyData)
		for rcpt, rerr := range results {
			if rerr != nil {
				slog.Error("lmtp: proxy delivery failed", "rcpt", rcpt, "err", rerr)
				if s.opts.Config.VerboseReplies {
					rerr = &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 2, 0}, Message: rerr.Error()}
				} else {
					rerr = &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 2, 0}, Message: "Proxy delivery failed"}
				}
			} else {
				slog.Info("lmtp: proxy delivered", "rcpt", rcpt, "size", len(proxyData))
			}
			status.SetStatus(rcpt, rerr)
		}
	}

	// Local recipients: deliver directly.
	for _, rcpt := range s.rcpts {
		deliverRcpt := rcpt
		if !s.opts.Config.SaveToDetailMailbox {
			deliverRcpt = stripDetail(rcpt)
		}

		msg := s.prependHeaders(data, rcpt, deliverRcpt)

		username, folder, _ := resolveMailbox(deliverRcpt)
		userInfo := s.resolveRcptUserInfo(rcpt, username)

		mboxBackend := mailbox.SelectPersonalBackend(s.opts.Mailbox, s.opts.MailboxByDriver, userInfo.Driver)
		rcptBox := mboxBackend.OpenUser(userInfo)
		rcptIdx := s.opts.Index.OpenUser(userInfo)
		rcptBox.Init() //nolint:errcheck // idempotent; provisioned in rcptLocal

		// Quota enforcement from the index (authoritative): reject when this
		// message would push the recipient over their limit. Sums the per-folder
		// hdr-vsize aggregate — no dict, no drift.
		if s.opts.QuotaEngine {
			// Per-message size cap applies even without a per-user quota_rule and
			// carries a distinct text so a client can tell "too large" from "full".
			if ms := s.opts.QuotaMailSize; ms > 0 && int64(len(msg)) > ms {
				slog.Warn("lmtp: delivery rejected: message exceeds max mail size", "rcpt", rcpt, "user", username, "size", len(msg), "max", ms)
				rcptBox.Close() //nolint:errcheck
				rcptIdx.Close() //nolint:errcheck
				status.SetStatus(rcpt, &goSmtp.SMTPError{
					Code: 552, EnhancedCode: goSmtp.EnhancedCode{5, 2, 3},
					Message: fmt.Sprintf("Requested allocation size %d exceeds max mail size %d", len(msg), ms),
				})
				continue
			}
			// Per-mailbox message-count cap is structural (independent of a
			// quota_rule): reject when the target folder would reach the limit.
			if mmc := s.opts.QuotaPolicy.MailboxMessageCount; mmc > 0 {
				if cur, ok := folderMessageCount(rcptIdx, folder); ok && cur+1 >= mmc {
					slog.Warn("lmtp: delivery rejected: too many messages in mailbox", "rcpt", rcpt, "user", username, "folder", folder)
					rcptBox.Close() //nolint:errcheck
					rcptIdx.Close() //nolint:errcheck
					status.SetStatus(rcpt, &goSmtp.SMTPError{
						Code: 552, EnhancedCode: goSmtp.EnhancedCode{5, 2, 2},
						Message: "Too many messages in the mailbox",
					})
					continue
				}
			}
			if lim := quota.ParseRules(userInfo.QuotaRules); len(userInfo.QuotaRules) > 0 {
				effLim, ignore := lim.EffectiveLimits(folder)
				effLim = s.opts.QuotaPolicy.Scale(effLim)
				if !ignore && !effLim.Unlimited() {
					entries, _ := rcptBox.ListFolders()
					u := quota.CountUsage(rcptIdx, mailbox.SelectableNames(entries), lim)
					// Inbound delivery is grace-eligible (LMTP/LDA overshoot).
					if quota.IsOverWithGrace(u, effLim, int64(len(msg)), 1, s.opts.QuotaPolicy.StorageGrace) {
						slog.Warn("lmtp: delivery rejected: mailbox full", "rcpt", rcpt, "user", username)
						rcptBox.Close() //nolint:errcheck
						rcptIdx.Close() //nolint:errcheck
						status.SetStatus(rcpt, &goSmtp.SMTPError{
							Code: 452, EnhancedCode: goSmtp.EnhancedCode{4, 2, 2},
							Message: s.quotaExceededMessage(),
						})
						continue
					}
					// Delivery accepted — fire any quota_warning crossed by this
					// increment (inbound delivery is an "over" transition).
					// Warnings track the user-wide limit, not the per-folder one.
					if len(s.opts.QuotaPolicy.Warnings) > 0 || s.opts.QuotaClone != nil {
						after := quota.Usage{StorageBytes: u.StorageBytes + int64(len(msg)), Messages: u.Messages + 1}
						if len(s.opts.QuotaPolicy.Warnings) > 0 {
							s.opts.QuotaWarner.Fire(username, userInfo.Home, s.opts.QuotaPolicy.Warnings, s.opts.QuotaPolicy.Scale(lim), u, after)
						}
						if s.opts.QuotaClone != nil {
							cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
							s.opts.QuotaClone.Write(cctx, username, after)
							ccancel()
						}
					}
				}
			}
		}

		deliveries := []sieve.Delivery{{Folder: folder}}
		deliverMsg := msg
		if s.opts.SieveEngine != nil {
			fopts := sieve.FilterOptions{
				Username: username,
				HomeDir:  userInfo.Home,
				EnvFrom:  s.from,
				EnvTo:    rcpt,
				MsgRaw:   msg,
				FolderExists: func(_ context.Context, f string) (bool, error) {
					box, _, rel, closeTarget := s.deliveryTarget(userInfo, rcptBox, rcptIdx, f, false)
					defer closeTarget()
					return box.FolderExists(rel)
				},
				MailboxByID: func(_ context.Context, id string) (string, bool) {
					return s.folderByMailboxID(rcptBox, rcptIdx, id)
				},
				MailboxMetadata: func(ctx context.Context, mbox, annotation string) (string, bool, error) {
					return s.mailboxMetadata(ctx, userInfo, rcptIdx, mbox, annotation)
				},
				ServerMetadata: func(ctx context.Context, annotation string) (string, bool, error) {
					return s.serverMetadata(ctx, userInfo, rcptIdx, annotation)
				},
			}
			result, ferr := s.opts.SieveEngine.Filter(context.Background(), fopts)
			if ferr != nil {
				slog.Error("lmtp: sieve filter error, using implicit keep", "rcpt", rcpt, "err", ferr)
			} else if result == nil {
				if ierr := s.opts.SieveEngine.InitUser(context.Background(), username, userInfo.Home); ierr != nil {
					slog.Warn("lmtp: sieve init user", "user", username, "err", ierr)
				}
			} else if result.Reject != nil {
				rcptBox.Close() //nolint:errcheck
				rcptIdx.Close() //nolint:errcheck
				code, enh := sieveRejectCode(result.Reject)
				slog.Info("lmtp: sieve rejected", "rcpt", rcpt, "reason", result.Reject.Reason)
				status.SetStatus(rcpt, &goSmtp.SMTPError{Code: code, EnhancedCode: enh, Message: result.Reject.Reason})
				continue
			} else if len(result.Deliveries) == 0 {
				rcptBox.Close() //nolint:errcheck
				rcptIdx.Close() //nolint:errcheck
				slog.Info("lmtp: sieve discard", "rcpt", rcpt)
				status.SetStatus(rcpt, nil)
				continue
			} else {
				deliveries = result.Deliveries
			}
			if result != nil && result.Message != nil {
				deliverMsg = result.Message
			}
		}

		var deliverErr error
		for _, d := range deliveries {
			// Route each delivery through the recipient's namespaces so a
			// namespace-prefixed target (e.g. Sieve fileinto "Public/News")
			// lands in that namespace's storage, not the recipient's own store.
			tBox, tIdx, rel, closeTarget := s.deliveryTarget(userInfo, rcptBox, rcptIdx, d.Folder, true)
			if d.Create {
				if err := tBox.Create(rel); err != nil {
					slog.Warn("lmtp: create folder", "folder", d.Folder, "err", err)
				}
			}
			uid, err := deliverOne(tBox, tIdx, rel, bytes.NewReader(deliverMsg), int64(len(deliverMsg)), s.opts.Locker, username, s.from, d.Flags)
			closeTarget()
			if err != nil {
				deliverErr = err
				break
			}
			s.ftsAutoindex(username, rel, uid)
		}
		rcptBox.Close() //nolint:errcheck
		rcptIdx.Close() //nolint:errcheck
		if deliverErr != nil {
			slog.Error("lmtp: delivery failed", "rcpt", rcpt, "err", deliverErr)
			if s.opts.Config.VerboseReplies {
				deliverErr = &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 2, 0}, Message: deliverErr.Error()}
			} else {
				deliverErr = &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 2, 0}, Message: "Local delivery failed"}
			}
		}
		status.SetStatus(rcpt, deliverErr)
	}
	return nil
}

// ftsAutoindex is the delivery-time FTS hook: fire-and-forget.
func (s *session) ftsAutoindex(username, folder string, uid uint32) {
	if s.opts.FTSClient == nil || !s.opts.FTSAutoindex || uid == 0 {
		return
	}
	client, maxRecent := s.opts.FTSClient, s.opts.FTSMaxRecent
	go func() {
		if err := client.Index(username, fts.MailboxRef{Name: folder}, uid, maxRecent); err != nil {
			slog.Debug("lmtp: fts autoindex failed", "user", username, "folder", folder, "uid", uid, "err", err)
			return
		}
		// Breadcrumb (#625): the delivery→index handoff fired, so an indexing gap
		// (delivered but never queued to FTS) is visible from the lmtp log.
		slog.Debug("lmtp: fts autoindex queued", "user", username, "folder", folder, "uid", uid, "max_recent", maxRecent)
	}()
}

func sieveRejectCode(r *sieve.RejectErr) (int, goSmtp.EnhancedCode) {
	return 550, goSmtp.EnhancedCode{5, 7, 1}
}

func (s *session) Reset() {
	slog.Debug("lmtp: command", "conn_id", s.connID, "cmd", "RSET")
	s.from = ""
	s.rcpts = nil
	s.proxyRcpts = nil
	s.rcptUserInfo = nil
}

func (s *session) Logout() error {
	slog.Debug("lmtp: command", "conn_id", s.connID, "cmd", "QUIT")
	return nil
}
