package dboxindex

import (
	"encoding/binary"
	"fmt"
)

// MapEntry says where one message's bytes are.
//
// FileID names the m.<N> the record sits in, Offset where it starts, and Size
// how far it runs -- pre and post metadata included. Size 0 means the record is
// the last in its file and may be any length, which is how the reference stores
// a message larger than four gigabytes.
type MapEntry struct {
	MapUID   uint32
	FileID   uint32
	Offset   uint32
	Size     uint32
	RefCount uint16
}

// Record types this reader needs beyond appends and expunges.
const (
	typeExtIntro     = 0x00000040
	typeExtRecUpdate = 0x00000200
	typeExtAtomicInc = 0x00001000
)

const (
	extIntroFixed = 20 // up to and including name_size
	mapRecordSize = 12 // file_id, offset, size
)

// ReadMap reconstructs the mdbox map from a transaction log.
//
// base is the extension table of the map's own index, or nil when there is
// none. It matters after a rotation: the intros in the new log refer to
// extensions by the id the base holds, having been introduced by name in a file
// that no longer exists. Without the base those ids resolve to nothing, and the
// records that follow belong to no extension -- a map that comes back empty
// with no error at all, which is the worst answer this reader could give.
//
// From the log alone, and that is not a shortcut: this store has no
// dovecot.map.index at all, because the base is only written once enough log
// has been read since the last rewrite. A map with a base is read the same way
// with the base's records as the starting point -- the same shape as a folder,
// and not covered by any fixture we hold.
//
// The extension the following records belong to is stated by an ext-intro and
// holds until the next one, so the records are order-dependent in a way the
// folder log's appends are not. A reader that ignored the intros would apply
// the refcount extension's bytes as if they were map records.
func ReadMap(b []byte, offset int, base []Extension) ([]MapEntry, error) {
	be, le := binary.BigEndian, binary.LittleEndian

	entries := map[uint32]*MapEntry{}
	var order []uint32
	// Which extension the records that follow belong to.
	//
	// An intro either names a new extension -- and the index gives it the next
	// id -- or carries an id and an empty name, meaning one it already knows.
	// Both forms appear in one log: the map and the ref extensions are
	// introduced by name once, and every transaction after that refers to them
	// as 0 and 1. A reader that only understood the named form would attribute
	// the first record of each and lose the rest, which is what the first
	// version of this did.
	type known struct {
		name  string
		width int
	}
	registry := make([]known, 0, len(base))
	for _, e := range base {
		registry = append(registry, known{name: e.Name, width: int(e.RecordSize)})
	}
	var currentExt string
	var currentWidth int

	for pos := offset; pos+8 <= len(b); {
		size := decodeSize(be.Uint32(b[pos:]))
		if size == 0 {
			break
		}
		if size < 8 || pos+int(size) > len(b) {
			return nil, fmt.Errorf("dboxindex/map: record at %d claims %d bytes, past a %d-byte file", pos, size, len(b))
		}
		recType := le.Uint32(b[pos+4:]) & typeMask
		data := b[pos+8 : pos+int(size)]

		switch recType {
		case typeExtIntro:
			if len(data) < extIntroFixed {
				return nil, fmt.Errorf("dboxindex/map: ext-intro at %d is %d bytes", pos, len(data))
			}
			width := int(le.Uint16(data[12:]))
			nameSize := int(le.Uint16(data[18:]))
			if extIntroFixed+nameSize > len(data) {
				return nil, fmt.Errorf("dboxindex/map: ext-intro at %d names %d bytes it does not have", pos, nameSize)
			}
			name := string(data[extIntroFixed : extIntroFixed+nameSize])
			extID := le.Uint32(data)
			switch {
			case name != "":
				// Re-introducing by name is ordinary -- the reference does it
				// for the map extension twice in this very log -- and it does
				// not create a second extension. Appending a slot for each
				// would shift every id that follows, which is how "ref" once
				// came back as the map and its two bytes were read as a
				// twelve-byte record.
				at := -1
				for i, k := range registry {
					if k.name == name {
						at = i
						break
					}
				}
				if at < 0 {
					registry = append(registry, known{name: name, width: width})
				} else {
					registry[at] = known{name: name, width: width}
				}
				currentExt, currentWidth = name, width
			case int(extID) < len(registry):
				k := registry[extID]
				currentExt = k.name
				// The intro repeats the width; trust what it says now over
				// what it said when the extension was introduced, since an
				// extension can be resized.
				if width > 0 {
					k.width = width
					registry[extID] = k
				}
				currentWidth = k.width
			default:
				// An id neither this log nor the base introduced. Refused
				// rather than skipped: skipping attributes nothing and returns
				// a map that is silently short, and a short map is a mailbox
				// whose messages point at nothing.
				return nil, fmt.Errorf("dboxindex/map: ext-intro at %d refers to extension %d, which neither this log nor the index header introduced", pos, extID)
			}

		case typeAppend:
			for i := 0; i+appendRecordSize <= len(data); i += appendRecordSize {
				uid := le.Uint32(data[i:])
				if _, seen := entries[uid]; !seen {
					entries[uid] = &MapEntry{MapUID: uid}
					order = append(order, uid)
				}
			}

		case typeExtRecUpdate:
			if currentExt == "" || currentWidth == 0 {
				break
			}
			// uid, then record_size bytes, padded to a 4-byte boundary.
			stride := (4 + currentWidth + 3) &^ 3
			for i := 0; i+stride <= len(data); i += stride {
				uid := le.Uint32(data[i:])
				e, ok := entries[uid]
				if !ok {
					e = &MapEntry{MapUID: uid}
					entries[uid] = e
					order = append(order, uid)
				}
				val := data[i+4 : i+4+currentWidth]
				switch currentExt {
				case "map":
					if len(val) < mapRecordSize {
						return nil, fmt.Errorf("dboxindex/map: map record for uid %d is %d bytes, want %d", uid, len(val), mapRecordSize)
					}
					e.FileID = le.Uint32(val)
					e.Offset = le.Uint32(val[4:])
					e.Size = le.Uint32(val[8:])
				case "ref":
					if len(val) >= 2 {
						e.RefCount = le.Uint16(val)
					}
				}
			}

		case typeExtAtomicInc:
			if currentExt != "ref" || currentWidth == 0 {
				break
			}
			// uid, then a signed delta of the extension's width.
			stride := (4 + currentWidth + 3) &^ 3
			for i := 0; i+stride <= len(data); i += stride {
				uid := le.Uint32(data[i:])
				e, ok := entries[uid]
				if !ok {
					continue
				}
				var delta int64
				switch currentWidth {
				case 1:
					delta = int64(int8(data[i+4]))
				case 2:
					delta = int64(int16(le.Uint16(data[i+4:])))
				case 4:
					delta = int64(int32(le.Uint32(data[i+4:])))
				default:
					continue
				}
				sum := int64(e.RefCount) + delta
				if sum < 0 {
					sum = 0
				}
				e.RefCount = uint16(sum)
			}

		case typeExpunge | expungeProt, typeExpungeGUID | expungeProt:
			// A map record is removed by purge rather than by an expunge in
			// this log, so nothing here removes entries; if that changes the
			// records arrive as expunges and are handled with the folder log.
		}
		pos += int(size)
	}

	out := make([]MapEntry, 0, len(order))
	for _, uid := range order {
		out = append(out, *entries[uid])
	}
	return out, nil
}
