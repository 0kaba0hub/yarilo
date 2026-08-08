// yarilo-backend-reg is the co-located backend pod's registration sidecar
// (#788). It owns the pod's SINGLE director registration: one BACKEND-UP for
// the pod IP, heartbeated while every protocol container in the pod is ready,
// and a graceful BACKEND-DOWN (LEAVE) on SIGTERM. It never touches mail bytes,
// session routing, auth, or storage — it is only the pod's voice in the
// director ring. Readiness is read passively from the shared readiness
// directory (readyfile): a protocol container that stops touching its file
// (wedged / not ready) makes the sidecar withhold the heartbeat, and the
// director expires the whole pod. See https://doc.yarilomail.org/DEPLOYMENT "backend liveness".
package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yarilomail/yarilo/internal/backendreg"
	"github.com/yarilomail/yarilo/internal/readyfile"
	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

func main() {
	logging.Setup("backend-reg")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tel := telemetry.NewWithOptions(telemetry.Options{
		Addr:      telemetry.Addr(cfg.Telemetry.Listen),
		Lifecycle: true,
		Pprof: telemetry.PprofOptions{
			Enabled: cfg.Telemetry.PprofEnabled,
			Heap:    cfg.Telemetry.PprofHeapEnabled,
		},
	})
	go func() {
		if err := tel.ListenAndServe(ctx); err != nil {
			slog.Error("telemetry server failed", "err", err)
		}
	}()
	tel.SetReady(true)

	client := buildClient(cfg)
	if client != nil {
		go client.Run(ctx)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
	tel.SetReady(false)

	if client != nil {
		// Graceful LEAVE: remove the pod from the ring immediately instead of
		// waiting out the TTL, then give the BACKEND-DOWN a moment to flush.
		client.Leave()
		time.Sleep(300 * time.Millisecond)
	}
	cancel()
}

// buildClient constructs the registration client, or nil (registration
// disabled) when director_addr or POD_IP is unset — the sidecar then just idles
// on its telemetry endpoint (no crash loop) until the pod is torn down.
func buildClient(cfg *config.Config) *backendreg.Client {
	reg := cfg.BackendRegister
	if reg.DirectorAddr == "" {
		slog.Info("backend-reg: backend_register.director_addr unset, registration disabled")
		return nil
	}
	ip := os.Getenv("POD_IP")
	if ip == "" {
		slog.Warn("backend-reg: POD_IP unset, registration disabled")
		return nil
	}

	tlsCfg, err := buildTLS(cfg)
	if err != nil {
		slog.Error("backend-reg: mTLS client config failed, registration disabled", "err", err)
		return nil
	}

	// Gate the heartbeat on every expected protocol's readiness file being
	// fresh. An empty ReadinessProtocols set means no gate (heartbeat
	// unconditionally) — Helm sets the pod's protocol list.
	dir := reg.ReadinessDir
	protos := reg.ReadinessProtocols
	staleAfter := time.Duration(reg.ReadinessStaleAfter) * time.Second
	healthy := func() bool {
		if len(protos) == 0 {
			return true
		}
		fresh, reason := readyfile.AllFresh(dir, protos, staleAfter)
		if !fresh {
			slog.Warn("backend-reg: pod not ready, withholding heartbeat", "reason", reason)
		}
		return fresh
	}

	slog.Info("backend-reg: registering pod with director",
		"director", reg.DirectorAddr, "ip", ip, "tag", reg.Tag, "protocols", protos)
	return backendreg.New(backendreg.Options{
		DirectorAddr: reg.DirectorAddr,
		SelfIP:       ip,
		// Port is nominal: the ring entry identifies the POD, and each login
		// proxy overrides it with its own protocol port (#788). 0 is fine —
		// the ring keys backends by IP.
		Port:     0,
		Tag:      reg.Tag,
		Vhosts:   reg.Vhosts,
		Interval: time.Duration(reg.RegisterInterval) * time.Second,
		Healthy:  healthy,
		TLS:      tlsCfg,
	})
}

func buildTLS(cfg *config.Config) (*tls.Config, error) {
	if !cfg.InternalTLS.Enabled {
		return nil, nil
	}
	return mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA, cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
}
