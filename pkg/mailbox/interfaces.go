package mailbox

import (
	"io"
	"time"
)

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
}

// SeqSet is a set of UIDs or sequence numbers (use UID=0 for seq).
type SeqSet []SeqRange

type SeqRange struct {
	From, To uint32 // inclusive; To==0 means '*'
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
// at creation time. Mirrors Dovecot's struct mail_storage (mail-storage-private.h:138).
//
// Init MUST be called before any other method — it creates the on-disk directory
// structure. Callers that open a handle but never call Init will see errors from
// the underlying filesystem operations.
//
// AppendUIDEntry records the uid→filename mapping used by the Maildir uidlist
// (yarilo-uidlist v3). Backends that do not need a separate uidlist (dbox, mdbox)
// implement this as a no-op — UIDs are managed exclusively by UserIndex.
//
// Close releases any open file descriptors held by the handle.
type UserMailbox interface {
	Init() error
	Create(folder string) error
	Delete(folder string) error
	Rename(oldName, newName string) error
	Save(folder string, r io.Reader, size int64, flags []string) (string, error)
	Fetch(folder, filename string) (io.ReadCloser, error)
	Remove(folder, filename string) error
	List(folder string) ([]*MessageMeta, error)
	FolderExists(folder string) (bool, error)
	ListFolders() ([]string, error)
	AppendUIDEntry(folder string, uid uint32, filename string) error
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
	UpdateFlags(folderID uint64, uid uint32, flags, keywords []string) error
	GetMessages(folderID uint64, uids SeqSet) ([]*MessageMeta, error)
	ExpungeMessage(folderID uint64, uid uint32) error
	NextModSeq(folderID uint64) (uint64, error)
	Keywords(folderID uint64) ([]string, error)
	// RenameFolder renames oldName to newName in the index.
	// Called by IMAP RENAME immediately after UserMailbox.Rename succeeds.
	RenameFolder(oldName, newName string) error
	// GetPOP3UIDLs loads saved POP3 UIDLs for a folder (uid → uidl string).
	// Returns an empty map when no saved UIDLs exist yet.
	GetPOP3UIDLs(folderID uint64) (map[uint32]string, error)
	// SavePOP3UIDLs persists POP3 UIDLs so subsequent sessions use stable values.
	SavePOP3UIDLs(folderID uint64, uidls map[uint32]string) error
	Close() error
}
