package mailindex

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Transaction-log version this package writes. Independent of the
// base index's version (HeaderMinSize / MajorVersion / etc.).
const (
	LogMajorVersion = 1
	LogMinorVersion = 3

	// LogHeaderMinSize is the minimum valid hdr_size on disk —
	// anything smaller is treated as corruption. Matches the
	// canonical MAIL_TRANSACTION_LOG_HEADER_MIN_SIZE.
	LogHeaderMinSize = 24

	// LogHeaderSize is the byte length written at LOG_MAJOR=1
	// LOG_MINOR=3: 40 bytes = base 24 + initial_modseq(8, v1.1+)
	// + compat_flags(1) + unused[3] + unused2(4) (v1.2+). Newer
	// minor versions may grow this; readers preserve unknown tail
	// bytes.
	LogHeaderSize = 40
)

// LogHeader is the header of a .log transaction file.
//
// Wire layout (all multibyte fields little-endian):
//
//	uint8  major_version              offset 0
//	uint8  minor_version              offset 1
//	uint16 hdr_size                   offset 2  — must be ≥ LogHeaderMinSize
//	uint32 indexid                    offset 4  — must match base index IndexID
//	uint32 file_seq                   offset 8  — bumped on rotation
//	uint32 prev_file_seq              offset 12 — chains to .log.2; 0 if reset
//	uint32 prev_file_offset           offset 16
//	uint32 create_stamp               offset 20 — UNIX timestamp of file creation
//	uint64 initial_modseq             offset 24 — present since 1.1; lo at 24, hi at 28
//	uint8  compat_flags               offset 32 — present since 1.2 (mirrors index compat)
//	uint8  unused[3]                  offset 33
//	uint32 unused2                    offset 36
//
// Parsing rule: hdr_size larger than this struct means ignore the
// unknown tail; smaller means assume the missing fields are 0.
// DecodeLogHeader does both — older writers' missing tail is
// zero-padded, newer writers' extra tail is preserved via HdrSize
// so the next Recreate keeps the on-disk size unchanged.
type LogHeader struct {
	MajorVersion   uint8
	MinorVersion   uint8
	HdrSize        uint16
	IndexID        uint32
	FileSeq        uint32
	PrevFileSeq    uint32
	PrevFileOffset uint32
	CreateStamp    uint32
	InitialModSeq  uint64
	CompatFlags    CompatFlag
	// unused[3] + unused2 padding written as zeros
}

// NewLogHeader returns a LogHeader for a freshly-created .log
// file: current version, supplied indexID + fileSeq, zero prev-file
// pointers (non-zero when rotating from an existing .log). The
// package has no time source, so createStamp is caller-supplied.
func NewLogHeader(indexID, fileSeq, createStamp uint32) LogHeader {
	return LogHeader{
		MajorVersion: LogMajorVersion,
		MinorVersion: LogMinorVersion,
		HdrSize:      LogHeaderSize,
		IndexID:      indexID,
		FileSeq:      fileSeq,
		CreateStamp:  createStamp,
		CompatFlags:  CompatLittleEndian,
	}
}

// Encode writes the log header to w in canonical 40-byte layout
// (LOG_MAJOR=1 LOG_MINOR=3).
func (h *LogHeader) Encode(w io.Writer) error {
	if h.HdrSize < LogHeaderMinSize {
		return fmt.Errorf("mailindex: log hdr_size=%d < %d: %w", h.HdrSize, LogHeaderMinSize, ErrCorrupted)
	}
	if h.CompatFlags&CompatLittleEndian == 0 {
		return ErrEndian
	}
	buf := make([]byte, LogHeaderSize)
	h.encodeInto(buf)
	_, err := w.Write(buf)
	return err
}

// EncodeBytes returns the canonical 40-byte log header bytes.
func (h *LogHeader) EncodeBytes() []byte {
	buf := make([]byte, LogHeaderSize)
	h.encodeInto(buf)
	return buf
}

func (h *LogHeader) encodeInto(buf []byte) {
	le := binary.LittleEndian
	buf[0] = h.MajorVersion
	buf[1] = h.MinorVersion
	le.PutUint16(buf[2:], h.HdrSize)
	le.PutUint32(buf[4:], h.IndexID)
	le.PutUint32(buf[8:], h.FileSeq)
	le.PutUint32(buf[12:], h.PrevFileSeq)
	le.PutUint32(buf[16:], h.PrevFileOffset)
	le.PutUint32(buf[20:], h.CreateStamp)
	le.PutUint64(buf[24:], h.InitialModSeq)
	buf[32] = uint8(h.CompatFlags)
	// buf[33..35] stay zero (unused[3])
	// buf[36..39] stay zero (unused2)
}

// DecodeLogHeader reads the on-disk log header from r. Accepts
// both shorter (older writer, hdr_size < ours) and longer (newer
// writer, hdr_size > ours) headers. The returned LogHeader has
// HdrSize set to whatever was on disk so writers can preserve it
// on next sync (per the "ignore any unknown fields" rule).
func DecodeLogHeader(r io.Reader) (LogHeader, error) {
	const minRead = 4 // need at least major+minor+hdr_size to know how much more
	prefix := make([]byte, minRead)
	if _, err := io.ReadFull(r, prefix); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return LogHeader{}, fmt.Errorf("mailindex: log header truncated: %w", ErrShortRead)
		}
		return LogHeader{}, fmt.Errorf("mailindex: read log header prefix: %w", err)
	}
	h := LogHeader{
		MajorVersion: prefix[0],
		MinorVersion: prefix[1],
		HdrSize:      binary.LittleEndian.Uint16(prefix[2:]),
	}
	if h.MajorVersion != LogMajorVersion {
		return LogHeader{}, fmt.Errorf("mailindex: log major %d (want %d): %w",
			h.MajorVersion, LogMajorVersion, ErrMajorMismatch)
	}
	if h.HdrSize < LogHeaderMinSize {
		return LogHeader{}, fmt.Errorf("mailindex: log hdr_size=%d < min %d: %w",
			h.HdrSize, LogHeaderMinSize, ErrCorrupted)
	}
	rest := make([]byte, h.HdrSize-minRead)
	if _, err := io.ReadFull(r, rest); err != nil {
		return LogHeader{}, fmt.Errorf("mailindex: read log header rest: %w", err)
	}
	buf := append(prefix, rest...)
	// Pad to LogHeaderSize so the decoder below indexes safely
	// when an older writer omitted the optional tail fields.
	if len(buf) < LogHeaderSize {
		buf = append(buf, make([]byte, LogHeaderSize-len(buf))...)
	}
	h.decodeFrom(buf)
	return h, nil
}

func (h *LogHeader) decodeFrom(buf []byte) {
	le := binary.LittleEndian
	h.IndexID = le.Uint32(buf[4:])
	h.FileSeq = le.Uint32(buf[8:])
	h.PrevFileSeq = le.Uint32(buf[12:])
	h.PrevFileOffset = le.Uint32(buf[16:])
	h.CreateStamp = le.Uint32(buf[20:])
	h.InitialModSeq = le.Uint64(buf[24:])
	if len(buf) >= 33 {
		h.CompatFlags = CompatFlag(buf[32])
	}
}

// ---- uint32 ↔ offset framing for transaction sizes -------------------

// EncodeFramedSize packs a 4-byte-aligned offset (< 1 GiB) into a
// 4-byte big-endian value with the high bit set on every byte
// (0x80808080). A torn write to any byte breaks the magic bits, so
// the reader rejects it as a partial transaction header.
//
//	offset must be < 0x40000000 and 4-byte aligned.
//	offset >>= 2
//	out_byte_0 = 0x80 | (offset & 0x7f)
//	out_byte_1 = 0x80 | ((offset >> 7) & 0x7f)
//	out_byte_2 = 0x80 | ((offset >> 14) & 0x7f)
//	out_byte_3 = 0x80 | ((offset >> 21) & 0x7f)
//	(big-endian on disk)
func EncodeFramedSize(offset uint32) (uint32, error) {
	if offset >= 0x40000000 {
		return 0, fmt.Errorf("mailindex: framed size %d >= 1 GiB: %w", offset, ErrCorrupted)
	}
	if offset&3 != 0 {
		return 0, fmt.Errorf("mailindex: framed size %d not 4-byte aligned: %w", offset, ErrCorrupted)
	}
	offset >>= 2
	v := uint32(0x80808080)
	v |= offset & 0x7f
	v |= ((offset >> 7) & 0x7f) << 8
	v |= ((offset >> 14) & 0x7f) << 16
	v |= ((offset >> 21) & 0x7f) << 24
	// On-disk order is big-endian.
	return byteSwap32(v), nil
}

// DecodeFramedSize is the inverse of EncodeFramedSize. Returns 0
// when the 0x80808080 magic bits are not all set (torn write).
func DecodeFramedSize(framed uint32) uint32 {
	framed = byteSwap32(framed)
	if framed&0x80808080 != 0x80808080 {
		return 0
	}
	out := framed & 0x7f
	out |= ((framed >> 8) & 0x7f) << 7
	out |= ((framed >> 16) & 0x7f) << 14
	out |= ((framed >> 24) & 0x7f) << 21
	return out << 2
}

func byteSwap32(v uint32) uint32 {
	return (v>>24)&0xff |
		((v>>16)&0xff)<<8 |
		((v>>8)&0xff)<<16 |
		(v&0xff)<<24
}

// ---- TxHeader: every tx record's 8-byte prefix --------------

// TxHeader is the 8-byte prefix every log record carries on
// disk.
//
// `Size` is the FULL byte count of (this 8-byte header +
// payload). It is stored on disk via EncodeFramedSize so a
// torn write cannot produce a valid-looking size. Readers call
// DecodeFramedSize and reject any record whose decoded size is
// zero (or smaller than 8) as a partial / corrupt write.
//
// `Type` is the bitmask: low bits = enum mail_transaction_type,
// high bits = MAIL_TRANSACTION_EXTERNAL / MAIL_TRANSACTION_SYNC
// flags. Use Type.Kind() to extract just the type bits.
type TxHeader struct {
	Size uint32
	Type TxTypeFlags
}

// TxTypeFlags is the on-disk `type` field: low 28 bits are the
// type enum, top bits are flags.
type TxTypeFlags uint32

const (
	// External and sync are the only standard flag bits in the
	// upper byte. EXPUNGE_PROT (0xcd90) is OR'd into expunge
	// records' type for corruption defence but lives in the
	// type enum bits — not a flag.
	TxFlagExternal TxTypeFlags = 0x10000000
	TxFlagSync     TxTypeFlags = 0x20000000

	// TxTypeMask is the low-28 bit mask that isolates the
	// transaction type enum from the flag bits.
	TxTypeMask TxTypeFlags = 0x0fffffff
)

// Kind returns the type-enum portion of t (flag bits stripped).
func (t TxTypeFlags) Kind() TxType { return TxType(t & TxTypeMask) }

// Flags returns the flag-bit portion of t (type bits stripped).
func (t TxTypeFlags) Flags() TxTypeFlags { return t &^ TxTypeMask }

// Has reports whether flag bit f is set on t.
func (t TxTypeFlags) Has(f TxTypeFlags) bool { return t&f == f }

// EncodeTxHeader writes the 8-byte tx record header (framed
// size + type) to buf. buf must be ≥ 8 bytes.
func EncodeTxHeader(buf []byte, h TxHeader) error {
	if len(buf) < 8 {
		return fmt.Errorf("mailindex: encode tx header into %d byte buf: %w", len(buf), ErrShortRead)
	}
	framed, err := EncodeFramedSize(h.Size)
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(buf[0:], framed)
	binary.LittleEndian.PutUint32(buf[4:], uint32(h.Type))
	return nil
}

// DecodeTxHeader reads the 8-byte tx record header from buf.
// Returns ErrCorrupted when the framed size fails its magic
// check (torn write).
func DecodeTxHeader(buf []byte) (TxHeader, error) {
	if len(buf) < 8 {
		return TxHeader{}, fmt.Errorf("mailindex: decode tx header from %d byte buf: %w", len(buf), ErrShortRead)
	}
	framed := binary.LittleEndian.Uint32(buf[0:])
	size := DecodeFramedSize(framed)
	if size == 0 {
		return TxHeader{}, fmt.Errorf("mailindex: tx framed size magic failed: %w", ErrCorrupted)
	}
	if size < 8 {
		return TxHeader{}, fmt.Errorf("mailindex: tx size %d < 8: %w", size, ErrCorrupted)
	}
	return TxHeader{
		Size: size,
		Type: TxTypeFlags(binary.LittleEndian.Uint32(buf[4:])),
	}, nil
}
