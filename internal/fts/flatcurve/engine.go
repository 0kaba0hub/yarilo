//go:build flatcurve

package flatcurve

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/0kaba0hub/go-xapian"

	"github.com/0kaba0hub/yarilo/pkg/fts"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// On-disk format constants (see docs/FTS.md for the format specification).
const (
	Label         = "fts-flatcurve"
	dbPrefix      = "index."
	currentPrefix = "current."
	versionKey    = "yarilo.fts-flatcurve"
	versionValue  = "1"

	allHdrPrefix = "A"
	boolPrefix   = "B"
	hdrPrefix    = "H"

	// maxTermBytes: the glass backend caps terms at ~245 bytes; the format
	// truncates at 200.
	maxTermBytes = 200

	// checkpointFile is a yarilo sidecar (not part of the flatcurve format):
	// the per-mailbox last_indexed_uid + settings checksum. A missing file
	// on a migrated index falls back to Xapian's get_lastdocid.
	checkpointFile = "yarilo.checkpoint"
)

// indexedHeaders are the header fields that get their own H<NAME> terms.
var indexedHeaders = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true, "subject": true,
}

// Options are the fts_flatcurve_* tunables with upstream defaults.
type Options struct {
	CommitLimit     int           // fts_flatcurve_commit_limit (500)
	MinTermSize     int           // fts_flatcurve_min_term_size (2)
	OptimizeLimit   int           // fts_flatcurve_optimize_limit (10)
	RotateCount     uint32        // fts_flatcurve_rotate_count (5000)
	RotateTime      time.Duration // fts_flatcurve_rotate_time (5000ms)
	SubstringSearch bool          // fts_flatcurve_substring_search (no)

	// MailboxDir resolves a mailbox's fts-flatcurve directory. The service
	// wires this to the real index-path resolver; nil uses
	// <IndexRoot>/<folder name>/fts-flatcurve.
	MailboxDir func(user fts.UserRef, mbox fts.MailboxRef) string
}

func (o Options) withDefaults() Options {
	if o.CommitLimit <= 0 {
		o.CommitLimit = 500
	}
	if o.MinTermSize <= 0 {
		o.MinTermSize = 2
	}
	if o.OptimizeLimit <= 0 {
		o.OptimizeLimit = 10
	}
	if o.RotateCount == 0 {
		o.RotateCount = 5000
	}
	if o.RotateTime <= 0 {
		o.RotateTime = 5000 * time.Millisecond
	}
	if o.MailboxDir == nil {
		// Co-locate the fts-flatcurve directory inside the mailbox's own
		// per-folder index directory (the same driver-aware layout the
		// fileindex and ACL store share via mailbox.FolderSubpath), then append
		// the label — e.g. mdbox INBOX → <root>/mailboxes/INBOX/dbox-Mails/
		// fts-flatcurve. Matches where the real index data lives instead of a
		// flat <root>/<folder>/fts-flatcurve path (#654).
		o.MailboxDir = func(user fts.UserRef, mbox fts.MailboxRef) string {
			sub := mailbox.FolderSubpath(user.Driver, mbox.Name, mbox.Name,
				mailbox.SepOrDefault(user.Separator))
			return filepath.Join(user.IndexRoot, sub, Label)
		}
	}
	return o
}

// legacyMailboxDir is the pre-#654 flat layout (<root>/<folder>/fts-flatcurve).
// Used only to migrate an existing index to the driver-aware path.
func legacyMailboxDir(user fts.UserRef, mbox fts.MailboxRef) string {
	return filepath.Join(user.IndexRoot, mbox.Name, Label)
}

// Engine implements fts.Engine over Xapian.
type Engine struct {
	opts Options
}

// New returns a flatcurve engine.
func New(opts Options) *Engine {
	return &Engine{opts: opts.withDefaults()}
}

func (e *Engine) Name() string { return "flatcurve" }

func (e *Engine) Caps() fts.Caps {
	return fts.Caps{
		Tokenized: true,
		Scoring:   true,
		Substring: e.opts.SubstringSearch,
	}
}

func (e *Engine) Close() error { return nil }

func (e *Engine) OpenUser(_ context.Context, user fts.UserRef) (fts.UserIndex, error) {
	return &userIndex{eng: e, user: user, boxes: map[string]*mboxState{}}, nil
}

// mboxState holds the open write shard for one mailbox. The service is the
// sole writer (docs/FTS.md §4), so a plain mutex per user index suffices.
type mboxState struct {
	dir     string
	cur     *xapian.WDB
	curPath string
	pending int    // uncommitted document updates
	curDocs uint32 // documents written to the current shard
}

type userIndex struct {
	eng   *Engine
	user  fts.UserRef
	mu    sync.Mutex
	boxes map[string]*mboxState
}

func (u *userIndex) state(mbox fts.MailboxRef) *mboxState {
	dir := u.eng.opts.MailboxDir(u.user, mbox)
	st, ok := u.boxes[dir]
	if !ok {
		u.migrateLegacyDir(mbox, dir)
		st = &mboxState{dir: dir}
		u.boxes[dir] = st
	}
	return st
}

// migrateLegacyDir moves an existing FTS index from the pre-#654 flat path to
// the driver-aware dir, so switching the resolver relocates the index in place
// instead of orphaning it and forcing a full reindex. Best-effort: on any
// failure a fresh index is built at newDir (self-heals via autoindex). The
// yarilo-fts service is the sole writer, so no cross-process race here. Caller
// holds u.mu.
func (u *userIndex) migrateLegacyDir(mbox fts.MailboxRef, newDir string) {
	legacy := legacyMailboxDir(u.user, mbox)
	if legacy == newDir {
		return // resolver already yields the flat path (or a custom override)
	}
	if _, err := os.Stat(newDir); err == nil {
		return // target already present — nothing to migrate
	}
	if _, err := os.Stat(legacy); err != nil {
		return // no legacy index — fresh mailbox
	}
	slog.Debug("fts/flatcurve: legacy dir migration starting", "from", legacy, "to", newDir)
	if err := os.MkdirAll(filepath.Dir(newDir), 0o700); err != nil {
		slog.Warn("fts/flatcurve: legacy dir migration: mkdir parent",
			"from", legacy, "to", newDir, "err", err)
		return
	}
	if err := os.Rename(legacy, newDir); err != nil {
		slog.Warn("fts/flatcurve: legacy dir migration failed; will reindex fresh",
			"from", legacy, "to", newDir, "err", err)
		return
	}
	slog.Info("fts/flatcurve: migrated legacy FTS dir to driver-aware path (#654)",
		"from", legacy, "to", newDir)
}

func shardPaths(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fts/flatcurve: readdir: %w", err)
	}
	var out []string
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, dbPrefix) || strings.HasPrefix(name, currentPrefix) {
			out = append(out, filepath.Join(dir, name))
		}
	}
	sort.Strings(out)
	return out, nil
}

func (st *mboxState) ensureCurrent() error {
	if st.cur != nil {
		return nil
	}
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		return fmt.Errorf("fts/flatcurve: mkdir: %w", err)
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return err
	}
	var curPath string
	for _, p := range paths {
		if strings.HasPrefix(filepath.Base(p), currentPrefix) {
			curPath = p // sorted: keep the highest suffix
		}
	}
	fresh := curPath == ""
	if fresh {
		curPath = filepath.Join(st.dir,
			fmt.Sprintf("%s%d", currentPrefix, time.Now().UnixMicro()))
	}
	// Create the shard directory ourselves and fsync its parent BEFORE handing the
	// path to Xapian. Xapian opens the glass DB with DB_NO_SYNC (no directory
	// fsync), so relying on it to create current.<ts> leaves the directory entry
	// unflushed — a rotate/rename or restart can then race a write into a
	// not-yet-durable directory, surfacing as "Couldn't write new rev file:
	// .../current.<ts>/v.tmp (No such file or directory)" and permanently
	// wedging the shard (#629). Making + fsyncing the directory first removes that
	// window.
	if err := os.MkdirAll(curPath, 0o700); err != nil {
		return fmt.Errorf("fts/flatcurve: mkdir current shard: %w", err)
	}
	if err := syncDir(st.dir); err != nil {
		return fmt.Errorf("fts/flatcurve: fsync shard parent: %w", err)
	}
	slog.Debug("fts/flatcurve: ensureCurrent opening shard", "dir", st.dir, "cur_path", curPath, "fresh", fresh)
	w, err := xapian.OpenWDB(curPath)
	if err != nil {
		slog.Warn("fts/flatcurve: ensureCurrent open failed", "dir", st.dir, "cur_path", curPath, "fresh", fresh, "err", err)
		return err
	}
	if fresh {
		if err := w.SetMetadata(versionKey, versionValue); err != nil {
			w.Close()
			return err
		}
	}
	st.cur = w
	st.curPath = curPath
	n, err := w.DocCount()
	if err != nil {
		w.Close()
		st.cur = nil
		return err
	}
	st.curDocs = n
	return nil
}

func (st *mboxState) commitCurrent() error {
	if st.cur == nil || st.pending == 0 {
		return nil
	}
	if err := st.cur.Commit(); err != nil {
		slog.Warn("fts/flatcurve: commitCurrent failed, discarding handle", "dir", st.dir, "cur_path", st.curPath, "pending", st.pending, "err", err)
		st.discardCurrent() // reopen on the next pass rather than keep a dead handle (#629)
		return err
	}
	st.pending = 0
	return nil
}

// rotate seals the current shard: current.### becomes index.### and the next
// write opens a fresh current shard.
func (st *mboxState) rotate() error {
	if st.cur == nil {
		return nil
	}
	if err := st.cur.Commit(); err != nil {
		slog.Warn("fts/flatcurve: rotate commit failed, discarding handle", "dir", st.dir, "cur_path", st.curPath, "err", err)
		st.discardCurrent() // reopen on the next pass (#629)
		return err
	}
	st.cur.Close()
	st.cur = nil
	st.pending = 0
	st.curDocs = 0
	sealed := filepath.Join(st.dir,
		fmt.Sprintf("%s%d", dbPrefix, time.Now().UnixMicro()))
	slog.Debug("fts/flatcurve: rotating shard", "dir", st.dir, "from", st.curPath, "to", sealed)
	if err := os.Rename(st.curPath, sealed); err != nil {
		slog.Warn("fts/flatcurve: rotate rename failed", "dir", st.dir, "from", st.curPath, "to", sealed, "err", err)
		return fmt.Errorf("fts/flatcurve: rotate: %w", err)
	}
	// Make the rename durable before the next ensureCurrent creates a new
	// current.<ts>: otherwise a restart could leave both the old (unrenamed) and
	// new shard directory entries unflushed, the state that wedges #629.
	if err := syncDir(st.dir); err != nil {
		return fmt.Errorf("fts/flatcurve: fsync after rotate: %w", err)
	}
	st.curPath = ""
	return nil
}

// syncDir fsyncs a directory so its entries (a freshly created or renamed shard)
// are durable — needed because the glass DB is opened with DB_NO_SYNC.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// discardCurrent force-releases the write shard WITHOUT committing. Called on any
// engine error so the next ensureCurrent reopens a fresh handle instead of
// returning the same poisoned one forever — the DatabaseClosedError "sticky
// handle" of #629. Uncommitted docs are dropped, but the caller returns the error
// so the ftsservice checkpoint does not advance and those UIDs are re-indexed on
// the next pass. Best-effort: close errors are ignored (the handle is dead anyway).
func (st *mboxState) discardCurrent() {
	if st.cur != nil {
		st.cur.Close()
		st.cur = nil
	}
	st.pending = 0
	st.curDocs = 0
	st.curPath = ""
}

// closeCurrent commits and releases the write shard so other opens (expunge
// across shards, rescan, optimize, external readers) see a settled state.
func (st *mboxState) closeCurrent() error {
	if st.cur == nil {
		return nil
	}
	err := st.cur.Commit()
	st.cur.Close()
	st.cur = nil
	st.pending = 0
	st.curDocs = 0
	st.curPath = ""
	return err
}

/* --- checkpoints ---------------------------------------------------------- */

// Checkpoint returns the persisted (last_indexed_uid, uidvalidity, settings
// checksum). The on-disk file is v2 ("2 <uidvalidity> <last_uid> <checksum>");
// a legacy v1 file ("1 <last_uid> <checksum>") reads uidvalidity back as 0 so the
// caller treats it as "unknown" and lets a UIDVALIDITY mismatch reset it (#638).
func (u *userIndex) Checkpoint(mbox fts.MailboxRef) (lastUID, uidValidity, sum uint32, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state(mbox)
	if data, rerr := os.ReadFile(filepath.Join(st.dir, checkpointFile)); rerr == nil {
		var version uint32
		if _, e := fmt.Sscanf(string(data), "%d", &version); e == nil {
			switch version {
			case 2:
				if _, e2 := fmt.Sscanf(string(data), "%d %d %d %d", &version, &uidValidity, &lastUID, &sum); e2 == nil {
					return lastUID, uidValidity, sum, nil
				}
			case 1:
				if _, e2 := fmt.Sscanf(string(data), "%d %d %d", &version, &lastUID, &sum); e2 == nil {
					return lastUID, 0, sum, nil
				}
			}
		}
	}
	// No yarilo checkpoint: a migrated index still knows its highest docid
	// (== UID). Checksum + uidvalidity 0 force a rebuild decision upstream.
	paths, perr := shardPaths(st.dir)
	if perr != nil || len(paths) == 0 {
		return 0, 0, 0, nil
	}
	if cerr := st.commitCurrent(); cerr != nil {
		return 0, 0, 0, cerr
	}
	db, derr := xapian.OpenDBMulti(paths)
	if derr != nil {
		return 0, 0, 0, derr
	}
	defer db.Close()
	last, lerr := db.LastDocID()
	if lerr != nil {
		return 0, 0, 0, lerr
	}
	return last, 0, 0, nil
}

func (u *userIndex) SetCheckpoint(mbox fts.MailboxRef, lastUID, uidValidity, sum uint32) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state(mbox)
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		return fmt.Errorf("fts/flatcurve: mkdir: %w", err)
	}
	tmp := filepath.Join(st.dir, checkpointFile+".tmp")
	body := fmt.Sprintf("2 %d %d %d\n", uidValidity, lastUID, sum)
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return fmt.Errorf("fts/flatcurve: checkpoint write: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(st.dir, checkpointFile)); err != nil {
		return fmt.Errorf("fts/flatcurve: checkpoint rename: %w", err)
	}
	return nil
}

/* --- indexing --------------------------------------------------------------- */

func (u *userIndex) BeginUpdate(mbox fts.MailboxRef) (fts.Update, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state(mbox)
	return &update{ui: u, st: st}, nil
}

type update struct {
	ui  *userIndex
	st  *mboxState
	uid uint32
	doc *xapian.Doc
	key fts.BuildKey
	// seenBool dedups header-existence terms within one document.
	seenBool map[string]bool
}

func (up *update) SetBuildKey(k fts.BuildKey) (bool, error) {
	if k.Type == fts.KeyBodyPartBinary {
		return false, nil
	}
	up.ui.mu.Lock()
	defer up.ui.mu.Unlock()
	if up.doc != nil && k.UID != up.uid {
		if err := up.flushDocLocked(); err != nil {
			return false, err
		}
	}
	if up.doc == nil {
		up.doc = xapian.NewDoc()
		up.seenBool = map[string]bool{}
		up.uid = k.UID
	}
	up.key = k
	if k.Type == fts.KeyHeader || k.Type == fts.KeyMIMEHeader {
		name := strings.ToLower(k.HdrName)
		if !up.seenBool[name] {
			up.seenBool[name] = true
			if err := up.doc.AddBooleanTerm(boolPrefix + name); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// normTerm applies the upstream term normalization: minimum length, the
// 200-byte cap (multibyte-safe) and the lowercased first ASCII character
// (Xapian treats a leading capital as a term prefix).
func normTerm(tok string, minSize int) string {
	if len(tok) < minSize {
		return ""
	}
	if len(tok) > maxTermBytes {
		b := []byte(tok[:maxTermBytes])
		for len(b) > 0 {
			r, size := utf8.DecodeLastRune(b)
			if r != utf8.RuneError || size != 1 {
				break
			}
			b = b[:len(b)-1]
		}
		tok = string(b)
	}
	if len(tok) > 0 && tok[0] >= 'A' && tok[0] <= 'Z' {
		tok = string(tok[0]+('a'-'A')) + tok[1:]
	}
	return tok
}

// addWithSuffixes adds prefix+term and, in substring mode, every suffix of
// the term not shorter than minSize (the upstream substring_search loop).
func (up *update) addWithSuffixes(prefix, term string, substring bool, minSize int) error {
	s := term
	for {
		if err := up.doc.AddTerm(prefix + s); err != nil {
			return err
		}
		if !substring {
			return nil
		}
		_, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if len(s) < minSize {
			return nil
		}
	}
}

func (up *update) BuildMore(data []byte) error {
	up.ui.mu.Lock()
	defer up.ui.mu.Unlock()
	if up.doc == nil {
		return fmt.Errorf("fts/flatcurve: BuildMore without a build key")
	}
	opts := up.ui.eng.opts
	term := normTerm(string(data), opts.MinTermSize)
	if term == "" {
		return nil
	}
	switch up.key.Type {
	case fts.KeyHeader, fts.KeyMIMEHeader:
		if err := up.addWithSuffixes(allHdrPrefix, term, opts.SubstringSearch, opts.MinTermSize); err != nil {
			return err
		}
		name := strings.ToLower(up.key.HdrName)
		if indexedHeaders[name] {
			p := hdrPrefix + strings.ToUpper(name)
			return up.addWithSuffixes(p, term, opts.SubstringSearch, opts.MinTermSize)
		}
		return nil
	default: // body
		return up.addWithSuffixes("", term, opts.SubstringSearch, opts.MinTermSize)
	}
}

func (up *update) flushDocLocked() error {
	if up.doc == nil {
		return nil
	}
	st := up.st
	if err := st.ensureCurrent(); err != nil {
		return err
	}
	if err := st.cur.ReplaceDocument(up.uid, up.doc); err != nil {
		st.discardCurrent() // poisoned shard → reopen on the next pass (#629)
		return err
	}
	up.doc.Free()
	up.doc = nil
	up.seenBool = nil
	st.pending++
	st.curDocs++
	opts := up.ui.eng.opts
	if st.pending >= opts.CommitLimit {
		if err := st.commitCurrent(); err != nil {
			st.discardCurrent()
			return err
		}
	}
	if st.curDocs >= opts.RotateCount {
		if err := st.rotate(); err != nil {
			st.discardCurrent()
			return err
		}
	}
	return nil
}

func (up *update) Commit() error {
	up.ui.mu.Lock()
	defer up.ui.mu.Unlock()
	if err := up.flushDocLocked(); err != nil {
		return err
	}
	return up.st.commitCurrent()
}

func (up *update) Rollback() error {
	up.ui.mu.Lock()
	defer up.ui.mu.Unlock()
	// Already-flushed documents stay (rescan reconciles); only the pending
	// document is discarded — the upstream failure semantics.
	if up.doc != nil {
		up.doc.Free()
		up.doc = nil
	}
	return nil
}

/* --- expunge / rescan / optimize -------------------------------------------- */

func (u *userIndex) Expunge(mbox fts.MailboxRef, uid uint32) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state(mbox)
	// The open write shard is checked in place; sealed shards are opened
	// one by one (the upstream per-shard probe).
	if st.cur != nil {
		existed, err := st.cur.DeleteDocument(uid)
		if err != nil {
			return err
		}
		if existed {
			st.pending++
			return st.commitCurrent()
		}
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if p == st.curPath && st.cur != nil {
			continue
		}
		w, err := xapian.OpenWDB(p)
		if err != nil {
			return err
		}
		existed, derr := w.DeleteDocument(uid)
		if derr == nil && existed {
			derr = w.Commit()
		}
		w.Close()
		if derr != nil {
			return derr
		}
		if existed {
			return nil
		}
	}
	return nil
}

func (u *userIndex) Rescan(mbox fts.MailboxRef, present []uint32) ([]uint32, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state(mbox)
	slog.Debug("fts/flatcurve: rescan closing current shard", "dir", st.dir, "cur_path", st.curPath)
	if err := st.closeCurrent(); err != nil {
		return nil, err
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return nil, err
	}
	presentSet := make(map[uint32]bool, len(present))
	for _, uid := range present {
		presentSet[uid] = true
	}
	if len(paths) == 0 {
		missing := append([]uint32(nil), present...)
		sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
		return missing, nil
	}
	db, err := xapian.OpenDBMulti(paths)
	if err != nil {
		return nil, err
	}
	indexed, err := db.DocIDs()
	db.Close()
	if err != nil {
		return nil, err
	}
	indexedSet := make(map[uint32]bool, len(indexed))
	var stale []uint32
	for _, uid := range indexed {
		indexedSet[uid] = true
		if !presentSet[uid] {
			stale = append(stale, uid)
		}
	}
	// Targeted deletes, shard by shard — no delete-above-lowest-gap storm.
	if len(stale) > 0 {
		for _, p := range paths {
			w, werr := xapian.OpenWDB(p)
			if werr != nil {
				return nil, werr
			}
			changed := false
			for _, uid := range stale {
				existed, derr := w.DeleteDocument(uid)
				if derr != nil {
					w.Close()
					return nil, derr
				}
				changed = changed || existed
			}
			if changed {
				if cerr := w.Commit(); cerr != nil {
					w.Close()
					return nil, cerr
				}
			}
			w.Close()
		}
	}
	var missing []uint32
	for _, uid := range present {
		if !indexedSet[uid] {
			missing = append(missing, uid)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	return missing, nil
}

func (u *userIndex) Optimize() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, st := range u.boxes {
		if err := optimizeDir(st); err != nil {
			return err
		}
	}
	return nil
}

func optimizeDir(st *mboxState) error {
	slog.Debug("fts/flatcurve: optimizeDir start", "dir", st.dir, "cur_path_before_close", st.curPath)
	if err := st.closeCurrent(); err != nil {
		return err
	}
	paths, err := shardPaths(st.dir)
	if err != nil || len(paths) < 2 {
		slog.Debug("fts/flatcurve: optimizeDir skip (fewer than 2 shards)", "dir", st.dir, "paths", paths, "err", err)
		return err
	}
	slog.Debug("fts/flatcurve: optimizeDir merging shards", "dir", st.dir, "paths", paths)
	db, err := xapian.OpenDBMulti(paths)
	if err != nil {
		return err
	}
	tmp := filepath.Join(st.dir, "optimize")
	_ = os.RemoveAll(tmp)
	cerr := db.Compact(tmp)
	db.Close()
	if cerr != nil {
		_ = os.RemoveAll(tmp)
		return cerr
	}
	for _, p := range paths {
		slog.Debug("fts/flatcurve: optimizeDir removing merged shard", "dir", st.dir, "path", p)
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("fts/flatcurve: optimize cleanup: %w", err)
		}
	}
	sealed := filepath.Join(st.dir,
		fmt.Sprintf("%s%d", dbPrefix, time.Now().UnixMicro()))
	slog.Debug("fts/flatcurve: optimizeDir sealing", "dir", st.dir, "from", tmp, "to", sealed)
	if err := os.Rename(tmp, sealed); err != nil {
		return fmt.Errorf("fts/flatcurve: optimize rename: %w", err)
	}
	return nil
}

func (u *userIndex) Refresh() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, st := range u.boxes {
		if err := st.commitCurrent(); err != nil {
			return err
		}
	}
	return nil
}

func (u *userIndex) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	var firstErr error
	for _, st := range u.boxes {
		if err := st.closeCurrent(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	u.boxes = map[string]*mboxState{}
	return firstErr
}

/* --- lookup -------------------------------------------------------------------- */

func (u *userIndex) Lookup(mbox fts.MailboxRef, q fts.Query) (fts.Result, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state(mbox)
	if err := st.commitCurrent(); err != nil {
		return fts.Result{}, err
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return fts.Result{}, err
	}
	if len(paths) == 0 {
		return fts.Result{}, nil
	}
	db, err := xapian.OpenDBMulti(paths)
	if err != nil {
		return fts.Result{}, err
	}
	defer db.Close()

	xq, maybe, err := u.buildQuery(q)
	if err != nil {
		return fts.Result{}, err
	}
	defer xq.Free()
	entries, err := db.Search(xq)
	if err != nil {
		return fts.Result{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].DocID < entries[j].DocID })
	res := fts.Result{}
	for _, ent := range entries {
		if maybe {
			res.Maybe = append(res.Maybe, ent.DocID)
		} else {
			res.Definite = append(res.Definite, ent.DocID)
		}
		res.Scores = append(res.Scores, fts.Score{UID: ent.DocID, Value: ent.Weight})
	}
	return res, nil
}

func (u *userIndex) buildQuery(q fts.Query) (*xapian.Query, bool, error) {
	if len(q.Terms) == 0 {
		return nil, false, fmt.Errorf("fts/flatcurve: empty query")
	}
	var acc *xapian.Query
	maybe := false
	op := xapian.OpOR
	if q.AndTerms {
		op = xapian.OpAND
	}
	for _, term := range q.Terms {
		tq, tMaybe, err := u.buildTerm(term)
		if err != nil {
			acc.Free()
			return nil, false, err
		}
		// One over-approximated arg makes the whole conjunction a maybe.
		maybe = maybe || tMaybe
		if term.Not {
			all, aerr := xapian.QueryMatchAll()
			if aerr != nil {
				tq.Free()
				acc.Free()
				return nil, false, aerr
			}
			if tq, err = xapian.QueryCombine(xapian.OpANDNOT, all, tq); err != nil {
				acc.Free()
				return nil, false, err
			}
		}
		if acc == nil {
			acc = tq
		} else if acc, err = xapian.QueryCombine(op, acc, tq); err != nil {
			return nil, false, err
		}
	}
	return acc, maybe, nil
}

func (u *userIndex) buildTerm(t fts.Term) (*xapian.Query, bool, error) {
	name := strings.ToLower(t.HdrName)
	if len(t.Words) == 0 {
		if t.Field != fts.FieldHeader {
			return nil, false, fmt.Errorf("fts/flatcurve: empty term")
		}
		// HEADER existence probe → the boolean term.
		q, err := xapian.QueryTerm(boolPrefix + name)
		return q, false, err
	}
	minSize := u.eng.opts.MinTermSize
	var acc *xapian.Query
	maybe := false
	for _, w := range t.Words {
		var wq *xapian.Query
		for _, v := range w.Variants {
			v = normTerm(v, 1)
			if v == "" {
				continue
			}
			vq, vMaybe, err := buildVariant(t.Field, name, v, minSize)
			if err != nil {
				wq.Free()
				acc.Free()
				return nil, false, err
			}
			maybe = maybe || vMaybe
			if wq == nil {
				wq = vq
			} else if wq, err = xapian.QueryCombine(xapian.OpOR, wq, vq); err != nil {
				acc.Free()
				return nil, false, err
			}
		}
		if wq == nil {
			continue
		}
		var err error
		if acc == nil {
			acc = wq
		} else if acc, err = xapian.QueryCombine(xapian.OpAND, acc, wq); err != nil {
			return nil, false, err
		}
	}
	if acc == nil {
		return nil, false, fmt.Errorf("fts/flatcurve: no usable variants in term")
	}
	return acc, maybe, nil
}

func buildVariant(field fts.FieldKind, hdrName, v string, minSize int) (*xapian.Query, bool, error) {
	switch field {
	case fts.FieldBody:
		q, err := xapian.QueryWildcard(v)
		return q, false, err
	case fts.FieldText:
		hq, err := xapian.QueryWildcard(allHdrPrefix + v)
		if err != nil {
			return nil, false, err
		}
		bq, err := xapian.QueryWildcard(v)
		if err != nil {
			hq.Free()
			return nil, false, err
		}
		q, err := xapian.QueryCombine(xapian.OpOR, hq, bq)
		return q, false, err
	case fts.FieldHeader:
		if indexedHeaders[hdrName] {
			q, err := xapian.QueryWildcard(hdrPrefix + strings.ToUpper(hdrName) + v)
			return q, false, err
		}
		// Non-indexed header: only the pooled A prefix knows the term —
		// an over-approximation the caller must re-verify (maybe).
		q, err := xapian.QueryWildcard(allHdrPrefix + v)
		return q, true, err
	default:
		return nil, false, fmt.Errorf("fts/flatcurve: unknown field kind %d", field)
	}
}
