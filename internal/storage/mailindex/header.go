package mailindex

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Major/minor version of the on-disk format this package writes.
// Major mismatch on read is a hard reject; minor mismatch upgrades
// in place on next write.
const (
	MajorVersion = 7
	MinorVersion = 3

	// HeaderMinSize is the fixed on-disk size of the base header
	// at MAJOR=7 MINOR=3. Higher minor versions MAY grow it;
	// readers must zero-fill missing tail bytes from older
	// writers and preserve unknown tail bytes from newer writers
	// (see hdr_size handling).
	HeaderMinSize = 120
)

// CompatFlag bits live in the compat_flags header byte.
type CompatFlag uint8

const (
	// CompatLittleEndian is set when every uint16/uint32/uint64
	// field on disk is little-endian. Big-endian indexes are
	// rejected on read.
	CompatLittleEndian CompatFlag = 0x01
)

// HeaderFlag bits live in the header's flags field.
type HeaderFlag uint32

const (
	// HdrFlagCorrupted is a transient in-memory marker — never
	// written to disk (the file is recreated instead). Surfaced
	// from readers; never set on write.
	HdrFlagCorrupted HeaderFlag = 0x0001
	// HdrFlagHaveDirty indicates one or more records carry the
	// per-message MAIL_FLAG_DIRTY backend bit.
	HdrFlagHaveDirty HeaderFlag = 0x0002
	// HdrFlagFsckd is the persisted "index needs a reactive rebuild" marker.
	// Set on a missing/corrupt-message read; the next folder open heals and
	// clears it. Persisted so it survives across processes and pods.
	HdrFlagFsckd HeaderFlag = 0x0004
)

// Header is the 120-byte base header of a mail-index file at
// MAJOR=7 MINOR=3.
//
// Every uint32 field is little-endian on disk regardless of host.
// The 3-byte unused gap after CompatFlags is always zero on write.
type Header struct {
	MajorVersion   uint8      // offset 0
	MinorVersion   uint8      // offset 1
	BaseHeaderSize uint16     // offset 2 — base header size at create time
	HeaderSize     uint32     // offset 4 — base + extended header bytes
	RecordSize     uint32     // offset 8 — base 5 + sum(extension record bytes)
	CompatFlags    CompatFlag // offset 12 — bit 0 = little-endian
	// 3 bytes unused at offset 13

	IndexID     uint32     // offset 16 — UNIX timestamp; ties .index/.log/.cache together
	Flags       HeaderFlag // offset 20 — HAVE_DIRTY | FSCKD (CORRUPTED is transient)
	UIDValidity uint32     // offset 24 — IMAP UIDVALIDITY (non-zero after first sync)
	NextUID     uint32     // offset 28 — monotonic next-save UID

	MessagesCount                uint32 // offset 32
	UnusedOldRecentMessagesCount uint32 // offset 36 — always 0 on write (legacy field)
	SeenMessagesCount            uint32 // offset 40
	DeletedMessagesCount         uint32 // offset 44

	FirstRecentUID          uint32 // offset 48 — first UID with MAIL_RECENT
	FirstUnseenUIDLowwater  uint32 // offset 52 — optimisation hint, never lies high
	FirstDeletedUIDLowwater uint32 // offset 56 — same convention for \Deleted

	LogFileSeq        uint32 // offset 60
	LogFileTailOffset uint32 // offset 64 — non-external txs tail..head still pending
	LogFileHeadOffset uint32 // offset 68

	UnusedOldSyncSizePart1 uint32 // offset 72 — always 0 on write (legacy)
	Log2RotateTime         uint32 // offset 76 — when .log → .log.2 happened; -1 = no .log.2
	LastTempFileScan       uint32 // offset 80 — last orphan-tmpfile sweep timestamp
	DayStamp               uint32 // offset 84 — start-of-day stamp for day_first_uid window

	// DayFirstUID is a rolling 8-day window of the first UID
	// appended on each day. Used by cache-purge to drop
	// MAIL_CACHE_DECISION_TEMP fields older than N days.
	// Index [0] is "today", [7] is "8 days ago". When day_stamp
	// crosses midnight: shift [0..6] → [1..7] then set [0] to
	// the first UID of the new day.
	DayFirstUID [8]uint32 // offset 88, 32 bytes
}

// NewHeader returns a Header for a fresh index: current version,
// BaseHeaderSize = HeaderSize = HeaderMinSize (no extensions),
// RecordSize = RecordMinSize, LITTLE_ENDIAN, supplied IndexID,
// all counts zero. Caller must stamp UIDValidity before the first
// message is appended.
func NewHeader(indexID uint32) Header {
	return Header{
		MajorVersion:   MajorVersion,
		MinorVersion:   MinorVersion,
		BaseHeaderSize: HeaderMinSize,
		HeaderSize:     HeaderMinSize,
		RecordSize:     RecordMinSize,
		CompatFlags:    CompatLittleEndian,
		IndexID:        indexID,
		NextUID:        1,
	}
}

// Encode writes h to w in the canonical 120-byte on-disk layout.
// Returns ErrEndian when CompatFlags omits CompatLittleEndian —
// this package writes little-endian only.
func (h *Header) Encode(w io.Writer) error {
	if h.CompatFlags&CompatLittleEndian == 0 {
		return ErrEndian
	}
	buf := make([]byte, HeaderMinSize)
	h.encodeInto(buf)
	_, err := w.Write(buf)
	return err
}

// EncodeBytes returns h as a fresh 120-byte slice.
func (h *Header) EncodeBytes() []byte {
	buf := make([]byte, HeaderMinSize)
	h.encodeInto(buf)
	return buf
}

func (h *Header) encodeInto(buf []byte) {
	le := binary.LittleEndian
	buf[0] = h.MajorVersion
	buf[1] = h.MinorVersion
	le.PutUint16(buf[2:], h.BaseHeaderSize)
	le.PutUint32(buf[4:], h.HeaderSize)
	le.PutUint32(buf[8:], h.RecordSize)
	buf[12] = uint8(h.CompatFlags)
	// buf[13:16] stay zero (unused[3])
	le.PutUint32(buf[16:], h.IndexID)
	le.PutUint32(buf[20:], uint32(h.Flags))
	le.PutUint32(buf[24:], h.UIDValidity)
	le.PutUint32(buf[28:], h.NextUID)
	le.PutUint32(buf[32:], h.MessagesCount)
	le.PutUint32(buf[36:], h.UnusedOldRecentMessagesCount)
	le.PutUint32(buf[40:], h.SeenMessagesCount)
	le.PutUint32(buf[44:], h.DeletedMessagesCount)
	le.PutUint32(buf[48:], h.FirstRecentUID)
	le.PutUint32(buf[52:], h.FirstUnseenUIDLowwater)
	le.PutUint32(buf[56:], h.FirstDeletedUIDLowwater)
	le.PutUint32(buf[60:], h.LogFileSeq)
	le.PutUint32(buf[64:], h.LogFileTailOffset)
	le.PutUint32(buf[68:], h.LogFileHeadOffset)
	le.PutUint32(buf[72:], h.UnusedOldSyncSizePart1)
	le.PutUint32(buf[76:], h.Log2RotateTime)
	le.PutUint32(buf[80:], h.LastTempFileScan)
	le.PutUint32(buf[84:], h.DayStamp)
	for i, v := range h.DayFirstUID {
		le.PutUint32(buf[88+i*4:], v)
	}
}

// DecodeHeader reads exactly HeaderMinSize bytes from r and parses
// them as a Header. Returns ErrShortRead on truncated input,
// ErrMajorMismatch on major-version mismatch, ErrEndian on a
// big-endian file. Minor-version skew is tolerated both ways: all
// fields are present because BaseHeaderSize is constant within a
// major version, and the on-disk minor value is preserved (never
// upgraded on read, never lowered).
func DecodeHeader(r io.Reader) (Header, error) {
	buf := make([]byte, HeaderMinSize)
	n, err := io.ReadFull(r, buf)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return Header{}, fmt.Errorf("mailindex: header truncated (%d/%d): %w", n, HeaderMinSize, ErrShortRead)
		}
		return Header{}, fmt.Errorf("mailindex: read header: %w", err)
	}
	return DecodeHeaderBytes(buf)
}

// DecodeHeaderBytes parses an already-read 120-byte slice into
// a Header. Returns the same error set as DecodeHeader.
func DecodeHeaderBytes(buf []byte) (Header, error) {
	if len(buf) < HeaderMinSize {
		return Header{}, fmt.Errorf("mailindex: header %d bytes (<%d): %w", len(buf), HeaderMinSize, ErrShortRead)
	}
	h := Header{
		MajorVersion: buf[0],
		MinorVersion: buf[1],
	}
	if h.MajorVersion != MajorVersion {
		return Header{}, fmt.Errorf("mailindex: major version %d (want %d): %w", h.MajorVersion, MajorVersion, ErrMajorMismatch)
	}
	le := binary.LittleEndian
	h.BaseHeaderSize = le.Uint16(buf[2:])
	h.HeaderSize = le.Uint32(buf[4:])
	h.RecordSize = le.Uint32(buf[8:])
	h.CompatFlags = CompatFlag(buf[12])
	if h.CompatFlags&CompatLittleEndian == 0 {
		return Header{}, fmt.Errorf("mailindex: compat_flags=0x%02x missing LITTLE_ENDIAN: %w", h.CompatFlags, ErrEndian)
	}
	h.IndexID = le.Uint32(buf[16:])
	h.Flags = HeaderFlag(le.Uint32(buf[20:]))
	h.UIDValidity = le.Uint32(buf[24:])
	h.NextUID = le.Uint32(buf[28:])
	h.MessagesCount = le.Uint32(buf[32:])
	h.UnusedOldRecentMessagesCount = le.Uint32(buf[36:])
	h.SeenMessagesCount = le.Uint32(buf[40:])
	h.DeletedMessagesCount = le.Uint32(buf[44:])
	h.FirstRecentUID = le.Uint32(buf[48:])
	h.FirstUnseenUIDLowwater = le.Uint32(buf[52:])
	h.FirstDeletedUIDLowwater = le.Uint32(buf[56:])
	h.LogFileSeq = le.Uint32(buf[60:])
	h.LogFileTailOffset = le.Uint32(buf[64:])
	h.LogFileHeadOffset = le.Uint32(buf[68:])
	h.UnusedOldSyncSizePart1 = le.Uint32(buf[72:])
	h.Log2RotateTime = le.Uint32(buf[76:])
	h.LastTempFileScan = le.Uint32(buf[80:])
	h.DayStamp = le.Uint32(buf[84:])
	for i := range h.DayFirstUID {
		h.DayFirstUID[i] = le.Uint32(buf[88+i*4:])
	}
	return h, nil
}

// HeaderSizeAlign rounds size up to the 8-byte alignment used
// everywhere in the extended-header region (extension entries,
// data[] blocks, the boundary between extensions and records).
func HeaderSizeAlign(size uint32) uint32 {
	return (size + 7) &^ 7
}
