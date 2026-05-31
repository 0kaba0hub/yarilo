// yarilo-admin-api is the storage-plane admin HTTP API.
//
// One instance runs per backend tag (or one per standalone deployment).
// In multi-pod backend cluster the director's /api/admin/proxy/...
// transparently routes admin requests to the right backend's
// yarilo-admin-api via the same wire protocol.
//
// Wire reference: docs/ADMIN-API.md
//
// Configuration: admin_api section of yarilo.yaml + dicts section
// (the live dicts opened here are exposed to operator HTTP clients).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/0kaba0hub/yarilo/internal/adminapi"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	_ "github.com/0kaba0hub/yarilo/pkg/dict/drivers/all"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
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

	listen := cfg.AdminAPI.Listen
	if listen == "" {
		listen = ":9105"
	}
	slog.Info("yarilo-admin-api starting",
		"version", version,
		"listen", listen,
		"internal_tls", cfg.InternalTLS.Enabled,
		"dicts", len(cfg.Dicts),
	)

	// Internal mTLS — same CA / cert pair as the rest of the cluster.
	// Mandatory in k8s (admin operations cross pods); plain TCP only
	// for local dev when internal_tls.enabled is false.
	var tlsCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		tlsCfg, err = mtls.ServerConfig(
			cfg.InternalTLS.Cert,
			cfg.InternalTLS.Key,
			cfg.InternalTLS.CA,
		)
		if err != nil {
			slog.Error("internal_tls server config failed", "err", err)
			os.Exit(1)
		}
	}

	// Open every configured dict eagerly so the API serves them as
	// soon as the listener is up. A missing/invalid dict aborts
	// startup — operators see the bad config immediately rather
	// than at first API call.
	dicts := map[string]dict.Dict{}
	for name, dc := range cfg.Dicts {
		if dc.Driver == "" {
			slog.Warn("admin-api: skipping dict with empty driver", "name", name)
			continue
		}
		d, err := dict.Open(dict.Config{Driver: dc.Driver, Settings: dc.Settings})
		if err != nil {
			slog.Error("admin-api: open dict failed", "name", name, "driver", dc.Driver, "err", err)
			os.Exit(1)
		}
		dicts[name] = d
		slog.Info("admin-api: opened dict", "name", name, "driver", dc.Driver)
	}
	defer func() {
		for name, d := range dicts {
			if err := d.Close(); err != nil {
				slog.Warn("admin-api: dict close failed", "name", name, "err", err)
			}
		}
	}()

	allowedNets := parseCIDRs(cfg.AdminAPI.AllowedNets)
	srv := adminapi.New(adminapi.Options{
		Addr:        listen,
		TLSConfig:   tlsCfg,
		Token:       cfg.AdminAPI.Token,
		AllowedNets: allowedNets,
		Dicts:       dicts,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := srv.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("admin-api: serve failed", "err", err)
		os.Exit(1)
	}
	slog.Info("yarilo-admin-api stopped")
}

func parseCIDRs(in []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(in))
	for _, s := range in {
		_, n, err := net.ParseCIDR(strings.TrimSpace(s))
		if err != nil {
			slog.Warn("admin-api: ignoring bad CIDR", "value", s, "err", err)
			continue
		}
		out = append(out, n)
	}
	return out
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
