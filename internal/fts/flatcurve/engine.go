//go:build flatcurve

package flatcurve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/0kaba0hub/yarilo/pkg/fts"
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
		o.MailboxDir = func(user fts.UserRef, mbox fts.MailboxRef) string {
			return filepath.Join(user.IndexRoot, mbox.Name, Label)
		}
	}
	return o
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
	cur     *xWDB
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
		st = &mboxState{dir: dir}
		u.boxes[dir] = st
	}
	return st
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
	w, err := openWDB(curPath)
	if err != nil {
		return err
	}
	if fresh {
		if err := w.setMetadata(versionKey, versionValue); err != nil {
			w.close()
			return err
		}
	}
	st.cur = w
	st.curPath = curPath
	n, err := w.docCount()
	if err != nil {
		w.close()
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
	if err := st.cur.commit(); err != nil {
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
	if err := st.cur.commit(); err != nil {
		return err
	}
	st.cur.close()
	st.cur = nil
	st.pending = 0
	st.curDocs = 0
	sealed := filepath.Join(st.dir,
		fmt.Sprintf("%s%d", dbPrefix, time.Now().UnixMicro()))
	if err := os.Rename(st.curPath, sealed); err != nil {
		return fmt.Errorf("fts/flatcurve: rotate: %w", err)
	}
	st.curPath = ""
	return nil
}

// closeCurrent commits and releases the write shard so other opens (expunge
// across shards, rescan, optimize, external readers) see a settled state.
func (st *mboxState) closeCurrent() error {
	if st.cur == nil {
		return nil
	}
	err := st.cur.commit()
	st.cur.close()
	st.cur = nil
	st.pending = 0
	st.curDocs = 0
	st.curPath = ""
	return err
}

/* --- checkpoints ---------------------------------------------------------- */

func (u *userIndex) Checkpoint(mbox fts.MailboxRef) (uint32, uint32, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state(mbox)
	data, err := os.ReadFile(filepath.Join(st.dir, checkpointFile))
	if err == nil {
		var version, lastUID, sum uint32
		if _, serr := fmt.Sscanf(string(data), "%d %d %d", &version, &lastUID, &sum); serr == nil && version == 1 {
			return lastUID, sum, nil
		}
	}
	// No yarilo checkpoint: a migrated index still knows its highest
	// docid (== UID). Settings checksum 0 forces a rebuild decision upstream.
	paths, err := shardPaths(st.dir)
	if err != nil || len(paths) == 0 {
		return 0, 0, nil
	}
	if err := st.commitCurrent(); err != nil {
		return 0, 0, err
	}
	db, err := openDBMulti(paths)
	if err != nil {
		return 0, 0, err
	}
	defer db.close()
	last, err := db.lastDocID()
	if err != nil {
		return 0, 0, err
	}
	return last, 0, nil
}

func (u *userIndex) SetCheckpoint(mbox fts.MailboxRef, lastUID, sum uint32) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state(mbox)
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		return fmt.Errorf("fts/flatcurve: mkdir: %w", err)
	}
	tmp := filepath.Join(st.dir, checkpointFile+".tmp")
	body := fmt.Sprintf("1 %d %d\n", lastUID, sum)
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
	doc *xDoc
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
		up.doc = newDoc()
		up.seenBool = map[string]bool{}
		up.uid = k.UID
	}
	up.key = k
	if k.Type == fts.KeyHeader || k.Type == fts.KeyMIMEHeader {
		name := strings.ToLower(k.HdrName)
		if !up.seenBool[name] {
			up.seenBool[name] = true
			if err := up.doc.addBooleanTerm(boolPrefix + name); err != nil {
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
		if err := up.doc.addTerm(prefix + s); err != nil {
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
	if err := st.cur.replaceDocument(up.uid, up.doc); err != nil {
		return err
	}
	up.doc.free()
	up.doc = nil
	up.seenBool = nil
	st.pending++
	st.curDocs++
	opts := up.ui.eng.opts
	if st.pending >= opts.CommitLimit {
		if err := st.commitCurrent(); err != nil {
			return err
		}
	}
	if st.curDocs >= opts.RotateCount {
		return st.rotate()
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
		up.doc.free()
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
		existed, err := st.cur.deleteDocument(uid)
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
		w, err := openWDB(p)
		if err != nil {
			return err
		}
		existed, derr := w.deleteDocument(uid)
		if derr == nil && existed {
			derr = w.commit()
		}
		w.close()
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
	db, err := openDBMulti(paths)
	if err != nil {
		return nil, err
	}
	indexed, err := db.docIDs()
	db.close()
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
			w, werr := openWDB(p)
			if werr != nil {
				return nil, werr
			}
			changed := false
			for _, uid := range stale {
				existed, derr := w.deleteDocument(uid)
				if derr != nil {
					w.close()
					return nil, derr
				}
				changed = changed || existed
			}
			if changed {
				if cerr := w.commit(); cerr != nil {
					w.close()
					return nil, cerr
				}
			}
			w.close()
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
	if err := st.closeCurrent(); err != nil {
		return err
	}
	paths, err := shardPaths(st.dir)
	if err != nil || len(paths) < 2 {
		return err
	}
	db, err := openDBMulti(paths)
	if err != nil {
		return err
	}
	tmp := filepath.Join(st.dir, "optimize")
	_ = os.RemoveAll(tmp)
	cerr := db.compact(tmp)
	db.close()
	if cerr != nil {
		_ = os.RemoveAll(tmp)
		return cerr
	}
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("fts/flatcurve: optimize cleanup: %w", err)
		}
	}
	sealed := filepath.Join(st.dir,
		fmt.Sprintf("%s%d", dbPrefix, time.Now().UnixMicro()))
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
	db, err := openDBMulti(paths)
	if err != nil {
		return fts.Result{}, err
	}
	defer db.close()

	xq, maybe, err := u.buildQuery(q)
	if err != nil {
		return fts.Result{}, err
	}
	defer xq.free()
	entries, err := db.search(xq)
	if err != nil {
		return fts.Result{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].docid < entries[j].docid })
	res := fts.Result{}
	for _, ent := range entries {
		if maybe {
			res.Maybe = append(res.Maybe, ent.docid)
		} else {
			res.Definite = append(res.Definite, ent.docid)
		}
		res.Scores = append(res.Scores, fts.Score{UID: ent.docid, Value: ent.weight})
	}
	return res, nil
}

func (u *userIndex) buildQuery(q fts.Query) (*xQuery, bool, error) {
	if len(q.Terms) == 0 {
		return nil, false, fmt.Errorf("fts/flatcurve: empty query")
	}
	var acc *xQuery
	maybe := false
	op := opOr
	if q.AndTerms {
		op = opAnd
	}
	for _, term := range q.Terms {
		tq, tMaybe, err := u.buildTerm(term)
		if err != nil {
			acc.free()
			return nil, false, err
		}
		// One over-approximated arg makes the whole conjunction a maybe.
		maybe = maybe || tMaybe
		if term.Not {
			all, aerr := queryMatchAll()
			if aerr != nil {
				tq.free()
				acc.free()
				return nil, false, aerr
			}
			if tq, err = queryCombine(opAndNot, all, tq); err != nil {
				acc.free()
				return nil, false, err
			}
		}
		if acc == nil {
			acc = tq
		} else if acc, err = queryCombine(op, acc, tq); err != nil {
			return nil, false, err
		}
	}
	return acc, maybe, nil
}

func (u *userIndex) buildTerm(t fts.Term) (*xQuery, bool, error) {
	name := strings.ToLower(t.HdrName)
	if len(t.Words) == 0 {
		if t.Field != fts.FieldHeader {
			return nil, false, fmt.Errorf("fts/flatcurve: empty term")
		}
		// HEADER existence probe → the boolean term.
		q, err := queryTerm(boolPrefix + name)
		return q, false, err
	}
	minSize := u.eng.opts.MinTermSize
	var acc *xQuery
	maybe := false
	for _, w := range t.Words {
		var wq *xQuery
		for _, v := range w.Variants {
			v = normTerm(v, 1)
			if v == "" {
				continue
			}
			vq, vMaybe, err := buildVariant(t.Field, name, v, minSize)
			if err != nil {
				wq.free()
				acc.free()
				return nil, false, err
			}
			maybe = maybe || vMaybe
			if wq == nil {
				wq = vq
			} else if wq, err = queryCombine(opOr, wq, vq); err != nil {
				acc.free()
				return nil, false, err
			}
		}
		if wq == nil {
			continue
		}
		var err error
		if acc == nil {
			acc = wq
		} else if acc, err = queryCombine(opAnd, acc, wq); err != nil {
			return nil, false, err
		}
	}
	if acc == nil {
		return nil, false, fmt.Errorf("fts/flatcurve: no usable variants in term")
	}
	return acc, maybe, nil
}

func buildVariant(field fts.FieldKind, hdrName, v string, minSize int) (*xQuery, bool, error) {
	switch field {
	case fts.FieldBody:
		q, err := queryWildcard(v)
		return q, false, err
	case fts.FieldText:
		hq, err := queryWildcard(allHdrPrefix + v)
		if err != nil {
			return nil, false, err
		}
		bq, err := queryWildcard(v)
		if err != nil {
			hq.free()
			return nil, false, err
		}
		q, err := queryCombine(opOr, hq, bq)
		return q, false, err
	case fts.FieldHeader:
		if indexedHeaders[hdrName] {
			q, err := queryWildcard(hdrPrefix + strings.ToUpper(hdrName) + v)
			return q, false, err
		}
		// Non-indexed header: only the pooled A prefix knows the term —
		// an over-approximation the caller must re-verify (maybe).
		q, err := queryWildcard(allHdrPrefix + v)
		return q, true, err
	default:
		return nil, false, fmt.Errorf("fts/flatcurve: unknown field kind %d", field)
	}
}
