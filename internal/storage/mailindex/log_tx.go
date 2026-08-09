package mailindex

import (
	"encoding/binary"
	"fmt"
)

// TxType is the type-enum portion of a transaction-log record's type field
// (TxHeader.Type masked to TxTypeMask).
type TxType uint32

const (
	TxTypeExpunge        TxType = 0x00000001
	TxTypeAppend         TxType = 0x00000002
	TxTypeFlagUpdate     TxType = 0x00000004
	TxTypeHeaderUpdate   TxType = 0x00000020
	TxTypeExtIntro       TxType = 0x00000040
	TxTypeExtReset       TxType = 0x00000080
	TxTypeExtHdrUpdate   TxType = 0x00000100
	TxTypeExtRecUpdate   TxType = 0x00000200
	TxTypeKeywordUpdate  TxType = 0x00000400
	TxTypeKeywordReset   TxType = 0x00000800
	TxTypeExtAtomicInc   TxType = 0x00001000
	TxTypeExpungeGUID    TxType = 0x00002000
	TxTypeModseqUpdate   TxType = 0x00008000
	TxTypeExtHdrUpdate32 TxType = 0x00010000
	TxTypeIndexDeleted   TxType = 0x00020000
	TxTypeIndexUndeleted TxType = 0x00040000
	TxTypeBoundary       TxType = 0x00080000
	TxTypeAttributeUpdt  TxType = 0x00100000
)

// TxExpungeProt is OR'd into the type field of Expunge / ExpungeGUID records
// as a corruption-defence marker. An expunge record without this bit set is
// treated as corrupt and not applied.
const TxExpungeProt TxTypeFlags = 0x0000cd90

// ---- per-tx payload structs --------------------------------

// TxExpunge: expunge every UID in [UID1, UID2]. Payload is a sequence of
// these, 8 bytes each.
type TxExpunge struct {
	UID1, UID2 uint32
}

// TxExpungeGUID: expunge UID iff its on-disk GUID matches. Payload is a
// sequence of these, 4 + 16 = 20 bytes each.
type TxExpungeGUID struct {
	UID  uint32
	GUID [16]byte
}

// TxFlagUpdate: update flags for every UID in [UID1, UID2]. 12 bytes per record.
type TxFlagUpdate struct {
	UID1, UID2    uint32
	AddFlags      MailFlag
	RemoveFlags   MailFlag
	ModSeqIncFlag uint8
	Padding       uint8
}

// TxModseqUpdate: bump modseq for one UID. 12 bytes per record.
type TxModseqUpdate struct {
	UID          uint32
	ModSeqLow32  uint32
	ModSeqHigh32 uint32
}

// TxHeaderUpdate: patch bytes [Offset, Offset+Size) of the base header. On
// disk: 4 bytes fixed header, Size bytes of data, then 0..3 bytes of padding
// to 32-bit alignment.
type TxHeaderUpdate struct {
	Offset uint16
	Data   []byte // len(Data) == Size on the wire
}

// TxExtIntro: introduce (or resize) an extension. ext_id refers to an
// already-known extension; ext_id=0xffffffff + Name creates a new one.
type TxExtIntro struct {
	ExtID       uint32 // 0xffffffff when creating
	ResetID     uint32
	HdrSize     uint32
	RecordSize  uint16
	RecordAlign uint16
	Flags       uint16
	Name        string // empty when ExtID != 0xffffffff
}

// TxExtIntroFlagNoShrink prevents EXT_INTRO from shrinking existing
// hdr_size / record_size / record_align; only grow.
const TxExtIntroFlagNoShrink uint16 = 0x01

// TxExtReset: bump the last-introduced extension's reset_id, optionally
// preserving its old per-record bytes.
type TxExtReset struct {
	NewResetID    uint32
	PreserveData  uint8
	UnusedPadding [3]uint8
}

// TxExtHdrUpdate: patch bytes [Offset, Offset+Size) of the last EXT_INTRO'd
// extension's header. 16-bit offset+size; for >=64KB patches use TxExtHdrUpdate32.
type TxExtHdrUpdate struct {
	Offset uint16
	Data   []byte
}

// TxExtHdrUpdate32 is the 32-bit-offset variant for headers >=64KB.
type TxExtHdrUpdate32 struct {
	Offset uint32
	Data   []byte
}

// TxExtRecUpdate: write Data into the last EXT_INTRO'd extension's per-record
// slot for UID. Data length must equal that extension's record_size.
type TxExtRecUpdate struct {
	UID  uint32
	Data []byte
}

// TxExtAtomicInc: atomically add Diff (signed) to the last EXT_INTRO'd
// extension's per-record value for UID. The extension's record_size must be
// 1, 2, 4, or 8.
type TxExtAtomicInc struct {
	UID  uint32
	Diff int32
}

// TxKeywordUpdate: add or remove a keyword for the given UID ranges.
// ModifyType is 0 (add) or 1 (remove).
type TxKeywordUpdate struct {
	ModifyType uint8
	Padding    uint8
	Name       string
	UIDRanges  []TxKeywordUIDRange
}

// TxKeywordUIDRange is one (UID1, UID2) pair inside a KEYWORD_UPDATE record.
type TxKeywordUIDRange struct {
	UID1, UID2 uint32
}

// TxKeywordModifyAdd / TxKeywordModifyRemove are the two valid values of
// TxKeywordUpdate.ModifyType.
const (
	TxKeywordModifyAdd    uint8 = 0
	TxKeywordModifyRemove uint8 = 1
)

// TxKeywordReset: clear ALL keywords for every UID in [UID1, UID2].
type TxKeywordReset struct {
	UID1, UID2 uint32
}

// TxBoundary opens a multi-record atomic transaction: Size is the total bytes
// of every subsequent record (including this boundary's own header) treated as
// one unit.
type TxBoundary struct {
	Size uint32
}

// ---- encoders -----------------------------------------------

// EncodeTxExpungePayload emits the payload bytes for an EXPUNGE record
// covering recs. Caller composes the full record:
//
//	hdr := TxHeader{Size: 8 + len(payload), Type: TxTypeFlags(TxTypeExpunge) | TxExpungeProt}
//	EncodeTxHeader + write(payload)
//
// Type must include TxExpungeProt; expunges without it are rejected.
func EncodeTxExpungePayload(recs []TxExpunge) []byte {
	out := make([]byte, 8*len(recs))
	le := binary.LittleEndian
	for i, r := range recs {
		le.PutUint32(out[i*8:], r.UID1)
		le.PutUint32(out[i*8+4:], r.UID2)
	}
	return out
}

// EncodeTxExpungeGUIDPayload emits the payload for EXPUNGE_GUID. 20 bytes per
// record. The same TxExpungeProt requirement applies.
func EncodeTxExpungeGUIDPayload(recs []TxExpungeGUID) []byte {
	const sz = 20
	out := make([]byte, sz*len(recs))
	le := binary.LittleEndian
	for i, r := range recs {
		le.PutUint32(out[i*sz:], r.UID)
		copy(out[i*sz+4:i*sz+sz], r.GUID[:])
	}
	return out
}

// EncodeTxFlagUpdatePayload emits the payload for FLAG_UPDATE.
// 12 bytes per record.
func EncodeTxFlagUpdatePayload(recs []TxFlagUpdate) []byte {
	const sz = 12
	out := make([]byte, sz*len(recs))
	le := binary.LittleEndian
	for i, r := range recs {
		le.PutUint32(out[i*sz:], r.UID1)
		le.PutUint32(out[i*sz+4:], r.UID2)
		out[i*sz+8] = uint8(r.AddFlags)
		out[i*sz+9] = uint8(r.RemoveFlags)
		out[i*sz+10] = r.ModSeqIncFlag
		out[i*sz+11] = r.Padding
	}
	return out
}

// EncodeTxModseqUpdatePayload emits the payload for MODSEQ_UPDATE.
// 12 bytes per record.
func EncodeTxModseqUpdatePayload(recs []TxModseqUpdate) []byte {
	const sz = 12
	out := make([]byte, sz*len(recs))
	le := binary.LittleEndian
	for i, r := range recs {
		le.PutUint32(out[i*sz:], r.UID)
		le.PutUint32(out[i*sz+4:], r.ModSeqLow32)
		le.PutUint32(out[i*sz+8:], r.ModSeqHigh32)
	}
	return out
}

// EncodeTxHeaderUpdatePayload emits the payload for HEADER_UPDATE.
// Layout per record: offset(2) + size(2) + data + 0..3 padding
// to 32-bit alignment.
func EncodeTxHeaderUpdatePayload(rec TxHeaderUpdate) []byte {
	size := 4 + len(rec.Data)
	pad := (4 - (size % 4)) % 4
	out := make([]byte, size+pad)
	le := binary.LittleEndian
	le.PutUint16(out[0:], rec.Offset)
	le.PutUint16(out[2:], uint16(len(rec.Data)))
	copy(out[4:], rec.Data)
	return out
}

// EncodeTxExtIntroPayload emits the payload for EXT_INTRO. One record only.
//
// Wire layout (20 fixed bytes + name):
//
//	uint32 ext_id        offset 0   0xffffffff when creating a new ext
//	uint32 reset_id      offset 4
//	uint32 hdr_size      offset 8   size of ext's data[] block
//	uint16 record_size   offset 12
//	uint16 record_align  offset 14
//	uint16 flags         offset 16  TxExtIntroFlagNoShrink etc.
//	uint16 name_size     offset 18
//	char   name[name_size] offset 20
func EncodeTxExtIntroPayload(rec TxExtIntro) []byte {
	out := make([]byte, 20+len(rec.Name))
	le := binary.LittleEndian
	le.PutUint32(out[0:], rec.ExtID)
	le.PutUint32(out[4:], rec.ResetID)
	le.PutUint32(out[8:], rec.HdrSize)
	le.PutUint16(out[12:], rec.RecordSize)
	le.PutUint16(out[14:], rec.RecordAlign)
	le.PutUint16(out[16:], rec.Flags)
	le.PutUint16(out[18:], uint16(len(rec.Name)))
	copy(out[20:], rec.Name)
	return out
}

// EncodeTxExtResetPayload emits the payload for EXT_RESET.
// 8 bytes: new_reset_id(4) + preserve_data(1) + unused[3].
func EncodeTxExtResetPayload(rec TxExtReset) []byte {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint32(out[0:], rec.NewResetID)
	out[4] = rec.PreserveData
	copy(out[5:], rec.UnusedPadding[:])
	return out
}

// EncodeTxExtHdrUpdatePayload emits payload for EXT_HDR_UPDATE.
// Layout: offset(2) + size(2) + data + 0..3 padding.
func EncodeTxExtHdrUpdatePayload(rec TxExtHdrUpdate) []byte {
	size := 4 + len(rec.Data)
	pad := (4 - (size % 4)) % 4
	out := make([]byte, size+pad)
	le := binary.LittleEndian
	le.PutUint16(out[0:], rec.Offset)
	le.PutUint16(out[2:], uint16(len(rec.Data)))
	copy(out[4:], rec.Data)
	return out
}

// EncodeTxExtHdrUpdate32Payload is the 32-bit offset+size variant of
// EncodeTxExtHdrUpdatePayload.
func EncodeTxExtHdrUpdate32Payload(rec TxExtHdrUpdate32) []byte {
	size := 8 + len(rec.Data)
	pad := (4 - (size % 4)) % 4
	out := make([]byte, size+pad)
	le := binary.LittleEndian
	le.PutUint32(out[0:], rec.Offset)
	le.PutUint32(out[4:], uint32(len(rec.Data)))
	copy(out[8:], rec.Data)
	return out
}

// EncodeTxExtRecUpdatePayload emits the payload for EXT_REC_UPDATE. Layout per
// record: uid(4) + data + 0..3 padding to 32-bit alignment. Data length comes
// from the last EXT_INTRO's record_size.
func EncodeTxExtRecUpdatePayload(recs []TxExtRecUpdate) ([]byte, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	recDataLen := len(recs[0].Data)
	for i := range recs {
		if len(recs[i].Data) != recDataLen {
			return nil, fmt.Errorf("mailindex: EXT_REC_UPDATE record %d has %d bytes, want %d: %w",
				i, len(recs[i].Data), recDataLen, ErrCorrupted)
		}
	}
	recSize := 4 + recDataLen
	pad := (4 - (recSize % 4)) % 4
	stride := recSize + pad
	out := make([]byte, stride*len(recs))
	le := binary.LittleEndian
	for i, r := range recs {
		off := i * stride
		le.PutUint32(out[off:], r.UID)
		copy(out[off+4:off+4+recDataLen], r.Data)
	}
	return out, nil
}

// EncodeTxExtAtomicIncPayload emits the payload for EXT_ATOMIC_INC. 8 bytes
// per record.
func EncodeTxExtAtomicIncPayload(recs []TxExtAtomicInc) []byte {
	const sz = 8
	out := make([]byte, sz*len(recs))
	le := binary.LittleEndian
	for i, r := range recs {
		le.PutUint32(out[i*sz:], r.UID)
		le.PutUint32(out[i*sz+4:], uint32(r.Diff))
	}
	return out
}

// DecodeTxExtAtomicIncPayload is the inverse. Kept beside the encoder so the
// two cannot drift: a reader that hand-rolls the layout starts reading rubbish
// the moment the format moves, silently and in whatever the extension happens
// to mean.
//
// A trailing partial record is ignored rather than reported: a torn write is
// the ordinary crash case for a log, and the whole records before it are still
// good.
func DecodeTxExtAtomicIncPayload(payload []byte) []TxExtAtomicInc {
	const sz = 8
	out := make([]TxExtAtomicInc, 0, len(payload)/sz)
	le := binary.LittleEndian
	for i := 0; i+sz <= len(payload); i += sz {
		out = append(out, TxExtAtomicInc{
			UID:  le.Uint32(payload[i:]),
			Diff: int32(le.Uint32(payload[i+4:])),
		})
	}
	return out
}

// EncodeTxKeywordUpdatePayload emits the payload for
// KEYWORD_UPDATE. Layout: modify_type(1) + padding(1) +
// name_size(2) + name + uid_ranges (4 bytes each pair).
func EncodeTxKeywordUpdatePayload(rec TxKeywordUpdate) []byte {
	headerLen := 4 + len(rec.Name)
	pad := (4 - (headerLen % 4)) % 4
	rangesLen := 8 * len(rec.UIDRanges)
	out := make([]byte, headerLen+pad+rangesLen)
	le := binary.LittleEndian
	out[0] = rec.ModifyType
	out[1] = rec.Padding
	le.PutUint16(out[2:], uint16(len(rec.Name)))
	copy(out[4:], rec.Name)
	for i, r := range rec.UIDRanges {
		off := headerLen + pad + i*8
		le.PutUint32(out[off:], r.UID1)
		le.PutUint32(out[off+4:], r.UID2)
	}
	return out
}

// EncodeTxKeywordResetPayload emits the payload for KEYWORD_RESET.
// 8 bytes per record.
func EncodeTxKeywordResetPayload(recs []TxKeywordReset) []byte {
	const sz = 8
	out := make([]byte, sz*len(recs))
	le := binary.LittleEndian
	for i, r := range recs {
		le.PutUint32(out[i*sz:], r.UID1)
		le.PutUint32(out[i*sz+4:], r.UID2)
	}
	return out
}

// EncodeTxBoundaryPayload emits the payload for BOUNDARY.
// 4 bytes total: the size of the multi-record transaction this
// boundary opens.
func EncodeTxBoundaryPayload(rec TxBoundary) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, rec.Size)
	return out
}

// EncodeTxAppendPayload emits the payload for APPEND. Each record encodes a
// full Record (base + extensions) using the supplied layout; the reader slices
// the payload by layout.RecordSize (learned from earlier EXT_INTRO records).
func EncodeTxAppendPayload(layout RecordLayout, recs []*Record) ([]byte, error) {
	stride := int(layout.RecordSize)
	out := make([]byte, stride*len(recs))
	for i, rec := range recs {
		if err := EncodeRecord(out[i*stride:(i+1)*stride], layout, rec); err != nil {
			return nil, err
		}
	}
	return out, nil
}
