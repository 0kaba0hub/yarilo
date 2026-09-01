package dboxconv

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// systemFlagBits is their flag byte, one bit at a time. Listed rather than
// masked: the bits above these are theirs to define, and a mask would carry
// whatever they add next into our index as a flag nobody set.
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

// ConvertMap makes their map records ours, pointing at the same storage files
// at the same offsets. Nothing is read from or written to the message files:
// the mail stays exactly where it is, which is what makes this conversion in
// place rather than a copy.
//
// One per store, before any folder, because a folder that has not been
// converted yet still addresses its mail through their map uids -- so theirs
// stays on disk until the last folder is done (#1569).
//
// "Only if ours is still empty" is decided inside the map's own lock, together
// with the append. Checked outside it, two sessions opening two different
// folders at once both find an empty map and both import it, and the store ends
// up with every record twice.
//
// Records with a zero refcount are skipped: that is a message waiting for their
// purge, and carrying it over would restore somebody's deleted mail.
func ConvertMap(storageDir string, dst *mdboxmap.Map) (int, error) {
	return dst.ImportOnce(func() ([]mdboxmap.RecordLayout, error) {
		entries, err := ReadForeignMap(storageDir)
		if err != nil {
			return nil, err
		}
		layouts := make([]mdboxmap.RecordLayout, 0, len(entries))
		for _, e := range entries {
			if e.RefCount == 0 {
				continue
			}
			// No GUID: their map does not carry one, and reading it would mean
			// opening every storage file to parse every trailer. Our folder
			// index still gets the right GUID -- it comes from their folder
			// index, which does carry it -- so EMAILID survives; what is left
			// without one is the map's own guid field, which only the storage
			// rebuild pairs by (#1573).
			layouts = append(layouts, mdboxmap.RecordLayout{
				FileID: e.FileID,
				Offset: e.Offset,
				Size:   e.Size,
			})
		}
		return layouts, nil
	})
}

// MapCorrespondence answers the only question a folder conversion asks of the
// two maps: which of our map uids describes the bytes one of their map uids
// describes.
//
// Derived rather than remembered. A record is identified by the file and offset
// it occupies, which neither map invented and neither can disagree about, so
// there is no sidecar to write, to keep in step, or to lose. It works for as
// long as their map is still on disk, which is exactly as long as any folder
// might still need it (#1569).
type MapCorrespondence struct {
	theirs map[uint32]dboxindex.MapEntry // their map uid -> where it points
	ours   map[[2]uint32]uint32          // (file id, offset) -> our map uid
}

// NewMapCorrespondence reads both maps and pairs them by position.
func NewMapCorrespondence(storageDir string, dst *mdboxmap.Map) (*MapCorrespondence, error) {
	entries, err := ReadForeignMap(storageDir)
	if err != nil {
		return nil, err
	}
	c := &MapCorrespondence{
		theirs: make(map[uint32]dboxindex.MapEntry, len(entries)),
		ours:   map[[2]uint32]uint32{},
	}
	for _, e := range entries {
		c.theirs[e.MapUID] = e
	}
	for _, r := range dst.Records() {
		c.ours[[2]uint32{r.FileID, r.Offset}] = r.UID
	}
	return c, nil
}

// Lookup returns our map uid for one of theirs, and the size of the record.
func (c *MapCorrespondence) Lookup(theirUID uint32) (ourUID, size uint32, err error) {
	e, ok := c.theirs[theirUID]
	if !ok {
		return 0, 0, fmt.Errorf("dboxconv: map uid %d is referenced by a folder and their map does not carry it", theirUID)
	}
	our, ok := c.ours[[2]uint32{e.FileID, e.Offset}]
	if !ok {
		return 0, 0, fmt.Errorf("dboxconv: no record of ours at file %d offset %d, where their map uid %d points",
			e.FileID, e.Offset, theirUID)
	}
	return our, e.Size, nil
}

// ConvertFolder turns one of their folders into our message metadata, ready for
// the index backend to write, together with the header state that says which
// UID space it belongs to.
//
// Their UIDs and their UIDVALIDITY are carried across unchanged. That is the
// point of reading a store in place: a client reconnects to a different server
// over the same mailbox and finds its own UIDs, so it resynchronises nothing.
// New UIDs with a new UIDVALIDITY would make every client refetch every
// mailbox, which costs what a migration over IMAP costs (#1568).
//
// RFC 3501 says UIDVALIDITY must change when UIDs are not preserved; here they
// are, so it must not.
func ConvertFolder(folderDir string, c *MapCorrespondence) ([]*mailbox.MessageMeta, dboxindex.HeaderState, error) {
	f, err := ReadForeignFolder(folderDir)
	if err != nil {
		return nil, dboxindex.HeaderState{}, err
	}
	recs, exts := f.Records, f.Exts
	mapExt, ok := dboxindex.Find(exts, "mdbox")
	if !ok {
		return nil, f.Header, fmt.Errorf("dboxconv: folder %s has no mdbox extension, so its messages cannot be located", folderDir)
	}
	guidExt, hasGUID := dboxindex.Find(exts, "guid")

	out := make([]*mailbox.MessageMeta, 0, len(recs))
	for _, r := range recs {
		field, ok := dboxindex.FieldOf(r, mapExt)
		if !ok || len(field) < 8 {
			return nil, f.Header, fmt.Errorf("dboxconv: folder %s uid %d: mdbox field is %d bytes", folderDir, r.UID, len(field))
		}
		theirMapUID := binary.LittleEndian.Uint32(field)
		saveDate := binary.LittleEndian.Uint32(field[4:])

		ourMapUID, size, err := c.Lookup(theirMapUID)
		if err != nil {
			return nil, f.Header, fmt.Errorf("dboxconv: folder %s uid %d: %w", folderDir, r.UID, err)
		}

		m := &mailbox.MessageMeta{
			// Theirs, unchanged.
			UID:      r.UID,
			Filename: strconv.FormatUint(uint64(ourMapUID), 10),
			Flags:    flagNames(r.Flags),
			Keywords: r.Keywords,
			// Their map record's size is the whole record -- header, body and
			// trailer -- so it is not the message size and is not put where one
			// belongs. The index recomputes what it needs on first read.
			Size:         0,
			VSize:        0,
			InternalDate: time.Unix(int64(saveDate), 0).UTC(),
		}
		_ = size
		if hasGUID {
			if raw, ok := dboxindex.FieldOf(r, guidExt); ok && len(raw) == 16 {
				copy(m.GUID[:], raw)
			}
		}
		out = append(out, m)
	}
	return out, f.Header, nil
}

// RemoveForeignFolder unlinks their folder files, and is the last step of a
// conversion rather than part of it: ours has to be written and fsynced first,
// so that a crash leaves a folder one of the two servers can still open. What
// must never exist is a folder with neither.
//
// Their map is not touched here. It is store-wide, and a folder that has not
// been converted yet still needs it (#1569).
func RemoveForeignFolder(dir string) error {
	for _, name := range []string{foreignIndex, foreignLog, foreignLogPrev, foreignCache} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("dboxconv: remove %s: %w", name, err)
		}
	}
	return nil
}

// ConvertSdboxFolder makes their sdbox folder index ours.
//
// No map and no correspondence: a message is a file in the folder's own
// directory, so its position is its path and nothing store-wide has to be whole
// first. What is left is their folder index, which carries the same uids, flags,
// keywords and guids an mdbox one does (#1592).
//
// The file name is read off the disk rather than derived, because two naming
// schemes are in the field and a folder can hold both: "u.<guid>" is what a
// store written with guids has, and "u.<uid>" is the older scheme. Deriving one
// of them would point the index at a file that is not there, and the folder
// would open with every body unreadable -- which is the failure that looks like
// corruption rather than like a wrong guess.
// The uids it could not place are returned rather than counted away: a folder
// whose index names four messages and whose directory holds three is something
// an operator has to be told about, by number and by uid.
func ConvertSdboxFolder(indexDir, mailDir string) ([]*mailbox.MessageMeta, dboxindex.HeaderState, []uint32, error) {
	f, err := ReadForeignFolder(indexDir)
	if err != nil {
		return nil, dboxindex.HeaderState{}, nil, err
	}
	present, err := sdboxFilesPresent(mailDir)
	if err != nil {
		return nil, f.Header, nil, err
	}
	var missing []uint32
	out := make([]*mailbox.MessageMeta, 0, len(f.Records))
	for _, r := range f.Records {
		// No guid here, and none in their index to take: an sdbox folder index
		// of theirs has no guid extension at all -- read off a store the
		// reference wrote, whose extensions are dbox-hdr, hdr-pop3-uidl, cache,
		// vsize and hdr-vsize. The identity lives in the message file, which is
		// what the driver's own scan reads, so the conversion leaves the folder
		// marked guid-pending and the backfill that already runs on every
		// select stamps them from there (#1592).
		//
		// No size and no save date either, for the same reason: their record
		// carries neither. The index recomputes what it needs on first read.
		m := &mailbox.MessageMeta{
			UID:      r.UID,
			Flags:    flagNames(r.Flags),
			Keywords: r.Keywords,
		}
		name, ok := sdboxNameFor(present, r.UID)
		if !ok {
			// A record whose file is gone: an expunge of theirs unlinks the
			// file, and their index can still name it for a moment. Skipped
			// rather than carried, since a record pointing at nothing serves an
			// unreadable message under a uid a client keeps asking for -- and
			// reported, because a folder losing messages silently is the same
			// healthy-looking emptiness this whole path exists to avoid.
			missing = append(missing, r.UID)
			continue
		}
		m.Filename = name
		out = append(out, m)
	}
	return out, f.Header, missing, nil
}

// sdboxFilesPresent is the set of message files in a folder directory, by name.
func sdboxFilesPresent(dir string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("dboxconv: read %s: %w", dir, err)
	}
	out := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), sdboxPrefix) {
			out[e.Name()] = struct{}{}
		}
	}
	return out, nil
}

// sdboxNameFor picks the file this record names.
//
// "u.<uid>" first because that is what the reference writes -- checked against
// a store it produced, where every file is named by uid and not one by guid.
// Our own driver names new files "u.<guidhex>", so a folder converted here and
// then written to holds both spellings and both are looked for.
func sdboxNameFor(present map[string]struct{}, uid uint32) (string, bool) {
	if name := sdboxPrefix + strconv.FormatUint(uint64(uid), 10); nameIn(present, name) {
		return name, true
	}
	return "", false
}

func nameIn(present map[string]struct{}, name string) bool {
	_, ok := present[name]
	return ok
}

// RemoveForeignSdboxFolder unlinks their folder index, the last step of an
// sdbox conversion. The message files are theirs and ours both -- the same
// files, read in place -- so nothing else in the directory is touched.
func RemoveForeignSdboxFolder(dir string) error { return RemoveForeignFolder(dir) }
