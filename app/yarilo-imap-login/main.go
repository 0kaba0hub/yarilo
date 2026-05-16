// yarilo-imap-login is the IMAP/IMAPS login proxy for the yarilo mail server.
// It accepts mail-client connections on port 993 (implicit TLS) and port 143
// (STARTTLS), handles the IMAP pre-auth exchange to learn the username, queries
// yarilo-director for the backend pod, and proxies the authenticated session.
// TLS is terminated here; yarilo-imap backends receive plain TCP.
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
	"time"

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

	svcs := cfg.Services
	if !svcs.IMAPS.Active() && !svcs.IMAP.Active() {
		slog.Error("no IMAP listener configured (imap or imaps must be enabled)")
		os.Exit(1)
	}

	slog.Info("yarilo-imap-login starting",
		"version", version,
		"telemetry", cfg.Telemetry.Listen,
		"director", cfg.DirectorService.Listen,
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
		)
		if err != nil {
			slog.Error("internal TLS config failed", "err", err)
			os.Exit(1)
		}
	}

	haproxyNets := parseCIDRs(cfg.General.HAProxy.TrustedNets)
	haproxyTimeout := time.Duration(cfg.General.HAProxy.Timeout) * time.Second
	localIP := os.Getenv("POD_IP")
	dirAddr := cfg.DirectorService.Listen

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
			Protocol:       login.ProtocolIMAPS,
			DirectorAddr:   dirAddr,
			DirectorTLS:    intTLS,
			LocalIP:        localIP,
			BackendTLS:     intTLS,
			ExtTLS:         extTLS,
			AnvilAddr:      cfg.AnvilService.Listen,
			AnvilTLS:       intTLS,
			AnvilFailOpen:  cfg.AnvilService.FailOpen,
			HAProxy:        svcs.IMAPS.HAProxy,
			HAProxyTimeout: haproxyTimeout,
			HAProxyNets:    haproxyNets,
		})
		go func() {
			if err := srv.Serve(ln); err != nil {
				slog.Error("imaps-login: server error", "err", err)
				os.Exit(1)
			}
		}()
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
			Protocol:       login.ProtocolIMAP,
			DirectorAddr:   dirAddr,
			DirectorTLS:    intTLS,
			LocalIP:        localIP,
			BackendTLS:     intTLS,
			StarttlsTLS:    extTLS,
			AnvilAddr:      cfg.AnvilService.Listen,
			AnvilTLS:       intTLS,
			AnvilFailOpen:  cfg.AnvilService.FailOpen,
			HAProxy:        svcs.IMAP.HAProxy,
			HAProxyTimeout: haproxyTimeout,
			HAProxyNets:    haproxyNets,
		})
		go func() {
			if err := srv.Serve(ln); err != nil {
				slog.Error("imap-login: server error", "err", err)
				os.Exit(1)
			}
		}()
		slog.Info("imap-login: listening", "addr", addr, "tls", "starttls")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
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
