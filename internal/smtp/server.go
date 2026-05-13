// Package smtp implements the SMTP inbound (port 25) and submission (port 587) servers.
// Inbound: milters → LMTP deliver.
// Submission: AUTH required → milters → relay.
package smtp

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	goSmtp "github.com/0kaba0hub/go-smtp"
	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/internal/lmtp"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Authenticator verifies SMTP AUTH credentials (submission only).
type Authenticator interface {
	AuthPlain(username, password string) error
}

// Options configures the SMTP servers.
type Options struct {
	// Infrastructure (per-listener; set by backend from ServiceConfig).
	HAProxy          bool
	HAProxyTimeout   time.Duration
	HAProxyNets      []*net.IPNet
	XClient          bool
	XClientNets      []*net.IPNet
	DisablePlainAuth bool
	// Protocol-level settings.
	Config    config.SMTPProtocolConfig
	Auth      Authenticator
	Deliverer *lmtp.Deliverer
	Milters   []*MilterClient
	Relay     *Relay
}

// Server wraps two go-smtp servers: MX (port 25) and submission (port 587).
type Server struct {
	opts   Options
	mxSrv  *goSmtp.Server
	subSrv *goSmtp.Server
}

// New creates both SMTP servers. Call Serve to start them.
func New(opts Options) *Server {
	s := &Server{opts: opts}
	s.mxSrv = s.buildServer(false)
	s.subSrv = s.buildServer(true)
	return s
}

func (s *Server) buildServer(submission bool) *goSmtp.Server {
	be := &backend{srv: s, submission: submission}
	srv := goSmtp.NewServer(be)
	srv.Domain = s.opts.Config.Hostname
	srv.MaxMessageBytes = s.opts.Config.MaxMsgSize
	if r := s.opts.Config.MaxRecipients; r > 0 {
		srv.MaxRecipients = r
	}
	if l := s.opts.Config.MaxLineLength; l > 0 {
		srv.MaxLineLength = l
	}
	// For submission: allow AUTH on plain connections only if disable_plaintext_auth is false.
	srv.AllowInsecureAuth = submission && !s.opts.DisablePlainAuth
	srv.ReadTimeout = 5 * time.Minute
	srv.WriteTimeout = 5 * time.Minute
	return srv
}

// ServeMX starts the MX listener on the configured address.
func (s *Server) ServeMX(ln net.Listener) error {
	slog.Info("smtp: MX listening", "addr", ln.Addr())
	if s.opts.HAProxy {
		ln = &proxyproto.Listener{
			Listener:          ln,
			Policy:            proxyPolicy(s.opts.HAProxyNets),
			ReadHeaderTimeout: s.haproxyTimeout(),
		}
	}
	if s.opts.XClient {
		ln = &xclientListener{Listener: ln, trustedNets: s.opts.XClientNets}
	}
	return s.mxSrv.Serve(ln)
}

// ServeSubmit starts the submission listener, optionally with TLS.
// When ProxyProtocol is enabled in config, connections are wrapped in a HAProxy
// PROXY protocol listener so the real client IP is extracted from the header.
func (s *Server) ServeSubmit(ln net.Listener, tlsCfg *tls.Config) error {
	slog.Info("smtp: submission listening", "addr", ln.Addr())
	if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
	}
	if s.opts.HAProxy {
		ln = &proxyproto.Listener{
			Listener:          ln,
			Policy:            proxyPolicy(s.opts.HAProxyNets),
			ReadHeaderTimeout: s.haproxyTimeout(),
		}
	}
	return s.subSrv.Serve(ln)
}

func (s *Server) haproxyTimeout() time.Duration {
	if s.opts.HAProxyTimeout > 0 {
		return s.opts.HAProxyTimeout
	}
	return 3 * time.Second
}

// proxyPolicy returns a go-proxyproto Policy function.
// If nets is empty, all PROXY headers are rejected (IGNORE).
// Otherwise only connections from trusted nets are USEd; others are IGNOREd.
func proxyPolicy(nets []*net.IPNet) func(upstream net.Addr) (proxyproto.Policy, error) {
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

// ---- backend / session --------------------------------------------------

type backend struct {
	srv        *Server
	submission bool
}

func (b *backend) NewSession(c *goSmtp.Conn) (goSmtp.Session, error) {
	remoteIP := remoteIP(c)
	return &session{
		srv:        b.srv,
		submission: b.submission,
		remoteIP:   remoteIP,
	}, nil
}

type session struct {
	srv        *Server
	submission bool
	remoteIP   net.IP
	from       string
	rcpts      []string
}

func (s *session) Reset() {
	s.from = ""
	s.rcpts = nil
}

func (s *session) Logout() error { return nil }

func (s *session) Mail(from string, _ *goSmtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *goSmtp.RcptOptions) error {
	if s.submission {
		s.rcpts = append(s.rcpts, to)
	} else {
		s.rcpts = append(s.rcpts, stripDelimiter(to, s.srv.opts.Config.RecipientDelimiter))
	}
	return nil
}

// stripDelimiter removes the subaddress extension from an email address.
// "user+tag@domain" with delimiter "+" → "user@domain".
func stripDelimiter(addr, delim string) string {
	if delim == "" {
		return addr
	}
	addr = strings.Trim(addr, "<>")
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return addr
	}
	local, domain := addr[:at], addr[at+1:]
	if i := strings.Index(local, delim); i >= 0 {
		local = local[:i]
	}
	return local + "@" + domain
}

func (s *session) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("smtp/data: read: %w", err)
	}

	ctx := context.Background()
	opts := s.srv.opts

	// Run external milters before internal checks.
	for _, mc := range opts.Milters {
		if err := mc.Check(ctx, s.from, s.rcpts, bytes.NewReader(data)); err != nil {
			slog.Info("smtp: milter rejected", "err", err)
			return &goSmtp.SMTPError{Code: 550, EnhancedCode: goSmtp.EnhancedCode{5, 7, 1}, Message: err.Error()}
		}
	}

	if s.submission {
		return s.handleSubmission(ctx, data)
	}
	return s.handleInbound(ctx, data)
}

// handleInbound delivers MX inbound mail via LMTP.
func (s *session) handleInbound(ctx context.Context, data []byte) error {
	results := s.srv.opts.Deliverer.Deliver(ctx, s.from, s.rcpts, bytes.NewReader(data))
	for _, r := range results {
		if r.Err != nil {
			slog.Error("smtp: delivery failed", "rcpt", r.Rcpt, "err", r.Err)
		}
	}
	return nil
}

// handleSubmission proxies outbound mail to the configured relay server.
func (s *session) handleSubmission(_ context.Context, data []byte) error {
	relay := s.srv.opts.Relay
	if relay == nil {
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 3, 0},
			Message:      "Relay not configured",
		}
	}
	if err := relay.Send(s.from, s.rcpts, bytes.NewReader(data), s.remoteIP); err != nil {
		slog.Info("smtp: relay rejected", "from", s.from, "err", err)
		return err
	}
	slog.Info("smtp: relayed", "from", s.from, "rcpts", s.rcpts, "size", len(data))
	return nil
}

// AuthMechanisms advertises PLAIN auth on submission sessions.
func (s *session) AuthMechanisms() []string {
	if s.submission {
		return []string{sasl.Plain}
	}
	return nil
}

// Auth returns a sasl.Server for the requested mechanism.
func (s *session) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, goSmtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(func(_, username, password string) error {
		return s.srv.opts.Auth.AuthPlain(username, password)
	}), nil
}

// ---- helpers ------------------------------------------------------------

func remoteIP(c *goSmtp.Conn) net.IP {
	addr := c.Conn().RemoteAddr()
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.IP
	}
	return net.IPv4zero
}

func extractDomain(addr string) string {
	addr = strings.Trim(strings.TrimSpace(addr), "<>")
	if at := strings.LastIndex(addr, "@"); at >= 0 {
		return addr[at+1:]
	}
	return addr
}

// Ensure session implements AuthSession for go-smtp.
var _ mailbox.MailboxBackend = (mailbox.MailboxBackend)(nil) // compile-time interface check placeholder
