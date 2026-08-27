// yarilo-director routes mail sessions to backend pods by consistent hash of
// the username and proxies them directly to the pod IP (bypassing kube-proxy).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
	"github.com/yarilomail/yarilo/internal/director"
	"github.com/yarilomail/yarilo/internal/lmtp"
	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	logging.Setup("director")

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

	// mTLS server config for the director-protocol listener:
	// presents our cert, requires+verifies client certs
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

	// mTLS client config for dialling ring peers. Must be a client config, not
	// the server one (no RootCAs/ServerName there). Peers are dialled by
	// ephemeral pod IP, so pin ServerName to ring_tls_server_name, a stable
	// name in every director's cert.
	var ringDialTLSCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		if ringTLSMisconfigured(cfg.InternalTLS.Enabled, cfg.DirectorService.Peers, cfg.DirectorService.RingTLSServerName) {
			// log + continue (dial stays nil): the ring won't converge,
			// which the ERROR makes obvious
			slog.Error("director: internal_tls enabled with peers configured but director_service.ring_tls_server_name is empty — ring dial cannot verify pod-IP peers without it; the ring will not converge. Set director_service.ring_tls_server_name to a name in the director internal-tls cert (chart default: <release>-director-ring)")
		} else {
			ringDialTLSCfg, err = mtls.ClientConfig(
				cfg.InternalTLS.Cert,
				cfg.InternalTLS.Key,
				cfg.InternalTLS.CA,
				cfg.DirectorService.RingTLSServerName,
				cfg.InternalTLS.SessionCacheSize,
				cfg.InternalTLS.SessionCacheTTL,
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

	// mTLS client config for dialling backend pods; shared internal cert,
	// pin internal_tls.server_name
	var backendTLSCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		backendTLSCfg, err = mtls.ClientConfig(
			cfg.InternalTLS.Cert,
			cfg.InternalTLS.Key,
			cfg.InternalTLS.CA,
			cfg.InternalTLS.ServerName,
			cfg.InternalTLS.SessionCacheSize,
			cfg.InternalTLS.SessionCacheTTL,
		)
		if err != nil {
			slog.Error("internal_tls client config failed", "err", err)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Ring identity: our own address for the JOIN handshake and right-neighbor
	// dial, needed before NewWithOptions constructs Membership.
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
	// A malformed username_hash template must abort at startup, never
	// silently mis-route. When set it governs case-folding, so warn that
	// the legacy bool is ignored.
	if raw := strings.TrimSpace(cfg.DirectorService.UsernameHash); raw != "" {
		if _, err := ring.ParseHashFormat(raw); err != nil {
			slog.Error("director: invalid director_service.username_hash", "err", err, "value", raw)
			os.Exit(1)
		}
		slog.Warn("director: username_hash set — it governs case-folding; username_hash_lowercase is ignored",
			"username_hash", raw)
	}
	srv := director.NewWithOptions(director.Options{
		UserExpire:            time.Duration(cfg.DirectorService.UserExpire) * time.Second,
		PingInterval:          time.Duration(cfg.DirectorService.PingInterval) * time.Second,
		PingTimeout:           time.Duration(cfg.DirectorService.PingTimeout) * time.Second,
		WriteTimeout:          time.Duration(cfg.DirectorService.WriteTimeout) * time.Second,
		UsernameHashLowercase: &usernameHashLowercase,
		UsernameHashFormat:    cfg.DirectorService.UsernameHash,
		AssignmentPolicy:      cfg.DirectorService.AssignmentPolicy,
		UserKickDelay:         time.Duration(cfg.DirectorService.UserKickDelay) * time.Second,
		MaxParallelKicks:      cfg.DirectorService.MaxParallelKicks,
		MaxParallelMoves:      cfg.DirectorService.MaxParallelMoves,
		FlushProgram:          cfg.DirectorService.FlushProgram,
		FlushProgramTimeout:   time.Duration(cfg.DirectorService.FlushProgramTimeoutSeconds) * time.Second,
		UserKillTimeout:       time.Duration(cfg.DirectorService.UserKillTimeout) * time.Second,
		UserKillConfirmGrace:  time.Duration(cfg.DirectorService.UserKillConfirmGrace) * time.Second,
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

	// telemetry starts after the server exists so the liveness watchdog
	// can probe the ring mutex
	tel := startTelemetry(cfg.Telemetry, srv)

	// register static backends from config in the ring
	resolveBackends(ctx, cfg, srv)

	// remove lease-managed backends that stop heartbeating, ring-wide
	srv.StartBackendExpiry(ctx)

	// keep a user's sticky pin alive while a proxied session is open;
	// idle users lapse back to the ring hash after user_expire
	srv.StartSessionRefresh(ctx)

	// clear a user's LOOKUP hold once ring-wide sessions confirm gone
	// or the hard timeout elapses
	srv.StartKillSweep(ctx)

	// start mail protocol proxy listeners
	if err := startProxies(ctx, srv, cfg, nil, backendTLSCfg); err != nil {
		slog.Error("proxy startup failed", "err", err)
		os.Exit(1)
	}

	// join the ring via the configured seeds, or run as a singleton
	// until a seed becomes reachable
	srv.StartMembership(ctx, cfg.DirectorService.Peers)
	if len(cfg.DirectorService.Peers) > 0 {
		slog.Info("director: ring join started", "seeds", cfg.DirectorService.Peers)
	}

	// Bind the director-protocol port before readiness is reported, so the
	// pod never announces ready without the port actually up.
	ringLn, err := srv.Listen(cfg.DirectorService.Listen, ringTLSCfg)
	if err != nil {
		slog.Error("director: listen failed", "addr", cfg.DirectorService.Listen, "err", err)
		os.Exit(1)
	}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ctx, ringLn); err != nil {
			errCh <- err
		}
		close(errCh)
	}()

	// start HTTP admin API
	apiToken := cfg.DirectorService.API.Token
	apiNets := parseCIDRs(cfg.DirectorService.API.AllowedNets)
	go func() {
		if err := srv.StartAPI(ctx, cfg.DirectorService.API.Listen, apiToken, apiNets); err != nil {
			slog.Error("director API error", "err", err)
		}
	}()

	// all configured ports are bound; report ready only now so Kubernetes
	// never routes to a port that is not listening yet
	tel.SetReady(true)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig.String())
		// leave the Service endpoints before draining
		tel.SetReady(false)
		// announce DIRECTOR-REMOVE + QUIT while connections are open so peers
		// evict us instantly, then give it a moment to flush
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

// resolveBackends resolves each headless-service hostname in MailServers to
// pod IPs (one A-record per pod) and registers them in the ring.
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

// startProxies starts the LMTP proxy listener with per-recipient fan-out.
// IMAP, POP3, and Submission are handled by dedicated login-pod binaries.
func startProxies(ctx context.Context, srv *director.Server, cfg *config.Config, _, _ *tls.Config) error {
	// gated on director_service.lmtp_listen, not the shared services.lmtp
	// block, which belongs to the lmtp/lmtp-login pods
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
		// The director's LMTP proxy: it announces this installation, not
		// submission, whose key overrides submission alone (#1506).
		Hostname:    cfg.Hostname,
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

// startTelemetry serves /healthz, /readyz, /metrics and /debug/loglevel.
// Lifecycle is on: ready is reported only once the caller's ports are bound.
func startTelemetry(cfg config.TelemetryConfig, srv *director.Server) *telemetry.Server {
	opts := telemetry.Options{
		Addr:      telemetry.Addr(cfg.Listen),
		Lifecycle: true,
		Pprof: telemetry.PprofOptions{
			Enabled:       cfg.PprofEnabled,
			BlockRate:     cfg.PprofBlockProfileRate,
			MutexFraction: cfg.PprofMutexProfileFraction,
		},
	}
	if wd := cfg.LivenessWatchdog; wd.Enabled {
		var gate *telemetry.Gate
		if wd.FaultInjectionEnabled {
			gate = telemetry.NewGate()
			opts.Fault = gate
		}
		opts.Watchdog = telemetry.WatchdogOptions{
			Check:            directorLivenessCheck(srv, gate),
			Interval:         time.Duration(wd.IntervalSeconds) * time.Second,
			Timeout:          time.Duration(wd.TimeoutSeconds) * time.Second,
			FailureThreshold: wd.FailureThreshold,
		}
	}
	tel := telemetry.NewWithOptions(opts)
	go func() {
		if err := tel.ListenAndServe(context.Background()); err != nil {
			slog.Error("telemetry server failed", "err", err)
		}
	}()
	return tel
}

// directorLivenessCheck reads the ring backend count to prove the routing path
// is not deadlocked: the read takes the same ring mutex every LOOKUP holds, so
// a wedged ring blocks it and the watchdog times out. All state is in-process;
// a restart rebuilds the ring from gossip + backend re-registration.
func directorLivenessCheck(srv *director.Server, gate *telemetry.Gate) telemetry.LivenessCheck {
	return func(ctx context.Context) error {
		if gate != nil {
			if err := gate.Check(ctx); err != nil {
				return err
			}
		}
		if srv != nil {
			srv.RingBackendCount()
		}
		return nil
	}
}

// ringTLSMisconfigured reports internal_tls on + peers configured but
// ring_tls_server_name empty: the ring dial would verify peer certs against
// ephemeral pod IPs and the ring would silently never converge.
func ringTLSMisconfigured(tlsEnabled bool, peers []string, serverName string) bool {
	return tlsEnabled && len(peers) > 0 && serverName == ""
}

// certHasSAN reports whether the leaf certificate lists name in its DNS SANs.
// Best-effort: an unreadable cert returns true to avoid a spurious warning.
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
		// only the leaf (first cert) matters
		return false
	}
	return true
}
