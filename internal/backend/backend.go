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
	"sync"
	"time"

	_ "github.com/0kaba0hub/yarilo/pkg/dict/drivers/all" // register all dict drivers

	"github.com/emersion/go-sasl"

	"github.com/0kaba0hub/yarilo/internal/auth/oauth2"
	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	authsql "github.com/0kaba0hub/yarilo/internal/auth/sql"
	"github.com/0kaba0hub/yarilo/internal/connlimit"
	imapsvr "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/lmtp"
	mssvr "github.com/0kaba0hub/yarilo/internal/managesieve"
	pop3svr "github.com/0kaba0hub/yarilo/internal/pop3"
	"github.com/0kaba0hub/yarilo/internal/sieve"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
	submsvr "github.com/0kaba0hub/yarilo/internal/submission"
	submproxy "github.com/0kaba0hub/yarilo/internal/submission/proxy"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
	authclient "github.com/0kaba0hub/yarilo/pkg/authclient"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
	"github.com/0kaba0hub/yarilo/pkg/quota"
)

// Server is the yarilo backend (or single-node) server.
type Server struct {
	cfg         *config.Config
	telem       *telemetry.Server
	imap        *imapsvr.Server // nil if neither IMAP nor IMAPS is active
	pop3        *pop3svr.Server // nil if neither POP3 nor POP3S is active
	submission  *submsvr.Server // nil if no Submission/Submissions is active
	lmtp        *lmtp.Server    // nil if LMTP not configured
	managesieve *mssvr.Server   // nil if ManageSieve not configured
	locker      locks.Locker    // cross-process write coordinator; nil = disabled
}

// Close releases backend resources. Session binaries should defer Close after
// backend.New for clean lock and dict release.
func (s *Server) Close() error {
	if s.locker != nil {
		return s.locker.Close()
	}
	return nil
}

// New creates and wires all components according to cfg.
func New(cfg *config.Config) (*Server, error) {
	// ---- auth ----
	passdbs, err := buildPassdbs(cfg.Auth.Passdb)
	if err != nil {
		return nil, fmt.Errorf("backend: auth: %w", err)
	}
	// OAuth2 passdbs join the chain ahead of SQL so an OAUTHBEARER
	// login resolves locally before SQL ever sees the bearer token
	// as a plaintext "password". When no providers are configured
	// this is a no-op and the chain stays SQL-only.
	if len(cfg.Auth.OAuth2) > 0 {
		oauth2pdbs, err := oauth2.BuildPassdbs(context.Background(), cfg.Auth.OAuth2)
		if err != nil {
			return nil, fmt.Errorf("backend: oauth2: %w", err)
		}
		passdbs = append(oauth2pdbs, passdbs...)
	}
	authCache := protocol.NewCache(
		cfg.Auth.Cache.SizeBytes,
		time.Duration(cfg.Auth.Cache.TTLSeconds)*time.Second,
		time.Duration(cfg.Auth.Cache.NegativeTTLSeconds)*time.Second,
	)
	authOpts := []protocol.AuthenticatorOption{
		protocol.WithAuthenticatorCache(authCache),
	}
	if cfg.Auth.MasterUsers.Enabled {
		masterdbs, err := buildPassdbs(cfg.Auth.MasterUsers.Masterdb)
		if err != nil {
			return nil, fmt.Errorf("backend: masterdb: %w", err)
		}
		authOpts = append(authOpts,
			protocol.WithAuthenticatorMasterUsers(true),
			protocol.WithAuthenticatorMasterdb(masterdbs),
			protocol.WithAuthenticatorMasterUserSeparator(cfg.Auth.MasterUsers.Separator),
		)
	}
	authChain := protocol.NewAuthenticator(passdbs, authOpts...)

	// ---- storage ----
	if cfg.Storage.MaildirRoot == "" {
		cfg.Storage.MaildirRoot = "/var/mail/vhosts"
	}
	if cfg.Storage.MailHomeTemplate == "" {
		cfg.Storage.MailHomeTemplate = "%d/%u"
	}
	resolver := &mailbox.Resolver{
		Root:               cfg.Storage.MaildirRoot,
		HomeTemplate:       cfg.Storage.MailHomeTemplate,
		DefaultVolatileDir: cfg.Storage.VolatileDir,
		DefaultIndexDir:    cfg.Storage.IndexDir,
		DefaultControlDir:  cfg.Storage.ControlDir,
		DefaultAltDir:      cfg.Storage.AltDir,
	}
	locker, err := buildLocksClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("backend: locks_client: %w", err)
	}
	mbox := buildMailbox(cfg.Storage, locker)

	// ---- per-namespace mailbox driver overrides ----
	// Builds a separate MailboxBackend instance per distinct
	// non-default driver referenced from cfg.Namespaces[*].Location.
	// Namespaces using the global driver are absent from the map and
	// resolve at session-open time to the global mbox.
	nsMailboxes, err := buildNamespaceMailboxes(cfg.Namespaces, cfg.Storage.Mailbox, cfg.Storage.MdboxAltStoragePath, locker)
	if err != nil {
		return nil, fmt.Errorf("backend: namespace mailboxes: %w", err)
	}

	// ---- dicts ----
	metadataDict, err := buildDict(cfg.Dicts, "metadata")
	if err != nil {
		return nil, fmt.Errorf("backend: dicts.metadata: %w", err)
	}
	quotaDict, err := buildDict(cfg.Dicts, "quota")
	if err != nil {
		return nil, fmt.Errorf("backend: dicts.quota: %w", err)
	}
	idxOpts := []file.Option{file.WithLocker(locker)}
	if quotaDict != nil {
		idxOpts = append(idxOpts, file.WithQuotaCounter(func(u *mailbox.UserInfo) (*quota.Counter, quota.Limits) {
			return quota.NewCounter(quotaDict, u.Username), quota.ParseRules(u.QuotaRules)
		}))
	}
	idx := file.New(idxOpts...)

	// ---- shared connection limiter (IMAP + POP3) ----
	connLimiter := connlimit.New(cfg.General.Limits.MaxUserIPConnections)

	// ---- HAProxy shared nets ----
	haproxyNets := parseCIDRs(cfg.General.HAProxy.TrustedNets)
	haproxyTimeout := time.Duration(cfg.General.HAProxy.Timeout) * time.Second
	authAddr := cfg.AuthService.ClientAddr()
	masterAddr := cfg.AuthService.MasterAddr
	var authTLS *tls.Config
	if cfg.InternalTLS.Enabled {
		t, err := mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
		if err != nil {
			return nil, fmt.Errorf("backend: auth_service mtls: %w", err)
		}
		authTLS = t
	}

	// ---- sieve ----
	svcs := cfg.Services
	var sieveEngine *sieve.Engine
	if cfg.Sieve.Enabled {
		sieveEngine = sieve.New(cfg.Sieve, locker)
	}

	// ---- IMAP ----
	var imapServer *imapsvr.Server
	if svcs.IMAP.Active() || svcs.IMAPS.Active() {
		primary := firstActive(svcs.IMAPS, svcs.IMAP)
		var imapTLS *tls.Config
		if svcs.IMAPS.Active() {
			t, err := buildTLS(cfg, svcs.IMAPS, alpnIMAP)
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
			Resolver:           resolver,
			Auth:               authChain,
			ProxyProtocol:      primary.HAProxy,
			HAProxyTimeout:     haproxyTimeout,
			HAProxyTrustedNets: haproxyNets,
			AuthAddr:           authAddr,
			AuthTLS:            authTLS,
			MasterAddr:         masterAddr,
			MasterTLS:          authTLS,
			IdleNotifyInterval: time.Duration(p.IdleNotifyInterval) * time.Second,
			MaxLineLength:      p.MaxLineLength,
			ConnLimit:          connLimiter,
			IDSend:             p.IDSend,
			LoginGreeting:      p.LoginGreeting,
			LogoutFormat:       p.LogoutFormat,
			ClientWorkarounds:  imapsvr.ParseIMAPWorkarounds(p.ClientWorkarounds),
			Locker:             locker,
			SpecialUseDefaults: p.SpecialUseDefaults,
			MetadataDict:       metadataDict,
			QuotaDict:          quotaDict,
			ACLEnabled:         p.ACL.Enabled,
			Namespaces:         buildNamespaces(cfg.Namespaces),
			NamespaceMailboxes: nsMailboxes,
			FailureDelay:       time.Duration(cfg.Auth.FailureDelaySeconds) * time.Second,
		})
	}

	// ---- POP3 ----
	var pop3Server *pop3svr.Server
	if svcs.POP3.Active() || svcs.POP3S.Active() {
		primary := firstActive(svcs.POP3S, svcs.POP3)
		var pop3TLS *tls.Config
		if svcs.POP3S.Active() {
			t, err := buildTLS(cfg, svcs.POP3S, alpnPOP3)
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
			Resolver:           resolver,
			Auth:               authChain,
			ProxyProtocol:      primary.HAProxy,
			HAProxyTimeout:     haproxyTimeout,
			HAProxyTrustedNets: haproxyNets,
			AuthAddr:           authAddr,
			AuthTLS:            authTLS,
			MasterAddr:         masterAddr,
			MasterTLS:          authTLS,
			NoFlagUpdates:      p.NoFlagUpdates,
			ReuseXUIDL:         p.ReuseXUIDL,
			UIDLFormat:         p.UIDLFormat,
			UIDLDuplicates:     p.UIDLDuplicates,
			EnableLast:         p.EnableLast,
			DeleteType:         p.DeleteType,
			DeletedFlag:        p.DeletedFlag,
			SaveUIDL:           p.SaveUIDL,
			LockSession:        p.LockSession,
			ConnLimit:          connLimiter,
			Locker:             locker,
			FailureDelay:       time.Duration(cfg.Auth.FailureDelaySeconds) * time.Second,
		})
	}

	// ---- SMTP submission ----
	var smtpServer *submsvr.Server
	if svcs.Submission.Active() || svcs.Submissions.Active() {
		primary := firstActive(svcs.Submission, svcs.Submissions)

		var submissionProxy *submproxy.Submission
		if cfg.Protocol.Submission.Relay.Host != "" {
			submissionProxy = submproxy.New(cfg.Protocol.Submission.Relay, cfg.Protocol.Submission.Hostname)
		}

		var submissionTLS *tls.Config
		if primary.SSLMode != "no" && primary.SSLMode != "" {
			t, err := buildTLS(cfg, primary, alpnSMTP)
			if err != nil {
				return nil, fmt.Errorf("backend: submission TLS: %w", err)
			}
			submissionTLS = t
		}

		smtpServer = submsvr.New(submsvr.Options{
			HAProxy:        primary.HAProxy,
			HAProxyTimeout: haproxyTimeout,
			HAProxyNets:    haproxyNets,
			AuthAddr:       authAddr,
			AuthTLS:        authTLS,
			TLSConfig:      submissionTLS,
			Config:         cfg.Protocol.Submission,
			Auth:           chainAuth{authChain},
			Proxy:          submissionProxy,
			FailureDelay:   time.Duration(cfg.Auth.FailureDelaySeconds) * time.Second,
		})
	}

	// ---- LMTP ----
	var lmtpServer *lmtp.Server
	if svcs.LMTP.Active() {
		var lmtpTLS *tls.Config
		if svcs.LMTP.SSLMode == "starttls" {
			t, err := buildTLS(cfg, svcs.LMTP)
			if err != nil {
				return nil, fmt.Errorf("backend: LMTP STARTTLS: %w", err)
			}
			lmtpTLS = t
		}
		lmtpOpts := lmtp.Options{
			Hostname:    cfg.Protocol.Submission.Hostname,
			Config:      cfg.Protocol.LMTP,
			Mailbox:     mbox,
			Index:       idx,
			Resolver:    resolver,
			TLSConfig:   lmtpTLS,
			Locker:      locker,
			QuotaDict:   quotaDict,
			AuthAddr:    authAddr,
			AuthTLS:     authTLS,
			SieveEngine: sieveEngine,
		}
		if addr := cfg.AuthService.MasterAddr; addr != "" {
			ac, err := authclient.Dial(addr, nil)
			if err != nil {
				return nil, fmt.Errorf("backend: lmtp userdb dial %s: %w", addr, err)
			}
			var acMu sync.Mutex
			lmtpResolver := lmtpOpts.Resolver
			if lmtpResolver == nil {
				lmtpResolver = &mailbox.Resolver{}
			}
			lmtpOpts.UserdbLookup = func(ctx context.Context, username string) (*mailbox.UserInfo, error) {
				acMu.Lock()
				cur := ac
				acMu.Unlock()

				ui, err := cur.Userdb(ctx, username)
				if err != nil {
					acMu.Lock()
					if ac == cur {
						_ = ac.Close()
						fresh, dialErr := authclient.Dial(addr, nil)
						if dialErr != nil {
							acMu.Unlock()
							slog.Warn("lmtp: userdb auth reconnect failed", "addr", addr, "err", dialErr)
							return nil, err
						}
						ac = fresh
						slog.Info("lmtp: userdb auth reconnected", "addr", addr)
					}
					cur = ac
					acMu.Unlock()
					ui, err = cur.Userdb(ctx, username)
					if err != nil {
						return nil, err
					}
				}
				if ui == nil {
					return nil, nil
				}
				mbi := lmtpResolver.UserInfo(username, ui.Home)
				mbi.Groups = ui.Groups
				mbi.QuotaRules = ui.QuotaRules
				if ui.VolatileDir != "" {
					vd := strings.ReplaceAll(ui.VolatileDir, "%h", mbi.Home)
					mbi.VolatileDir = mailbox.ExpandVars(vd, username)
				}
				if ui.IndexDir != "" {
					id := strings.ReplaceAll(ui.IndexDir, "%h", mbi.Home)
					mbi.IndexDir = mailbox.ExpandVars(id, username)
				}
				if ui.ControlDir != "" {
					cd := strings.ReplaceAll(ui.ControlDir, "%h", mbi.Home)
					mbi.ControlDir = mailbox.ExpandVars(cd, username)
				}
				if ui.AltDir != "" {
					ad := strings.ReplaceAll(ui.AltDir, "%h", mbi.Home)
					mbi.AltDir = mailbox.ExpandVars(ad, username)
				}
				return mbi, nil
			}
		}
		lmtpServer = lmtp.New(lmtpOpts)
	}

	// ---- ManageSieve ----
	var msServer *mssvr.Server
	if svcs.ManageSieveBE.Active() {
		msServer = mssvr.New(mssvr.Options{
			Locker:      locker,
			DefaultName: cfg.Sieve.DefaultName,
			Resolver:    resolver,
			Config:      cfg.Protocol.ManageSieve,
			AuthAddr:    authAddr,
			AuthTLS:     authTLS,
			MasterAddr:  masterAddr,
			MasterTLS:   authTLS,
		})
	}

	// ---- telemetry ----
	telemAddr := cfg.Telemetry.Listen
	if telemAddr == "" {
		telemAddr = ":8080"
	}
	telem := telemetry.New(telemAddr)

	return &Server{
		cfg:         cfg,
		telem:       telem,
		imap:        imapServer,
		pop3:        pop3Server,
		submission:  smtpServer,
		lmtp:        lmtpServer,
		managesieve: msServer,
		locker:      locker,
	}, nil
}

// RunIMAP starts the IMAP/IMAPS listeners and telemetry, then blocks until ctx is cancelled.
func (s *Server) RunIMAP(ctx context.Context) error {
	go func() {
		if err := s.telem.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry: server error", "err", err)
		}
	}()
	s.telem.SetReady(true)

	svcs := s.cfg.Services
	if s.imap == nil {
		slog.Warn("imap: no listeners configured")
		<-ctx.Done()
		return nil
	}
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
	<-ctx.Done()
	return nil
}

// RunPOP3 starts the POP3/POP3S listeners and telemetry, then blocks until ctx is cancelled.
func (s *Server) RunPOP3(ctx context.Context) error {
	go func() {
		if err := s.telem.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry: server error", "err", err)
		}
	}()
	s.telem.SetReady(true)

	svcs := s.cfg.Services
	if s.pop3 == nil {
		slog.Warn("pop3: no listeners configured")
		<-ctx.Done()
		return nil
	}
	if svcs.POP3S.Active() {
		go func() {
			err := s.pop3.ListenAndServeTLS()
			slog.Error("pop3: TLS server error", "err", err)
			os.Exit(1)
		}()
	}
	if svcs.POP3.Active() {
		go func() {
			err := s.pop3.ListenAndServe()
			slog.Error("pop3: plain server error", "err", err)
			os.Exit(1)
		}()
	}
	<-ctx.Done()
	return nil
}

// RunLMTP starts the LMTP listener and telemetry, then blocks until ctx is cancelled.
func (s *Server) RunLMTP(ctx context.Context) error {
	go func() {
		if err := s.telem.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry: server error", "err", err)
		}
	}()
	s.telem.SetReady(true)

	svcs := s.cfg.Services
	if s.lmtp == nil || !svcs.LMTP.Active() {
		slog.Warn("lmtp: no listener configured")
		<-ctx.Done()
		return nil
	}
	go func() {
		ln, err := net.Listen("tcp", listenAddr(svcs.LMTP))
		if err != nil {
			slog.Error("lmtp: listen error", "err", err)
			os.Exit(1)
		}
		if svcs.LMTP.SSLMode == "ssl" {
			tlsCfg, err := buildTLS(s.cfg, svcs.LMTP)
			if err != nil {
				slog.Error("lmtp: TLS error", "err", err)
				os.Exit(1)
			}
			if tlsCfg != nil {
				ln = tls.NewListener(ln, tlsCfg)
			}
		}
		if err := s.lmtp.Serve(ln); err != nil {
			slog.Error("lmtp: server error", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	return nil
}

// RunManageSieve starts the ManageSieve backend listener and telemetry,
// then blocks until ctx is cancelled.
func (s *Server) RunManageSieve(ctx context.Context) error {
	go func() {
		if err := s.telem.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry: server error", "err", err)
		}
	}()
	s.telem.SetReady(true)

	svcs := s.cfg.Services
	if s.managesieve == nil || !svcs.ManageSieveBE.Active() {
		slog.Warn("managesieve: no listener configured")
		<-ctx.Done()
		return nil
	}
	go func() {
		ln, err := net.Listen("tcp", listenAddr(svcs.ManageSieveBE))
		if err != nil {
			slog.Error("managesieve: listen error", "err", err)
			os.Exit(1)
		}
		if err := s.managesieve.ServeManageSieve(ctx, ln); err != nil {
			slog.Error("managesieve: server error", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	return nil
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
				err := s.pop3.ListenAndServeTLS()
				slog.Error("pop3: TLS server error", "err", err)
				os.Exit(1)
			}()
		}
		if svcs.POP3.Active() {
			go func() {
				err := s.pop3.ListenAndServe()
				slog.Error("pop3: plain server error", "err", err)
				os.Exit(1)
			}()
		}
	}

	// Submission (STARTTLS)
	if s.submission != nil && svcs.Submission.Active() {
		go func() {
			ln, err := net.Listen("tcp", listenAddr(svcs.Submission))
			if err != nil {
				slog.Error("smtp: submission listen error", "err", err)
				os.Exit(1)
			}
			var tlsCfg *tls.Config
			if svcs.Submission.SSLMode == "ssl" {
				t, err := buildTLS(s.cfg, svcs.Submission, alpnSMTP)
				if err != nil {
					slog.Error("smtp: submission TLS error", "err", err)
					os.Exit(1)
				}
				tlsCfg = t
			}
			if err := s.submission.Serve(ln, tlsCfg); err != nil {
				slog.Error("smtp: submission server error", "err", err)
				os.Exit(1)
			}
		}()
	}

	// LMTP (port 24) — local delivery for external MTAs
	if s.lmtp != nil && svcs.LMTP.Active() {
		go func() {
			ln, err := net.Listen("tcp", listenAddr(svcs.LMTP))
			if err != nil {
				slog.Error("lmtp: listen error", "err", err)
				os.Exit(1)
			}
			if svcs.LMTP.SSLMode == "ssl" {
				tlsCfg, err := buildTLS(s.cfg, svcs.LMTP)
				if err != nil {
					slog.Error("lmtp: TLS error", "err", err)
					os.Exit(1)
				}
				if tlsCfg != nil {
					ln = tls.NewListener(ln, tlsCfg)
				}
			}
			if err := s.lmtp.Serve(ln); err != nil {
				slog.Error("lmtp: server error", "err", err)
				os.Exit(1)
			}
		}()
	}

	// Submissions (SSL-only, port 465)
	if s.submission != nil && svcs.Submissions.Active() {
		go func() {
			ln, err := net.Listen("tcp", listenAddr(svcs.Submissions))
			if err != nil {
				slog.Error("smtp: submissions listen error", "err", err)
				os.Exit(1)
			}
			tlsCfg, err := buildTLS(s.cfg, svcs.Submissions, alpnSMTP)
			if err != nil {
				slog.Error("smtp: submissions TLS error", "err", err)
				os.Exit(1)
			}
			if err := s.submission.Serve(ln, tlsCfg); err != nil {
				slog.Error("smtp: submissions server error", "err", err)
				os.Exit(1)
			}
		}()
	}

	<-ctx.Done()
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

// buildTLS resolves TLS config for a service (merges general.ssl + per-service override)
// and sets ALPN protocols matching the protocol (IANA RFC 7301 names).
// Clients sending ALPN must match one of the listed protocols; clients without
// ALPN are accepted (Dovecot 2.4 semantics).
func buildTLS(cfg *config.Config, svc *config.ServiceConfig, alpn ...string) (*tls.Config, error) {
	ssl := cfg.ResolveSSL(svc)
	if ssl.TLSCert == "" {
		return nil, nil
	}
	tlsCfg, err := config.BuildTLSConfig(ssl)
	if err != nil {
		return nil, err
	}
	if len(alpn) > 0 {
		tlsCfg.NextProtos = alpn
	}
	return tlsCfg, nil
}

// ALPN protocol identifiers (IANA RFC 7301 registry).
const (
	alpnIMAP = "imap"
	alpnPOP3 = "pop3"
	alpnSMTP = "smtp"
)

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

// chainAuth adapts protocol.Authenticator to smtp.Authenticator.
// The wrapper exists because go-smtp's Authenticator surface speaks
// (username, password) → error rather than the richer
// AuthResponse / Fields shape; here we discard everything except
// the "did the chain accept these credentials" decision.
type chainAuth struct{ c protocol.Authenticator }

func (a chainAuth) AuthPlain(username, password string) error {
	resp, err := a.c.Authenticate(username, password, "smtp", "")
	if err != nil {
		return fmt.Errorf("smtp/auth: %w", err)
	}
	if resp == nil || resp.Result != protocol.AuthOK {
		return fmt.Errorf("smtp/auth: authentication failed")
	}
	return nil
}

// AuthPlainMaster forwards a SASL PLAIN response carrying an
// authzid to the underlying protocol chain via
// MasterAuthenticator. When the underlying chain does not
// implement MasterAuthenticator (e.g. a test stub or a driver
// that pre-dates AUTH-3) the call fails opaquely — the wire
// reply stays indistinguishable from a wrong-password rejection.
func (a chainAuth) AuthPlainMaster(authzid, authid, password string) error {
	master, ok := a.c.(protocol.MasterAuthenticator)
	if !ok {
		return fmt.Errorf("smtp/auth: authentication failed")
	}
	resp, err := master.AuthenticateMaster(authzid, authid, password, "smtp", "")
	if err != nil {
		return fmt.Errorf("smtp/auth: %w", err)
	}
	if resp == nil || resp.Result != protocol.AuthOK {
		return fmt.Errorf("smtp/auth: authentication failed")
	}
	return nil
}

// LookupSCRAMSha256 satisfies submission.SCRAMSha256LookupAuthenticator
// by forwarding to the underlying protocol chain. Returns
// (nil, nil) when the chain does not expose SCRAM verifiers,
// which the submission session interprets as "do not advertise
// SCRAM mechs in EHLO" so a deployment without verifiers never
// surfaces a mech it cannot serve.
func (a chainAuth) LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error) {
	lookup, ok := a.c.(protocol.SCRAMSha256Lookup)
	if !ok {
		return nil, nil
	}
	return lookup.LookupSCRAMSha256(username)
}

// LookupSCRAMSha1 is the SHA-1 counterpart of LookupSCRAMSha256.
func (a chainAuth) LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error) {
	lookup, ok := a.c.(protocol.SCRAMSha1Lookup)
	if !ok {
		return nil, nil
	}
	return lookup.LookupSCRAMSha1(username)
}

func buildMailbox(cfg config.StorageConfig, locker locks.Locker) mailbox.MailboxBackend {
	return buildMailboxByDriver(cfg.Mailbox, cfg.MdboxAltStoragePath, locker)
}

// buildMailboxByDriver constructs a MailboxBackend for the named
// driver. Defaults to maildir for unknown / empty drivers so an
// operator's typo does not crash startup. Reused by buildMailbox
// (global default from cfg.Storage.Mailbox) and by
// buildNamespaceMailboxes (per-namespace override from
// cfg.Namespaces[*].Location).
func buildMailboxByDriver(driver, mdboxAltPath string, locker locks.Locker) mailbox.MailboxBackend {
	switch strings.ToLower(driver) {
	case "sdbox", "dbox":
		return dboxv2.New(dboxv2.WithLocker(locker))
	case "mdbox":
		return mdbox.New(mdbox.WithLocker(locker), mdbox.WithAltStorage(mdboxAltPath))
	default:
		return maildir.New(maildir.WithLocker(locker))
	}
}

// buildNamespaceMailboxes constructs the per-namespace MailboxBackend
// override map for cfg.Namespaces. A namespace declared with a
// `location:` URL whose driver prefix differs from cfg.Storage.Mailbox
// gets its own backend instance; namespaces without a location: or
// using the same driver as the global default are absent from the map
// and resolve at session-open time to the global backend.
//
// The override map is keyed by namespace prefix (same key the IMAP
// session dispatcher uses). Same-driver namespaces share their
// Backend instance to keep the in-memory footprint small.
func buildNamespaceMailboxes(namespaces []config.NamespaceConfig, globalDriver, mdboxAltPath string, locker locks.Locker) (map[string]mailbox.MailboxBackend, error) {
	if len(namespaces) == 0 {
		return nil, nil
	}
	globalDriver = strings.ToLower(globalDriver)
	if globalDriver == "" {
		globalDriver = "maildir"
	}
	byDriver := make(map[string]mailbox.MailboxBackend) // shared per driver string
	overrides := map[string]mailbox.MailboxBackend{}
	for _, ns := range namespaces {
		if ns.Location == "" {
			// Personal-style namespace inheriting the global default.
			continue
		}
		loc, ok, err := mailbox.ParseLocation(ns.Location, nil)
		if err != nil {
			return nil, fmt.Errorf("namespace %q: %w", ns.Prefix, err)
		}
		if !ok {
			continue
		}
		drv := strings.ToLower(loc.Driver)
		if drv == globalDriver {
			// Same driver as global default — no override needed; the
			// session opens against the global Mailbox backend.
			continue
		}
		b, exists := byDriver[drv]
		if !exists {
			b = buildMailboxByDriver(drv, mdboxAltPath, locker)
			byDriver[drv] = b
			slog.Info("backend: per-namespace mailbox backend built", "driver", drv, "ns", ns.Prefix)
		}
		overrides[ns.Prefix] = b
	}
	if len(overrides) == 0 {
		return nil, nil
	}
	return overrides, nil
}

// buildNamespaces translates cfg.Namespaces into the wire-protocol
// shape the IMAP server consumes. An empty slice in => empty slice
// out; the server applies its built-in single-personal-namespace
// fallback so pre-v1.20 deployments without a namespaces: block keep
// working unchanged.
//
// Separator defaults to "/" when omitted; non-single-rune values are
// dropped to "/" with a warning so a misconfigured yaml does not
// produce a malformed NAMESPACE response.
func buildNamespaces(cfg []config.NamespaceConfig) []imapsvr.NamespaceSpec {
	if len(cfg) == 0 {
		return nil
	}
	out := make([]imapsvr.NamespaceSpec, 0, len(cfg))
	for i, ns := range cfg {
		t := strings.ToLower(strings.TrimSpace(ns.Type))
		var nsType imapsvr.NamespaceType
		switch t {
		case "personal":
			nsType = imapsvr.NamespacePersonal
		case "other", "other_users":
			nsType = imapsvr.NamespaceOther
		case "shared":
			nsType = imapsvr.NamespaceShared
		default:
			slog.Warn("backend: skipping namespace with unknown type",
				"index", i, "type", ns.Type, "prefix", ns.Prefix)
			continue
		}
		sep := '/'
		if rs := []rune(ns.Separator); len(rs) == 1 {
			sep = rs[0]
		} else if ns.Separator != "" {
			slog.Warn("backend: namespace separator must be a single character, defaulting to /",
				"index", i, "separator", ns.Separator)
		}
		out = append(out, imapsvr.NamespaceSpec{
			Type:      nsType,
			Prefix:    ns.Prefix,
			Separator: sep,
			List:      ns.List,
			Location:  ns.Location,
		})
	}
	return out
}

// buildDict opens the named dict from cfg.Dicts, returning nil when the
// entry is absent. The caller decides whether nil is acceptable —
// IMAP METADATA tolerates a nil dict (the feature degrades to "Metadata
// storage not configured"); other consumers may require a non-nil
// result and error out at startup.
func buildDict(dicts map[string]config.DictConfig, name string) (dict.Dict, error) {
	cfg, ok := dicts[name]
	if !ok {
		return nil, nil
	}
	if cfg.Driver == "" {
		return nil, fmt.Errorf("dict %q has empty driver", name)
	}
	d, err := dict.Open(dict.Config{Driver: cfg.Driver, Settings: cfg.Settings})
	if err != nil {
		return nil, fmt.Errorf("open dict %q: %w", name, err)
	}
	slog.Info("backend: dict opened", "name", name, "driver", cfg.Driver)
	return d, nil
}

// buildLocksClient constructs a yarilo-locks client per cfg.LocksClient.
// Returns (nil, nil) when locks are disabled (Mode == ""). The returned
// Locker must be closed by Server.Close.
func buildLocksClient(cfg *config.Config) (locks.Locker, error) {
	lc := cfg.LocksClient
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch lc.Mode {
	case "":
		return nil, nil
	case "embedded":
		if lc.Socket == "" {
			return nil, fmt.Errorf("locks_client.socket is required for embedded mode")
		}
		return locks.NewClient(ctx, locks.DialUnix(lc.Socket))
	case "remote":
		if len(lc.Endpoints) == 0 {
			return nil, fmt.Errorf("locks_client.endpoints must list at least one host:port for remote mode")
		}
		if cfg.InternalTLS.Enabled {
			tlsCfg, err := mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
			if err != nil {
				return nil, fmt.Errorf("locks_client mtls: %w", err)
			}
			return locks.NewClient(ctx, locks.DialTLS(lc.Endpoints[0], tlsCfg))
		}
		// Single-endpoint connect for now; failover across Endpoints is a
		// follow-up (custom Dialer iterating the list until first success).
		return locks.NewClient(ctx, locks.DialTCP(lc.Endpoints[0]))
	default:
		return nil, fmt.Errorf("locks_client: unknown mode %q (want remote | embedded | \"\")", lc.Mode)
	}
}

func buildPassdbs(entries []config.PassdbEntry) ([]protocol.Passdb, error) {
	var dbs []protocol.Passdb
	for _, e := range entries {
		switch strings.ToLower(e.Driver) {
		case "sqlite", "mysql", "postgres":
			db, err := authsql.New(authsql.Config{
				Driver:            e.Driver,
				DSN:               e.DSN,
				PasswordQuery:     e.PasswordQuery,
				UserQuery:         e.UserQuery,
				IterateQuery:      e.IterateQuery,
				DefaultPassScheme: e.DefaultPassScheme,
				SkipSchema:        e.SkipSchema,
			})
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
