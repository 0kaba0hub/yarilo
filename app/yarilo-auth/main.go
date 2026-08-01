// yarilo-auth is the standalone authentication service for the yarilo mail server.
// It exposes the yarilo-auth TCP+mTLS protocol on the configured address and
// serves /healthz, /readyz, /metrics on the telemetry port.
package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/0kaba0hub/yarilo/internal/auth/oauth2"
	"github.com/0kaba0hub/yarilo/internal/auth/passdbs"
	"github.com/0kaba0hub/yarilo/internal/auth/policy"
	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
	"github.com/0kaba0hub/yarilo/internal/warden"
	"github.com/0kaba0hub/yarilo/pkg/authtoken"
	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/logging"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
	"github.com/0kaba0hub/yarilo/pkg/retry"
)

// version is set via pkg/build; kept for vet compatibility.

func main() {
	logging.Setup("auth")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	slog.Info("yarilo-auth starting",
		"version", build.Version,
		"listen", cfg.AuthService.Listen,
		"telemetry", cfg.Telemetry.Listen,
	)

	// Each passdb that can serve userdb lookups (SQL, passwd-file) is also
	// exposed as a userdb, so backend-api admin lookups and the
	// master-protocol LIST command run off the same store.
	dbs, userdbs, err := passdbs.Build(cfg.Auth.Passdb)
	if err != nil {
		slog.Error("passdb init failed", "err", err)
		os.Exit(1)
	}

	// OAuth2 passdbs join the chain ahead of SQL so an OAUTHBEARER login
	// resolves through the validator before SQL sees the bearer token as
	// a plaintext "password".
	if len(cfg.Auth.OAuth2) > 0 {
		oauth2pdbs, err := oauth2.BuildPassdbs(context.Background(), cfg.Auth.OAuth2)
		if err != nil {
			slog.Error("oauth2 init failed", "err", err)
			os.Exit(1)
		}
		dbs = append(oauth2pdbs, dbs...)
		slog.Info("yarilo-auth oauth2 providers wired", "count", len(oauth2pdbs))
	}

	var tlsCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		tlsCfg, err = mtls.ServerConfig(
			cfg.InternalTLS.Cert,
			cfg.InternalTLS.Key,
			cfg.InternalTLS.CA,
		)
		if err != nil {
			slog.Error("internal_tls config failed", "err", err)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Build a userdb chain shared by both the client-protocol server
	// (enriches a successful passdb with userdb_* fields) and the
	// master-protocol server (USER / LIST handlers). One backend, two
	// consumers, no duplicated config.
	var combinedUserdb protocol.Userdb
	switch len(userdbs) {
	case 0:
		// No backends — every userdb-side surface returns NOTFOUND;
		// passdb-only auth still works because the enrichment branch
		// no-ops when userdb is nil.
	case 1:
		combinedUserdb = userdbs[0]
	default:
		combinedUserdb = protocol.UserdbChain(userdbs)
	}

	authCache := protocol.NewCache(
		cfg.Auth.Cache.CacheSizeBytes(),
		time.Duration(cfg.Auth.Cache.TTLSeconds)*time.Second,
		time.Duration(cfg.Auth.Cache.NegativeTTLSeconds)*time.Second,
	)

	// Telemetry starts after the cache exists so the liveness watchdog can
	// probe it: a wedged cache mutex is exactly the "up but cannot
	// authenticate" state no other probe catches.
	tel := startTelemetry(cfg.Telemetry, authCache)

	tokenStore, tokenClose := buildTokenStore(cfg.Auth.Token, cfg.General.StartupDialRetries)
	defer tokenClose()

	srvOpts := []protocol.ServerOption{
		protocol.WithTokenStore(tokenStore),
		protocol.WithUserdb(combinedUserdb),
		protocol.WithFailureDelay(time.Duration(cfg.Auth.FailureDelaySeconds) * time.Second),
		protocol.WithInternalFailureDelay(time.Duration(cfg.Auth.InternalFailureDelayMs) * time.Millisecond),
		protocol.WithCache(authCache),
	}

	// Auth-penalty: dial the warden service and route Lookup/Update
	// through it. Startup connection failure is fatal (operator asked for
	// the feature); per-request warden errors are log-only and fall back
	// to no-tarpit.
	if cfg.Auth.Penalty.Enabled {
		if cfg.WardenService.Listen == "" {
			slog.Error("auth.penalty.enabled requires warden_service.listen")
			os.Exit(1)
		}
		// Build a CLIENT mTLS config for the outbound warden dial, NOT the
		// server config used for our own listener: the server config carries
		// no ServerName and would verify warden's cert against the dial host
		// instead of the shared internal SAN. Mirrors the login pods' dial.
		var penaltyTLS *tls.Config
		if cfg.InternalTLS.Enabled {
			penaltyTLS, err = mtls.ClientConfig(
				cfg.InternalTLS.Cert,
				cfg.InternalTLS.Key,
				cfg.InternalTLS.CA,
				cfg.InternalTLS.ServerName,
				cfg.InternalTLS.SessionCacheSize,
				cfg.InternalTLS.SessionCacheTTL,
			)
			if err != nil {
				slog.Error("warden penalty client tls config failed", "err", err)
				os.Exit(1)
			}
		}
		// A resilient pool, not a single Dial: it redials on a transport
		// error and retries once, so the tarpit survives a warden restart
		// without an auth restart. It also dials lazily, so auth starts even
		// if warden is momentarily down; penalty stays fail-open until it
		// reconnects, so there is no startup CrashLoop.
		penaltyPool := warden.NewPool(cfg.WardenService.ClientAddr(), penaltyTLS, 0, 5*time.Second)
		defer penaltyPool.Close()
		srvOpts = append(srvOpts,
			protocol.WithPenalty(penaltyPool, warden.PenaltyToSecs),
		)
		slog.Info("yarilo-auth penalty enabled", "warden", cfg.WardenService.ClientAddr())
	}

	// Policy server: HTTP hook into wforce or equivalent. URL="" disables.
	if cfg.Auth.Policy.URL != "" {
		pc, err := policy.New(policy.Config{
			URL:              cfg.Auth.Policy.URL,
			APIHeader:        cfg.Auth.Policy.APIHeader,
			HashMech:         cfg.Auth.Policy.HashMech,
			HashNonce:        cfg.Auth.Policy.HashNonce,
			HashTruncateBits: cfg.Auth.Policy.HashTruncateBits,
			Timeout:          time.Duration(cfg.Auth.Policy.TimeoutMs) * time.Millisecond,
			RejectOnFail:     cfg.Auth.Policy.RejectOnFail,
			LogOnly:          cfg.Auth.Policy.LogOnly,
		})
		if err != nil {
			slog.Error("policy client init", "err", err)
			os.Exit(1)
		}
		srvOpts = append(srvOpts,
			protocol.WithPolicy(policy.ProtocolAdapter{C: pc}, protocol.PolicyMode{
				CheckBefore: cfg.Auth.Policy.CheckBefore,
				CheckAfter:  cfg.Auth.Policy.CheckAfter,
				ReportAfter: cfg.Auth.Policy.ReportAfter,
			}),
		)
		slog.Info("yarilo-auth policy enabled",
			"url", cfg.Auth.Policy.URL,
			"reject_on_fail", cfg.Auth.Policy.RejectOnFail,
			"log_only", cfg.Auth.Policy.LogOnly,
		)
	}
	if cfg.Auth.MasterUsers.Enabled {
		masterdbs, _, err := passdbs.Build(cfg.Auth.MasterUsers.Masterdb)
		if err != nil {
			slog.Error("masterdb init failed", "err", err)
			os.Exit(1)
		}
		srvOpts = append(srvOpts,
			protocol.WithMasterUsers(true),
			protocol.WithMasterdb(masterdbs),
			protocol.WithMasterUserSeparator(cfg.Auth.MasterUsers.Separator),
		)
		slog.Info("yarilo-auth master users enabled",
			"masterdb_drivers", len(masterdbs),
			"separator", cfg.Auth.MasterUsers.Separator,
		)
	}
	if cfg.Storage.MailPath != "" {
		srvOpts = append(srvOpts, protocol.WithDefaultMailPath(cfg.Storage.MailPath))
	}
	if cfg.Storage.MailInboxPath != "" {
		srvOpts = append(srvOpts, protocol.WithDefaultInboxPath(cfg.Storage.MailInboxPath))
	}
	srv := protocol.NewServer(dbs, srvOpts...)
	errCh := make(chan error, 3)
	// Bind before readiness: ListenAndServe would bind inside the
	// goroutine, so the pod would announce ready before the port came up.
	clientLn, err := srv.Listen(cfg.AuthService.Listen, tlsCfg)
	if err != nil {
		slog.Error("auth: listen failed", "addr", cfg.AuthService.Listen, "err", err)
		os.Exit(1)
	}
	go func() {
		if err := srv.Serve(ctx, clientLn); err != nil {
			errCh <- err
		}
	}()

	// Master protocol — userdb-only lookups + LIST. Skipped when
	// master_listen is unset, keeping single-binary dev / smoke runs free
	// of an extra bind nothing consumes.
	if cfg.AuthService.MasterListen != "" {
		masterOpts := []protocol.MasterServerOption{
			protocol.WithMasterCache(authCache),
			protocol.WithMasterTokenStore(tokenStore),
		}
		if cfg.Storage.MailPath != "" {
			masterOpts = append(masterOpts, protocol.WithMasterDefaultMailPath(cfg.Storage.MailPath))
		}
		if cfg.Storage.MailInboxPath != "" {
			masterOpts = append(masterOpts, protocol.WithMasterDefaultInboxPath(cfg.Storage.MailInboxPath))
		}
		master := protocol.NewMasterServer(combinedUserdb, masterOpts...)
		slog.Info("yarilo-auth master listener", "addr", cfg.AuthService.MasterListen)
		masterLn, merr := master.Listen(cfg.AuthService.MasterListen, tlsCfg)
		if merr != nil {
			slog.Error("auth/master: listen failed", "addr", cfg.AuthService.MasterListen, "err", merr)
			os.Exit(1)
		}
		go func() {
			if err := master.Serve(ctx, masterLn); err != nil {
				errCh <- err
			}
		}()
	}

	// Both ports are bound, so the pod can serve.
	tel.SetReady(true)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig.String())
		cancel()
		grace := time.Duration(cfg.AuthService.Shutdown.SessionGracePeriod) * time.Second
		if grace > 0 {
			time.Sleep(grace)
		}
	case err := <-errCh:
		if err != nil {
			slog.Error("auth server error", "err", err)
			os.Exit(1)
		}
	}

	slog.Info("yarilo-auth stopped")
}

// startTelemetry serves /healthz, /readyz, /metrics and /debug/loglevel,
// returning the server so the caller can report readiness once its
// listeners are bound. When enabled, the liveness watchdog probes the
// auth cache.
func startTelemetry(cfg config.TelemetryConfig, cache *protocol.Cache) *telemetry.Server {
	opts := telemetry.Options{Addr: telemetry.Addr(cfg.Listen), Lifecycle: true}
	if wd := cfg.LivenessWatchdog; wd.Enabled {
		var gate *telemetry.Gate
		if wd.FaultInjectionEnabled {
			gate = telemetry.NewGate()
			opts.Fault = gate
		}
		opts.Watchdog = telemetry.WatchdogOptions{
			Check:            authLivenessCheck(cache, gate),
			Interval:         time.Duration(wd.IntervalSeconds) * time.Second,
			Timeout:          time.Duration(wd.TimeoutSeconds) * time.Second,
			FailureThreshold: wd.FailureThreshold,
		}
	}
	tel := telemetry.NewWithOptions(opts)
	go func() {
		if err := tel.ListenAndServe(context.Background()); err != nil {
			slog.Error("telemetry server failed", "err", err)
		}
	}()
	return tel
}

// authLivenessCheck exercises the in-process auth cache mutex to prove
// the auth path is not deadlocked. It reads cache stats (a
// side-effect-free take of c.mu) and never touches the passdb/userdb
// backend, so a shared-database outage cannot trip every auth pod at
// once. A wedged mutex blocks the read; the watchdog sees the timeout as
// a failure.
func authLivenessCheck(cache *protocol.Cache, gate *telemetry.Gate) telemetry.LivenessCheck {
	return func(ctx context.Context) error {
		if gate != nil {
			if err := gate.Check(ctx); err != nil {
				return err
			}
		}
		if cache != nil {
			cache.Stats()
		}
		return nil
	}
}

// buildTokenStore creates the appropriate TokenStore based on config and returns
// it along with a cleanup function. The caller must invoke cleanup on exit.
func buildTokenStore(cfg config.AuthTokenConfig, dialRetries int) (protocol.TokenStore, func()) {
	ttl := time.Duration(cfg.TTLSeconds) * time.Second
	if cfg.Backend == "redis" {
		if cfg.RedisAddr == "" {
			slog.Error("auth.token.backend=redis requires auth.token.redis_addr")
			os.Exit(1)
		}
		opt, err := redis.ParseURL(cfg.RedisAddr)
		if err != nil {
			slog.Error("auth.token.redis_addr invalid", "err", err)
			os.Exit(1)
		}
		rdb := redis.NewClient(opt)
		if err := retry.Do(context.Background(), dialRetries, time.Second, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return rdb.Ping(ctx).Err()
		}); err != nil {
			slog.Error("auth: redis ping failed", "addr", cfg.RedisAddr, "err", err)
			os.Exit(1)
		}
		slog.Info("auth: token store backend=redis", "addr", cfg.RedisAddr)
		return authtoken.NewRedis(rdb, ttl, authtoken.WithKeyPrefix(cfg.KeyPrefix)), func() { rdb.Close() }
	}
	slog.Info("auth: token store backend=memory")
	s := authtoken.New(ttl)
	return s, s.Close
}
