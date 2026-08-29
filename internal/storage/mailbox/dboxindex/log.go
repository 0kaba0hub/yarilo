package dboxindex

import (
	"encoding/binary"
	"fmt"
)

// LogHeader introduces one transaction log file.
//
// PrevFileSeq and PrevFileOffset are where the previous file ended, which is
// how a reader that has to cross a rotation knows what it is looking at. They
// are zero for the first file.
type LogHeader struct {
	MajorVersion   uint8
	MinorVersion   uint8
	HeaderSize     uint16
	IndexID        uint32
	FileSeq        uint32
	PrevFileSeq    uint32
	PrevFileOffset uint32
	CreateStamp    uint32
	InitialModseq  uint64
}

const (
	logMajorVersion  = 1
	logMinHeaderSize = 24
)

// ParseLogHeader reads the header of a transaction log file.
func ParseLogHeader(b []byte) (LogHeader, error) {
	var h LogHeader
	if len(b) < logMinHeaderSize {
		return h, fmt.Errorf("dboxindex: log file is %d bytes, too short for a header", len(b))
	}
	h.MajorVersion = b[0]
	h.MinorVersion = b[1]
	if h.MajorVersion != logMajorVersion {
		return h, fmt.Errorf("dboxindex: log major version %d, this reader knows %d", h.MajorVersion, logMajorVersion)
	}
	le := binary.LittleEndian
	h.HeaderSize = le.Uint16(b[2:])
	if int(h.HeaderSize) < logMinHeaderSize {
		return h, fmt.Errorf("dboxindex: log header claims %d bytes, fewer than the fields it must carry", h.HeaderSize)
	}
	if len(b) < int(h.HeaderSize) {
		return h, fmt.Errorf("dboxindex: log header says %d bytes and the file is %d", h.HeaderSize, len(b))
	}
	h.IndexID = le.Uint32(b[4:])
	h.FileSeq = le.Uint32(b[8:])
	h.PrevFileSeq = le.Uint32(b[12:])
	h.PrevFileOffset = le.Uint32(b[16:])
	h.CreateStamp = le.Uint32(b[20:])
	if h.HeaderSize >= 32 {
		h.InitialModseq = le.Uint64(b[24:])
	}
	if h.IndexID == 0 {
		return h, fmt.Errorf("dboxindex: log indexid is zero, which marks the file corrupt")
	}
	return h, nil
}

// Record types. Only the ones an import needs are named; the rest are skipped
// by their size, which is why an unknown type is not an error.
const (
	typeExpunge     = 0x00000001
	typeAppend      = 0x00000002
	typeExpungeGUID = 0x00002000

	// typeMask drops the flag bits a type carries.
	typeMask = 0x0fffffff
	// expungeProt is ORed into both expunge types. A record without it is a
	// corrupt log claiming to delete mail, which the reference refuses to
	// act on and so does this.
	expungeProt = 0x0000cd90
)

// decodeSize undoes the packing the reference applies to a record's size.
//
// The size is not stored as a number: every byte carries its high bit set, so
// that no size can be mistaken for the start of a record and a torn write is
// visible. Returns 0 when the marker bits are absent, which is how the
// reference detects the end of what was completely written.
func decodeSize(be uint32) uint32 {
	if be&0x80808080 != 0x80808080 {
		return 0
	}
	return ((be & 0x0000007f) |
		(be&0x00007f00)>>8<<7 |
		(be&0x007f0000)>>16<<14 |
		(be&0x7f000000)>>24<<21) << 2
}

// Change is one thing that happened to a mailbox.
type Change struct {
	Type ChangeType
	UID  uint32
	// Flags is the message's flag byte, for an append.
	Flags uint8
}

// ChangeType is what a Change did.
type ChangeType int

// The changes this reader produces.
const (
	Appended ChangeType = iota
	Expunged
)

// appendRecordSize is the width of one record inside an append.
//
// Not the index's record size, which is wider: the reference iterates appends
// as a plain struct mail_index_record, so extensions -- keywords, the map uid,
// the guid -- arrive as separate records afterwards rather than inside this
// one. Reading appends at the index's width finds nothing at all, which is what
// the first version of this reader did.
const appendRecordSize = 8

// ReadChanges walks a transaction log from offset and returns the appends and
// expunges it carries, in order.
//
// Records of other types are skipped by their own size rather than refused.
// That is not laxness: the log holds keyword updates, extension introductions
// and modseq bumps that an import does not need, and a reader that stopped at
// the first one it did not know would read nothing at all.
//
// A message may be expunged by more than one record -- the reference writes
// both a plain and a modseq-carrying expunge for the same uid -- so callers
// apply these to a set rather than counting them.
func ReadChanges(b []byte, offset int) ([]Change, error) {
	var out []Change
	be := binary.BigEndian
	le := binary.LittleEndian
	for pos := offset; pos+8 <= len(b); {
		size := decodeSize(be.Uint32(b[pos:]))
		if size == 0 {
			// The tail was not completely written. The reference stops here
			// too, and what follows is rewritten by the next transaction.
			break
		}
		if size < 8 || pos+int(size) > len(b) {
			return out, fmt.Errorf("dboxindex: record at %d claims %d bytes, past the end of a %d-byte file", pos, size, len(b))
		}
		recType := le.Uint32(b[pos+4:])
		data := b[pos+8 : pos+int(size)]

		switch recType & typeMask {
		case typeAppend:
			for i := 0; i+appendRecordSize <= len(data); i += appendRecordSize {
				out = append(out, Change{
					Type:  Appended,
					UID:   le.Uint32(data[i:]),
					Flags: data[i+4],
				})
			}
		// Not exercised by the fixtures, and worth saying so rather than
		// leaving it to look covered: the reference's own header says to
		// avoid this type in favour of the guid-carrying one, and a 2.4
		// store writes only the latter. This branch is here for a log
		// written by something older, and removing it changes no test.
		case typeExpunge | expungeProt:
			for i := 0; i+8 <= len(data); i += 8 {
				first, last := le.Uint32(data[i:]), le.Uint32(data[i+4:])
				if last < first || last-first > uint32(len(b)) {
					return out, fmt.Errorf("dboxindex: expunge range %d..%d at offset %d", first, last, pos)
				}
				for uid := first; uid <= last; uid++ {
					out = append(out, Change{Type: Expunged, UID: uid})
				}
			}
		case typeExpungeGUID | expungeProt:
			const guidLen = 16
			for i := 0; i+4+guidLen <= len(data); i += 4 + guidLen {
				out = append(out, Change{Type: Expunged, UID: le.Uint32(data[i:])})
			}
		}
		pos += int(size)
	}
	return out, nil
}
