// FETCH ENVELOPE from the index cache (#1030, commit 3): the listing's hot
// path stops opening message files. The cached value is the parsed envelope
// in our own length-prefixed encoding -- the producer generation in the cache
// header owns its identity, so the encoding may evolve with a gen bump and
// never with a silent reinterpretation.
//
// Three different kinds of "not there", none of them a client error:
//
//	file invalid  (ErrCacheInvalid)  -> cache absent for the folder; the old
//	                                    file is removed and a fresh pair is
//	                                    created lazily on the next write
//	offset 0                         -> nothing cached for the message; parse
//	                                    as today, append, stamp
//	record hit, field missing        -> parse, append a chain record carrying
//	                                    the field, stamp the new head
package imap

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
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

func lockCachePath(path string) func() {
	mu, _ := cachePathMu.LoadOrStore(path, &sync.Mutex{})
	m, ok := mu.(*sync.Mutex)
	if !ok {
		// Unreachable: this map only ever stores *sync.Mutex. Failing open
		// would silently drop the in-process tier, so fail loud instead.
		panic("imap: cache mutex map holds a foreign type")
	}
	m.Lock()
	return m.Unlock
}

// cacheFieldEnvelope is the cache field name for the encoded envelope.
const cacheFieldEnvelope = "yarilo.envelope"

// indexCacher is the slice of the file-index surface the cache reader needs.
// Asserted at use: an index backend without it simply serves no cache.
type indexCacher interface {
	CachePairIdentity(folderID uint64) (indexID, resetID uint32, ok bool, err error)
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
type folderCache struct {
	file   *mailindex.CacheFile
	envID  uint32
	bsID   uint32
	stamps map[uint32]uint32 // uid -> new head offset, flushed on close
	idx    indexCacher
	fid    uint64
	// unlock releases the locks taken for the open-append-stamp window, in
	// reverse acquisition order. Two sessions of one account in one folder
	// are the ordinary case: without the lock their appends interleave under
	// two descriptors, and a stamped offset resolves inside a file the other
	// writer has since extended -- a fully valid-looking record for a
	// DIFFERENT message, which no invalidation level can see.
	unlock []func()
}

// openFolderCache opens (or lazily creates) the folder's cache pair. Any
// invalidity removes the stale file and starts a fresh one -- the cache is
// derived data, absence is its recovery mode.
func (s *session) openFolderCache(idx mailbox.UserIndex, folderID uint64) *folderCache {
	ic, ok := idx.(indexCacher)
	if !ok {
		return nil
	}
	indexID, resetID, extOK, err := ic.CachePairIdentity(folderID)
	if err != nil || !extOK {
		return nil
	}
	path, err := ic.CachePath(folderID)
	if err != nil {
		return nil
	}
	fc := &folderCache{idx: ic, fid: folderID, stamps: make(map[uint32]uint32)}
	// The whole open-append-stamp window runs under the lock, reads
	// included: remove-and-recreate under a live descriptor and the
	// read-modify-write of the field table are only safe when nobody else
	// is inside the pair. In-process mutex first, then the cross-process
	// MailboxKey -- the same two tiers every shared write path uses.
	fc.unlock = append(fc.unlock, lockCachePath(path))
	if lkr := s.srv.opts.Locker; lkr != nil && s.userInfo != nil && s.folder != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		lk, lerr := locks.Acquire(ctx, lkr, locks.MailboxKey(s.userInfo.Username, s.folder.Name), s.userInfo.Username, 30*time.Second)
		cancel()
		if lerr != nil {
			slog.Debug("imap: cache lock unavailable; serving uncached", "sid", s.sid, "err", lerr)
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
		if errors.Is(err, mailindex.ErrCacheInvalid) {
			// Wrong pair, wrong generation: garbage by definition. Remove
			// and recreate; the parse the client already paid for seeds it.
			_ = os.Remove(path)
		}
		cf, cerr := mailindex.CreateCache(path, indexID, resetID)
		if cerr != nil {
			slog.Debug("imap: cache create failed; serving uncached", "sid", s.sid, "err", cerr)
			fc.release()
			return nil
		}
		fc.file = cf
	default:
		slog.Debug("imap: cache open failed; serving uncached", "sid", s.sid, "err", err)
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
	} {
		id, ok := fc.file.FieldID(want.name)
		if !ok {
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
	return fc
}

// head is the message's current chain head: the offset stamped earlier in
// THIS window when there is one, else what the index carries. Without it a
// second append for the same message in one FETCH (envelope, then body
// structure) would chain from the stale head and orphan the first value.
func (fc *folderCache) head(m *mailbox.MessageMeta) uint32 {
	if off, ok := fc.stamps[m.UID]; ok {
		return off
	}
	return m.CacheOffset
}

// read returns the merged field values for a message, or nil on a miss.
func (fc *folderCache) read(m *mailbox.MessageMeta) map[uint32][]byte {
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
func (fc *folderCache) storeField(m *mailbox.MessageMeta, fieldID uint32, data []byte) {
	if fc == nil {
		return
	}
	off, err := fc.file.AppendRecord(fc.head(m), []mailindex.CacheFieldValue{
		{FieldID: fieldID, Data: data},
	})
	if err != nil {
		slog.Debug("imap: cache append failed", "uid", m.UID, "err", err)
		return
	}
	fc.stamps[m.UID] = off
}

// envelope returns the cached envelope for a message, or nil on any of the
// three misses.
func (fc *folderCache) envelope(m *mailbox.MessageMeta) *imaplib.Envelope {
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

// store appends the freshly-parsed envelope for a message.
func (fc *folderCache) store(m *mailbox.MessageMeta, env *imaplib.Envelope) {
	if fc == nil || env == nil {
		return
	}
	fc.storeField(m, fc.envID, encodeEnvelope(env))
}

// bodyStructure returns the cached body structure, or nil on any miss.
func (fc *folderCache) bodyStructure(m *mailbox.MessageMeta) imaplib.BodyStructure {
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
func (fc *folderCache) storeBodyStructure(m *mailbox.MessageMeta, bs imaplib.BodyStructure) {
	if fc == nil || bs == nil {
		return
	}
	enc := encodeBodyStructure(bs)
	if _, ok := decodeBodyStructure(enc); !ok {
		slog.Debug("imap: body structure not representable; leaving uncached", "uid", m.UID)
		return
	}
	fc.storeField(m, fc.bsID, enc)
}

// close flushes the batched offset stamps -- one index write per FETCH, not
// per message -- releases the descriptor (no long-lived handles, #1176), and
// only then drops the locks: the stamp is part of the guarded window.
func (fc *folderCache) close() {
	if fc == nil {
		return
	}
	if len(fc.stamps) > 0 {
		if err := fc.idx.SetCacheOffsets(fc.fid, fc.stamps); err != nil {
			slog.Debug("imap: cache stamp failed", "err", err)
		}
	}
	if fc.file != nil {
		if err := fc.file.Close(); err != nil {
			slog.Debug("imap: cache close failed", "err", err)
		}
	}
	fc.release()
}

// release drops held locks in reverse acquisition order.
func (fc *folderCache) release() {
	for i := len(fc.unlock) - 1; i >= 0; i-- {
		fc.unlock[i]()
	}
	fc.unlock = nil
}
