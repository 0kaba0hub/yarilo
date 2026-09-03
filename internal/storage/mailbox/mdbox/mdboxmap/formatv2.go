package mdboxmap

import (
	"encoding/binary"
	"fmt"
)

// Base index format v2: a fixed header and fixed-width records sorted by map_uid,
// so an open costs a read with no parsing and lookup needs no built index.
const (
	baseMagic     = "YMAP"
	baseVersion2  = 2
	baseHeaderLen = 80
	baseRecordLen = 36
)

// baseHeader is the v2 file header. Everything the map needs to answer before
// touching a record lives here, including how far into the append log the
// records below already incorporate.
type baseHeader struct {
	Version       uint8
	RecordSize    uint32
	RecordCount   uint32
	NextMapUID    uint32
	HighestFileID uint32
	RebuildCount  uint32
	CreateFileID  uint32
	CreateTime    uint64

	// Lineage names the log this base is the root of, and every rewrite mints a
	// new one -- which is how a reader tells a replaced file from a touched one
	// without anyone declaring it.
	Lineage uint32
	// FoldedLineage / FoldedOffset name the log this base absorbed and how far
	// into it. A reader that still has that log replays only past the offset;
	// replaying from the start would apply every refcount delta a second time.
	FoldedLineage uint32
	FoldedOffset  uint64
	// RecordsDigest is how a reader proves rather than assumes that a rewritten
	// base holds the records it already has: a rewrite that changed them cannot
	// match, including one added later that nobody declared.
	RecordsDigest uint64

	IndexID uint32
}

func encodeBaseHeader(h baseHeader) []byte {
	b := make([]byte, baseHeaderLen)
	copy(b[0:4], baseMagic)
	b[4] = baseVersion2
	binary.LittleEndian.PutUint32(b[8:12], h.RecordSize)
	binary.LittleEndian.PutUint32(b[12:16], h.RecordCount)
	binary.LittleEndian.PutUint32(b[16:20], h.NextMapUID)
	binary.LittleEndian.PutUint32(b[20:24], h.HighestFileID)
	binary.LittleEndian.PutUint32(b[24:28], h.RebuildCount)
	binary.LittleEndian.PutUint32(b[28:32], h.CreateFileID)
	binary.LittleEndian.PutUint64(b[32:40], h.CreateTime)
	binary.LittleEndian.PutUint64(b[40:48], h.FoldedOffset)
	binary.LittleEndian.PutUint64(b[48:56], h.RecordsDigest)
	binary.LittleEndian.PutUint32(b[56:60], h.IndexID)
	binary.LittleEndian.PutUint32(b[60:64], h.Lineage)
	binary.LittleEndian.PutUint32(b[64:68], h.FoldedLineage)
	return b
}

// errUnknownBaseVersion means the file is not a v2 base this binary can read.
// The map never guesses: a misparse here picks the wrong physical bytes for a
// message and the wrong record for a physical delete.
type errUnknownBaseVersion struct {
	version uint8
	magicOK bool
}

func (e errUnknownBaseVersion) Error() string {
	if !e.magicOK {
		return "mdboxmap: base index is not format v2"
	}
	return fmt.Sprintf("mdboxmap: base index version %d is not readable by this binary", e.version)
}

func decodeBaseHeader(b []byte) (baseHeader, error) {
	if len(b) < baseHeaderLen {
		return baseHeader{}, errUnknownBaseVersion{}
	}
	if string(b[0:4]) != baseMagic {
		return baseHeader{}, errUnknownBaseVersion{}
	}
	if b[4] != baseVersion2 {
		return baseHeader{}, errUnknownBaseVersion{version: b[4], magicOK: true}
	}
	h := baseHeader{
		Version:       b[4],
		RecordSize:    binary.LittleEndian.Uint32(b[8:12]),
		RecordCount:   binary.LittleEndian.Uint32(b[12:16]),
		NextMapUID:    binary.LittleEndian.Uint32(b[16:20]),
		HighestFileID: binary.LittleEndian.Uint32(b[20:24]),
		RebuildCount:  binary.LittleEndian.Uint32(b[24:28]),
		CreateFileID:  binary.LittleEndian.Uint32(b[28:32]),
		CreateTime:    binary.LittleEndian.Uint64(b[32:40]),
		FoldedOffset:  binary.LittleEndian.Uint64(b[40:48]),
		RecordsDigest: binary.LittleEndian.Uint64(b[48:56]),
		IndexID:       binary.LittleEndian.Uint32(b[56:60]),
		Lineage:       binary.LittleEndian.Uint32(b[60:64]),
		FoldedLineage: binary.LittleEndian.Uint32(b[64:68]),
	}
	if h.RecordSize != baseRecordLen {
		return baseHeader{}, fmt.Errorf("mdboxmap: base record size %d, want %d", h.RecordSize, baseRecordLen)
	}
	return h, nil
}

// putRecord writes one fixed-width record at b[0:baseRecordLen].
func putRecord(b []byte, e MapEntry) {
	binary.LittleEndian.PutUint32(b[0:4], e.UID)
	binary.LittleEndian.PutUint32(b[4:8], e.FileID)
	binary.LittleEndian.PutUint32(b[8:12], e.Offset)
	binary.LittleEndian.PutUint32(b[12:16], e.Size)
	binary.LittleEndian.PutUint16(b[16:18], e.RefCount)
	binary.LittleEndian.PutUint16(b[18:20], 0)
	copy(b[20:36], e.GUID[:])
}

// getRecord reads one fixed-width record from b[0:baseRecordLen].
func getRecord(b []byte) MapEntry {
	var e MapEntry
	e.UID = binary.LittleEndian.Uint32(b[0:4])
	e.FileID = binary.LittleEndian.Uint32(b[4:8])
	e.Offset = binary.LittleEndian.Uint32(b[8:12])
	e.Size = binary.LittleEndian.Uint32(b[12:16])
	e.RefCount = binary.LittleEndian.Uint16(b[16:18])
	copy(e.GUID[:], b[20:36])
	return e
}

// recordUID reads only the map_uid of the record at b — the single field the
// binary search compares, so the search never materialises a record.
func recordUID(b []byte) uint32 { return binary.LittleEndian.Uint32(b[0:4]) }

// digestRecords hashes the record area. FNV-1a: this is a change detector for a
// file we just read, not a defence against a forged one.
func digestRecords(recs []byte) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for _, c := range recs {
		h ^= uint64(c)
		h *= prime64
	}
	return h
}
