// yarilo-jmap is the JMAP backend (RFC 8620 core, RFC 8621 mail). It listens on
// :10443 behind yarilo-jmap-login, which terminates the client's TLS and
// authenticates; this process trusts the hop and never re-runs the passdb
// chain. Session resource only so far.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yarilomail/yarilo/internal/jmap"
	"github.com/yarilomail/yarilo/internal/readyfile"
	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

func main() {
	logging.Setup("jmap")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	svc := cfg.Services.JMAPBE
	if !svc.Active() {
		slog.Error("services.jmap_be is not enabled")
		os.Exit(1)
	}
	addr := fmt.Sprintf(":%d", svc.Port)

	// Internal mTLS, not the client certificate: the client's TLS ended at the
	// login pod and this hop is between components.
	tlsCfg, err := internalTLS(cfg)
	if err != nil {
		slog.Error("internal TLS config failed", "err", err)
		os.Exit(1)
	}
	trust := jmap.ResolveTrust(tlsCfg != nil, svc.XClient, parseCIDRs(cfg.General.XClient.TrustedNets))

	slog.Info("yarilo-jmap starting",
		"version", build.Version,
		"listen", addr,
		"mtls", tlsCfg != nil,
		"trust", trust.Mode.String(),
		"base_url", cfg.Protocol.JMAP.BaseURL,
		"telemetry", cfg.Telemetry.Listen,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	tel := telemetry.NewWithOptions(telemetry.Options{
		Addr:      telemetry.Addr(cfg.Telemetry.Listen),
		Lifecycle: true,
	})
	go func() {
		if err := tel.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry server failed", "err", err)
		}
	}()

	// Publish this protocol container's readiness into the co-located pod's
	// shared directory (#788); the yarilo-backend-reg sidecar gates the pod's
	// director heartbeat on it. Ready = listener bound. No-op when
	// readiness_dir is unset.
	var ready atomic.Bool
	reg := cfg.BackendRegister
	go readyfile.Touch(ctx, reg.ReadinessDir, "jmap",
		time.Duration(reg.ReadinessTouchInterval)*time.Second, ready.Load)

	srv := jmap.New(jmap.Options{
		Addr:      addr,
		TLSConfig: tlsCfg,
		Trust:     trust,
		Limits:    jmap.LimitsFrom(cfg.Protocol.JMAP),
		OnListen:  func() { ready.Store(true) },
	})
	tel.SetReady(true)
	if err := srv.Serve(ctx); err != nil && ctx.Err() == nil {
		slog.Error("jmap server failed", "err", err)
		os.Exit(1)
	}
	slog.Info("yarilo-jmap stopped")
}

func internalTLS(cfg *config.Config) (*tls.Config, error) {
	if !cfg.InternalTLS.Enabled {
		return nil, nil
	}
	return mtls.ServerConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
}

// parseCIDRs turns the trusted-net list into matchers, skipping and logging a
// malformed entry rather than failing startup over one typo.
func parseCIDRs(ss []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("jmap: invalid trusted CIDR", "cidr", s, "err", err)
			continue
		}
		nets = append(nets, n)
	}
	return nets
}
