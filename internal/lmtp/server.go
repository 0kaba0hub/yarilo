// Package lmtp implements an LMTP server (RFC 2033) for local mail delivery.
// External MTAs (e.g. Postfix) connect on port 24 or a Unix socket and use
// LHLO + per-recipient DATA responses to deliver mail to local mailboxes.
package lmtp

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"time"

	goSmtp "github.com/0kaba0hub/go-smtp"
	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Options configures the LMTP server.
type Options struct {
	Hostname string
	Config   config.LMTPProtocolConfig
	Mailbox  mailbox.MailboxBackend
	Index    mailbox.IndexBackend
	Resolver *mailbox.Resolver

	// HAProxy PROXY protocol support.
	ProxyProtocol      bool
	HAProxyTimeout     time.Duration
	HAProxyTrustedNets []*net.IPNet

	// XCLIENT extension support (Postfix-compatible).
	XClient            bool
	XClientTrustedNets []*net.IPNet

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

	// AnvilAddr is the yarilo-anvil server address. When non-empty the
	// LMTP backend issues LOOKUP + CONNECT per RCPT TO to enforce
	// lmtp_user_concurrency_limit cluster-wide. Empty disables anvil
	// integration (single-pod dev / unit-test path); the per-recipient
	// concurrency check then falls back to "no limit, just deliver".
	AnvilAddr string
	// AnvilTLS optionally wraps the anvil dialer with mTLS.
	AnvilTLS *tls.Config
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
	be := &backend{opts: opts, router: router}

	srv := goSmtp.NewServer(be)
	srv.Domain = opts.Hostname
	srv.LMTP = true
	srv.TLSConfig = opts.TLSConfig
	srv.ReadTimeout = time.Duration(opts.Config.ReadTimeout) * time.Second
	srv.WriteTimeout = time.Duration(opts.Config.WriteTimeout) * time.Second

	s.srv = srv
	return s
}

// Serve starts accepting LMTP connections on ln, optionally wrapping it with
// HAProxy PROXY protocol and XCLIENT support.
func (s *Server) Serve(ln net.Listener) error {
	slog.Info("lmtp: listening", "addr", ln.Addr().String(),
		"haproxy", s.opts.ProxyProtocol,
		"xclient", s.opts.XClient,
		"proxy_mode", s.opts.Router != nil,
	)
	if s.opts.ProxyProtocol {
		timeout := s.opts.HAProxyTimeout
		if timeout == 0 {
			timeout = 3 * time.Second
		}
		ln = &proxyproto.Listener{
			Listener:          ln,
			Policy:            proxyPolicy(s.opts.HAProxyTrustedNets),
			ReadHeaderTimeout: timeout,
		}
	}
	if s.opts.XClient {
		ln = &xclientListener{Listener: ln, trustedNets: s.opts.XClientTrustedNets}
	}
	if wa := parseWorkarounds(s.opts.Config.ClientWorkarounds); wa != 0 {
		ln = &lmtpWorkaroundListener{Listener: ln, workarounds: wa}
	}
	return s.srv.Serve(ln)
}

// proxyPolicy returns a go-proxyproto Policy func.
// Empty nets → IGNORE (reject all PROXY headers).
// Trusted CIDR nets → USE; others IGNORE.
func proxyPolicy(nets []*net.IPNet) func(net.Addr) (proxyproto.Policy, error) {
	return func(upstream net.Addr) (proxyproto.Policy, error) {
		if len(nets) == 0 {
			return proxyproto.IGNORE, nil
		}
		tcpAddr, ok := upstream.(*net.TCPAddr)
		if !ok {
			return proxyproto.IGNORE, nil
		}
		for _, n := range nets {
			if n.Contains(tcpAddr.IP) {
				return proxyproto.USE, nil
			}
		}
		return proxyproto.IGNORE, nil
	}
}

// ---- backend ----------------------------------------------------------------

type backend struct {
	opts   Options
	router *proxyRouter
}

func (b *backend) NewSession(c *goSmtp.Conn) (goSmtp.Session, error) {
	peerIP := ""
	if c != nil {
		if addr := c.Conn().RemoteAddr(); addr != nil {
			if host, _, err := net.SplitHostPort(addr.String()); err == nil {
				peerIP = host
			} else {
				peerIP = addr.String()
			}
		}
	}
	return &session{opts: b.opts, router: b.router, peerIP: peerIP}, nil
}

// ---- session ----------------------------------------------------------------

type session struct {
	opts       Options
	router     *proxyRouter
	peerIP     string // upstream MTA IP, captured at NewSession
	from       string
	rcpts      []string            // local recipients
	proxyRcpts map[string][]string // backend addr → []rcpt (proxy mode)

	// anvilConn is the active connection to yarilo-anvil for
	// this LMTP session. Opened lazily on the first local RCPT,
	// closed in Logout. Nil when AnvilAddr is unset (single-pod
	// dev mode / unit tests).
	anvilConn *anvilSessionClient
	// rcptAnvilIDs maps RCPT TO address → anvil session id so
	// LMTPData / Reset / Logout can DISCONNECT the matching
	// entry once delivery completes (success or failure).
	rcptAnvilIDs map[string]string
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
	userInfo := resolver.UserInfo(user, "")
	box := s.opts.Mailbox.OpenUser(userInfo)
	defer box.Close() //nolint:errcheck

	exists, err := box.FolderExists("INBOX")
	if err != nil {
		slog.Error("lmtp: user lookup failed", "user", user, "err", err)
		return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Temporary user lookup error"}
	}
	if !exists {
		// Auto-provision: LMTP is internal — the upstream MTA already vetted
		// the recipient. Create INBOX on first delivery (Dovecot LMTP parity).
		if err := box.Init(); err != nil {
			slog.Error("lmtp: auto-provision failed", "user", user, "err", err)
			return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Mailbox provisioning failed"}
		}
		slog.Info("lmtp: provisioned mailbox", "user", user)
	}

	// Cluster-wide concurrency check (Dovecot lmtp-local.c:282).
	// Lazy-init the anvil session client on first RCPT. Anvil
	// unreachable is non-fatal — log + deliver without the limit,
	// matching Dovecot's tolerance when the anvil socket is gone.
	if s.opts.AnvilAddr != "" {
		if s.anvilConn == nil {
			s.anvilConn = newAnvilSessionClient(s.opts.AnvilAddr, s.opts.AnvilTLS,
				s.opts.Config.UserConcurrencyLimit, s.peerIP)
		}
		id, err := s.anvilConn.reserveDelivery(user)
		if errors.Is(err, ErrTooManyConcurrent) {
			return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Too many concurrent deliveries for user"}
		}
		if err != nil {
			slog.Warn("lmtp: anvil unavailable, accepting without cluster limit", "user", user, "err", err)
		} else {
			if s.rcptAnvilIDs == nil {
				s.rcptAnvilIDs = make(map[string]string)
			}
			s.rcptAnvilIDs[to] = id
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

	// Local recipients: deliver directly. The cluster-wide
	// concurrency check (lmtp_user_concurrency_limit) already
	// fired at RCPT TO via anvil reserveDelivery; here we just
	// deliver and DISCONNECT the matching anvil entry on the way
	// out (success or failure).
	for _, rcpt := range s.rcpts {
		deliverRcpt := rcpt
		if !s.opts.Config.SaveToDetailMailbox {
			deliverRcpt = stripDetail(rcpt)
		}

		msg := s.prependHeaders(data, rcpt, deliverRcpt)

		username, folder, _ := resolveMailbox(deliverRcpt)
		resolver := s.opts.Resolver
		if resolver == nil {
			resolver = &mailbox.Resolver{}
		}
		userInfo := resolver.UserInfo(username, "")
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
		// Release the anvil reservation regardless of outcome.
		// Any leftover entries get swept by releaseAll in Logout.
		if id, ok := s.rcptAnvilIDs[rcpt]; ok {
			s.anvilConn.releaseDelivery(id)
			delete(s.rcptAnvilIDs, rcpt)
		}
		status.SetStatus(rcpt, err)
	}
	return nil
}

func (s *session) Reset() {
	// Release every still-outstanding anvil reservation. The
	// SMTP standard lets a client RSET mid-transaction; without
	// this we'd leak slots until session Logout fires.
	s.anvilConn.releaseAll()
	s.rcptAnvilIDs = nil
	s.from = ""
	s.rcpts = nil
	s.proxyRcpts = nil
}

func (s *session) Logout() error {
	// Final cleanup — DATA never arrived (or arrived and Reset
	// already cleared the map; releaseAll is idempotent).
	s.anvilConn.releaseAll()
	s.rcptAnvilIDs = nil
	return nil
}
