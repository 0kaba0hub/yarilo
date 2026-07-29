// yarilo-imap-login is the IMAP/IMAPS login proxy for the yarilo mail server.
// It accepts mail-client connections on port 993 (implicit TLS) and port 143
// (STARTTLS), handles the IMAP pre-auth exchange to learn the username, queries
// yarilo-director for the backend pod, and proxies the authenticated session.
// TLS is terminated here; yarilo-imap backends receive plain TCP.
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
	})).With("service", "imap-login"))

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
	if !svcs.IMAPS.Active() && !svcs.IMAP.Active() {
		slog.Error("no IMAP listener configured (imap or imaps must be enabled)")
		os.Exit(1)
	}

	slog.Info("yarilo-imap-login starting",
		"version", build.Version,
		"telemetry", cfg.Telemetry.Listen,
		"backend_addr", cfg.IMAPLoginService.BackendAddr,
		"director_addr", cfg.IMAPLoginService.DirectorAddr,
	)

	// External TLS (client-facing cert) for IMAPS / STARTTLS.
	var extTLS *tls.Config
	if cfg.General.SSL.TLSCert != "" && cfg.General.SSL.TLSKey != "" {
		extTLS, err = config.BuildTLSConfig(cfg.General.SSL)
		if err != nil {
			slog.Error("TLS config failed", "err", err)
			os.Exit(1)
		}
		extTLS.NextProtos = []string{"imap"}
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
	haproxyTimeout := time.Duration(cfg.General.HAProxy.Timeout) * time.Second
	xclientNets := parseCIDRs(cfg.General.XClient.TrustedNets)
	localIP := os.Getenv("POD_IP")
	// dirAddr must be the REMOTE yarilo-director service address
	// (imap_login_service.director_addr), never cfg.DirectorService.Listen
	// — that's this process's own in-process director bind address, used
	// only by standalone deployments that embed the director locally; a
	// k8s director deployment has no director running in THIS pod at all,
	// so falling back to it silently dialed localhost where nothing
	// listens (#735).
	if err := config.ValidateBackendOrDirector("imap_login_service", cfg.IMAPLoginService.BackendAddr, cfg.IMAPLoginService.DirectorAddr); err != nil {
		slog.Error("config validation failed", "err", err)
		os.Exit(1)
	}
	dirAddr := cfg.IMAPLoginService.DirectorAddr

	// ctx drives the per-listener director watch (#736): the persistent
	// connection that delivers USER-KICKED pushes so kicks actually reach this
	// login pod's sessions. Cancelled on shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var loginServers []*login.Server

	go runTelemetry(cfg.Telemetry.Listen)

	// Port 993 — implicit TLS (IMAPS).
	if svcs.IMAPS.Active() {
		addr := fmt.Sprintf(":%d", svcs.IMAPS.Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("imaps-login: listen failed", "addr", addr, "err", err)
			os.Exit(1)
		}
		srv := login.New(login.Options{
			Protocol:          login.ProtocolIMAPS,
			DirectorAddr:      dirAddr,
			BackendAddr:       cfg.IMAPLoginService.BackendAddr,
			BackendPort:       cfg.IMAPLoginService.BackendPort,
			Tag:               cfg.IMAPLoginService.DirectorTag,
			DirectorTLS:       intTLS,
			LocalIP:           localIP,
			BackendTLS:        intTLS,
			ExtTLS:            extTLS,
			AuthAddr:          cfg.AuthService.ClientAddr(),
			AuthTLS:           intTLS,
			AuthMaxAttempts:   cfg.Auth.MaxAttempts,
			OAuth2Enabled:     len(cfg.Auth.OAuth2) > 0,
			DisablePlainAuth:  svcs.IMAPS.DisablePlainAuth,
			AnvilAddr:         cfg.AnvilService.ClientAddr(),
			AnvilTLS:          intTLS,
			AnvilFailOpen:     cfg.AnvilService.FailOpen,
			AnvilConns:        cfg.AnvilService.Conns,
			DialRetries:       cfg.General.StartupDialRetries,
			LookupHoldMax:     cfg.Login.LookupHoldMax,
			LookupHoldBackoff: time.Duration(cfg.Login.LookupHoldBackoffMs) * time.Millisecond,
			HAProxy:           svcs.IMAPS.HAProxy,
			HAProxyTimeout:    haproxyTimeout,
			HAProxyNets:       haproxyNets,
			XClient:           svcs.IMAPS.XClient,
			XClientNets:       xclientNets,
		})
		loginServers = append(loginServers, srv)
		go func(srv *login.Server, ln net.Listener) {
			if err := srv.Serve(ln); err != nil {
				slog.Error("imaps-login: server error", "err", err)
				os.Exit(1)
			}
		}(srv, ln)
		if dirAddr != "" {
			go srv.Watch(ctx)
		}
		slog.Info("imap-login: listening", "addr", addr, "tls", "implicit")
	}

	// Port 143 — STARTTLS.
	if svcs.IMAP.Active() {
		addr := fmt.Sprintf(":%d", svcs.IMAP.Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("imap-login: listen failed", "addr", addr, "err", err)
			os.Exit(1)
		}
		srv := login.New(login.Options{
			Protocol:          login.ProtocolIMAP,
			DirectorAddr:      dirAddr,
			BackendAddr:       cfg.IMAPLoginService.BackendAddr,
			BackendPort:       cfg.IMAPLoginService.BackendPort,
			Tag:               cfg.IMAPLoginService.DirectorTag,
			DirectorTLS:       intTLS,
			LocalIP:           localIP,
			BackendTLS:        intTLS,
			StarttlsTLS:       extTLS,
			AuthAddr:          cfg.AuthService.ClientAddr(),
			AuthTLS:           intTLS,
			OAuth2Enabled:     len(cfg.Auth.OAuth2) > 0,
			DisablePlainAuth:  svcs.IMAP.DisablePlainAuth,
			AnvilAddr:         cfg.AnvilService.ClientAddr(),
			AnvilTLS:          intTLS,
			AnvilFailOpen:     cfg.AnvilService.FailOpen,
			AnvilConns:        cfg.AnvilService.Conns,
			DialRetries:       cfg.General.StartupDialRetries,
			LookupHoldMax:     cfg.Login.LookupHoldMax,
			LookupHoldBackoff: time.Duration(cfg.Login.LookupHoldBackoffMs) * time.Millisecond,
			HAProxy:           svcs.IMAP.HAProxy,
			HAProxyTimeout:    haproxyTimeout,
			HAProxyNets:       haproxyNets,
			XClient:           svcs.IMAP.XClient,
			XClientNets:       xclientNets,
		})
		loginServers = append(loginServers, srv)
		go func(srv *login.Server, ln net.Listener) {
			if err := srv.Serve(ln); err != nil {
				slog.Error("imap-login: server error", "err", err)
				os.Exit(1)
			}
		}(srv, ln)
		if dirAddr != "" {
			go srv.Watch(ctx)
		}
		slog.Info("imap-login: listening", "addr", addr, "tls", "starttls")
	}

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
	slog.Info("yarilo-imap-login stopped")
}

func parseCIDRs(ss []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("imap-login: invalid CIDR", "cidr", s, "err", err)
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
