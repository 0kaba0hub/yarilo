// yarilo-jmap is the JMAP backend (RFC 8620 core, RFC 8621 mail). It listens on
// :10443 behind yarilo-jmap-login, which terminates the client's TLS and
// authenticates; this process trusts the hop and never re-runs the passdb
// chain. Session resource only so far.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yarilomail/yarilo/internal/backend"
	"github.com/yarilomail/yarilo/internal/jmap"
	"github.com/yarilomail/yarilo/internal/readyfile"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

func main() {
	logging.Setup("jmap")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	svc := cfg.Services.JMAPBE
	if !svc.Active() {
		slog.Error("services.jmap_be is not enabled")
		os.Exit(1)
	}
	addr := fmt.Sprintf(":%d", svc.Port)

	// Internal mTLS, not the client certificate: the client's TLS ended at the
	// login pod and this hop is between components.
	tlsCfg, err := internalTLS(cfg)
	if err != nil {
		slog.Error("internal TLS config failed", "err", err)
		os.Exit(1)
	}
	// The userdb lookup and the locks client dial other components, so they
	// need the client half of the same internal mTLS.
	intTLS, err := internalClientTLS(cfg)
	if err != nil {
		slog.Error("internal mTLS client config failed", "err", err)
		os.Exit(1)
	}
	trust := jmap.ResolveTrust(tlsCfg != nil, svc.XClient, parseCIDRs(cfg.General.XClient.TrustedNets))

	// Every URL in the session resource is prefixed with this, so an empty one
	// publishes paths no client can follow. It is a warning rather than a
	// refusal: nothing leaks, and a bound port with a named cause in the log
	// diagnoses itself, where a pod that will not start looks like an image or
	// scheduling fault.
	if cfg.JMAPService.AuthMasterAddr == "" {
		slog.Warn("jmap: jmap_service.auth_master_addr is empty — userdb lookups fall back to the storage templates, which is wrong for any user whose home or mail location is not template-derived")
	}
	if cfg.Protocol.JMAP.BaseURL == "" {
		slog.Warn("jmap: protocol.jmap.jmap_base_url is empty — the session resource will advertise URLs no client can follow; set it to the public origin clients reach this deployment on")
	}

	slog.Info("yarilo-jmap starting",
		"version", build.Version,
		"listen", addr,
		"mtls", tlsCfg != nil,
		"trust", trust.Mode.String(),
		"base_url", cfg.Protocol.JMAP.BaseURL,
		"telemetry", cfg.Telemetry.Listen,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	tel := telemetry.NewWithOptions(telemetry.Options{
		Addr: telemetry.Addr(cfg.Telemetry.Listen),
		Pprof: telemetry.PprofOptions{
			Enabled:       cfg.Telemetry.PprofEnabled,
			Heap:          cfg.Telemetry.PprofHeapEnabled,
			BlockRate:     cfg.Telemetry.PprofBlockProfileRate,
			MutexFraction: cfg.Telemetry.PprofMutexProfileFraction,
		},
		Lifecycle: true,
	})
	go func() {
		if err := tel.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry server failed", "err", err)
		}
	}()

	store, err := buildStorage(cfg, intTLS)
	if err != nil {
		slog.Error("storage wiring failed", "err", err)
		os.Exit(1)
	}

	// Publish this protocol container's readiness into the co-located pod's
	// shared directory (#788); the yarilo-backend-reg sidecar gates the pod's
	// director heartbeat on it. Ready = listener bound. No-op when
	// readiness_dir is unset.
	var ready atomic.Bool
	reg := cfg.BackendRegister
	go readyfile.Touch(ctx, reg.ReadinessDir, "jmap",
		time.Duration(reg.ReadinessTouchInterval)*time.Second, ready.Load)

	// The same client the session protocols use, so a query expands terms
	// through the identical language chain the service indexed with. Its pool
	// is this process's own: fts_max_conns is per process, so this adds to what
	// yarilo-backend-api already holds rather than sharing it.
	ftsClient, ftsChain, err := backend.BuildFTS(cfg)
	if err != nil {
		slog.Error("fts client", "err", err)
		os.Exit(1)
	}
	if ftsClient != nil {
		defer ftsClient.Close() //nolint:errcheck
	}
	var ftsOpts *jmap.FTS
	if ftsClient != nil && cfg.FTS.Search {
		ftsOpts = &jmap.FTS{
			Client:     ftsClient,
			Chain:      ftsChain,
			MaxConns:   cfg.FTS.MaxConns,
			MaxFolders: cfg.Protocol.JMAP.MaxQueryFolders,
			AddMissing: cfg.FTS.SearchAddMissing,
			Timeout:    time.Duration(cfg.FTS.SearchTimeoutSecs) * time.Second,
		}
	}

	// Whether this process can answer a text filter at all is a deployment
	// fact, and until now it was only observable by asking it one: without the
	// client Email/query refuses text conditions with unsupportedFilter, which
	// is correct and invisible (#1279).
	if ftsOpts != nil {
		slog.Info("jmap: full-text search wired", "fts_addr", cfg.FTS.Addr)
	} else {
		slog.Warn("jmap: full-text search is NOT wired in this process; Email/query text conditions will be refused as unsupportedFilter",
			"fts_enabled", cfg.FTS.Enabled, "fts_mode", cfg.FTS.Mode,
			"fts_addr", cfg.FTS.Addr, "fts_search", cfg.FTS.Search)
	}

	srv := jmap.New(jmap.Options{
		Addr:              addr,
		TLSConfig:         tlsCfg,
		Trust:             trust,
		Limits:            jmap.LimitsFrom(cfg.Protocol.JMAP),
		OnListen:          func() { ready.Store(true) },
		Storage:           store,
		FTS:               ftsOpts,
		SnippetMaxChars:   cfg.Protocol.JMAP.SnippetMaxChars,
		MaxBodyValueBytes: uint32(cfg.Protocol.JMAP.MaxBodyValueBytes), //nolint:gosec // config-bounded
		QueryMaxLimit:     uint(cfg.Protocol.JMAP.QueryMaxLimit),       //nolint:gosec // config-bounded
	})
	tel.SetReady(true)
	if err := srv.Serve(ctx); err != nil && ctx.Err() == nil {
		slog.Error("jmap server failed", "err", err)
		os.Exit(1)
	}
	slog.Info("yarilo-jmap stopped")
}

func internalTLS(cfg *config.Config) (*tls.Config, error) {
	if !cfg.InternalTLS.Enabled {
		return nil, nil
	}
	return mtls.ServerConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
}

// parseCIDRs turns the trusted-net list into matchers, skipping and logging a
// malformed entry rather than failing startup over one typo.
func parseCIDRs(ss []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("jmap: invalid trusted CIDR", "cidr", s, "err", err)
			continue
		}
		nets = append(nets, n)
	}
	return nets
}

// buildStorage wires the per-user mail access. Every dependency here is one the
// session protocols already use, so JMAP reads exactly what IMAP would.
func buildStorage(cfg *config.Config, intTLS *tls.Config) (*jmap.Storage, error) {
	locker, err := buildLocker(cfg, intTLS)
	if err != nil {
		return nil, err
	}
	// A nil locker would silently downgrade the subscription and special-use
	// reads to unlocked ones. Every deployment configures locks_client, so an
	// absent one is a misconfiguration to fail on, not to work around.
	if locker == nil {
		return nil, fmt.Errorf("jmap: locks_client is not configured; set mode remote (or embedded for a single-node dev run)")
	}
	resolver := backend.BuildResolver(cfg)
	// The index takes the locker too. OpenFolder is not purely a read: a folder
	// with no index yet is created, migrated and log-compacted on open, and
	// file.withDistLock runs those unguarded when no locker is wired. That
	// would race a live IMAP SELECT on the same shared index.
	idxOpts := backend.IndexOptions(cfg.Storage, locker)
	return &jmap.Storage{
		Mailbox: backend.BuildMailbox(cfg.Storage, locker),
		MailboxByDriver: func(driver string) mailbox.MailboxBackend {
			return backend.BuildMailboxByDriver(driver, cfg.Storage, locker)
		},
		Index:              file.New(idxOpts...),
		ResolveUser:        userResolver(cfg.JMAPService.AuthMasterAddr, resolver, intTLS),
		Locker:             locker,
		SpecialUseDefaults: cfg.Protocol.IMAP.SpecialUseDefaults,
	}, nil
}

// userResolver prefers the yarilo-auth master userdb, which carries the
// per-user storage identity (home, mail location, INDEX= overrides). Without an
// address it falls back to the resolver's template defaults.
func userResolver(masterAddr string, resolver *mailbox.Resolver, authTLS *tls.Config) func(string) (*mailbox.UserInfo, error) {
	if masterAddr == "" {
		return func(u string) (*mailbox.UserInfo, error) {
			return resolver.UserInfo(u, ""), nil
		}
	}
	return func(u string) (*mailbox.UserInfo, error) {
		cl, err := authclient.Dial(masterAddr, authTLS)
		if err != nil {
			return nil, fmt.Errorf("jmap: master dial: %w", err)
		}
		defer cl.Close() //nolint:errcheck
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ui, err := cl.Userdb(ctx, u)
		if err != nil {
			return nil, fmt.Errorf("jmap: userdb %s: %w", u, err)
		}
		if ui == nil {
			return nil, fmt.Errorf("jmap: userdb: user not found: %s", u)
		}
		return backend.ResolveUserInfo(resolver, u, ui), nil
	}
}

func buildLocker(cfg *config.Config, intTLS *tls.Config) (locks.Locker, error) {
	lc := cfg.LocksClient
	// The deadline has to cover the startup wait as well as one dial: a 10s
	// context would cancel the wait at 10s and report a timeout instead of the
	// service still being absent.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second+lc.StartupWait())
	defer cancel()
	switch lc.Mode {
	case "":
		return nil, nil
	case "embedded":
		if lc.Socket == "" {
			return nil, fmt.Errorf("locks_client.socket required for embedded mode")
		}
		return locks.NewClientWaiting(ctx, locks.DialUnix(lc.Socket), lc.StartupWait())
	case "remote":
		if len(lc.Endpoints) == 0 {
			return nil, fmt.Errorf("locks_client.endpoints must have at least one entry for remote mode")
		}
		if intTLS != nil {
			return locks.NewClientWaiting(ctx, locks.DialTLS(lc.Endpoints[0], intTLS), lc.StartupWait())
		}
		return locks.NewClientWaiting(ctx, locks.DialTCP(lc.Endpoints[0]), lc.StartupWait())
	default:
		return nil, fmt.Errorf("locks_client: unknown mode %q", lc.Mode)
	}
}

func internalClientTLS(cfg *config.Config) (*tls.Config, error) {
	if !cfg.InternalTLS.Enabled {
		return nil, nil
	}
	return mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA,
		cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
}
