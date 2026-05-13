package smtp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/emersion/go-sasl"
	goSmtp "github.com/0kaba0hub/go-smtp"

	"github.com/0kaba0hub/yarilo/pkg/config"
)

// Relay is an SMTP client that forwards submission messages to a relay server.
// It mirrors Dovecot's submission_relay_* behaviour: one connection per message,
// fail-closed (4xx to client) on any transport error.
type Relay struct {
	cfg config.SMTPRelayConfig
}

func NewRelay(cfg config.SMTPRelayConfig) *Relay {
	return &Relay{cfg: cfg}
}

// Send relays a message to the configured relay server.
// Transport errors → 451 4.4.0. SMTP-level rejections are passed through as-is.
func (r *Relay) Send(from string, rcpts []string, body io.Reader) error {
	if r.cfg.Host == "" {
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 3, 0},
			Message:      "Relay not configured",
		}
	}

	c, err := r.dial()
	if err != nil {
		slog.Error("smtp/relay: connect failed", "host", r.cfg.Host, "err", err)
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 4, 0},
			Message:      "Failed to connect to relay server",
		}
	}
	defer c.Close()

	if r.cfg.User != "" {
		auth := sasl.NewPlainClient("", r.cfg.User, r.cfg.Password)
		if err := c.Auth(auth); err != nil {
			slog.Error("smtp/relay: auth failed", "user", r.cfg.User, "err", err)
			return &goSmtp.SMTPError{
				Code:         451,
				EnhancedCode: goSmtp.EnhancedCode{4, 7, 0},
				Message:      "Relay authentication failed",
			}
		}
	}

	if err := c.SendMail(from, rcpts, body); err != nil {
		var smtpErr *goSmtp.SMTPError
		if errors.As(err, &smtpErr) {
			return smtpErr
		}
		slog.Error("smtp/relay: send failed", "err", err)
		return &goSmtp.SMTPError{
			Code:         451,
			EnhancedCode: goSmtp.EnhancedCode{4, 4, 0},
			Message:      "Relay send error",
		}
	}
	return nil
}

func (r *Relay) dial() (*goSmtp.Client, error) {
	addr := fmt.Sprintf("%s:%d", r.cfg.Host, r.cfg.Port)
	ct := r.connectTimeout()

	var c *goSmtp.Client

	switch r.cfg.SSL {
	case "smtps":
		d := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: ct},
			Config:    r.tlsConfig(),
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
		c, err = goSmtp.NewClientStartTLS(conn, r.tlsConfig())
		if err != nil {
			conn.Close()
			return nil, err
		}
	default: // "no"
		conn, err := net.DialTimeout("tcp", addr, ct)
		if err != nil {
			return nil, err
		}
		c = goSmtp.NewClient(conn)
	}

	cmd := r.commandTimeout()
	c.CommandTimeout = cmd
	c.SubmissionTimeout = cmd + 2*time.Minute
	return c, nil
}

func (r *Relay) tlsConfig() *tls.Config {
	return &tls.Config{
		ServerName:         r.cfg.Host,
		InsecureSkipVerify: !r.cfg.SSLVerify, //nolint:gosec
	}
}

func (r *Relay) connectTimeout() time.Duration {
	if r.cfg.ConnectTimeout > 0 {
		return time.Duration(r.cfg.ConnectTimeout) * time.Second
	}
	return 30 * time.Second
}

func (r *Relay) commandTimeout() time.Duration {
	if r.cfg.CommandTimeout > 0 {
		return time.Duration(r.cfg.CommandTimeout) * time.Second
	}
	return 5 * time.Minute
}
