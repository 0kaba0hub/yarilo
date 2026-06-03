// yarilo-backend-api is the backend-plane admin HTTP API.
//
// One instance runs per backend tag (or one per standalone
// deployment). Operators reach it via the yarilo-admin CLI's
// `backend` subtree (yarilo-admin backend dict ..., backend folder ...,
// backend user ..., backend index ..., backend subscriptions ...,
// backend specialuse ..., backend metadata ...).
//
// Wire reference: docs/BACKEND-API.md
//
// Configuration: backend_api section of yarilo.yaml + storage /
// namespaces / dicts / locks_client / internal_tls sections.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/0kaba0hub/yarilo/internal/backendapi"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
	"github.com/0kaba0hub/yarilo/pkg/authclient"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	_ "github.com/0kaba0hub/yarilo/pkg/dict/drivers/all"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
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

	listen := cfg.BackendAPI.Listen
	if listen == "" {
		listen = ":9105"
	}
	slog.Info("yarilo-backend-api starting",
		"version", version,
		"listen", listen,
		"internal_tls", cfg.InternalTLS.Enabled,
		"dicts", len(cfg.Dicts),
		"namespaces", len(cfg.Namespaces),
	)

	var tlsCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		tlsCfg, err = mtls.ServerConfig(
			cfg.InternalTLS.Cert,
			cfg.InternalTLS.Key,
			cfg.InternalTLS.CA,
		)
		if err != nil {
			slog.Error("internal_tls server config failed", "err", err)
			os.Exit(1)
		}
	}

	dicts := openDicts(cfg.Dicts)
	defer func() {
		for name, d := range dicts {
			if err := d.Close(); err != nil {
				slog.Warn("backend-api: dict close failed", "name", name, "err", err)
			}
		}
	}()

	locker, err := buildLocksClient(cfg)
	if err != nil {
		slog.Error("backend-api: locks client", "err", err)
		os.Exit(1)
	}
	defer func() {
		if locker != nil {
			_ = locker.Close()
		}
	}()

	resolver := &mailbox.Resolver{
		Root:         cfg.Storage.MaildirRoot,
		HomeTemplate: cfg.Storage.MailHomeTemplate,
	}
	if resolver.Root == "" {
		resolver.Root = "/var/mail/vhosts"
	}
	if resolver.HomeTemplate == "" {
		resolver.HomeTemplate = "%d/%n"
	}

	mb := buildMailbox(cfg.Storage.Mailbox, locker)
	idx := file.New(file.WithLocker(locker))
	nsOverrides, err := buildNamespaceMailboxes(cfg.Namespaces, cfg.Storage.Mailbox, locker)
	if err != nil {
		slog.Error("backend-api: namespace mailbox wiring", "err", err)
		os.Exit(1)
	}

	var anvilTLS *tls.Config
	if cfg.InternalTLS.Enabled && cfg.AnvilService.Listen != "" {
		anvilTLS, err = mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
		if err != nil {
			slog.Error("backend-api: anvil mtls client config failed", "err", err)
			os.Exit(1)
		}
	}

	// Dial yarilo-auth's master-protocol listener when configured.
	// Empty AuthMasterAddr keeps the legacy single-binary / smoke
	// flow alive — handleUserInfo skips userdb enrichment and
	// /api/backend/user/iterate returns 503.
	var authcl *authclient.Client
	if cfg.BackendAPI.AuthMasterAddr != "" {
		var authTLS *tls.Config
		if cfg.InternalTLS.Enabled {
			authTLS, err = mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
			if err != nil {
				slog.Error("backend-api: auth mtls client config failed", "err", err)
				os.Exit(1)
			}
		}
		authcl, err = authclient.Dial(cfg.BackendAPI.AuthMasterAddr, authTLS)
		if err != nil {
			slog.Error("backend-api: authclient dial",
				"addr", cfg.BackendAPI.AuthMasterAddr, "err", err)
			os.Exit(1)
		}
		defer func() { _ = authcl.Close() }()
		slog.Info("backend-api: authclient connected", "addr", cfg.BackendAPI.AuthMasterAddr)
	}

	srv := backendapi.New(backendapi.Options{
		Addr:               listen,
		TLSConfig:          tlsCfg,
		Token:              cfg.BackendAPI.Token,
		AllowedNets:        parseCIDRs(cfg.BackendAPI.AllowedNets),
		Dicts:              dicts,
		Mailbox:            mb,
		Index:              idx,
		Resolver:           resolver,
		NamespaceMailboxes: nsOverrides,
		Namespaces:         cfg.Namespaces,
		Locker:             locker,
		SpecialUseDefaults: cfg.Protocol.IMAP.SpecialUseDefaults,
		MetadataDict:       dicts["metadata"],
		QuotaDict:          dicts["quota"],
		AnvilAddr:          cfg.AnvilService.Listen,
		AnvilTLS:           anvilTLS,
		AuthClient:         authcl,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := srv.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("backend-api: serve failed", "err", err)
		os.Exit(1)
	}
	slog.Info("yarilo-backend-api stopped")
}

func openDicts(specs map[string]config.DictConfig) map[string]dict.Dict {
	out := map[string]dict.Dict{}
	for name, dc := range specs {
		if dc.Driver == "" {
			slog.Warn("backend-api: skipping dict with empty driver", "name", name)
			continue
		}
		d, err := dict.Open(dict.Config{Driver: dc.Driver, Settings: dc.Settings})
		if err != nil {
			slog.Error("backend-api: open dict failed", "name", name, "driver", dc.Driver, "err", err)
			os.Exit(1)
		}
		out[name] = d
		slog.Info("backend-api: opened dict", "name", name, "driver", dc.Driver)
	}
	return out
}

func buildMailbox(driver string, locker locks.Locker) mailbox.MailboxBackend {
	switch strings.ToLower(driver) {
	case "sdbox", "dbox":
		return dboxv2.New(dboxv2.WithLocker(locker))
	case "mdbox":
		return mdbox.New(mdbox.WithLocker(locker))
	default:
		return maildir.New(maildir.WithLocker(locker))
	}
}

func buildNamespaceMailboxes(namespaces []config.NamespaceConfig, globalDriver string, locker locks.Locker) (map[string]mailbox.MailboxBackend, error) {
	if len(namespaces) == 0 {
		return nil, nil
	}
	globalDriver = strings.ToLower(globalDriver)
	if globalDriver == "" {
		globalDriver = "maildir"
	}
	byDriver := make(map[string]mailbox.MailboxBackend)
	overrides := map[string]mailbox.MailboxBackend{}
	for _, ns := range namespaces {
		if ns.Location == "" {
			continue
		}
		loc, ok, err := mailbox.ParseLocation(ns.Location, nil)
		if err != nil {
			return nil, fmt.Errorf("backend-api: namespace %q: %w", ns.Prefix, err)
		}
		if !ok {
			continue
		}
		drv := strings.ToLower(loc.Driver)
		if drv == globalDriver {
			continue
		}
		b, exists := byDriver[drv]
		if !exists {
			b = buildMailbox(drv, locker)
			byDriver[drv] = b
		}
		overrides[ns.Prefix] = b
	}
	if len(overrides) == 0 {
		return nil, nil
	}
	return overrides, nil
}

func buildLocksClient(cfg *config.Config) (locks.Locker, error) {
	lc := cfg.LocksClient
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch lc.Mode {
	case "":
		return nil, nil
	case "embedded":
		if lc.Socket == "" {
			return nil, fmt.Errorf("locks_client.socket required for embedded mode")
		}
		return locks.NewClient(ctx, locks.DialUnix(lc.Socket))
	case "remote":
		if len(lc.Endpoints) == 0 {
			return nil, fmt.Errorf("locks_client.endpoints must have at least one entry for remote mode")
		}
		var tlsCfg *tls.Config
		if cfg.InternalTLS.Enabled {
			t, err := mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
			if err != nil {
				return nil, fmt.Errorf("locks_client mtls: %w", err)
			}
			tlsCfg = t
		}
		return locks.NewClient(ctx, locks.DialTLS(lc.Endpoints[0], tlsCfg))
	default:
		return nil, fmt.Errorf("locks_client: unknown mode %q", lc.Mode)
	}
}

func parseCIDRs(in []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(in))
	for _, s := range in {
		_, n, err := net.ParseCIDR(strings.TrimSpace(s))
		if err != nil {
			slog.Warn("backend-api: ignoring bad CIDR", "value", s, "err", err)
			continue
		}
		out = append(out, n)
	}
	return out
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
