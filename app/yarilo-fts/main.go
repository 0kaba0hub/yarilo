// yarilo-fts is the full-text-search service: the sole owner of the FTS
// indexes — indexing queue + worker and the LOOKUP endpoint — speaking the
// pkg/ftsproto wire protocol. Engine selection is explicit via fts.fts_engine;
// the flatcurve engine is present only in binaries built with -tags flatcurve
// (the fts image). See docs/FTS.md.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/0kaba0hub/yarilo/internal/backend"
	"github.com/0kaba0hub/yarilo/internal/fts/buildmail"
	"github.com/0kaba0hub/yarilo/internal/fts/language"
	"github.com/0kaba0hub/yarilo/internal/ftsservice"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/pkg/authclient"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/ftsproto"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})).With("service", "fts"))

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}
	fc := cfg.FTS
	if !fc.Enabled {
		slog.Error("fts.enabled is false — nothing to serve")
		os.Exit(1)
	}

	engine, err := ftsservice.BuildEngine(fc)
	if err != nil {
		slog.Error("engine init failed", "err", err)
		os.Exit(1)
	}

	locker := buildLocker(cfg)
	resolver := backend.BuildResolver(cfg)
	chain, err := language.NewChain(language.Settings{
		Language: firstOr(fc.Languages, "en"),
		Filters:  fc.LanguageFilters,
	})
	if err != nil {
		slog.Error("language chain init failed", "err", err)
		os.Exit(1)
	}

	svc, err := ftsservice.New(ftsservice.Options{
		Engine:  engine,
		Mailbox: backend.BuildMailbox(cfg.Storage, locker),
		MailboxByDriver: func(driver string) mailbox.MailboxBackend {
			return backend.BuildMailboxByDriver(driver, cfg.Storage, locker)
		},
		Index:       file.New(),
		ResolveUser: userResolver(fc.AuthMasterAddr, resolver),
		Chain:       chain,
		Build: buildmail.Options{
			HeaderIncludes: fc.HeaderIncludes,
			HeaderExcludes: fc.HeaderExcludes,
			MaxSize:        fc.MessageMaxSize,
		},
		CommitLimit: fc.CommitLimit,
		LockMailbox: lockMailbox(locker),
	})
	if err != nil {
		slog.Error("service init failed", "err", err)
		os.Exit(1)
	}

	listen := fc.Listen
	if listen == "" {
		listen = ":9106"
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		slog.Error("listen failed", "addr", listen, "err", err)
		os.Exit(1)
	}
	slog.Info("yarilo-fts listening", "addr", listen, "engine", engine.Name(), "mode", fc.Mode)

	go func() {
		if err := ftsproto.Serve(ln, svc); err != nil {
			slog.Error("serve failed", "err", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	slog.Info("shutting down")
	ln.Close() //nolint:errcheck
	if err := svc.Close(); err != nil {
		slog.Error("close failed", "err", err)
	}
}

// userResolver prefers the yarilo-auth master userdb (per-user storage
// identity: home, mail location, INDEX= overrides); without an address it
// falls back to the resolver's template defaults.
func userResolver(masterAddr string, resolver *mailbox.Resolver) func(string) (*mailbox.UserInfo, error) {
	if masterAddr == "" {
		return func(u string) (*mailbox.UserInfo, error) {
			return resolver.UserInfo(u, ""), nil
		}
	}
	return func(u string) (*mailbox.UserInfo, error) {
		cl, err := authclient.Dial(masterAddr, nil)
		if err != nil {
			return nil, fmt.Errorf("fts: master dial: %w", err)
		}
		defer cl.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ui, err := cl.Userdb(ctx, u)
		if err != nil {
			return nil, fmt.Errorf("fts: userdb %s: %w", u, err)
		}
		if ui == nil {
			return nil, fmt.Errorf("fts: userdb: user not found: %s", u)
		}
		return backend.ResolveUserInfo(resolver, u, ui), nil
	}
}

// lockMailbox wraps every index write in the cross-process mailbox lock
// (project rule). nil locker (locks disabled in config) runs direct.
func lockMailbox(locker locks.Locker) func(user, folder string, fn func() error) error {
	if locker == nil {
		return nil
	}
	owner := fmt.Sprintf("yarilo-fts/%d", os.Getpid())
	return func(user, folder string, fn func() error) error {
		key := locks.MailboxKey(user, folder)
		if folder == "" {
			key = locks.MailboxKey(user, "*fts-optimize*")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		lk, err := locker.Lock(ctx, key, owner, 5*time.Minute)
		if err != nil {
			return fmt.Errorf("fts: lock %s: %w", key, err)
		}
		defer locker.Unlock(context.Background(), lk.ID) //nolint:errcheck
		return fn()
	}
}

func buildLocker(cfg *config.Config) locks.Locker {
	lc := cfg.LocksClient
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var (
		l   locks.Locker
		err error
	)
	switch lc.Mode {
	case "embedded":
		l, err = locks.NewClient(ctx, locks.DialUnix(lc.Socket))
	case "remote":
		if len(lc.Endpoints) == 0 {
			slog.Error("locks_client.endpoints required for remote mode")
			os.Exit(1)
		}
		if cfg.InternalTLS.Enabled {
			tlsCfg, terr := mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
			if terr != nil {
				slog.Error("locks mtls failed", "err", terr)
				os.Exit(1)
			}
			l, err = locks.NewClient(ctx, locks.DialTLS(lc.Endpoints[0], tlsCfg))
		} else {
			l, err = locks.NewClient(ctx, locks.DialTCP(lc.Endpoints[0]))
		}
	default:
		slog.Warn("locks not configured — fts index writes run unguarded")
		return nil
	}
	if err != nil {
		slog.Error("locks client failed", "err", err)
		os.Exit(1)
	}
	return l
}

func firstOr(xs []string, def string) string {
	if len(xs) > 0 {
		return xs[0]
	}
	return def
}

func logLevel() slog.Level {
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
