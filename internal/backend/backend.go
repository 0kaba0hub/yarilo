// Package backend wires all server components for the backend (or single) mode.
package backend

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	authsql "github.com/0kaba0hub/yarilo/internal/auth/sql"
	imapsvr "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/config"
)

// Server is the yarilo backend (or single-node) server.
type Server struct {
	cfg   *config.Config
	telem *telemetry.Server
	imap  *imapsvr.Server
	index *file.Backend
}

// New creates and wires all components according to cfg.
func New(cfg *config.Config) (*Server, error) {
	// ---- auth ----
	passdbs, err := buildPassdbs(cfg.Auth.Passdb)
	if err != nil {
		return nil, fmt.Errorf("backend: auth: %w", err)
	}
	authSrv := protocol.Chain(passdbs)

	// ---- storage ----
	if cfg.Storage.MaildirRoot == "" {
		cfg.Storage.MaildirRoot = "/var/mail/vhosts"
	}
	mbox, err := maildir.New(cfg.Storage.MaildirRoot)
	if err != nil {
		return nil, fmt.Errorf("backend: maildir: %w", err)
	}

	indexRoot := cfg.Storage.IndexDir
	if indexRoot == "" {
		indexRoot = cfg.Storage.MaildirRoot
	}
	idx := file.New(indexRoot)

	// ---- TLS ----
	var tlsCfg *tls.Config
	if cfg.IMAP.TLSCert != "" && cfg.IMAP.TLSKey != "" {
		cert, err := tls.LoadX509KeyPair(cfg.IMAP.TLSCert, cfg.IMAP.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("backend: TLS: %w", err)
		}
		tlsCfg = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	// ---- IMAP ----
	imapAddr := cfg.IMAP.Listen
	if imapAddr == "" {
		imapAddr = ":993"
	}
	imap := imapsvr.New(imapsvr.Options{
		Addr:      imapAddr,
		AddrPlain: cfg.IMAP.ListenPlain,
		TLSConfig: tlsCfg,
		Mailbox:   mbox,
		Index:     idx,
		Auth:      authSrv,
	})

	// ---- telemetry ----
	telemAddr := cfg.Telemetry.Listen
	if telemAddr == "" {
		telemAddr = ":8080"
	}
	telem := telemetry.New(telemAddr)

	return &Server{
		cfg:   cfg,
		telem: telem,
		imap:  imap,
		index: idx,
	}, nil
}

// Run starts all servers. Blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	// Auth socket
	go func() {
		slog.Info("backend: auth socket not yet bound (single-node skips it)")
	}()

	// Telemetry
	go func() {
		if err := s.telem.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry: server error", "err", err)
		}
	}()

	s.telem.SetReady(true)

	// IMAP
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

	<-ctx.Done()
	s.index.Close() //nolint:errcheck
	return nil
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
