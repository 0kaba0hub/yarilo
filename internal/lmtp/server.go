// Package lmtp implements an LMTP server (RFC 2033) for local mail delivery.
// External MTAs (e.g. Postfix) connect on port 24 or a Unix socket and use
// LHLO + per-recipient DATA responses to deliver mail to local mailboxes.
package lmtp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"time"

	goSmtp "github.com/0kaba0hub/go-smtp"

	"github.com/0kaba0hub/yarilo/internal/loginproto"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
	"github.com/0kaba0hub/yarilo/pkg/quota"
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

	// Locker is the cross-process write coordinator. When non-nil, after a
	// successful delivery the server emits a `delivered` EVENT on the
	// mailbox key so subscribed IMAP IDLE sessions on other pods receive
	// the notification immediately. Nil disables cross-pod notifications.
	Locker locks.Locker

	// UserdbLookup fetches per-user storage config from the userdb before
	// accepting RCPT TO. Returning (nil, nil) means user not found → 550.
	// Returning (nil, err) means transient failure → 451.
	// When nil (unit tests / director-only nodes) the check is skipped.
	UserdbLookup func(ctx context.Context, username string) (*mailbox.UserInfo, error)

	// QuotaDict is the dict backend for per-user quota counters. When
	// non-nil, delivery is rejected with 452 4.2.2 "Mailbox full" if the
	// recipient's quota_rules limit would be exceeded. Nil disables the
	// check (quota enforcement still runs at the index layer, but LMTP
	// cannot return a pre-flight 452 without this).
	QuotaDict dict.Dict

	// AuthAddr is the yarilo-auth client-protocol address used by the
	// PreambleListener to verify session tokens forwarded by lmtp-login.
	// When empty, preamble verification is skipped (plain TCP; use only
	// in unit tests or when lmtp-login is not in the path).
	AuthAddr string
	// AuthTLS optionally wraps the auth-client dialer with mTLS.
	AuthTLS *tls.Config
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
	return &session{opts: b.opts, router: b.router, srv: b.srv, peerIP: peerIP, mtaConn: mtaConn}, nil
}

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
}

func (s *session) Mail(from string, _ *goSmtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *goSmtp.RcptOptions) error {
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

	// RCPT-time quota pre-check: reject immediately if already over quota.
	if s.opts.QuotaDict != nil && len(userInfo.QuotaRules) > 0 {
		lim := quota.ParseRules(userInfo.QuotaRules)
		effLim, ignore := lim.EffectiveLimits("INBOX")
		if !ignore && (effLim.StorageBytes > 0 || effLim.Messages > 0) {
			ctr := quota.NewCounter(s.opts.QuotaDict, user)
			ctxQ, cancelQ := context.WithTimeout(context.Background(), 2*time.Second)
			u, cerr := ctr.Get(ctxQ)
			cancelQ()
			if cerr == nil && quota.IsOver(u, effLim, 0, 0) {
				slog.Warn("lmtp: rcpt rejected: mailbox full", "user", user)
				return &goSmtp.SMTPError{Code: 452, EnhancedCode: goSmtp.EnhancedCode{4, 2, 2}, Message: "Mailbox full"}
			}
		}
	}

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

// LMTPData delivers the message and reports per-recipient status via status.SetStatus.
func (s *session) LMTPData(r io.Reader, status goSmtp.StatusCollector) error {
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
		userInfo := s.rcptUserInfo[rcpt]
		if userInfo == nil {
			resolver := s.opts.Resolver
			if resolver == nil {
				resolver = &mailbox.Resolver{}
			}
			userInfo = resolver.UserInfo(username, "")
		}

		if s.opts.QuotaDict != nil {
			lim := quota.ParseRules(userInfo.QuotaRules)
			effLim, ignore := lim.EffectiveLimits(folder)
			if !ignore && (effLim.StorageBytes > 0 || effLim.Messages > 0) {
				ctr := quota.NewCounter(s.opts.QuotaDict, username)
				if u, cerr := ctr.Get(context.Background()); cerr == nil && quota.IsOver(u, effLim, int64(len(msg)), 1) {
					slog.Warn("lmtp: delivery rejected: mailbox full", "rcpt", rcpt, "user", username)
					status.SetStatus(rcpt, &goSmtp.SMTPError{
						Code: 452, EnhancedCode: goSmtp.EnhancedCode{4, 2, 2},
						Message: "Mailbox full",
					})
					continue
				}
			}
		}

		rcptBox := s.opts.Mailbox.OpenUser(userInfo)
		rcptIdx := s.opts.Index.OpenUser(userInfo)
		rcptBox.Init() //nolint:errcheck // idempotent; provisioned in rcptLocal
		err := deliverOne(rcptBox, rcptIdx, folder, bytes.NewReader(msg), int64(len(msg)), s.opts.Locker, username)
		rcptBox.Close() //nolint:errcheck
		rcptIdx.Close() //nolint:errcheck
		if err != nil {
			slog.Error("lmtp: delivery failed", "rcpt", rcpt, "err", err)
			if s.opts.Config.VerboseReplies {
				err = &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 2, 0}, Message: err.Error()}
			} else {
				err = &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 2, 0}, Message: "Local delivery failed"}
			}
		} else {
			slog.Info("lmtp: delivered", "rcpt", rcpt, "size", len(msg))
		}
		status.SetStatus(rcpt, err)
	}
	return nil
}

func (s *session) Reset() {
	s.from = ""
	s.rcpts = nil
	s.proxyRcpts = nil
	s.rcptUserInfo = nil
}

func (s *session) Logout() error {
	return nil
}
