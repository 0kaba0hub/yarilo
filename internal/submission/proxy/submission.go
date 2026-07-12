// Package proxy implements the outbound submission proxy.
// Accepts an authenticated session from an MUA and forwards it to
// the configured upstream MTA — one TCP connection per message, fail-closed.
package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/emersion/go-sasl"
	goSmtp "github.com/emersion/go-smtp"

	"github.com/0kaba0hub/yarilo/pkg/config"
)

// Submission proxies outbound mail to the configured upstream MTA.
// One connection per message, fail-closed (4xx to client) on any transport error.
type Submission struct {
	cfg      config.RelayConfig
	hostname string // EHLO hostname sent to the upstream
}

func New(cfg config.RelayConfig, hostname string) *Submission {
	if hostname == "" {
		hostname = "localhost"
	}
	return &Submission{cfg: cfg, hostname: hostname}
}

// capabilities holds ESMTP extensions advertised by the upstream MTA.
type capabilities struct {
	has8BitMIME bool
	hasDSN      bool
	hasXCLIENT  bool
}

// Send forwards a message to the upstream MTA.
// clientIP is the originating MUA address; used for XCLIENT when Trusted=true.
// Transport errors → 451 4.4.0. SMTP-level rejections are passed through as-is.
func (s *Submission) Send(from string, rcpts []string, body io.Reader, clientIP net.IP) error {
	if s.cfg.Host == "" {
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 3, 0},
			Message:      "Upstream MTA not configured",
		}
	}

	c, err := s.dial()
	if err != nil {
		slog.Error("submission/proxy: connect failed", "host", s.cfg.Host, "err", err)
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 4, 0},
			Message:      "Failed to connect to upstream MTA",
		}
	}
	defer c.Close()

	if err := c.Hello(s.hostname); err != nil {
		slog.Error("submission/proxy: EHLO failed", "err", err)
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 4, 0},
			Message:      "Upstream EHLO failed",
		}
	}

	caps := s.probeCapabilities(c)
	slog.Debug("submission/proxy: upstream capabilities",
		"host", s.cfg.Host,
		"8bitmime", caps.has8BitMIME,
		"dsn", caps.hasDSN,
		"xclient", caps.hasXCLIENT,
	)

	if s.cfg.Trusted && caps.hasXCLIENT && clientIP != nil {
		if err := c.XClient(goSmtp.XClientData{Addr: clientIP}); err != nil {
			slog.Warn("submission/proxy: XCLIENT failed", "err", err)
		}
	}

	if s.cfg.User != "" {
		auth := sasl.NewPlainClient("", s.cfg.User, s.cfg.Password)
		if err := c.Auth(auth); err != nil {
			slog.Error("submission/proxy: auth failed", "user", s.cfg.User, "err", err)
			return &goSmtp.SMTPError{
				Code:         451,
				EnhancedCode: goSmtp.EnhancedCode{4, 7, 0},
				Message:      "Upstream authentication failed",
			}
		}
	}

	if err := c.SendMail(from, rcpts, body); err != nil {
		var smtpErr *goSmtp.SMTPError
		if errors.As(err, &smtpErr) {
			return smtpErr
		}
		slog.Error("submission/proxy: send failed", "err", err)
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 4, 0},
			Message:      "Upstream send error",
		}
	}
	return nil
}

func (s *Submission) probeCapabilities(c *goSmtp.Client) capabilities {
	has8bit, _ := c.Extension("8BITMIME")
	hasDSN, _ := c.Extension("DSN")
	hasXCLIENT, _ := c.Extension("XCLIENT")
	return capabilities{
		has8BitMIME: has8bit,
		hasDSN:      hasDSN,
		hasXCLIENT:  hasXCLIENT,
	}
}

func (s *Submission) dial() (*goSmtp.Client, error) {
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	ct := s.connectTimeout()

	var c *goSmtp.Client

	switch s.cfg.SSL {
	case "smtps":
		d := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: ct},
			Config:    s.tlsConfig(),
		}
		conn, err := d.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		c = goSmtp.NewClient(conn)
	case "starttls":
		conn, err := net.DialTimeout("tcp", addr, ct)
		if err != nil {
			return nil, err
		}
		var startErr error
		c, startErr = goSmtp.NewClientStartTLS(conn, s.tlsConfig())
		if startErr != nil {
			conn.Close()
			return nil, startErr
		}
	default: // "no"
		conn, err := net.DialTimeout("tcp", addr, ct)
		if err != nil {
			return nil, err
		}
		c = goSmtp.NewClient(conn)
	}

	cmd := s.commandTimeout()
	c.CommandTimeout = cmd
	c.SubmissionTimeout = cmd + 2*time.Minute
	return c, nil
}

func (s *Submission) tlsConfig() *tls.Config {
	return &tls.Config{
		ServerName:         s.cfg.Host,
		InsecureSkipVerify: !s.cfg.SSLVerify, //nolint:gosec
	}
}

func (s *Submission) connectTimeout() time.Duration {
	if s.cfg.ConnectTimeout > 0 {
		return time.Duration(s.cfg.ConnectTimeout) * time.Second
	}
	return 30 * time.Second
}

func (s *Submission) commandTimeout() time.Duration {
	if s.cfg.CommandTimeout > 0 {
		return time.Duration(s.cfg.CommandTimeout) * time.Second
	}
	return 5 * time.Minute
}
