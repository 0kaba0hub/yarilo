// yarilo-jmap serves the JMAP endpoint (RFC 8620 core, RFC 8621 mail). It
// terminates TLS itself rather than sitting behind a login proxy, since JMAP is
// HTTP and carries its credentials per request. Session resource only so far.
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

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/jmap"
	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

func main() {
	logging.Setup("jmap")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	svc := cfg.Services.JMAP
	if !svc.Active() {
		slog.Error("services.jmap is not enabled")
		os.Exit(1)
	}
	addr := fmt.Sprintf(":%d", svc.Port)

	tlsCfg, err := listenerTLS(cfg, svc)
	if err != nil {
		slog.Error("TLS config failed", "err", err)
		os.Exit(1)
	}
	auth, err := dialAuth(cfg)
	if err != nil {
		slog.Error("auth client failed", "err", err, "addr", authAddr(cfg))
		os.Exit(1)
	}
	defer auth.Close() //nolint:errcheck

	slog.Info("yarilo-jmap starting",
		"version", build.Version,
		"listen", addr,
		"tls", tlsCfg != nil,
		"base_url", cfg.Protocol.JMAP.BaseURL,
		"telemetry", cfg.Telemetry.Listen,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	tel := telemetry.NewWithOptions(telemetry.Options{
		Addr:      telemetry.Addr(cfg.Telemetry.Listen),
		Lifecycle: true,
	})
	go func() {
		if err := tel.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry server failed", "err", err)
		}
	}()

	srv := jmap.New(jmap.Options{
		Addr:               addr,
		Protocol:           cfg.Protocol.JMAP,
		TLSConfig:          tlsCfg,
		Auth:               authAdapter{auth},
		DisablePlainAuth:   svc.DisablePlainAuth,
		ConnectionLimit:    svc.ConnectionLimit,
		ProxyProtocol:      svc.HAProxy,
		HAProxyTrustedNets: parseCIDRs(cfg.General.HAProxy.TrustedNets),
		HAProxyTimeout:     time.Duration(cfg.General.HAProxy.Timeout) * time.Second,
	})
	tel.SetReady(true)
	if err := srv.Serve(ctx); err != nil && !isCancelled(ctx, err) {
		slog.Error("jmap server failed", "err", err)
		os.Exit(1)
	}
	slog.Info("yarilo-jmap stopped")
}

// listenerTLS builds the client-facing TLS config. The per-service ssl block
// wins over general.ssl, matching every other listener.
func listenerTLS(cfg *config.Config, svc *config.ServiceConfig) (*tls.Config, error) {
	ssl := cfg.General.SSL
	if svc.SSL != nil {
		ssl = *svc.SSL
	}
	if ssl.TLSCert == "" || ssl.TLSKey == "" {
		return nil, nil
	}
	t, err := config.BuildTLSConfig(ssl)
	if err != nil {
		return nil, err
	}
	t.NextProtos = []string{"h2", "http/1.1"}
	return t, nil
}

func authAddr(cfg *config.Config) string {
	if cfg.AuthService.Addr != "" {
		return cfg.AuthService.Addr
	}
	return cfg.AuthService.Listen
}

func dialAuth(cfg *config.Config) (*authclient.Client, error) {
	var tlsCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		var err error
		tlsCfg, err = mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA,
			cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
		if err != nil {
			return nil, fmt.Errorf("auth mtls client config: %w", err)
		}
	}
	return authclient.Dial(authAddr(cfg), tlsCfg)
}

// authAdapter narrows the auth client to the username the server needs.
type authAdapter struct{ cl *authclient.Client }

func (a authAdapter) Authenticate(username, password, service, remoteIP, sessionID string) (string, error) {
	res, err := a.cl.Authenticate(username, password, service, remoteIP, sessionID)
	if err != nil {
		return "", err
	}
	if res.Nologin {
		return "", fmt.Errorf("jmap: login disabled for %s", username)
	}
	return res.Username, nil
}

// isCancelled reports whether err is just the shutdown signal arriving.
func isCancelled(ctx context.Context, err error) bool {
	return ctx.Err() != nil && err == ctx.Err()
}

// parseCIDRs turns the configured trusted-net list into matchers, skipping and
// logging a malformed entry rather than failing startup over one typo.
func parseCIDRs(ss []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("jmap: invalid trusted CIDR", "cidr", s, "err", err)
			continue
		}
		nets = append(nets, n)
	}
	return nets
}
