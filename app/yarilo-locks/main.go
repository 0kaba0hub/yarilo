// yarilo-locks is the cross-process write-coordination service, selected by
// locks_service.mode in yarilo.yaml:
//
//	remote   — TCP+mTLS listener backed by Redis; the k8s production default.
//	           Replicas share state via Redis, so it scales 1 → N without rework.
//	embedded — Unix-socket listener backed by an in-memory map; dev/CI/unit tests
//	           only. State is ephemeral and pod-local, so it cannot cross pods.
//
// Both modes speak the same TAB-delimited wire protocol behind pkg/locks.Locker.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

// version is set via pkg/build.

func main() {
	logging.Setup("locks")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	lcfg := cfg.LocksService
	if lcfg.Mode == "" {
		slog.Error("locks_service.mode is required (remote | embedded)")
		os.Exit(1)
	}

	slog.Info("yarilo-locks starting",
		"version", build.Version,
		"mode", lcfg.Mode,
		"telemetry", cfg.Telemetry.Listen,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reg := prometheus.NewRegistry()
	metrics := locks.NewMetrics(reg, lcfg.Mode)

	backend, backendReady, err := buildBackend(lcfg)
	if err != nil {
		slog.Error("backend init failed", "err", err, "mode", lcfg.Mode)
		os.Exit(1)
	}
	defer func() { _ = backend.Close() }()

	ln, err := buildListener(cfg, lcfg)
	if err != nil {
		slog.Error("listener init failed", "err", err, "mode", lcfg.Mode)
		os.Exit(1)
	}

	srv := locks.NewServer(backend, slog.Default(), metrics)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ctx, ln)
	}()

	go runTelemetry(cfg.Telemetry, reg, backendReady)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig.String())
		srv.Close()
		grace := time.Duration(lcfg.Shutdown.SessionGracePeriod) * time.Second
		if grace <= 0 {
			grace = 5 * time.Second
		}
		// Wait for Serve to return or grace to expire.
		select {
		case <-serveErr:
		case <-time.After(grace):
			slog.Warn("graceful shutdown timed out", "grace", grace)
		}
		cancel()
	case err := <-serveErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("server exited with error", "err", err)
			os.Exit(1)
		}
	}

	slog.Info("yarilo-locks stopped")
}

// buildBackend instantiates the state backend for the configured mode. The
// returned readiness func reports current usability; /readyz consults it per request.
func buildBackend(lcfg config.LocksServiceConfig) (locks.Backend, func() bool, error) {
	switch lcfg.Mode {
	case "embedded":
		b := locks.NewMemoryBackend()
		ready := func() bool { return true } // in-memory is always live
		return b, ready, nil
	case "remote":
		if lcfg.Redis == "" {
			return nil, nil, fmt.Errorf("locks_service.redis is required for remote mode")
		}
		opts, err := redis.ParseURL(lcfg.Redis)
		if err != nil {
			return nil, nil, fmt.Errorf("parse redis url: %w", err)
		}
		rdb := redis.NewClient(opts)
		// No eager Ping: redis.NewClient is lazy, so the process comes up even when
		// Redis is not yet reachable rather than crash-looping. The startupProbe gates
		// traffic and `ready` reports Redis health on /readyz. A bad URL or unknown
		// mode still fails loudly below since no retry fixes those.
		opts2 := []locks.RedisOption{}
		if lcfg.KeyPrefix != "" {
			opts2 = append(opts2, locks.WithKeyPrefix(lcfg.KeyPrefix))
		}
		if lcfg.ChannelPrefix != "" {
			opts2 = append(opts2, locks.WithChannelPrefix(lcfg.ChannelPrefix))
		}
		ready := func() bool {
			pctx, pcancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer pcancel()
			return rdb.Ping(pctx).Err() == nil
		}
		return locks.NewRedisBackend(rdb, opts2...), ready, nil
	default:
		return nil, nil, fmt.Errorf("unknown locks_service.mode %q (want embedded | remote)", lcfg.Mode)
	}
}

// buildListener returns the net.Listener for the mode: embedded a Unix socket
// (a stale one is removed first), remote a TCP listener wrapped in cfg.InternalTLS mTLS.
func buildListener(cfg *config.Config, lcfg config.LocksServiceConfig) (net.Listener, error) {
	switch lcfg.Mode {
	case "embedded":
		if lcfg.Socket == "" {
			return nil, fmt.Errorf("locks_service.socket is required for embedded mode")
		}
		// Remove a stale socket from a previous run; ignore not-exists.
		if err := os.Remove(lcfg.Socket); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale socket %q: %w", lcfg.Socket, err)
		}
		return locks.ListenUnix(lcfg.Socket)
	case "remote":
		if lcfg.Listen == "" {
			return nil, fmt.Errorf("locks_service.listen is required for remote mode")
		}
		if !cfg.InternalTLS.Enabled {
			// Plain TCP only when a service mesh handles transport security.
			return net.Listen("tcp", lcfg.Listen)
		}
		tlsCfg, err := mtls.ServerConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
		if err != nil {
			return nil, fmt.Errorf("mtls config: %w", err)
		}
		return locks.ListenTLS(lcfg.Listen, tlsCfg)
	default:
		return nil, fmt.Errorf("unknown mode %q", lcfg.Mode)
	}
}

// runTelemetry serves /healthz, /readyz, /metrics on addr. /healthz is liveness
// (always 200); /readyz reports backend reachability so a pod whose Redis
// connection dropped is taken out of rotation.
func runTelemetry(cfg config.TelemetryConfig, reg *prometheus.Registry, backendReady func() bool) {
	tel := telemetry.NewWithOptions(telemetry.Options{
		Addr:     telemetry.Addr(cfg.Listen),
		Registry: reg,
		Checks:   []telemetry.Check{telemetry.FuncCheck("backend", backendReady)},
		Pprof: telemetry.PprofOptions{
			Enabled: cfg.PprofEnabled,
			Heap:    cfg.PprofHeapEnabled,
		},
	})
	if err := tel.ListenAndServe(context.Background()); err != nil {
		slog.Error("telemetry server failed", "err", err)
	}
}
