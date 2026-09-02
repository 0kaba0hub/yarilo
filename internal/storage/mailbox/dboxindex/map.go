package dboxindex

import (
	"encoding/binary"
	"fmt"
)

// MapEntry says where one message's bytes are, metadata included. Size 0 is the
// last record in its file and any length: a message over four gigabytes.
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

// ReadMap reconstructs the mdbox map from a transaction log alone, which is all
// a store has until a base is written. base is the map index's extension table:
// after a rotation the intros carry only ids, and without it the map comes back
// empty with no error. Records belong to the extension last named.
func ReadMap(b []byte, offset int, base []Extension) ([]MapEntry, error) {
	return ReadMapOnto(nil, b, offset, base)
}

// ReadMapOnto applies a map log onto the base index's own records, not optional
// wherever a base exists: once rotated the log holds nothing from before it, so
// the log alone reads as an empty map and the folders naming the lost uids
// cannot be converted (#1583).
func ReadMapOnto(seed []MapEntry, b []byte, offset int, base []Extension) ([]MapEntry, error) {
	be, le := binary.BigEndian, binary.LittleEndian

	entries := map[uint32]*MapEntry{}
	var order []uint32
	for _, e := range seed {
		cp := e
		entries[e.MapUID] = &cp
		order = append(order, e.MapUID)
	}
	// Which extension the records that follow belong to. An intro either names
	// a new one or carries a known id with an empty name; both forms appear in
	// one log, so the named form alone loses all but the first record.
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
				// A name may be introduced twice in one log and is still one
				// extension: append a slot per intro and every later id
				// shifts, which once read "ref" as the map.
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
				// The intro repeats the width, and an extension can be
				// resized: trust it over what the introduction said.
				if width > 0 {
					k.width = width
					registry[extID] = k
				}
				currentWidth = k.width
			default:
				// An id neither this log nor the base introduced. Refused, not
				// skipped: a silently short map is a mailbox whose messages
				// point at nothing.
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

// ParseMapRecords reads the map entries a map index's base holds: position in a
// "map" extension, refcount in a "ref" one, both at the offsets the extension
// table gives and nowhere else.
func ParseMapRecords(raw []byte, h Header, exts []Extension) ([]MapEntry, error) {
	mapExt, ok := Find(exts, "map")
	if !ok {
		return nil, fmt.Errorf("dboxindex/map: base index carries no map extension")
	}
	refExt, hasRef := Find(exts, "ref")

	recs, err := ParseRecords(raw, h)
	if err != nil {
		return nil, err
	}
	out := make([]MapEntry, 0, len(recs))
	le := binary.LittleEndian
	for _, r := range recs {
		field, ok := FieldIn(r.Raw, mapExt)
		if !ok || len(field) < mapRecordSize {
			return nil, fmt.Errorf("dboxindex/map: uid %d has %d bytes of map data, want %d",
				r.UID, len(field), mapRecordSize)
		}
		e := MapEntry{
			MapUID: r.UID,
			FileID: le.Uint32(field),
			Offset: le.Uint32(field[4:]),
			Size:   le.Uint32(field[8:]),
			// A record in the base is referenced unless the ref extension says
			// otherwise: reading it as zero would hand every message to the
			// next purge.
			RefCount: 1,
		}
		if hasRef {
			if rf, ok := FieldIn(r.Raw, refExt); ok && len(rf) >= 2 {
				e.RefCount = le.Uint16(rf)
			}
		}
		out = append(out, e)
	}
	return out, nil
}
