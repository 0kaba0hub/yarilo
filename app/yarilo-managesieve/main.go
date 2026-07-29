// yarilo-managesieve is the ManageSieve backend for the yarilo mail server.
// It handles RFC 5804 ManageSieve sessions proxied from yarilo-managesieve-login,
// storing and retrieving per-user Sieve scripts via the configured dict backend.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/0kaba0hub/yarilo/internal/backend"
	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/logging"
)

func main() {
	logging.Setup("managesieve")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	slog.Info("yarilo-managesieve starting",
		"version", build.Version,
		"telemetry", cfg.Telemetry.Listen,
	)

	// ManageSieve backend — disable all unrelated listeners.
	cfg.Services.IMAP = nil
	cfg.Services.IMAPS = nil
	cfg.Services.POP3 = nil
	cfg.Services.POP3S = nil
	cfg.Services.LMTP = nil
	cfg.Services.Submission = nil
	cfg.Services.Submissions = nil

	srv, err := backend.New(cfg)
	if err != nil {
		slog.Error("backend init failed", "err", err)
		os.Exit(1)
	}
	defer func() { _ = srv.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.RunManageSieve(ctx); err != nil {
		slog.Error("managesieve server error", "err", err)
		os.Exit(1)
	}

	slog.Info("yarilo-managesieve stopped")
}
