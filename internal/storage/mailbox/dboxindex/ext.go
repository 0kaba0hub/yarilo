package dboxindex

import (
	"encoding/binary"
	"fmt"
)

// Extension is one named area the index adds to every record, and optionally to
// the header.
//
// Where a field lives is not a constant anywhere: RecordOffset and RecordSize
// say where this extension sits inside a record, and they move as extensions
// are added and removed. Reading a map uid or a guid at a hardcoded offset
// works until the store gains an extension, and then reads somebody else's
// bytes without complaining.
type Extension struct {
	Name         string
	HeaderSize   uint32
	ResetID      uint32
	RecordOffset uint16
	RecordSize   uint16
	RecordAlign  uint16

	// HeaderData is the extension's own header, which is where the keyword
	// names live for the keywords extension.
	HeaderData []byte
}

const extHeaderSize = 16

// align8 rounds up the way the reference does; every extension entry and its
// header are padded to eight bytes.
func align8(n uint32) uint32 { return (n + 7) &^ 7 }

// ParseExtensions reads the extension table between the base header and the end
// of the header area.
//
// The entries are walked, not searched: each one carries the size of its own
// name and header, and the next begins after both, padded. A reader that
// assumed a fixed stride would find the first extension and garbage after it.
func ParseExtensions(b []byte, h Header) ([]Extension, error) {
	if uint32(len(b)) < h.HeaderSize {
		return nil, fmt.Errorf("dboxindex: header says %d bytes and the file is %d", h.HeaderSize, len(b))
	}
	le := binary.LittleEndian
	var out []Extension

	for off := uint32(h.BaseHeaderSize); off+extHeaderSize < h.HeaderSize; {
		var e Extension
		e.HeaderSize = le.Uint32(b[off:])
		e.ResetID = le.Uint32(b[off+4:])
		e.RecordOffset = le.Uint16(b[off+8:])
		e.RecordSize = le.Uint16(b[off+10:])
		e.RecordAlign = le.Uint16(b[off+12:])
		nameSize := uint32(le.Uint16(b[off+14:]))

		nameAt := off + extHeaderSize
		if nameAt+nameSize > h.HeaderSize {
			return out, fmt.Errorf("dboxindex: extension at %d names %d bytes, past the header", off, nameSize)
		}
		e.Name = string(b[nameAt : nameAt+nameSize])

		dataAt := off + align8(extHeaderSize+nameSize)
		if dataAt+e.HeaderSize > h.HeaderSize {
			return out, fmt.Errorf("dboxindex: extension %q claims a %d-byte header, past the header area", e.Name, e.HeaderSize)
		}
		if e.HeaderSize > 0 {
			e.HeaderData = b[dataAt : dataAt+e.HeaderSize]
		}
		if uint32(e.RecordOffset)+uint32(e.RecordSize) > h.RecordSize {
			return out, fmt.Errorf("dboxindex: extension %q sits at %d..%d in a %d-byte record",
				e.Name, e.RecordOffset, uint32(e.RecordOffset)+uint32(e.RecordSize), h.RecordSize)
		}
		out = append(out, e)

		next := dataAt + align8(e.HeaderSize)
		if next <= off {
			return out, fmt.Errorf("dboxindex: extension at %d does not advance", off)
		}
		off = next
	}
	return out, nil
}

// Find returns the extension with the given name.
func Find(exts []Extension, name string) (Extension, bool) {
	for _, e := range exts {
		if e.Name == name {
			return e, true
		}
	}
	return Extension{}, false
}

// FieldIn returns the bytes an extension occupies in one record.
func FieldIn(rec []byte, e Extension) ([]byte, bool) {
	end := int(e.RecordOffset) + int(e.RecordSize)
	if e.RecordSize == 0 || end > len(rec) {
		return nil, false
	}
	return rec[e.RecordOffset:end], true
}

// KeywordNames reads the keyword table out of the keywords extension's header.
//
// The names live once, in the header; a record carries only a bitmask, one bit
// per name in this order. So a keyword cannot be read from a record alone, and
// the two have to be read together or not at all.
func KeywordNames(e Extension) ([]string, error) {
	b := e.HeaderData
	if len(b) < 4 {
		return nil, fmt.Errorf("dboxindex: keyword header is %d bytes", len(b))
	}
	le := binary.LittleEndian
	count := le.Uint32(b)
	const recSize = 8 // uint32 unused + uint32 name_offset
	namesAt := 4 + int(count)*recSize
	if count > uint32(len(b)) || namesAt > len(b) {
		return nil, fmt.Errorf("dboxindex: keyword header claims %d keywords in %d bytes", count, len(b))
	}
	names := b[namesAt:]

	out := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		off := le.Uint32(b[4+int(i)*recSize+4:])
		if off > uint32(len(names)) {
			return nil, fmt.Errorf("dboxindex: keyword %d names offset %d, past %d bytes of names", i, off, len(names))
		}
		s := names[off:]
		if end := indexByte(s, 0); end >= 0 {
			s = s[:end]
		}
		out = append(out, string(s))
	}
	return out, nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// KeywordsOf returns the keywords set on one record.
//
// The mask is little-endian bits over the name list: bit n of byte m is name
// m*8+n. A record whose extension is absent has none, which is not an error --
// a mailbox that never had a keyword carries no keyword extension at all.
func KeywordsOf(rec []byte, e Extension, names []string) []string {
	mask, ok := FieldIn(rec, e)
	if !ok {
		return nil
	}
	var out []string
	for i, name := range names {
		byteAt, bit := i/8, uint(i%8)
		if byteAt >= len(mask) {
			break
		}
		if mask[byteAt]&(1<<bit) != 0 {
			out = append(out, name)
		}
	}
	return out
}
