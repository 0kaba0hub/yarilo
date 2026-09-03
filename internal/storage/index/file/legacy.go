package file

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// The pre-Phase-2 ("yarilo-legacy") .index layout, ours and read-only: kept so
// a folder is migrated the first time it is opened, and dead once every folder
// has been touched once. Wire layout in INTERNALS.md §7.
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
