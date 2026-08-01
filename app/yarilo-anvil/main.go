// yarilo-anvil is the connection-accounting service for the yarilo mail server.
// It enforces mail_max_userip_connections across all login pods by tracking
// active per-user@IP connections over the yarilo-anvil TCP+mTLS protocol.
package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/0kaba0hub/yarilo/internal/anvil"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/logging"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	logging.Setup("anvil")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	slog.Info("yarilo-anvil starting",
		"version", build.Version,
		"listen", cfg.AnvilService.Listen,
		"telemetry", cfg.Telemetry.Listen,
		"max_userip_connections", cfg.General.Limits.MaxUserIPConnections,
		"internal_tls", cfg.InternalTLS.Enabled,
	)

	var tlsCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		tlsCfg, err = mtls.ServerConfig(
			cfg.InternalTLS.Cert,
			cfg.InternalTLS.Key,
			cfg.InternalTLS.CA,
		)
		if err != nil {
			slog.Error("internal_tls config failed", "err", err)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Shared-state backend (#908): memory (default) or Redis. Redis lets penalty
	// state survive a restart and be shared across replicas; readiness gates on it
	// like locks. In this phase only penalties are Redis-backed.
	var stateOpts []anvil.ServerOption
	var stateChecks []telemetry.Check
	var closeState func()
	if cfg.AnvilService.StateBackend == "redis" {
		opt, perr := redis.ParseURL(cfg.AnvilService.RedisAddr)
		if perr != nil {
			slog.Error("anvil: invalid redis_addr", "addr", cfg.AnvilService.RedisAddr, "err", perr)
			os.Exit(1)
		}
		rdb := redis.NewClient(opt)
		prefix := cfg.AnvilService.KeyPrefix
		if prefix == "" {
			prefix = "yarilo:anvil:"
		}
		chanPrefix := cfg.AnvilService.ChannelPrefix
		if chanPrefix == "" {
			chanPrefix = "yarilo:anvil:events:"
		}
		backend := anvil.NewRedisBackend(rdb, prefix, chanPrefix, anvil.DefaultPenaltyDecay, anvil.DefaultSessionTTL, cfg.General.Limits.MaxUserIPConnections)
		stateOpts = append(stateOpts, anvil.WithStateBackend(backend))
		closeState = func() { _ = backend.Close() }
		stateChecks = append(stateChecks, telemetry.FuncCheck("state-redis", func() bool {
			pctx, pcancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer pcancel()
			return rdb.Ping(pctx).Err() == nil
		}))
		slog.Info("anvil: state backend=redis", "addr", cfg.AnvilService.RedisAddr, "key_prefix", prefix)
	} else {
		slog.Info("anvil: state backend=memory")
	}
	if closeState != nil {
		defer closeState()
	}

	srv := anvil.NewServer(cfg.General.Limits.MaxUserIPConnections, stateOpts...)

	// Telemetry starts after the server exists so the liveness watchdog can probe
	// its session-tracking mutex (#904).
	tel := startTelemetry(cfg.Telemetry, srv, stateChecks)
	errCh := make(chan error, 1)
	// Bind before readiness: ListenAndServe would bind inside the goroutine, so the
	// pod would announce itself ready without knowing the port came up.
	ln, err := srv.Listen(cfg.AnvilService.Listen, tlsCfg)
	if err != nil {
		slog.Error("anvil: listen failed", "addr", cfg.AnvilService.Listen, "err", err)
		os.Exit(1)
	}
	go func() {
		if err := srv.Serve(ctx, ln); err != nil {
			errCh <- err
		}
		close(errCh)
	}()

	// The port is bound, so the pod can serve.
	tel.SetReady(true)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig.String())
		cancel()
		grace := time.Duration(cfg.AnvilService.Shutdown.SessionGracePeriod) * time.Second
		if grace > 0 {
			time.Sleep(grace)
		}
	case err := <-errCh:
		if err != nil {
			slog.Error("anvil server error", "err", err)
			os.Exit(1)
		}
	}

	slog.Info("yarilo-anvil stopped")
}

// startTelemetry serves /healthz, /readyz, /metrics and /debug/loglevel, and
// returns the server so the caller can report readiness once its listener is
// actually bound. When the liveness watchdog is enabled it probes the anvil
// session-tracking mutex (#904).
func startTelemetry(cfg config.TelemetryConfig, srv *anvil.Server, checks []telemetry.Check) *telemetry.Server {
	opts := telemetry.Options{Addr: telemetry.Addr(cfg.Listen), Lifecycle: true, Checks: checks}
	if wd := cfg.LivenessWatchdog; wd.Enabled {
		var gate *telemetry.Gate
		if wd.FaultInjectionEnabled {
			gate = telemetry.NewGate()
			opts.Fault = gate
		}
		opts.Watchdog = telemetry.WatchdogOptions{
			Check:            anvilLivenessCheck(srv, gate),
			Interval:         time.Duration(wd.IntervalSeconds) * time.Second,
			Timeout:          time.Duration(wd.TimeoutSeconds) * time.Second,
			FailureThreshold: wd.FailureThreshold,
		}
	}
	tel := telemetry.NewWithOptions(opts)
	go func() {
		if err := tel.ListenAndServe(context.Background()); err != nil {
			slog.Error("telemetry server failed", "err", err)
		}
	}()
	return tel
}

// anvilLivenessCheck exercises the session-tracking mutex to prove the anvil hot
// path is not deadlocked (#904). Reading the session count takes the same s.mu
// every CONNECT/DISCONNECT/LOOKUP holds; a handler wedged under that lock blocks
// the read, which the watchdog observes as a failure via its own timeout. All
// state is in-process, so this touches nothing shared.
func anvilLivenessCheck(srv *anvil.Server, gate *telemetry.Gate) telemetry.LivenessCheck {
	return func(ctx context.Context) error {
		if gate != nil {
			if err := gate.Check(ctx); err != nil {
				return err
			}
		}
		if srv != nil {
			srv.SessionCount()
		}
		return nil
	}
}
