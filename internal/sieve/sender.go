package sieve

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/textproto"
	"os"
	"strings"
	"time"

	goSmtp "github.com/0kaba0hub/go-smtp"
	"github.com/emersion/go-sasl"
	"github.com/foxcpp/go-sieve/interp"

	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
)

// Sender dispatches outbound mail for Sieve redirect and vacation actions
// using a configured upstream MTA (submission_host).
type Sender struct {
	cfg config.SieveConfig
}

func newSender(cfg config.SieveConfig) *Sender {
	return &Sender{cfg: cfg}
}

// sendRedirect forwards the original message to addr with the original envelope-from.
func (s *Sender) sendRedirect(ctx context.Context, envFrom, addr string, raw []byte) error {
	return s.submit(envFrom, []string{addr}, bytes.NewReader(raw))
}

// sendVacation sends an RFC 5230 auto-reply to the original sender, after
// checking dedup state and RFC 5230 §4.5 skip conditions.
func (s *Sender) sendVacation(
	ctx context.Context,
	d dict.Dict,
	opts FilterOptions,
	hdr textproto.MIMEHeader,
	resp interp.VacationResponse,
) error {
	sender := opts.EnvFrom
	if sender == "" || sender == "<>" {
		return nil
	}

	// RFC 5230 §4.5: skip if sender is a mailing list or auto-generated message.
	if isMailingList(hdr) || isAutoSubmitted(hdr) {
		return nil
	}

	intervalSecs := vacationIntervalSecs(resp)

	sent, err := vacationSent(ctx, d, opts.Username, opts.HomeDir, sender, resp.Handle)
	if err != nil {
		return fmt.Errorf("sieve/sender: vacation dedup lookup: %w", err)
	}
	if sent {
		return nil
	}

	from := resp.From
	if from == "" {
		from = opts.EnvTo
	}

	msg := buildVacationReply(from, sender, resp.Subject, resp.Body)

	// Envelope-from is null to prevent mail loops (RFC 5230 §4.5).
	if err := s.submit("", []string{sender}, bytes.NewReader(msg)); err != nil {
		return fmt.Errorf("sieve/sender: vacation send: %w", err)
	}

	if err := markVacationSent(ctx, d, opts.Username, opts.HomeDir, sender, resp.Handle, intervalSecs); err != nil {
		slog.Warn("sieve/sender: vacation dedup mark failed", "user", opts.Username, "err", err)
	}
	return nil
}

func (s *Sender) submit(from string, to []string, body *bytes.Reader) error {
	c, err := s.dial()
	if err != nil {
		return fmt.Errorf("sieve/sender: dial %s: %w", s.cfg.SubmissionHost, err)
	}
	defer c.Close()

	if err := c.Hello("localhost"); err != nil {
		return fmt.Errorf("sieve/sender: EHLO: %w", err)
	}

	if user := os.Getenv("YARILO_SIEVE_SUBMISSION_USER"); user != "" {
		auth := sasl.NewPlainClient("", user, os.Getenv("YARILO_SIEVE_SUBMISSION_PASSWORD"))
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("sieve/sender: AUTH: %w", err)
		}
	}

	if err := c.SendMail(from, to, body); err != nil {
		return fmt.Errorf("sieve/sender: DATA: %w", err)
	}
	return nil
}

func (s *Sender) dial() (*goSmtp.Client, error) {
	host, port, err := net.SplitHostPort(s.cfg.SubmissionHost)
	if err != nil {
		// No port — use default 25.
		host = s.cfg.SubmissionHost
		port = "25"
	}
	addr := net.JoinHostPort(host, port)
	ct := s.connectTimeout()

	var c *goSmtp.Client
	switch strings.ToLower(s.cfg.SubmissionSSL) {
	case "smtps", "submissions":
		d := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: ct},
			Config:    &tls.Config{ServerName: host}, //nolint:gosec
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
		c, err = goSmtp.NewClientStartTLS(conn, &tls.Config{ServerName: host}) //nolint:gosec
		if err != nil {
			conn.Close()
			return nil, err
		}
	default:
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

func (s *Sender) connectTimeout() time.Duration {
	if s.cfg.SubmissionTimeout > 0 {
		return time.Duration(s.cfg.SubmissionTimeout) * time.Second
	}
	return 30 * time.Second
}

func (s *Sender) commandTimeout() time.Duration {
	if s.cfg.SubmissionTimeout > 0 {
		return time.Duration(s.cfg.SubmissionTimeout) * time.Second
	}
	return 30 * time.Second
}

// buildVacationReply constructs a minimal RFC 5230 auto-reply message.
func buildVacationReply(from, to, subject, body string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("Auto-Submitted: auto-replied\r\n")
	b.WriteString("X-Auto-Response-Suppress: OOF, AutoReply\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.Bytes()
}

// vacationIntervalSecs returns the dedup interval in seconds from a VacationResponse.
func vacationIntervalSecs(resp interp.VacationResponse) int {
	if resp.Seconds > 0 {
		return resp.Seconds
	}
	days := resp.Days
	if days <= 0 {
		days = 7
	}
	return days * 86400
}

// isMailingList reports whether the message headers indicate a mailing list.
func isMailingList(hdr textproto.MIMEHeader) bool {
	if hdr.Get("List-Id") != "" {
		return true
	}
	prec := strings.ToLower(hdr.Get("Precedence"))
	return prec == "bulk" || prec == "list" || prec == "junk"
}

// isAutoSubmitted reports whether the message was automatically generated
// (Auto-Submitted header present and not "no").
func isAutoSubmitted(hdr textproto.MIMEHeader) bool {
	v := strings.ToLower(strings.TrimSpace(hdr.Get("Auto-Submitted")))
	return v != "" && v != "no"
}
