// Package submission implements the submission servers (port 587 / 465).
// AUTH PLAIN is required. Messages are forwarded to the configured upstream MTA
// via protocol.submission.relay. No MX inbound — external MTAs deliver to LMTP (port 24).
package submission

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

	"github.com/0kaba0hub/yarilo/internal/submission/proxy"
	"github.com/0kaba0hub/yarilo/pkg/config"
)

// Authenticator verifies submission AUTH credentials.
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
	Config config.SubmissionProtocolConfig
	Auth   Authenticator
	Proxy  *proxy.Submission
}

// Server is the submission server (port 587 / 465).
type Server struct {
	opts        Options
	subSrv      *goSmtp.Server
	workarounds submissionWorkarounds
}

// New creates the submission server. Call Serve to start it.
func New(opts Options) *Server {
	s := &Server{opts: opts, workarounds: parseWorkarounds(opts.Config.Workarounds)}
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

// Serve starts the submission listener. tlsCfg non-nil = implicit TLS (port 465).
// STARTTLS is handled by go-smtp when TLSConfig is set on the server; for ssl mode
// the listener is wrapped with tls.NewListener before calling Serve.
func (s *Server) Serve(ln net.Listener, tlsCfg *tls.Config) error {
	slog.Info("submission: listening", "addr", ln.Addr())
	if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
	}
	if s.workarounds != 0 {
		ln = &workaroundListener{Listener: ln, workarounds: s.workarounds}
	}
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
	return &session{srv: b.srv, conn: c, remoteIP: remoteIP}, nil
}

type session struct {
	srv      *Server
	conn     *goSmtp.Conn
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
		return fmt.Errorf("submission/data: read: %w", err)
	}
	p := s.srv.opts.Proxy
	if p == nil {
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 3, 0},
			Message:      "Upstream MTA not configured",
		}
	}
	body := data
	if s.srv.opts.Config.AddReceivedHeader {
		body = append([]byte(s.receivedHeader()), data...)
	}
	if err := p.Send(s.from, s.rcpts, bytes.NewReader(body), s.remoteIP); err != nil {
		slog.Info("submission: proxy rejected", "from", s.from, "err", err)
		return err
	}
	slog.Info("submission: proxied", "from", s.from, "rcpts", s.rcpts, "size", len(body))
	return nil
}

func (s *session) receivedHeader() string {
	helo := ""
	tlsActive := false
	if s.conn != nil {
		helo = s.conn.Hostname()
		_, tlsActive = s.conn.TLSConnectionState()
	}
	with := "ESMTPA"
	if tlsActive {
		with = "ESMTPSA"
	}
	hostname := s.srv.opts.Config.Hostname
	if hostname == "" {
		hostname = "yarilo"
	}
	client := s.remoteIP.String()
	if helo == "" {
		helo = client
	}
	return fmt.Sprintf("Received: from %s ([%s])\r\n\tby %s with %s;\r\n\t%s\r\n",
		helo, client, hostname, with, time.Now().UTC().Format(time.RFC1123Z))
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
