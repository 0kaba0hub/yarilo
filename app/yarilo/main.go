package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/0kaba0hub/yarilo/internal/backend"
	"github.com/0kaba0hub/yarilo/pkg/config"
)

var debug bool

func debugf(msg string, args ...any) {
	if debug {
		slog.Debug(msg, args...)
	}
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "yarilo"))

	cfgPath := flag.String("config", "config/yarilo.yaml", "path to config file")
	flag.Parse()

	debug = strings.ToLower(os.Getenv("LOG_LEVEL")) == "debug"
	if debug {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})).With("service", "yarilo"))
	}

	debugf("starting", "config", *cfgPath)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	srv, err := backend.New(cfg)
	if err != nil {
		slog.Error("failed to init server", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("yarilo starting", "mode", cfg.Mode)
	if err := srv.Run(ctx); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
	slog.Info("yarilo stopped")
}
