package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"time"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbuild"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/locks"
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
	var st threadStats
	return runThreadBackfillInto(o, &st)
}

// runThreadBackfillInto is runThreadBackfill with the tally handed in, so a
// test can read the numbers the run reported rather than parsing them back out
// of a log line.
func runThreadBackfillInto(o threadOpts, st *threadStats) error {
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
	resolver := layoutResolver(cfg, o.Root, o.Template)
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
	// One backend per driver, chosen per user, because a deployment holds
	// accounts of several drivers at once: the sandbox alone runs mdbox,
	// maildir and sdbox side by side. A single --driver for the whole run
	// makes every account of the other two unreadable.
	byDriver := func(d string) mailbox.MailboxBackend {
		return mailboxbuild.ByDriver(d, cfg.Storage, locker)
	}
	// Resolving one account is a value rather than a call, so a test can hand
	// this step an account with a driver and a mail root of its own without a
	// live userdb -- which is the only shape in which #1456 is visible.
	resolveUser := func(user string) (*mailbox.UserInfo, error) {
		return guidUserInfo(resolver, authcl, guidOpts{
			Offline: o.Offline, IndexTmpl: o.IndexTmpl, MailTmpl: o.MailTmpl,
		}, user)
	}
	for _, user := range users {
		if err := threadUser(boxBE, byDriver, idxBE, resolveUser, locker, o, user, st); err != nil {
			return fmt.Errorf("thread backfill %s: %w", user, err)
		}
	}
	slog.Info("thread backfill complete", "users", st.Users, "skipped", st.Skipped,
		"folders", st.Folders, "messages", st.Messages, "threads", st.Threads,
		"unreadable", st.Unreadable, "driver", driver, "dry_run", o.DryRun)
	return nil
}

// threadUser rebuilds one account, holding the account's thread lock across
// the WHOLE sequence -- the skip check, the walk and the install.
//
// The walk is long: it reads every message's headers. A delivery that landed
// during it has already appended to the live sidecar, and installing a file
// built before that delivery would erase it. The lock is what makes "rebuild"
// mean the account's history plus nothing lost, rather than the history as it
// looked when the walk started.
//
// With no lock service (a stopped store, the case the warning above names)
// there is nothing to hold, and the tool says so rather than pretending.
func threadUser(boxBE mailbox.MailboxBackend, byDriver func(string) mailbox.MailboxBackend, idxBE mailbox.IndexBackend, resolveUser func(string) (*mailbox.UserInfo, error), locker locks.Locker, o threadOpts, user string, st *threadStats) error {
	if locker == nil {
		return threadUserLocked(boxBE, byDriver, idxBE, resolveUser, o, user, st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), backfillLockTimeout)
	defer cancel()
	return locks.WithLock(ctx, locker, locks.ThreadsKey(user),
		locks.Owner(user, ""), backfillLockTTL, backfillLockRenew,
		func(context.Context) error {
			return threadUserLocked(boxBE, byDriver, idxBE, resolveUser, o, user, st)
		})
}

// The account's thread lock during a rebuild. The timeout bounds the whole
// account rather than one write, because that is what is held; the TTL is
// short with a renew loop under it, so a tool that dies mid-account releases
// in seconds instead of blocking delivery until the TTL runs out.
const (
	backfillLockTimeout = 30 * time.Minute
	backfillLockTTL     = 15 * time.Second
	backfillLockRenew   = 5 * time.Second
)

func threadUserLocked(boxBE mailbox.MailboxBackend, byDriver func(string) mailbox.MailboxBackend, idxBE mailbox.IndexBackend, resolveUser func(string) (*mailbox.UserInfo, error), o threadOpts, user string, st *threadStats) error {
	// Through userdb, exactly as a session resolves the account. Passing nil
	// here asked the resolver for defaults instead, which gave every account
	// the default driver's mail path: the sidecar was looked for beside a
	// Maildir that mdbox and sdbox accounts do not have, and written to the
	// home directory rather than to the mail root the deliveries use (#1456).
	info, err := resolveUser(user)
	if err != nil {
		return err
	}
	// The account's own driver decides its storage, with the run-wide backend
	// as the fallback for an account userdb says nothing about.
	box := mailbox.SelectPersonalBackend(boxBE, byDriver, info.Driver)
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

	userBox := box.OpenUser(info)
	defer userBox.Close() //nolint:errcheck
	idx := idxBE.OpenUser(info)
	defer idx.Close() //nolint:errcheck

	entries, err := userBox.ListFolders()
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

	before := st.Messages
	state, err := buildSidecar(userBox, idx, names, path, user, st)
	if err != nil {
		return err
	}
	messages := st.Messages - before

	st.Users++
	st.Threads += len(state.Threads())
	if o.DryRun {
		// The state is real, so these numbers are the ones a real run would
		// produce. Only the install is skipped, and the working file goes with
		// it: a dry run leaves nothing behind.
		_ = os.Remove(path + ".rebuild")
		slog.Info("thread backfill: would build", "user", user, "folders", len(names),
			"messages", messages, "threads", len(state.Threads()), "path", path)
		return nil
	}
	// Whole-file replacement: the account's threading is either the old file
	// or the new one, never a half-written mixture of two histories.
	if err := os.Rename(path+".rebuild", path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	slog.Info("thread backfill: built", "user", user, "folders", len(names),
		"messages", messages, "threads", len(state.Threads()), "path", path)
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
func buildSidecar(box mailbox.UserMailbox, idx mailbox.UserIndex, names []string, path, user string, st *threadStats) (*threads.State, error) {
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
			p := threads.PlacementFor(state, mailbox.FormatObjectID(m.GUID), head)
			// Written even on a dry run, and deliberately: Append is what
			// applies a placement to the state, so skipping it leaves every
			// later message seeing "nothing to join" and the run reporting
			// zero conversations for any mailbox. A dry run exists to give an
			// operator numbers before choosing a window; numbers taken in a
			// mode the real run never uses are worse than none. The file is
			// temporary either way -- what a dry run skips is the install.
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
