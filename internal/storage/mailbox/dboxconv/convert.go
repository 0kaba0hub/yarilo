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

// systemFlagBits is their flag byte, listed rather than masked: a mask would
// carry whatever bit they define next into our index as a flag nobody set.
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

// ConvertMap makes their map records ours, pointing at the same files at the
// same offsets. One per store, before any folder (#1569). "Only if ours is
// empty" is decided inside the map's lock with the append, or two sessions both
// import. A zero refcount awaits their purge: carrying it restores deleted mail.
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
			// No GUID: theirs carries none and reading it means opening every
			// storage file. EMAILID comes from their folder index; only the
			// storage rebuild pairs by the map's own guid (#1573).
			layouts = append(layouts, mdboxmap.RecordLayout{
				FileID: e.FileID,
				Offset: e.Offset,
				Size:   e.Size,
			})
		}
		return layouts, nil
	})
}

// MapCorrespondence answers which of our map uids describes the bytes one of
// theirs does, derived from file id and offset so there is no sidecar (#1569).
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

// ConvertFolder turns one of their folders into our message metadata. Their
// UIDs and UIDVALIDITY carry across unchanged, so a client resynchronises
// nothing (#1568).
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
			// Their size is the whole record, header and trailer included, so
			// it is not the message size. The index recomputes on first read.
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

// RemoveForeignFolder unlinks their folder files, last: ours is fsynced first,
// so a crash leaves a folder one of the two servers can still open (#1569).
func RemoveForeignFolder(dir string) error {
	for _, name := range []string{foreignIndex, foreignLog, foreignLogPrev, foreignCache} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("dboxconv: remove %s: %w", name, err)
		}
	}
	return nil
}

// ConvertSdboxFolder makes their sdbox folder index ours. No map: a message's
// position is its path (#1592). The file name is read off the disk, not
// derived, or every body reads as corruption; unplaceable uids are returned.
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
		// Their sdbox index has no guid extension, size or save date: identity
		// lives in the message file, so the folder stays guid-pending and the
		// select-time backfill stamps it (#1592).
		m := &mailbox.MessageMeta{
			UID:      r.UID,
			Flags:    flagNames(r.Flags),
			Keywords: r.Keywords,
		}
		name, ok := sdboxNameFor(present, r.UID)
		if !ok {
			// Their expunge unlinks the file while their index may still name
			// it. Skipped rather than serving an unreadable uid, and reported.
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

// sdboxNameFor picks the file this record names. They write "u.<uid>"; our
// driver writes "u.<guidhex>", so a converted folder holds both spellings.
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

// RemoveForeignSdboxFolder unlinks their folder index, last. The message files
// are read in place by both, so nothing else in the directory is touched.
func RemoveForeignSdboxFolder(dir string) error { return RemoveForeignFolder(dir) }
