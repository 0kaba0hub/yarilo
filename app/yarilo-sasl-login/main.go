// yarilo-sasl-login proxies Postfix SASL authentication to yarilo-auth.
package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/0kaba0hub/yarilo/internal/sasllogin"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/logging"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

func main() {
	logging.Setup("sasl-login")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	sl := cfg.SASLLogin

	authAddr := sl.AuthAddr
	if authAddr == "" {
		authAddr = cfg.AuthService.ClientAddr()
	}

	var authTLS *tls.Config
	if cfg.InternalTLS.Enabled {
		authTLS, err = mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA, cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
		if err != nil {
			slog.Error("internal tls config failed", "err", err)
			os.Exit(1)
		}
	}

	trustedNets := parseCIDRs(sl.TrustedNets)
	haproxyNets := parseCIDRs(sl.HAProxyNets)

	srv := sasllogin.New(sasllogin.Options{
		AuthAddr:       authAddr,
		AuthTLS:        authTLS,
		TrustedNets:    trustedNets,
		HAProxy:        sl.HAProxy,
		HAProxyTimeout: time.Duration(sl.HAProxyTimeout) * time.Second,
		HAProxyNets:    haproxyNets,
	})

	listen := sl.Listen
	if listen == "" {
		listen = ":12325"
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		slog.Error("listen failed", "addr", listen, "err", err)
		os.Exit(1)
	}

	telemetryAddr := cfg.Telemetry.Listen
	tel := startTelemetry(telemetryAddr)

	slog.Info("yarilo-sasl-login starting",
		"version", build.Version,
		"listen", listen,
		"auth_addr", authAddr,
		"auth_tls", authTLS != nil,
		"trusted_nets", sl.TrustedNets,
		"haproxy", sl.HAProxy,
		"telemetry", telemetryAddr,
	)

	// Every configured port is bound and serving now, so the pod can accept
	// clients. Reporting earlier would let Kubernetes route to a port that is not
	// listening yet.
	tel.SetReady(true)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Serve(ctx, ln); err != nil {
		slog.Error("sasl-login server error", "err", err)
		os.Exit(1)
	}

	slog.Info("yarilo-sasl-login stopped")
}

// startTelemetry serves /healthz, /readyz, /metrics and /debug/loglevel, and
// returns the server so the caller can report readiness once its listeners are
// actually bound.
//
// Lifecycle is on: without it /readyz answers 200 from the moment the process
// starts, which says nothing. With it, ready means this pod holds its ports.
func startTelemetry(addr string) *telemetry.Server {
	tel := telemetry.NewWithOptions(telemetry.Options{Addr: addr, Lifecycle: true})
	go func() {
		if err := tel.ListenAndServe(context.Background()); err != nil {
			slog.Error("telemetry server failed", "err", err)
		}
	}()
	return tel
}

func parseCIDRs(ss []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("invalid CIDR, skipping", "cidr", s, "err", err)
			continue
		}
		out = append(out, n)
	}
	return out
}
