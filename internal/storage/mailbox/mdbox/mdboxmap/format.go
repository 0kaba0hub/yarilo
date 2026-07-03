package mdboxmap

import (
	"encoding/binary"
	"fmt"

	"github.com/0kaba0hub/yarilo/internal/storage/mailindex"
)

// On-disk filenames. The yarilo-native name is what we write;
// the legacy name is read once at Open() time and the file is
// atomically renamed to the yarilo-native name, so subsequent
// runs see only the yarilo file.
const (
	MapIndexFileName       = "yarilo.map.index"
	LegacyMapIndexFileName = "dovecot.map.index"
)

// Extension names and on-disk sizes are pinned by the wire
// spec (INTERNALS.md §map). Renaming or resizing breaks the
// in-place format the rest of the storage layer depends on.
const (
	// extMap holds {file_id, offset, size} per record.
	extMap     = "map"
	extMapSize = 12 // 3 * uint32

	// extRef holds the uint16 refcount per record. Mutated only
	// through TxExtAtomicInc transactions.
	extRef     = "ref"
	extRefSize = 2

	// extGUID holds the 128-bit message GUID (16 bytes) per record.
	// Stored in the global map so rebuild can pair physical m.<N>
	// records with map_uids via GUID match (strategy 1) rather
	// than the less-robust offset match (strategy 2). Records
	// written before GUID indexing carry zero GUIDs — the rebuild
	// path falls back to offset matching for those.
	extGUID     = "guid"
	extGUIDSize = 16 // 128-bit

	// mapHeaderSize is the size of the extension-header data for
	// the "map" extension. It stores highest_file_id (uint32).
	mapHeaderSize = 4
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

// decodeRefExt is the inverse of encodeRefExt. Missing or short
// data is treated as refcount 0 — newly-allocated records do not
// yet carry a value for the extension.
func decodeRefExt(b []byte) uint16 {
	if len(b) < extRefSize {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

// encodeMapHeader packs highest_file_id into the 4-byte ext
// header data for "map".
func encodeMapHeader(highestFileID uint32) []byte {
	buf := make([]byte, mapHeaderSize)
	binary.LittleEndian.PutUint32(buf, highestFileID)
	return buf
}

// decodeMapHeader extracts highest_file_id from the "map"
// extension's header bytes. A missing or short header is treated
// as zero so a freshly-initialised file reads back cleanly.
func decodeMapHeader(b []byte) uint32 {
	if len(b) < mapHeaderSize {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
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

// defaultExtensions returns the three extensions every map.index
// file must carry. Used by Open when the file does not yet exist
// and the caller is creating it from scratch. Extension order
// matches descending RecordAlign so ComputeRecordLayout packs
// them without padding: map(align=4), ref(align=2), guid(align=1).
func defaultExtensions(highestFileID uint32) []mailindex.Extension {
	return []mailindex.Extension{
		{
			Name:        extMap,
			HdrSize:     mapHeaderSize,
			RecordSize:  extMapSize,
			RecordAlign: 4,
			HdrData:     encodeMapHeader(highestFileID),
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
