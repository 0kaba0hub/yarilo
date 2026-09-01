package file

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxconv"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/internal/userstate/subs"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// mailRootDir is where the messages are, as opposed to where our index is. The
// two are different questions: our index follows INDEX=, their files sit with
// the mail, and the mdbox driver roots the mail one level below the home.
func (u *userIndex) mailRootDir() string {
	return dboxconv.StoreRoot(u.home, u.mailPath, u.driver)
}

// foreignRoots are the roots another implementation may have kept its index
// under, most specific first.
//
// Their index follows their own INDEX= setting, exactly as ours follows ours:
// with one set, both a folder's index and the map live under it and only the
// message files stay with the mail. A store written that way has nothing at all
// under the mail root except payloads, so looking there alone finds no foreign
// folder and converts nothing -- silently, since a store with no foreign index
// is indistinguishable from a store already converted (#1583).
//
// Which root is theirs cannot be read off their config, which is not here. Ours
// is the one candidate worth trying, because an in-place conversion happens on
// a deployment pointed at the store the other implementation left, laid out the
// way it left it.
func (u *userIndex) foreignRoots() []string {
	if u.indexRoot != "" && u.indexRoot != u.mailRootDir() {
		return []string{u.indexRoot, u.mailRootDir()}
	}
	return []string{u.mailRootDir()}
}

// foreignFolderDir returns the directory holding this folder's foreign index,
// and the root it was found under, or false when there is none.
//
// Two shapes per root, because the reference writes two. Beside the messages
// the index sits inside the dbox-Mails leaf; under a separate index root it can
// sit in the mailbox directory itself, with no leaf at all:
//
//	index/mailboxes/INBOX/dovecot.index.log
//	index/mailboxes/Archive/2026/dovecot.index.log
//
// Both were taken from live stores rather than derived -- the first from the
// local install, the flat one from the field (#1583). Which one appears depends
// on a setting of theirs that is not in reach from here, so both are looked for
// and the first that exists wins. Trying only one leaves a store looking
// already converted, which is the silent outcome this path exists to avoid.
func (u *userIndex) foreignFolderDir(folder string) (dir, root string, ok bool) {
	sub := mailbox.FolderSubpathEscaped(u.driver, folder, folder, u.separator, u.escapeChar)
	// The mailbox directory itself: the same path without the driver's leaf.
	flat := filepath.Dir(sub)
	for _, r := range u.foreignRoots() {
		for _, candidate := range []string{filepath.Join(r, sub), filepath.Join(r, flat)} {
			if dboxconv.HasForeignFolder(candidate) {
				return candidate, r, true
			}
		}
	}
	return "", "", false
}

// foreignMapDir returns the directory holding their map. It follows their index
// rather than their mail: the map is index-side state, and INDEX= moves it.
func (u *userIndex) foreignMapDir(root string) (string, bool) {
	for _, r := range append([]string{root}, u.foreignRoots()...) {
		if candidate := filepath.Join(r, "storage"); dboxconv.HasForeignMap(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// convertForeignFolder builds this folder's index from another implementation's,
// in place, and reports whether it did.
//
// Called where a fresh empty index would otherwise be created, and under the
// same lock, so the whole conversion -- read theirs, write ours, remove theirs
// -- is one critical section. Two sessions opening a folder at once convert it
// once, and no session can observe a half-removed pair (#1524).
//
// Both dbox drivers. What differs is where a message is: mdbox addresses one
// through a store-wide map, which has to be whole before any folder reads
// through it, and sdbox does not -- a single-message file sits in its folder's
// own directory, so its position is its path. The sdbox half is
// convertForeignSdboxFolder.
func (u *userIndex) convertForeignFolder(fs *folderState) (bool, error) {
	switch u.driver {
	case "mdbox", "sdbox", "dbox":
	default:
		return false, nil
	}
	dir, foreignRoot, ok := u.foreignFolderDir(fs.folder)
	if !ok {
		// Their names are spelled the way their configuration said, ours the
		// way this one does, and where the two differ nothing matches by name:
		// the folder is not found, so nothing is converted, and every non-ASCII
		// folder in the store is invisible (#1586).
		//
		// So a miss is not yet an answer. If anything of theirs is still in the
		// tree, the disk is brought to this deployment's encoding once, and the
		// lookup is asked again. The walk costs a store with foreign leftovers
		// in it, which is exactly the window this runs in.
		adopted, aerr := u.adoptForeignNames()
		if aerr != nil {
			return false, aerr
		}
		if !adopted {
			return false, nil
		}
		if dir, foreignRoot, ok = u.foreignFolderDir(fs.folder); !ok {
			return false, nil
		}
	}
	if u.driver != "mdbox" {
		return u.convertForeignSdboxFolder(fs, dir)
	}
	// Before any of the work: a conversion writes our index, imports their map
	// and unlinks their files, so a store that cannot be written to fails
	// partway having paid for every step before it -- and pays again on the
	// next open, and the one after that. Refusing up front turns repeated
	// silent work into one loud answer (#1571).
	//
	// A read-only store is often deliberate -- a snapshot, a replica -- so the
	// refusal names the offline path rather than inventing a way to half-serve
	// the folder. Nothing is remembered about the attempt: a "we tried" marker
	// would be another silent state, and the next open should say the same
	// thing until somebody changes the mount.
	//
	// Four directories, because the section writes to four. Two of them are
	// storage directories rather than one: their map is where they left it,
	// under the mail root, and it is read and finally deleted there; ours is
	// opened and written where the driver will look for it, which INDEX= moves
	// out of the mail tree. Writing ours beside theirs left the driver opening
	// an empty map of its own -- the folder index pointed at map uids nothing
	// could resolve, every body came back unreadable, and the rebuild that
	// follows dropped the records (#1579).
	// Resolving the paths is stat and nothing else, so it happens before the
	// probe without breaking the probe's rule. What it must not do is act on
	// what it finds: a missing map of theirs is refused below, after the probe,
	// so a store that is both unwritable and incomplete answers with the fault
	// an operator can do something about.
	theirStorage, haveTheirMap := u.foreignMapDir(foreignRoot)
	if !haveTheirMap {
		theirStorage = filepath.Join(foreignRoot, "storage")
	}
	// ourStorage mirrors the mdbox driver's mapStoragePath(). It is a different
	// layer and cannot be called from here, so the rule is written twice on
	// purpose -- and writing it twice is what produced #1579 in the first
	// place. The pair is held by a test rather than by care:
	// TestConversionBodiesReadableWithASeparateIndexTree reads bodies through
	// the driver after converting with INDEX= set, so the two halves drifting
	// apart again fails there.
	ourStorage := theirStorage
	if u.indexRoot != "" {
		ourStorage = filepath.Join(u.indexRoot, "storage")
	}
	// Our index directory and our storage directory have to exist before they
	// can be probed, and with INDEX= set they are a tree of their own. Creating
	// them is work the conversion would do a moment later anyway.
	for _, d := range []string{fs.indexDir, ourStorage} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return false, fmt.Errorf("fileindex/convert: folder %q: %w (%v)", fs.folder, dboxconv.ErrReadOnly, err)
		}
	}
	if err := dboxconv.CheckWritable(fs.indexDir, dir, theirStorage, ourStorage); err != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: %w", fs.folder, err)
	}
	if !haveTheirMap {
		// A folder of theirs with no map of theirs: the messages it names
		// cannot be located at all. Refused rather than converted into an empty
		// folder, which is the one outcome nobody checks.
		return false, fmt.Errorf("fileindex/convert: folder %q has a foreign index and no foreign map under %s",
			fs.folder, foreignRoot)
	}

	var opts []mdboxmap.Option
	if u.b.locker != nil {
		opts = append(opts, mdboxmap.WithLocker(u.b.locker), mdboxmap.WithOwner(u.owner))
	}
	m, err := mdboxmap.Open(ourStorage, u.username, opts...)
	if err != nil {
		return false, fmt.Errorf("fileindex/convert: open map: %w", err)
	}
	defer m.Close() //nolint:errcheck

	// The map is store-wide and has to be whole before any folder reads through
	// it. Converting it here, on the first folder that needs it, keeps the
	// store's shape correct without a separate pass.
	//
	// The folder lock this runs under is per folder, so it does not order two
	// sessions opening two different folders. The map's own lock does, and
	// ConvertMap decides whether to import inside it: without that both
	// sessions find an empty map and import their whole store twice.
	n, err := dboxconv.ConvertMap(theirStorage, m)
	if err != nil {
		return false, fmt.Errorf("fileindex/convert: map: %w", err)
	}
	if n > 0 {
		slog.Info("fileindex: converted a foreign map", "user", u.username, "records", n,
			"from", theirStorage, "to", ourStorage)
	}

	// Subscriptions are store-wide like the map, and they are the one piece of
	// this conversion a user sees the moment it happens: a folder list that
	// changed under them. Carried on the first folder that converts, which is
	// also the first moment anything of ours exists to carry them into.
	//
	// Adding is idempotent, so running it per folder costs a few names and
	// needs no marker of its own. Union rather than replace: a store being
	// converted may already carry subscriptions of ours, and dropping either
	// side loses a user's own choice.
	if dboxconv.HasForeignSubscriptions(u.mailRootDir()) {
		if err := u.carryForeignSubscriptions(); err != nil {
			return false, err
		}
	}

	corr, err := dboxconv.NewMapCorrespondence(theirStorage, m)
	if err != nil {
		return false, fmt.Errorf("fileindex/convert: pair maps: %w", err)
	}
	metas, hdr, err := dboxconv.ConvertFolder(dir, corr)
	if err != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: %w", fs.folder, err)
	}

	// Their map carries no GUIDs, so the records the import appended have none;
	// their folder index does carry one per message, and it is in hand right
	// now. Stamping here is what makes the storage rebuild able to pair a
	// physical record with its map entry on a converted store, and it costs a
	// map write rather than a walk over every storage file (#1573).
	guids := make(map[uint32][16]byte, len(metas))
	for _, meta := range metas {
		mapUID, perr := strconv.ParseUint(meta.Filename, 10, 32)
		if perr != nil {
			return false, fmt.Errorf("fileindex/convert: folder %q uid %d: map uid %q: %w",
				fs.folder, meta.UID, meta.Filename, perr)
		}
		guids[uint32(mapUID)] = meta.GUID
	}
	if n, gerr := m.SetGUIDs(guids); gerr != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: stamp guids: %w", fs.folder, gerr)
	} else if n > 0 {
		slog.Debug("fileindex: stamped guids onto converted map records",
			"user", u.username, "folder", fs.folder, "records", n)
	}

	// Their UID space, kept whole: the same UIDVALIDITY, the same UIDs, and
	// their next_uid rather than one past the highest surviving message. A
	// client reconnecting over this mailbox then finds what it left (#1568).
	//
	// The uidValidity the opener asked for plays no part, which is why it is
	// not a parameter here: a folder that exists in their store is not a new
	// folder, and the only right answer is the one their index carries.
	if hdr.UIDValidity == 0 {
		return false, fmt.Errorf("fileindex/convert: folder %q: their index carries no uid_validity", fs.folder)
	}
	if err := fs.createFresh(hdr.UIDValidity); err != nil {
		return false, err
	}
	for _, meta := range metas {
		if err := fs.appendLocked(meta); err != nil {
			return false, fmt.Errorf("fileindex/convert: folder %q uid %d: %w", fs.folder, meta.UID, err)
		}
	}
	// next_uid after the appends, which raise it to the highest uid plus one.
	// Theirs can be higher -- a message appended and then expunged moved their
	// counter and left nothing behind -- and reusing that number would hand a
	// client a uid it has already seen carrying different mail.
	if hdr.NextUID > fs.file.Header.NextUID {
		fs.file.Header.NextUID = hdr.NextUID
	}
	// Ours durable before theirs is unlinked. The lock orders sessions; this
	// orders the disk, and a crash between the two has to leave a folder one of
	// the two servers can still open.
	//
	// Both halves are needed and neither substitutes for the other: the file's
	// own bytes have to reach the disk before the rename, and the directory
	// entry has to reach it before the unlink. An index flush is not durable by
	// default anywhere else, because everywhere else the state still exists
	// somewhere if the tail is lost.
	fs.fsyncOnFlush = true
	defer func() { fs.fsyncOnFlush = false }()
	if err := fs.flush(true); err != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: %w", fs.folder, err)
	}
	if err := fsyncDir(fs.indexDir); err != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: %w", fs.folder, err)
	}
	if err := dboxconv.RemoveForeignFolder(dir); err != nil {
		return false, err
	}
	slog.Info("fileindex: converted a foreign folder", "user", u.username, "folder", fs.folder,
		"messages", len(metas), "from", dir)

	// The store's conversion ends when its last folder does. Their map has to
	// outlive every folder that still reads through it, so what decides is not
	// this folder but whether any of theirs is left anywhere in the tree
	// (#1569). A store with a folder nobody ever opens keeps their map, which
	// is correct: that folder still needs it.
	//
	// A failure here is logged and not returned. The folder in hand is
	// converted and readable; refusing to open it because a file elsewhere
	// could not be unlinked would turn a tidying step into an outage.
	dropped, derr := dboxconv.DropForeignMapIfDone(theirStorage, filepath.Join(foreignRoot, "mailboxes"), u.mailRootDir(), m)
	switch {
	case derr != nil:
		slog.Warn("fileindex: could not finish the store conversion; their map is still in place",
			"user", u.username, "storage", theirStorage, "err", derr)
	case dropped:
		slog.Info("fileindex: store fully converted; removed their map",
			"user", u.username, "storage", theirStorage)
	}
	return true, nil
}

// fsyncDir flushes a directory entry, so a file written inside it is found
// again after a crash rather than merely having been written.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("fsync dir %s: %w", dir, err)
	}
	defer d.Close() //nolint:errcheck
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync dir %s: %w", dir, err)
	}
	return nil
}

// carryForeignSubscriptions copies their subscribed folder list into ours.
//
// Their file lives with the mail; ours lives in the control root, which is a
// different directory whenever a deployment sets one. The rule for that comes
// from mailbox.ControlRoot, called where this index was opened -- the same
// function the sessions and the admin API call, rather than a second spelling
// of it (#1579).
func (u *userIndex) carryForeignSubscriptions() error {
	names, err := subs.ReadForeign(filepath.Join(u.mailRootDir(), "subscriptions"), u.separator)
	if err != nil {
		return fmt.Errorf("fileindex/convert: subscriptions: %w", err)
	}
	// The personal namespace's file, by the same helper every caller uses. A
	// foreign store has one subscription file and it is the personal one; the
	// per-namespace siblings have no counterpart there.
	store := subs.New(u.controlRoot, mailbox.NamespaceSubsFile("", u.separator, "personal"),
		u.username, u.owner, u.b.locker)
	for _, name := range names {
		if err := store.Add(name); err != nil {
			return fmt.Errorf("fileindex/convert: subscribe %q: %w", name, err)
		}
	}
	slog.Info("fileindex: carried their subscriptions", "user", u.username,
		"folders", len(names), "from", u.mailRootDir(), "to", u.controlRoot)
	return nil
}

// adoptForeignNames brings the folder directories of a store still holding
// foreign state to this deployment's name encoding, and reports whether it
// renamed anything.
//
// Both trees, because a folder is a directory in each: the mail tree holds its
// messages and the index tree holds its index, and a rename in one without the
// other splits the folder in half.
func (u *userIndex) adoptForeignNames() (bool, error) {
	roots := u.foreignRoots()
	stale := false
	for _, r := range roots {
		left, err := dboxconv.AnyForeignFolderLeft(filepath.Join(r, "mailboxes"))
		if err != nil {
			return false, fmt.Errorf("fileindex/convert: %w", err)
		}
		if left {
			stale = true
			break
		}
	}
	if !stale {
		return false, nil
	}

	total := 0
	// The mail tree as well as the index roots: a store with INDEX= set has its
	// folders named in both, and only one of them is in foreignRoots when the
	// two differ.
	for _, r := range append(roots, u.mailRootDir()) {
		n, err := dboxconv.AdoptNames(filepath.Join(r, "mailboxes"), u.listUTF8)
		if err != nil {
			return false, fmt.Errorf("fileindex/convert: %w", err)
		}
		total += n
	}
	if total == 0 {
		return false, nil
	}
	slog.Info("fileindex: brought a foreign store's folder names to this deployment's encoding",
		"user", u.username, "renamed", total, "list_utf8", u.listUTF8)
	return true, nil
}

// convertForeignSdboxFolder is the same conversion for a store that keeps one
// message per file: their folder index becomes ours, and the mail is already
// where our driver looks for it (#1592).
//
// Everything store-wide falls away. There is no map to import first, no
// correspondence to pair uids through, and no map guids to stamp. What is left
// is their folder index and the two pieces that belong to any store being taken
// over: the name encoding, done by the caller before this is reached, and the
// subscriptions.
//
// The critical section is the one the mdbox path uses and for the same reason:
// ours is written and fsynced, then theirs is unlinked, all under the folder
// lock the caller holds.
func (u *userIndex) convertForeignSdboxFolder(fs *folderState, dir string) (bool, error) {
	// Their index and their mail are two directories whenever their INDEX= is
	// set, and the reference store this was checked against has exactly that
	// shape: index/mailboxes/INBOX holds the log, sdbox/mailboxes/INBOX/
	// dbox-Mails holds u.1 to u.4. Reading the file names from the index
	// directory finds none, and the folder converts to nothing at all.
	mailDir := filepath.Join(u.mailRootDir(),
		mailbox.FolderSubpathEscaped(u.driver, fs.folder, fs.folder, u.separator, u.escapeChar))

	// Two directories rather than four: ours to write the index into, theirs to
	// unlink from. Neither of the storage directories exists in this shape.
	if err := os.MkdirAll(fs.indexDir, 0o700); err != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: %w (%v)", fs.folder, dboxconv.ErrReadOnly, err)
	}
	if err := dboxconv.CheckWritable(fs.indexDir, dir); err != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: %w", fs.folder, err)
	}

	if dboxconv.HasForeignSubscriptions(u.mailRootDir()) {
		if err := u.carryForeignSubscriptions(); err != nil {
			return false, err
		}
	}

	metas, hdr, missing, err := dboxconv.ConvertSdboxFolder(dir, mailDir)
	if err != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: %w", fs.folder, err)
	}
	for _, uid := range missing {
		slog.Warn("fileindex: a message their index names has no file, so it is not carried over",
			"user", u.username, "folder", fs.folder, "uid", uid, "dir", mailDir)
	}
	if hdr.UIDValidity == 0 {
		return false, fmt.Errorf("fileindex/convert: folder %q: their index carries no uid_validity", fs.folder)
	}
	if err := fs.createFresh(hdr.UIDValidity); err != nil {
		return false, err
	}
	// Their sdbox index carries no guid, so the records appended below have
	// none. A fresh index is marked guid-complete, which would mean nothing
	// ever comes back to fill them and every adopted message loses its EMAILID
	// for good; marked pending, the backfill that already runs on every select
	// stamps them from the message files through the driver's own scan -- the
	// one place that knows how to read them (#1592).
	if ext := findExt(fs.file.Extensions, extNameGUID); ext != nil {
		ext.HdrData = encodeGUIDHdr(guidStatePending)
	}
	for _, meta := range metas {
		if err := fs.appendLocked(meta); err != nil {
			return false, fmt.Errorf("fileindex/convert: folder %q uid %d: %w", fs.folder, meta.UID, err)
		}
	}
	if hdr.NextUID > fs.file.Header.NextUID {
		fs.file.Header.NextUID = hdr.NextUID
	}
	fs.fsyncOnFlush = true
	defer func() { fs.fsyncOnFlush = false }()
	if err := fs.flush(true); err != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: %w", fs.folder, err)
	}
	if err := fsyncDir(fs.indexDir); err != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: %w", fs.folder, err)
	}
	if err := dboxconv.RemoveForeignSdboxFolder(dir); err != nil {
		return false, err
	}
	slog.Info("fileindex: converted a foreign folder", "user", u.username, "folder", fs.folder,
		"messages", len(metas), "skipped", len(missing), "from", dir)
	return true, nil
}

// AdoptForeignNames brings a foreign store's folder directories to this
// deployment's name encoding, before anything lists them.
//
// It used to run only from a folder conversion, which begins when a folder is
// opened -- and a client lists before it opens anything. So the first listing
// after a takeover served their encoding, and the subscribed folders in it came
// back \NonExistent: their names had been read from their subscriptions file
// and converted, and no directory of those names existed yet. Three of four
// folders were unreachable until the connection after (#1609).
//
// Under the per-user index lock, because two connections on one login is
// ordinary and this renames directories in two trees with no folder lock held.
// The second holder finds nothing left to rename.
func (u *userIndex) AdoptForeignNames() error {
	run := func() error {
		_, err := u.adoptForeignNames()
		return err
	}
	if u.b.locker == nil {
		return run()
	}
	key := locks.IndexKey(u.username)
	if u.b.locker.HoldsResource(key) {
		return run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, u.b.locker, key, u.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("fileindex/adopt names: %w", err)
	}
	defer func() { _ = u.b.locker.Unlock(ctx, lk.ID) }()
	return run()
}

// freshUIDValidity is the value a folder that has no past is created with.
//
// The caller's number is a wall-clock stamp, and a stamp repeats: two folders
// created in one second share it, and so does a folder recreated in the same
// second as its own delete -- which RFC 3501 §6.3.4 forbids because a client's
// cached UIDs would still look valid for different mail (#1614). The allocator
// takes the stamp as a floor and never returns a number twice.
//
// A failure here falls back to the caller's stamp. The allocator is a
// correctness improvement over that, not a precondition for opening a mailbox,
// and refusing the folder would be the larger harm.
func (u *userIndex) freshUIDValidity(requested uint32) uint32 {
	if u.uidValidity == nil {
		return requested
	}
	v, err := u.uidValidity.Next(requested)
	if err != nil {
		slog.Warn("fileindex: uidvalidity allocator failed; falling back to the caller's stamp",
			"user", u.username, "requested", requested, "err", err)
		return requested
	}
	return v
}
