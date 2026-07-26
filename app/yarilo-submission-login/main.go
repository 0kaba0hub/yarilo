// yarilo-submission-login is the SMTP submission login proxy for the yarilo mail server.
// It accepts client connections on port 587 (STARTTLS) and port 465 (implicit TLS),
// handles the SMTP AUTH exchange, queries yarilo-director for the backend pod, and
// proxies the authenticated SMTP session (MAIL FROM / RCPT TO / DATA).
// TLS is terminated here; yarilo-submission backends receive plain TCP.
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
	})).With("service", "submission-login"))

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
	if !svcs.Submission.Active() && !svcs.Submissions.Active() {
		slog.Error("no submission listener configured (submission or submissions must be enabled)")
		os.Exit(1)
	}

	slog.Info("yarilo-submission-login starting",
		"version", build.Version,
		"telemetry", cfg.Telemetry.Listen,
		"backend_addr", cfg.SubmissionLoginSvc.BackendAddr,
		"director_addr", cfg.SubmissionLoginSvc.DirectorAddr,
	)

	// External TLS (client-facing cert) for Submissions / STARTTLS.
	var extTLS *tls.Config
	if cfg.General.SSL.TLSCert != "" && cfg.General.SSL.TLSKey != "" {
		extTLS, err = config.BuildTLSConfig(cfg.General.SSL)
		if err != nil {
			slog.Error("TLS config failed", "err", err)
			os.Exit(1)
		}
		extTLS.NextProtos = []string{"smtp"}
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
	// See yarilo-imap-login/main.go for why this must be the component's
	// own director_addr, not cfg.DirectorService.Listen (#735).
	if err := config.ValidateBackendOrDirector("submission_login_service", cfg.SubmissionLoginSvc.BackendAddr, cfg.SubmissionLoginSvc.DirectorAddr); err != nil {
		slog.Error("config validation failed", "err", err)
		os.Exit(1)
	}
	dirAddr := cfg.SubmissionLoginSvc.DirectorAddr

	// ctx drives the per-listener director watch (#736) so USER-KICKED pushes
	// reach this pod's sessions. Cancelled on shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runTelemetry(cfg.Telemetry.Listen)

	// Port 465 — implicit TLS (Submissions).
	if svcs.Submissions.Active() {
		addr := fmt.Sprintf(":%d", svcs.Submissions.Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("submissions-login: listen failed", "addr", addr, "err", err)
			os.Exit(1)
		}
		srv := login.New(login.Options{
			Protocol:         login.ProtocolSubmissions,
			DirectorAddr:     dirAddr,
			BackendAddr:      cfg.SubmissionLoginSvc.BackendAddr,
			BackendPort:      cfg.SubmissionLoginSvc.BackendPort,
			Tag:              cfg.SubmissionLoginSvc.DirectorTag,
			DirectorTLS:      intTLS,
			LocalIP:          localIP,
			BackendTLS:       intTLS,
			ExtTLS:           extTLS,
			AuthAddr:         cfg.AuthService.ClientAddr(),
			AuthTLS:          intTLS,
			AuthMaxAttempts:  cfg.Auth.MaxAttempts,
			OAuth2Enabled:    len(cfg.Auth.OAuth2) > 0,
			DisablePlainAuth: svcs.Submissions.DisablePlainAuth,
			AnvilAddr:        cfg.AnvilService.ClientAddr(),
			AnvilTLS:         intTLS,
			AnvilFailOpen:    cfg.AnvilService.FailOpen,
			DialRetries:      cfg.General.StartupDialRetries,
			HAProxy:          svcs.Submissions.HAProxy,
			HAProxyTimeout:   haproxyTimeout,
			HAProxyNets:      haproxyNets,
		})
		go func() {
			slog.Error("submissions-login: server error", "err", srv.Serve(ln))
			os.Exit(1)
		}()
		if dirAddr != "" {
			go srv.Watch(ctx)
		}
		slog.Info("submission-login: listening", "addr", addr, "tls", "implicit")
	}

	// Port 587 — STARTTLS.
	if svcs.Submission.Active() {
		addr := fmt.Sprintf(":%d", svcs.Submission.Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("submission-login: listen failed", "addr", addr, "err", err)
			os.Exit(1)
		}
		srv := login.New(login.Options{
			Protocol:         login.ProtocolSubmission,
			DirectorAddr:     dirAddr,
			BackendAddr:      cfg.SubmissionLoginSvc.BackendAddr,
			BackendPort:      cfg.SubmissionLoginSvc.BackendPort,
			Tag:              cfg.SubmissionLoginSvc.DirectorTag,
			DirectorTLS:      intTLS,
			LocalIP:          localIP,
			BackendTLS:       intTLS,
			StarttlsTLS:      extTLS,
			AuthAddr:         cfg.AuthService.ClientAddr(),
			AuthTLS:          intTLS,
			AuthMaxAttempts:  cfg.Auth.MaxAttempts,
			OAuth2Enabled:    len(cfg.Auth.OAuth2) > 0,
			DisablePlainAuth: svcs.Submission.DisablePlainAuth,
			AnvilAddr:        cfg.AnvilService.ClientAddr(),
			AnvilTLS:         intTLS,
			AnvilFailOpen:    cfg.AnvilService.FailOpen,
			DialRetries:      cfg.General.StartupDialRetries,
			HAProxy:          svcs.Submission.HAProxy,
			HAProxyTimeout:   haproxyTimeout,
			HAProxyNets:      haproxyNets,
		})
		go func() {
			slog.Error("submission-login: server error", "err", srv.Serve(ln))
			os.Exit(1)
		}()
		if dirAddr != "" {
			go srv.Watch(ctx)
		}
		slog.Info("submission-login: listening", "addr", addr, "tls", "starttls")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
	cancel()
	slog.Info("yarilo-submission-login stopped")
}

func parseCIDRs(ss []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("submission-login: invalid CIDR", "cidr", s, "err", err)
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
