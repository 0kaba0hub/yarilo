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
	"github.com/0kaba0hub/yarilo/internal/auth/scram"
	"github.com/0kaba0hub/yarilo/internal/loginproto"
	"github.com/0kaba0hub/yarilo/internal/submission/proxy"
	"github.com/0kaba0hub/yarilo/pkg/config"
)

// Authenticator verifies submission AUTH credentials.
type Authenticator interface {
	AuthPlain(username, password string) error
}

// MasterAuthenticator extends Authenticator with the SASL PLAIN
// authzid surface (master-user impersonation) exposed on the
// submission port. Adapters that only implement Authenticator (the
// AuthPlain-only surface) keep working unchanged; the session-level
// SASL hook type-asserts into MasterAuthenticator to decide whether
// to honour a distinct authzid.
type MasterAuthenticator interface {
	AuthPlainMaster(authzid, authid, password string) error
}

// SCRAMSha256LookupAuthenticator exposes per-user SCRAM-SHA-256
// verifiers to the session-layer SASL mech. The submission
// session type-asserts opts.Auth into this interface to decide
// whether to advertise SCRAM-SHA-256 / SCRAM-SHA-256-PLUS in
// EHLO's AUTH= extension.
type SCRAMSha256LookupAuthenticator interface {
	LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error)
}

// SCRAMSha1LookupAuthenticator is the SHA-1 counterpart of
// SCRAMSha256LookupAuthenticator. Same type-assertion pattern
// gates SCRAM-SHA-1 / SCRAM-SHA-1-PLUS advertisement.
type SCRAMSha1LookupAuthenticator interface {
	LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error)
}

// Options configures the submission server.
type Options struct {
	// Infrastructure (per-listener; set by backend from ServiceConfig).
	HAProxy        bool
	HAProxyTimeout time.Duration
	HAProxyNets    []*net.IPNet
	// AuthAddr is the host:port of yarilo-auth used by the PreambleListener to
	// verify session tokens forwarded by login pods.
	AuthAddr         string
	AuthTLS          *tls.Config
	DisablePlainAuth bool
	// TLSConfig enables STARTTLS on plain-text listeners (port 587).
	// For implicit TLS (port 465) the listener is wrapped in Serve(_, tlsCfg).
	TLSConfig *tls.Config
	// Protocol-level settings.
	Config config.SubmissionProtocolConfig
	Auth   Authenticator
	Proxy  *proxy.Submission

	// FailureDelay holds the goroutine for this duration before
	// surfacing an auth-failure to the client. Equalises wall-clock
	// between unknown-user / wrong-password / non-master-backend-
	// with-authzid so timing carries no signal. Zero disables.
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
	if s.opts.AuthAddr != "" {
		ln = &loginproto.PreambleListener{Listener: ln, AuthAddr: s.opts.AuthAddr, AuthTLS: s.opts.AuthTLS, ExpectedService: "smtp"}
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
	username := ""
	sid := ""
	if pc, ok := c.Conn().(*loginproto.PreambleConn); ok {
		username = pc.Username
		sid = pc.SessionID
	}
	return &session{srv: b.srv, conn: c, remoteIP: remoteIP, username: username, sid: sid}, nil
}

type session struct {
	srv      *Server
	conn     *goSmtp.Conn
	remoteIP net.IP
	username string // set from preamble for pre-authenticated sessions
	sid      string // cross-service correlation ID from login-proxy
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
		slog.Info("submission: proxy rejected", "sid", s.sid, "from", s.from, "err", err, "result", "fail")
		return err
	}
	slog.Info("submission: proxied", "sid", s.sid, "from", s.from, "rcpts", s.rcpts, "size", len(body), "result", "ok")
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
		out = append(out, sasl.XOAuth2)
	}
	if _, ok := s.srv.opts.Auth.(SCRAMSha256LookupAuthenticator); ok {
		out = append(out, sasl.ScramSha256)
		if s.tlsExporter() != nil {
			out = append(out, sasl.ScramSha256Plus)
		}
	}
	if _, ok := s.srv.opts.Auth.(SCRAMSha1LookupAuthenticator); ok {
		out = append(out, sasl.ScramSha1)
		if s.tlsExporter() != nil {
			out = append(out, sasl.ScramSha1Plus)
		}
	}
	return out
}

// scramLookupShim adapts the submission-side
// SCRAMSha256LookupAuthenticator to the protocol-side
// SCRAMSha256Lookup that scram.NewSha256 expects. One method,
// one delegation — the indirection keeps the protocol package
// out of the submission package's public interface set.
type scramLookupShim struct {
	a SCRAMSha256LookupAuthenticator
}

func (s scramLookupShim) LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error) {
	return s.a.LookupSCRAMSha256(username)
}

// scramSha1LookupShim mirrors scramLookupShim for the SHA-1 family.
type scramSha1LookupShim struct {
	a SCRAMSha1LookupAuthenticator
}

func (s scramSha1LookupShim) LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error) {
	return s.a.LookupSCRAMSha1(username)
}

// completeSCRAMLogin is the OnSuccess hook for the session's
// SCRAM adapter. The SCRAM exchange has verified the user; we
// only need to record that the session is authenticated.
// AuthPlain is the existing surface go-smtp uses to mark the
// session authenticated; calling it with empty password keeps
// the surface uniform across all SASL mechs — the chainAuth
// adapter on the backend recognises this path as "already
// SCRAM-verified, no further check".
func (s *session) completeSCRAMLogin(_ string) error {
	// SCRAM success means the SCRAM SASL server already verified
	// the credential. go-smtp uses the session's authenticated
	// state to gate MAIL FROM; the framework flips that bit
	// automatically once Auth's sasl.Server returns done=true
	// with no error.
	return nil
}

// tlsExporter returns the 32-byte RFC 9266 channel-binding
// material derived from the underlying TLS conn, or nil when the
// connection is not TLS 1.3+.
func (s *session) tlsExporter() []byte {
	if s.conn == nil {
		return nil
	}
	state, ok := s.conn.TLSConnectionState()
	if !ok {
		return nil
	}
	if state.Version < tls.VersionTLS13 {
		return nil
	}
	out, err := state.ExportKeyingMaterial("EXPORTER-Channel-Binding", nil, 32)
	if err != nil {
		return nil
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
	case sasl.XOAuth2:
		if !s.srv.opts.OAuth2Enabled {
			return nil, goSmtp.ErrAuthUnknownMechanism
		}
		return oauth2.NewXOAuth2SASLServer(s.authXOAuth2SASL), nil
	case sasl.ScramSha256:
		lookup, ok := s.srv.opts.Auth.(SCRAMSha256LookupAuthenticator)
		if !ok {
			return nil, goSmtp.ErrAuthUnknownMechanism
		}
		return scram.NewSha256(scramLookupShim{lookup}, s.completeSCRAMLogin), nil
	case sasl.ScramSha256Plus:
		lookup, ok := s.srv.opts.Auth.(SCRAMSha256LookupAuthenticator)
		if !ok {
			return nil, goSmtp.ErrAuthUnknownMechanism
		}
		cb := s.tlsExporter()
		if cb == nil {
			return nil, goSmtp.ErrAuthUnknownMechanism
		}
		return scram.NewSha256Plus(scramLookupShim{lookup}, cb, s.completeSCRAMLogin), nil
	case sasl.ScramSha1:
		lookup, ok := s.srv.opts.Auth.(SCRAMSha1LookupAuthenticator)
		if !ok {
			return nil, goSmtp.ErrAuthUnknownMechanism
		}
		return scram.NewSha1(scramSha1LookupShim{lookup}, s.completeSCRAMLogin), nil
	case sasl.ScramSha1Plus:
		lookup, ok := s.srv.opts.Auth.(SCRAMSha1LookupAuthenticator)
		if !ok {
			return nil, goSmtp.ErrAuthUnknownMechanism
		}
		cb := s.tlsExporter()
		if cb == nil {
			return nil, goSmtp.ErrAuthUnknownMechanism
		}
		return scram.NewSha1Plus(scramSha1LookupShim{lookup}, cb, s.completeSCRAMLogin), nil
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
			"sid", s.sid,
			"user", opts.Username,
			"mech", "OAUTHBEARER",
			"remoteIP", connRemoteIP(s.conn).String(),
			"result", "fail",
		)
		return &sasl.OAuthBearerError{
			Status:  "invalid_token",
			Schemes: "bearer",
		}
	}
	slog.Info("submission: login",
		"sid", s.sid,
		"user", opts.Username,
		"mech", "OAUTHBEARER",
		"remoteIP", connRemoteIP(s.conn).String(),
		"result", "ok",
	)
	return nil
}

// authXOAuth2SASL mirrors authOAuthBearerSASL for the XOAUTH2
// wire format. Same token validation path — only the struct
// type carrying (Username, Token) differs.
func (s *session) authXOAuth2SASL(opts sasl.XOAuth2Options) *sasl.OAuthBearerError {
	if err := s.srv.opts.Auth.AuthPlain(opts.Username, opts.Token); err != nil {
		if d := s.srv.opts.FailureDelay; d > 0 {
			time.Sleep(d)
		}
		slog.Info("submission: auth failed",
			"sid", s.sid,
			"user", opts.Username,
			"mech", "XOAUTH2",
			"remoteIP", connRemoteIP(s.conn).String(),
			"result", "fail",
		)
		return &sasl.OAuthBearerError{
			Status:  "invalid_token",
			Schemes: "bearer",
		}
	}
	slog.Info("submission: login",
		"sid", s.sid,
		"user", opts.Username,
		"mech", "XOAUTH2",
		"remoteIP", connRemoteIP(s.conn).String(),
		"result", "ok",
	)
	return nil
}

// authPlainSASL is the PlainAuthenticator callback for SMTP
// AUTH PLAIN. Empty authzid (or authzid == authid) takes the
// regular path; a distinct authzid routes through
// MasterAuthenticator when supported, else fails opaquely.
//
// Emits an audit log on success — `master_user` is empty for a
// regular login, set to the master's identity on impersonation.
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
		// Timing-leak mitigation: same wall-clock for every failure
		// cause (wrong password, unknown user, non-master backend
		// with authzid, etc).
		if d := s.srv.opts.FailureDelay; d > 0 {
			time.Sleep(d)
		}
		slog.Info("submission: auth failed",
			"sid", s.sid,
			"user", authid,
			"remoteIP", connRemoteIP(s.conn).String(),
			"result", "fail",
		)
		return err
	}
	slog.Info("submission: login",
		"sid", s.sid,
		"user", target,
		"master_user", master,
		"remoteIP", connRemoteIP(s.conn).String(),
		"result", "ok",
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
