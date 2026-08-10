// yarilo-lmtp-login is the LMTP login proxy for the yarilo mail server.
// It accepts MTA connections (e.g. from Postfix), performs per-recipient
// warden CONNECT and yarilo-auth SESSION token issuance, then fans out one
// backend LMTP connection per recipient — each preceded by a YARILO preamble.
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

	"github.com/yarilomail/yarilo/internal/lmtplogin"
	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

func main() {
	logging.Setup("lmtp-login")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	if !cfg.Services.LMTP.Active() {
		slog.Error("no LMTP listener configured (services.lmtp must be enabled)")
		os.Exit(1)
	}

	lmtpCfg := cfg.LMTPLoginService
	if lmtpCfg.BackendAddr == "" && lmtpCfg.DirectorAddr == "" {
		slog.Error("lmtp_login_service: set either backend_addr (standalone) or director_addr (director mode)")
		os.Exit(1)
	}

	// #741: backend_addr wins when both are set, unifying with the other
	// four login components' existing precedence (internal/login) — lmtp-login
	// previously inverted this (director_addr won). Warn loudly since flipping
	// the winner is a silent behavior change for any deploy that set both.
	if lmtpCfg.BackendAddr != "" && lmtpCfg.DirectorAddr != "" {
		slog.Warn("lmtp_login_service: both backend_addr and director_addr set; backend_addr wins per unified precedence (#741) — remove backend_addr to keep director routing")
	}
	mode := lmtpCfg.BackendAddr
	if mode == "" && lmtpCfg.DirectorAddr != "" {
		mode = "director:" + lmtpCfg.DirectorAddr
	}
	slog.Info("yarilo-lmtp-login starting",
		"version", build.Version,
		"backend", mode,
		"telemetry", cfg.Telemetry.Listen,
	)

	hostname := cfg.Protocol.Submission.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}

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

	opts := lmtplogin.Options{
		Hostname:         hostname,
		BackendAddr:      lmtpCfg.BackendAddr,
		DirectorAddr:     lmtpCfg.DirectorAddr,
		DirectorTLS:      intTLS,
		BackendTLS:       intTLS,
		DirectorTag:      lmtpCfg.DirectorTag,
		BackendPort:      lmtpCfg.BackendPort,
		LocalIP:          os.Getenv("POD_IP"),
		AuthMasterAddr:   cfg.AuthService.MasterAddr,
		AuthMasterTLS:    intTLS,
		WardenAddr:       cfg.WardenService.ClientAddr(),
		WardenTLS:        intTLS,
		ConcurrencyLimit: cfg.Protocol.LMTP.UserConcurrencyLimit,
		// Inbound client-IP forwarding (#742): a Postfix relay in front conveys
		// the original SMTP client's IP via PROXY protocol and/or XCLIENT.
		HAProxy:        cfg.Services.LMTP.HAProxy,
		HAProxyTimeout: time.Duration(cfg.General.HAProxy.Timeout) * time.Second,
		HAProxyNets:    parseCIDRs(cfg.General.HAProxy.TrustedNets),
		XClient:        cfg.Services.LMTP.XClient,
		XClientNets:    parseCIDRs(cfg.General.XClient.TrustedNets),
	}

	addr := fmt.Sprintf(":%d", cfg.Services.LMTP.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("lmtp-login: listen failed", "addr", addr, "err", err)
		os.Exit(1)
	}

	srv := lmtplogin.New(opts)
	go func() {
		slog.Error("lmtp-login: server error", "err", srv.Serve(ln))
		os.Exit(1)
	}()
	slog.Info("lmtp-login: listening", "addr", addr)

	tel := startTelemetry(cfg.Telemetry)

	// Every configured port is bound and serving now, so the pod can accept
	// clients. Reporting earlier would let Kubernetes route to a port that is not
	// listening yet.
	tel.SetReady(true)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
	// Leave the Service endpoints before draining, so no new client is routed
	// here while in-flight work finishes.
	tel.SetReady(false)
}

func parseCIDRs(ss []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("lmtp-login: invalid CIDR", "cidr", s, "err", err)
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
		Addr:      telemetry.Addr(cfg.Listen),
		Lifecycle: true,
		Pprof: telemetry.PprofOptions{
			Enabled:       cfg.PprofEnabled,
			Heap:          cfg.PprofHeapEnabled,
			BlockRate:     cfg.PprofBlockProfileRate,
			MutexFraction: cfg.PprofMutexProfileFraction,
		},
	})
	go func() {
		if err := tel.ListenAndServe(context.Background()); err != nil {
			slog.Error("telemetry server failed", "err", err)
		}
	}()
	return tel
}
