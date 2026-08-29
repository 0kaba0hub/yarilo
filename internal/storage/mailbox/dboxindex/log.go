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
	typeExpunge       = 0x00000001
	typeAppend        = 0x00000002
	typeFlagUpdate    = 0x00000004
	typeKeywordUpdate = 0x00000400
	typeKeywordReset  = 0x00000800
	typeExpungeGUID   = 0x00002000

	// modifyAdd is the modify_type of a keyword update that adds. Zero, from
	// the reference's own enum -- MODIFY_ADD is the first value and carries no
	// explicit number, which is exactly how a reader guesses 1 and reads every
	// addition as a removal.
	modifyAdd    = 0
	modifyRemove = 1

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

	// AddFlags and RemoveFlags are the two masks of a flag update. They are
	// masks and not a value: the reference says so in its own header, and a
	// reader that treated an update as "set the flags to this" gets the right
	// answer only for the one case where a caller replaces every flag by
	// setting RemoveFlags to 0xff -- and the wrong answer for every ordinary
	// one.
	AddFlags, RemoveFlags uint8

	// Mask is the keywords extension's bytes, for KeywordMaskSet. Bit n of
	// byte m is the (m*8+n)th keyword name.
	Mask []byte

	// Keyword is the name a keyword update names. Keywords arrive by name in
	// the log and as bits in the base, so the two have to be brought together
	// rather than read separately.
	Keyword string
}

// ChangeType is what a Change did.
type ChangeType int

// The changes this reader produces.
const (
	Appended ChangeType = iota
	Expunged
	FlagsChanged
	KeywordAdded
	KeywordRemoved
	KeywordsReset
	// KeywordMaskSet carries the keywords extension's raw bytes for one
	// message. A message appended in the tail gets its keywords this way and
	// not by name: the reference writes the record's extension data rather
	// than a keyword update, so a reader that only understood names would give
	// a freshly delivered message no keywords at all.
	KeywordMaskSet
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
	// Which extension the ext-rec-updates that follow belong to, by the same
	// two forms the map log uses: named, or by an id an earlier intro gave.
	type known struct {
		name  string
		width int
	}
	var registry []known
	var currentExt string
	var currentWidth int
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
		case typeExtIntro:
			if len(data) < extIntroFixed {
				break
			}
			width := int(le.Uint16(data[12:]))
			nameSize := int(le.Uint16(data[18:]))
			if extIntroFixed+nameSize > len(data) {
				break
			}
			name := string(data[extIntroFixed : extIntroFixed+nameSize])
			extID := le.Uint32(data)
			switch {
			case name != "":
				at := -1
				for i, k := range registry {
					if k.name == name {
						at = i
						break
					}
				}
				if at < 0 {
					registry = append(registry, known{name: name, width: width})
				} else {
					registry[at] = known{name: name, width: width}
				}
				currentExt, currentWidth = name, width
			case int(extID) < len(registry):
				currentExt, currentWidth = registry[extID].name, registry[extID].width
			default:
				// Unknown here, and not fatal: a folder log carries
				// extensions this reader has no interest in, and the records
				// that follow are simply not attributed.
				currentExt, currentWidth = "", 0
			}

		case typeExtRecUpdate:
			if currentExt != "keywords" || currentWidth == 0 {
				break
			}
			stride := (4 + currentWidth + 3) &^ 3
			for i := 0; i+stride <= len(data); i += stride {
				mask := make([]byte, currentWidth)
				copy(mask, data[i+4:i+4+currentWidth])
				out = append(out, Change{Type: KeywordMaskSet, UID: le.Uint32(data[i:]), Mask: mask})
			}

		case typeFlagUpdate:
			// uid1, uid2, add, remove, then padding the reference reserves.
			const stride = 12
			for i := 0; i+stride <= len(data); i += stride {
				first, last := le.Uint32(data[i:]), le.Uint32(data[i+4:])
				if last < first {
					return out, fmt.Errorf("dboxindex: flag update range %d..%d at offset %d", first, last, pos)
				}
				add, remove := data[i+8], data[i+9]
				if add == 0 && remove == 0 {
					// A modseq-only bump carries neither mask. Skipping it is
					// not an optimisation: recording it as a change would let
					// a later reader think the flags moved.
					continue
				}
				for uid := first; uid <= last; uid++ {
					out = append(out, Change{Type: FlagsChanged, UID: uid, AddFlags: add, RemoveFlags: remove})
				}
			}

		case typeKeywordUpdate:
			const fixed = 4 // modify_type, padding, name_size
			if len(data) < fixed {
				break
			}
			nameSize := int(le.Uint16(data[2:]))
			if fixed+nameSize > len(data) {
				return out, fmt.Errorf("dboxindex: keyword update at %d names %d bytes it does not have", pos, nameSize)
			}
			name := string(data[fixed : fixed+nameSize])
			var kind ChangeType
			switch data[0] {
			case modifyAdd:
				kind = KeywordAdded
			case modifyRemove:
				kind = KeywordRemoved
			default:
				// MODIFY_REPLACE, which the reference does not write to the
				// log for keywords. Refused rather than guessed at: taking it
				// for either of the other two silently sets or clears a
				// keyword nobody asked about.
				return out, fmt.Errorf("dboxindex: keyword update at %d has modify type %d", pos, data[0])
			}
			// The uid ranges begin after the name, aligned to four bytes.
			at := (fixed + nameSize + 3) &^ 3
			for i := at; i+8 <= len(data); i += 8 {
				first, last := le.Uint32(data[i:]), le.Uint32(data[i+4:])
				if last < first {
					return out, fmt.Errorf("dboxindex: keyword range %d..%d at offset %d", first, last, pos)
				}
				for uid := first; uid <= last; uid++ {
					out = append(out, Change{Type: kind, UID: uid, Keyword: name})
				}
			}

		case typeKeywordReset:
			for i := 0; i+8 <= len(data); i += 8 {
				first, last := le.Uint32(data[i:]), le.Uint32(data[i+4:])
				if last < first {
					return out, fmt.Errorf("dboxindex: keyword reset range %d..%d at offset %d", first, last, pos)
				}
				for uid := first; uid <= last; uid++ {
					out = append(out, Change{Type: KeywordsReset, UID: uid})
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

// keywordsFromMask turns the keywords extension's bytes into names.
func keywordsFromMask(mask []byte, names []string) []string {
	var out []string
	for i, name := range names {
		byteAt, bit := i/8, uint(i%8)
		if byteAt >= len(mask) {
			break
		}
		if mask[byteAt]&(1<<bit) != 0 {
			out = append(out, name)
		}
	}
	return out
}

// Apply folds a log's changes onto the base's records and returns the mailbox.
//
// keywordNames is the table from the keywords extension of the base, needed
// because a message appended in the tail carries its keywords as a bitmask over
// that table rather than by name.
//
// Idempotent, and that is a requirement of the format rather than a nicety.
// The base header carries two offsets: it is synced up to head_offset, but the
// records between tail_offset and head_offset have not necessarily reached the
// mailbox, so the reference re-reads from tail when it syncs a file
// (mail-index-sync-update.c). A reader that starts there sees changes the base
// has already absorbed, and applying them twice would deliver a message twice.
//
// So: an append for a uid the base already carries leaves the base's record
// alone, and an expunge for a uid nobody has is nothing. Neither is an error --
// the format expects the overlap.
//
// The order is the log's order, because a uid may be appended and expunged
// within one tail.
func Apply(base []Record, changes []Change, keywordNames []string) []Record {
	out := make([]Record, len(base))
	copy(out, base)
	at := make(map[uint32]int, len(base))
	for i, r := range base {
		at[r.UID] = i
	}
	gone := make(map[uint32]bool)

	for _, c := range changes {
		switch c.Type {
		case Appended:
			if _, have := at[c.UID]; have {
				// Already in the base: this is the overlap, not a second
				// message. The base's record is the one that carries the
				// flags the reference has since applied.
				delete(gone, c.UID)
				continue
			}
			at[c.UID] = len(out)
			out = append(out, Record{UID: c.UID, Flags: c.Flags})
			delete(gone, c.UID)
		case Expunged:
			gone[c.UID] = true

		case FlagsChanged:
			i, have := at[c.UID]
			if !have {
				// A message the base does not carry and this tail did not
				// append: its flags belong to a mailbox this reader is not
				// looking at.
				continue
			}
			// Masks, in the reference's own order: remove, then add. Reversing
			// them turns "replace everything with \Seen" -- remove 0xff, add
			// \Seen -- into a message with no flags at all.
			out[i].Flags &^= c.RemoveFlags
			out[i].Flags |= c.AddFlags

		case KeywordAdded:
			if i, have := at[c.UID]; have && !hasKeyword(out[i].Keywords, c.Keyword) {
				out[i].Keywords = append(append([]string(nil), out[i].Keywords...), c.Keyword)
			}

		case KeywordRemoved:
			if i, have := at[c.UID]; have {
				out[i].Keywords = withoutKeyword(out[i].Keywords, c.Keyword)
			}

		case KeywordsReset:
			if i, have := at[c.UID]; have {
				out[i].Keywords = nil
			}

		case KeywordMaskSet:
			i, have := at[c.UID]
			if !have || len(keywordNames) == 0 {
				// Without the names the mask says nothing, and guessing at it
				// would put somebody else's keyword on the message.
				continue
			}
			out[i].Keywords = keywordsFromMask(c.Mask, keywordNames)
		}
	}
	if len(gone) == 0 {
		return out
	}
	kept := out[:0]
	for _, r := range out {
		if !gone[r.UID] {
			kept = append(kept, r)
		}
	}
	return kept
}

func hasKeyword(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// withoutKeyword returns list without want, in a fresh slice: the caller's base
// record may share its keyword slice with the one this reader was handed.
func withoutKeyword(list []string, want string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s != want {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
