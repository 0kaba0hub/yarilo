// yarilo-monitor is a sidecar health checker for yarilo-director pods.
// It probes backend pods directly via IMAP/POP3/LMTP login and reports
// state changes to the director (BACKEND-FLUSH on failure, BACKEND-UP on recovery).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/0kaba0hub/yarilo/internal/monitor"
)

var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})).With("service", "monitor"))

	cfgPath := os.Getenv("MONITOR_CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/monitor.yaml"
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	slog.Info("yarilo-monitor starting",
		"version", version,
		"director", cfg.DirectorAddr,
		"tags", len(cfg.Tags),
		"interval", cfg.Interval,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig.String())
		cancel()
	}()

	monitor.New(cfg).Run(ctx)
	slog.Info("yarilo-monitor stopped")
}

func loadConfig(path string) (*monitor.Config, error) {
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, err
	}
	var cfg monitor.Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
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
