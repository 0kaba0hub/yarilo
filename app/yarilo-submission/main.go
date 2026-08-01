// yarilo-submission is the SMTP submission proxy for the yarilo mail server.
// It accepts client connections on port 587 (STARTTLS) and port 465 (implicit TLS),
// authenticates via the configured passdb chain, and relays mail to the upstream MTA.
// No mailbox access — purely a proxy between mail clients and the upstream MTA.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/emersion/go-sasl"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	authsql "github.com/0kaba0hub/yarilo/internal/auth/sql"
	"github.com/0kaba0hub/yarilo/internal/readyfile"
	submsvr "github.com/0kaba0hub/yarilo/internal/submission"
	submproxy "github.com/0kaba0hub/yarilo/internal/submission/proxy"
	"github.com/0kaba0hub/yarilo/internal/telemetry"
	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/logging"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	logging.Setup("submission")

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
		"version", build.Version,
		"telemetry", telemetry.Addr(cfg.Telemetry.Listen), // resolved (honours TELEMETRY_LISTEN)
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

	authCache := protocol.NewCache(
		cfg.Auth.Cache.CacheSizeBytes(),
		time.Duration(cfg.Auth.Cache.TTLSeconds)*time.Second,
		time.Duration(cfg.Auth.Cache.NegativeTTLSeconds)*time.Second,
	)
	authOpts := []protocol.AuthenticatorOption{
		protocol.WithAuthenticatorCache(authCache),
	}
	if cfg.Auth.MasterUsers.Enabled {
		var masterdbs []protocol.Passdb
		for _, entry := range cfg.Auth.MasterUsers.Masterdb {
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
				slog.Error("masterdb init failed", "driver", entry.Driver, "err", err)
				os.Exit(1)
			}
			masterdbs = append(masterdbs, db)
		}
		authOpts = append(authOpts,
			protocol.WithAuthenticatorMasterUsers(true),
			protocol.WithAuthenticatorMasterdb(masterdbs),
			protocol.WithAuthenticatorMasterUserSeparator(cfg.Auth.MasterUsers.Separator),
		)
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
	haproxyTimeout := time.Duration(cfg.General.HAProxy.Timeout) * time.Second

	authAddr := cfg.AuthService.ClientAddr()
	var authTLS *tls.Config
	if cfg.InternalTLS.Enabled {
		t, err := mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA, cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
		if err != nil {
			slog.Error("auth_service mtls config failed", "err", err)
			os.Exit(1)
		}
		authTLS = t
	}

	primary := firstActive(svcs.Submission, svcs.Submissions)
	srv := submsvr.New(submsvr.Options{
		HAProxy:          primary.HAProxy,
		HAProxyTimeout:   haproxyTimeout,
		HAProxyNets:      haproxyNets,
		AuthAddr:         authAddr,
		AuthTLS:          authTLS,
		DisablePlainAuth: primary.DisablePlainAuth,
		TLSConfig:        extTLS,
		Config:           cfg.Protocol.Submission,
		Auth:             chainAuth{protocol.NewAuthenticator(dbs, authOpts...)},
		Proxy:            relay,
		FailureDelay:     time.Duration(cfg.Auth.FailureDelaySeconds) * time.Second,
		OAuth2Enabled:    len(cfg.Auth.OAuth2) > 0,
	})

	go runTelemetry(telemetry.Addr(cfg.Telemetry.Listen))

	// Publish this protocol container's readiness into the co-located pod's
	// shared directory (#788); the yarilo-backend-reg sidecar gates the pod's
	// director heartbeat on it. Ready = listeners bound (a relay proxy has no
	// wedge-prone data path). No-op when readiness_dir is unset.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ready atomic.Bool
	reg := cfg.BackendRegister
	go readyfile.Touch(ctx, reg.ReadinessDir, "submission",
		time.Duration(reg.ReadinessTouchInterval)*time.Second, ready.Load)

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

	ready.Store(true) // listeners bound → the readiness toucher may start signalling

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
	cancel() // stop touching the readiness file so the sidecar drops this pod
	slog.Info("yarilo-submission stopped")
}

// chainAuth adapts protocol.Authenticator to submission.Authenticator.
// go-smtp's auth surface speaks (username, password) → error rather
// than the richer AuthResponse / Fields shape; this wrapper discards
// everything except the "did the chain accept these credentials"
// decision.
type chainAuth struct{ c protocol.Authenticator }

func (a chainAuth) AuthPlain(username, password string) error {
	resp, err := a.c.Authenticate(username, password, "smtp", "")
	if err != nil {
		return fmt.Errorf("smtp/auth: %w", err)
	}
	if resp == nil || resp.Result != protocol.AuthOK {
		return fmt.Errorf("smtp/auth: authentication failed")
	}
	return nil
}

// AuthPlainMaster forwards SASL PLAIN responses carrying a
// non-empty authzid through the master-user flow. When the
// wrapped chain does not implement protocol.MasterAuthenticator
// (master-users disabled in config) the call fails opaquely so
// the wire reply matches a wrong-password rejection.
func (a chainAuth) AuthPlainMaster(authzid, authid, password string) error {
	master, ok := a.c.(protocol.MasterAuthenticator)
	if !ok {
		return fmt.Errorf("smtp/auth: authentication failed")
	}
	resp, err := master.AuthenticateMaster(authzid, authid, password, "smtp", "")
	if err != nil {
		return fmt.Errorf("smtp/auth: %w", err)
	}
	if resp == nil || resp.Result != protocol.AuthOK {
		return fmt.Errorf("smtp/auth: authentication failed")
	}
	return nil
}

// LookupSCRAMSha256 forwards the lookup to the underlying chain.
// Returning (nil, nil) when the chain has no SCRAM support keeps
// EHLO advertisement gated correctly.
func (a chainAuth) LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error) {
	lookup, ok := a.c.(protocol.SCRAMSha256Lookup)
	if !ok {
		return nil, nil
	}
	return lookup.LookupSCRAMSha256(username)
}

// LookupSCRAMSha1 is the SHA-1 counterpart of LookupSCRAMSha256.
func (a chainAuth) LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error) {
	lookup, ok := a.c.(protocol.SCRAMSha1Lookup)
	if !ok {
		return nil, nil
	}
	return lookup.LookupSCRAMSha1(username)
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
	// One shared implementation for /healthz, /readyz, /metrics and
	// /debug/loglevel. No Checks yet: this component's /readyz was an
	// unconditional 200 before unification, and turning that into a real
	// condition is a behaviour change, not a refactor — see the readiness issue
	// for the per-component conditions.
	tel := telemetry.NewWithOptions(telemetry.Options{Addr: addr})
	if err := tel.ListenAndServe(context.Background()); err != nil {
		slog.Error("telemetry server failed", "err", err)
	}
}
