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
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	authsql "github.com/0kaba0hub/yarilo/internal/auth/sql"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

// version is stamped at build time via -ldflags="-X main.version=<tag>".
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})))

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
		"version", version,
		"listen", cfg.AuthService.Listen,
		"telemetry", cfg.Telemetry.Listen,
	)

	var dbs []protocol.Passdb
	var userdbs []protocol.Userdb
	for _, entry := range cfg.Auth.Passdb {
		sqlCfg := authsql.Config{
			Driver:            entry.Driver,
			DSN:               entry.DSN,
			PasswordQuery:     entry.PasswordQuery,
			UserQuery:         entry.UserQuery,
			IterateQuery:      entry.IterateQuery,
			DefaultPassScheme: entry.DefaultPassScheme,
			SkipSchema:        entry.SkipSchema,
		}
		db, err := authsql.New(sqlCfg)
		if err != nil {
			slog.Error("passdb init failed", "driver", entry.Driver, "err", err)
			os.Exit(1)
		}
		dbs = append(dbs, db)
		// Each passdb entry that ships its own UserQuery /
		// IterateQuery is also exposed as a userdb. Backend-api
		// admin lookups and the master-protocol LIST command run
		// off the same DSN — operators almost always want both
		// roles served by the same SQL row set.
		userdb, err := authsql.NewUserdb(sqlCfg)
		if err != nil {
			slog.Error("userdb init failed", "driver", entry.Driver, "err", err)
			os.Exit(1)
		}
		userdbs = append(userdbs, userdb)
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

	srvOpts := []protocol.ServerOption{
		protocol.WithUserdb(combinedUserdb),
		protocol.WithFailureDelay(time.Duration(cfg.Auth.FailureDelaySeconds) * time.Second),
		protocol.WithInternalFailureDelay(time.Duration(cfg.Auth.InternalFailureDelayMs) * time.Millisecond),
	}
	if cfg.Auth.MasterUsers.Enabled {
		var masterdbs []protocol.Passdb
		for _, entry := range cfg.Auth.MasterUsers.Masterdb {
			sqlCfg := authsql.Config{
				Driver:            entry.Driver,
				DSN:               entry.DSN,
				PasswordQuery:     entry.PasswordQuery,
				UserQuery:         entry.UserQuery,
				IterateQuery:      entry.IterateQuery,
				DefaultPassScheme: entry.DefaultPassScheme,
				SkipSchema:        entry.SkipSchema,
			}
			db, err := authsql.New(sqlCfg)
			if err != nil {
				slog.Error("masterdb init failed", "driver", entry.Driver, "err", err)
				os.Exit(1)
			}
			masterdbs = append(masterdbs, db)
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
	srv := protocol.NewServer(dbs, srvOpts...)
	errCh := make(chan error, 2)
	go func() {
		if err := srv.ListenAndServe(ctx, cfg.AuthService.Listen, tlsCfg); err != nil {
			errCh <- err
		}
	}()

	// Master protocol — userdb-only lookups + LIST. Skipped when
	// master_listen is unset; that keeps single-binary dev / smoke
	// runs free of an extra bind that nothing consumes.
	if cfg.AuthService.MasterListen != "" {
		master := protocol.NewMasterServer(combinedUserdb)
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

func logLevel() slog.Level {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
