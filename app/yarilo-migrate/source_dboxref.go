package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
)

// dboxRefWalker reads an mdbox store another dbox v2 implementation wrote.
//
// Two branches, and the difference between them is what an operator gets:
//
//   - with the folder indexes, flags and keywords come across;
//   - without them, only what the stored records carry, which is bodies and
//     the folder each was first saved to.
//
// UIDs are not carried in either case. The messages go through the destination
// driver's own save path, so they are allocated fresh, and UIDVALIDITY with
// them -- the same as every other conversion this tool does.
type dboxRefWalker struct{}

// systemFlagBits is the five flags an IMAP client knows.
//
// The reference keeps its own bits in the same byte: 0x20 is session state,
// 0x40 is MAIL_INDEX_MAIL_FLAG_BACKEND and 0x80 is DIRTY. Copying the byte
// whole would carry those across as flags nobody set, on a path where nothing
// would notice -- they are not IMAP flags, so no client asks for them and no
// comparison of flag names sees them.
//
// What keeps them out is this list and not a mask. There was a 0x1f mask here
// as well; a mutation removing it passed, because every bit named below is
// already inside it. Two guards where one does the work is one guard nobody
// maintains -- and it would be the mask that looked responsible while the list
// did the job.
var systemFlagBits = []struct {
	bit  uint8
	name string
}{
	{0x01, `\Answered`},
	{0x02, `\Flagged`},
	{0x04, `\Deleted`},
	{0x08, `\Seen`},
	{0x10, `\Draft`},
}

func flagNames(b uint8) []string {
	var out []string
	for _, f := range systemFlagBits {
		if b&f.bit != 0 {
			out = append(out, f.name)
		}
	}
	return out
}

func (dboxRefWalker) Walk(home string, visit func(sourceMessage) error) error {
	root := filepath.Join(home, "mdbox")
	storage := filepath.Join(root, "storage")

	entries, err := readReferenceMap(storage)
	if err != nil {
		return err
	}
	byMapUID := make(map[uint32]dboxindex.MapEntry, len(entries))
	for _, e := range entries {
		byMapUID[e.MapUID] = e
	}

	mailboxes := filepath.Join(root, "mailboxes")
	folders, err := dboxindex.WalkFolders(os.DirFS(mailboxes))
	if err != nil {
		return fmt.Errorf("dbox-ref: walk folders under %s: %w", mailboxes, err)
	}

	open := map[uint32]*os.File{}
	defer func() {
		for _, f := range open {
			_ = f.Close()
		}
	}()

	for _, folder := range folders {
		recs, exts, ferr := readReferenceFolder(filepath.Join(mailboxes, folder.Path, "dbox-Mails"))
		if ferr != nil {
			return ferr
		}
		for _, r := range recs {
			msg, merr := buildMessage(folder.Name, r, exts, byMapUID, storage, open)
			if merr != nil {
				return merr
			}
			if msg == nil {
				continue
			}
			if err := visit(*msg); err != nil {
				return err
			}
		}
	}
	return nil
}

// readReferenceMap reads the store's map, from its base when it has one and
// from its log in any case.
func readReferenceMap(storage string) ([]dboxindex.MapEntry, error) {
	var seed []dboxindex.Extension
	if raw, err := os.ReadFile(filepath.Join(storage, "dovecot.map.index")); err == nil {
		h, herr := dboxindex.ParseHeader(raw)
		if herr != nil {
			return nil, fmt.Errorf("dbox-ref: map index: %w", herr)
		}
		seed, herr = dboxindex.ParseExtensions(raw, h)
		if herr != nil {
			return nil, fmt.Errorf("dbox-ref: map extensions: %w", herr)
		}
	}

	raw, err := os.ReadFile(filepath.Join(storage, "dovecot.map.index.log"))
	if err != nil {
		return nil, fmt.Errorf("dbox-ref: map log: %w", err)
	}
	lh, err := dboxindex.ParseLogHeader(raw)
	if err != nil {
		return nil, fmt.Errorf("dbox-ref: map log header: %w", err)
	}
	return dboxindex.ReadMap(raw, int(lh.HeaderSize), seed)
}

// readReferenceFolder returns the folder's messages, with flags and keywords.
//
// A missing index is an error, and deliberately so. The second branch of
// #1524 -- scanning the stored records and placing each message by the folder
// its trailer names -- is not written yet, and until it is, a folder without an
// index has to stop the import rather than come back empty. Empty is the one
// answer nobody checks: the folder appears, the message count is zero, and the
// mail is gone with nothing in the output saying so.
func readReferenceFolder(dir string) ([]dboxindex.Record, []dboxindex.Extension, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "dovecot.index"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("dbox-ref: %s has no index; importing a folder without one is not available yet, and importing it as empty would lose its mail silently", dir)
		}
		return nil, nil, fmt.Errorf("dbox-ref: read index %s: %w", dir, err)
	}
	h, err := dboxindex.ParseHeader(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("dbox-ref: index %s: %w", dir, err)
	}
	recs, err := dboxindex.ParseRecords(raw, h)
	if err != nil {
		return nil, nil, fmt.Errorf("dbox-ref: records %s: %w", dir, err)
	}
	exts, err := dboxindex.ParseExtensions(raw, h)
	if err != nil {
		return nil, nil, fmt.Errorf("dbox-ref: extensions %s: %w", dir, err)
	}

	var names []string
	if kw, ok := dboxindex.Find(exts, "keywords"); ok {
		names, err = dboxindex.KeywordNames(kw)
		if err != nil {
			return nil, nil, fmt.Errorf("dbox-ref: keywords %s: %w", dir, err)
		}
		for i := range recs {
			recs[i].Keywords = dboxindex.KeywordsOf(recs[i].Raw, kw, names)
		}
	}
	if tail, terr := os.ReadFile(filepath.Join(dir, "dovecot.index.log")); terr == nil {
		changes, cerr := dboxindex.ReadChanges(tail, int(h.LogFileTailOffset), exts)
		if cerr != nil {
			return nil, nil, fmt.Errorf("dbox-ref: log %s: %w", dir, cerr)
		}
		recs = dboxindex.Apply(recs, changes, names)
	}
	return recs, exts, nil
}

// buildMessage turns one index record into a message, reading its body through
// the map.
func buildMessage(folder string, r dboxindex.Record, exts []dboxindex.Extension,
	byMapUID map[uint32]dboxindex.MapEntry, storage string, open map[uint32]*os.File) (*sourceMessage, error) {

	mdbox, ok := dboxindex.Find(exts, "mdbox")
	if !ok {
		return nil, fmt.Errorf("dbox-ref: folder %s uid %d: no mdbox extension, so its bytes cannot be found", folder, r.UID)
	}
	field, ok := dboxindex.FieldOf(r, mdbox)
	if !ok || len(field) < 8 {
		return nil, fmt.Errorf("dbox-ref: folder %s uid %d: mdbox field is %d bytes", folder, r.UID, len(field))
	}
	mapUID := binary.LittleEndian.Uint32(field)
	saveDate := binary.LittleEndian.Uint32(field[4:])

	entry, ok := byMapUID[mapUID]
	if !ok {
		return nil, fmt.Errorf("dbox-ref: folder %s uid %d references map uid %d, which the map does not carry", folder, r.UID, mapUID)
	}

	f, ok := open[entry.FileID]
	if !ok {
		path := filepath.Join(storage, fmt.Sprintf("m.%d", entry.FileID))
		var err error
		if f, err = os.Open(path); err != nil {
			return nil, fmt.Errorf("dbox-ref: open %s: %w", path, err)
		}
		open[entry.FileID] = f
	}
	hdrSize, err := dboxv2.FileHeaderSize(f)
	if err != nil {
		return nil, fmt.Errorf("dbox-ref: m.%d: %w", entry.FileID, err)
	}
	body, err := dboxv2.ReadRecordBodyAt(f, int64(entry.Offset), hdrSize)
	if err != nil {
		return nil, fmt.Errorf("dbox-ref: folder %s uid %d: %w", folder, r.UID, err)
	}

	msg := &sourceMessage{
		Folder:       folder,
		Body:         body,
		Flags:        append(flagNames(r.Flags), r.Keywords...),
		InternalDate: time.Unix(int64(saveDate), 0).UTC(),
	}
	if g, ok := dboxindex.Find(exts, "guid"); ok {
		if raw, ok := dboxindex.FieldOf(r, g); ok && len(raw) == 16 {
			copy(msg.GUID[:], raw)
		}
	}
	return msg, nil
}
