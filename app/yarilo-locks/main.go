// yarilo-locks is the cross-process write-coordination service for yarilo.
// Two modes via locks_service.mode in yarilo.yaml:
//
//	remote   — TCP+mTLS listener backed by Redis. The production default for
//	           every k8s deployment (single-node or sharded). Multiple replicas
//	           share state through Redis; clients connect via a ClusterIP
//	           Service. Scales from 1 → N replicas without code or config rework.
//
//	embedded — Unix-socket listener backed by an in-memory map. Dev / CI / unit
//	           tests only. State is ephemeral and local to the pod, so embedded
//	           is single-process by construction and not used in Helm deployments.
//
// The same TAB-delimited wire protocol serves both modes; the pkg/locks.Locker
// interface used by session processes does not care which mode is active.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
	"github.com/0kaba0hub/yarilo/pkg/retry"
)

// version is stamped at build time via -ldflags="-X main.version=<tag>".
// version is set via pkg/build; kept for vet compatibility

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})).With("service", "locks"))

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

	backend, backendReady, err := buildBackend(ctx, lcfg, cfg.General.StartupDialRetries)
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

	go runTelemetry(cfg.Telemetry.Listen, reg, backendReady)

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

// buildBackend instantiates the state backend for the configured mode.
// The returned readiness function reports whether the backend is presently
// usable; /readyz consults it on each request.
func buildBackend(ctx context.Context, lcfg config.LocksServiceConfig, dialRetries int) (locks.Backend, func() bool, error) {
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
		if err := retry.Do(ctx, dialRetries, time.Second, func() error {
			pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
			defer pcancel()
			return rdb.Ping(pctx).Err()
		}); err != nil {
			_ = rdb.Close()
			return nil, nil, fmt.Errorf("redis ping: %w", err)
		}
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

// buildListener returns a net.Listener appropriate to the mode.
// Embedded: Unix socket; the path's parent directory must exist and the
// process must own a stale socket if one is present (it is removed).
// Remote: TCP wrapped in mTLS using cfg.InternalTLS.
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
			// Plain TCP in remote mode is only acceptable when the cluster
			// runs a service mesh handling transport security.
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

// runTelemetry serves /healthz, /readyz, /metrics on addr.
// /healthz is process liveness (always 200 while running).
// /readyz reports backend reachability — useful for k8s rolling updates so a
// pod whose Redis connection has dropped is taken out of rotation.
func runTelemetry(addr string, reg *prometheus.Registry, backendReady func() bool) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if backendReady != nil && !backendReady() {
			http.Error(w, "backend not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("telemetry server failed", "err", err)
	}
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
