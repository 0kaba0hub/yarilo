// Package lmtp implements an LMTP server (RFC 2033) for local mail delivery.
// External MTAs (e.g. Postfix) connect on port 24 or a Unix socket and use
// LHLO + per-recipient DATA responses to deliver mail to local mailboxes.
package lmtp

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"time"

	goSmtp "github.com/0kaba0hub/go-smtp"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Server is an LMTP server backed by a MailboxBackend and IndexBackend.
type Server struct {
	srv *goSmtp.Server
	mb  mailbox.MailboxBackend
	idx mailbox.IndexBackend
}

// New creates an LMTP server. hostname is used in the LHLO greeting.
func New(hostname string, mb mailbox.MailboxBackend, idx mailbox.IndexBackend) *Server {
	s := &Server{mb: mb, idx: idx}
	be := &backend{mb: mb, idx: idx}

	srv := goSmtp.NewServer(be)
	srv.Domain = hostname
	srv.LMTP = true
	srv.ReadTimeout = 5 * time.Minute
	srv.WriteTimeout = 5 * time.Minute

	s.srv = srv
	return s
}

// Serve starts accepting LMTP connections on ln.
func (s *Server) Serve(ln net.Listener) error {
	slog.Info("lmtp: listening", "addr", ln.Addr())
	return s.srv.Serve(ln)
}

// ---- backend ----------------------------------------------------------------

type backend struct {
	mb  mailbox.MailboxBackend
	idx mailbox.IndexBackend
}

func (b *backend) NewSession(_ *goSmtp.Conn) (goSmtp.Session, error) {
	return &session{mb: b.mb, idx: b.idx}, nil
}

// ---- session ----------------------------------------------------------------

type session struct {
	mb    mailbox.MailboxBackend
	idx   mailbox.IndexBackend
	from  string
	rcpts []string
}

func (s *session) Mail(from string, _ *goSmtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *goSmtp.RcptOptions) error {
	s.rcpts = append(s.rcpts, to)
	return nil
}

// Data is never called in LMTP mode — LMTPData handles DATA instead.
func (s *session) Data(_ io.Reader) error { return nil }

// LMTPData delivers the message and reports per-recipient status via status.SetStatus.
// This is what differentiates LMTP from SMTP: each recipient gets its own result code.
func (s *session) LMTPData(r io.Reader, status goSmtp.StatusCollector) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	received := buildReceivedHeader(s.from)
	full := append([]byte(received), data...)

	for _, rcpt := range s.rcpts {
		err := deliverOne(s.mb, s.idx, rcpt, bytes.NewReader(full), int64(len(full)))
		if err != nil {
			slog.Error("lmtp: delivery failed", "rcpt", rcpt, "err", err)
		} else {
			slog.Info("lmtp: delivered", "rcpt", rcpt, "size", len(full))
		}
		status.SetStatus(rcpt, err)
	}
	return nil
}

func (s *session) Reset() {
	s.from = ""
	s.rcpts = nil
}

func (s *session) Logout() error { return nil }
