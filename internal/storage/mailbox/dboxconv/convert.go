package dboxconv

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
// the index backend to write.
//
// UIDs are assigned fresh here, as the offline import does. That is wrong for
// the purpose of reading a store in place -- a client that keeps its UIDs
// resynchronises nothing, and one that does not refetches everything -- and it
// is deliberately deferred rather than overlooked (#1568).
func ConvertFolder(folderDir string, c *MapCorrespondence) ([]*mailbox.MessageMeta, error) {
	recs, exts, err := ReadForeignFolder(folderDir)
	if err != nil {
		return nil, err
	}
	mapExt, ok := dboxindex.Find(exts, "mdbox")
	if !ok {
		return nil, fmt.Errorf("dboxconv: folder %s has no mdbox extension, so its messages cannot be located", folderDir)
	}
	guidExt, hasGUID := dboxindex.Find(exts, "guid")

	out := make([]*mailbox.MessageMeta, 0, len(recs))
	for i, r := range recs {
		field, ok := dboxindex.FieldOf(r, mapExt)
		if !ok || len(field) < 8 {
			return nil, fmt.Errorf("dboxconv: folder %s uid %d: mdbox field is %d bytes", folderDir, r.UID, len(field))
		}
		theirMapUID := binary.LittleEndian.Uint32(field)
		saveDate := binary.LittleEndian.Uint32(field[4:])

		ourMapUID, size, err := c.Lookup(theirMapUID)
		if err != nil {
			return nil, fmt.Errorf("dboxconv: folder %s uid %d: %w", folderDir, r.UID, err)
		}

		m := &mailbox.MessageMeta{
			// Fresh, ascending, and not theirs (#1568).
			UID:      uint32(i + 1),
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
	return out, nil
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
