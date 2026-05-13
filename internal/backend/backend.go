// Package backend wires all server components for the backend (or single) mode.
package backend

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	authsql "github.com/0kaba0hub/yarilo/internal/auth/sql"
	"github.com/0kaba0hub/yarilo/internal/connlimit"
	imapsvr "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/lmtp"
	pop3svr "github.com/0kaba0hub/yarilo/internal/pop3"
	smtpsvr "github.com/0kaba0hub/yarilo/internal/smtp"
	smtpproxy "github.com/0kaba0hub/yarilo/internal/smtp/proxy"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/dbox"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Server is the yarilo backend (or single-node) server.
type Server struct {
	cfg   *config.Config
	telem *telemetry.Server
	imap  *imapsvr.Server // nil if neither IMAP nor IMAPS is active
	pop3  *pop3svr.Server // nil if neither POP3 nor POP3S is active
	smtp  *smtpsvr.Server // nil if no SMTP/Submission/Submissions is active
	index *file.Backend
}

// New creates and wires all components according to cfg.
func New(cfg *config.Config) (*Server, error) {
	// ---- auth ----
	passdbs, err := buildPassdbs(cfg.Auth.Passdb)
	if err != nil {
		return nil, fmt.Errorf("backend: auth: %w", err)
	}
	authChain := protocol.Chain(passdbs)

	// ---- storage ----
	if cfg.Storage.MaildirRoot == "" {
		cfg.Storage.MaildirRoot = "/var/mail/vhosts"
	}
	mbox, err := buildMailbox(cfg.Storage)
	if err != nil {
		return nil, fmt.Errorf("backend: mailbox: %w", err)
	}
	indexRoot := cfg.Storage.IndexDir
	if indexRoot == "" {
		indexRoot = cfg.Storage.MaildirRoot
	}
	idx := file.New(indexRoot)

	// ---- shared connection limiter (IMAP + POP3) ----
	connLimiter := connlimit.New(cfg.General.Limits.MaxUserIPConnections)

	// ---- HAProxy / XClient shared nets ----
	haproxyNets := parseCIDRs(cfg.General.HAProxy.TrustedNets)
	xclientNets := parseCIDRs(cfg.General.XClient.TrustedNets)
	haproxyTimeout := time.Duration(cfg.General.HAProxy.Timeout) * time.Second

	// ---- IMAP ----
	var imapServer *imapsvr.Server
	svcs := cfg.Services
	if svcs.IMAP.Active() || svcs.IMAPS.Active() {
		primary := firstActive(svcs.IMAPS, svcs.IMAP)
		var imapTLS *tls.Config
		if svcs.IMAPS.Active() {
			t, err := buildTLS(cfg, svcs.IMAPS)
			if err != nil {
				return nil, fmt.Errorf("backend: IMAPS TLS: %w", err)
			}
			imapTLS = t
		}
		p := cfg.Protocol.IMAP
		imapServer = imapsvr.New(imapsvr.Options{
			Addr:               listenAddr(svcs.IMAPS),
			AddrPlain:          listenAddr(svcs.IMAP),
			TLSConfig:          imapTLS,
			Mailbox:            mbox,
			Index:              idx,
			Auth:               authChain,
			ProxyProtocol:      primary.HAProxy,
			HAProxyTimeout:     haproxyTimeout,
			HAProxyTrustedNets: haproxyNets,
			XClient:            primary.XClient,
			XClientTrustedNets: xclientNets,
			DisablePlainAuth:   primary.DisablePlainAuth,
			IdleNotifyInterval: time.Duration(p.IdleNotifyInterval) * time.Second,
			MaxLineLength:      p.MaxLineLength,
			ConnLimit:          connLimiter,
			IDSend:             p.IDSend,
			LoginGreeting:      p.LoginGreeting,
			LogoutFormat:       p.LogoutFormat,
		})
	}

	// ---- POP3 ----
	var pop3Server *pop3svr.Server
	if svcs.POP3.Active() || svcs.POP3S.Active() {
		primary := firstActive(svcs.POP3S, svcs.POP3)
		var pop3TLS *tls.Config
		if svcs.POP3S.Active() {
			t, err := buildTLS(cfg, svcs.POP3S)
			if err != nil {
				return nil, fmt.Errorf("backend: POP3S TLS: %w", err)
			}
			pop3TLS = t
		}
		p := cfg.Protocol.POP3
		pop3Server = pop3svr.New(pop3svr.Options{
			Addr:               listenAddr(svcs.POP3S),
			AddrPlain:          listenAddr(svcs.POP3),
			TLSConfig:          pop3TLS,
			Mailbox:            mbox,
			Index:              idx,
			Auth:               authChain,
			ProxyProtocol:      primary.HAProxy,
			HAProxyTimeout:     haproxyTimeout,
			HAProxyTrustedNets: haproxyNets,
			XClient:            primary.XClient,
			XClientTrustedNets: xclientNets,
			DisablePlainAuth:   primary.DisablePlainAuth,
			NoFlagUpdates:      p.NoFlagUpdates,
			ReuseXUIDL:         p.ReuseXUIDL,
			UIDLFormat:         p.UIDLFormat,
			UIDLDuplicates:     p.UIDLDuplicates,
			EnableLast:         p.EnableLast,
			DeleteType:         p.DeleteType,
			DeletedFlag:        p.DeletedFlag,
			ConnLimit:          connLimiter,
		})
	}

	// ---- SMTP ----
	var smtpServer *smtpsvr.Server
	if svcs.SMTP.Active() || svcs.Submission.Active() || svcs.Submissions.Active() {
		// HAProxy/XClient settings come from whichever SMTP service is primary (MX → submission).
		primary := firstActive(svcs.SMTP, svcs.Submission, svcs.Submissions)
		// DisablePlainAuth applies to submission; use the submission service setting.
		subSvc := firstActive(svcs.Submission, svcs.Submissions)
		disablePlain := true
		if subSvc != nil {
			disablePlain = subSvc.DisablePlainAuth
		}

		var submissionProxy *smtpproxy.Submission
		if cfg.Protocol.SMTP.Relay.Host != "" {
			submissionProxy = smtpproxy.New(cfg.Protocol.SMTP.Relay, cfg.Protocol.SMTP.Hostname)
		}

		smtpServer = smtpsvr.New(smtpsvr.Options{
			HAProxy:          primary.HAProxy,
			HAProxyTimeout:   haproxyTimeout,
			HAProxyNets:      haproxyNets,
			XClient:          primary.XClient,
			XClientNets:      xclientNets,
			DisablePlainAuth: disablePlain,
			Config:           cfg.Protocol.SMTP,
			Auth:             chainAuth{authChain},
			Deliverer:        lmtp.New(mbox, idx),
			Proxy:            submissionProxy,
		})
	}

	// ---- telemetry ----
	telemAddr := cfg.Telemetry.Listen
	if telemAddr == "" {
		telemAddr = ":8080"
	}
	telem := telemetry.New(telemAddr)

	return &Server{
		cfg:   cfg,
		telem: telem,
		imap:  imapServer,
		pop3:  pop3Server,
		smtp:  smtpServer,
		index: idx,
	}, nil
}

// Run starts all configured servers and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	go func() {
		if err := s.telem.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry: server error", "err", err)
		}
	}()
	s.telem.SetReady(true)

	svcs := s.cfg.Services

	// IMAP
	if s.imap != nil {
		if svcs.IMAPS.Active() {
			go func() {
				if err := s.imap.ListenAndServeTLS(); err != nil {
					slog.Error("imap: TLS server error", "err", err)
					os.Exit(1)
				}
			}()
		}
		if svcs.IMAP.Active() {
			go func() {
				if err := s.imap.ListenAndServe(); err != nil {
					slog.Error("imap: plain server error", "err", err)
					os.Exit(1)
				}
			}()
		}
	}

	// POP3
	if s.pop3 != nil {
		if svcs.POP3S.Active() {
			go func() {
				if err := s.pop3.ListenAndServeTLS(); err != nil {
					slog.Error("pop3: TLS server error", "err", err)
					os.Exit(1)
				}
			}()
		}
		if svcs.POP3.Active() {
			go func() {
				if err := s.pop3.ListenAndServe(); err != nil {
					slog.Error("pop3: plain server error", "err", err)
					os.Exit(1)
				}
			}()
		}
	}

	// SMTP MX
	if s.smtp != nil && svcs.SMTP.Active() {
		go func() {
			ln, err := net.Listen("tcp", listenAddr(svcs.SMTP))
			if err != nil {
				slog.Error("smtp: MX listen error", "err", err)
				os.Exit(1)
			}
			if err := s.smtp.ServeMX(ln); err != nil {
				slog.Error("smtp: MX server error", "err", err)
				os.Exit(1)
			}
		}()
	}

	// Submission (STARTTLS)
	if s.smtp != nil && svcs.Submission.Active() {
		go func() {
			ln, err := net.Listen("tcp", listenAddr(svcs.Submission))
			if err != nil {
				slog.Error("smtp: submission listen error", "err", err)
				os.Exit(1)
			}
			var tlsCfg *tls.Config
			if svcs.Submission.SSLMode == "ssl" {
				t, err := buildTLS(s.cfg, svcs.Submission)
				if err != nil {
					slog.Error("smtp: submission TLS error", "err", err)
					os.Exit(1)
				}
				tlsCfg = t
			}
			if err := s.smtp.ServeSubmit(ln, tlsCfg); err != nil {
				slog.Error("smtp: submission server error", "err", err)
				os.Exit(1)
			}
		}()
	}

	// Submissions (SSL-only, port 465)
	if s.smtp != nil && svcs.Submissions.Active() {
		go func() {
			ln, err := net.Listen("tcp", listenAddr(svcs.Submissions))
			if err != nil {
				slog.Error("smtp: submissions listen error", "err", err)
				os.Exit(1)
			}
			tlsCfg, err := buildTLS(s.cfg, svcs.Submissions)
			if err != nil {
				slog.Error("smtp: submissions TLS error", "err", err)
				os.Exit(1)
			}
			if err := s.smtp.ServeSubmit(ln, tlsCfg); err != nil {
				slog.Error("smtp: submissions server error", "err", err)
				os.Exit(1)
			}
		}()
	}

	<-ctx.Done()
	s.index.Close() //nolint:errcheck
	return nil
}

// ---- helpers ----------------------------------------------------------------

// firstActive returns the first non-nil active ServiceConfig from the list.
func firstActive(svcs ...*config.ServiceConfig) *config.ServiceConfig {
	for _, s := range svcs {
		if s.Active() {
			return s
		}
	}
	return nil
}

// listenAddr converts a ServiceConfig into a TCP listen address.
func listenAddr(svc *config.ServiceConfig) string {
	if svc == nil {
		return ""
	}
	return fmt.Sprintf(":%d", svc.Port)
}

// buildTLS resolves TLS config for a service (merges general.ssl + per-service override).
func buildTLS(cfg *config.Config, svc *config.ServiceConfig) (*tls.Config, error) {
	ssl := cfg.ResolveSSL(svc)
	if ssl.TLSCert == "" {
		return nil, nil
	}
	return config.BuildTLSConfig(ssl)
}

func parseCIDRs(cidrs []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, s := range cidrs {
		_, ipnet, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("backend: invalid CIDR, skipping", "cidr", s, "err", err)
			continue
		}
		nets = append(nets, ipnet)
	}
	return nets
}

// chainAuth adapts protocol.Chain to smtp.Authenticator.
type chainAuth struct{ c protocol.Chain }

func (a chainAuth) AuthPlain(username, password string) error {
	resp, err := a.c.Authenticate(username, password, "smtp")
	if err != nil {
		return fmt.Errorf("smtp/auth: %w", err)
	}
	if resp == nil || resp.Result != protocol.AuthOK {
		return fmt.Errorf("smtp/auth: authentication failed")
	}
	return nil
}

func buildMailbox(cfg config.StorageConfig) (mailbox.MailboxBackend, error) {
	switch strings.ToLower(cfg.Mailbox) {
	case "dbox":
		return dbox.New(cfg.MaildirRoot)
	case "mdbox":
		return mdbox.New(cfg.MaildirRoot)
	default:
		return maildir.New(cfg.MaildirRoot)
	}
}

func buildPassdbs(entries []config.PassdbEntry) ([]protocol.Passdb, error) {
	var dbs []protocol.Passdb
	for _, e := range entries {
		switch strings.ToLower(e.Driver) {
		case "sqlite", "mysql", "postgres":
			db, err := authsql.New(e.Driver, e.DSN)
			if err != nil {
				return nil, fmt.Errorf("passdb %s: %w", e.Driver, err)
			}
			dbs = append(dbs, db)
		default:
			return nil, fmt.Errorf("unknown passdb driver: %s", e.Driver)
		}
	}
	return dbs, nil
}
