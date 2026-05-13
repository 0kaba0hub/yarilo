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

	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Server is an LMTP server backed by a MailboxBackend and IndexBackend.
type Server struct {
	srv *goSmtp.Server
	cfg config.LMTPProtocolConfig
	mb  mailbox.MailboxBackend
	idx mailbox.IndexBackend
}

// New creates an LMTP server from config.
func New(hostname string, cfg config.LMTPProtocolConfig, mb mailbox.MailboxBackend, idx mailbox.IndexBackend) *Server {
	s := &Server{cfg: cfg, mb: mb, idx: idx}
	be := &backend{cfg: cfg, mb: mb, idx: idx}

	srv := goSmtp.NewServer(be)
	srv.Domain = hostname
	srv.LMTP = true
	if cfg.LoginGreeting != "" {
		srv.Domain = hostname // greeting is built as "220 <domain> LMTP ..."
	}
	srv.ReadTimeout = time.Duration(cfg.ReadTimeout) * time.Second
	srv.WriteTimeout = time.Duration(cfg.WriteTimeout) * time.Second

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
	cfg config.LMTPProtocolConfig
	mb  mailbox.MailboxBackend
	idx mailbox.IndexBackend
}

func (b *backend) NewSession(_ *goSmtp.Conn) (goSmtp.Session, error) {
	return &session{cfg: b.cfg, mb: b.mb, idx: b.idx}, nil
}

// ---- session ----------------------------------------------------------------

type session struct {
	cfg   config.LMTPProtocolConfig
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
	user, _, err := resolveMailbox(to)
	if err != nil {
		return &goSmtp.SMTPError{Code: 501, EnhancedCode: goSmtp.EnhancedCode{5, 1, 3}, Message: "Bad recipient address"}
	}
	exists, err := s.mb.FolderExists(user, "INBOX")
	if err != nil {
		slog.Error("lmtp: user lookup failed", "user", user, "err", err)
		return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Temporary user lookup error"}
	}
	if !exists {
		msg := "User doesn't exist: " + to
		return &goSmtp.SMTPError{Code: 550, EnhancedCode: goSmtp.EnhancedCode{5, 1, 1}, Message: msg}
	}
	s.rcpts = append(s.rcpts, to)
	return nil
}

// Data is never called in LMTP mode — LMTPData handles DATA instead.
func (s *session) Data(_ io.Reader) error { return nil }

// LMTPData delivers the message and reports per-recipient status via status.SetStatus.
func (s *session) LMTPData(r io.Reader, status goSmtp.StatusCollector) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	var full []byte
	if s.cfg.AddReceivedHeader {
		received := buildReceivedHeader(s.from)
		full = append([]byte(received), data...)
	} else {
		full = data
	}

	for _, rcpt := range s.rcpts {
		deliverRcpt := rcpt
		if !s.cfg.SaveToDetailMailbox {
			// Deliver to INBOX regardless of +detail suffix.
			deliverRcpt = stripDetail(rcpt)
		}
		err := deliverOne(s.mb, s.idx, deliverRcpt, bytes.NewReader(full), int64(len(full)))
		if err != nil {
			slog.Error("lmtp: delivery failed", "rcpt", rcpt, "err", err)
			if s.cfg.VerboseReplies {
				err = &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 2, 0}, Message: err.Error()}
			} else {
				err = &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 2, 0}, Message: "Local delivery failed"}
			}
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
