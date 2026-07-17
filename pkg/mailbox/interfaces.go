package mailbox

import (
	"encoding/hex"
	"errors"
	"io"
	"time"
)

// ErrCorruptStorage is returned (wrapped) by a mailbox driver's read path when a
// message's backing storage is missing, truncated or malformed — as opposed to a
// transient I/O error. It is the reactive-rebuild trigger: a caller that reads a
// message and sees this via errors.Is marks the folder corrupt so the next open
// rebuilds the index from storage. Transient errors (EIO, timeouts) must NOT be
// wrapped with it, so a flaky disk does not trigger a mass index reset.
var ErrCorruptStorage = errors.New("mailbox: corrupt message storage")

// CorruptionMarker is an optional capability of an IndexBackend handle: it
// persists a per-folder "needs rebuild" marker (the FSCKD header flag) so a
// missing/corrupt message detected on one read triggers a reactive heal on the
// next open — possibly in another process. Kept off the core UserIndex
// interface so alternate index backends and test mocks need not implement it;
// callers type-assert. The current marker is exposed read-side as Folder.Fsckd.
type CorruptionMarker interface {
	// MarkFolderCorrupt sets the marker. Idempotent.
	MarkFolderCorrupt(folderID uint64) error
	// ClearFolderCorrupt clears the marker. Called under the mailbox lock right
	// after the reactive heal, and by the operator rebuild endpoint.
	ClearFolderCorrupt(folderID uint64) error
}

// ReactiveHealer is an index-authoritative driver that can self-heal a folder
// whose index references missing/corrupt storage. Only such drivers should ever
// have a folder flagged FSCKD: marking a driver that cannot heal (mdbox until
// #594 Phase 2b) would strand the folder corrupt forever with nothing to clear
// the marker.
type ReactiveHealer interface {
	HealCorruptFolder(idx UserIndex, folder *Folder) (int, error)
}

// CanReactiveHeal reports whether box can self-heal corruption. The corruption
// marker must be gated on this: flag only what something will later clear.
func CanReactiveHeal(box any) bool {
	_, ok := box.(ReactiveHealer)
	return ok
}

// FolderAgnosticStorage marks a driver whose Scan enumerates storage-wide (not
// per-folder) records — mdbox, where messages are shared across folders through
// the map. A per-folder rebuild (idxrebuild.RebuildFolder) is unsafe for such a
// driver: it would import every stored message into the target folder with fresh
// UIDs. These drivers need a dedicated storage-wide rebuild; the operator
// per-folder endpoint must reject them.
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
}

// StorageWideRebuilder is a folder-agnostic driver (mdbox) that can rebuild its
// whole storage: reconcile the shared map against the physical files, reset
// every folder index against the surviving messages, recompute each map record's
// refcount from the actual folder references (so an unreferenced message becomes
// zero-ref garbage for the next purge — never resurrected), and drop map records
// whose message vanished — all under the storage (map) lock. idx is the user's
// index backend for the same namespace. Implemented by mdbox; driven by the
// operator rebuild-storage endpoint and (later) the reactive trigger.
//
// Orphan RESTORE (re-filing an unreferenced message into a mailbox) happens only
// when restoreOrphans is set AND the message carries an ORIG_MAILBOX tag, so it
// is re-filed into its recorded home folder — never blindly adopted. The default
// (restoreOrphans false) leaves unreferenced messages zero-ref for purge, so a
// default run is byte-identical to the pre-restore behaviour. Even a tag only
// proves "was once in this folder", not "is lost", so the resurrection decision
// stays the operator's, per-request.
type StorageWideRebuilder interface {
	RebuildStorage(idx UserIndex, restoreOrphans bool) (StorageRebuildStats, error)
}

// MarkCorruptOnFetchErr flags folder for a reactive heal when err reports
// corrupt storage (ErrCorruptStorage). It resolves the folder ID via idx and
// records the marker if idx supports it — a no-op otherwise. Shared by the read
// paths of every protocol (IMAP, POP3, ManageSieve/imapsieve, FTS) so an
// sdbox mailbox self-heals no matter which protocol first trips over the bad
// message. Best-effort: any resolution/marking error is swallowed.
//
// box is the driver that produced err: the marker is only persisted when the
// driver can actually heal it (CanReactiveHeal), so a driver without a reactive
// rebuilder never leaves a folder stuck FSCKD.
//
// It returns true only when it actually marked the folder, so a caller can gate
// its own per-session/per-scan "already flagged" state without duplicating the
// corruption classification: `if !marked && MarkCorruptOnFetchErr(...) { marked = true }`.
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
// for IMAP MAILBOXID / EMAILID (OBJECTID): 32 lowercase hex characters, the
// same string form other mail servers use for the 128-bit GUID.
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
	// Stored as FlagBackend (0x40) in the on-disk index record so Fetch()
	// can open the correct tier without a wasted primary-tier syscall.
	// Only meaningful for mdbox; other drivers ignore it.
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
	// GUID is a stable 16-byte identifier stamped at folder creation
	// time. Survives RENAME (unlike Name). Used as the key namespace
	// for per-folder metadata in pkg/dict (RFC 5464 METADATA) and as
	// the rename-stable handle for ACL state, quota counters, etc.
	GUID [16]byte
	// Fsckd is true when the folder index carries the persisted FSCKD marker —
	// a dbox driver detected a missing/corrupt message and flagged the index
	// for a rebuild on the next open. The session runs the reactive rebuild and
	// clears the marker.
	Fsckd bool
}

// SeqSet is a set of UIDs or sequence numbers (use UID=0 for seq).
type SeqSet []SeqRange

type SeqRange struct {
	From, To uint32 // inclusive; To==0 means '*'
}

// ScanRecord is one entry produced by UserMailbox.Scan — the raw
// per-message info a storage driver can reconstruct from disk
// alone (no help from the index). Used by admin rebuild flows to
// regenerate the fileindex after corruption or operator request.
//
// Fields populated per driver:
//
//	maildir: Filename, Size (from "S=" or stat), VSize (from "W="),
//	         InternalDate (from stat mtime), Flags (parsed from
//	         the ":2,FLAGS" trailer); GUID stays zero — Maildir
//	         filenames carry no GUID.
//	dbox:    Filename, GUID (from "G<hex>\n" trailer line),
//	         Size+VSize (from "Z<hex>" / "V<hex>"), InternalDate
//	         (from "R<hex>" Unix epoch); Flags empty — dbox
//	         delegates flag storage to the index.
//	mdbox:   not implemented in this phase — driver returns
//	         "not yet implemented" until Phase MDBOX-PROD-READY.
//
// FlagsUpdate carries the new flag and keyword sets for one message in a
// batch UpdateFlagsMulti call.
type FlagsUpdate struct {
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

type ScanRecord struct {
	Filename     string
	GUID         [16]byte
	Size         uint32
	VSize        uint32
	InternalDate time.Time
	Flags        []string
	// OrigMailbox is the mailbox a message was originally saved into, recovered
	// from storage metadata (mdbox trailer). Empty for records written before the
	// key existed or for drivers that don't store it. A storage-wide rebuild uses
	// it to restore an orphan to its home folder instead of guessing.
	OrigMailbox string
}

// MailboxBackend is the per-process factory for user-scoped storage handles.
// It holds no per-user state; all per-user state lives in UserMailbox.
//
// Phase 5 — multi-namespace stub:
//
//	OpenNamespace(user *UserInfo, namespace string) (UserMailbox, error)
//	    Returns a handle for a shared or public namespace (e.g. "shared/" or "Public").
//	    Implementation: separate Backend instance rooted at the namespace storage dir,
//	    wrapped with the same UserMailbox contract. Namespace list comes from
//	    config.Namespaces (private/shared/public, each with its own location template).
type MailboxBackend interface {
	OpenUser(*UserInfo) UserMailbox
}

// UserMailbox is a per-session, per-user storage handle bound to a single UserInfo
// at creation time.
//
// Init MUST be called before any other method — it creates the on-disk directory
// structure. Callers that open a handle but never call Init will see errors from
// the underlying filesystem operations.
//
// Save takes the assigned UID as a parameter. Drivers that encode the UID in
// the on-disk filename (sdbox: u.<uid>; mdbox: map_uid bookkeeping) use it
// directly; Maildir ignores the UID for its filename but writes the
// uid→filename mapping into the dovecot-uidlist sidecar inline. The canonical
// caller flow is:
//
//	uid     := idx.AllocateUID(folderID)
//	filename := box.Save(folder, r, uid, size, flags)
//	idx.AppendMessage(folderID, &MessageMeta{UID: uid, Filename: filename, ...})
//
// If Save fails after AllocateUID, the UID is burnt — the index
// simply skips the hole on the next scan.
//
// Close releases any open file descriptors held by the handle.
type UserMailbox interface {
	Init() error
	Create(folder string) error
	Delete(folder string) error
	Rename(oldName, newName string) error
	Save(folder string, r io.Reader, uid uint32, size int64, flags []string) (string, error)
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
	// Scan walks the on-disk representation of folder and yields
	// every visible message as a ScanRecord. Used by the admin
	// rebuild flow to regenerate the fileindex independently of
	// whatever the index currently believes. Returns
	// (nil, fmt.Errorf("driver/scan: not yet implemented"))
	// from drivers that have not implemented disk-scan yet.
	Scan(folder string) ([]ScanRecord, error)
	Close() error
}

// IndexBackend is the per-process factory for user-scoped index handles.
//
// Phase 5 — multi-namespace stub:
//
//	OpenNamespace(user *UserInfo, namespace string) (UserIndex, error)
//	    Returns an index handle for a non-private namespace.
//	    Implementation: separate Backend instance rooted at the namespace index dir.
type IndexBackend interface {
	OpenUser(*UserInfo) UserIndex
}

// UserIndex is a per-session, per-user index handle.
// All folder IDs are local to this handle — they must not be shared across handles.
type UserIndex interface {
	OpenFolder(folder string, uidValidity uint32) (*Folder, error)
	SaveFolder(f *Folder) error
	AppendMessage(folderID uint64, m *MessageMeta) error
	// AllocateUID atomically reserves and persists the folder's next UID
	// under the cross-process mailbox lock. The caller passes the UID to
	// UserMailbox.Save, then records the full MessageMeta via AppendMessage.
	//
	// If the caller fails between AllocateUID and AppendMessage the UID
	// is burnt (uid hole). Periodic rebuild reconciles state by scanning
	// the on-disk tree.
	AllocateUID(folderID uint64) (uint32, error)
	// AllocateUIDWithModSeq atomically reserves the folder's next UID and
	// pre-allocates the next modseq value in one lock/reload/flush cycle,
	// replacing the separate AllocateUID + NextModSeq calls in Append.
	AllocateUIDWithModSeq(folderID uint64) (uid uint32, modseq uint64, err error)
	// AllocateAndAppend assigns a UID and records the message in a single
	// lock/reload/flush cycle — the go-imap appendBytes pattern applied to
	// persistent storage. m.UID and m.ModSeq are filled in by the call;
	// all other fields must be set by the caller. m.Filename must already
	// be known (box.Save completes before this call). Replaces the two-step
	// AllocateUID(WithModSeq) + AppendMessage pattern that caused APPEND stalls.
	AllocateAndAppend(folderID uint64, m *MessageMeta) error
	UpdateFlags(folderID uint64, uid uint32, flags, keywords []string) error
	// UpdateFilename repoints the stored on-disk filename for a UID without
	// touching flags, UID or modseq. Used by maildir sync-on-open when a second
	// MUA renamed a tracked file out of band (a flag change moves the ":2,"
	// trailer); the message keeps its identity but must be reachable at its new
	// name. No-op when uid is unknown.
	UpdateFilename(folderID uint64, uid uint32, filename string) error
	// UpdateFlagsMulti replaces flags+keywords for a batch of UIDs in a
	// single lock/reload/flush cycle. Returns the new modseq per UID.
	UpdateFlagsMulti(folderID uint64, updates map[uint32]FlagsUpdate) (map[uint32]uint64, error)
	// SetAltTier sets or clears the AltTier marker (FlagBackend) for every
	// message in folderID whose Filename matches one of the supplied names.
	// Called by the altmove API after physically relocating mdbox m.<N> files
	// so subsequent Fetch calls can skip the primary-tier open() attempt.
	// Operates under the cross-process mailbox lock for the folder.
	SetAltTier(folderID uint64, filenames []string, altTier bool) error
	GetMessages(folderID uint64, uids SeqSet) ([]*MessageMeta, error)
	ExpungeMessage(folderID uint64, uid uint32) error
	NextModSeq(folderID uint64) (uint64, error)
	// Vanished returns UIDs expunged from folderID with modseq strictly
	// greater than sinceModSeq. Drives QRESYNC (RFC 7162) SELECT and
	// UID FETCH (CHANGEDSINCE N VANISHED) responses.
	Vanished(folderID uint64, sinceModSeq uint64) ([]uint32, error)
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
	// ResetFolder atomically replaces the on-disk record set for
	// folderID with the supplied messages. Preserves UIDVALIDITY
	// and the folder GUID; bumps NextUID past max(records.UID);
	// HighestModSeq advances by one so QRESYNC clients invalidate
	// their caches. Drives the admin rebuild flow. Caller has
	// already taken the cross-process mailbox lock and made a
	// .bak of the old base file.
	ResetFolder(folderID uint64, records []*MessageMeta) error
	// OptimizeIndex compacts the .index.log overlay into the base
	// .index file. Returns a no-op nil when there is nothing to
	// compact. Takes the same X lock as a normal write.
	OptimizeIndex(folderID uint64) error
	// FolderVSize returns the folder's cached aggregate virtual size and
	// message count (the hdr-vsize extension). The index-derived source of
	// truth the count quota backend sums across a user's folders.
	FolderVSize(folderID uint64) (bytes uint64, messages uint32, err error)
	// RecomputeVSize forces a rebuild of the folder's vsize aggregate from
	// records and persists it — the admin recovery path for a corrupted count.
	RecomputeVSize(folderID uint64) error
	Close() error
}
