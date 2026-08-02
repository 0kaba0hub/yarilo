// yarilo-quota-status is a Postfix policy service that rejects incoming
// messages to recipients whose mailbox is over quota.
//
// Postfix configuration example:
//
//	smtpd_recipient_restrictions =
//	    reject_unauth_destination
//	    check_policy_service inet:[127.0.0.1]:12340
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

	_ "github.com/yarilomail/yarilo/pkg/dict/drivers/all"

	backendpkg "github.com/yarilomail/yarilo/internal/backend"
	"github.com/yarilomail/yarilo/internal/quotastatus"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/mtls"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	logging.Setup("quota-status")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	// Telemetry (/healthz, /readyz, /metrics) — same as every other component,
	// so orchestrators health-check quota-status the standard HTTP way.
	tel := startTelemetry(cfg.Telemetry.Listen)

	// Storage access: quota is enforced by summing the recipient's index
	// aggregate (the count backend), exactly as a delivery agent would — no
	// dict. Reads need no locker.
	resolver := backendpkg.BuildResolver(cfg)
	mbox := backendpkg.BuildMailbox(cfg.Storage, nil)
	idx := file.New()

	qs := cfg.QuotaStatus
	limits := quota.ParseRules(qs.DefaultQuotaRules)

	var aliasDict dict.Dict
	if qs.AliasDict != "" {
		aliasDict, err = openDict(cfg.Dicts, qs.AliasDict)
		if err != nil {
			slog.Error("alias dict init failed", "name", qs.AliasDict, "err", err)
			os.Exit(1)
		}
		if aliasDict == nil {
			slog.Warn("alias_dict configured but not found in dicts", "name", qs.AliasDict)
		}
	}

	var authcl *authclient.Client
	if qs.AuthMasterAddr != "" {
		var authTLS *tls.Config
		if cfg.InternalTLS.Enabled {
			authTLS, err = mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA, cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
			if err != nil {
				slog.Error("auth mtls config failed", "err", err)
				os.Exit(1)
			}
		}
		authcl, err = authclient.Dial(qs.AuthMasterAddr, authTLS)
		if err != nil {
			slog.Error("authclient dial failed", "addr", qs.AuthMasterAddr, "err", err)
			os.Exit(1)
		}
		defer func() { _ = authcl.Close() }()
		slog.Info("authclient connected", "addr", qs.AuthMasterAddr)
	}

	var lookup func(ctx context.Context, username string) (*mailbox.UserInfo, error)
	if authcl != nil {
		lookup = func(ctx context.Context, username string) (*mailbox.UserInfo, error) {
			ui, err := authcl.Userdb(ctx, username)
			if err != nil {
				return nil, err
			}
			return backendpkg.ResolveUserInfo(resolver, username, ui), nil
		}
	}

	srv := quotastatus.New(quotastatus.Options{
		Enabled:            cfg.Quota.Enabled,
		Limits:             limits,
		UserdbLookup:       lookup,
		Mailbox:            mbox,
		Index:              idx,
		AliasDict:          aliasDict,
		AliasMaxHops:       qs.AliasMaxHops,
		RecipientDelimiter: qs.RecipientDelimiter,
		ExceededMessage:    cfg.Quota.ExceededMessage,
		MailSize:           quota.ParseSize(cfg.Quota.MailSize),
		Policy:             cfg.Quota.QuotaPolicy(),
		Nouser:             qs.Nouser,
	})

	listen := qs.Listen
	if listen == "" {
		listen = ":12340"
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		slog.Error("listen failed", "addr", listen, "err", err)
		os.Exit(1)
	}

	slog.Info("yarilo-quota-status starting",
		"version", build.Version,
		"listen", listen,
		"alias_dict_configured", aliasDict != nil,
		"alias_max_hops", qs.AliasMaxHops,
		"default_rules", qs.DefaultQuotaRules,
		"per_user_rules_configured", authcl != nil,
	)

	// Every configured port is bound and serving now, so the pod can accept
	// clients. Reporting earlier would let Kubernetes route to a port that is not
	// listening yet.
	tel.SetReady(true)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Serve(ctx, ln); err != nil {
		slog.Error("policy server error", "err", err)
		os.Exit(1)
	}

	slog.Info("yarilo-quota-status stopped")
}

// startTelemetry serves /healthz, /readyz, /metrics and /debug/loglevel, and
// returns the server so the caller can report readiness once its listeners are
// actually bound.
//
// Lifecycle is on: without it /readyz answers 200 from the moment the process
// starts, which says nothing. With it, ready means this pod holds its ports.
func startTelemetry(addr string) *telemetry.Server {
	tel := telemetry.NewWithOptions(telemetry.Options{Addr: addr, Lifecycle: true})
	go func() {
		if err := tel.ListenAndServe(context.Background()); err != nil {
			slog.Error("telemetry server failed", "err", err)
		}
	}()
	return tel
}

func openDict(dicts map[string]config.DictConfig, name string) (dict.Dict, error) {
	cfg, ok := dicts[name]
	if !ok {
		return nil, nil
	}
	if cfg.Driver == "" {
		return nil, fmt.Errorf("dict %q: empty driver", name)
	}
	d, err := dict.Open(dict.Config{Driver: cfg.Driver, Settings: cfg.Settings})
	if err != nil {
		return nil, fmt.Errorf("dict %q: %w", name, err)
	}
	slog.Info("dict opened", "name", name, "driver", cfg.Driver)
	return d, nil
}
