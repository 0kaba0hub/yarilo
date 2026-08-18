package jmap

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The two object types get their own state, because they change for different
// reasons: renaming a mailbox touches no message, and marking a message read
// touches no mailbox except its counts. One state for both would send every
// client to refetch everything on every change.

// emailStateFields are the per-folder marker for the Email state, in the order
// they are encoded.
//
// highestModSeq says that something changed. nextUID is what says WHAT KIND:
// at diff time a UID at or above the previous nextUID did not exist then, so it
// is a creation, while a lower one with a newer modseq is an update. Without it
// the two are indistinguishable and every change would have to be reported as
// an update -- true, and useless, because the client would refetch each one as
// though it were new.
func emailStateFields(f *mailbox.Folder) []uint64 {
	return []uint64{uint64(f.UIDValidity), f.HighestModSeq, uint64(f.NextUID)}
}

// emailState describes the account for Email/get, Email/set and, later,
// Email/changes.
func (s *Server) emailState(h *userHandle) (string, error) {
	entries, err := h.box.ListFolders()
	if err != nil {
		return "", fmt.Errorf("jmap: list folders: %w", err)
	}
	desc := jmapcore.Description{Kind: jmapcore.KindEmail}
	for _, e := range entries {
		if !e.Selectable {
			continue
		}
		f, err := h.idx.OpenFolder(e.Name, 0)
		if err != nil {
			return "", fmt.Errorf("jmap: open folder %q: %w", e.Name, err)
		}
		desc.Entries = append(desc.Entries, jmapcore.StateEntry{
			Key: stateKey(f.GUID), Fields: emailStateFields(f),
		})
	}
	return desc.String(), nil
}

// mailboxState describes the mailbox set. The marker is a digest of the
// properties a client can see, so a rename, a subscribe or an arriving message
// (through the counts, which are Mailbox properties) all move it.
func mailboxState(list []jmapcore.Mailbox) string {
	desc := jmapcore.Description{Kind: jmapcore.KindMailbox}
	for _, mb := range list {
		desc.Entries = append(desc.Entries, jmapcore.StateEntry{
			Key: mailboxKeyOf(mb.ID), Fields: []uint64{mailboxDigest(mb)},
		})
	}
	return desc.String()
}

// mailboxDigest folds one mailbox's visible properties into 8 bytes.
func mailboxDigest(mb jmapcore.Mailbox) uint64 {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\x00%t\x00", mb.ID, mb.Name,
		mb.TotalEmails, mb.UnreadEmails, mb.IsSubscribed)
	if mb.Role != nil {
		fmt.Fprintf(h, "%s", *mb.Role)
	}
	h.Write([]byte{0}) //nolint:errcheck // hash writes cannot fail
	if mb.ParentID != nil {
		fmt.Fprintf(h, "%s", *mb.ParentID)
	}
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}

// stateKey is the folder's identity in a description: the first 8 bytes of its
// GUID, which survives a rename as the name does not.
func stateKey(guid [16]byte) [8]byte {
	var k [8]byte
	copy(k[:], guid[:8])
	return k
}

// mailboxKeyOf derives the same key from the client-facing id, which is the
// formatted GUID. A container mailbox has no GUID of its own, so its id is
// hashed instead -- it still has to appear, or its disappearance would go
// unreported.
func mailboxKeyOf(id string) [8]byte {
	var k [8]byte
	if raw, err := hex.DecodeString(id); err == nil && len(raw) == 16 {
		copy(k[:], raw[:8])
		return k
	}
	// A \NoSelect container has no GUID of its own, so its id is hashed
	// instead. It still needs an entry, or its disappearance would never be
	// reported.
	sum := sha256.Sum256([]byte(id))
	copy(k[:], sum[:8])
	return k
}
