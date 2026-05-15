// yarilo-auth is the standalone authentication service for the yarilo mail server.
// It exposes the yarilo-auth TCP+mTLS protocol on the configured address and
// serves /healthz, /readyz, /metrics on the telemetry port.
package main

import (
	"context"
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
	for _, entry := range cfg.Auth.Passdb {
		db, err := authsql.New(authsql.Config{
			Driver:            entry.Driver,
			DSN:               entry.DSN,
			PasswordQuery:     entry.PasswordQuery,
			UserQuery:         entry.UserQuery,
			IterateQuery:      entry.IterateQuery,
			DefaultPassScheme: entry.DefaultPassScheme,
			SkipSchema:        entry.SkipSchema,
		})
		if err != nil {
			slog.Error("passdb init failed", "driver", entry.Driver, "err", err)
			os.Exit(1)
		}
		dbs = append(dbs, db)
	}

	tlsCfg, err := mtls.ServerConfig(
		cfg.AuthService.MTLS.Cert,
		cfg.AuthService.MTLS.Key,
		cfg.AuthService.MTLS.CA,
	)
	if err != nil {
		slog.Error("mtls config failed", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runTelemetry(cfg.Telemetry.Listen)

	srv := protocol.NewServer(dbs)
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(ctx, cfg.AuthService.Listen, tlsCfg); err != nil {
			errCh <- err
		}
		close(errCh)
	}()

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
