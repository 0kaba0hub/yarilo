// yarilo-pop3-login-backend is the POP3 backend login sidecar.
// It runs in the same pod as yarilo-pop3, accepts connections from
// yarilo-pop3-login frontend pods (via mTLS), performs the XCLIENT handshake
// to propagate the real client IP, then proxies the session to yarilo-pop3.
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

	"github.com/0kaba0hub/yarilo/internal/login"
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

	lb := cfg.LoginBackend.POP3
	if !lb.Enabled {
		slog.Error("pop3 login backend not enabled (login_backend.pop3.enabled must be true)")
		os.Exit(1)
	}

	slog.Info("yarilo-pop3-login-backend starting",
		"version", version,
		"telemetry", cfg.Telemetry.Listen,
		"port", lb.Port,
		"session_addr", lb.SessionAddr,
	)

	var extTLS *tls.Config
	if cfg.InternalTLS.Enabled {
		extTLS, err = mtls.ServerConfig(
			cfg.InternalTLS.Cert,
			cfg.InternalTLS.Key,
			cfg.InternalTLS.CA,
		)
		if err != nil {
			slog.Error("internal TLS server config failed", "err", err)
			os.Exit(1)
		}
	}

	addr := fmt.Sprintf(":%d", lb.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("pop3-login-backend: listen failed", "addr", addr, "err", err)
		os.Exit(1)
	}

	sidecar := login.NewSidecar(login.SidecarOptions{
		Protocol:    login.ProtocolPOP3S,
		ExtTLS:      extTLS,
		SessionAddr: lb.SessionAddr,
	})
	go func() {
		if err := sidecar.Serve(ln); err != nil {
			slog.Error("pop3-login-backend: server error", "err", err)
			os.Exit(1)
		}
	}()
	slog.Info("pop3-login-backend: listening", "addr", addr)

	go runTelemetry(cfg.Telemetry.Listen)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
	slog.Info("yarilo-pop3-login-backend stopped")
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
