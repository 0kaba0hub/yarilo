package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbuild"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Threading state is built here and nowhere else for existing accounts: there
// is no lazy path, by decision (#1425). Until this step has run for an
// account, every message is its own conversation -- which is exactly how the
// server behaved before threading existed, so an unmigrated account is not a
// half-state, it is the old state.
type threadOpts struct {
	ConfigPath string
	Driver     string
	Root       string
	Template   string
	User       string
	Offline    bool
	IndexTmpl  string
	MailTmpl   string
	DryRun     bool
	Force      bool
}

type threadStats struct {
	Users      int
	Skipped    int
	Folders    int
	Messages   int
	Threads    int
	Unreadable int
}

func runThreadBackfill(o threadOpts) error {
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
	authcl, err := guidAuthClient(cfg, guidOpts{ConfigPath: o.ConfigPath, Offline: o.Offline})
	if err != nil {
		return err
	}
	if authcl != nil {
		defer authcl.Close() //nolint:errcheck
	}
	resolver := guidResolver(cfg, guidOpts{Root: o.Root, Template: o.Template})
	driver := o.Driver
	if driver == "" {
		driver = cfg.Storage.MailDriver
	}
	if driver == "" {
		return fmt.Errorf("no storage driver: set --driver or storage.mailbox in --config")
	}
	boxBE := mailboxbuild.ByDriver(driver, cfg.Storage, locker)
	// Never fabricate an index: a fresh one reads as an empty folder, and this
	// step would then write a sidecar saying the account has no mail.
	idxBE := indexfile.New(indexfile.WithLocker(locker), indexfile.WithNoCreate())

	users, err := guidUsers(resolver.Root, resolver.HomeTemplate, o.User)
	if err != nil {
		return err
	}
	var st threadStats
	for _, user := range users {
		if err := threadUser(boxBE, idxBE, resolver, o, user, &st); err != nil {
			return fmt.Errorf("thread backfill %s: %w", user, err)
		}
	}
	slog.Info("thread backfill complete", "users", st.Users, "skipped", st.Skipped,
		"folders", st.Folders, "messages", st.Messages, "threads", st.Threads,
		"unreadable", st.Unreadable, "driver", driver, "dry_run", o.DryRun)
	return nil
}

func threadUser(boxBE mailbox.MailboxBackend, idxBE mailbox.IndexBackend, resolver *mailbox.Resolver, o threadOpts, user string, st *threadStats) error {
	info, err := guidUserInfo(resolver, nil, guidOpts{
		Offline: true, IndexTmpl: o.IndexTmpl, MailTmpl: o.MailTmpl,
	}, user)
	if err != nil {
		return err
	}
	path := threads.PathFor(info)
	if path == "" {
		return fmt.Errorf("no control root for %s", user)
	}
	if !o.Force {
		if _, serr := os.Stat(path); serr == nil {
			// Already built. Rebuilding is a whole-file replacement, so it is
			// asked for rather than assumed: a rerun of the tool over a live
			// deployment should not rewrite state the deliveries have been
			// extending.
			st.Skipped++
			slog.Debug("thread backfill: already built, skipping", "user", user, "path", path)
			return nil
		}
	}

	box := boxBE.OpenUser(info)
	defer box.Close() //nolint:errcheck
	idx := idxBE.OpenUser(info)
	defer idx.Close() //nolint:errcheck

	entries, err := box.ListFolders()
	if err != nil {
		return fmt.Errorf("list folders: %w", err)
	}
	// Deterministic order, and this is not cosmetic. Which message names a
	// conversation depends on which is seen first, so two runs over the same
	// mailbox must walk it the same way -- otherwise a rebuild produces
	// different thread ids from the same history, and the rebuildability this
	// design leans on evaporates.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Selectable {
			names = append(names, e.Name)
		}
	}

	state, err := buildSidecar(box, idx, names, path, o, user, st)
	if err != nil {
		return err
	}

	st.Users++
	st.Threads += len(state.Threads())
	if o.DryRun {
		slog.Info("thread backfill: would build", "user", user,
			"messages", st.Messages, "threads", len(state.Threads()), "path", path)
		return nil
	}
	// Whole-file replacement: the account's threading is either the old file
	// or the new one, never a half-written mixture of two histories.
	if err := os.Rename(path+".rebuild", path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	slog.Info("thread backfill: built", "user", user, "folders", len(names),
		"threads", len(state.Threads()), "path", path)
	return nil
}

// buildSidecar walks the account and writes the rebuilt sidecar to a temporary
// file, returning the state it folded.
//
// The folder order is normalised HERE rather than trusted from the caller, and
// that is the whole of the rebuildability claim: which message names a
// conversation depends on which is seen first, so a rebuild that walked the
// mailbox in whatever order the filesystem offered would produce different
// thread ids from the same history -- and every client's cached conversation
// would be wrong after a rerun.
func buildSidecar(box mailbox.UserMailbox, idx mailbox.UserIndex, names []string, path string, o threadOpts, user string, st *threadStats) (*threads.State, error) {
	ordered := append([]string(nil), names...)
	sort.Strings(ordered)

	state := threads.Empty()
	tmp := path + ".rebuild"
	_ = os.Remove(tmp)

	for _, name := range ordered {
		st.Folders++
		folder, ferr := idx.OpenFolder(name, 0)
		if ferr != nil {
			return nil, fmt.Errorf("open %s: %w", name, ferr)
		}
		metas, merr := mailbox.ReadMessages(idx, folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
		if merr != nil {
			return nil, fmt.Errorf("read %s: %w", name, merr)
		}
		sort.Slice(metas, func(i, j int) bool { return metas[i].UID < metas[j].UID })
		for _, m := range metas {
			if m.GUID == ([16]byte{}) {
				// No identity, nothing to key a conversation by. The GUID
				// backfill is the step that fixes this, and saying so beats
				// threading it under a zero id shared by every such message.
				st.Unreadable++
				continue
			}
			head, herr := readHeaders(box, name, m)
			if herr != nil {
				st.Unreadable++
				slog.Warn("thread backfill: message unreadable, left unthreaded",
					"user", user, "folder", name, "uid", m.UID, "err", herr)
				continue
			}
			st.Messages++
			p := threads.PlacementFor(state, hex.EncodeToString(m.GUID[:]), head)
			if o.DryRun {
				continue
			}
			if aerr := threads.Append(tmp, state, p); aerr != nil {
				return nil, fmt.Errorf("append %s: %w", name, aerr)
			}
		}
	}
	return state, nil
}

// readHeaders reads a message up to the end of its headers.
//
// Threading needs four headers and nothing else, and an account being migrated
// can be tens of gigabytes: reading whole bodies to find the top of each one
// would make this step cost the size of the mail store rather than the size of
// its metadata.
func readHeaders(box mailbox.UserMailbox, folder string, m *mailbox.MessageMeta) ([]byte, error) {
	rc, err := box.Fetch(folder, m.Filename, m.AltTier)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck

	var out []byte
	br := bufio.NewReader(rc)
	for {
		line, rerr := br.ReadBytes('\n')
		out = append(out, line...)
		if len(out) > maxHeaderBytes {
			// A message whose headers never end is malformed; take what we
			// have rather than reading a body into memory looking for a blank
			// line that is not coming.
			return out, nil
		}
		if rerr != nil {
			if rerr == io.EOF {
				return out, nil
			}
			return nil, rerr
		}
		if len(line) == 2 && line[0] == '\r' && line[1] == '\n' {
			return out, nil
		}
		if len(line) == 1 && line[0] == '\n' {
			return out, nil
		}
	}
}

// maxHeaderBytes bounds a single message's header block.
const maxHeaderBytes = 256 * 1024
