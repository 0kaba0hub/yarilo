package mailbox

import (
	"encoding/hex"
	"errors"
	"io"
	"time"
)

// ErrCorruptStorage is wrapped by a driver's read path when a message's backing
// storage is missing, truncated or malformed. Callers detect it via errors.Is and
// mark the folder for a rebuild on next open. Transient I/O errors (EIO, timeouts)
// must NOT be wrapped with it.
var ErrCorruptStorage = errors.New("mailbox: corrupt message storage")

// CorruptionMarker is an optional capability of an IndexBackend handle that
// persists a per-folder "needs rebuild" marker so a corrupt message detected on
// one read triggers a heal on the next open. Kept off the core UserIndex interface;
// callers type-assert. The marker is exposed read-side as Folder.Fsckd.
// UIDSpaceAdopter takes a folder's UID space from what a store already records
// rather than minting a fresh one. Kept off the core UserIndex interface, like
// CorruptionMarker: callers type-assert.
//
// For adopting a store another implementation wrote. A client reconnecting over
// the same mailbox keeps its cache only if both the UIDVALIDITY and the UIDs are
// the ones it saw; a fresh UID space makes it refetch everything, which costs
// what a migration over IMAP costs.
// FlagWriter records a message's flag state where the storage keeps it, and
// returns the name the message now has.
//
// One point, not three: a driver that keeps flags outside the index has to be
// told once, with the whole set, after the index has settled it. Kept off the
// core UserMailbox interface -- the dbox drivers do not implement it, because
// there the flags live in the index by design and the reference does the same.
//
// Maildir is the driver that needs it. A maildir keeps flags in the filename so
// the store describes itself: rebuild the index from the directory and the
// state comes back. A flag change that never reaches the name breaks that --
// the rebuild returns the state each message had when it was delivered, in our
// own store, with no other implementation involved (#1601).
type FlagWriter interface {
	// WriteFlags renames the message to carry flags and keywords, and returns
	// its new filename. The name is unchanged when nothing about it changes,
	// and the caller records whatever comes back.
	WriteFlags(folder, filename string, flags, keywords []string) (string, error)
}

// ErrUIDSpaceInUse says a folder already holds messages, so its UID space is one
// a session may have seen and is not open to being replaced.
//
// Not a failure at the call site: a caller adopting a store asks about every
// folder it opens, and most of them are ordinary folders with mail in them.
var ErrUIDSpaceInUse = errors.New("the folder already holds messages")

type UIDSpaceAdopter interface {
	// AdoptUIDSpace sets the folder's UIDVALIDITY and next UID. It must refuse a
	// folder that already holds messages: rewriting the UID space under a
	// mailbox somebody is already reading is the one thing UIDVALIDITY exists to
	// prevent.
	AdoptUIDSpace(folderID uint64, uidValidity, nextUID uint32) error
}

type CorruptionMarker interface {
	// MarkFolderCorrupt sets the marker. Idempotent.
	MarkFolderCorrupt(folderID uint64) error
	// ClearFolderCorrupt clears the marker, under the mailbox lock after the heal
	// and by the operator rebuild endpoint.
	ClearFolderCorrupt(folderID uint64) error
}

// ReactiveHealer is a driver that can self-heal a folder whose index references
// missing/corrupt storage. Only such drivers may have a folder flagged corrupt;
// a driver that cannot heal would strand the folder with nothing to clear the marker.
type ReactiveHealer interface {
	// HealCorruptFolder repairs folder and returns the UIDs it expunged (records
	// whose backing message vanished), so the caller can invalidate their FTS
	// documents.
	HealCorruptFolder(idx UserIndex, folder *Folder) ([]uint32, error)
}

// CanReactiveHeal reports whether box can self-heal corruption. Marking a folder
// corrupt must be gated on this: flag only what something will later clear.
func CanReactiveHeal(box any) bool {
	if um, ok := box.(UserMailbox); ok {
		box = Driver(um)
	}
	_, ok := box.(ReactiveHealer)
	return ok
}

// FolderAgnosticStorage marks a driver whose Scan enumerates storage-wide (not
// per-folder) records, such as mdbox where messages are shared across folders. A
// per-folder rebuild is unsafe for such a driver — it would import every stored
// message into the target folder with fresh UIDs — so it needs a dedicated
// storage-wide rebuild and the per-folder endpoint must reject it.
type FolderAgnosticStorage interface {
	FolderAgnosticScan() bool
}

// StorageRebuildStats reports what a storage-wide rebuild did.
type StorageRebuildStats struct {
	Scanned             int    // physical messages read from storage
	FoldersRebuilt      int    // per-folder indexes reset
	Expunged            int    // map records dropped (message vanished from storage)
	UnreferencedZeroref int    // present messages referenced by no folder; refcount reset to 0 for purge (NOT resurrected)
	OrphansRestored     int    // unreferenced messages re-filed into their ORIG_MAILBOX (only when restoreOrphans is set)
	RebuildCount        uint32 // new generation counter after the rebuild
	// ExpungedUIDs maps folder name → UIDs dropped from that folder's index by
	// the rebuild, so the operator caller can invalidate their FTS documents
	// (otherwise ghost entries until the next rescan).
	ExpungedUIDs map[string][]uint32
}

// StorageWideRebuilder is a folder-agnostic driver that can rebuild its whole
// storage under the storage (map) lock: reconcile the shared map against physical
// files, reset every folder index against surviving messages, recompute each map
// record's refcount from actual folder references (unreferenced messages become
// zero-ref garbage for the next purge, never resurrected), and drop map records
// whose message vanished. idx is the user's index backend for the same namespace.
//
// Orphan restore (re-filing an unreferenced message) happens only when
// restoreOrphans is set AND the message carries an ORIG_MAILBOX tag; it is re-filed
// into its recorded home folder, never blindly adopted. The default leaves
// unreferenced messages zero-ref for purge.
type StorageWideRebuilder interface {
	RebuildStorage(idx UserIndex, restoreOrphans bool) (StorageRebuildStats, error)
}

// MarkCorruptOnFetchErr flags folder for a heal when err wraps ErrCorruptStorage.
// It resolves the folder ID via idx and records the marker if idx supports it,
// else a no-op. box is the driver that produced err: the marker is persisted only
// when the driver can heal it (CanReactiveHeal). Best-effort — any
// resolution/marking error is swallowed.
//
// Returns true only when it actually marked the folder, so a caller can gate its
// own "already flagged" state without repeating the corruption classification.
func MarkCorruptOnFetchErr(box any, idx UserIndex, folder string, err error) bool {
	if err == nil || !errors.Is(err, ErrCorruptStorage) {
		return false
	}
	if !CanReactiveHeal(box) {
		return false
	}
	cm, ok := idx.(CorruptionMarker)
	if !ok {
		return false
	}
	f, oerr := idx.OpenFolder(folder, 0)
	if oerr != nil {
		return false
	}
	return cm.MarkFolderCorrupt(f.ID) == nil
}

// FormatObjectID renders a 16-byte GUID as the RFC 8474 object identifier used
// for IMAP MAILBOXID / EMAILID (OBJECTID): 32 lowercase hex characters.
func FormatObjectID(guid [16]byte) string {
	return hex.EncodeToString(guid[:])
}

// MessageMeta holds per-message metadata stored in the index.
type MessageMeta struct {
	UID          uint32
	Filename     string // backend-specific filename returned by UserMailbox.Save
	Flags        []string
	Keywords     []string
	ModSeq       uint64
	Size         uint32
	VSize        uint32
	InternalDate time.Time
	GUID         [16]byte
	CacheOffset  uint32
	// AltTier is true when the message body resides in alt (cold) storage.
	// Stored as FlagBackend (0x40) in the on-disk index record so Fetch() opens
	// the correct tier without a wasted primary-tier syscall. Only meaningful for
	// mdbox; other drivers ignore it.
	AltTier bool
}

// Folder holds index-level folder metadata.
type Folder struct {
	ID            uint64
	Name          string
	UIDValidity   uint32
	NextUID       uint32
	Messages      uint32
	Unseen        uint32
	HighestModSeq uint64
	// GUID is a stable 16-byte identifier stamped at folder creation time.
	// Survives RENAME (unlike Name). Used as the key namespace for per-folder
	// metadata in pkg/dict (RFC 5464 METADATA) and as the rename-stable handle
	// for ACL state, quota counters, etc.
	GUID [16]byte
	// Fsckd is true when the folder index carries the persisted corruption marker:
	// a driver detected a missing/corrupt message and flagged the index for a
	// rebuild on the next open. The session runs the rebuild and clears the marker.
	Fsckd bool
}

// SeqSet is a set of UIDs or sequence numbers (use UID=0 for seq).
type SeqSet []SeqRange

type SeqRange struct {
	From, To uint32 // inclusive; To==0 means '*'
}

// FolderStamp is what a cheap validity check compares. Both files are included
// because a write moves the log and a fold moves both; the SIZE is what makes
// it sound where mtime granularity is coarse, as it is on NFS.
type FolderStamp struct {
	BaseSize, LogSize int64
	BaseMod, LogMod   time.Time
}

// FlagsUpdate carries the new flag and keyword sets for one message in a
// batch UpdateFlagsMulti call.
type FlagsUpdate struct {
	Flags    []string
	Keywords []string
	// Mode says what Flags and Keywords mean. The zero value replaces the whole
	// set, which is what STORE FLAGS (...) asks for. FlagsAdd and FlagsRemove
	// name only what changes, and the index resolves them against the record as
	// its lock finds it — the difference that keeps a concurrent change from
	// being overwritten by a set the caller read earlier (#1250).
	Mode FlagsMode
}

// FlagsMode selects how a FlagsUpdate is applied.
type FlagsMode int

const (
	// FlagsSet replaces the flag and keyword set. Correct where the caller
	// genuinely declares the set: STORE FLAGS (...), a Sieve setflag action, or
	// a maildir sync where the filename is the truth.
	FlagsSet FlagsMode = iota
	// FlagsAdd unions the named flags and keywords into the record's own.
	FlagsAdd
	// FlagsRemove subtracts them from the record's own.
	FlagsRemove
)

// FlagsResult is what a message ended up with after a batch update: the flags
// and keywords the index holds, and the modseq of the change. The resulting set
// is returned rather than assumed because under FlagsAdd or FlagsRemove the
// caller does not know it — that is the point of naming only the delta.
type FlagsResult struct {
	ModSeq   uint64
	Flags    []string
	Keywords []string
}

// SyncStats reports what a proactive index reconcile changed. Imported counts
// files that gained a UID, Expunged counts index records whose file vanished,
// Updated counts tracked messages whose on-disk filename was repointed. Changed
// is false when the folder matched the index exactly and no index write ran.
type SyncStats struct {
	Imported int
	Expunged int
	Updated  int
	Changed  bool
}

// RFC822Size returns the size to report as RFC822.SIZE and to match in SEARCH
// LARGER/SMALLER: the VIRTUAL size, the octet count of the message as transmitted
// with CRLF line endings (RFC 3501 §6.4.5), never the physical on-disk size. When
// VSize == 0 the backend recorded no virtual size and the physical Size is used.
func (m *MessageMeta) RFC822Size() uint32 {
	if m == nil {
		return 0
	}
	if m.VSize != 0 {
		return m.VSize
	}
	return m.Size
}

// RFC822Size mirrors MessageMeta.RFC822Size for scan records.
func (r *ScanRecord) RFC822Size() uint32 {
	if r == nil {
		return 0
	}
	if r.VSize != 0 {
		return r.VSize
	}
	return r.Size
}

// ScanRecord is one entry produced by UserMailbox.Scan: the raw per-message info
// a driver reconstructs from disk alone (no index). Drives admin rebuild flows.
type ScanRecord struct {
	Filename     string
	GUID         [16]byte
	Size         uint32
	VSize        uint32
	InternalDate time.Time
	Flags        []string
	// OrigMailbox is the mailbox a message was originally saved into, recovered
	// from storage metadata (mdbox trailer). Empty when unrecorded. A storage-wide
	// rebuild uses it to restore an orphan to its home folder instead of guessing.
	OrigMailbox string
}

// MailboxBackend is the per-process factory for user-scoped storage handles.
// It holds no per-user state; all per-user state lives in UserMailbox.
type MailboxBackend interface {
	OpenUser(*UserInfo) UserMailbox
}

// UserMailbox is a per-session, per-user storage handle bound to a single UserInfo
// at creation time. Init MUST be called before any other method; it creates the
// on-disk directory structure.
//
// Save takes the assigned UID as a parameter and returns the message's stored
// filename plus its VIRTUAL (CRLF-normalized) size, which must land in
// MessageMeta.VSize so RFC822.SIZE is consistent from the first FETCH. If Save
// fails after the UID was allocated, the UID is burnt and the index skips the hole
// on the next scan. Close releases any open file descriptors.
//
// Save is the single generation site for per-message GUIDs (RFC 8474 EMAILID,
// JMAP Email.id). A zero guid means "assign a fresh one"; a non-zero guid is
// stored verbatim so MOVE keeps one identity across folders. The returned GUID
// is the effective one and must land in MessageMeta.GUID. Drivers persist it in
// storage (dbox record trailer, maildir uidlist), so it survives an index
// rebuild.
type UserMailbox interface {
	Init() error
	Create(folder string) error
	Delete(folder string) error
	Rename(oldName, newName string) error
	Save(folder string, r io.Reader, uid uint32, size int64, flags []string, guid [16]byte) (name string, vsize uint32, outGUID [16]byte, err error)
	// Move relocates one message between folders keeping its identity: the
	// returned GUID equals guid (RFC 8474: MOVE must not change EMAILID).
	// Source and destination lock in name order, so a concurrent A->B / B->A
	// pair cannot deadlock.
	Move(srcFolder, dstFolder, filename string, guid [16]byte) (newName string, outGUID [16]byte, err error)
	// Fetch returns a reader for the message body. altTier hints that the
	// message lives in alt (cold) storage so the driver can open it directly
	// without trying the primary path first. The hint is set from
	// MessageMeta.AltTier which is persisted in the index as FlagBackend.
	Fetch(folder, filename string, altTier bool) (io.ReadCloser, error)
	Remove(folder, filename string) error
	List(folder string) ([]*MessageMeta, error)
	FolderExists(folder string) (bool, error)
	// ListFolders returns every folder in the user's personal namespace,
	// including nested folders (dbox drivers recurse the physical tree) and
	// \NoSelect containers (FolderEntry.Selectable=false).
	ListFolders() ([]FolderEntry, error)
	// Scan walks folder's on-disk representation and yields every visible message
	// as a ScanRecord, so the admin rebuild flow can regenerate the fileindex
	// independently of the index. Drivers without disk-scan return a "not yet
	// implemented" error.
	Scan(folder string) ([]ScanRecord, error)
	Close() error
}

// IndexBackend is the per-process factory for user-scoped index handles.
type IndexBackend interface {
	OpenUser(*UserInfo) UserIndex
}

// UserIndex is a per-session, per-user index handle.
// All folder IDs are local to this handle — they must not be shared across handles.
type UserIndex interface {
	OpenFolder(folder string, uidValidity uint32) (*Folder, error)
	SaveFolder(f *Folder) error
	AppendMessage(folderID uint64, m *MessageMeta) error
	// AllocateUID atomically reserves and persists the folder's next UID under the
	// cross-process mailbox lock. If the caller fails between AllocateUID and
	// AppendMessage the UID is burnt (uid hole), reconciled by a later rebuild.
	AllocateUID(folderID uint64) (uint32, error)
	// AllocateUIDWithModSeq atomically reserves the folder's next UID and
	// pre-allocates the next modseq value in one lock/reload/flush cycle.
	AllocateUIDWithModSeq(folderID uint64) (uid uint32, modseq uint64, err error)
	// AllocateAndAppend assigns a UID and records the message in a single
	// lock/reload/flush cycle. m.UID and m.ModSeq are filled in by the call; all
	// other fields (including m.Filename) must be set by the caller beforehand.
	AllocateAndAppend(folderID uint64, m *MessageMeta) error
	UpdateFlags(folderID uint64, uid uint32, flags, keywords []string) error

	// AddFlags adds flags and keywords, keeping whatever the message already
	// carries. Use it wherever the caller wants to set one flag rather than to
	// declare the whole set: UpdateFlags writes an absolute list, so a list
	// built from an earlier read silently drops every change made since (#1250).
	AddFlags(folderID uint64, uid uint32, flags, keywords []string) error

	// RemoveFlags clears flags and keywords, leaving the rest untouched. The
	// counterpart of AddFlags and needed for the same reason: clearing one flag
	// through UpdateFlags means sending the whole remaining set, which is a set
	// the caller read earlier.
	RemoveFlags(folderID uint64, uid uint32, flags, keywords []string) error
	// UpdateFilename repoints the stored on-disk filename for a UID without
	// touching flags, UID or modseq. Used by maildir sync-on-open when another MUA
	// renamed a tracked file out of band. No-op when uid is unknown.
	UpdateFilename(folderID uint64, uid uint32, filename string) error
	// UpdateFlagsMulti replaces flags+keywords for a batch of UIDs in a
	// single lock/reload/flush cycle. Returns the new modseq per UID.
	UpdateFlagsMulti(folderID uint64, updates map[uint32]FlagsUpdate) (map[uint32]FlagsResult, error)
	// SetAltTier sets or clears the AltTier marker (FlagBackend) for every message
	// in folderID whose Filename matches one of the supplied names, under the
	// folder's cross-process mailbox lock. Called by the altmove API after
	// relocating mdbox files so subsequent Fetch calls skip the primary-tier open.
	SetAltTier(folderID uint64, filenames []string, altTier bool) error
	GetMessages(folderID uint64, uids SeqSet) ([]*MessageMeta, error)
	ExpungeMessage(folderID uint64, uid uint32) error
	NextModSeq(folderID uint64) (uint64, error)
	// Vanished returns UIDs expunged from folderID with modseq strictly
	// greater than sinceModSeq. Drives QRESYNC (RFC 7162) SELECT and
	// UID FETCH (CHANGEDSINCE N VANISHED) responses.
	Vanished(folderID uint64, sinceModSeq uint64) ([]uint32, error)
	// VanishedGUIDs answers the same question as Vanished by message identity:
	// a protocol that addresses messages by GUID cannot report a UID, and the
	// message is gone, so its GUID can be read from nowhere else afterwards.
	// complete is false when a record in range cannot be named, and the caller
	// must degrade rather than report a shorter list as the whole truth.
	VanishedGUIDs(folderID uint64, sinceModSeq uint64) (guids [][16]byte, complete bool, err error)
	// FolderStamp describes the folder's files without opening it: the sizes
	// and modification times of the base index and its log. It is a proof of
	// unchangedness for a cached read, never a substitute for reading -- the
	// log is what moves modseq between folds, so the base alone would answer
	// with a stale marker.
	FolderStamp(folder string) (FolderStamp, error)
	// ExpungeFloor reports the modseq below which Vanished can no longer
	// answer, because compaction folded the log that held those records. Zero
	// means nothing has been folded away. A caller below the floor must
	// degrade rather than read an empty Vanished as "nothing was deleted".
	ExpungeFloor(folderID uint64) (uint64, error)
	Keywords(folderID uint64) ([]string, error)
	// RenameFolder renames oldName to newName in the index.
	// Called by IMAP RENAME immediately after UserMailbox.Rename succeeds.
	RenameFolder(oldName, newName string) error
	// DeleteFolder removes folder's index state.
	// Called by IMAP DELETE immediately after UserMailbox.Delete succeeds.
	DeleteFolder(folder string) error
	// GetPOP3UIDLs loads saved POP3 UIDLs for a folder (uid → uidl string).
	// Returns an empty map when no saved UIDLs exist yet.
	GetPOP3UIDLs(folderID uint64) (map[uint32]string, error)
	// SavePOP3UIDLs persists POP3 UIDLs so subsequent sessions use stable values.
	SavePOP3UIDLs(folderID uint64, uidls map[uint32]string) error
	// ResetFolder atomically replaces folderID's on-disk record set with the
	// supplied messages. Preserves UIDVALIDITY and the folder GUID; bumps NextUID
	// past max(records.UID). Each surviving record keeps its own ModSeq (a record
	// with none is stamped fresh) and HighestModSeq advances to the greatest one.
	// Returns UIDs present before the reset but absent after, so the caller can
	// invalidate their FTS documents. Caller holds the mailbox lock and has made a
	// .bak of the old base file.
	ResetFolder(folderID uint64, records []*MessageMeta) ([]uint32, error)
	// OptimizeIndex compacts the .index.log overlay into the base .index file,
	// under the same write lock as a normal write. No-op nil when nothing to compact.
	OptimizeIndex(folderID uint64) error
	// FolderVSize returns the folder's cached aggregate virtual size and message
	// count (the hdr-vsize extension), which the count quota backend sums across a
	// user's folders.
	FolderVSize(folderID uint64) (bytes uint64, messages uint32, err error)
	// GUIDBackfillNeeded reports whether the folder still holds records written
	// before per-message GUIDs existed. Constant time: it reads a header state,
	// never the records, so a large folder does not pay a scan on every open.
	GUIDBackfillNeeded(folderID uint64) (bool, error)
	// SetGUIDs stamps GUIDs onto existing records and marks the folder done.
	// Values must come from storage (Scan), never be invented here, or the
	// index and the mail store would disagree on a message's identity. Records
	// that already carry a GUID keep it, so a resumed pass is a no-op for them.
	SetGUIDs(folderID uint64, guids map[uint32][16]byte) error
	// RecomputeVSize forces a rebuild of the folder's vsize aggregate from
	// records and persists it — the admin recovery path for a corrupted count.
	RecomputeVSize(folderID uint64) error
	Close() error
}

// UnlockedReader is the optional capability an index has when its on-disk files
// can prove their own freshness: a read whose answer only reaches a caller can
// then be served without the cross-process lock. An index without the property
// is simply an index without this method, so callers ask for it rather than
// require it.
//
// It is deliberately not part of UserIndex. Making it a required method would
// force every index implementation to have the property or to lie about it, and
// the whole point is that a file which cannot prove its state falls back to the
// locked path.
type UnlockedReader interface {
	GetMessagesUnlocked(folderID uint64, uids SeqSet) ([]*MessageMeta, error)
}

// ReadMessages is the read for a caller whose answer goes to a client and
// decides nothing on disk — FETCH, SEARCH, a JMAP query, a diagnostic dump. It
// takes the lock-free path where the index offers one.
//
// A caller whose answer chooses what to rewrite or delete must call
// GetMessages directly. That is not a preference: a stale answer there does not
// become visible a moment later, it decides wrongly and the write lands anyway.
// Keeping the two as separate calls is what makes the choice visible at the
// call site instead of hidden in an argument.
func ReadMessages(idx UserIndex, folderID uint64, uids SeqSet) ([]*MessageMeta, error) {
	if u, ok := idx.(UnlockedReader); ok {
		return u.GetMessagesUnlocked(folderID, uids)
	}
	return idx.GetMessages(folderID, uids)
}

// CorruptIndexError says the folder's index cannot be read because what is on
// disk is not what this version writes -- a wrong major version, a truncated
// header. It names the folder because that is the one thing an operator needs
// and the one thing a generic error loses: the failure is per folder, the rest
// of the account keeps working, and without the name there is nothing to point
// a repair at.
type CorruptIndexError struct {
	Folder string
	Err    error
}

func (e *CorruptIndexError) Error() string {
	return "mailbox: index of folder " + e.Folder + " is unreadable: " + e.Err.Error()
}

func (e *CorruptIndexError) Unwrap() error { return e.Err }
