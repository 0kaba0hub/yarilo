package sieve

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	goSmtp "github.com/emersion/go-smtp"
	"github.com/foxcpp/go-sieve/interp"

	"github.com/0kaba0hub/yarilo/pkg/config"
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
	return s.submit(ctx, envFrom, []string{addr}, bytes.NewReader(raw))
}

// sendVacation sends an RFC 5230 auto-reply to the original sender, after
// checking dedup state and RFC 5230 §4.5 skip conditions.
func (s *Sender) sendVacation(
	ctx context.Context,
	store ScriptStore,
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

	sent, err := store.VacationSent(ctx, opts.Username, opts.HomeDir, resp.Handle, sender)
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
	if err := s.submit(ctx, "", []string{sender}, bytes.NewReader(msg)); err != nil {
		return fmt.Errorf("sieve/sender: vacation send: %w", err)
	}

	if err := store.MarkVacationSent(ctx, opts.Username, opts.HomeDir, resp.Handle, sender, intervalSecs); err != nil {
		slog.Warn("sieve/sender: vacation dedup mark failed", "user", opts.Username, "err", err)
	}
	return nil
}

func (s *Sender) submit(ctx context.Context, from string, to []string, body *bytes.Reader) error {
	c, err := s.dial(ctx)
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

func (s *Sender) dial(ctx context.Context) (*goSmtp.Client, error) {
	host, port, err := net.SplitHostPort(s.cfg.SubmissionHost)
	if err != nil {
		// No port — use default 25.
		host = s.cfg.SubmissionHost
		port = "25"
	}
	addr := net.JoinHostPort(host, port)
	nd := &net.Dialer{Timeout: s.connectTimeout()}

	var c *goSmtp.Client
	switch strings.ToLower(s.cfg.SubmissionSSL) {
	case "smtps", "submissions":
		d := &tls.Dialer{
			NetDialer: nd,
			Config:    &tls.Config{ServerName: host}, //nolint:gosec
		}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		c = goSmtp.NewClient(conn)
	case "starttls":
		conn, err := nd.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		c, err = goSmtp.NewClientStartTLS(conn, &tls.Config{ServerName: host}) //nolint:gosec
		if err != nil {
			conn.Close()
			return nil, err
		}
	default:
		conn, err := nd.DialContext(ctx, "tcp", addr)
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
	fmt.Fprintf(&b, "Message-ID: <%d.sieve-vacation@yarilo>\r\n", time.Now().UnixNano())
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 +0000"))
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

// sendNotify dispatches an RFC 5435 enotify action.
// Only the mailto: method is sent via SMTP; other methods are logged and dropped.
func (s *Sender) sendNotify(ctx context.Context, opts FilterOptions, hdr textproto.MIMEHeader, n interp.ActionNotify) error {
	// RFC 5435 §2.7: do not send notifications for auto-submitted messages.
	if isAutoSubmitted(hdr) {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(n.Method), "mailto:") {
		slog.Warn("sieve/sender: unsupported notify method, dropped", "method", n.Method, "user", opts.Username)
		return nil
	}

	u, err := url.Parse(n.Method)
	if err != nil {
		return fmt.Errorf("sieve/sender: parse mailto URI %q: %w", n.Method, err)
	}
	recipient := u.Opaque
	if recipient == "" {
		return fmt.Errorf("sieve/sender: mailto URI has no recipient: %q", n.Method)
	}

	q := u.Query()
	subject := q.Get("subject")
	if subject == "" {
		subject = "Notification"
	}
	body := n.Message
	if body == "" {
		body = q.Get("body")
	}

	// Envelope-from: use original sender (default behavior).
	// Falls back to <> if original message had null sender.
	envelopeFrom := opts.EnvFrom

	from := opts.EnvTo
	if from == "" {
		from = n.From
	}

	msg := buildNotifyMessage(from, recipient, subject, body)
	if err := s.submit(ctx, envelopeFrom, []string{recipient}, bytes.NewReader(msg)); err != nil {
		return fmt.Errorf("sieve/sender: notify send to %s: %w", recipient, err)
	}
	return nil
}

// sendReport dispatches an ARF (RFC 5965) feedback report about the current
// message to the target address (vnd.yarilo.report). The report is sent From the
// reporting mailbox; envelope-from is the reporting mailbox so a bounce returns
// to it rather than looping to the original sender.
func (s *Sender) sendReport(ctx context.Context, opts FilterOptions, r interp.ActionReport) error {
	if r.Target == "" {
		return fmt.Errorf("sieve/sender: report has no target")
	}
	from := opts.EnvTo
	ua := s.cfg.ReportUserAgent
	if ua == "" {
		ua = "yarilo"
	}
	msg := buildReportMessage(from, r.Target, ua, opts.EnvFrom, opts.EnvTo, r, opts.MsgRaw)
	if err := s.submit(ctx, from, []string{r.Target}, bytes.NewReader(msg)); err != nil {
		return fmt.Errorf("sieve/sender: report send to %s: %w", r.Target, err)
	}
	return nil
}

// buildReportMessage constructs an RFC 5965 multipart/report feedback message:
// a human-readable part, a machine-readable message/feedback-report part, and
// the reported message (full, or headers-only when requested).
func buildReportMessage(from, to, userAgent, origMailFrom, origRcptTo string, r interp.ActionReport, orig []byte) []byte {
	boundary := fmt.Sprintf("yarilo-report-%d", time.Now().UnixNano())
	now := time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 +0000")

	var b bytes.Buffer
	fmt.Fprintf(&b, "Message-ID: <%d.sieve-report@yarilo>\r\n", time.Now().UnixNano())
	fmt.Fprintf(&b, "Date: %s\r\n", now)
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s report\r\n", r.FeedbackType)
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("X-Auto-Response-Suppress: All\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/report; report-type=feedback-report; boundary=\"%s\"\r\n", boundary)
	b.WriteString("\r\n")

	// Part 1: human-readable explanation.
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(r.Message)
	b.WriteString("\r\n\r\n")

	// Part 2: machine-readable feedback report (RFC 5965 §3.1).
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: message/feedback-report\r\n\r\n")
	b.WriteString("Version: 1\r\n")
	fmt.Fprintf(&b, "Feedback-Type: %s\r\n", r.FeedbackType)
	fmt.Fprintf(&b, "User-Agent: %s\r\n", userAgent)
	if origMailFrom != "" {
		fmt.Fprintf(&b, "Original-Mail-From: %s\r\n", origMailFrom)
	}
	if origRcptTo != "" {
		fmt.Fprintf(&b, "Original-Rcpt-To: %s\r\n", origRcptTo)
	}
	fmt.Fprintf(&b, "Arrival-Date: %s\r\n", now)
	b.WriteString("\r\n")

	// Part 3: the reported message (full or headers-only).
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	if r.HeadersOnly {
		b.WriteString("Content-Type: text/rfc822-headers\r\n\r\n")
		b.Write(messageHeaders(orig))
	} else {
		b.WriteString("Content-Type: message/rfc822\r\n\r\n")
		b.Write(orig)
	}
	b.WriteString("\r\n")

	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

// messageHeaders returns the header block of raw (everything up to and including
// the blank separator line), for the headers-only report variant.
func messageHeaders(raw []byte) []byte {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i+4]
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return raw[:i+2]
	}
	return raw
}

// buildNotifyMessage constructs a minimal RFC 5435 notification email.
func buildNotifyMessage(from, to, subject, body string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Message-ID: <%d.sieve-notify@yarilo>\r\n", time.Now().UnixNano())
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 +0000"))
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("Auto-Submitted: auto-notified\r\n")
	b.WriteString("X-Auto-Response-Suppress: All\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.Bytes()
}
