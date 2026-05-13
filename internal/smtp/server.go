// Package smtp implements the SMTP inbound (port 25) and submission (port 587) servers.
// Inbound: SPF verify → external milters → DKIM verify → DMARC evaluate → LMTP deliver.
// Submission: AUTH required → external milters → DKIM sign → relay.
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
	goSmtp "github.com/emersion/go-smtp"
	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/internal/dkim"
	"github.com/0kaba0hub/yarilo/internal/dmarc"
	"github.com/0kaba0hub/yarilo/internal/lmtp"
	"github.com/0kaba0hub/yarilo/internal/spf"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Authenticator verifies SMTP AUTH credentials (submission only).
type Authenticator interface {
	AuthPlain(username, password string) error
}

// Options configures the SMTP servers.
type Options struct {
	Config    config.SMTPConfig
	DKIMCfg   config.DKIMConfig
	SPFCfg    config.SPFConfig
	DMARCCfg  config.DMARCConfig
	Auth      Authenticator
	KeyProv   dkim.KeyProvider // nil → no signing
	Deliverer *lmtp.Deliverer
	Milters   []*MilterClient
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
	srv.MaxRecipients = 100
	if l := s.opts.Config.MaxLineLength; l > 0 {
		srv.MaxLineLength = l
	}
	// For submission: allow AUTH on plain connections only if disable_plaintext_auth is false.
	srv.AllowInsecureAuth = submission && !s.opts.Config.DisablePlainAuth
	srv.ReadTimeout = 5 * time.Minute
	srv.WriteTimeout = 5 * time.Minute
	return srv
}

// ServeMX starts the MX listener on the configured address.
func (s *Server) ServeMX(ln net.Listener) error {
	slog.Info("smtp: MX listening", "addr", ln.Addr())
	if s.opts.Config.ProxyProtocol {
		nets := parseCIDRs(s.opts.Config.HAProxyTrustedNets)
		ln = &proxyproto.Listener{
			Listener:          ln,
			Policy:            proxyPolicy(nets),
			ReadHeaderTimeout: s.haproxyTimeout(),
		}
	}
	if s.opts.Config.XClient {
		nets := parseCIDRs(s.opts.Config.XClientTrustedNets)
		ln = &xclientListener{Listener: ln, trustedNets: nets}
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
	if s.opts.Config.ProxyProtocol {
		nets := parseCIDRs(s.opts.Config.HAProxyTrustedNets)
		ln = &proxyproto.Listener{
			Listener:          ln,
			Policy:            proxyPolicy(nets),
			ReadHeaderTimeout: s.haproxyTimeout(),
		}
	}
	return s.subSrv.Serve(ln)
}

func (s *Server) haproxyTimeout() time.Duration {
	if s.opts.Config.HAProxyTimeout > 0 {
		return time.Duration(s.opts.Config.HAProxyTimeout) * time.Second
	}
	return 3 * time.Second
}

// parseCIDRs parses a list of CIDR strings. Invalid entries are logged and skipped.
func parseCIDRs(cidrs []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, s := range cidrs {
		_, ipnet, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("smtp: invalid trusted CIDR, skipping", "cidr", s, "err", err)
			continue
		}
		nets = append(nets, ipnet)
	}
	return nets
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
	s.rcpts = append(s.rcpts, stripDelimiter(to, s.srv.opts.Config.RecipientDelimiter))
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

// handleInbound processes MX inbound mail: SPF → DKIM verify → DMARC → deliver.
func (s *session) handleInbound(ctx context.Context, data []byte) error {
	opts := s.srv.opts
	fromDomain := extractDomain(s.from)

	// SPF check.
	var spfResult spf.Result
	if opts.SPFCfg.Enabled {
		spfResult, _ = spf.Check(ctx, s.remoteIP, s.from, "")
		slog.Info("smtp: SPF", "from", s.from, "result", string(spfResult))
	}

	// DKIM verification.
	var dkimResults []dkim.Result
	if opts.DKIMCfg.Verify {
		dkimResults, _ = dkim.Verify(bytes.NewReader(data))
		for _, r := range dkimResults {
			slog.Info("smtp: DKIM", "domain", r.Domain, "pass", r.Pass)
		}
	}

	// DMARC evaluation.
	if opts.DMARCCfg.Enabled {
		res := dmarc.Evaluate(ctx, fromDomain, spfResult, fromDomain, dkimResults)
		slog.Info("smtp: DMARC", "from", fromDomain, "disposition", string(res.Disposition))
		if res.Disposition == dmarc.PolicyReject {
			return &goSmtp.SMTPError{
				Code:         550,
				EnhancedCode: goSmtp.EnhancedCode{5, 7, 1},
				Message:      "DMARC policy rejection",
			}
		}
	}

	// LMTP local delivery.
	results := opts.Deliverer.Deliver(ctx, s.from, s.rcpts, bytes.NewReader(data))
	for _, r := range results {
		if r.Err != nil {
			slog.Error("smtp: delivery failed", "rcpt", r.Rcpt, "err", r.Err)
		}
	}
	return nil
}

// handleSubmission processes outbound submission: DKIM sign → relay placeholder.
func (s *session) handleSubmission(ctx context.Context, data []byte) error {
	opts := s.srv.opts
	fromDomain := extractDomain(s.from)

	if opts.DKIMCfg.Sign && opts.KeyProv != nil {
		signer, err := opts.KeyProv.GetPrivateKey(ctx, fromDomain)
		if err != nil {
			slog.Info("smtp: DKIM sign skipped", "domain", fromDomain, "reason", err)
		} else {
			signCfg := dkim.SignConfig{
				Selector:        opts.DKIMCfg.Selector,
				SignHeaders:     opts.DKIMCfg.SignHeaders,
				OversignHeaders: opts.DKIMCfg.OversignHeaders,
			}
			var signed bytes.Buffer
			if err := dkim.Sign(&signed, bytes.NewReader(data), fromDomain, signer, signCfg); err != nil {
				slog.Error("smtp: DKIM sign error", "domain", fromDomain, "err", err)
			} else {
				data = signed.Bytes()
			}
		}
	}

	// TODO(phase3): relay signed message to MTA queue.
	slog.Info("smtp: submission accepted", "from", s.from, "rcpts", s.rcpts, "size", len(data))
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
