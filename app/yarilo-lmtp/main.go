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
	"syscall"

	"github.com/yarilomail/yarilo/internal/backend"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/logging"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	logging.Setup("lmtp")

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
		"version", build.Version,
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
