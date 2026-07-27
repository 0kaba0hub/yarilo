// yarilo-director is the consistent-hash routing service for the yarilo mail server.
// It accepts mail client connections (IMAP/POP3/LMTP), extracts the username from
// the protocol preamble, routes via consistent hash to a backend pod, and proxies
// the session directly to that pod IP (bypassing kube-proxy).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/0kaba0hub/yarilo/internal/director"
	"github.com/0kaba0hub/yarilo/internal/lmtp"
	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})).With("service", "director"))

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	slog.Info("yarilo-director starting",
		"version", build.Version,
		"listen", cfg.DirectorService.Listen,
		"telemetry", cfg.Telemetry.Listen,
		"internal_tls", cfg.InternalTLS.Enabled,
	)

	// Internal mTLS for the director-protocol LISTENER (other directors connect
	// here). Server config: presents our cert, requires+verifies client certs.
	var ringTLSCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		ringTLSCfg, err = mtls.ServerConfig(
			cfg.InternalTLS.Cert,
			cfg.InternalTLS.Key,
			cfg.InternalTLS.CA,
		)
		if err != nil {
			slog.Error("internal_tls server config failed", "err", err)
			os.Exit(1)
		}
	}

	// Internal mTLS for DIALLING ring peers (JOIN + right-neighbor + seed
	// polls). This must be a CLIENT config — a server config has no RootCAs and
	// no ServerName, so reusing it for the dial verified nothing (#753). Ring
	// peers are dialled by ephemeral pod IP, so we pin ServerName to a stable
	// name present in every director's cert (director_service
	// .ring_tls_server_name, chart-defaulted to <release>-director-ring).
	var ringDialTLSCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		if ringTLSMisconfigured(cfg.InternalTLS.Enabled, cfg.DirectorService.Peers, cfg.DirectorService.RingTLSServerName) {
			// #753: log + continue (leave the dial nil) rather than exit — the
			// ring simply won't converge, which the ERROR makes obvious.
			slog.Error("director: internal_tls enabled with peers configured but director_service.ring_tls_server_name is empty — ring dial cannot verify pod-IP peers without it; the ring will not converge. Set director_service.ring_tls_server_name to a name in the director internal-tls cert (chart default: <release>-director-ring)")
		} else {
			ringDialTLSCfg, err = mtls.ClientConfig(
				cfg.InternalTLS.Cert,
				cfg.InternalTLS.Key,
				cfg.InternalTLS.CA,
				cfg.DirectorService.RingTLSServerName,
			)
			if err != nil {
				slog.Error("internal_tls ring dial config failed", "err", err)
				os.Exit(1)
			}
			if name := cfg.DirectorService.RingTLSServerName; name != "" && !certHasSAN(cfg.InternalTLS.Cert, name) {
				slog.Warn("director: ring_tls_server_name is not present in this director's internal-tls certificate SANs — peers present the same cert, so ring TLS handshakes will fail; re-issue the cert with this name (chart Certificate handles this)",
					"ring_tls_server_name", name)
			}
		}
	}

	// Internal mTLS client config for dialling backend pods (shared internal
	// cert → pin internal_tls.server_name, #816).
	var backendTLSCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		backendTLSCfg, err = mtls.ClientConfig(
			cfg.InternalTLS.Cert,
			cfg.InternalTLS.Key,
			cfg.InternalTLS.CA,
			cfg.InternalTLS.ServerName,
		)
		if err != nil {
			slog.Error("internal_tls client config failed", "err", err)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runTelemetry(cfg.Telemetry.Listen)

	// Ring identity (#750): the pod's own address for the JOIN handshake and
	// the right-neighbor dial, computed before NewWithOptions since
	// Membership is constructed inside it.
	localIP := os.Getenv("POD_IP")
	_, portStr, _ := net.SplitHostPort(cfg.DirectorService.Listen)
	localPort := 0
	if p, err := strconv.Atoi(portStr); err == nil {
		localPort = p
	}
	if cfg.DirectorService.RingSecret == "" && len(cfg.DirectorService.Peers) > 0 {
		slog.Warn("director: peers (seed) configured but ring_secret is empty — every DIRECTOR-JOIN will be rejected, this node can only run as a singleton ring")
	}

	usernameHashLowercase := cfg.DirectorService.UsernameHashLowercase
	srv := director.NewWithOptions(director.Options{
		UserExpire:            time.Duration(cfg.DirectorService.UserExpire) * time.Second,
		PingInterval:          time.Duration(cfg.DirectorService.PingInterval) * time.Second,
		PingTimeout:           time.Duration(cfg.DirectorService.PingTimeout) * time.Second,
		WriteTimeout:          time.Duration(cfg.DirectorService.WriteTimeout) * time.Second,
		UsernameHashLowercase: &usernameHashLowercase,
		AssignmentPolicy:      cfg.DirectorService.AssignmentPolicy,
		UserKickDelay:         time.Duration(cfg.DirectorService.UserKickDelay) * time.Second,
		MaxParallelKicks:      cfg.DirectorService.MaxParallelKicks,
		PeerTLS:               ringDialTLSCfg,
		LocalIP:               localIP,
		LocalPort:             localPort,
		RingSecret:            []byte(cfg.DirectorService.RingSecret),
		MinMembers:            cfg.DirectorService.MinMembers,
		JoinAllowedNets:       parseCIDRs(cfg.DirectorService.JoinAllowedNets),
		AntiEntropyInterval:   time.Duration(cfg.DirectorService.AntiEntropyInterval) * time.Second,
		SeedPollInterval:      time.Duration(cfg.DirectorService.SeedPollInterval) * time.Second,
		SeedPollIdleInterval:  time.Duration(cfg.DirectorService.SeedPollIdleInterval) * time.Second,
		TombstoneTTL:          time.Duration(cfg.DirectorService.TombstoneTTL) * time.Second,
		BackendExpire:         time.Duration(cfg.DirectorService.BackendExpire) * time.Second,
		UnreachableReporters:  cfg.DirectorService.BackendUnreachableReporters,
		UnreachableWindow:     time.Duration(cfg.DirectorService.BackendUnreachableWindow) * time.Second,
	})

	// Resolve static backends from config and register them in the ring.
	resolveBackends(ctx, cfg, srv)

	// Backend heartbeat-lease expiry (#776): remove lease-managed backends
	// that stop heartbeating (silent hang / crash), ring-wide.
	srv.StartBackendExpiry(ctx)

	// Session-driven pin freshness (#708 PR-B): keep a user's sticky pin alive
	// while they hold a live proxied session (#804 registry); idle users lapse
	// back to the ring hash after user_expire.
	srv.StartSessionRefresh(ctx)

	// Start mail protocol proxy listeners.
	if err := startProxies(ctx, srv, cfg, nil, backendTLSCfg); err != nil {
		slog.Error("proxy startup failed", "err", err)
		os.Exit(1)
	}

	// Start ring membership (#750): joins via the configured seeds, or runs
	// as a singleton (N=1) until a seed becomes reachable / is configured.
	srv.StartMembership(ctx, cfg.DirectorService.Peers)
	if len(cfg.DirectorService.Peers) > 0 {
		slog.Info("director: ring join started", "seeds", cfg.DirectorService.Peers)
	}

	// Start director-protocol server (ring management, inter-director sync).
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(ctx, cfg.DirectorService.Listen, ringTLSCfg); err != nil {
			errCh <- err
		}
		close(errCh)
	}()

	// Start HTTP admin API.
	apiToken := cfg.DirectorService.API.Token
	apiNets := parseCIDRs(cfg.DirectorService.API.AllowedNets)
	go func() {
		if err := srv.StartAPI(ctx, cfg.DirectorService.API.Listen, apiToken, apiNets); err != nil {
			slog.Error("director API error", "err", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig.String())
		// Graceful ring leave (#770): announce DIRECTOR-REMOVE + QUIT while
		// connections are still open, so peers evict us instantly with no
		// death-detection window, then give it a moment to flush before
		// tearing everything down.
		srv.GracefulLeave()
		time.Sleep(500 * time.Millisecond)
		cancel()
		grace := time.Duration(cfg.DirectorService.Shutdown.SessionGracePeriod) * time.Second
		if grace > 0 {
			time.Sleep(grace)
		}
	case err := <-errCh:
		if err != nil {
			slog.Error("director server error", "err", err)
			os.Exit(1)
		}
	}

	slog.Info("yarilo-director stopped")
}

// resolveBackends resolves each headless-service hostname in MailServers to pod IPs
// and registers them in the ring. For headless k8s services, DNS returns one A-record
// per pod so the director connects directly to each pod, bypassing kube-proxy.
func resolveBackends(ctx context.Context, cfg *config.Config, srv *director.Server) {
	for _, ms := range cfg.DirectorService.MailServers {
		addrs, err := net.DefaultResolver.LookupHost(ctx, ms.Host)
		if err != nil {
			slog.Error("director: resolve backend", "host", ms.Host, "err", err)
			continue
		}
		for _, addr := range addrs {
			srv.AddBackend(addr, ms.Port, ms.Tag, ms.Vhosts)
		}
		slog.Info("director: backends resolved", "host", ms.Host, "pods", len(addrs), "tag", ms.Tag)
	}
}

// startProxies starts the LMTP proxy listener on the director.
// IMAP, POP3, and Submission are handled by dedicated login-pod binaries;
// LMTP is proxied here with per-recipient fan-out via lmtp.Server.
func startProxies(ctx context.Context, srv *director.Server, cfg *config.Config, _, _ *tls.Config) error {
	// Gated on director_service.lmtp_listen, NOT the shared services.lmtp
	// block — that one belongs to the lmtp/lmtp-login pods, and reusing it
	// here forced deployments to rewrite services.lmtp whenever the
	// director was enabled, silently breaking lmtp-login (#748 item 1).
	addr := cfg.DirectorService.LMTPListen
	if addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("lmtp proxy: listen %s: %w", addr, err)
	}

	backendPort := cfg.DirectorService.LMTPBackendPort
	if backendPort == 0 {
		if _, p, perr := net.SplitHostPort(addr); perr == nil {
			if n, cerr := strconv.Atoi(p); cerr == nil {
				backendPort = n
			}
		}
	}
	lmtpSrv := lmtp.New(lmtp.Options{
		Hostname:    cfg.Protocol.Submission.Hostname,
		Config:      cfg.Protocol.LMTP,
		Router:      srv,
		BackendPort: backendPort,
	})

	slog.Info("director: lmtp proxy listening", "addr", addr)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		if err := lmtpSrv.Serve(ln); err != nil {
			slog.Error("director: lmtp proxy error", "err", err)
		}
	}()
	return nil
}

// parseCIDRs parses a list of CIDR strings into *net.IPNet values.
// Invalid entries are logged and skipped.
func parseCIDRs(ss []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("director: invalid CIDR in trusted_nets", "cidr", s, "err", err)
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

// ringTLSMisconfigured reports the loud-failure condition (#753): internal_tls
// is on and ring peers are configured, but ring_tls_server_name is empty — the
// ring dial would then verify peer certs against ephemeral pod IPs and fail, so
// the ring silently never converges. The caller logs an ERROR with a fix hint.
func ringTLSMisconfigured(tlsEnabled bool, peers []string, serverName string) bool {
	return tlsEnabled && len(peers) > 0 && serverName == ""
}

// certHasSAN reports whether the PEM cert file's leaf certificate lists name in
// its DNS SANs (#753). Best-effort: an unreadable/unparseable cert returns true
// so we do not emit a spurious warning on top of the real load error elsewhere.
func certHasSAN(certFile, name string) bool {
	pemBytes, err := os.ReadFile(certFile)
	if err != nil {
		return true
	}
	for {
		var block *pem.Block
		block, pemBytes = pem.Decode(pemBytes)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		crt, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		for _, dns := range crt.DNSNames {
			if dns == name {
				return true
			}
		}
		// Only the leaf (first cert) matters for what peers verify.
		return false
	}
	return true
}
