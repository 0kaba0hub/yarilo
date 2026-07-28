// yarilo-pop3-login is the POP3/POP3S login proxy for the yarilo mail server.
// It accepts mail-client connections on port 995 (implicit TLS) and port 110
// (STARTTLS), handles the POP3 USER/PASS exchange, queries yarilo-director for
// the backend pod, and proxies the authenticated session.
// TLS is terminated here; yarilo-pop3 backends receive plain TCP.
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

	"github.com/0kaba0hub/yarilo/internal/login"
	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})).With("service", "pop3-login"))

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	svcs := cfg.Services
	if !svcs.POP3S.Active() && !svcs.POP3.Active() {
		slog.Error("no POP3 listener configured (pop3 or pop3s must be enabled)")
		os.Exit(1)
	}

	slog.Info("yarilo-pop3-login starting",
		"version", build.Version,
		"telemetry", cfg.Telemetry.Listen,
		"backend_addr", cfg.POP3LoginService.BackendAddr,
		"director_addr", cfg.POP3LoginService.DirectorAddr,
	)

	// External TLS (client-facing cert) for POP3S / STARTTLS.
	var extTLS *tls.Config
	if cfg.General.SSL.TLSCert != "" && cfg.General.SSL.TLSKey != "" {
		extTLS, err = config.BuildTLSConfig(cfg.General.SSL)
		if err != nil {
			slog.Error("TLS config failed", "err", err)
			os.Exit(1)
		}
		extTLS.NextProtos = []string{"pop3"}
	}

	// Internal mTLS for director + backend connections.
	var intTLS *tls.Config
	if cfg.InternalTLS.Enabled {
		intTLS, err = mtls.ClientConfig(
			cfg.InternalTLS.Cert,
			cfg.InternalTLS.Key,
			cfg.InternalTLS.CA,
			cfg.InternalTLS.ServerName,
			cfg.InternalTLS.SessionCacheSize,
			cfg.InternalTLS.SessionCacheTTL,
		)
		if err != nil {
			slog.Error("internal TLS config failed", "err", err)
			os.Exit(1)
		}
	}

	haproxyNets := parseCIDRs(cfg.General.HAProxy.TrustedNets)
	xclientNets := parseCIDRs(cfg.General.XClient.TrustedNets)
	haproxyTimeout := time.Duration(cfg.General.HAProxy.Timeout) * time.Second
	localIP := os.Getenv("POD_IP")
	// See yarilo-imap-login/main.go for why this must be the component's
	// own director_addr, not cfg.DirectorService.Listen (#735).
	if err := config.ValidateBackendOrDirector("pop3_login_service", cfg.POP3LoginService.BackendAddr, cfg.POP3LoginService.DirectorAddr); err != nil {
		slog.Error("config validation failed", "err", err)
		os.Exit(1)
	}
	dirAddr := cfg.POP3LoginService.DirectorAddr

	// ctx drives the per-listener director watch (#736) so USER-KICKED pushes
	// reach this pod's sessions. Cancelled on shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runTelemetry(cfg.Telemetry.Listen)

	// Port 995 — implicit TLS (POP3S).
	if svcs.POP3S.Active() {
		addr := fmt.Sprintf(":%d", svcs.POP3S.Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("pop3s-login: listen failed", "addr", addr, "err", err)
			os.Exit(1)
		}
		srv := login.New(login.Options{
			Protocol:          login.ProtocolPOP3S,
			DirectorAddr:      dirAddr,
			BackendAddr:       cfg.POP3LoginService.BackendAddr,
			BackendPort:       cfg.POP3LoginService.BackendPort,
			Tag:               cfg.POP3LoginService.DirectorTag,
			DirectorTLS:       intTLS,
			LocalIP:           localIP,
			BackendTLS:        intTLS,
			ExtTLS:            extTLS,
			AuthAddr:          cfg.AuthService.ClientAddr(),
			AuthTLS:           intTLS,
			AuthMaxAttempts:   cfg.Auth.MaxAttempts,
			OAuth2Enabled:     len(cfg.Auth.OAuth2) > 0,
			DisablePlainAuth:  svcs.POP3S.DisablePlainAuth,
			AnvilAddr:         cfg.AnvilService.ClientAddr(),
			AnvilTLS:          intTLS,
			AnvilFailOpen:     cfg.AnvilService.FailOpen,
			DialRetries:       cfg.General.StartupDialRetries,
			LookupHoldMax:     cfg.Login.LookupHoldMax,
			LookupHoldBackoff: time.Duration(cfg.Login.LookupHoldBackoffMs) * time.Millisecond,
			HAProxy:           svcs.POP3S.HAProxy,
			HAProxyTimeout:    haproxyTimeout,
			HAProxyNets:       haproxyNets,
			XClient:           svcs.POP3S.XClient,
			XClientNets:       xclientNets,
		})
		go func() {
			slog.Error("pop3s-login: server error", "err", srv.Serve(ln))
			os.Exit(1)
		}()
		if dirAddr != "" {
			go srv.Watch(ctx)
		}
		slog.Info("pop3-login: listening", "addr", addr, "tls", "implicit")
	}

	// Port 110 — STARTTLS.
	if svcs.POP3.Active() {
		addr := fmt.Sprintf(":%d", svcs.POP3.Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("pop3-login: listen failed", "addr", addr, "err", err)
			os.Exit(1)
		}
		srv := login.New(login.Options{
			Protocol:          login.ProtocolPOP3,
			DirectorAddr:      dirAddr,
			BackendAddr:       cfg.POP3LoginService.BackendAddr,
			BackendPort:       cfg.POP3LoginService.BackendPort,
			Tag:               cfg.POP3LoginService.DirectorTag,
			DirectorTLS:       intTLS,
			LocalIP:           localIP,
			BackendTLS:        intTLS,
			StarttlsTLS:       extTLS,
			AuthAddr:          cfg.AuthService.ClientAddr(),
			AuthTLS:           intTLS,
			AuthMaxAttempts:   cfg.Auth.MaxAttempts,
			OAuth2Enabled:     len(cfg.Auth.OAuth2) > 0,
			DisablePlainAuth:  svcs.POP3.DisablePlainAuth,
			AnvilAddr:         cfg.AnvilService.ClientAddr(),
			AnvilTLS:          intTLS,
			AnvilFailOpen:     cfg.AnvilService.FailOpen,
			DialRetries:       cfg.General.StartupDialRetries,
			LookupHoldMax:     cfg.Login.LookupHoldMax,
			LookupHoldBackoff: time.Duration(cfg.Login.LookupHoldBackoffMs) * time.Millisecond,
			HAProxy:           svcs.POP3.HAProxy,
			HAProxyTimeout:    haproxyTimeout,
			HAProxyNets:       haproxyNets,
			XClient:           svcs.POP3.XClient,
			XClientNets:       xclientNets,
		})
		go func() {
			slog.Error("pop3-login: server error", "err", srv.Serve(ln))
			os.Exit(1)
		}()
		if dirAddr != "" {
			go srv.Watch(ctx)
		}
		slog.Info("pop3-login: listening", "addr", addr, "tls", "starttls")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
	cancel()
	slog.Info("yarilo-pop3-login stopped")
}

func parseCIDRs(ss []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("pop3-login: invalid CIDR", "cidr", s, "err", err)
			continue
		}
		nets = append(nets, n)
	}
	return nets
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
