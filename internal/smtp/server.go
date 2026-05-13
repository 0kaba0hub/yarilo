// Package smtp implements the submission servers (port 587 / 465).
// AUTH PLAIN is required. Messages are forwarded to the configured upstream MTA
// via protocol.smtp.relay. No MX inbound — external MTAs deliver to LMTP (port 24).
package smtp

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	goSmtp "github.com/0kaba0hub/go-smtp"
	"github.com/emersion/go-sasl"
	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/internal/smtp/proxy"
	"github.com/0kaba0hub/yarilo/pkg/config"
)

// Authenticator verifies SMTP AUTH credentials.
type Authenticator interface {
	AuthPlain(username, password string) error
}

// Options configures the submission server.
type Options struct {
	// Infrastructure (per-listener; set by backend from ServiceConfig).
	HAProxy          bool
	HAProxyTimeout   time.Duration
	HAProxyNets      []*net.IPNet
	XClient          bool
	XClientNets      []*net.IPNet
	DisablePlainAuth bool
	// Protocol-level settings.
	Config config.SMTPProtocolConfig
	Auth   Authenticator
	Proxy  *proxy.Submission
}

// Server is the submission SMTP server (port 587 / 465).
type Server struct {
	opts   Options
	subSrv *goSmtp.Server
}

// New creates the submission server. Call ServeSubmit to start it.
func New(opts Options) *Server {
	s := &Server{opts: opts}
	be := &backend{srv: s}
	srv := goSmtp.NewServer(be)
	srv.Domain = opts.Config.Hostname
	srv.MaxMessageBytes = opts.Config.MaxMsgSize
	if r := opts.Config.MaxRecipients; r > 0 {
		srv.MaxRecipients = r
	}
	if l := opts.Config.MaxLineLength; l > 0 {
		srv.MaxLineLength = l
	}
	srv.AllowInsecureAuth = !opts.DisablePlainAuth
	srv.ReadTimeout = 5 * time.Minute
	srv.WriteTimeout = 5 * time.Minute
	s.subSrv = srv
	return s
}

// ServeSubmit starts the submission listener. tlsCfg non-nil = implicit TLS (port 465).
// STARTTLS is handled by go-smtp when TLSConfig is set on the server; for ssl mode
// the listener is wrapped with tls.NewListener before calling ServeSubmit.
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

type backend struct{ srv *Server }

func (b *backend) NewSession(c *goSmtp.Conn) (goSmtp.Session, error) {
	remoteIP := connRemoteIP(c)
	return &session{srv: b.srv, remoteIP: remoteIP}, nil
}

type session struct {
	srv      *Server
	remoteIP net.IP
	from     string
	rcpts    []string
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
	s.rcpts = append(s.rcpts, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("smtp/data: read: %w", err)
	}
	p := s.srv.opts.Proxy
	if p == nil {
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 3, 0},
			Message:      "Upstream MTA not configured",
		}
	}
	if err := p.Send(s.from, s.rcpts, bytes.NewReader(data), s.remoteIP); err != nil {
		slog.Info("smtp: proxy rejected", "from", s.from, "err", err)
		return err
	}
	slog.Info("smtp: proxied", "from", s.from, "rcpts", s.rcpts, "size", len(data))
	return nil
}

// AuthMechanisms advertises PLAIN auth.
func (s *session) AuthMechanisms() []string {
	return []string{sasl.Plain}
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

func connRemoteIP(c *goSmtp.Conn) net.IP {
	addr := c.Conn().RemoteAddr()
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.IP
	}
	return net.IPv4zero
}
