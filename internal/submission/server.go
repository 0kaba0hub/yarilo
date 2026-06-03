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

	"github.com/0kaba0hub/yarilo/internal/auth/oauth2"
	"github.com/0kaba0hub/yarilo/internal/submission/proxy"
	"github.com/0kaba0hub/yarilo/pkg/config"
)

// Authenticator verifies submission AUTH credentials.
type Authenticator interface {
	AuthPlain(username, password string) error
}

// MasterAuthenticator extends Authenticator with the SASL PLAIN
// authzid surface — i.e. Dovecot's master-user impersonation
// model exposed on the submission port. Adapters that only
// implement Authenticator (the AuthPlain-only surface) keep
// working unchanged; the session-level SASL hook type-asserts
// into MasterAuthenticator to decide whether to honour a
// distinct authzid.
type MasterAuthenticator interface {
	AuthPlainMaster(authzid, authid, password string) error
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
	// TLSConfig enables STARTTLS on plain-text listeners (port 587).
	// For implicit TLS (port 465) the listener is wrapped in Serve(_, tlsCfg).
	TLSConfig *tls.Config
	// Protocol-level settings.
	Config config.SubmissionProtocolConfig
	Auth   Authenticator
	Proxy  *proxy.Submission

	// FailureDelay holds the goroutine for this duration before
	// surfacing an auth-failure to the client. Mirrors Dovecot's
	// auth_failure_delay so unknown-user / wrong-password / non-
	// master-backend-with-authzid all return in the same wall-
	// clock time. Zero disables.
	FailureDelay time.Duration

	// OAuth2Enabled flips advertisement and acceptance of the
	// OAUTHBEARER SASL mechanism. Set by the wiring when at least
	// one OAuth provider is configured under auth.oauth2.
	OAuth2Enabled bool
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
	srv.TLSConfig = opts.TLSConfig
	s.subSrv = srv
	return s
}

// Serve starts the submission listener. tlsCfg non-nil = implicit TLS (port 465).
// STARTTLS is handled by go-smtp when TLSConfig is set on the server; for ssl mode
// the listener is wrapped with tls.NewListener before calling Serve.
func (s *Server) Serve(ln net.Listener, tlsCfg *tls.Config) error {
	slog.Info("submission: listening", "addr", ln.Addr().String())
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

// AuthMechanisms advertises supported SASL mechanisms.
// PLAIN: single-line base64 of \0user\0pass (RFC 4616).
// LOGIN: interactive, two server prompts (legacy — Outlook, some Android MUAs).
// OAUTHBEARER (RFC 7628): advertised only when an OAuth provider
// is configured under auth.oauth2.
func (s *session) AuthMechanisms() []string {
	out := []string{sasl.Plain, sasl.Login}
	if s.srv.opts.OAuth2Enabled {
		out = append(out, sasl.OAuthBearer)
	}
	return out
}

// Auth returns a sasl.Server for the requested mechanism. The
// PLAIN handler honours authzid via MasterAuthenticator when the
// backend supports it; LOGIN has no authzid surface (RFC limit)
// so it always dispatches to plain AuthPlain. OAUTHBEARER routes
// the bearer token through the regular AuthPlain surface; the
// OAuth passdb downstream extracts the token from req.Password.
func (s *session) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(s.authPlainSASL), nil
	case sasl.Login:
		return sasl.NewLoginServer(func(username, password string) error {
			return s.srv.opts.Auth.AuthPlain(username, password)
		}), nil
	case sasl.OAuthBearer:
		if !s.srv.opts.OAuth2Enabled {
			return nil, goSmtp.ErrAuthUnknownMechanism
		}
		return oauth2.NewOAuthBearerSASLServer(s.authOAuthBearerSASL), nil
	}
	return nil, goSmtp.ErrAuthUnknownMechanism
}

// authOAuthBearerSASL is the OAuthBearerAuthenticator callback.
// go-sasl has already parsed the GS2 envelope; we translate
// (Username, Token) into the AuthPlain surface (token-as-
// password) so the OAuth passdb downstream sees it.
func (s *session) authOAuthBearerSASL(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
	if err := s.srv.opts.Auth.AuthPlain(opts.Username, opts.Token); err != nil {
		if d := s.srv.opts.FailureDelay; d > 0 {
			time.Sleep(d)
		}
		slog.Info("submission: auth failed",
			"user", opts.Username,
			"mech", "OAUTHBEARER",
			"remoteIP", connRemoteIP(s.conn).String(),
		)
		return &sasl.OAuthBearerError{
			Status:  "invalid_token",
			Schemes: "bearer",
		}
	}
	slog.Info("submission: login",
		"user", opts.Username,
		"mech", "OAUTHBEARER",
		"remoteIP", connRemoteIP(s.conn).String(),
	)
	return nil
}

// authPlainSASL is the PlainAuthenticator callback for SMTP
// AUTH PLAIN. Empty authzid (or authzid == authid) takes the
// regular path; a distinct authzid routes through
// MasterAuthenticator when supported, else fails opaquely.
//
// Emits an audit log on success — `master_user` is empty for a
// regular login, set to the master's identity on impersonation
// (mirrors Dovecot's per-event `master_user=` field).
func (s *session) authPlainSASL(authzid, authid, password string) error {
	target := authid
	master := ""
	var err error
	if authzid == "" || authzid == authid {
		err = s.srv.opts.Auth.AuthPlain(authid, password)
	} else if m, ok := s.srv.opts.Auth.(MasterAuthenticator); ok {
		err = m.AuthPlainMaster(authzid, authid, password)
		if err == nil {
			target = authzid
			master = authid
		}
	} else {
		err = goSmtp.ErrAuthFailed
	}
	if err != nil {
		// Timing-leak mitigation. Mirrors Dovecot's
		// auth_failure_delay — same wall-clock for every failure
		// cause (wrong password, unknown user, non-master backend
		// with authzid, etc).
		if d := s.srv.opts.FailureDelay; d > 0 {
			time.Sleep(d)
		}
		slog.Info("submission: auth failed",
			"user", authid,
			"remoteIP", connRemoteIP(s.conn).String(),
		)
		return err
	}
	slog.Info("submission: login",
		"user", target,
		"master_user", master,
		"remoteIP", connRemoteIP(s.conn).String(),
	)
	return nil
}

// ---- helpers ------------------------------------------------------------

func connRemoteIP(c *goSmtp.Conn) net.IP {
	addr := c.Conn().RemoteAddr()
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.IP
	}
	return net.IPv4zero
}
