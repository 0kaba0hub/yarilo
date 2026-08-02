package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

// guidStats counts what a pre-migration pass touched.
type guidStats struct {
	Users    int
	Folders  int
	Pending  int // folders that still needed the pass
	Migrated int // folders stamped by this run
}

// runGUIDBackfill stamps per-message GUIDs across an existing store, moving the
// cost off a user's first SELECT. It writes to shared storage: without a config
// to build the yarilo-locks client it is only safe against a stopped store.
func runGUIDBackfill(driver, root, onlyUser, cfgPath string, dryRun bool) error {
	locker, err := guidLocker(cfgPath)
	if err != nil {
		return err
	}
	if locker == nil && !dryRun {
		slog.Warn("no --config: running without yarilo-locks, safe only against a stopped store")
	}
	boxBE, idxBE, err := guidBackends(driver, locker)
	if err != nil {
		return err
	}
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}

	users, err := guidUsers(root, onlyUser)
	if err != nil {
		return err
	}
	var st guidStats
	for _, user := range users {
		if err := backfillUser(boxBE, idxBE, resolver, user, dryRun, &st); err != nil {
			return fmt.Errorf("guid backfill %s: %w", user, err)
		}
		st.Users++
	}
	slog.Info("guid backfill complete", "users", st.Users, "folders", st.Folders,
		"pending", st.Pending, "migrated", st.Migrated, "dry_run", dryRun)
	return nil
}

func backfillUser(boxBE mailbox.MailboxBackend, idxBE mailbox.IndexBackend, resolver *mailbox.Resolver, user string, dryRun bool, st *guidStats) error {
	info := resolver.UserInfo(user, "")
	box := boxBE.OpenUser(info)
	defer box.Close() //nolint:errcheck
	idx := idxBE.OpenUser(info)
	defer idx.Close() //nolint:errcheck

	entries, err := box.ListFolders()
	if err != nil {
		return fmt.Errorf("list folders: %w", err)
	}
	for _, e := range entries {
		if !e.Selectable {
			continue
		}
		st.Folders++
		folder, err := idx.OpenFolder(e.Name, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", e.Name, err)
		}
		need, err := idx.GUIDBackfillNeeded(folder.ID)
		if err != nil {
			return fmt.Errorf("check %s: %w", e.Name, err)
		}
		if !need {
			continue
		}
		st.Pending++
		if dryRun {
			slog.Info("would backfill", "user", user, "folder", e.Name)
			continue
		}
		if err := idxrebuild.BackfillGUIDs(box, idx, folder, e.Name); err != nil {
			return fmt.Errorf("backfill %s: %w", e.Name, err)
		}
		st.Migrated++
		slog.Info("backfilled", "user", user, "folder", e.Name)
	}
	return nil
}

// guidUsers returns the <domain>/<user> pairs under root as user@domain, or the
// single --user value when given.
func guidUsers(root, onlyUser string) ([]string, error) {
	if onlyUser != "" {
		return []string{onlyUser}, nil
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 2 || !d.IsDir() {
			return nil
		}
		out = append(out, parts[1]+"@"+parts[0])
		return nil
	})
	return out, err
}

func guidBackends(driver string, locker locks.Locker) (mailbox.MailboxBackend, mailbox.IndexBackend, error) {
	var box mailbox.MailboxBackend
	switch strings.ToLower(driver) {
	case "maildir":
		box = maildir.New(maildir.WithLocker(locker))
	case "sdbox", "dbox":
		box = dboxv2.New(dboxv2.WithLocker(locker))
	case "mdbox":
		box = mdbox.New(mdbox.WithLocker(locker))
	default:
		return nil, nil, fmt.Errorf("unknown --driver %q (want maildir|sdbox|mdbox)", driver)
	}
	return box, indexfile.New(indexfile.WithLocker(locker)), nil
}

// guidLocker builds the yarilo-locks client from the service config. An empty
// path means no locker, for offline runs against a stopped store.
func guidLocker(cfgPath string) (locks.Locker, error) {
	if cfgPath == "" {
		return nil, nil
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("config load %s: %w", cfgPath, err)
	}
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
		if cfg.InternalTLS.Enabled {
			tlsCfg, tlsErr := mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA,
				cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
			if tlsErr != nil {
				return nil, fmt.Errorf("locks_client mtls: %w", tlsErr)
			}
			return locks.NewClient(ctx, locks.DialTLS(lc.Endpoints[0], tlsCfg))
		}
		return locks.NewClient(ctx, locks.DialTCP(lc.Endpoints[0]))
	default:
		return nil, fmt.Errorf("locks_client: unknown mode %q", lc.Mode)
	}
}
