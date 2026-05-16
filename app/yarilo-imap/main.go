// yarilo-imap is the IMAP/IMAPS backend for the yarilo mail server.
// It handles IMAP sessions proxied from yarilo-director, authenticating via
// yarilo-auth and serving mail from the configured storage backend.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/0kaba0hub/yarilo/internal/backend"
	"github.com/0kaba0hub/yarilo/pkg/config"
)

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

	slog.Info("yarilo-imap starting",
		"version", version,
		"telemetry", cfg.Telemetry.Listen,
	)

	srv, err := backend.New(cfg)
	if err != nil {
		slog.Error("backend init failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.RunIMAP(ctx); err != nil {
		slog.Error("imap server error", "err", err)
		os.Exit(1)
	}

	slog.Info("yarilo-imap stopped")
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
