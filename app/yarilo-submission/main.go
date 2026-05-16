// yarilo-submission is the SMTP submission proxy for the yarilo mail server.
// It accepts client connections on port 587 (STARTTLS) and port 465 (implicit TLS),
// authenticates via the configured passdb chain, and relays mail to the upstream MTA.
// No mailbox access — purely a proxy between mail clients and the upstream MTA.
package main

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	authsql "github.com/0kaba0hub/yarilo/internal/auth/sql"
	submsvr "github.com/0kaba0hub/yarilo/internal/submission"
	submproxy "github.com/0kaba0hub/yarilo/internal/submission/proxy"
	"github.com/0kaba0hub/yarilo/pkg/config"
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
	if !svcs.Submission.Active() && !svcs.Submissions.Active() {
		slog.Error("no submission listener configured (submission or submissions must be enabled)")
		os.Exit(1)
	}

	slog.Info("yarilo-submission starting",
		"version", version,
		"telemetry", cfg.Telemetry.Listen,
	)

	// ---- auth chain ----
	var dbs []protocol.Passdb
	for _, entry := range cfg.Auth.Passdb {
		db, err := authsql.New(authsql.Config{
			Driver:            entry.Driver,
			DSN:               entry.DSN,
			PasswordQuery:     entry.PasswordQuery,
			UserQuery:         entry.UserQuery,
			IterateQuery:      entry.IterateQuery,
			DefaultPassScheme: entry.DefaultPassScheme,
			SkipSchema:        entry.SkipSchema,
		})
		if err != nil {
			slog.Error("passdb init failed", "driver", entry.Driver, "err", err)
			os.Exit(1)
		}
		dbs = append(dbs, db)
	}

	// ---- relay proxy ----
	var relay *submproxy.Submission
	if cfg.Protocol.Submission.Relay.Host != "" {
		relay = submproxy.New(cfg.Protocol.Submission.Relay, cfg.Protocol.Submission.Hostname)
	}

	// ---- TLS ----
	var extTLS *tls.Config
	if cfg.General.SSL.TLSCert != "" && cfg.General.SSL.TLSKey != "" {
		extTLS, err = config.BuildTLSConfig(cfg.General.SSL)
		if err != nil {
			slog.Error("TLS config failed", "err", err)
			os.Exit(1)
		}
		extTLS.NextProtos = []string{"smtp"}
	}

	haproxyNets := parseCIDRs(cfg.General.HAProxy.TrustedNets)
	xclientNets := parseCIDRs(cfg.General.XClient.TrustedNets)
	haproxyTimeout := time.Duration(cfg.General.HAProxy.Timeout) * time.Second

	primary := firstActive(svcs.Submission, svcs.Submissions)
	srv := submsvr.New(submsvr.Options{
		HAProxy:          primary.HAProxy,
		HAProxyTimeout:   haproxyTimeout,
		HAProxyNets:      haproxyNets,
		XClient:          primary.XClient,
		XClientNets:      xclientNets,
		DisablePlainAuth: primary.DisablePlainAuth,
		TLSConfig:        extTLS,
		Config:           cfg.Protocol.Submission,
		Auth:             chainAuth{protocol.Chain(dbs)},
		Proxy:            relay,
	})

	go runTelemetry(cfg.Telemetry.Listen)

	// port 587 — STARTTLS
	if svcs.Submission.Active() {
		addr := fmt.Sprintf(":%d", svcs.Submission.Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("submission: listen failed", "addr", addr, "err", err)
			os.Exit(1)
		}
		go func() {
			if err := srv.Serve(ln, nil); err != nil {
				slog.Error("submission: server error", "err", err)
				os.Exit(1)
			}
		}()
		slog.Info("submission: listening", "addr", addr, "tls", "starttls")
	}

	// port 465 — implicit TLS
	if svcs.Submissions.Active() {
		addr := fmt.Sprintf(":%d", svcs.Submissions.Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("submissions: listen failed", "addr", addr, "err", err)
			os.Exit(1)
		}
		go func() {
			if err := srv.Serve(ln, extTLS); err != nil {
				slog.Error("submissions: server error", "err", err)
				os.Exit(1)
			}
		}()
		slog.Info("submission: listening", "addr", addr, "tls", "implicit")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
	slog.Info("yarilo-submission stopped")
}

// chainAuth adapts protocol.Chain to submission.Authenticator.
type chainAuth struct{ c protocol.Chain }

func (a chainAuth) AuthPlain(username, password string) error {
	resp, err := a.c.Authenticate(username, password, "smtp")
	if err != nil {
		return fmt.Errorf("smtp/auth: %w", err)
	}
	if resp == nil || resp.Result != protocol.AuthOK {
		return fmt.Errorf("smtp/auth: authentication failed")
	}
	return nil
}

func parseCIDRs(ss []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("submission: invalid CIDR", "cidr", s, "err", err)
			continue
		}
		nets = append(nets, n)
	}
	return nets
}

func firstActive(svcs ...*config.ServiceConfig) *config.ServiceConfig {
	for _, s := range svcs {
		if s != nil && s.Enabled {
			return s
		}
	}
	return &config.ServiceConfig{}
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
