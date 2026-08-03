// yarilo-jmap-login is the JMAP login proxy. It accepts clients on :8443,
// authenticates each request against yarilo-auth, accounts the connection in
// yarilo-warden, and proxies to the pod yarilo-director names for that user.
// TLS is terminated here; yarilo-jmap backends never see the client's.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/jmaplogin"
	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/internal/warden"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

func main() {
	logging.Setup("jmap-login")

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
	if err := config.ValidateBackendOrDirector("jmap_login_service",
		cfg.JMAPLoginService.BackendAddr, cfg.JMAPLoginService.DirectorAddr); err != nil {
		slog.Error("config validation failed", "err", err)
		os.Exit(1)
	}

	extTLS, err := clientTLS(cfg, svc)
	if err != nil {
		slog.Error("TLS config failed", "err", err)
		os.Exit(1)
	}
	intTLS, err := internalTLS(cfg)
	if err != nil {
		slog.Error("internal TLS config failed", "err", err)
		os.Exit(1)
	}

	auth, err := authclient.Dial(authAddr(cfg), intTLS)
	if err != nil {
		slog.Error("auth client failed", "err", err, "addr", authAddr(cfg))
		os.Exit(1)
	}
	defer auth.Close() //nolint:errcheck

	var wardenPool *warden.Pool
	if addr := cfg.WardenService.Addr; addr != "" {
		wardenPool = warden.NewPool(addr, intTLS, wardenConns(cfg), 10*time.Second)
		defer wardenPool.Close()
	}

	addr := fmt.Sprintf(":%d", svc.Port)
	slog.Info("yarilo-jmap-login starting",
		"version", build.Version,
		"listen", addr,
		"tls", extTLS != nil,
		"backend_addr", cfg.JMAPLoginService.BackendAddr,
		"director_addr", cfg.JMAPLoginService.DirectorAddr,
		"warden", cfg.WardenService.Addr,
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

	srv := jmaplogin.New(jmaplogin.Options{
		Addr:               addr,
		ExtTLS:             extTLS,
		Auth:               authAdapter{auth},
		DisablePlainAuth:   svc.DisablePlainAuth,
		Router:             router(cfg),
		BackendTLS:         intTLS,
		Warden:             wardenPool,
		WardenFailOpen:     cfg.WardenService.FailOpen,
		ProxyProtocol:      svc.HAProxy,
		HAProxyTrustedNets: parseCIDRs(cfg.General.HAProxy.TrustedNets),
		HAProxyTimeout:     time.Duration(cfg.General.HAProxy.Timeout) * time.Second,
		LocalIP:            os.Getenv("POD_IP"),
	})
	tel.SetReady(true)
	if err := srv.Serve(ctx); err != nil && ctx.Err() == nil {
		slog.Error("jmap-login server failed", "err", err)
		os.Exit(1)
	}
	slog.Info("yarilo-jmap-login stopped")
}

// router picks the fixed backend when one is configured, else the director.
// Same artefact serves both shapes; only the wiring differs.
func router(cfg *config.Config) jmaplogin.Router {
	if cfg.JMAPLoginService.BackendAddr != "" {
		return jmaplogin.StaticRouter{Addr: cfg.JMAPLoginService.BackendAddr}
	}
	var dirTLS *tls.Config
	if cfg.InternalTLS.Enabled {
		dirTLS, _ = mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA,
			cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
	}
	return jmaplogin.DirectorRouter{
		Addr:    cfg.JMAPLoginService.DirectorAddr,
		TLS:     dirTLS,
		Tag:     cfg.JMAPLoginService.DirectorTag,
		LocalIP: os.Getenv("POD_IP"),
		Port:    backendPort(cfg),
	}
}

// backendPort is the JMAP container's port, which differs from whatever the
// ring reports for the pod.
func backendPort(cfg *config.Config) int {
	if p := cfg.JMAPLoginService.BackendPort; p != 0 {
		return p
	}
	if be := cfg.Services.JMAPBE; be != nil && be.Port != 0 {
		return be.Port
	}
	return 0
}

// clientTLS builds the client-facing config. A per-service ssl block wins over
// general.ssl, matching every other listener.
//
// A listener declared ssl with no certificate is refused rather than served in
// the clear: it would look healthy while carrying credentials in plaintext.
func clientTLS(cfg *config.Config, svc *config.ServiceConfig) (*tls.Config, error) {
	ssl := cfg.General.SSL
	if svc.SSL != nil {
		ssl = *svc.SSL
	}
	if ssl.TLSCert == "" || ssl.TLSKey == "" {
		if strings.EqualFold(svc.SSLMode, "ssl") {
			return nil, fmt.Errorf("services.jmap.ssl_mode is ssl but no certificate is configured: " +
				"set general.ssl.tls_cert/tls_key (helm: components.jmapLogin.tls.secretName), " +
				"or set ssl_mode to \"no\" if TLS is terminated upstream")
		}
		return nil, nil
	}
	t, err := config.BuildTLSConfig(ssl)
	if err != nil {
		return nil, err
	}
	t.NextProtos = []string{"h2", "http/1.1"}
	return t, nil
}

func internalTLS(cfg *config.Config) (*tls.Config, error) {
	if !cfg.InternalTLS.Enabled {
		return nil, nil
	}
	return mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA,
		cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
}

func authAddr(cfg *config.Config) string {
	if cfg.AuthService.Addr != "" {
		return cfg.AuthService.Addr
	}
	return cfg.AuthService.Listen
}

func wardenConns(cfg *config.Config) int {
	if n := cfg.WardenService.Conns; n > 0 {
		return n
	}
	return 4
}

// authAdapter narrows the auth client to the username the proxy needs.
type authAdapter struct{ cl *authclient.Client }

func (a authAdapter) Authenticate(username, password, service, remoteIP, sessionID string) (string, error) {
	res, err := a.cl.Authenticate(username, password, service, remoteIP, sessionID)
	if err != nil {
		return "", err
	}
	if res.Nologin {
		return "", fmt.Errorf("jmap-login: login disabled for %s", username)
	}
	return res.Username, nil
}

// parseCIDRs turns the trusted-net list into matchers, skipping and logging a
// malformed entry rather than failing startup over one typo.
func parseCIDRs(ss []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("jmap-login: invalid trusted CIDR", "cidr", s, "err", err)
			continue
		}
		nets = append(nets, n)
	}
	return nets
}
