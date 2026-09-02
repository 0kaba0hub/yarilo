package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mboxenc"
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
type dboxRefWalker struct {
	// Stats is filled as the walk runs, so the summary can say how each
	// message arrived. Without it the two branches are indistinguishable in
	// the output, and an operator who lost every flag in the account has no
	// way to know it happened.
	Stats *ImportStats
}

// ImportStats counts how the messages were found.
type ImportStats struct {
	// FromIndex is messages read through a folder index: flags and keywords
	// carried. FromRecords is messages recovered by scanning the store:
	// neither carried, and placed by the folder the record names.
	FromIndex   int
	FromRecords int

	FoldersIndexed int
	FoldersScanned int
}

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

// Folders lists what the store holds, so an empty one is carried across too.
func (w dboxRefWalker) Folders(home string) ([]string, error) {
	mailboxes := filepath.Join(home, "mdbox", "mailboxes")
	folders, err := dboxindex.WalkFolders(os.DirFS(mailboxes))
	if err != nil {
		return nil, fmt.Errorf("dbox-ref: walk folders under %s: %w", mailboxes, err)
	}
	out := make([]string, 0, len(folders))
	for _, f := range folders {
		out = append(out, f.Name)
	}
	return out, nil
}

func (w dboxRefWalker) Walk(home string, visit func(sourceMessage) error) error {
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

	// Which messages the index branch has already delivered, by map uid. The
	// scan below covers what it did not, and without this a folder that was
	// read from its index would have its messages delivered twice: once from
	// there and once from the store.
	done := map[uint32]bool{}

	for _, folder := range folders {
		dir := filepath.Join(mailboxes, folder.Path, "dbox-Mails")
		recs, exts, ferr := readReferenceFolder(dir)
		if ferr != nil {
			if !errors.Is(ferr, errNoIndex) {
				return ferr
			}
			// The second branch. This folder has no index, so its messages are
			// found by scanning the store instead: without flags, without
			// keywords, and placed by the folder their record names -- which is
			// where each was first saved, not necessarily where it is now.
			w.count(func(s *ImportStats) { s.FoldersScanned++ })
			continue
		}
		w.count(func(s *ImportStats) { s.FoldersIndexed++ })

		for _, r := range recs {
			msg, mapUID, merr := buildMessage(folder.Name, r, exts, byMapUID, storage, open)
			if merr != nil {
				return merr
			}
			if err := visit(*msg); err != nil {
				return err
			}
			done[mapUID] = true
			w.count(func(s *ImportStats) { s.FromIndex++ })
		}
	}

	return w.scanStore(storage, byMapUID, done, visit)
}

// scanStore delivers what the index branch did not: every referenced record the
// folders did not account for.
//
// mdbox only. The placement comes from the B trailer key, and only mdbox writes
// it -- sdbox passes nothing there, because a single-message file
// already sits inside its folder's directory and needs no hint. So a store of
// that shape has nothing to recover from here, and does not need this branch:
// its folder is its path.
//
// The record names the folder it was first saved to, and that is the only
// placement available here. A message moved since then arrives where it
// started, which is why this is a recovery path and not a migration.
func (w dboxRefWalker) scanStore(storage string, byMapUID map[uint32]dboxindex.MapEntry,
	done map[uint32]bool, visit func(sourceMessage) error) error {

	// Offsets of the records still wanted, per file.
	wanted := map[uint32]map[int64]uint32{}
	for uid, e := range byMapUID {
		if done[uid] || e.RefCount == 0 {
			// Already delivered, or referenced by nothing: a zero refcount is
			// a message waiting for a purge, and restoring it would undelete
			// somebody's mail.
			continue
		}
		if wanted[e.FileID] == nil {
			wanted[e.FileID] = map[int64]uint32{}
		}
		wanted[e.FileID][int64(e.Offset)] = uid
	}
	if len(wanted) == 0 {
		return nil
	}

	fileIDs := make([]uint32, 0, len(wanted))
	for id := range wanted {
		fileIDs = append(fileIDs, id)
	}
	sort.Slice(fileIDs, func(i, j int) bool { return fileIDs[i] < fileIDs[j] })

	for _, id := range fileIDs {
		path := filepath.Join(storage, fmt.Sprintf("m.%d", id))
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("dbox-ref: open %s: %w", path, err)
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("dbox-ref: stat %s: %w", path, err)
		}
		err = dboxv2.WalkRecords(f, info.Size(), func(rec dboxv2.StoredRecord) error {
			if _, want := wanted[id][rec.Offset]; !want {
				return nil
			}
			folder, ferr := folderFromRecord(rec)
			if ferr != nil {
				return fmt.Errorf("dbox-ref: %s offset %d: %w", path, rec.Offset, ferr)
			}
			if folder == "" {
				// Nothing says where it belongs. INBOX is a guess, and a guess
				// is worse than a refusal here: the message would land in a
				// folder it was never in and nobody would know which ones.
				//
				// sdbox writes no B at all, so a store of that shape never
				// reaches this branch usefully -- see the note on scanStore.
				return fmt.Errorf("dbox-ref: %s offset %d: the record names no folder, so it cannot be placed", path, rec.Offset)
			}
			w.count(func(s *ImportStats) { s.FromRecords++ })
			return visit(sourceMessage{
				Folder:       folder,
				Body:         rec.Body,
				InternalDate: rec.Received,
				GUID:         rec.GUID,
			})
		})
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// count applies f to the stats when the caller asked for them.
func (w dboxRefWalker) count(f func(*ImportStats)) {
	if w.Stats != nil {
		f(w.Stats)
	}
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

// errNoIndex says a folder has no index of its own, which sends its messages
// down the scanning branch rather than stopping the import.
var errNoIndex = errors.New("no index; its messages are recovered from the store, without flags or keywords")

// readReferenceFolder returns the folder's messages, with flags and keywords.
//
// Two ways in, and which one applies is decided by what is on disk rather than
// by what is missing:
//
//	base present            -> the base, then its log from tail_offset
//	no base, log present    -> the log from its start
//	neither                 -> errNoIndex, and the caller scans the store
//
// The middle case is not an edge: the reference writes a base only once a size
// threshold or a rotation forces it, so a folder written a moment ago has its
// whole state -- appends, flags, keywords -- in the log and no base at all.
// Reading that as "no index" loses every flag in the folder while reporting
// only that the folder was recovered, and on a freshly created store it loses
// every flag in the account (#1564). The map reader already had this rule; the
// two now say the same thing.
//
// A missing index with no log is still an error to this function rather than an
// empty folder. Empty is the one answer nobody checks: the folder appears, the
// message count is zero, and the mail is gone with nothing in the output saying
// so. The caller turns it into the scan.
func readReferenceFolder(dir string) ([]dboxindex.Record, []dboxindex.Extension, error) {
	logPath := filepath.Join(dir, "dovecot.index.log")
	raw, err := os.ReadFile(filepath.Join(dir, "dovecot.index"))
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("dbox-ref: read index %s: %w", dir, err)
		}
		tail, terr := os.ReadFile(logPath)
		if terr != nil {
			if os.IsNotExist(terr) {
				return nil, nil, fmt.Errorf("dbox-ref: %s: %w", dir, errNoIndex)
			}
			return nil, nil, fmt.Errorf("dbox-ref: read log %s: %w", dir, terr)
		}
		return readFolderFromLog(dir, tail)
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
	if tail, terr := os.ReadFile(logPath); terr == nil {
		changes, cerr := dboxindex.ReadChanges(tail, int(h.LogFileTailOffset), exts)
		if cerr != nil {
			return nil, nil, fmt.Errorf("dbox-ref: log %s: %w", dir, cerr)
		}
		recs = dboxindex.Apply(recs, changes, names)
	}
	return recs, exts, nil
}

// readFolderFromLog builds the folder out of its log alone, for a folder whose
// base index has not been written yet.
//
// The extensions come from the log's own intro records rather than from a base
// that does not exist. That is what makes the messages readable at all: an
// mdbox message is found through the map uid its extension carries, and with no
// name for that extension there is nothing to look it up by.
func readFolderFromLog(dir string, tail []byte) ([]dboxindex.Record, []dboxindex.Extension, error) {
	h, err := dboxindex.ParseLogHeader(tail)
	if err != nil {
		return nil, nil, fmt.Errorf("dbox-ref: log %s: %w", dir, err)
	}
	changes, exts, err := dboxindex.ReadChangesAndExtensions(tail, int(h.HeaderSize), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("dbox-ref: log %s: %w", dir, err)
	}
	// No keyword table: the base holds one and there is no base. A keyword set
	// in the log names itself, so those arrive; one carried as a bitmask alone
	// has nothing to resolve against and does not.
	return dboxindex.Apply(nil, changes, nil), exts, nil
}

// buildMessage turns one index record into a message, reading its body through
// the map.
func buildMessage(folder string, r dboxindex.Record, exts []dboxindex.Extension,
	byMapUID map[uint32]dboxindex.MapEntry, storage string, open map[uint32]*os.File) (*sourceMessage, uint32, error) {

	mdbox, ok := dboxindex.Find(exts, "mdbox")
	if !ok {
		return nil, 0, fmt.Errorf("dbox-ref: folder %s uid %d: no mdbox extension, so its bytes cannot be found", folder, r.UID)
	}
	field, ok := dboxindex.FieldOf(r, mdbox)
	if !ok || len(field) < 8 {
		return nil, 0, fmt.Errorf("dbox-ref: folder %s uid %d: mdbox field is %d bytes", folder, r.UID, len(field))
	}
	mapUID := binary.LittleEndian.Uint32(field)
	saveDate := binary.LittleEndian.Uint32(field[4:])

	entry, ok := byMapUID[mapUID]
	if !ok {
		return nil, 0, fmt.Errorf("dbox-ref: folder %s uid %d references map uid %d, which the map does not carry", folder, r.UID, mapUID)
	}

	f, ok := open[entry.FileID]
	if !ok {
		path := filepath.Join(storage, fmt.Sprintf("m.%d", entry.FileID))
		var err error
		if f, err = os.Open(path); err != nil {
			return nil, 0, fmt.Errorf("dbox-ref: open %s: %w", path, err)
		}
		open[entry.FileID] = f
	}
	hdrSize, err := dboxv2.FileHeaderSize(f)
	if err != nil {
		return nil, 0, fmt.Errorf("dbox-ref: m.%d: %w", entry.FileID, err)
	}
	body, err := dboxv2.ReadRecordBodyAt(f, int64(entry.Offset), hdrSize)
	if err != nil {
		return nil, 0, fmt.Errorf("dbox-ref: folder %s uid %d: %w", folder, r.UID, err)
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
	return msg, mapUID, nil
}

// folderFromRecord is the folder a stored record belongs to, as a client would
// name it.
//
// B is the storage name and not the one a client sees: the reference writes
// box->name, which is modified UTF-7 for any folder whose name is not plain
// ASCII. Used raw, a message from "Вхідні/Робота" is delivered into a folder
// literally called "&BBIENQQ0BDwEPQVW-/&BCAEPgQxBD4EQgQw-" -- found, no error,
// and not where the user had it. The same decoding the folder walk applies to
// names on disk applies here.
func folderFromRecord(rec dboxv2.StoredRecord) (string, error) {
	if rec.OrigMailbox == "" {
		return "", nil
	}
	name, err := mboxenc.FromModUTF7(rec.OrigMailbox)
	if err != nil {
		return "", fmt.Errorf("folder name %q does not decode: %w", rec.OrigMailbox, err)
	}
	return name, nil
}
