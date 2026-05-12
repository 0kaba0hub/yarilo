package mailbox

import (
	"io"
	"time"
)

// MessageMeta holds per-message metadata stored in the index.
type MessageMeta struct {
	UID          uint32
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

// MailboxBackend is the storage interface for raw message bytes.
type MailboxBackend interface {
	// Init ensures directory structure exists for user.
	Init(user string) error

	// Create creates a new folder.
	Create(user, folder string) error

	// Delete removes a folder and all its messages.
	Delete(user, folder string) error

	// Save writes a message into the folder's tmp → cur flow.
	// Returns the assigned filename (backend-specific).
	Save(user, folder string, r io.Reader, size int64, flags []string) (string, error)

	// Fetch returns a reader for the raw RFC 5322 message bytes.
	Fetch(user, folder, filename string) (io.ReadCloser, error)

	// Remove deletes a message by filename.
	Remove(user, folder, filename string) error

	// List returns all messages in a folder.
	List(user, folder string) ([]*MessageMeta, error)

	// FolderExists reports whether the folder exists.
	FolderExists(user, folder string) (bool, error)

	// ListFolders returns all folder names for a user.
	ListFolders(user string) ([]string, error)
}

// IndexBackend is the storage interface for IMAP index data.
type IndexBackend interface {
	// OpenFolder opens or creates the index for a folder.
	OpenFolder(user, folder string, uidValidity uint32) (*Folder, error)

	// SaveFolder persists updated folder metadata.
	SaveFolder(user string, f *Folder) error

	// AppendMessage adds a new message record to the folder index.
	AppendMessage(folderID uint64, m *MessageMeta) error

	// UpdateFlags updates the flags for a single message.
	UpdateFlags(folderID uint64, uid uint32, flags, keywords []string) error

	// GetMessages returns messages matching the SeqSet (by UID).
	GetMessages(folderID uint64, uids SeqSet) ([]*MessageMeta, error)

	// ExpungeMessage removes a message record from the index.
	ExpungeMessage(folderID uint64, uid uint32) error

	// NextModSeq increments and returns the next modseq for the folder.
	NextModSeq(folderID uint64) (uint64, error)

	// Close releases resources.
	Close() error
}
