// Package msgcache reads and writes the per-folder index cache
// (yarilo.index.cache) that lets a listing answer without opening message
// files. Shared by the protocol servers: one cache, one set of invalidation
// rules. Format and invalidation: https://doc.yarilomail.org/BACKEND-API
package msgcache

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// cachePathMu serialises cache-pair access within this process, keyed by the
// cache file path -- the in-process fast path of the two-tier rule; the
// cross-process tier is the MailboxKey lock below. Entries are never removed:
// the set of open folders per process is small and bounded.
var cachePathMu sync.Map // path -> *sync.Mutex

// shared takes the read side: without it, sharing the cross-process key buys
// nothing for two sessions in one pod (#1673).
func lockCachePath(path string, shared bool) func() {
	mu, _ := cachePathMu.LoadOrStore(path, &sync.RWMutex{})
	m, ok := mu.(*sync.RWMutex)
	if !ok {
		// Unreachable: this map only ever stores *sync.RWMutex. Failing open
		// would silently drop the in-process tier, so fail loud instead.
		panic("imap: cache mutex map holds a foreign type")
	}
	if shared {
		m.RLock()
		return m.RUnlock
	}
	m.Lock()
	return m.Unlock
}

// cacheFieldEnvelope is the cache field name for the encoded envelope.
const cacheFieldEnvelope = "yarilo.envelope"

// cacheFieldReferences holds the References header, the one threading field
// ENVELOPE does not carry (RFC 3501 has Message-ID and In-Reply-To and stops
// there). Without it THREAD opens every matched message to read a header the
// cache already holds the rest of (#1461).
//
// A separate field rather than a wider envelope encoding: the envelope's bytes
// are read by every FETCH, and a threading header nobody fetches has no place
// in them. Fields are looked up by name, so a file written before this one
// existed simply does not carry it, and a file that does is read by older
// binaries without complaint -- the field table lives in the file itself.
const cacheFieldReferences = "yarilo.references"

// indexCacher is the slice of the file-index surface the cache reader needs.
// Asserted at use: an index backend without it simply serves no cache.
// Index is the slice of the index surface the cache needs. Asserted at use:
// a backend without it serves no cache.
type Index interface {
	CachePairIdentity(folderID uint64) (indexID, resetID uint32, ok bool, err error)
	// EnsureCacheExtension adds the extension to an index written before it
	// existed. Without it the lazy add is unreachable: only a stamping write
	// adds the extension, and stamping needs a pair that the missing
	// extension prevents opening.
	EnsureCacheExtension(folderID uint64) (indexID, resetID uint32, err error)
	// BumpCacheGeneration abandons the current generation and returns the
	// next file_seq. Discarding a file without it leaves the index's stamps
	// applying to whatever gets written at those offsets next.
	BumpCacheGeneration(folderID uint64) (uint32, error)
	CachePath(folderID uint64) (string, error)
	SetCacheOffsets(folderID uint64, offsets map[uint32]uint32) error
}

/* --- envelope codec ------------------------------------------------------- */

// envelopeCodecVersion is the first byte of every encoded envelope. A reader
// seeing an unknown version treats the value as a miss; changing the encoding
// itself requires a CacheProducerGen bump, which invalidates the whole file.
const envelopeCodecVersion = 1

func putStr(b []byte, s string) []byte {
	var l [4]byte
	binary.LittleEndian.PutUint32(l[:], uint32(len(s)))
	return append(append(b, l[:]...), s...)
}

func getStr(b []byte) (string, []byte, error) {
	if len(b) < 4 {
		return "", nil, errors.New("short length")
	}
	n := binary.LittleEndian.Uint32(b)
	b = b[4:]
	if uint32(len(b)) < n {
		return "", nil, errors.New("short string")
	}
	return string(b[:n]), b[n:], nil
}

func putAddrs(b []byte, addrs []imaplib.Address) []byte {
	var l [4]byte
	binary.LittleEndian.PutUint32(l[:], uint32(len(addrs)))
	b = append(b, l[:]...)
	for _, a := range addrs {
		b = putStr(b, a.Name)
		b = putStr(b, a.Mailbox)
		b = putStr(b, a.Host)
	}
	return b
}

func getAddrs(b []byte) ([]imaplib.Address, []byte, error) {
	if len(b) < 4 {
		return nil, nil, errors.New("short address count")
	}
	n := binary.LittleEndian.Uint32(b)
	b = b[4:]
	if n > 1<<16 {
		return nil, nil, errors.New("address count implausible")
	}
	var out []imaplib.Address
	for i := uint32(0); i < n; i++ {
		var a imaplib.Address
		var err error
		if a.Name, b, err = getStr(b); err != nil {
			return nil, nil, err
		}
		if a.Mailbox, b, err = getStr(b); err != nil {
			return nil, nil, err
		}
		if a.Host, b, err = getStr(b); err != nil {
			return nil, nil, err
		}
		out = append(out, a)
	}
	return out, b, nil
}

// encodeEnvelope serialises the parsed envelope. A nil date encodes as zero.
func encodeEnvelope(env *imaplib.Envelope) []byte {
	b := []byte{envelopeCodecVersion}
	var d [8]byte
	if !env.Date.IsZero() {
		binary.LittleEndian.PutUint64(d[:], uint64(env.Date.Unix()))
	}
	b = append(b, d[:]...)
	b = putStr(b, env.Subject)
	for _, list := range [][]imaplib.Address{env.From, env.Sender, env.ReplyTo, env.To, env.Cc, env.Bcc} {
		b = putAddrs(b, list)
	}
	var l [4]byte
	binary.LittleEndian.PutUint32(l[:], uint32(len(env.InReplyTo)))
	b = append(b, l[:]...)
	for _, s := range env.InReplyTo {
		b = putStr(b, s)
	}
	b = putStr(b, env.MessageID)
	return b
}

// decodeEnvelope is the inverse; any malformation is (nil, false) -- a cache
// miss, never an error.
func decodeEnvelope(b []byte) (*imaplib.Envelope, bool) {
	if len(b) < 9 || b[0] != envelopeCodecVersion {
		return nil, false
	}
	env := &imaplib.Envelope{}
	if unix := binary.LittleEndian.Uint64(b[1:9]); unix != 0 {
		env.Date = time.Unix(int64(unix), 0).UTC()
	}
	b = b[9:]
	var err error
	if env.Subject, b, err = getStr(b); err != nil {
		return nil, false
	}
	for _, dst := range []*[]imaplib.Address{&env.From, &env.Sender, &env.ReplyTo, &env.To, &env.Cc, &env.Bcc} {
		if *dst, b, err = getAddrs(b); err != nil {
			return nil, false
		}
	}
	if len(b) < 4 {
		return nil, false
	}
	n := binary.LittleEndian.Uint32(b)
	b = b[4:]
	if n > 1<<16 {
		return nil, false
	}
	for i := uint32(0); i < n; i++ {
		var s string
		if s, b, err = getStr(b); err != nil {
			return nil, false
		}
		env.InReplyTo = append(env.InReplyTo, s)
	}
	if env.MessageID, _, err = getStr(b); err != nil {
		return nil, false
	}
	return env, true
}

/* --- per-FETCH cache handle ----------------------------------------------- */

// folderCache serves one FETCH's worth of envelope lookups and batches the
// write-back. Opened lazily on the first envelope-needing message; nil-safe
// throughout, so every failure degrades to "parse as today".
// Handle is one request's view of a folder's cache. Opened per FETCH,
// closed after the batched stamp. nil is a valid value meaning "no cache":
// every method tolerates it, so a miss degrades to parsing.
type Handle struct {
	file   *mailindex.CacheFile
	envID  uint32
	bsID   uint32
	refsID uint32
	stamps map[uint32]uint32 // uid -> new head offset, flushed on close
	idx    Index
	fid    uint64
	// unlock releases the locks taken for the open-append-stamp window, in
	// reverse acquisition order. Two sessions of one account in one folder
	// are the ordinary case: without the lock their appends interleave under
	// two descriptors, and a stamped offset resolves inside a file the other
	// writer has since extended -- a fully valid-looking record for a
	// DIFFERENT message, which no invalidation level can see.
	unlock []func()

	// Deferred mode: what the request parsed, kept until Close can take the
	// locks again. Order matters -- two fields for one message chain, so they
	// must be appended in the order they were produced.
	deferred bool
	pending  []pendingField
	// reopen carries what the second window needs to prove it is looking at
	// the same cache generation the first one read.
	reopen struct {
		idx     mailbox.UserIndex
		ic      Index
		fid     uint64
		opts    Options
		path    string
		indexID uint32
		resetID uint32
	}
}

// pendingField is one field value a deferred handle has not written yet.
type pendingField struct {
	meta    mailbox.MessageMeta
	fieldID uint32
	data    []byte
}

// UID is the message this field belongs to.
func (p pendingField) UID() uint32 { return p.meta.UID }

// openFolderCache opens (or lazily creates) the folder's cache pair. Any
// invalidity removes the stale file and starts a fresh one -- the cache is
// derived data, absence is its recovery mode.
// Options carries what the cache needs from its caller: the lock identity
// and a trace id for logs.
// lockID: a caller that supplied none still names a holder (#1670).
func (o Options) lockID() string {
	if o.SessionID != "" {
		return o.SessionID
	}
	if o.TraceID != "" {
		return o.TraceID
	}
	return locks.NewID()
}

type Options struct {
	Locker locks.Locker
	User   string
	// SessionID names the session in the lock owner. Required: a cache write
	// holds the mailbox key, and an anonymous holder is unattributable (#1670).
	SessionID string
	Folder    string
	TraceID   string
	// DeferWrites releases the cache locks as soon as the file has been read
	// into memory, and takes them again in Close to append what the request
	// parsed.
	//
	// For a caller that writes its response while holding the handle. FETCH
	// does: it opened the pair, then read bodies from storage and wrote them
	// to a socket, all inside the locked window, so one client on a slow link
	// held both tiers -- the in-process path mutex and the cross-process
	// mailbox lock -- for as long as its transfer took. Every other session of
	// that user on that folder waited (#1545).
	DeferWrites bool

	// Shared: readers of one folder do not refuse each other. Requires
	// DeferWrites, and never for a caller that changes flags (#1673).
	Shared bool
}

// openExclusive retries a shared open that reached a step which writes. Once:
// the second pass is not shared, so it cannot come back here.
func openExclusive(idx mailbox.UserIndex, folderID uint64, opts Options) *Handle {
	opts.Shared = false
	return Open(idx, folderID, opts)
}

// Open returns a handle on the folder's cache, or nil when none can be
// served -- a wrong pair, an unopenable file, a backend without a cache. The
// caller parses as it would without one.
func Open(idx mailbox.UserIndex, folderID uint64, opts Options) *Handle {
	if opts.Shared && !opts.DeferWrites {
		// storeField writes through a live descriptor when the handle is not
		// deferred, and a shared key does not exclude the other writer (#1673).
		if flag.Lookup("test.v") != nil {
			panic("msgcache: Shared without DeferWrites writes the cache under a shared key (#1673)")
		}
		slog.Error("msgcache: Shared without DeferWrites; opening exclusively")
		opts.Shared = false
	}
	ic, ok := idx.(Index)
	if !ok {
		return nil
	}
	indexID, resetID, extOK, err := ic.CachePairIdentity(folderID)
	if err != nil {
		return nil
	}
	if !extOK {
		// Every mailbox older than the extension arrives here, which is every
		// mailbox in an upgraded deployment.
		if indexID, resetID, err = ic.EnsureCacheExtension(folderID); err != nil {
			slog.Debug("msgcache: cache extension unavailable; serving uncached", "trace_id", opts.TraceID, "err", err)
			return nil
		}
	}
	path, err := ic.CachePath(folderID)
	if err != nil {
		return nil
	}
	fc := &Handle{idx: ic, fid: folderID, stamps: make(map[uint32]uint32)}
	// The whole open-append-stamp window runs under the lock, reads
	// included: remove-and-recreate under a live descriptor and the
	// read-modify-write of the field table are only safe when nobody else
	// is inside the pair. In-process mutex first, then the cross-process
	// MailboxKey -- the same two tiers every shared write path uses.
	fc.unlock = append(fc.unlock, lockCachePath(path, opts.Shared))
	if lkr := opts.Locker; lkr != nil && opts.User != "" && opts.Folder != "" {
		ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), "msgcache"), 35*time.Second)
		key := locks.MailboxKey(opts.User, opts.Folder)
		owner := locks.Owner(opts.User, opts.lockID())
		var lk locks.Lock
		var lerr error
		if opts.Shared {
			lk, lerr = locks.AcquireShared(ctx, lkr, key, owner, 30*time.Second)
		} else {
			lk, lerr = locks.Acquire(ctx, lkr, key, owner, 30*time.Second)
		}
		cancel()
		if lerr != nil {
			slog.Debug("msgcache: cache lock unavailable; serving uncached", "trace_id", opts.TraceID, "err", lerr)
			fc.release()
			return nil
		}
		fc.unlock = append(fc.unlock, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = lkr.Unlock(ctx, lk.ID)
		})
	}
	cf, err := mailindex.OpenCache(path, indexID, resetID)
	switch {
	case err == nil:
		fc.file = cf
	case os.IsNotExist(err), errors.Is(err, mailindex.ErrCacheInvalid):
		if opts.Shared {
			// Creating or repairing the file is a write, and this handle holds
			// the key in shared mode. Start again exclusively (#1673).
			fc.release()
			return openExclusive(idx, folderID, opts)
		}
		if errors.Is(err, mailindex.ErrCacheInvalid) {
			// Garbage by definition -- but recreating under the SAME file_seq
			// would leave the index's stamps pointing into a fresh file,
			// where the first append to reach one of those offsets answers
			// its FETCH with another message's record. Enter a new
			// generation, which kills every stamp in one index write.
			_ = os.Remove(path)
			newSeq, berr := ic.BumpCacheGeneration(folderID)
			if berr != nil {
				slog.Debug("msgcache: cache generation bump failed; serving uncached", "trace_id", opts.TraceID, "err", berr)
				fc.release()
				return nil
			}
			resetID = newSeq
		}
		cf, cerr := mailindex.CreateCache(path, indexID, resetID)
		if cerr != nil {
			slog.Debug("msgcache: cache create failed; serving uncached", "trace_id", opts.TraceID, "err", cerr)
			fc.release()
			return nil
		}
		fc.file = cf
	default:
		slog.Debug("msgcache: cache open failed; serving uncached", "trace_id", opts.TraceID, "err", err)
		fc.release()
		return nil
	}
	// Both fields are registered up front: AddFields is a read-modify-write
	// of the in-file table, so doing it once per window beats doing it per
	// field, and a file that carries only one of them is a file two producer
	// versions wrote.
	for _, want := range []struct {
		name string
		dst  *uint32
	}{
		{cacheFieldEnvelope, &fc.envID},
		{cacheFieldBodyStructure, &fc.bsID},
		{cacheFieldReferences, &fc.refsID},
	} {
		id, ok := fc.file.FieldID(want.name)
		if !ok {
			if opts.Shared {
				// AddFields is a read-modify-write of the in-file table.
				fc.file.Close()
				fc.release()
				return openExclusive(idx, folderID, opts)
			}
			first, aerr := fc.file.AddFields([]mailindex.CacheField{{
				Name: want.name, Type: mailindex.CacheFieldVariableSize, Decision: mailindex.CacheDecisionYes,
			}})
			if aerr != nil {
				fc.file.Close()
				fc.release()
				return nil
			}
			id = first
		}
		*want.dst = id
	}
	fc.reopen.idx, fc.reopen.ic = idx, ic
	fc.reopen.fid, fc.reopen.opts = folderID, opts
	fc.reopen.path, fc.reopen.indexID, fc.reopen.resetID = path, indexID, resetID
	if opts.DeferWrites {
		// Reads come from memory from here on, so the locks are not protecting
		// anything the caller still does. What they do protect -- appending
		// under a second descriptor, and remove-and-recreate -- happens in
		// Close, under a fresh window.
		//
		// A snapshot going stale is a cache miss and nothing worse: another
		// session's append is simply not in it, and the message is re-parsed.
		fc.file.Preload()
		fc.deferred = true
		fc.release()
	}
	return fc
}

// flush writes what a deferred handle collected, under a second window.
//
// The generation is re-checked rather than assumed. Between the two windows
// another session may have found the file invalid and bumped it, and appending
// into a new generation at offsets computed against the old one would stamp the
// index at positions where some other message's record will land -- a valid
// record for the wrong message, which is the one failure no invalidation level
// can see. A changed generation drops the writes: they are derived data, and
// the next read re-parses.
func (fc *Handle) flush() {
	if len(fc.pending) == 0 {
		return
	}
	r := fc.reopen
	// The session travels: this window is the exclusive one every shared FETCH
	// now pays, and held_by must name who holds it (#1670).
	second := Open(r.idx, r.fid, Options{
		Locker: r.opts.Locker, User: r.opts.User, Folder: r.opts.Folder,
		TraceID: r.opts.TraceID, SessionID: r.opts.SessionID,
	})
	if second == nil {
		return
	}
	// Checked here and not before opening: the identity read outside the lock
	// is stale the moment it is read, and a bump landing between that read and
	// this Open would put these records into a generation they were not
	// computed against. second read its own identity while holding both locks,
	// so this comparison is the one that decides.
	if second.reopen.indexID != r.indexID || second.reopen.resetID != r.resetID {
		slog.Debug("msgcache: cache generation moved while the response was written; dropping cached fields",
			"trace_id", r.opts.TraceID, "pending", len(fc.pending))
		second.Close()
		return
	}

	// Chain heads are re-read, not carried over. Between the two windows
	// another session may have appended a record for one of these UIDs and
	// stamped a new head; chaining from the offset this request saw would hop
	// over that record and leave it unreachable. Losing a cached field is only
	// a re-parse, but it is exactly the "tolerate what somebody else wrote"
	// that splitting the window promised (#1545).
	heads := map[uint32]uint32{}
	if msgs, merr := r.idx.GetMessages(r.fid, mailbox.SeqSet{}); merr == nil {
		for _, m := range msgs {
			heads[m.UID] = m.CacheOffset
		}
	} else {
		slog.Debug("msgcache: could not re-read chain heads; dropping cached fields",
			"trace_id", r.opts.TraceID, "err", merr)
		second.Close()
		return
	}
	for _, p := range fc.pending {
		off, live := heads[p.UID()]
		if !live {
			// Expunged while the response was being written, which is ordinary
			// on a busy folder. Appending anyway writes bytes no chain reaches:
			// nothing stamps an offset for a UID the index no longer carries,
			// so the record sits in the file until the generation is bumped.
			// Not corruption -- growth on the path a busy mailbox takes most
			// often (#1549).
			continue
		}
		meta := p.meta
		meta.CacheOffset = off
		second.storeField(&meta, p.fieldID, p.data)
	}
	second.Close()
}

// head is the message's current chain head: the offset stamped earlier in
// THIS window when there is one, else what the index carries. Without it a
// second append for the same message in one FETCH (envelope, then body
// structure) would chain from the stale head and orphan the first value.
func (fc *Handle) head(m *mailbox.MessageMeta) uint32 {
	if fc == nil {
		return 0
	}
	if off, ok := fc.stamps[m.UID]; ok {
		return off
	}
	return m.CacheOffset
}

// read returns the merged field values for a message, or nil on a miss.
func (fc *Handle) read(m *mailbox.MessageMeta) map[uint32][]byte {
	if fc == nil {
		return nil
	}
	off := fc.head(m)
	if off == 0 {
		return nil // nothing cached for this message
	}
	vals, err := fc.file.ReadRecord(off)
	if err != nil {
		return nil // a bad chain is a miss; the re-parse overwrites the head
	}
	return vals
}

// storeField appends one field value for a message and moves the chain head.
func (fc *Handle) storeField(m *mailbox.MessageMeta, fieldID uint32, data []byte) {
	if fc == nil {
		return
	}
	if fc.deferred {
		// Copied: the caller's buffer belongs to the response being written.
		buf := make([]byte, len(data))
		copy(buf, data)
		fc.pending = append(fc.pending, pendingField{meta: *m, fieldID: fieldID, data: buf})
		return
	}
	off, err := fc.file.AppendRecord(fc.head(m), []mailindex.CacheFieldValue{
		{FieldID: fieldID, Data: data},
	})
	if err != nil {
		slog.Debug("msgcache: cache append failed", "uid", m.UID, "err", err)
		return
	}
	fc.stamps[m.UID] = off
}

// envelope returns the cached envelope for a message, or nil on any of the
// three misses.
func (fc *Handle) Envelope(m *mailbox.MessageMeta) *imaplib.Envelope {
	if fc == nil {
		return nil
	}
	data, ok := fc.read(m)[fc.envID]
	if !ok {
		return nil // no record, or a record without this field
	}
	env, ok := decodeEnvelope(data)
	if !ok {
		return nil
	}
	return env
}

// Preload tells the handle it is about to be read in full, so it reads the
// cache file once instead of paying a syscall per record. Callers that touch
// one message should not call it.
func (fc *Handle) Preload() {
	if fc == nil || fc.file == nil {
		return
	}
	fc.file.Preload()
}

// References returns the cached References header as a list of message ids, or
// nil when the message has no record, no such field, or genuinely had no
// References header.
//
// The last two cases are told apart by the empty marker: a message with no
// References is stored as one empty entry, so that a header nobody has to read
// again is not re-read for ever.
func (fc *Handle) References(m *mailbox.MessageMeta) ([]string, bool) {
	if fc == nil {
		return nil, false
	}
	data, ok := fc.read(m)[fc.refsID]
	if !ok {
		return nil, false
	}
	if len(data) == 0 {
		return nil, true // cached, and the message has none
	}
	return splitRefs(data), true
}

// EnvelopeAndReferences reads both in ONE pass over the message's record.
//
// Asking for them separately costs two full reads -- each walks the record
// chain, does its own ReadAt and decodes every field in it -- and threading
// needs both for every message it touches. On ten thousand messages that
// second pass was most of what a THREAD cost: the algorithm itself is ~8ms
// there, while the doubled read is ~170ms (#1461).
func (fc *Handle) EnvelopeAndReferences(m *mailbox.MessageMeta) (*imaplib.Envelope, []string, bool) {
	if fc == nil {
		return nil, nil, false
	}
	vals := fc.read(m)
	envData, ok := vals[fc.envID]
	if !ok {
		return nil, nil, false
	}
	env, ok := decodeEnvelope(envData)
	if !ok {
		return nil, nil, false
	}
	refsData, cached := vals[fc.refsID]
	if !cached {
		return env, nil, false
	}
	if len(refsData) == 0 {
		return env, nil, true // cached, and the message has none
	}
	return env, splitRefs(refsData), true
}

// StoreReferences caches the References of a message. An empty list is stored
// as an empty value rather than skipped: "no References" is an answer, and
// skipping it would make every such message a permanent miss.
func (fc *Handle) StoreReferences(m *mailbox.MessageMeta, refs []string) {
	if fc == nil {
		return
	}
	fc.storeField(m, fc.refsID, []byte(strings.Join(refs, "\n")))
}

// store appends the freshly-parsed envelope for a message.
func (fc *Handle) StoreEnvelope(m *mailbox.MessageMeta, env *imaplib.Envelope) {
	if fc == nil || env == nil {
		return
	}
	fc.storeField(m, fc.envID, encodeEnvelope(env))
}

// bodyStructure returns the cached body structure, or nil on any miss.
func (fc *Handle) BodyStructure(m *mailbox.MessageMeta) imaplib.BodyStructure {
	if fc == nil {
		return nil
	}
	data, ok := fc.read(m)[fc.bsID]
	if !ok {
		return nil
	}
	bs, ok := decodeBodyStructure(data)
	if !ok {
		return nil
	}
	return bs
}

// storeBodyStructure appends the freshly-parsed body structure. A structure
// that does not survive its own codec is not stored: serving a value the
// reader would reject is worse than a miss, and this is where an unknown
// node kind is caught.
func (fc *Handle) StoreBodyStructure(m *mailbox.MessageMeta, bs imaplib.BodyStructure) {
	if fc == nil || bs == nil {
		return
	}
	enc := encodeBodyStructure(bs)
	if _, ok := decodeBodyStructure(enc); !ok {
		slog.Debug("msgcache: body structure not representable; leaving uncached", "uid", m.UID)
		return
	}
	fc.storeField(m, fc.bsID, enc)
}

// close flushes the batched offset stamps -- one index write per FETCH, not
// per message -- releases the descriptor (no long-lived handles, #1176), and
// only then drops the locks: the stamp is part of the guarded window.
func (fc *Handle) Close() {
	if fc == nil {
		return
	}
	if fc.deferred {
		if fc.file != nil {
			if err := fc.file.Close(); err != nil {
				slog.Debug("msgcache: cache close failed", "err", err)
			}
			fc.file = nil
		}
		fc.flush()
		return
	}
	if len(fc.stamps) > 0 {
		if err := fc.idx.SetCacheOffsets(fc.fid, fc.stamps); err != nil {
			slog.Debug("msgcache: cache stamp failed", "err", err)
		}
	}
	if fc.file != nil {
		if err := fc.file.Close(); err != nil {
			slog.Debug("msgcache: cache close failed", "err", err)
		}
	}
	fc.release()
}

// release drops held locks in reverse acquisition order.
func (fc *Handle) release() {
	for i := len(fc.unlock) - 1; i >= 0; i-- {
		fc.unlock[i]()
	}
	fc.unlock = nil
}
