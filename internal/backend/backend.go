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

	_ "github.com/yarilomail/yarilo/pkg/dict/drivers/all" // register all dict drivers

	"github.com/emersion/go-sasl"

	"github.com/yarilomail/yarilo/internal/auth/oauth2"
	"github.com/yarilomail/yarilo/internal/auth/passdbs"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/internal/connlimit"
	"github.com/yarilomail/yarilo/internal/fts/language"
	imapsvr "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/lmtp"
	mssvr "github.com/yarilomail/yarilo/internal/managesieve"
	pop3svr "github.com/yarilomail/yarilo/internal/pop3"
	"github.com/yarilomail/yarilo/internal/quotawarn"
	"github.com/yarilomail/yarilo/internal/readyfile"
	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbuild"
	submsvr "github.com/yarilomail/yarilo/internal/submission"
	submproxy "github.com/yarilomail/yarilo/internal/submission/proxy"
	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/internal/userstate/acl"
	authclient "github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/mtls"
	"github.com/yarilomail/yarilo/pkg/quota"
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

	// Per-protocol TLS configs, kept so each Run* binds its listener before
	// reporting readiness. New cannot bind: the co-located pod runs one
	// protocol per container off a single config.
	imapTLS       *tls.Config
	pop3TLS       *tls.Config
	submissionTLS *tls.Config
}

// Close releases backend resources. Session binaries should defer Close after
// backend.New for clean lock and dict release.
func (s *Server) Close() error {
	if s.locker != nil {
		return s.locker.Close()
	}
	return nil
}

// startReadyFile publishes this protocol's readiness into the shared pod
// directory: the file's mtime is bumped only while telemetry reports ready,
// so the yarilo-backend-reg sidecar gates the pod heartbeat on it. No-op when
// backend_register.readiness_dir is unset.
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
	// OAuth2 passdbs go ahead of SQL so SQL never sees a bearer token
	// as a plaintext "password".
	if len(cfg.Auth.OAuth2) > 0 {
		oauth2pdbs, err := oauth2.BuildPassdbs(context.Background(), cfg.Auth.OAuth2)
		if err != nil {
			return nil, fmt.Errorf("backend: oauth2: %w", err)
		}
		passdbs = append(oauth2pdbs, passdbs...)
	}
	authCache := protocol.NewCache(
		cfg.Auth.Cache.CacheSizeBytes(),
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
		Root:                     cfg.Storage.MaildirRoot,
		HomeTemplate:             cfg.Storage.MailHomeTemplate,
		DefaultVolatileDir:       cfg.Storage.VolatileDir,
		DefaultIndexDir:          cfg.Storage.IndexDir,
		DefaultControlDir:        cfg.Storage.ControlDir,
		DefaultAltDir:            cfg.Storage.AltDir,
		DefaultMailPath:          cfg.Storage.MailPath,
		DefaultSeparator:         personalSeparator(cfg.Namespaces),
		DefaultStorageEscapeChar: cfg.Storage.MailboxListStorageEscapeChar,
		DefaultSkipNFCNormalize:  !cfg.Storage.MailboxListNormalizeToNFC,
	}
	locker, err := buildLocksClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("backend: locks_client: %w", err)
	}
	mbox := buildMailbox(cfg.Storage, locker)

	// Per-namespace mailbox driver overrides; namespaces on the global
	// driver are absent from the map.
	nsMailboxes, err := buildNamespaceMailboxes(cfg.Namespaces, cfg.Storage.Mailbox, cfg.Storage, locker)
	if err != nil {
		return nil, fmt.Errorf("backend: namespace mailboxes: %w", err)
	}

	// ---- dicts ----
	metadataDict, err := buildDict(cfg.Dicts, "metadata")
	if err != nil {
		return nil, fmt.Errorf("backend: dicts.metadata: %w", err)
	}
	// quota data goes to dicts only via the quota_clone mirror; enforcement
	// reads the index
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
	// mTLS server config for the login->backend data path: the PreambleListener
	// verifies the login's client cert against the internal CA before reading
	// the YARILO preamble.
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
	// Dict for the Sieve duplicate test (RFC 7352). driver=redis makes the
	// dedup window cross-pod; absent/memory keeps it per-process.
	dupDict, err := buildDict(cfg.Dicts, "sieve_duplicate")
	if err != nil {
		return nil, fmt.Errorf("backend: sieve duplicate dict: %w", err)
	}
	if cfg.Sieve.Enabled {
		sieveEngine = sieve.New(cfg.Sieve, locker, sieveDict, dupDict)
	}

	// ---- IMAP ----
	// TLS configs are kept on Server so each Run* can bind after New returns.
	var imapTLS, pop3TLS, submissionTLS *tls.Config
	var imapServer *imapsvr.Server
	if svcs.IMAP.Active() || svcs.IMAPS.Active() {
		primary := firstActive(svcs.IMAPS, svcs.IMAP)
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
			UserdbLookup:       ownerUserdbLookup(masterAddr, authTLS, resolver),
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
			// warden push of the SELECTed mailbox, used by `yarctl who`
			WardenAddr:           cfg.WardenService.ClientAddr(),
			WardenTLS:            authTLS,
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
			// dial the auth-master lazily over internal mTLS; see lazyUserdbLookup
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
	telemOpts := telemetry.Options{
		Addr:      telemAddr,
		Lifecycle: true,
		Pprof: telemetry.PprofOptions{
			Enabled: cfg.Telemetry.PprofEnabled,
			Heap:    cfg.Telemetry.PprofHeapEnabled,
		},
	}
	// Liveness watchdog: stats the mail store base (catches hung NFS via the
	// watchdog timeout) and enters a local gate (fault injection). Off by default.
	if wd := cfg.Telemetry.LivenessWatchdog; wd.Enabled {
		storePath := storeHealthPath(cfg.Storage)
		var gate *telemetry.Gate
		if wd.FaultInjectionEnabled {
			gate = telemetry.NewGate()
			telemOpts.Fault = gate
		}
		telemOpts.Watchdog = telemetry.WatchdogOptions{
			Check:            storeLivenessCheck(storePath, gate),
			Interval:         time.Duration(wd.IntervalSeconds) * time.Second,
			Timeout:          time.Duration(wd.TimeoutSeconds) * time.Second,
			FailureThreshold: wd.FailureThreshold,
		}
	}
	telem := telemetry.NewWithOptions(telemOpts)

	return &Server{
		cfg:         cfg,
		telem:       telem,
		imap:        imapServer,
		pop3:        pop3Server,
		submission:  smtpServer,
		lmtp:        lmtpServer,
		managesieve: msServer,
		locker:      locker,

		imapTLS:       imapTLS,
		pop3TLS:       pop3TLS,
		submissionTLS: submissionTLS,
	}, nil
}

// storeHealthPath derives the directory to stat as the mail store liveness
// signal: the leading non-templated prefix of the first configured location,
// e.g. "/mnt/mail/%d/%n" -> "/mnt/mail". Empty disables the stat leg.
func storeHealthPath(sc config.StorageConfig) string {
	for _, loc := range []string{sc.MailPath, sc.MaildirRoot, sc.MailHomeTemplate} {
		if loc == "" {
			continue
		}
		if i := strings.IndexByte(loc, '%'); i >= 0 {
			loc = loc[:i]
		}
		loc = strings.TrimRight(loc, "/")
		if loc == "" {
			loc = "/"
		}
		return loc
	}
	return ""
}

// storeLivenessCheck enters the local gate and stats the mail store base.
// No explicit stat deadline: the watchdog timeout bounds the whole check.
func storeLivenessCheck(storePath string, gate *telemetry.Gate) telemetry.LivenessCheck {
	return func(ctx context.Context) error {
		if gate != nil {
			if err := gate.Check(ctx); err != nil {
				return err
			}
		}
		if storePath != "" {
			if _, err := os.Stat(storePath); err != nil {
				return fmt.Errorf("backend: mail store stat %q: %w", storePath, err)
			}
		}
		return nil
	}
}

// bindTCP binds addr and returns the listener, so callers can hold every port
// before reporting readiness; binding after SetReady leaves a window where a
// client gets connection refused.
func bindTCP(proto, addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%s: bind %s: %w", proto, addr, err)
	}
	slog.Info("listener bound", "proto", proto, "addr", addr)
	return ln, nil
}

// bindTLS is bindTCP for a TLS listener.
func bindTLS(proto, addr string, cfg *tls.Config) (net.Listener, error) {
	ln, err := tls.Listen("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("%s: bind %s (tls): %w", proto, addr, err)
	}
	slog.Info("listener bound", "proto", proto, "addr", addr, "tls", true)
	return ln, nil
}

// RunIMAP starts the IMAP/IMAPS listeners and telemetry, then blocks until ctx is cancelled.
func (s *Server) RunIMAP(ctx context.Context) error {
	go func() {
		if err := s.telem.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry: server error", "err", err)
		}
	}()

	svcs := s.cfg.Services
	if s.imap == nil {
		slog.Warn("imap: no listeners configured")
		// nothing to serve; report ready so the pod isn't stuck out of rotation
		s.telem.SetReady(true)
		<-ctx.Done()
		return nil
	}

	// Bind every configured port first; a failure is fatal, otherwise the pod
	// would report ready with a port unbound.
	var tlsLn, plainLn net.Listener
	if svcs.IMAPS.Active() {
		ln, err := bindTLS("imap", listenAddr(svcs.IMAPS), s.imapTLS)
		if err != nil {
			return err
		}
		tlsLn = ln
	}
	if svcs.IMAP.Active() {
		ln, err := bindTCP("imap", listenAddr(svcs.IMAP))
		if err != nil {
			return err
		}
		plainLn = ln
	}

	// every port is accepting now
	s.telem.SetReady(true)

	if tlsLn != nil {
		go func() {
			if err := s.imap.Serve(tlsLn); err != nil {
				slog.Error("imap: TLS server error", "err", err)
				os.Exit(1)
			}
		}()
	}
	if plainLn != nil {
		go func() {
			if err := s.imap.Serve(plainLn); err != nil {
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
	svcs := s.cfg.Services
	if s.pop3 == nil {
		slog.Warn("pop3: no listeners configured")
		s.telem.SetReady(true)
		<-ctx.Done()
		return nil
	}

	// bind before reporting ready
	var tlsLn, plainLn net.Listener
	if svcs.POP3S.Active() {
		ln, err := bindTLS("pop3", listenAddr(svcs.POP3S), s.pop3TLS)
		if err != nil {
			return err
		}
		tlsLn = ln
	}
	if svcs.POP3.Active() {
		ln, err := bindTCP("pop3", listenAddr(svcs.POP3))
		if err != nil {
			return err
		}
		plainLn = ln
	}

	s.telem.SetReady(true)

	if tlsLn != nil {
		go func() {
			slog.Error("pop3: TLS server error", "err", s.pop3.Serve(tlsLn))
			os.Exit(1)
		}()
	}
	if plainLn != nil {
		go func() {
			slog.Error("pop3: plain server error", "err", s.pop3.Serve(plainLn))
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
	svcs := s.cfg.Services
	if s.lmtp == nil || !svcs.LMTP.Active() {
		slog.Warn("lmtp: no listener configured")
		s.telem.SetReady(true)
		<-ctx.Done()
		return nil
	}

	// bind before reporting ready; a TLS error is fatal at startup
	ln, err := bindTCP("lmtp", listenAddr(svcs.LMTP))
	if err != nil {
		return err
	}
	if svcs.LMTP.SSLMode == "ssl" {
		tlsCfg, terr := buildTLS(s.cfg, svcs.LMTP)
		if terr != nil {
			ln.Close()
			return fmt.Errorf("lmtp: TLS: %w", terr)
		}
		if tlsCfg != nil {
			ln = tls.NewListener(ln, tlsCfg)
		}
	}

	s.telem.SetReady(true)

	go func() {
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
	svcs := s.cfg.Services
	if s.managesieve == nil || !svcs.ManageSieveBE.Active() {
		slog.Warn("managesieve: no listener configured")
		s.telem.SetReady(true)
		<-ctx.Done()
		return nil
	}

	// bind before reporting ready
	ln, err := bindTCP("managesieve", listenAddr(svcs.ManageSieveBE))
	if err != nil {
		return err
	}

	s.telem.SetReady(true)

	go func() {
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

	svcs := s.cfg.Services

	// Bind every configured port before reporting readiness: in standalone,
	// "ready" means every protocol is accepting.
	type listener struct {
		name  string
		ln    net.Listener
		serve func(net.Listener) error
	}
	var listeners []listener
	// a bind failure aborts startup, so close already-bound listeners
	closeAll := func() {
		for _, l := range listeners {
			l.ln.Close()
		}
	}

	if s.imap != nil {
		if svcs.IMAPS.Active() {
			ln, err := bindTLS("imap", listenAddr(svcs.IMAPS), s.imapTLS)
			if err != nil {
				closeAll()
				return err
			}
			listeners = append(listeners, listener{"imap/tls", ln, s.imap.Serve})
		}
		if svcs.IMAP.Active() {
			ln, err := bindTCP("imap", listenAddr(svcs.IMAP))
			if err != nil {
				closeAll()
				return err
			}
			listeners = append(listeners, listener{"imap", ln, s.imap.Serve})
		}
	}

	if s.pop3 != nil {
		if svcs.POP3S.Active() {
			ln, err := bindTLS("pop3", listenAddr(svcs.POP3S), s.pop3TLS)
			if err != nil {
				closeAll()
				return err
			}
			listeners = append(listeners, listener{"pop3/tls", ln, s.pop3.Serve})
		}
		if svcs.POP3.Active() {
			ln, err := bindTCP("pop3", listenAddr(svcs.POP3))
			if err != nil {
				closeAll()
				return err
			}
			listeners = append(listeners, listener{"pop3", ln, s.pop3.Serve})
		}
	}

	// Submission STARTTLS: TLS config is handed to Serve, not wrapped around
	// the listener, since the upgrade happens mid-session.
	if s.submission != nil && svcs.Submission.Active() {
		ln, err := bindTCP("submission", listenAddr(svcs.Submission))
		if err != nil {
			closeAll()
			return err
		}
		var tlsCfg *tls.Config
		if svcs.Submission.SSLMode == "ssl" {
			t, terr := buildTLS(s.cfg, svcs.Submission, alpnSMTP)
			if terr != nil {
				ln.Close()
				closeAll()
				return fmt.Errorf("submission: TLS: %w", terr)
			}
			tlsCfg = t
		}
		listeners = append(listeners, listener{"submission", ln, func(l net.Listener) error {
			return s.submission.Serve(l, tlsCfg)
		}})
	}

	if s.lmtp != nil && svcs.LMTP.Active() {
		ln, err := bindTCP("lmtp", listenAddr(svcs.LMTP))
		if err != nil {
			closeAll()
			return err
		}
		if svcs.LMTP.SSLMode == "ssl" {
			tlsCfg, terr := buildTLS(s.cfg, svcs.LMTP)
			if terr != nil {
				ln.Close()
				closeAll()
				return fmt.Errorf("lmtp: TLS: %w", terr)
			}
			if tlsCfg != nil {
				ln = tls.NewListener(ln, tlsCfg)
			}
		}
		listeners = append(listeners, listener{"lmtp", ln, s.lmtp.Serve})
	}

	// Submissions (implicit TLS on 465).
	if s.submission != nil && svcs.Submissions.Active() {
		ln, err := bindTCP("submissions", listenAddr(svcs.Submissions))
		if err != nil {
			closeAll()
			return err
		}
		tlsCfg, terr := buildTLS(s.cfg, svcs.Submissions, alpnSMTP)
		if terr != nil {
			ln.Close()
			closeAll()
			return fmt.Errorf("submissions: TLS: %w", terr)
		}
		listeners = append(listeners, listener{"submissions", ln, func(l net.Listener) error {
			return s.submission.Serve(l, tlsCfg)
		}})
	}

	// every port is accepting now
	s.telem.SetReady(true)

	for _, l := range listeners {
		l := l
		go func() {
			if err := l.serve(l.ln); err != nil {
				slog.Error("server error", "proto", l.name, "err", err)
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

// chainAuth adapts protocol.Authenticator to the SMTP server's
// (username, password) -> error surface; only the accept/reject decision
// is kept.
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

// AuthPlainMaster forwards a SASL PLAIN response carrying an authzid to the
// chain's MasterAuthenticator. If the chain doesn't implement it, the failure
// is deliberately indistinguishable from a wrong-password rejection.
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

// LookupSCRAMSha256 forwards to the chain's SCRAM verifier lookup. Returns
// (nil, nil) when the chain has none, so SCRAM mechs are not advertised in EHLO.
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

// lazyUserdbLookup builds the LMTP UserdbLookup resolving a recipient's userdb
// via yarilo-auth. The client is dialled lazily on first lookup and re-dialled
// on error; an eager dial at New would block readiness when yarilo-auth is
// slow or when a plain dial hits an mTLS listener (#821).
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
					ac = nil // next lookup re-dials
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

// ownerUserdbLookup builds the IMAP UserdbLookup for owner-templated namespaces,
// the same lazy auth-master lookup LMTP uses. Returns nil when no master is
// configured, which leaves owner resolution failing closed rather than half-wired.
func ownerUserdbLookup(masterAddr string, authTLS *tls.Config, resolver *mailbox.Resolver) func(context.Context, string) (*mailbox.UserInfo, error) {
	if masterAddr == "" {
		return nil
	}
	if resolver == nil {
		resolver = &mailbox.Resolver{}
	}
	return lazyUserdbLookup(masterAddr,
		func() (*authclient.Client, error) { return authclient.Dial(masterAddr, authTLS) },
		resolver)
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
	// Stamp the per-user driver + embedded INDEX=/CONTROL=/ALT=/VOLATILEDIR=
	// modifiers; the separate userdb dir fields set above win. Same parse as
	// IMAP/POP3 so all consumers resolve identical paths.
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

// BuildMailboxByDriver constructs the mailbox backend for a named driver
// (mdbox / sdbox / maildir) with the same options the session pods use.
// Exported for standalone binaries (yarilo-fts).
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
		Root:                     sc.MaildirRoot,
		HomeTemplate:             sc.MailHomeTemplate,
		DefaultVolatileDir:       sc.VolatileDir,
		DefaultIndexDir:          sc.IndexDir,
		DefaultControlDir:        sc.ControlDir,
		DefaultAltDir:            sc.AltDir,
		DefaultMailPath:          sc.MailPath,
		DefaultSeparator:         personalSeparator(cfg.Namespaces),
		DefaultStorageEscapeChar: cfg.Storage.MailboxListStorageEscapeChar,
		DefaultSkipNFCNormalize:  !cfg.Storage.MailboxListNormalizeToNFC,
	}
}

func buildMailbox(cfg config.StorageConfig, locker locks.Locker) mailbox.MailboxBackend {
	return buildMailboxByDriver(cfg.Mailbox, cfg, locker)
}

// buildMailboxByDriver wraps mailboxbuild.ByDriver so every binary builds
// mdbox (and its tuning) identically.
func buildMailboxByDriver(driver string, sc config.StorageConfig, locker locks.Locker) mailbox.MailboxBackend {
	return mailboxbuild.ByDriver(driver, sc, locker)
}

// buildNamespaceMailboxes builds the per-namespace MailboxBackend override
// map, keyed by namespace prefix. Only namespaces whose location: driver
// differs from the global default get an entry; same-driver namespaces share
// one backend instance.
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
			// inherits the global default
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
			// same driver as global default — no override needed
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

// personalSeparator returns the personal namespace's hierarchy separator
// (default "." — maildir++), used to stamp UserInfo for paths without
// per-namespace IMAP context (LMTP, backend-api).
func personalSeparator(cfg []config.NamespaceConfig) string {
	for _, ns := range cfg {
		if strings.EqualFold(strings.TrimSpace(ns.Type), "personal") && ns.Separator != "" {
			return ns.Separator
		}
	}
	return "."
}

// nsListMode resolves the operator's list setting; config validation already
// rejected unknown values, so a failed normalisation here can only mean the
// value bypassed validation -- fall back to the kind default rather than crash.
func nsListMode(ns config.NamespaceConfig) imapsvr.ListMode {
	mode, ok := mailbox.NamespaceListMode(ns.Prefix, ns.List)
	if !ok {
		mode, _ = mailbox.NamespaceListMode(ns.Prefix, "")
	}
	return imapsvr.ListMode(mode)
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
			Type:          nsType,
			Prefix:        ns.Prefix,
			Separator:     sep,
			List:          nsListMode(ns),
			Location:      ns.Location,
			IgnoreACL:     ns.IgnoreACL,
			Subscriptions: ns.Subscriptions,
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
	// A pool, not a single connection: one connection serialises request and
	// response, so a search that fans out over several folders would queue on
	// it however many goroutines the caller starts. Connections open on demand,
	// so a pool of four costs nothing until four calls overlap.
	return ftsproto.NewPool(fc.Addr, fc.MaxConns, 10*time.Second), chain, nil
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
