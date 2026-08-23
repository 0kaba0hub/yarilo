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
	gen, pos := h.threadMark()
	desc := jmapcore.Description{Kind: jmapcore.KindEmail, Extra: []uint64{gen, pos}}
	for _, e := range entries {
		if !e.Selectable {
			continue
		}
		// Two stats instead of a base read and a log replay, when nothing has
		// moved since the marker was built. A mismatch -- or a stamp that
		// cannot be taken -- falls through to the open.
		stamp, stampErr := h.idx.FolderStamp(e.Name)
		if stampErr == nil {
			if mark, ok := s.states.get(h.info.Username, e.Name, stamp); ok {
				desc.Entries = append(desc.Entries, jmapcore.StateEntry{Key: mark.key, Fields: mark.fields})
				continue
			}
		}
		f, err := h.idx.OpenFolder(e.Name, 0)
		if err != nil {
			return "", fmt.Errorf("jmap: open folder %q: %w", e.Name, err)
		}
		mark := cachedMark{stamp: stamp, key: stateKey(f.GUID), fields: emailStateFields(f)}
		if stampErr == nil {
			// Stamped AFTER the read would be wrong: a write landing between
			// the two would be covered by a stamp that predates it. Taken
			// before, the worst case is an entry that looks stale and is
			// recomputed.
			s.states.put(h.info.Username, e.Name, mark)
		}
		desc.Entries = append(desc.Entries, jmapcore.StateEntry{Key: mark.key, Fields: mark.fields})
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
			Key: mailboxKeyOf(mb.ID), Fields: mailboxFields(mb),
		})
	}
	return desc.String()
}

// mailboxFields carries the digest and, when the id is a GUID, the rest of it.
//
// The second half is there so a DELETED mailbox can still be named: it is gone
// from the account by the time the diff runs, so its id cannot be looked up
// anywhere, and a destroyed list that cannot name what it destroyed is no use
// to a client. A container mailbox has no GUID -- its id is its path -- and
// carries the digest alone; Mailbox/changes refuses rather than inventing an id
// for one of those.
func mailboxFields(mb jmapcore.Mailbox) []uint64 {
	fields := []uint64{mailboxDigest(mb)}
	if raw, err := hex.DecodeString(mb.ID); err == nil && len(raw) == 16 {
		fields = append(fields, binary.BigEndian.Uint64(raw[8:]))
	}
	return fields
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

// folderMark is one folder as the state describes it, kept together with the
// handle so a diff does not have to open it a second time.
type folderMark struct {
	key    [8]byte
	name   string
	folder *mailbox.Folder
}

type folderMarks []folderMark

func (fm folderMarks) description(threadGen, threadPos uint64) jmapcore.Description {
	desc := jmapcore.Description{Kind: jmapcore.KindEmail, Extra: []uint64{threadGen, threadPos}}
	for _, f := range fm {
		desc.Entries = append(desc.Entries, jmapcore.StateEntry{
			Key: f.key, Fields: emailStateFields(f.folder),
		})
	}
	return desc
}

// folderMarks opens every selectable folder once and reads what the Email state
// is made of.
func (s *Server) folderMarks(h *userHandle) (folderMarks, error) {
	entries, err := h.box.ListFolders()
	if err != nil {
		return nil, fmt.Errorf("jmap: list folders: %w", err)
	}
	out := make(folderMarks, 0, len(entries))
	for _, e := range entries {
		if !e.Selectable {
			continue
		}
		f, err := h.idx.OpenFolder(e.Name, 0)
		if err != nil {
			return nil, fmt.Errorf("jmap: open folder %q: %w", e.Name, err)
		}
		out = append(out, folderMark{key: stateKey(f.GUID), name: e.Name, folder: f})
	}
	return out, nil
}

// changedIDs splits a folder's messages the way JMAP asks for them.
type changedIDs struct{ created, updated []string }

// folderMessageIDs reads one folder and sorts its messages into created and
// updated relative to a previous marker.
//
// The split rests on nextUID: a UID at or above the one the client last saw did
// not exist then, so the message is new; a lower one whose modseq has moved is
// an update. Without that field both would have to be reported as updates, and
// a client would refetch every changed message as though it had never seen it.
func (s *Server) folderMessageIDs(h *userHandle, f folderMark, sinceModSeq uint64, sinceNextUID uint32) (changedIDs, error) {
	metas, err := mailbox.ReadMessages(h.idx, f.folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		return changedIDs{}, fmt.Errorf("jmap: read folder %q: %w", f.name, err)
	}
	var out changedIDs
	for _, m := range metas {
		switch {
		case m.UID >= sinceNextUID:
			out.created = append(out.created, emailID(m))
		case m.ModSeq > sinceModSeq:
			out.updated = append(out.updated, emailID(m))
		}
	}
	return out, nil
}

// destroyedMailboxID rebuilds the id of a mailbox that is gone, from the two
// halves the description kept. ok is false for a container, whose id is its
// path and cannot be reconstructed -- the caller refuses the call rather than
// reporting an id it made up.
func destroyedMailboxID(key [8]byte, fields []uint64) (string, bool) {
	if len(fields) < 2 {
		return "", false
	}
	raw := make([]byte, 16)
	copy(raw, key[:])
	binary.BigEndian.PutUint64(raw[8:], fields[1])
	return hex.EncodeToString(raw), true
}
