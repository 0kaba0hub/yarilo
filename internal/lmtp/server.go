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
	srv    *goSmtp.Server
	cfg    config.LMTPProtocolConfig
	mb     mailbox.MailboxBackend
	idx    mailbox.IndexBackend
	router *proxyRouter // non-nil when proxy mode is active
}

// New creates an LMTP server from config.
func New(hostname string, cfg config.LMTPProtocolConfig, mb mailbox.MailboxBackend, idx mailbox.IndexBackend) *Server {
	var router *proxyRouter
	if cfg.Proxy.Enabled {
		router = newProxyRouter(hostname, cfg.Proxy)
	}

	s := &Server{cfg: cfg, mb: mb, idx: idx, router: router}
	be := &backend{cfg: cfg, mb: mb, idx: idx, router: router}

	srv := goSmtp.NewServer(be)
	srv.Domain = hostname
	srv.LMTP = true
	srv.ReadTimeout = time.Duration(cfg.ReadTimeout) * time.Second
	srv.WriteTimeout = time.Duration(cfg.WriteTimeout) * time.Second

	s.srv = srv
	return s
}

// Serve starts accepting LMTP connections on ln.
func (s *Server) Serve(ln net.Listener) error {
	slog.Info("lmtp: listening", "addr", ln.Addr(), "proxy", s.cfg.Proxy.Enabled)
	return s.srv.Serve(ln)
}

// ---- backend ----------------------------------------------------------------

type backend struct {
	cfg    config.LMTPProtocolConfig
	mb     mailbox.MailboxBackend
	idx    mailbox.IndexBackend
	router *proxyRouter
}

func (b *backend) NewSession(_ *goSmtp.Conn) (goSmtp.Session, error) {
	return &session{cfg: b.cfg, mb: b.mb, idx: b.idx, router: b.router}, nil
}

// ---- session ----------------------------------------------------------------

type session struct {
	cfg        config.LMTPProtocolConfig
	mb         mailbox.MailboxBackend
	idx        mailbox.IndexBackend
	router     *proxyRouter
	from       string
	rcpts      []string            // local recipients
	proxyRcpts map[string][]string // backend addr → []rcpt (proxy mode)
}

func (s *session) Mail(from string, _ *goSmtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *goSmtp.RcptOptions) error {
	if s.router != nil {
		return s.rcptProxy(to)
	}
	return s.rcptLocal(to)
}

func (s *session) rcptLocal(to string) error {
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
		return &goSmtp.SMTPError{Code: 550, EnhancedCode: goSmtp.EnhancedCode{5, 1, 1}, Message: "User doesn't exist: " + to}
	}
	s.rcpts = append(s.rcpts, to)
	return nil
}

func (s *session) rcptProxy(to string) error {
	user, _, err := resolveMailbox(to)
	if err != nil {
		return &goSmtp.SMTPError{Code: 501, EnhancedCode: goSmtp.EnhancedCode{5, 1, 3}, Message: "Bad recipient address"}
	}
	addr, err := s.router.route(user)
	if err != nil {
		slog.Error("lmtp: proxy route failed", "user", user, "err", err)
		return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Temporary routing error"}
	}
	if s.proxyRcpts == nil {
		s.proxyRcpts = make(map[string][]string)
	}
	s.proxyRcpts[addr] = append(s.proxyRcpts[addr], to)
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

	// Proxy recipients: fan-out to backends in parallel.
	if len(s.proxyRcpts) > 0 {
		results := s.router.proxyFanOut(s.proxyRcpts, s.from, full)
		for rcpt, rerr := range results {
			if rerr != nil {
				slog.Error("lmtp: proxy delivery failed", "rcpt", rcpt, "err", rerr)
				if s.cfg.VerboseReplies {
					rerr = &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 2, 0}, Message: rerr.Error()}
				} else {
					rerr = &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 2, 0}, Message: "Proxy delivery failed"}
				}
			} else {
				slog.Info("lmtp: proxy delivered", "rcpt", rcpt, "size", len(full))
			}
			status.SetStatus(rcpt, rerr)
		}
	}

	// Local recipients: deliver directly.
	for _, rcpt := range s.rcpts {
		deliverRcpt := rcpt
		if !s.cfg.SaveToDetailMailbox {
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
	s.proxyRcpts = nil
}

func (s *session) Logout() error { return nil }
