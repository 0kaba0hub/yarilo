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
	"strings"
	"syscall"

	_ "github.com/0kaba0hub/yarilo/pkg/dict/drivers/all"

	"github.com/0kaba0hub/yarilo/internal/quotastatus"
	"github.com/0kaba0hub/yarilo/pkg/authclient"
	"github.com/0kaba0hub/yarilo/pkg/build"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
	"github.com/0kaba0hub/yarilo/pkg/quota"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})).With("service", "quota-status"))

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	quotaDict, err := openDict(cfg.Dicts, "quota")
	if err != nil {
		slog.Error("quota dict init failed", "err", err)
		os.Exit(1)
	}
	if quotaDict == nil {
		slog.Warn("dicts.quota not configured — all recipients will be allowed")
	}

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
			authTLS, err = mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
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

	srv := quotastatus.New(quotastatus.Options{
		QuotaDict:    quotaDict,
		Limits:       limits,
		AuthClient:   authcl,
		AliasDict:    aliasDict,
		AliasMaxHops: qs.AliasMaxHops,
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
		"quota_dict_configured", quotaDict != nil,
		"alias_dict_configured", aliasDict != nil,
		"alias_max_hops", qs.AliasMaxHops,
		"default_rules", qs.DefaultQuotaRules,
		"per_user_rules_configured", authcl != nil,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Serve(ctx, ln); err != nil {
		slog.Error("policy server error", "err", err)
		os.Exit(1)
	}

	slog.Info("yarilo-quota-status stopped")
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
