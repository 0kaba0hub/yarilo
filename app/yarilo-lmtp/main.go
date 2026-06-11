// yarilo-lmtp is the LMTP backend for the yarilo mail server.
// It accepts local mail delivery from MTAs (proxied via yarilo-director),
// authenticates recipients via yarilo-auth, and stores messages in the
// configured storage backend.
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

	slog.Info("yarilo-lmtp starting",
		"version", version,
		"telemetry", cfg.Telemetry.Listen,
	)

	// LMTP session binary — disable all non-LMTP services so backend.New
	// does not try to start IMAP/POP3/Submission listeners or load their TLS certs.
	cfg.Services.IMAPS = nil
	cfg.Services.IMAP = nil
	cfg.Services.POP3S = nil
	cfg.Services.POP3 = nil
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

	if err := srv.RunLMTP(ctx); err != nil {
		slog.Error("lmtp server error", "err", err)
		os.Exit(1)
	}

	slog.Info("yarilo-lmtp stopped")
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
