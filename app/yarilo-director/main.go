// yarilo-director is the consistent-hash routing service for the yarilo mail server.
// It accepts mail client connections (IMAP/POP3/LMTP), extracts the username from
// the protocol preamble, routes via consistent hash to a backend pod, and proxies
// the session directly to that pod IP (bypassing kube-proxy).
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/0kaba0hub/yarilo/internal/director"
	"github.com/0kaba0hub/yarilo/pkg/config"
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

	slog.Info("yarilo-director starting",
		"version", version,
		"listen", cfg.DirectorService.Listen,
		"telemetry", cfg.Telemetry.Listen,
		"internal_tls", cfg.InternalTLS.Enabled,
	)

	// Internal mTLS for director-protocol server (other directors connect here).
	var ringTLSCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		ringTLSCfg, err = mtls.ServerConfig(
			cfg.InternalTLS.Cert,
			cfg.InternalTLS.Key,
			cfg.InternalTLS.CA,
		)
		if err != nil {
			slog.Error("internal_tls server config failed", "err", err)
			os.Exit(1)
		}
	}

	// Internal mTLS client config for dialling backend pods.
	var backendTLSCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		backendTLSCfg, err = mtls.ClientConfig(
			cfg.InternalTLS.Cert,
			cfg.InternalTLS.Key,
			cfg.InternalTLS.CA,
		)
		if err != nil {
			slog.Error("internal_tls client config failed", "err", err)
			os.Exit(1)
		}
	}

	// External TLS (client-facing cert) for IMAPS / POP3S.
	var extTLSCfg *tls.Config
	if cfg.General.SSL.TLSCert != "" && cfg.General.SSL.TLSKey != "" {
		extTLSCfg, err = config.BuildTLSConfig(cfg.General.SSL)
		if err != nil {
			slog.Error("external TLS config failed", "err", err)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runTelemetry(cfg.Telemetry.Listen)

	srv := director.NewWithOptions(director.Options{
		UserExpire:   time.Duration(cfg.DirectorService.UserExpire) * time.Second,
		PingInterval: time.Duration(cfg.DirectorService.PingInterval) * time.Second,
		PingTimeout:  time.Duration(cfg.DirectorService.PingTimeout) * time.Second,
	})

	// Resolve static backends from config and register them in the ring.
	resolveBackends(ctx, cfg, srv)

	// Start mail protocol proxy listeners.
	if err := startProxies(ctx, srv, cfg, extTLSCfg, backendTLSCfg); err != nil {
		slog.Error("proxy startup failed", "err", err)
		os.Exit(1)
	}

	// Start director-protocol server (ring management, inter-director sync).
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(ctx, cfg.DirectorService.Listen, ringTLSCfg); err != nil {
			errCh <- err
		}
		close(errCh)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig.String())
		cancel()
		grace := time.Duration(cfg.DirectorService.Shutdown.SessionGracePeriod) * time.Second
		if grace > 0 {
			time.Sleep(grace)
		}
	case err := <-errCh:
		if err != nil {
			slog.Error("director server error", "err", err)
			os.Exit(1)
		}
	}

	slog.Info("yarilo-director stopped")
}

// resolveBackends resolves each headless-service hostname in MailServers to pod IPs
// and registers them in the ring. For headless k8s services, DNS returns one A-record
// per pod so the director connects directly to each pod, bypassing kube-proxy.
func resolveBackends(ctx context.Context, cfg *config.Config, srv *director.Server) {
	for _, ms := range cfg.DirectorService.MailServers {
		addrs, err := net.DefaultResolver.LookupHost(ctx, ms.Host)
		if err != nil {
			slog.Error("director: resolve backend", "host", ms.Host, "err", err)
			continue
		}
		for _, addr := range addrs {
			srv.AddBackend(addr, ms.Port, ms.Tag)
		}
		slog.Info("director: backends resolved", "host", ms.Host, "pods", len(addrs), "tag", ms.Tag)
	}
}

// startProxies starts one proxy listener per enabled mail-protocol listener.
func startProxies(ctx context.Context, srv *director.Server, cfg *config.Config, extTLS, backendTLS *tls.Config) error {
	type spec struct {
		svc      *config.ServiceConfig
		protocol string
		tls      *tls.Config
	}
	specs := []spec{
		{cfg.Services.IMAPS, "imaps", extTLS},
		{cfg.Services.IMAP, "imap", nil},
		{cfg.Services.POP3S, "pop3s", extTLS},
		{cfg.Services.POP3, "pop3", nil},
		{cfg.Services.LMTP, "lmtp", nil},
	}
	for _, s := range specs {
		if s.svc == nil || !s.svc.Enabled {
			continue
		}
		addr := fmt.Sprintf(":%d", s.svc.Port)
		if err := srv.StartProxy(ctx, director.ProxyConfig{
			Protocol:    s.protocol,
			Addr:        addr,
			ExtTLS:      s.tls,
			BackendTLS:  backendTLS,
			BackendPort: s.svc.Port, // backend pod listens on the same port number
		}); err != nil {
			return err
		}
	}
	return nil
}

func runTelemetry(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", promhttp.Handler())
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
