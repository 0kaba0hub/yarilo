package file

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/0kaba0hub/yarilo/internal/storage/mailindex"
)

// Pre-Phase-2 ("yarilo-legacy") .index file layout — preserved
// here purely to enable automatic on-open migration. The
// canonical reader is the runtime path; this code is dead the
// moment every existing folder has been touched once.
//
// Legacy header was 120 bytes with:
//
//	offset 0  uint8  major (=7)
//	offset 1  uint8  minor (=4 in legacy)
//	offset 2  uint16 base_header_size
//	offset 4  uint32 header_size (== base, no real extensions)
//	offset 8  uint32 record_size (= 5 + 8 + 4 = 17)
//	offset 12 uint8  compat_flags (0x01)
//	offset 16 uint32 index_id
//	offset 24 uint32 uid_validity
//	offset 28 uint32 next_uid
//	offset 32 uint32 messages_count
//	offset 36 uint32 seen_count
//	offset 40 uint32 deleted_count
//	offset 44 uint32 log_file_seq
//	offset 48 uint32 log_file_tail
//	offset 52 uint32 log_file_head
//	offset 56 uint64 modseq          ← yarilo-only — straddles two legacy index fields
//	offset 64 [16]byte folder_guid   ← yarilo-only — straddles 4 legacy index fields
//
// Per-record (17 bytes):
//
//	offset 0  uint32 uid
//	offset 4  uint8  flags
//	offset 5  uint64 modseq
//	offset 13 uint32 keyword_bits
//
// A sibling .index.keywords text file held the keyword name
// registry (one name per line, index = line number). The .names
// sidecar (UID → filename) used today is preserved verbatim.
const (
	legacyMinor       = 4
	legacyRecordSize  = 17
	legacyHdrOffMagic = 56 // 8-byte modseq lives here in legacy format only.
	legacyHdrOffGUID  = 64
)

// legacySnapshot is the in-memory representation a successful
// legacy decode produces. adoptLegacy in folder.go consumes it.
type legacySnapshot struct {
	IndexID       uint32
	UIDValidity   uint32
	NextUID       uint32
	HighestModSeq uint64
	MailboxGUID   [16]byte
	Records       []*mailindex.Record
	Keywords      keywordsHdr
	Filenames     map[uint32]string
}

// detectAndDecodeLegacy peeks at path and either decodes the
// full legacy snapshot or returns (_, false, nil) if the file
// is already in canonical format.
//
// Detection heuristic: legacy files set minor=4 AND record_size
// matches the legacy 17-byte layout AND header_size equals
// base_header_size (no real extended header). Either side missing
// → assume canonical format and let the regular reader handle it.
func detectAndDecodeLegacy(path string) (legacySnapshot, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return legacySnapshot{}, false, err
	}
	defer f.Close()
	hdrBuf := make([]byte, 120)
	if _, err := io.ReadFull(f, hdrBuf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return legacySnapshot{}, false, nil
		}
		return legacySnapshot{}, false, err
	}
	le := binary.LittleEndian
	if hdrBuf[0] != 7 {
		return legacySnapshot{}, false, nil // not even a major-7 index
	}
	minor := hdrBuf[1]
	headerSize := le.Uint32(hdrBuf[4:])
	baseHeaderSize := le.Uint16(hdrBuf[2:])
	recordSize := le.Uint32(hdrBuf[8:])
	if minor != legacyMinor || recordSize != legacyRecordSize || headerSize != uint32(baseHeaderSize) {
		return legacySnapshot{}, false, nil
	}

	snap := legacySnapshot{
		IndexID:       le.Uint32(hdrBuf[16:]),
		UIDValidity:   le.Uint32(hdrBuf[24:]),
		NextUID:       le.Uint32(hdrBuf[28:]),
		HighestModSeq: le.Uint64(hdrBuf[legacyHdrOffMagic:]),
		Filenames:     map[uint32]string{},
	}
	copy(snap.MailboxGUID[:], hdrBuf[legacyHdrOffGUID:legacyHdrOffGUID+16])

	// Records: read until EOF.
	recBuf := make([]byte, legacyRecordSize)
	for {
		_, err := io.ReadFull(f, recBuf)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return legacySnapshot{}, true, fmt.Errorf("fileindex/legacy: read rec: %w", err)
		}
		uid := le.Uint32(recBuf[0:])
		if uid == 0 {
			continue
		}
		flags := recBuf[4]
		modseq := le.Uint64(recBuf[5:])
		kwBits := le.Uint32(recBuf[13:])
		rec := &mailindex.Record{
			UID:   uid,
			Flags: mailindex.MailFlag(flags),
			Ext: map[string][]byte{
				extNameModSeq:   encodeModseqRec(modseq),
				extNameKeywords: encodeKeywordsRec(kwBits),
			},
		}
		snap.Records = append(snap.Records, rec)
	}
	return snap, true, nil
}
