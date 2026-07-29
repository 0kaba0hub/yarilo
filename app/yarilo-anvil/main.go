// yarilo-anvil is the connection-accounting service for the yarilo mail server.
// It enforces mail_max_userip_connections across all login pods by tracking
// active per-user@IP connections over the yarilo-anvil TCP+mTLS protocol.
package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/0kaba0hub/yarilo/internal/anvil"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/logging"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	logging.Setup("anvil")

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
		"version", build.Version,
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
	// One shared implementation for /healthz, /readyz, /metrics and
	// /debug/loglevel. No Checks yet: this component's /readyz was an
	// unconditional 200 before unification, and turning that into a real
	// condition is a behaviour change, not a refactor — see the readiness issue
	// for the per-component conditions.
	tel := telemetry.NewWithOptions(telemetry.Options{Addr: addr})
	if err := tel.ListenAndServe(context.Background()); err != nil {
		slog.Error("telemetry server failed", "err", err)
	}
}
