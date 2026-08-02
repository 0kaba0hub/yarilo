// yarilo-pop3 is the POP3/POP3S backend for the yarilo mail server.
// It handles POP3 sessions proxied from yarilo-director, authenticating via
// yarilo-auth and serving mail from the configured storage backend.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/yarilomail/yarilo/internal/backend"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/logging"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	logging.Setup("pop3")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	slog.Info("yarilo-pop3 starting",
		"version", build.Version,
		"telemetry", cfg.Telemetry.Listen,
	)

	// POP3 session binary — TLS is terminated at yarilo-pop3-login.
	// Disable TLS-requiring and unrelated service listeners.
	cfg.Services.IMAPS = nil
	cfg.Services.IMAP = nil
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

	if err := srv.RunPOP3(ctx); err != nil {
		slog.Error("pop3 server error", "err", err)
		os.Exit(1)
	}

	slog.Info("yarilo-pop3 stopped")
}
