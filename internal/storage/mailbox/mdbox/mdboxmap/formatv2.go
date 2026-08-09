package mdboxmap

import (
	"encoding/binary"
	"fmt"
)

// Base index format v2: a fixed-size header followed by fixed-width records
// sorted by map_uid. A record is reachable by offset arithmetic and findable by
// binary search over the file bytes, so opening the map costs a read and no
// per-record parsing, and lookup needs no heap-built index.
const (
	baseMagic     = "YMAP"
	baseVersion2  = 2
	baseHeaderLen = 64
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
	// LogReplayOffset is the byte offset of the append log that the records
	// below already contain. It is honoured only when LogSeq matches the log's
	// own header; otherwise the log belongs to an earlier base and is replayed
	// whole.
	LogReplayOffset uint64
	IndexID         uint32
	LogSeq          uint32
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
	binary.LittleEndian.PutUint64(b[40:48], h.LogReplayOffset)
	binary.LittleEndian.PutUint32(b[48:52], h.IndexID)
	binary.LittleEndian.PutUint32(b[52:56], h.LogSeq)
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
		Version:         b[4],
		RecordSize:      binary.LittleEndian.Uint32(b[8:12]),
		RecordCount:     binary.LittleEndian.Uint32(b[12:16]),
		NextMapUID:      binary.LittleEndian.Uint32(b[16:20]),
		HighestFileID:   binary.LittleEndian.Uint32(b[20:24]),
		RebuildCount:    binary.LittleEndian.Uint32(b[24:28]),
		CreateFileID:    binary.LittleEndian.Uint32(b[28:32]),
		CreateTime:      binary.LittleEndian.Uint64(b[32:40]),
		LogReplayOffset: binary.LittleEndian.Uint64(b[40:48]),
		IndexID:         binary.LittleEndian.Uint32(b[48:52]),
		LogSeq:          binary.LittleEndian.Uint32(b[52:56]),
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
