// yarilo-lmtp-login is the LMTP login proxy for the yarilo mail server.
// It accepts MTA connections (e.g. from Postfix), performs per-recipient
// anvil CONNECT and yarilo-auth SESSION token issuance, then fans out one
// backend LMTP connection per recipient — each preceded by a YARILO preamble.
package main

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/0kaba0hub/yarilo/internal/lmtplogin"
	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})).With("service", "lmtp-login"))

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

	mode := lmtpCfg.BackendAddr
	if lmtpCfg.DirectorAddr != "" {
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
		DirectorTag:      lmtpCfg.DirectorTag,
		BackendPort:      lmtpCfg.BackendPort,
		LocalIP:          os.Getenv("POD_IP"),
		AuthMasterAddr:   cfg.AuthService.MasterAddr,
		AuthMasterTLS:    intTLS,
		AnvilAddr:        cfg.AnvilService.ClientAddr(),
		AnvilTLS:         intTLS,
		ConcurrencyLimit: cfg.Protocol.LMTP.UserConcurrencyLimit,
	}

	addr := fmt.Sprintf(":%d", cfg.Services.LMTP.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("lmtp-login: listen failed", "addr", addr, "err", err)
		os.Exit(1)
	}

	srv := lmtplogin.New(opts)
	go func() {
		if err := srv.Serve(ln); err != nil {
			slog.Error("lmtp-login: server error", "err", err)
			os.Exit(1)
		}
	}()
	slog.Info("lmtp-login: listening", "addr", addr)

	go runTelemetry(cfg.Telemetry.Listen)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
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
