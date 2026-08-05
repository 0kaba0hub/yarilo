package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbuild"
	"github.com/yarilomail/yarilo/internal/userdbinfo"
	"github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

// guidOpts is one pre-migration invocation. Layout, driver and locking come
// from the service config so the tool addresses the same store the services do;
// the flags are overrides for a store with no config to hand.
type guidOpts struct {
	ConfigPath string
	Driver     string // overrides storage.mailbox
	Root       string // overrides storage.maildir_root
	Template   string // overrides storage.mail_home_template
	User       string // one user@domain instead of every user under root
	IndexTmpl  string // offline stand-in for the userdb INDEX= override
	MailTmpl   string // offline stand-in for the userdb mail_path override
	Offline    bool   // resolve from flags only, never consult userdb
	DryRun     bool
}

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
func runGUIDBackfill(o guidOpts) error {
	cfg, err := guidConfig(o.ConfigPath)
	if err != nil {
		return err
	}
	locker, err := guidLocker(cfg)
	if err != nil {
		return err
	}
	if locker == nil && !o.DryRun {
		slog.Warn("no yarilo-locks client: safe only against a stopped store", "config", o.ConfigPath)
	}
	authcl, err := guidAuthClient(cfg, o)
	if err != nil {
		return err
	}
	if authcl != nil {
		defer authcl.Close() //nolint:errcheck
	}
	resolver := guidResolver(cfg, o)
	driver := o.Driver
	if driver == "" {
		driver = cfg.Storage.Mailbox
	}
	if driver == "" {
		return fmt.Errorf("no storage driver: set --driver or storage.mailbox in --config")
	}
	// ByDriver falls back to maildir on an unknown name, which would silently
	// read the wrong store here, so the name is checked first.
	switch strings.ToLower(driver) {
	case "maildir", "sdbox", "dbox", "mdbox":
	default:
		return fmt.Errorf("unknown storage driver %q (want maildir|sdbox|mdbox)", driver)
	}
	// ByDriver is the constructor that applies the storage settings the
	// services run with (alt path, UTF-8 list, NFC normalisation).
	boxBE := mailboxbuild.ByDriver(driver, cfg.Storage, locker)
	// Never fabricate an index: a fresh one reads as an empty folder, so a
	// mis-resolved path would report success having stamped nothing.
	idxBE := indexfile.New(indexfile.WithLocker(locker), indexfile.WithNoCreate())

	users, err := guidUsers(resolver.Root, resolver.HomeTemplate, o.User)
	if err != nil {
		return err
	}
	var st guidStats
	for _, user := range users {
		if err := backfillUser(boxBE, idxBE, resolver, authcl, o, user, &st); err != nil {
			return fmt.Errorf("guid backfill %s: %w", user, err)
		}
		st.Users++
	}
	slog.Info("guid backfill complete", "users", st.Users, "folders", st.Folders,
		"pending", st.Pending, "migrated", st.Migrated, "driver", driver,
		"root", resolver.Root, "home_template", resolver.HomeTemplate, "dry_run", o.DryRun)
	return nil
}

func backfillUser(boxBE mailbox.MailboxBackend, idxBE mailbox.IndexBackend, resolver *mailbox.Resolver, authcl *authclient.Client, o guidOpts, user string, st *guidStats) error {
	info, err := guidUserInfo(resolver, authcl, o, user)
	if err != nil {
		return err
	}
	box := boxBE.OpenUser(info)
	defer box.Close() //nolint:errcheck
	idx := idxBE.OpenUser(info)
	defer idx.Close() //nolint:errcheck

	entries, err := box.ListFolders()
	if err != nil {
		return fmt.Errorf("list folders: %w", err)
	}
	dryRun := o.DryRun
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

// guidConfig loads the service config. No path yields an empty config, so a
// configless run still works against a stopped store.
func guidConfig(path string) (*config.Config, error) {
	if path == "" {
		return &config.Config{}, nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("config load %s: %w", path, err)
	}
	return cfg, nil
}

// guidResolver mirrors the resolver the services build, so the tool reads the
// same homes, index and control dirs. Flags win over the config, and the
// built-in defaults are the last resort.
func guidResolver(cfg *config.Config, o guidOpts) *mailbox.Resolver {
	r := &mailbox.Resolver{
		Root:                     cfg.Storage.MaildirRoot,
		HomeTemplate:             cfg.Storage.MailHomeTemplate,
		DefaultVolatileDir:       cfg.Storage.VolatileDir,
		DefaultIndexDir:          cfg.Storage.IndexDir,
		DefaultControlDir:        cfg.Storage.ControlDir,
		DefaultAltDir:            cfg.Storage.AltDir,
		DefaultMailPath:          cfg.Storage.MailPath,
		DefaultSeparator:         guidSeparator(cfg.Namespaces),
		DefaultStorageEscapeChar: cfg.Storage.MailboxListStorageEscapeChar,
	}
	if o.Root != "" {
		r.Root = o.Root
	}
	if o.Template != "" {
		r.HomeTemplate = o.Template
	}
	if r.Root == "" {
		r.Root = "/var/mail/vhosts"
	}
	if r.HomeTemplate == "" {
		r.HomeTemplate = "%d/%n"
	}
	return r
}

func guidSeparator(nss []config.NamespaceConfig) string {
	for _, ns := range nss {
		if strings.EqualFold(strings.TrimSpace(ns.Type), "personal") && ns.Separator != "" {
			return ns.Separator
		}
	}
	return ""
}

// guidUsers enumerates the users under root for the given home template. Only a
// template whose leaf directory names the user can be walked: "%u" is the full
// address, "%n" the local part with "%d" above it; anything else needs --user.
func guidUsers(root, template, onlyUser string) ([]string, error) {
	if onlyUser != "" {
		return []string{onlyUser}, nil
	}
	parts := strings.Split(strings.Trim(filepath.ToSlash(template), "/"), "/")
	leaf := parts[len(parts)-1]
	domainAt := -1
	for i, p := range parts {
		if p == "%d" {
			domainAt = i
		}
	}
	if leaf != "%u" && (leaf != "%n" || domainAt < 0) {
		return nil, fmt.Errorf("cannot enumerate users for mail_home_template %q: pass --user", template)
	}

	depth := len(parts)
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil // the root itself is not a user directory
		}
		seg := strings.Split(filepath.ToSlash(rel), "/")
		if len(seg) != depth || !d.IsDir() {
			return nil
		}
		if leaf == "%u" {
			out = append(out, seg[depth-1])
		} else {
			out = append(out, seg[depth-1]+"@"+seg[domainAt])
		}
		return nil
	})
	return out, err
}

// guidLocker builds the yarilo-locks client the services use. An empty
// locks_client mode, or no config at all, means no locker.
func guidLocker(cfg *config.Config) (locks.Locker, error) {
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

// guidUserInfo resolves one user's storage identity. With a userdb reachable
// the per-user overrides come from there, exactly as a session resolves them;
// offline they come from the templates, which is why the two are exclusive.
func guidUserInfo(resolver *mailbox.Resolver, authcl *authclient.Client, o guidOpts, user string) (*mailbox.UserInfo, error) {
	ui := resolver.UserInfo(user, "")
	if authcl != nil {
		pui, err := authcl.Userdb(context.Background(), user)
		if err != nil {
			return nil, fmt.Errorf("userdb lookup %s: %w", user, err)
		}
		if pui == nil {
			return nil, fmt.Errorf("userdb has no user %s", user)
		}
		userdbinfo.Apply(ui, pui, user)
		return ui, nil
	}
	// Same helper the resolver and the userdb overlay use, so "~/index" here
	// means what it means everywhere else.
	if o.IndexTmpl != "" {
		ui.IndexDir = mailbox.ExpandLocation(o.IndexTmpl, ui.Home, user)
	}
	if o.MailTmpl != "" {
		ui.MailPath = mailbox.ExpandLocation(o.MailTmpl, ui.Home, user)
	}
	return ui, nil
}

// guidAuthClient dials yarilo-auth for the per-user overrides. --offline takes
// the templates instead; asking for both is an error, since a template that
// disagrees with userdb addresses a mailbox the sessions do not use.
func guidAuthClient(cfg *config.Config, o guidOpts) (*authclient.Client, error) {
	addr := cfg.BackendAPI.AuthMasterAddr
	if o.Offline {
		if addr != "" {
			slog.Info("offline: ignoring auth_master_addr, resolving from flags", "addr", addr)
		}
		return nil, nil
	}
	if o.IndexTmpl != "" || o.MailTmpl != "" {
		return nil, fmt.Errorf("--index-template/--mail-template are offline-only: add --offline, " +
			"or drop them and let userdb supply the per-user overrides")
	}
	if addr == "" {
		return nil, fmt.Errorf("no backend_api.auth_master_addr in --config: " +
			"per-user INDEX=/mail_path overrides would be missed; set it, or run --offline with the templates")
	}
	var tlsCfg *tls.Config
	if cfg.InternalTLS.Enabled {
		var err error
		tlsCfg, err = mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA,
			cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
		if err != nil {
			return nil, fmt.Errorf("auth mtls client config: %w", err)
		}
	}
	cl, err := authclient.Dial(addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("auth master dial %s: %w", addr, err)
	}
	return cl, nil
}
