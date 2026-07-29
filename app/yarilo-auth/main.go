// yarilo-auth is the standalone authentication service for the yarilo mail server.
// It exposes the yarilo-auth TCP+mTLS protocol on the configured address and
// serves /healthz, /readyz, /metrics on the telemetry port.
package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/0kaba0hub/yarilo/internal/anvil"
	"github.com/0kaba0hub/yarilo/internal/auth/oauth2"
	"github.com/0kaba0hub/yarilo/internal/auth/passdbs"
	"github.com/0kaba0hub/yarilo/internal/auth/policy"
	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/pkg/authtoken"
	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/logging"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
	"github.com/0kaba0hub/yarilo/pkg/retry"
)

// version is stamped at build time via -ldflags="-X main.version=<tag>".
// version is set via pkg/build; kept for vet compatibility

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

	// Each passdb entry that can serve userdb lookups (SQL, passwd-file) is
	// exposed as a userdb too: backend-api admin lookups and the master-protocol
	// LIST command run off the same backend — operators almost always want both
	// roles served by the same store.
	dbs, userdbs, err := passdbs.Build(cfg.Auth.Passdb)
	if err != nil {
		slog.Error("passdb init failed", "err", err)
		os.Exit(1)
	}

	// OAuth2 passdbs join the chain ahead of SQL so an OAUTHBEARER
	// login resolves through the validator before SQL ever sees
	// the bearer token as a plaintext "password".
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

	go runTelemetry(cfg.Telemetry.Listen)

	// Build a userdb chain shared by both the client-protocol
	// server (RunAuth enriches successful passdb with userdb_*
	// fields — Phase AUTH-2 PR 3) and the master-protocol server
	// (USER / LIST handlers). One backend, two consumers, no
	// duplicated config.
	var combinedUserdb protocol.Userdb
	switch len(userdbs) {
	case 0:
		// No backends — every userdb-side surface returns NOTFOUND;
		// passdb-only auth still works because RunAuth no-ops the
		// enrichment branch when userdb is nil.
	case 1:
		combinedUserdb = userdbs[0]
	default:
		combinedUserdb = protocol.UserdbChain(userdbs)
	}

	authCache := protocol.NewCache(
		cfg.Auth.Cache.SizeBytes,
		time.Duration(cfg.Auth.Cache.TTLSeconds)*time.Second,
		time.Duration(cfg.Auth.Cache.NegativeTTLSeconds)*time.Second,
	)

	tokenStore, tokenClose := buildTokenStore(cfg.Auth.Token, cfg.General.StartupDialRetries)
	defer tokenClose()

	srvOpts := []protocol.ServerOption{
		protocol.WithTokenStore(tokenStore),
		protocol.WithUserdb(combinedUserdb),
		protocol.WithFailureDelay(time.Duration(cfg.Auth.FailureDelaySeconds) * time.Second),
		protocol.WithInternalFailureDelay(time.Duration(cfg.Auth.InternalFailureDelayMs) * time.Millisecond),
		protocol.WithCache(authCache),
	}

	// Auth-penalty: dial the anvil service and route Lookup/Update
	// through it. Connection failure at startup → fatal (operator
	// asked for the feature). Per-request anvil errors are
	// non-fatal and log-only — Server falls back to no-tarpit.
	if cfg.Auth.Penalty.Enabled {
		if cfg.AnvilService.Listen == "" {
			slog.Error("auth.penalty.enabled requires anvil_service.listen")
			os.Exit(1)
		}
		penaltyConn, err := anvil.Dial(cfg.AnvilService.ClientAddr(), tlsCfg, 5*time.Second)
		if err != nil {
			slog.Error("anvil dial for penalty", "addr", cfg.AnvilService.ClientAddr(), "err", err)
			os.Exit(1)
		}
		srvOpts = append(srvOpts,
			protocol.WithPenalty(penaltyConn, anvil.PenaltyToSecs),
		)
		slog.Info("yarilo-auth penalty enabled", "anvil", cfg.AnvilService.ClientAddr())
	}

	// Policy server: HTTP hook into wforce or equivalent. URL=""
	// disables.
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
	go func() {
		if err := srv.ListenAndServe(ctx, cfg.AuthService.Listen, tlsCfg); err != nil {
			errCh <- err
		}
	}()

	// Master protocol — userdb-only lookups + LIST. Skipped when
	// master_listen is unset; that keeps single-binary dev / smoke
	// runs free of an extra bind that nothing consumes.
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
		go func() {
			if err := master.ListenAndServe(ctx, cfg.AuthService.MasterListen, tlsCfg); err != nil {
				errCh <- err
			}
		}()
	}

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

func runTelemetry(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", promhttp.Handler())
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("telemetry server failed", "err", err)
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
		return authtoken.NewRedis(rdb, ttl), func() { rdb.Close() }
	}
	slog.Info("auth: token store backend=memory")
	s := authtoken.New(ttl)
	return s, s.Close
}
