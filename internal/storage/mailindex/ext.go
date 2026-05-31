package mailindex

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// ExtNameMaxLength is the hard limit on extension names enforced
// by the on-disk format. Mirrors MAIL_INDEX_EXT_NAME_MAX_LENGTH.
const ExtNameMaxLength = 64

// Extension describes one registered extension. Extensions are
// declared at file-creation time (or via the EXT_INTRO
// transaction during incremental log writes) and pin a
// well-known name to a fixed per-record byte slot plus an
// optional header-data blob.
//
// RecordSize is how many bytes this extension adds to every
// Record. Zero is legal — for header-only extensions like the
// per-folder "modseq" header that carry no per-record data.
//
// HdrSize is the size of the data[] block in this extension's
// on-disk mail_index_ext_header entry. Zero is legal.
//
// RecordAlign is the alignment required for RecordOffset within
// the on-disk record. Values are 1/2/4/8; must be a power of
// two ≤ 8 (the 8-byte ceiling matches the canonical reader).
//
// ResetID is bumped whenever the meaning of every per-record
// byte for this extension changes — e.g. when a cache file is
// rotated so old offsets are invalid. Readers must drop stale
// per-record data when the on-disk ResetID is higher than what
// they cached.
//
// RecordOffset is computed by ComputeRecordLayout at registration
// time; callers do not set it manually.
type Extension struct {
	Name         string
	HdrSize      uint32
	RecordSize   uint16
	RecordAlign  uint16
	ResetID      uint32
	RecordOffset uint16

	// HdrData is the bytes stored in this extension's data[]
	// block in the extended header. Length MUST equal HdrSize.
	HdrData []byte
}

// RecordLayout summarises the per-record byte layout of the
// index: total record size and the per-extension offset table
// that EncodeRecord/DecodeRecord look up.
//
// RecordSize equals RecordMinSize + sum of every extension's
// RecordSize, rounded so each extension's RecordOffset honours
// its RecordAlign.
type RecordLayout struct {
	RecordSize uint32
	Extensions []Extension // sorted by RecordOffset; same set as supplied to Compute
}

// ComputeRecordLayout assigns RecordOffset to each extension in
// exts and returns the resulting layout. Algorithm:
//
//   - extensions are processed in descending RecordAlign so the
//     widest alignment wins the lowest offset (matches the
//     canonical layout convention; avoids padding waste);
//   - within each alignment class, ordering is by Name (stable);
//   - each extension's offset is rounded up from the current
//     write position to its RecordAlign.
//
// The supplied exts slice is not mutated; the returned layout
// holds copies with RecordOffset filled in.
func ComputeRecordLayout(exts []Extension) (RecordLayout, error) {
	for _, e := range exts {
		if e.Name == "" {
			return RecordLayout{}, fmt.Errorf("mailindex: extension with empty name: %w", ErrCorrupted)
		}
		if len(e.Name) > ExtNameMaxLength {
			return RecordLayout{}, fmt.Errorf("mailindex: extension name %q too long (max %d): %w",
				e.Name, ExtNameMaxLength, ErrCorrupted)
		}
		if e.RecordAlign == 0 && e.RecordSize != 0 {
			return RecordLayout{}, fmt.Errorf("mailindex: extension %q has zero RecordAlign but non-zero RecordSize: %w",
				e.Name, ErrCorrupted)
		}
		if e.RecordAlign > 8 || (e.RecordAlign != 0 && e.RecordAlign&(e.RecordAlign-1) != 0) {
			return RecordLayout{}, fmt.Errorf("mailindex: extension %q has invalid RecordAlign %d: %w",
				e.Name, e.RecordAlign, ErrCorrupted)
		}
		if uint32(len(e.HdrData)) != e.HdrSize {
			return RecordLayout{}, fmt.Errorf("mailindex: extension %q HdrData=%d bytes, HdrSize=%d: %w",
				e.Name, len(e.HdrData), e.HdrSize, ErrCorrupted)
		}
	}
	out := make([]Extension, len(exts))
	copy(out, exts)
	sort.Slice(out, func(i, j int) bool {
		if out[i].RecordAlign != out[j].RecordAlign {
			return out[i].RecordAlign > out[j].RecordAlign
		}
		return out[i].Name < out[j].Name
	})
	pos := uint32(RecordMinSize)
	for i := range out {
		if out[i].RecordSize == 0 {
			out[i].RecordOffset = 0
			continue
		}
		if out[i].RecordAlign > 1 {
			rem := pos % uint32(out[i].RecordAlign)
			if rem != 0 {
				pos += uint32(out[i].RecordAlign) - rem
			}
		}
		if pos > 0xFFFF {
			return RecordLayout{}, fmt.Errorf("mailindex: extension %q RecordOffset overflows uint16: %w",
				out[i].Name, ErrCorrupted)
		}
		out[i].RecordOffset = uint16(pos)
		pos += uint32(out[i].RecordSize)
	}
	// Final layout sorted by RecordOffset for deterministic
	// iteration in decode/encode paths.
	sort.Slice(out, func(i, j int) bool { return out[i].RecordOffset < out[j].RecordOffset })
	return RecordLayout{
		RecordSize: pos,
		Extensions: out,
	}, nil
}

// extHeaderEntrySize returns the on-disk byte count of the
// extended-header entry that introduces ext: the 16-byte fixed
// part + name padded to 8-byte alignment + data[] padded the
// same way.
//
// On-disk layout:
//
//	uint32 hdr_size
//	uint32 reset_id
//	uint16 record_offset
//	uint16 record_size
//	uint16 record_align
//	uint16 name_size
//	char   name[name_size]
//	padding to next 8-byte boundary
//	char   data[hdr_size]
//	padding to next 8-byte boundary
func extHeaderEntrySize(ext *Extension) uint32 {
	size := uint32(16)
	size += uint32(len(ext.Name))
	size = HeaderSizeAlign(size)
	size += ext.HdrSize
	size = HeaderSizeAlign(size)
	return size
}

// EncodeExtHeaders renders the extended-header region (the bytes
// that live between BaseHeaderSize and HeaderSize). Returns a
// fresh buffer ready to be concatenated after Header.EncodeBytes.
//
// Pass the slice of extensions in the order they were registered
// (which is also their identity for EXT_INTRO log records).
// The returned bytes are 8-byte aligned at the tail.
func EncodeExtHeaders(exts []Extension) ([]byte, error) {
	var total uint32
	for i := range exts {
		total += extHeaderEntrySize(&exts[i])
	}
	if total == 0 {
		return nil, nil
	}
	buf := make([]byte, total)
	le := binary.LittleEndian
	pos := uint32(0)
	for i := range exts {
		ext := &exts[i]
		entryStart := pos
		le.PutUint32(buf[pos:], ext.HdrSize)
		le.PutUint32(buf[pos+4:], ext.ResetID)
		le.PutUint16(buf[pos+8:], ext.RecordOffset)
		le.PutUint16(buf[pos+10:], ext.RecordSize)
		le.PutUint16(buf[pos+12:], ext.RecordAlign)
		le.PutUint16(buf[pos+14:], uint16(len(ext.Name)))
		pos += 16
		copy(buf[pos:], ext.Name)
		pos += uint32(len(ext.Name))
		// pad to 8-byte boundary relative to entryStart so the
		// data[] block begins at an 8-aligned offset within the
		// entry — matches HeaderSizeAlign of (16 + name_size).
		afterName := HeaderSizeAlign(pos - entryStart)
		pos = entryStart + afterName
		if ext.HdrSize > 0 {
			copy(buf[pos:], ext.HdrData)
			pos += ext.HdrSize
		}
		afterData := HeaderSizeAlign(pos - entryStart)
		pos = entryStart + afterData
	}
	return buf, nil
}

// DecodeExtHeaders parses the extended-header region into a
// slice of Extensions. The returned slice's order matches the
// on-disk order, which is also the canonical extension-id order
// (first registered = id 0, second = id 1, ...).
//
// extHeaderBytes must equal the bytes between BaseHeaderSize and
// HeaderSize from the on-disk file. ErrShortRead / ErrCorrupted
// surface for any inconsistency.
func DecodeExtHeaders(extHeaderBytes []byte) ([]Extension, error) {
	if len(extHeaderBytes) == 0 {
		return nil, nil
	}
	out := []Extension{}
	le := binary.LittleEndian
	pos := uint32(0)
	for pos < uint32(len(extHeaderBytes)) {
		entryStart := pos
		if pos+16 > uint32(len(extHeaderBytes)) {
			return nil, fmt.Errorf("mailindex: ext header entry truncated at %d: %w", pos, ErrShortRead)
		}
		ext := Extension{
			HdrSize:      le.Uint32(extHeaderBytes[pos:]),
			ResetID:      le.Uint32(extHeaderBytes[pos+4:]),
			RecordOffset: le.Uint16(extHeaderBytes[pos+8:]),
			RecordSize:   le.Uint16(extHeaderBytes[pos+10:]),
			RecordAlign:  le.Uint16(extHeaderBytes[pos+12:]),
		}
		nameSize := uint32(le.Uint16(extHeaderBytes[pos+14:]))
		pos += 16
		if nameSize == 0 || nameSize > ExtNameMaxLength {
			return nil, fmt.Errorf("mailindex: ext name size %d invalid: %w", nameSize, ErrCorrupted)
		}
		if pos+nameSize > uint32(len(extHeaderBytes)) {
			return nil, fmt.Errorf("mailindex: ext name truncated at %d: %w", pos, ErrShortRead)
		}
		ext.Name = string(extHeaderBytes[pos : pos+nameSize])
		pos += nameSize
		// pad name → 8-byte boundary relative to entryStart
		afterName := HeaderSizeAlign(pos - entryStart)
		pos = entryStart + afterName
		if ext.HdrSize > 0 {
			if pos+ext.HdrSize > uint32(len(extHeaderBytes)) {
				return nil, fmt.Errorf("mailindex: ext %q data truncated at %d: %w", ext.Name, pos, ErrShortRead)
			}
			ext.HdrData = make([]byte, ext.HdrSize)
			copy(ext.HdrData, extHeaderBytes[pos:pos+ext.HdrSize])
			pos += ext.HdrSize
		}
		afterData := HeaderSizeAlign(pos - entryStart)
		pos = entryStart + afterData
		out = append(out, ext)
	}
	return out, nil
}
