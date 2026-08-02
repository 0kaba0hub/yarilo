package file

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// Extension names match the canonical wire spec. These are the
// exact strings the on-disk EXT_INTRO record carries.
const (
	extNameDboxHdr      = "dbox-hdr"
	extNameModSeq       = "modseq"
	extNameKeywords     = "keywords"
	extNameInternalDate = "idate"
	extNameHdrVsize     = "hdr-vsize"
	extNameVsize        = "vsize"
)

// idate extension layout (0 bytes header, 4 bytes per-record):
//
//	per-record:
//	  uint32 unix_time  // seconds since epoch; 0 = unknown
const idateRecSize = 4

func encodeIdateRec(t time.Time) []byte {
	out := make([]byte, idateRecSize)
	if !t.IsZero() {
		binary.LittleEndian.PutUint32(out, uint32(t.Unix()))
	}
	return out
}

func decodeIdateRec(b []byte) time.Time {
	if len(b) < idateRecSize {
		return time.Time{}
	}
	unix := binary.LittleEndian.Uint32(b)
	if unix == 0 {
		return time.Time{}
	}
	return time.Unix(int64(unix), 0).UTC()
}

// hdr-vsize extension layout (16-byte header, 0 per-record). Caches the
// aggregate virtual size of the folder so quota does not rescan every read.
// The {HighestUID, MessageCount} pair validates the cache: if it matches the
// folder's current state the cached Vsize is trusted, otherwise it is
// recalculated from the per-record vsize extension.
//
//	uint64 vsize          // sum of every message's virtual (RFC822) size
//	uint32 highest_uid    // largest UID folded into vsize
//	uint32 message_count  // messages folded into vsize
const (
	hdrVsizeSize            = 16
	hdrVsizeOffVsize        = 0
	hdrVsizeOffHighestUID   = 8
	hdrVsizeOffMessageCount = 12
)

type hdrVsize struct {
	Vsize        uint64
	HighestUID   uint32
	MessageCount uint32
}

func encodeHdrVsize(h hdrVsize) []byte {
	out := make([]byte, hdrVsizeSize)
	le := binary.LittleEndian
	le.PutUint64(out[hdrVsizeOffVsize:], h.Vsize)
	le.PutUint32(out[hdrVsizeOffHighestUID:], h.HighestUID)
	le.PutUint32(out[hdrVsizeOffMessageCount:], h.MessageCount)
	return out
}

func decodeHdrVsize(b []byte) (hdrVsize, error) {
	if len(b) < hdrVsizeSize {
		return hdrVsize{}, fmt.Errorf("fileindex: hdr-vsize too short (%d < %d)", len(b), hdrVsizeSize)
	}
	le := binary.LittleEndian
	return hdrVsize{
		Vsize:        le.Uint64(b[hdrVsizeOffVsize:]),
		HighestUID:   le.Uint32(b[hdrVsizeOffHighestUID:]),
		MessageCount: le.Uint32(b[hdrVsizeOffMessageCount:]),
	}, nil
}

// vsize extension layout (0-byte header, 4 bytes per-record): the per-message
// virtual (RFC822) size. Populated at append from MessageMeta.VSize; summed by
// the hdr-vsize recalc.
const vsizeRecSize = 4

func encodeVsizeRec(v uint32) []byte {
	out := make([]byte, vsizeRecSize)
	binary.LittleEndian.PutUint32(out, v)
	return out
}

func decodeVsizeRec(b []byte) uint32 {
	if len(b) < vsizeRecSize {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

// dbox-hdr extension layout (24 bytes header, 0 per-record):
//
//	uint32 map_uid_validity  // 0 for non-mdbox folders
//	byte   mailbox_guid[16]
//	uint8  flags
//	uint8  unused[3]
const (
	dboxHdrSize         = 24
	dboxHdrOffMapUIDVal = 0
	dboxHdrOffGUID      = 4
	dboxHdrOffFlags     = 20
)

// modseq extension layout:
//
//	header (16 bytes):
//	  uint64 highest_modseq
//	  uint32 log_seq
//	  uint32 log_offset
//	per-record (8 bytes):
//	  uint64 modseq
const (
	modseqHdrSize         = 16
	modseqHdrOffHighest   = 0
	modseqHdrOffLogSeq    = 8
	modseqHdrOffLogOffset = 12
	modseqRecSize         = 8
)

// keywords extension layout:
//
//	header (variable):
//	  uint32 keywords_count
//	  struct { uint32 unused; uint32 name_offset } [keywords_count]
//	  byte name_data[]  // null-terminated names, contiguous
//	per-record (4 bytes):
//	  uint32 bitmask  // bit N set ⇒ keyword index N present
//
// We cap the keyword count at 32 for Phase 2 (matches the
// pre-rewrite yarilo limit). Growing the bitmask to multi-uint32
// is straightforward — just bump keywordsRecSize and add more
// bits — but is deferred until we hit the limit in production.
const (
	keywordsRecSize      = 4
	keywordsMaxBits      = 32
	keywordsHdrEntrySize = 8 // {unused uint32, name_offset uint32}
)

// dboxHdr is the parsed dbox-hdr extension header.
type dboxHdr struct {
	MapUIDValidity uint32
	MailboxGUID    [16]byte
	Flags          uint8
}

func encodeDboxHdr(h dboxHdr) []byte {
	out := make([]byte, dboxHdrSize)
	le := binary.LittleEndian
	le.PutUint32(out[dboxHdrOffMapUIDVal:], h.MapUIDValidity)
	copy(out[dboxHdrOffGUID:dboxHdrOffGUID+16], h.MailboxGUID[:])
	out[dboxHdrOffFlags] = h.Flags
	return out
}

func decodeDboxHdr(b []byte) (dboxHdr, error) {
	if len(b) < dboxHdrSize {
		return dboxHdr{}, fmt.Errorf("fileindex: dbox-hdr too short (%d < %d)", len(b), dboxHdrSize)
	}
	le := binary.LittleEndian
	h := dboxHdr{
		MapUIDValidity: le.Uint32(b[dboxHdrOffMapUIDVal:]),
		Flags:          b[dboxHdrOffFlags],
	}
	copy(h.MailboxGUID[:], b[dboxHdrOffGUID:dboxHdrOffGUID+16])
	return h, nil
}

// modseqHdr is the parsed modseq extension header.
type modseqHdr struct {
	HighestModSeq uint64
	LogSeq        uint32
	LogOffset     uint32
}

func encodeModseqHdr(h modseqHdr) []byte {
	out := make([]byte, modseqHdrSize)
	le := binary.LittleEndian
	le.PutUint64(out[modseqHdrOffHighest:], h.HighestModSeq)
	le.PutUint32(out[modseqHdrOffLogSeq:], h.LogSeq)
	le.PutUint32(out[modseqHdrOffLogOffset:], h.LogOffset)
	return out
}

func decodeModseqHdr(b []byte) (modseqHdr, error) {
	if len(b) < modseqHdrSize {
		return modseqHdr{}, fmt.Errorf("fileindex: modseq hdr too short (%d < %d)", len(b), modseqHdrSize)
	}
	le := binary.LittleEndian
	return modseqHdr{
		HighestModSeq: le.Uint64(b[modseqHdrOffHighest:]),
		LogSeq:        le.Uint32(b[modseqHdrOffLogSeq:]),
		LogOffset:     le.Uint32(b[modseqHdrOffLogOffset:]),
	}, nil
}

// keywordsHdr is the parsed keyword name registry from the
// keywords extension header.
type keywordsHdr struct {
	Names []string // index N is the keyword stored at bit N of the per-record bitmask
}

func encodeKeywordsHdr(h keywordsHdr) []byte {
	if len(h.Names) == 0 {
		out := make([]byte, 4)
		// count = 0; no entries; no name data
		return out
	}
	// 4-byte count + N * 8-byte entry + concatenated null-terminated names.
	nameDataLen := 0
	for _, n := range h.Names {
		nameDataLen += len(n) + 1 // null terminator
	}
	total := 4 + keywordsHdrEntrySize*len(h.Names) + nameDataLen
	out := make([]byte, total)
	le := binary.LittleEndian
	le.PutUint32(out[0:], uint32(len(h.Names)))
	entriesStart := 4
	namesStart := entriesStart + keywordsHdrEntrySize*len(h.Names)
	pos := 0
	for i, n := range h.Names {
		entryOff := entriesStart + i*keywordsHdrEntrySize
		// unused uint32 = 0
		le.PutUint32(out[entryOff+4:], uint32(pos))
		copy(out[namesStart+pos:], n)
		out[namesStart+pos+len(n)] = 0
		pos += len(n) + 1
	}
	return out
}

func decodeKeywordsHdr(b []byte) (keywordsHdr, error) {
	if len(b) < 4 {
		return keywordsHdr{}, nil
	}
	le := binary.LittleEndian
	count := le.Uint32(b[0:])
	if count == 0 {
		return keywordsHdr{}, nil
	}
	if count > keywordsMaxBits {
		return keywordsHdr{}, fmt.Errorf("fileindex: keyword count %d exceeds max %d", count, keywordsMaxBits)
	}
	entriesStart := uint32(4)
	entriesEnd := entriesStart + count*keywordsHdrEntrySize
	if uint32(len(b)) < entriesEnd {
		return keywordsHdr{}, fmt.Errorf("fileindex: keyword entries truncated")
	}
	out := keywordsHdr{Names: make([]string, count)}
	nameBase := entriesEnd
	nameData := b[nameBase:]
	for i := uint32(0); i < count; i++ {
		entryOff := entriesStart + i*keywordsHdrEntrySize
		nameOff := le.Uint32(b[entryOff+4:])
		if int(nameOff) >= len(nameData) {
			return keywordsHdr{}, fmt.Errorf("fileindex: keyword %d name offset %d out of range", i, nameOff)
		}
		end := nameOff
		for end < uint32(len(nameData)) && nameData[end] != 0 {
			end++
		}
		out.Names[i] = string(nameData[nameOff:end])
	}
	return out, nil
}

// keywordsBitmaskFor converts a list of keyword names into the
// 4-byte per-record bitmask, allocating new keyword indices when
// names are not yet in registry (returns the possibly-updated
// registry too). Returns an error when adding a new keyword
// would push past keywordsMaxBits.
func keywordsBitmaskFor(registry keywordsHdr, names []string) (uint32, keywordsHdr, error) {
	if len(names) == 0 {
		return 0, registry, nil
	}
	out := registry
	var bits uint32
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		idx := -1
		for i, existing := range out.Names {
			if existing == n {
				idx = i
				break
			}
		}
		if idx < 0 {
			if len(out.Names) >= keywordsMaxBits {
				return 0, registry, fmt.Errorf("fileindex: keyword registry full (max %d, can't add %q)", keywordsMaxBits, n)
			}
			out.Names = append(out.Names, n)
			idx = len(out.Names) - 1
		}
		bits |= 1 << uint(idx)
	}
	return bits, out, nil
}

// keywordsFromBitmask is the reverse: decode a per-record
// bitmask into the keyword names sorted alphabetically (stable
// output for tests + IDLE pushes).
func keywordsFromBitmask(registry keywordsHdr, bits uint32) []string {
	if bits == 0 {
		return nil
	}
	out := make([]string, 0, 4)
	for i, name := range registry.Names {
		if bits&(1<<uint(i)) != 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// defaultExtensions returns the extension set every freshly
// created index registers. The ResetID is set to the supplied
// uidValidity so every fresh open gets an unambiguous "this is
// a new generation of state" marker without needing a separate
// random ID.
func defaultExtensions(uidValidity uint32, guid [16]byte) []mailindex.Extension {
	return []mailindex.Extension{
		{
			Name:        extNameDboxHdr,
			HdrSize:     dboxHdrSize,
			HdrData:     encodeDboxHdr(dboxHdr{MailboxGUID: guid}),
			RecordSize:  0,
			RecordAlign: 0,
			ResetID:     uidValidity,
		},
		{
			Name:        extNameModSeq,
			HdrSize:     modseqHdrSize,
			HdrData:     encodeModseqHdr(modseqHdr{HighestModSeq: 1}),
			RecordSize:  modseqRecSize,
			RecordAlign: 8,
			ResetID:     uidValidity,
		},
		{
			Name:        extNameKeywords,
			HdrSize:     4, // count=0, no entries
			HdrData:     encodeKeywordsHdr(keywordsHdr{}),
			RecordSize:  keywordsRecSize,
			RecordAlign: 4,
			ResetID:     uidValidity,
		},
		{
			Name:        extNameInternalDate,
			HdrSize:     0,
			HdrData:     nil,
			RecordSize:  idateRecSize,
			RecordAlign: 4,
			ResetID:     uidValidity,
		},
		{
			Name:        extNameHdrVsize,
			HdrSize:     hdrVsizeSize,
			HdrData:     encodeHdrVsize(hdrVsize{}),
			RecordSize:  0,
			RecordAlign: 8,
			ResetID:     uidValidity,
		},
		{
			Name:        extNameVsize,
			HdrSize:     0,
			HdrData:     nil,
			RecordSize:  vsizeRecSize,
			RecordAlign: 4,
			ResetID:     uidValidity,
		},
	}
}

// findExt locates the extension with the given name in a slice
// or returns nil. Sorted-by-RecordOffset slices stay sorted on
// return; the function is read-only.
func findExt(exts []mailindex.Extension, name string) *mailindex.Extension {
	for i := range exts {
		if exts[i].Name == name {
			return &exts[i]
		}
	}
	return nil
}
