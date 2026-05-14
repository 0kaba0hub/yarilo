// Package lmtp implements an LMTP server (RFC 2033) for local mail delivery.
// External MTAs (e.g. Postfix) connect on port 24 or a Unix socket and use
// LHLO + per-recipient DATA responses to deliver mail to local mailboxes.
package lmtp

import (
	"bytes"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"time"

	goSmtp "github.com/0kaba0hub/go-smtp"
	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Options configures the LMTP server.
type Options struct {
	Hostname string
	Config   config.LMTPProtocolConfig
	Mailbox  mailbox.MailboxBackend
	Index    mailbox.IndexBackend

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

	// Ring is the director's consistent-hashing ring. Non-nil on director nodes
	// activates proxy mode; nil on backend nodes means local delivery.
	Ring *ring.Ring
}

// Server is an LMTP server backed by a MailboxBackend and IndexBackend.
type Server struct {
	srv     *goSmtp.Server
	opts    Options
	router  *proxyRouter   // non-nil when proxy mode is active
	userSem *userSemaphore // non-nil when UserConcurrencyLimit > 0
}

// New creates an LMTP server from Options.
func New(opts Options) *Server {
	var router *proxyRouter
	if opts.Ring != nil {
		timeout := time.Duration(opts.Config.Proxy.Timeout) * time.Second
		if timeout == 0 {
			timeout = 125 * time.Second
		}
		router = newProxyRouter(opts.Hostname, opts.Ring, timeout)
	}

	var userSem *userSemaphore
	if opts.Config.UserConcurrencyLimit > 0 {
		userSem = newUserSemaphore(opts.Config.UserConcurrencyLimit)
	}

	s := &Server{opts: opts, router: router, userSem: userSem}
	be := &backend{opts: opts, router: router, userSem: userSem}

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
	slog.Info("lmtp: listening", "addr", ln.Addr(),
		"haproxy", s.opts.ProxyProtocol,
		"xclient", s.opts.XClient,
		"proxy_mode", s.opts.Ring != nil,
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
	opts    Options
	router  *proxyRouter
	userSem *userSemaphore
}

func (b *backend) NewSession(_ *goSmtp.Conn) (goSmtp.Session, error) {
	return &session{opts: b.opts, router: b.router, userSem: b.userSem}, nil
}

// ---- session ----------------------------------------------------------------

type session struct {
	opts       Options
	router     *proxyRouter
	userSem    *userSemaphore
	from       string
	rcpts      []string            // local recipients
	proxyRcpts map[string][]string // backend addr → []rcpt (proxy mode)
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
	exists, err := s.opts.Mailbox.FolderExists(user, "INBOX")
	if err != nil {
		slog.Error("lmtp: user lookup failed", "user", user, "err", err)
		return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Temporary user lookup error"}
	}
	if !exists {
		// Auto-provision: LMTP is internal — the upstream MTA already vetted
		// the recipient. Create INBOX on first delivery (Dovecot LMTP parity).
		if err := s.opts.Mailbox.Init(user); err != nil {
			slog.Error("lmtp: auto-provision failed", "user", user, "err", err)
			return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Mailbox provisioning failed"}
		}
		slog.Info("lmtp: provisioned mailbox", "user", user)
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

		if s.userSem != nil {
			user, _, _ := resolveMailbox(deliverRcpt)
			if !s.userSem.acquire(user) {
				slog.Warn("lmtp: user concurrency limit reached", "user", user)
				status.SetStatus(rcpt, &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 4, 5}, Message: "Too many concurrent deliveries"})
				continue
			}
			defer s.userSem.release(user) //nolint:gocritic
		}

		err := deliverOne(s.opts.Mailbox, s.opts.Index, deliverRcpt, bytes.NewReader(msg), int64(len(msg)))
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
}

func (s *session) Logout() error { return nil }
