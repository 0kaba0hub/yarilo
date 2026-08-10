// yarilo-warden is the connection-accounting service for the yarilo mail server.
// It enforces mail_max_userip_connections across all login pods by tracking
// active per-user@IP connections over the yarilo-warden TCP+mTLS protocol.
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

	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/internal/warden"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	logging.Setup("warden")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	slog.Info("yarilo-warden starting",
		"version", build.Version,
		"listen", cfg.WardenService.Listen,
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
	var stateOpts []warden.ServerOption
	var stateChecks []telemetry.Check
	var closeState func()
	if cfg.WardenService.StateBackend == "redis" {
		opt, perr := redis.ParseURL(cfg.WardenService.RedisAddr)
		if perr != nil {
			slog.Error("warden: invalid redis_addr", "addr", cfg.WardenService.RedisAddr, "err", perr)
			os.Exit(1)
		}
		rdb := redis.NewClient(opt)
		prefix := cfg.WardenService.KeyPrefix
		if prefix == "" {
			prefix = "yarilo:warden:"
		}
		chanPrefix := cfg.WardenService.ChannelPrefix
		if chanPrefix == "" {
			chanPrefix = "yarilo:warden:events:"
		}
		backend := warden.NewRedisBackend(rdb, prefix, chanPrefix, warden.DefaultPenaltyDecay, warden.DefaultSessionTTL, cfg.General.Limits.MaxUserIPConnections)
		stateOpts = append(stateOpts, warden.WithStateBackend(backend))
		closeState = func() { _ = backend.Close() }
		stateChecks = append(stateChecks, telemetry.FuncCheck("state-redis", func() bool {
			pctx, pcancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer pcancel()
			return rdb.Ping(pctx).Err() == nil
		}))
		slog.Info("warden: state backend=redis", "addr", cfg.WardenService.RedisAddr, "key_prefix", prefix, "channel_prefix", chanPrefix)
	} else {
		// memory state is per-pod: sessions/penalty counters and the kick bus live
		// in this process only. Running more than one replica would fragment them —
		// each pod would see a different subset of sessions, enforce the limit
		// independently, and miss kicks published to a sibling — so replicas MUST
		// stay 1. The Helm chart fails closed on replicas>1 with memory; this warn
		// is the runtime half, self-explaining without grepping the issue.
		slog.Warn("warden: state backend=memory — replicas MUST stay 1; >1 fragments sessions/penalty/kick. Set state_backend=redis to scale out")
	}
	if closeState != nil {
		defer closeState()
	}

	srv := warden.NewServer(cfg.General.Limits.MaxUserIPConnections, stateOpts...)

	// Telemetry starts after the server exists so the liveness watchdog can probe
	// its session-tracking mutex (#904).
	tel := startTelemetry(cfg.Telemetry, srv, stateChecks)
	errCh := make(chan error, 1)
	// Bind before readiness: ListenAndServe would bind inside the goroutine, so the
	// pod would announce itself ready without knowing the port came up.
	ln, err := srv.Listen(cfg.WardenService.Listen, tlsCfg)
	if err != nil {
		slog.Error("warden: listen failed", "addr", cfg.WardenService.Listen, "err", err)
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
		grace := time.Duration(cfg.WardenService.Shutdown.SessionGracePeriod) * time.Second
		if grace > 0 {
			time.Sleep(grace)
		}
	case err := <-errCh:
		if err != nil {
			slog.Error("warden server error", "err", err)
			os.Exit(1)
		}
	}

	slog.Info("yarilo-warden stopped")
}

// startTelemetry serves /healthz, /readyz, /metrics and /debug/loglevel, and
// returns the server so the caller can report readiness once its listener is
// actually bound. When the liveness watchdog is enabled it probes the warden
// session-tracking mutex (#904).
func startTelemetry(cfg config.TelemetryConfig, srv *warden.Server, checks []telemetry.Check) *telemetry.Server {
	opts := telemetry.Options{
		Addr:      telemetry.Addr(cfg.Listen),
		Lifecycle: true,
		Checks:    checks,
		Pprof: telemetry.PprofOptions{
			Enabled:       cfg.PprofEnabled,
			Heap:          cfg.PprofHeapEnabled,
			BlockRate:     cfg.PprofBlockProfileRate,
			MutexFraction: cfg.PprofMutexProfileFraction,
		},
	}
	if wd := cfg.LivenessWatchdog; wd.Enabled {
		var gate *telemetry.Gate
		if wd.FaultInjectionEnabled {
			gate = telemetry.NewGate()
			opts.Fault = gate
		}
		opts.Watchdog = telemetry.WatchdogOptions{
			Check:            wardenLivenessCheck(srv, gate),
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

// wardenLivenessCheck exercises the session-tracking mutex to prove the warden hot
// path is not deadlocked (#904). Reading the session count takes the same s.mu
// every CONNECT/DISCONNECT/LOOKUP holds; a handler wedged under that lock blocks
// the read, which the watchdog observes as a failure via its own timeout. All
// state is in-process, so this touches nothing shared.
func wardenLivenessCheck(srv *warden.Server, gate *telemetry.Gate) telemetry.LivenessCheck {
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
