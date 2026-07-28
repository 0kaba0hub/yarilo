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
	"github.com/0kaba0hub/yarilo/internal/auth/passdbs"
	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/connlimit"
	"github.com/0kaba0hub/yarilo/internal/fts/language"
	imapsvr "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/lmtp"
	mssvr "github.com/0kaba0hub/yarilo/internal/managesieve"
	pop3svr "github.com/0kaba0hub/yarilo/internal/pop3"
	"github.com/0kaba0hub/yarilo/internal/quotawarn"
	"github.com/0kaba0hub/yarilo/internal/readyfile"
	"github.com/0kaba0hub/yarilo/internal/sieve"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailboxbuild"
	submsvr "github.com/0kaba0hub/yarilo/internal/submission"
	submproxy "github.com/0kaba0hub/yarilo/internal/submission/proxy"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
	"github.com/0kaba0hub/yarilo/internal/userstate/acl"
	authclient "github.com/0kaba0hub/yarilo/pkg/authclient"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/ftsproto"
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

// startReadyFile publishes this protocol container's readiness into the shared
// co-located-pod directory (#788): readyfile.Touch bumps the file's mtime ONLY
// while telemetry reports ready, so the yarilo-backend-reg sidecar (which owns
// the pod's single director registration) gates the pod's heartbeat on this
// protocol being alive. A wedged data path stops touching → the file goes stale
// → the pod is expired ring-wide. No-op when backend_register.readiness_dir is
// unset (standalone / single-process runs). The director registration itself no
// longer lives here — it is the sidecar's job.
func (s *Server) startReadyFile(ctx context.Context, proto string) {
	reg := s.cfg.BackendRegister
	ready := func() bool { return s.telem != nil && s.telem.IsReady() }
	go readyfile.Touch(ctx, reg.ReadinessDir, proto, time.Duration(reg.ReadinessTouchInterval)*time.Second, ready)
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
		DefaultMailPath:    cfg.Storage.MailPath,
		DefaultSeparator:   personalSeparator(cfg.Namespaces),
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
	nsMailboxes, err := buildNamespaceMailboxes(cfg.Namespaces, cfg.Storage.Mailbox, cfg.Storage, locker)
	if err != nil {
		return nil, fmt.Errorf("backend: namespace mailboxes: %w", err)
	}

	// ---- dicts ----
	metadataDict, err := buildDict(cfg.Dicts, "metadata")
	if err != nil {
		return nil, fmt.Errorf("backend: dicts.metadata: %w", err)
	}
	// The quota_clone mirror (built below from quota_clone_dicts) is the only
	// dict consumer of quota data; the enforcement path reads the index.
	idxOpts := []file.Option{file.WithLocker(locker)}
	if cfg.Storage.IndexLogCompactMinBytes != 0 {
		idxOpts = append(idxOpts, file.WithLogCompaction(
			cfg.Storage.IndexLogCompactMinBytes,
			cfg.Storage.IndexLogCompactMaxBytes,
			time.Duration(cfg.Storage.IndexLogCompactMinAgeSecs)*time.Second,
		))
	}
	idx := file.New(idxOpts...)

	// ---- quota_warning action runner (shared by IMAP + LMTP) ----
	quotaWarner := quotawarn.New(cfg.Quota.WarningBinDir, cfg.Quota.WarningExecTimeout)

	// ---- quota_clone mirror (fan-out to N dicts, shared by IMAP + LMTP) ----
	var cloneDicts []dict.Dict
	for _, name := range cfg.Quota.CloneDicts {
		d, err := buildDict(cfg.Dicts, name)
		if err != nil {
			return nil, fmt.Errorf("backend: quota_clone dict %q: %w", name, err)
		}
		if d == nil {
			slog.Warn("quota_clone_dicts references an undefined dict", "name", name)
			continue
		}
		cloneDicts = append(cloneDicts, d)
	}
	quotaClone := quota.NewClone(cloneDicts)

	ftsClient, ftsChain, err := buildFTS(cfg)
	if err != nil {
		return nil, err
	}
	quotaCloneFlushDelay := time.Duration(cfg.Quota.CloneFlushDelay) * time.Second
	if quotaCloneFlushDelay <= 0 {
		quotaCloneFlushDelay = 10 * time.Second
	}

	// ---- shared connection limiter (IMAP + POP3) ----
	connLimiter := connlimit.New(cfg.General.Limits.MaxUserIPConnections)

	// ---- HAProxy shared nets ----
	haproxyNets := parseCIDRs(cfg.General.HAProxy.TrustedNets)
	haproxyTimeout := time.Duration(cfg.General.HAProxy.Timeout) * time.Second
	authAddr := cfg.AuthService.ClientAddr()
	masterAddr := cfg.AuthService.MasterAddr
	var authTLS *tls.Config
	if cfg.InternalTLS.Enabled {
		t, err := mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA, cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
		if err != nil {
			return nil, fmt.Errorf("backend: auth_service mtls: %w", err)
		}
		authTLS = t
	}
	// Internal mTLS SERVER config for the login->backend data path (#824): the
	// login pods dial the backend session ports over mTLS (their BackendTLS), so
	// the PreambleListener must terminate it — verifying the login's client cert
	// against the internal CA — before reading the YARILO preamble. The
	// server-side mirror of #816's client dials.
	var internalServerTLS *tls.Config
	if cfg.InternalTLS.Enabled {
		t, err := mtls.ServerConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
		if err != nil {
			return nil, fmt.Errorf("backend: internal_tls server: %w", err)
		}
		internalServerTLS = t
	}

	// ---- sieve ----
	svcs := cfg.Services
	var sieveEngine *sieve.Engine
	sieveDict, err := buildDict(cfg.Dicts, cfg.Sieve.ScriptsDictName)
	if err != nil {
		return nil, fmt.Errorf("backend: sieve dict: %w", err)
	}
	// Dedicated dict for the duplicate test (RFC 7352). driver=redis makes the
	// dedup window cross-pod; absent/memory keeps per-process behaviour.
	dupDict, err := buildDict(cfg.Dicts, "sieve_duplicate")
	if err != nil {
		return nil, fmt.Errorf("backend: sieve duplicate dict: %w", err)
	}
	if cfg.Sieve.Enabled {
		sieveEngine = sieve.New(cfg.Sieve, locker, sieveDict, dupDict)
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
		storageCfg := cfg.Storage
		aclGlobal, err := acl.NewGlobal(cfg.ACL.Global)
		if err != nil {
			return nil, fmt.Errorf("backend: acl global: %w", err)
		}
		imapServer = imapsvr.New(imapsvr.Options{
			Addr:      listenAddr(svcs.IMAPS),
			AddrPlain: listenAddr(svcs.IMAP),
			TLSConfig: imapTLS,
			Mailbox:   mbox,
			MailboxByDriver: func(driver string) mailbox.MailboxBackend {
				return buildMailboxByDriver(driver, storageCfg, locker)
			},
			Index:              idx,
			Resolver:           resolver,
			Auth:               authChain,
			ProxyProtocol:      primary.HAProxy,
			HAProxyTimeout:     haproxyTimeout,
			HAProxyTrustedNets: haproxyNets,
			AuthAddr:           authAddr,
			AuthTLS:            authTLS,
			PreambleTLS:        internalServerTLS,
			MasterAddr:         masterAddr,
			MasterTLS:          authTLS,
			IdleNotifyInterval: time.Duration(p.IdleNotifyInterval) * time.Second,
			MaxLineLength:      p.MaxLineLength,
			ConnLimit:          connLimiter,
			// yarilo-anvil for the SELECT push (#WHO folder tracking): the imap
			// session reports its currently-SELECTed mailbox so `yarctl
			// who` can render it. Without this the anvil client is a permanent
			// no-op and the WHO FOLDERS column is always empty.
			AnvilAddr:            cfg.AnvilService.ClientAddr(),
			AnvilTLS:             authTLS,
			IDSend:               p.IDSend,
			LoginGreeting:        p.LoginGreeting,
			LogoutFormat:         p.LogoutFormat,
			ClientWorkarounds:    imapsvr.ParseIMAPWorkarounds(p.ClientWorkarounds),
			Locker:               locker,
			SpecialUseDefaults:   p.SpecialUseDefaults,
			MetadataDict:         metadataDict,
			SieveEngine:          sieveEngine,
			IMAPQuota:            cfg.Protocol.IMAP.IMAPQuota,
			MaildirSyncOnSelect:  cfg.Storage.MaildirSyncOnSelect,
			DboxReactiveRebuild:  cfg.Storage.DboxReactiveRebuild,
			QuotaEngine:          cfg.Quota.Enabled,
			QuotaName:            cfg.Quota.Name,
			QuotaExceededMessage: cfg.Quota.ExceededMessage,
			QuotaMailSize:        quota.ParseSize(cfg.Quota.MailSize),
			QuotaPolicy:          cfg.Quota.QuotaPolicy(),
			QuotaWarner:          quotaWarner,
			QuotaClone:           quotaClone,
			QuotaCloneFlushDelay: quotaCloneFlushDelay,
			FTS: imapsvr.FTSOptions{
				Client:        ftsClient,
				Chain:         ftsChain,
				AddMissing:    cfg.FTS.SearchAddMissing,
				ReadFallback:  cfg.FTS.SearchReadFallback,
				Timeout:       time.Duration(cfg.FTS.SearchTimeoutSecs) * time.Second,
				Strict:        cfg.FTS.SearchStrict,
				Autoindex:     cfg.FTS.Autoindex,
				MaxRecent:     cfg.FTS.AutoindexMaxRecentMsgs,
				SearchEnabled: cfg.FTS.Search,
			},
			ACLEnabled:           cfg.ACL.Enabled,
			ACLDefaultsFromInbox: cfg.ACL.DefaultsFromInbox,
			ACLCacheTTL:          time.Duration(cfg.ACL.CacheTTL) * time.Second,
			ACLGlobal:            aclGlobal,
			ACLGlobalsOnly:       cfg.ACL.GlobalsOnly,
			Namespaces:           buildNamespaces(cfg.Namespaces),
			NamespaceMailboxes:   nsMailboxes,
			FailureDelay:         time.Duration(cfg.Auth.FailureDelaySeconds) * time.Second,
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
			Addr:      listenAddr(svcs.POP3S),
			AddrPlain: listenAddr(svcs.POP3),
			TLSConfig: pop3TLS,
			Mailbox:   mbox,
			MailboxByDriver: func(driver string) mailbox.MailboxBackend {
				return buildMailboxByDriver(driver, cfg.Storage, locker)
			},
			Index:              idx,
			Resolver:           resolver,
			Auth:               authChain,
			ProxyProtocol:      primary.HAProxy,
			HAProxyTimeout:     haproxyTimeout,
			HAProxyTrustedNets: haproxyNets,
			AuthAddr:           authAddr,
			AuthTLS:            authTLS,
			PreambleTLS:        internalServerTLS,
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
			PreambleTLS:    internalServerTLS,
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
		lmtpStorageCfg := cfg.Storage
		lmtpACLGlobal, err := acl.NewGlobal(cfg.ACL.Global)
		if err != nil {
			return nil, fmt.Errorf("backend: lmtp acl global: %w", err)
		}
		lmtpOpts := lmtp.Options{
			Hostname:             cfg.Protocol.Submission.Hostname,
			Config:               cfg.Protocol.LMTP,
			Mailbox:              mbox,
			Index:                idx,
			Resolver:             resolver,
			TLSConfig:            lmtpTLS,
			Locker:               locker,
			QuotaEngine:          cfg.Quota.Enabled,
			QuotaExceededMessage: cfg.Quota.ExceededMessage,
			QuotaMailSize:        quota.ParseSize(cfg.Quota.MailSize),
			QuotaPolicy:          cfg.Quota.QuotaPolicy(),
			QuotaWarner:          quotaWarner,
			QuotaClone:           quotaClone,
			FTSClient:            ftsClient,
			FTSAutoindex:         cfg.FTS.Autoindex,
			FTSMaxRecent:         cfg.FTS.AutoindexMaxRecentMsgs,
			MetadataDict:         metadataDict,
			AuthAddr:             authAddr,
			AuthTLS:              authTLS,
			PreambleTLS:          internalServerTLS,
			SieveEngine:          sieveEngine,
			Namespaces:           cfg.Namespaces,
			ACLEnabled:           cfg.ACL.Enabled,
			ACLGlobal:            lmtpACLGlobal,
			ACLGlobalsOnly:       cfg.ACL.GlobalsOnly,
			ACLDefaultsFromInbox: cfg.ACL.DefaultsFromInbox,
			ACLCacheTTL:          time.Duration(cfg.ACL.CacheTTL) * time.Second,
			MailboxByDriver: func(driver string) mailbox.MailboxBackend {
				return buildMailboxByDriver(driver, lmtpStorageCfg, locker)
			},
		}
		if addr := cfg.AuthService.MasterAddr; addr != "" {
			lmtpResolver := lmtpOpts.Resolver
			if lmtpResolver == nil {
				lmtpResolver = &mailbox.Resolver{}
			}
			// Dial the auth-master over internal mTLS (authTLS), and LAZILY —
			// see lazyUserdbLookup for why an eager, nil-TLS dial wedged lmtp
			// readiness under internal_tls (#821).
			lmtpOpts.UserdbLookup = lazyUserdbLookup(addr,
				func() (*authclient.Client, error) { return authclient.Dial(addr, authTLS) },
				lmtpResolver)
		}
		lmtpServer = lmtp.New(lmtpOpts)
	}

	// ---- ManageSieve ----
	var msServer *mssvr.Server
	if svcs.ManageSieveBE.Active() {
		msServer = mssvr.New(mssvr.Options{
			Locker:          locker,
			DefaultName:     cfg.Sieve.DefaultName,
			Resolver:        resolver,
			Config:          cfg.Protocol.ManageSieve,
			AuthAddr:        authAddr,
			AuthTLS:         authTLS,
			PreambleTLS:     internalServerTLS,
			MasterAddr:      masterAddr,
			MasterTLS:       authTLS,
			SieveExtensions: cfg.Sieve.SieveExtensions,
			ScriptsDriver:   cfg.Sieve.ScriptsDriver,
			ScriptsDict:     sieveDict,
		})
	}

	// ---- telemetry ----
	telemAddr := telemetry.Addr(cfg.Telemetry.Listen)
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
	s.startReadyFile(ctx, "imap")
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
	s.startReadyFile(ctx, "pop3")
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
	s.startReadyFile(ctx, "lmtp")
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
	s.startReadyFile(ctx, "managesieve")
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
// ALPN are accepted.
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

// ResolveUserInfo maps a userdb protocol.UserInfo to the storage-layer
// mailbox.UserInfo used to open a user's mailbox + index: it expands the home
// directory via the resolver and the %h/%u template modifiers, and copies the
// storage identity (Driver, MailPath, quota rules, dir overrides). Shared by
// LMTP delivery and the quota-status policy service so both resolve identical
// paths.
// lazyUserdbLookup builds the LMTP UserdbLookup that resolves a recipient's
// userdb via yarilo-auth. The auth-master client is dialled LAZILY on the first
// lookup — never at backend.New — and reconnected on error (#821). Eager dialing
// wedged lmtp readiness whenever yarilo-auth was slow, and under internal_tls a
// plain (nil-TLS) dial to the mTLS auth listener HUNG in the handshake with no
// error, blocking the pod forever. dial is injected so the laziness is testable.
func lazyUserdbLookup(addr string, dial func() (*authclient.Client, error), resolver *mailbox.Resolver) func(context.Context, string) (*mailbox.UserInfo, error) {
	var acMu sync.Mutex
	var ac *authclient.Client
	return func(ctx context.Context, username string) (*mailbox.UserInfo, error) {
		acMu.Lock()
		if ac == nil {
			c, dialErr := dial()
			if dialErr != nil {
				acMu.Unlock()
				return nil, fmt.Errorf("lmtp: userdb auth dial %s: %w", addr, dialErr)
			}
			ac = c
		}
		cur := ac
		acMu.Unlock()

		ui, err := cur.Userdb(ctx, username)
		if err != nil {
			acMu.Lock()
			if ac == cur {
				_ = ac.Close()
				fresh, dialErr := dial()
				if dialErr != nil {
					ac = nil // reset so the next lookup re-dials
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
		return ResolveUserInfo(resolver, username, ui), nil
	}
}

func ResolveUserInfo(resolver *mailbox.Resolver, username string, ui *protocol.UserInfo) *mailbox.UserInfo {
	if ui == nil {
		return nil
	}
	mbi := resolver.UserInfo(username, ui.Home)
	mbi.Groups = ui.Groups
	mbi.ACLUser = ui.ACLUser
	mbi.ACLGroups = ui.ACLGroups
	mbi.QuotaRules = ui.QuotaRules
	mbi.QuotaOverFlag = ui.QuotaOverFlag
	if ui.VolatileDir != "" {
		vd := mailbox.ExpandHome(ui.VolatileDir, mbi.Home)
		vd = strings.ReplaceAll(vd, "%h", mbi.Home)
		mbi.VolatileDir = mailbox.ExpandVars(vd, username)
	}
	if ui.IndexDir != "" {
		id := mailbox.ExpandHome(ui.IndexDir, mbi.Home)
		id = strings.ReplaceAll(id, "%h", mbi.Home)
		mbi.IndexDir = mailbox.ExpandVars(id, username)
	}
	if ui.ControlDir != "" {
		cd := mailbox.ExpandHome(ui.ControlDir, mbi.Home)
		cd = strings.ReplaceAll(cd, "%h", mbi.Home)
		mbi.ControlDir = mailbox.ExpandVars(cd, username)
	}
	if ui.AltDir != "" {
		ad := mailbox.ExpandHome(ui.AltDir, mbi.Home)
		ad = strings.ReplaceAll(ad, "%h", mbi.Home)
		mbi.AltDir = mailbox.ExpandVars(ad, username)
	}
	if ui.MailPath != "" {
		mbi.MailPath = mailbox.ExpandHome(ui.MailPath, mbi.Home)
	}
	if ui.InboxPath != "" {
		mbi.InboxPath = mailbox.ExpandHome(ui.InboxPath, mbi.Home)
	}
	// Stamp the per-user driver + any embedded INDEX=/CONTROL=/ALT=/VOLATILEDIR=
	// modifiers via the shared resolver (the separate userdb dir fields set above
	// win). Same parse IMAP/POP3 use, so LMTP and quota-status resolve identically.
	if err := mailbox.StampLocation(mbi, ui.MailLocation); err != nil {
		slog.Warn("backend: mail_location parse failed; using global mailbox backend",
			"user", username, "mail_location", ui.MailLocation, "err", err)
	}
	return mbi
}

// BuildMailbox constructs the mailbox backend for a storage config. Exported so
// standalone binaries (quota-status) build the same backend as the session pods.
// A nil locker is fine for read-only consumers.
func BuildMailbox(cfg config.StorageConfig, locker locks.Locker) mailbox.MailboxBackend {
	return buildMailbox(cfg, locker)
}

// BuildMailboxByDriver constructs the mailbox backend for a named per-user
// driver (mdbox / sdbox / maildir), applying the same options the session pods
// use. Exported so standalone binaries (yarilo-fts) resolve each user's
// storage format from the userdb mail_location instead of the global default.
func BuildMailboxByDriver(driver string, cfg config.StorageConfig, locker locks.Locker) mailbox.MailboxBackend {
	return buildMailboxByDriver(driver, cfg, locker)
}

// BuildResolver builds the storage path resolver from config, applying the same
// defaults as the session pods.
func BuildResolver(cfg *config.Config) *mailbox.Resolver {
	sc := cfg.Storage
	if sc.MaildirRoot == "" {
		sc.MaildirRoot = "/var/mail/vhosts"
	}
	if sc.MailHomeTemplate == "" {
		sc.MailHomeTemplate = "%d/%u"
	}
	return &mailbox.Resolver{
		Root:               sc.MaildirRoot,
		HomeTemplate:       sc.MailHomeTemplate,
		DefaultVolatileDir: sc.VolatileDir,
		DefaultIndexDir:    sc.IndexDir,
		DefaultControlDir:  sc.ControlDir,
		DefaultAltDir:      sc.AltDir,
		DefaultMailPath:    sc.MailPath,
		DefaultSeparator:   personalSeparator(cfg.Namespaces),
	}
}

func buildMailbox(cfg config.StorageConfig, locker locks.Locker) mailbox.MailboxBackend {
	return buildMailboxByDriver(cfg.Mailbox, cfg, locker)
}

// buildMailboxByDriver constructs a MailboxBackend for the named driver from sc.
// Thin wrapper over the shared mailboxbuild.ByDriver so every binary builds mdbox
// (and its tuning) identically — see #639.
func buildMailboxByDriver(driver string, sc config.StorageConfig, locker locks.Locker) mailbox.MailboxBackend {
	return mailboxbuild.ByDriver(driver, sc, locker)
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
func buildNamespaceMailboxes(namespaces []config.NamespaceConfig, globalDriver string, sc config.StorageConfig, locker locks.Locker) (map[string]mailbox.MailboxBackend, error) {
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
			b = buildMailboxByDriver(drv, sc, locker)
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
// personalSeparator returns the IMAP hierarchy separator of the personal
// namespace (default "." — maildir++), used to stamp UserInfo for the LMTP
// and backend-api paths that have no per-namespace IMAP context.
func personalSeparator(cfg []config.NamespaceConfig) string {
	for _, ns := range cfg {
		if strings.EqualFold(strings.TrimSpace(ns.Type), "personal") && ns.Separator != "" {
			return ns.Separator
		}
	}
	return "."
}

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
		sep := '.'
		if rs := []rune(ns.Separator); len(rs) == 1 {
			sep = rs[0]
		} else if ns.Separator != "" {
			slog.Warn("backend: namespace separator must be a single character, defaulting to .",
				"index", i, "separator", ns.Separator)
		}
		out = append(out, imapsvr.NamespaceSpec{
			Type:      nsType,
			Prefix:    ns.Prefix,
			Separator: sep,
			List:      ns.List,
			Location:  ns.Location,
			IgnoreACL: ns.IgnoreACL,
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
			tlsCfg, err := mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA, cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
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

// buildFTS wires the session-side FTS client (docs/FTS.md §11). Sessions
// only ever talk to the yarilo-fts service over the wire — remote mode; the
// embedded mode is for the service's own tests/CLI.
func buildFTS(cfg *config.Config) (ftsproto.Client, *language.MultiChain, error) {
	fc := cfg.FTS
	if !fc.Enabled {
		return nil, nil, nil
	}
	if fc.Mode != "remote" {
		return nil, nil, nil
	}
	if fc.Addr == "" {
		return nil, nil, fmt.Errorf("fts.fts_addr is required in remote mode")
	}
	if err := language.ValidateTokenizerConfig(fc.LanguageTokenizerAlgorithm, fc.LanguageTokenizerWB5A, fc.LanguageTokenizerExplicitPrefix); err != nil {
		return nil, nil, fmt.Errorf("fts tokenizer config: %w", err)
	}
	// The session side must build the IDENTICAL chain set the yarilo-fts
	// service indexes with (same languages, same per-language filters,
	// same token/address limits) — otherwise query expansion (#726 item 4:
	// per-language filter overrides) would diverge from what was actually
	// indexed.
	chain, err := language.NewMultiChain(languagesOrDefault(fc.Languages), fc.LanguageFilters, fc.LanguageFiltersOverride,
		fc.LanguageTokenMaxLen, fc.LanguageAddressMaxLen, fc.DetectionMinRunes)
	if err != nil {
		return nil, nil, fmt.Errorf("fts language chain: %w", err)
	}
	return ftsproto.NewLazy(fc.Addr, 10*time.Second), chain, nil
}

// languagesOrDefault mirrors app/yarilo-fts/main.go's languagesOr: MultiChain
// always needs at least one language, and the session side's configured set
// must match the yarilo-fts service's set exactly for query expansion to
// cover what indexing could have picked.
func languagesOrDefault(xs []string) []string {
	if len(xs) > 0 {
		return xs
	}
	return []string{"en"}
}

func buildPassdbs(entries []config.PassdbEntry) ([]protocol.Passdb, error) {
	dbs, _, err := passdbs.Build(entries)
	return dbs, err
}
