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
// under, most specific first: their INDEX= moves both a folder's index and the
// map off the mail root, leaving only payloads there, so looking at the mail
// root alone silently converts nothing (#1583). Their setting is not readable
// from here, so ours is the one candidate worth trying.
func (u *userIndex) foreignRoots() []string {
	if u.indexRoot != "" && u.indexRoot != u.mailRootDir() {
		return []string{u.indexRoot, u.mailRootDir()}
	}
	return []string{u.mailRootDir()}
}

// foreignFolderDir returns the directory holding this folder's foreign index and
// the root it was found under. Two shapes per root, since the reference writes
// two -- inside the dbox-Mails leaf beside the messages, or flat in the mailbox
// directory under a separate index root -- both taken from live stores (#1583),
// and trying only one leaves a store looking already converted.
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

// convertForeignFolder builds this folder's index from another implementation's
// in place, under the same lock a fresh empty index would use -- one critical
// section, so two sessions opening at once convert it once and neither observes
// a half-removed pair (#1524). Both dbox drivers: mdbox addresses a message
// through a store-wide map that must be whole first, sdbox through its own
// path. The sdbox half is convertForeignSdboxFolder.
func (u *userIndex) convertForeignFolder(fs *folderState) (bool, error) {
	switch u.driver {
	case "mdbox", "sdbox", "dbox":
	default:
		return false, nil
	}
	dir, foreignRoot, ok := u.foreignFolderDir(fs.folder)
	if !ok {
		// A miss is not yet an answer: their names may be spelled by their
		// encoding, ours by this deployment's, and every non-ASCII folder is
		// invisible until the disk is brought to our encoding once (#1586).
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
	// Refused up front rather than failing partway: a conversion writes our
	// index, imports their map and unlinks their files, so an unwritable store
	// would pay for every step and pay again on the next open (#1571). Nothing
	// is remembered about the attempt -- the next open says the same thing
	// until the mount changes.
	//
	// Four directories: theirs and ours are separate storage dirs, since
	// INDEX= moves ours out of the mail tree. Writing ours beside theirs once
	// left the driver opening an empty map of its own, unresolvable map uids,
	// and a rebuild that dropped the records (#1579). Resolving the paths is
	// stat only, so it happens before the writability probe; a missing map of
	// theirs is still refused after the probe, not acted on before it.
	theirStorage, haveTheirMap := u.foreignMapDir(foreignRoot)
	if !haveTheirMap {
		theirStorage = filepath.Join(foreignRoot, "storage")
	}
	// ourStorage mirrors the mdbox driver's mapStoragePath(), a different layer
	// this cannot call -- writing the rule twice is what produced #1579.
	// TestConversionBodiesReadableWithASeparateIndexTree is what keeps the two
	// from drifting apart again.
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
		// A folder of theirs with no map of theirs cannot locate its messages
		// at all. Refused rather than converted into an empty folder.
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

	// The map is store-wide and must be whole before any folder reads through
	// it; converted here, on the first folder that needs it. The folder lock
	// this runs under does not order two sessions opening different folders --
	// the map's own lock does, and ConvertMap decides whether to import inside
	// it, or both sessions find an empty map and import twice.
	n, err := dboxconv.ConvertMap(theirStorage, m)
	if err != nil {
		return false, fmt.Errorf("fileindex/convert: map: %w", err)
	}
	if n > 0 {
		slog.Info("fileindex: converted a foreign map", "user", u.username, "records", n,
			"from", theirStorage, "to", ourStorage)
	}

	// Subscriptions are store-wide like the map, and the one piece a user sees
	// the moment it happens: a folder list changed under them. Carried on the
	// first folder that converts. Adding is idempotent and a union, not a
	// replace -- a store being converted may already carry ours.
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

	// Their map carries no GUIDs; their folder index does, and it is in hand
	// right now. Stamping it lets a later rebuild pair records by GUID at the
	// cost of a map write, not a walk over every storage file (#1573).
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

	// Their UID space kept whole -- same UIDVALIDITY, same UIDs, their next_uid
	// -- so a reconnecting client finds what it left (#1568). The uidValidity
	// the opener asked for plays no part: a folder that exists is not new.
	if hdr.UIDValidity == 0 {
		return false, fmt.Errorf("fileindex/convert: folder %q: their index carries no uid_validity", fs.folder)
	}
	if err := fs.createFresh(hdr.UIDValidity); err != nil {
		return false, err
	}
	u.rememberIdentity(fs.folder, hdr.UIDValidity)
	for _, meta := range metas {
		if err := fs.appendLocked(meta); err != nil {
			return false, fmt.Errorf("fileindex/convert: folder %q uid %d: %w", fs.folder, meta.UID, err)
		}
	}
	// Theirs can be higher than the appends leave it: an appended-then-expunged
	// message moved their counter and left nothing behind, and reusing that
	// number reissues a uid a client has already seen under different mail.
	if hdr.NextUID > fs.file.Header.NextUID {
		fs.file.Header.NextUID = hdr.NextUID
	}
	// Ours durable before theirs is unlinked, so a crash between the two leaves
	// a folder one of the two servers can still open. Both halves are needed:
	// the file's bytes before the rename, the directory entry before the
	// unlink -- a flush is not durable by default anywhere else.
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

	// The store's conversion ends when its last folder does: their map outlives
	// any folder still reading through it (#1569). Logged, not returned -- the
	// folder in hand is converted and readable regardless.
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

// carryForeignSubscriptions copies their subscribed folder list into ours,
// theirs beside the mail and ours in the control root -- via
// mailbox.ControlRoot, the same call sessions and the admin API make (#1579).
func (u *userIndex) carryForeignSubscriptions() error {
	names, err := subs.ReadForeign(filepath.Join(u.mailRootDir(), "subscriptions"), u.separator)
	if err != nil {
		return fmt.Errorf("fileindex/convert: subscriptions: %w", err)
	}
	// The personal namespace's file: a foreign store has one subscription file
	// and per-namespace siblings have no counterpart there.
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
	// The mail tree as well as the index roots: with INDEX= set only one of
	// them is in foreignRoots, and folders are named in both.
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

// convertForeignSdboxFolder is the mdbox conversion for a store that keeps one
// message per file: their folder index becomes ours, the mail is already where
// our driver looks (#1592). Nothing store-wide -- no map, no correspondence, no
// guids to stamp. Same critical section as the mdbox path: ours written and
// fsynced, then theirs unlinked, under the folder lock the caller holds.
func (u *userIndex) convertForeignSdboxFolder(fs *folderState, dir string) (bool, error) {
	// Their index and their mail are two directories whenever their INDEX= is
	// set; reading file names from the index directory finds none and converts
	// nothing.
	mailDir := filepath.Join(u.mailRootDir(),
		mailbox.FolderSubpathEscaped(u.driver, fs.folder, fs.folder, u.separator, u.escapeChar))

	// Two directories, not four: ours to write, theirs to unlink. Neither
	// storage directory exists in this shape.
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
	// Recorded before their index is unlinked below, or it exists nowhere and a
	// crash between the two loses the identity adoption exists to keep (#1611).
	u.rememberIdentity(fs.folder, hdr.UIDValidity)
	// Their sdbox index carries no guid, and marking a fresh index complete
	// would mean nothing ever fills them; marked pending, the backfill on
	// every select stamps them from the message files instead (#1592).
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
// deployment's name encoding, before anything lists them -- running it only
// from a folder conversion left the first listing after a takeover serving
// their encoding, with subscribed folders coming back \NonExistent (#1609).
//
// Under the per-user index lock: two connections on one login is ordinary, and
// this renames directories in two trees with no folder lock held.
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

// freshUIDValidity is the value a folder with no past is created with. The
// caller's number is a wall-clock stamp that repeats -- two folders in one
// second, or a folder recreated in its own delete's second, which RFC 3501
// §6.3.4 forbids -- so the allocator takes it as a floor and never repeats
// (#1614). A failure falls back to the caller's stamp rather than refusing.
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

// identityFor is the UIDVALIDITY a folder is opened with when it has no index:
// a new folder and one whose index was lost arrive here as one case, and must
// not get the same answer -- a folder the record knows keeps its identity, or
// every client resynchronises (#1611). Recorded on the way out for the next
// loss to read; an unwritable record is logged and not fatal.
func (u *userIndex) identityFor(folder string, requested uint32) uint32 {
	if u.folders != nil {
		if v, ok, err := u.folders.UIDValidity(folder); err != nil {
			slog.Warn("fileindex: folder identity record unreadable", "user", u.username, "folder", folder, "err", err)
		} else if ok {
			slog.Info("fileindex: folder index was missing; its identity came from the record",
				"user", u.username, "folder", folder, "uidvalidity", v)
			return v
		}
	}
	v := u.freshUIDValidity(requested)
	u.rememberIdentity(folder, v)
	return v
}

// rememberIdentity stores what a folder was created with.
func (u *userIndex) rememberIdentity(folder string, uidValidity uint32) {
	if u.folders == nil {
		return
	}
	if err := u.folders.Record(folder, uidValidity, time.Now()); err != nil {
		slog.Warn("fileindex: folder identity not recorded", "user", u.username, "folder", folder, "err", err)
	}
}

// newFolderUIDValidity is always fresh: a create must not inherit an identity
// even under a known name, since that folder was deleted and made again and
// RFC 3501 §6.3.4 requires it to look new.
func (u *userIndex) newFolderUIDValidity(folder string, requested uint32) uint32 {
	v := u.freshUIDValidity(requested)
	u.rememberIdentity(folder, v)
	return v
}

// refuseIfIndexLost stops an mdbox folder the identity record knows from being
// served as a new empty one when our index is gone. Undetectable before the
// record existed: an mdbox folder directory holds no message files, so one
// without an index is byte-for-byte a folder just created (#1608, #1611).
//
// Refused, not repaired: mdbox's scan is storage-wide and files a message by
// where it was FIRST saved, so it cannot refile a moved one. Only the
// storage-wide rebuild does, and it requires quiescence -- run live, a
// delivery that reached storage but has not yet appended to its folder counts
// as referenced by nobody, its refcount goes to zero, and the next purge
// reclaims live mail. Firing it from a folder open is exactly that race.
//
// A folder older than the record has no entry and opens as before -- the
// honest edge of what the record can prove; refusing on a weaker signal would
// refuse folders that really are new.
func (u *userIndex) refuseIfIndexLost(fs *folderState) error {
	if u.driver != "mdbox" || u.folders == nil {
		return nil
	}
	v, known, err := u.folders.UIDValidity(fs.folder)
	if err != nil || !known {
		return nil
	}
	return fmt.Errorf(
		"fileindex/openfolder: folder %q exists at %s with uidvalidity %d and its index is gone; "+
			"its messages are in the shared storage and only a storage-wide rebuild can file them back -- "+
			"run it with this user's mailboxes stopped, since a rebuild under live delivery can drop live mail: %w",
		fs.folder, fs.indexDir, v, mailbox.ErrIndexLost)
}
