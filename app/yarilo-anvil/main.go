// yarilo-anvil is the connection-accounting service for the yarilo mail server.
// It enforces mail_max_userip_connections across all login pods by tracking
// active per-user@IP connections over the yarilo-anvil TCP+mTLS protocol.
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

	"github.com/0kaba0hub/yarilo/internal/anvil"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})).With("service", "anvil"))

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	slog.Info("yarilo-anvil starting",
		"version", version,
		"listen", cfg.AnvilService.Listen,
		"telemetry", cfg.Telemetry.Listen,
		"max_userip_connections", cfg.General.Limits.MaxUserIPConnections,
		"internal_tls", cfg.InternalTLS.Enabled,
	)

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

	srv := anvil.NewServer(cfg.General.Limits.MaxUserIPConnections)
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(ctx, cfg.AnvilService.Listen, tlsCfg); err != nil {
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
		grace := time.Duration(cfg.AnvilService.Shutdown.SessionGracePeriod) * time.Second
		if grace > 0 {
			time.Sleep(grace)
		}
	case err := <-errCh:
		if err != nil {
			slog.Error("anvil server error", "err", err)
			os.Exit(1)
		}
	}

	slog.Info("yarilo-anvil stopped")
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
