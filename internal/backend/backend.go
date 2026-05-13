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
	"github.com/0kaba0hub/yarilo/internal/dkim"
	imapsvr "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/lmtp"
	smtpsvr "github.com/0kaba0hub/yarilo/internal/smtp"
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
	cfg     *config.Config
	telem   *telemetry.Server
	imap    *imapsvr.Server
	smtp    *smtpsvr.Server
	smtpTLS *tls.Config
	index   *file.Backend
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

	// ---- TLS configs ----
	var imapTLS, smtpTLS *tls.Config
	if cfg.IMAP.TLSCert != "" && cfg.IMAP.TLSKey != "" {
		t, err := config.BuildTLSConfig(
			cfg.IMAP.TLSCert, cfg.IMAP.TLSKey,
			cfg.IMAP.TLSAltCert, cfg.IMAP.TLSAltKey,
			cfg.IMAP.TLSMinVersion, cfg.IMAP.TLSPreferServer,
		)
		if err != nil {
			return nil, fmt.Errorf("backend: IMAP TLS: %w", err)
		}
		imapTLS = t
	}
	if cfg.SMTP.TLSCert != "" && cfg.SMTP.TLSKey != "" {
		t, err := config.BuildTLSConfig(
			cfg.SMTP.TLSCert, cfg.SMTP.TLSKey,
			cfg.SMTP.TLSAltCert, cfg.SMTP.TLSAltKey,
			cfg.SMTP.TLSMinVersion, cfg.SMTP.TLSPreferServer,
		)
		if err != nil {
			return nil, fmt.Errorf("backend: SMTP TLS: %w", err)
		}
		smtpTLS = t
	}

	// ---- IMAP ----
	imapAddr := cfg.IMAP.Listen
	if imapAddr == "" {
		imapAddr = ":993"
	}
	imap := imapsvr.New(imapsvr.Options{
		Addr:             imapAddr,
		AddrPlain:        cfg.IMAP.ListenPlain,
		TLSConfig:        imapTLS,
		Mailbox:          mbox,
		Index:            idx,
		Auth:             authChain,
		ProxyProtocol:    cfg.IMAP.ProxyProtocol,
		HAProxyTimeout:   time.Duration(cfg.IMAP.HAProxyTimeout) * time.Second,
		DisablePlainAuth: cfg.IMAP.DisablePlainAuth,
	})

	// ---- DKIM key provider ----
	var keyProv dkim.KeyProvider
	if cfg.DKIM.Sign {
		switch strings.ToLower(cfg.DKIM.Keys.Backend) {
		case "dynamic":
			d := cfg.DKIM.Keys.Dynamic
			ttl := time.Duration(d.CacheTTL) * time.Second
			if ttl == 0 {
				ttl = 5 * time.Minute
			}
			kp, err := dkim.NewSQLKeyProvider(d.Driver, d.DSN, d.Query, ttl)
			if err != nil {
				return nil, fmt.Errorf("backend: DKIM SQL key provider: %w", err)
			}
			keyProv = kp
		default: // static
			keyProv = dkim.NewStaticKeyProvider(cfg.DKIM.Keys.Static)
		}
	}

	// ---- milter clients ----
	var milters []*smtpsvr.MilterClient
	for _, mc := range cfg.SMTP.Milters {
		c, err := smtpsvr.NewMilterClient(mc.Socket, mc.Timeout)
		if err != nil {
			return nil, fmt.Errorf("backend: milter %s: %w", mc.Socket, err)
		}
		milters = append(milters, c)
	}

	// ---- SMTP ----
	smtp := smtpsvr.New(smtpsvr.Options{
		Config:    cfg.SMTP,
		DKIMCfg:   cfg.DKIM,
		SPFCfg:    cfg.SPF,
		DMARCCfg:  cfg.DMARC,
		Auth:      chainAuth{authChain},
		KeyProv:   keyProv,
		Deliverer: lmtp.New(mbox, idx),
		Milters:   milters,
	})

	// ---- telemetry ----
	telemAddr := cfg.Telemetry.Listen
	if telemAddr == "" {
		telemAddr = ":8080"
	}
	telem := telemetry.New(telemAddr)

	return &Server{
		cfg:     cfg,
		telem:   telem,
		imap:    imap,
		smtp:    smtp,
		smtpTLS: smtpTLS,
		index:   idx,
	}, nil
}

// Run starts all servers. Blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	go func() {
		if err := s.telem.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry: server error", "err", err)
		}
	}()
	s.telem.SetReady(true)

	if s.cfg.IMAP.TLSCert != "" {
		go func() {
			if err := s.imap.ListenAndServeTLS(); err != nil {
				slog.Error("imap: TLS server error", "err", err)
				os.Exit(1)
			}
		}()
	}
	if s.cfg.IMAP.ListenPlain != "" {
		go func() {
			if err := s.imap.ListenAndServe(); err != nil {
				slog.Error("imap: plain server error", "err", err)
				os.Exit(1)
			}
		}()
	}

	mxAddr := s.cfg.SMTP.ListenMX
	if mxAddr == "" {
		mxAddr = ":25"
	}
	go func() {
		ln, err := net.Listen("tcp", mxAddr)
		if err != nil {
			slog.Error("smtp: MX listen error", "err", err)
			os.Exit(1)
		}
		if err := s.smtp.ServeMX(ln); err != nil {
			slog.Error("smtp: MX server error", "err", err)
			os.Exit(1)
		}
	}()

	subAddr := s.cfg.SMTP.ListenSubmit
	if subAddr == "" {
		subAddr = ":587"
	}
	go func() {
		ln, err := net.Listen("tcp", subAddr)
		if err != nil {
			slog.Error("smtp: submission listen error", "err", err)
			os.Exit(1)
		}
		if err := s.smtp.ServeSubmit(ln, s.smtpTLS); err != nil {
			slog.Error("smtp: submission server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	s.index.Close() //nolint:errcheck
	return nil
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
