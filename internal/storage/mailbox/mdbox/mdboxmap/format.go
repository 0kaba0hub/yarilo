package mdboxmap

import (
	"encoding/binary"
	"fmt"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// On-disk filenames. The legacy name is read once at Open and renamed, so later
// runs see only ours.
const (
	MapIndexFileName       = "yarilo.map.index"
	LegacyMapIndexFileName = "dovecot.map.index"
)

// Extension names and sizes are pinned by the wire spec. They describe the log's
// record layout and the v1 base; v2 carries the same fields at fixed offsets.
const (
	// extMap holds {file_id, offset, size} per record.
	extMap     = "map"
	extMapSize = 12 // 3 * uint32

	// extRef holds the uint16 refcount per record. Mutated only
	// through TxExtAtomicInc transactions.
	extRef     = "ref"
	extRefSize = 2

	// extGUID lets a rebuild pair records by GUID rather than by offset; those
	// written before it carry zeroes and fall back to the offset match.
	extGUID     = "guid"
	extGUIDSize = 16 // 128-bit

	// mapHeaderSize covers the counters and the append file's creation stamp,
	// persisted rather than taken from btime, unreliable over NFS (#623).
	mapHeaderSize        = 20
	mapHeaderRebuildSize = 8
	mapHeaderLegacySize  = 4
)

// MapEntry is one parsed map record. UID is the map_uid; the
// remaining fields describe where the message body lives.
type MapEntry struct {
	UID      uint32 // map_uid
	FileID   uint32
	Offset   uint32
	Size     uint32
	RefCount uint16
	GUID     [16]byte // 128-bit message GUID; zero for pre-GUID records
}

// encodeMapExt packs (file_id, offset, size) into the 12-byte
// per-record blob for the "map" extension.
func encodeMapExt(fileID, offset, size uint32) []byte {
	buf := make([]byte, extMapSize)
	binary.LittleEndian.PutUint32(buf[0:4], fileID)
	binary.LittleEndian.PutUint32(buf[4:8], offset)
	binary.LittleEndian.PutUint32(buf[8:12], size)
	return buf
}

// decodeMapExt is the inverse of encodeMapExt.
func decodeMapExt(b []byte) (fileID, offset, size uint32, err error) {
	if len(b) < extMapSize {
		return 0, 0, 0, fmt.Errorf("mdboxmap: map ext %d bytes (<%d)", len(b), extMapSize)
	}
	fileID = binary.LittleEndian.Uint32(b[0:4])
	offset = binary.LittleEndian.Uint32(b[4:8])
	size = binary.LittleEndian.Uint32(b[8:12])
	return fileID, offset, size, nil
}

// encodeRefExt packs a uint16 refcount.
func encodeRefExt(r uint16) []byte {
	buf := make([]byte, extRefSize)
	binary.LittleEndian.PutUint16(buf, r)
	return buf
}

// decodeRefExt inverts encodeRefExt; missing or short data is refcount 0, which
// is what a freshly allocated record carries.
func decodeRefExt(b []byte) uint16 {
	if len(b) < extRefSize {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

// encodeMapHeader packs highest_file_id + rebuild_count + create_file_id +
// create_time into the 20-byte ext header data for "map".
func encodeMapHeader(highestFileID, rebuildCount, createFileID uint32, createTime uint64) []byte {
	buf := make([]byte, mapHeaderSize)
	binary.LittleEndian.PutUint32(buf[0:4], highestFileID)
	binary.LittleEndian.PutUint32(buf[4:8], rebuildCount)
	binary.LittleEndian.PutUint32(buf[8:12], createFileID)
	binary.LittleEndian.PutUint64(buf[12:20], createTime)
	return buf
}

// decodeMapHeader reads the "map" extension header, tolerating the shorter ones
// written before each field existed: a missing field reads back as zero.
func decodeMapHeader(b []byte) (highestFileID, rebuildCount, createFileID uint32, createTime uint64) {
	if len(b) >= 4 {
		highestFileID = binary.LittleEndian.Uint32(b[0:4])
	}
	if len(b) >= mapHeaderRebuildSize {
		rebuildCount = binary.LittleEndian.Uint32(b[4:8])
	}
	if len(b) >= mapHeaderSize {
		createFileID = binary.LittleEndian.Uint32(b[8:12])
		createTime = binary.LittleEndian.Uint64(b[12:20])
	}
	return highestFileID, rebuildCount, createFileID, createTime
}

// encodeGUIDExt packs a 128-bit GUID into the 16-byte per-record
// blob for the "guid" extension.
func encodeGUIDExt(guid [16]byte) []byte {
	b := make([]byte, extGUIDSize)
	copy(b, guid[:])
	return b
}

// decodeGUIDExt is the inverse of encodeGUIDExt. Short or missing
// data returns a zero GUID (pre-GUID records).
func decodeGUIDExt(b []byte) [16]byte {
	var g [16]byte
	if len(b) >= extGUIDSize {
		copy(g[:], b[:extGUIDSize])
	}
	return g
}

// defaultExtensions returns the three extensions a fresh map file carries, in
// descending alignment so the layout packs without padding.
func defaultExtensions(highestFileID uint32) []mailindex.Extension {
	return []mailindex.Extension{
		{
			Name:        extMap,
			HdrSize:     mapHeaderSize,
			RecordSize:  extMapSize,
			RecordAlign: 4,
			HdrData:     encodeMapHeader(highestFileID, 0, 0, 0),
		},
		{
			Name:        extRef,
			HdrSize:     0,
			RecordSize:  extRefSize,
			RecordAlign: 2,
		},
		{
			Name:        extGUID,
			HdrSize:     0,
			RecordSize:  extGUIDSize,
			RecordAlign: 1,
		},
	}
}
