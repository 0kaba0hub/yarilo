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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yarilomail/yarilo/internal/login"
	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	logging.Setup("imap-login")

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

	// A listener that declares TLS without a certificate must not bind silently.
	if warn, err := config.CheckListenerTLS("services.imaps", svcs.IMAPS, cfg.General.SSL); err != nil {
		slog.Error("listener TLS check failed", "err", err)
		os.Exit(1)
	} else if warn != "" {
		slog.Warn(warn)
	}
	if warn, err := config.CheckListenerTLS("services.imap", svcs.IMAP, cfg.General.SSL); err != nil {
		slog.Error("listener TLS check failed", "err", err)
		os.Exit(1)
	} else if warn != "" {
		slog.Warn(warn)
	}

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

	tel := startTelemetry(cfg.Telemetry)

	// Port 993 — implicit TLS (IMAPS).
	if svcs.IMAPS.Active() {
		addr := fmt.Sprintf(":%d", svcs.IMAPS.Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("imaps-login: listen failed", "addr", addr, "err", err)
			os.Exit(1)
		}
		srv := login.New(login.Options{
			Protocol:            login.ProtocolIMAPS,
			DirectorAddr:        dirAddr,
			BackendAddr:         cfg.IMAPLoginService.BackendAddr,
			BackendPort:         cfg.IMAPLoginService.BackendPort,
			Tag:                 cfg.IMAPLoginService.DirectorTag,
			DirectorTLS:         intTLS,
			LocalIP:             localIP,
			BackendTLS:          intTLS,
			ExtTLS:              extTLS,
			AuthAddr:            cfg.AuthService.ClientAddr(),
			AuthTLS:             intTLS,
			AuthMaxAttempts:     cfg.Auth.MaxAttempts,
			OAuth2Enabled:       len(cfg.Auth.OAuth2) > 0,
			DisablePlainAuth:    svcs.IMAPS.DisablePlainAuth,
			WardenAddr:          cfg.WardenService.ClientAddr(),
			WardenTLS:           intTLS,
			WardenFailOpen:      cfg.WardenService.FailOpen,
			WardenConns:         cfg.WardenService.Conns,
			DialRetries:         cfg.General.StartupDialRetries,
			LookupHoldMax:       cfg.Login.LookupHoldMax,
			TransientRetries:    cfg.Login.TransientRetries,
			TransientReloginCap: cfg.Login.TransientReloginCap,
			LookupHoldBackoff:   time.Duration(cfg.Login.LookupHoldBackoffMs) * time.Millisecond,
			HAProxy:             svcs.IMAPS.HAProxy,
			HAProxyTimeout:      haproxyTimeout,
			HAProxyNets:         haproxyNets,
			XClient:             svcs.IMAPS.XClient,
			XClientNets:         xclientNets,
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
			WardenAddr:        cfg.WardenService.ClientAddr(),
			WardenTLS:         intTLS,
			WardenFailOpen:    cfg.WardenService.FailOpen,
			WardenConns:       cfg.WardenService.Conns,
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

	// Every configured port is bound and serving now, so the pod can accept
	// clients. Reporting earlier would let Kubernetes route to a port that is not
	// listening yet.
	tel.SetReady(true)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
	// Leave the Service endpoints before draining, so no new client is routed
	// here while in-flight sessions finish.
	tel.SetReady(false)
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

// startTelemetry serves /healthz, /readyz, /metrics and /debug/loglevel, and
// returns the server so the caller can report readiness once its listeners are
// actually bound.
//
// Lifecycle is on: without it /readyz answers 200 from the moment the process
// starts, which says nothing. With it, ready means this pod holds its ports.
func startTelemetry(cfg config.TelemetryConfig) *telemetry.Server {
	tel := telemetry.NewWithOptions(telemetry.Options{
		Addr:      cfg.Listen,
		Lifecycle: true,
		Pprof: telemetry.PprofOptions{
			Enabled: cfg.PprofEnabled,
			Heap:    cfg.PprofHeapEnabled,
		},
	})
	go func() {
		if err := tel.ListenAndServe(context.Background()); err != nil {
			slog.Error("telemetry server failed", "err", err)
		}
	}()
	return tel
}
