// yarilo-managesieve-login is the ManageSieve (RFC 5804) login proxy.
// It accepts client connections on port 4190, speaks the pre-auth ManageSieve
// exchange (CAPABILITY, AUTHENTICATE PLAIN, STARTTLS), and proxies the
// authenticated session to yarilo-managesieve backends.
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
	"github.com/0kaba0hub/yarilo/internal/sieve"
	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/logging"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

func main() {
	logging.Setup("managesieve-login")

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
	if !svcs.ManageSieve.Active() {
		slog.Error("no ManageSieve listener configured (managesieve must be enabled)")
		os.Exit(1)
	}

	slog.Info("yarilo-managesieve-login starting",
		"version", build.Version,
		"telemetry", cfg.Telemetry.Listen,
		"backend_addr", cfg.ManageSieveLoginService.BackendAddr,
		"director_addr", cfg.ManageSieveLoginService.DirectorAddr,
	)

	// Client-facing TLS for STARTTLS on port 4190.
	var extTLS *tls.Config
	if cfg.General.SSL.TLSCert != "" && cfg.General.SSL.TLSKey != "" {
		extTLS, err = config.BuildTLSConfig(cfg.General.SSL)
		if err != nil {
			slog.Error("TLS config failed", "err", err)
			os.Exit(1)
		}
		extTLS.NextProtos = []string{"managesieve"}
	}

	// Internal mTLS for backend connections.
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

	haproxyNets := parseCIDRs(cfg.ManageSieveLoginService.HAProxyNets)
	haproxyTimeout := time.Duration(cfg.ManageSieveLoginService.HAProxyTimeout) * time.Second
	localIP := os.Getenv("POD_IP")
	// #735: managesieve-login never wired DirectorAddr at all (BackendAddr
	// was the only option) — director mode was silently unavailable, not
	// just misdirected to localhost like the other three logins.
	if err := config.ValidateBackendOrDirector("managesieve_login_service", cfg.ManageSieveLoginService.BackendAddr, cfg.ManageSieveLoginService.DirectorAddr); err != nil {
		slog.Error("config validation failed", "err", err)
		os.Exit(1)
	}

	go runTelemetry(cfg.Telemetry.Listen)

	// Port 4190 — STARTTLS (ManageSieve does not have an implicit-TLS variant).
	addr := fmt.Sprintf(":%d", svcs.ManageSieve.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("managesieve-login: listen failed", "addr", addr, "err", err)
		os.Exit(1)
	}
	var loginServers []*login.Server
	srv := login.New(login.Options{
		Protocol:            login.ProtocolManageSieve,
		DirectorAddr:        cfg.ManageSieveLoginService.DirectorAddr,
		DirectorTLS:         intTLS,
		BackendAddr:         cfg.ManageSieveLoginService.BackendAddr,
		BackendPort:         cfg.ManageSieveLoginService.BackendPort,
		Tag:                 cfg.ManageSieveLoginService.DirectorTag,
		LocalIP:             localIP,
		BackendTLS:          intTLS,
		StarttlsTLS:         extTLS,
		AuthAddr:            cfg.AuthService.ClientAddr(),
		AuthTLS:             intTLS,
		AuthMaxAttempts:     cfg.Auth.MaxAttempts,
		DisablePlainAuth:    svcs.ManageSieve.DisablePlainAuth,
		SieveExtensions:     strings.Join(sieve.SupportedExtensions, " "),
		SieveMaxInvalidCmds: cfg.Protocol.ManageSieve.MaxInvalidCommands,
		AnvilAddr:           cfg.AnvilService.ClientAddr(),
		AnvilTLS:            intTLS,
		AnvilFailOpen:       cfg.AnvilService.FailOpen,
		AnvilConns:          cfg.AnvilService.Conns,
		DialRetries:         cfg.General.StartupDialRetries,
		LookupHoldMax:       cfg.Login.LookupHoldMax,
		LookupHoldBackoff:   time.Duration(cfg.Login.LookupHoldBackoffMs) * time.Millisecond,
		HAProxy:             cfg.ManageSieveLoginService.HAProxy,
		HAProxyTimeout:      haproxyTimeout,
		HAProxyNets:         haproxyNets,
	})
	loginServers = append(loginServers, srv)
	go func(srv *login.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil {
			slog.Error("managesieve-login: server error", "err", err)
			os.Exit(1)
		}
	}(srv, ln)
	// Persistent director watch (#736): delivers USER-KICKED so kicks reach
	// this pod's sessions. Cancelled on shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if cfg.ManageSieveLoginService.DirectorAddr != "" {
		go srv.Watch(ctx)
	}
	slog.Info("managesieve-login: listening", "addr", addr, "tls", "starttls")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
	sgrace := cfg.Login.SessionGracePeriod
	if sgrace <= 0 {
		sgrace = 30
	}
	sctx, scancel := context.WithTimeout(context.Background(), time.Duration(sgrace)*time.Second)
	for _, srv := range loginServers {
		_ = srv.Shutdown(sctx)
	}
	scancel()
	cancel()
	slog.Info("yarilo-managesieve-login stopped")
}

func parseCIDRs(ss []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("managesieve-login: invalid CIDR", "cidr", s, "err", err)
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
