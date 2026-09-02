package dboxindex

import (
	"encoding/binary"
	"fmt"
)

// LogHeader introduces one transaction log file; PrevFileSeq/PrevFileOffset are
// where the previous ended, so a reader can cross a rotation.
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
	typeHeaderUpdate  = 0x00000020
	typeKeywordUpdate = 0x00000400
	typeKeywordReset  = 0x00000800
	typeExpungeGUID   = 0x00002000

	// modifyAdd is the modify_type of a keyword update that adds. Zero, being
	// first in an enum with no explicit numbers -- guess 1 and every addition
	// reads as a removal.
	modifyAdd    = 0
	modifyRemove = 1

	// typeMask drops the flag bits a type carries.
	typeMask = 0x0fffffff
	// expungeProt is ORed into both expunge types: a record without it is a
	// corrupt log claiming to delete mail, and is not acted on.
	expungeProt = 0x0000cd90
)

// decodeSize unpacks a record's size, whose every byte carries its high bit set
// so a torn write is visible. 0 when those bits are absent, which is the end of
// what was completely written.
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

	// AddFlags and RemoveFlags are masks, not a value: read as "set the flags
	// to this" only the replace-everything case comes out right.
	AddFlags, RemoveFlags uint8

	// ExtName and ExtData are the extension and its bytes, for ExtRecordSet.
	// ExtData also carries a header span, for HeaderField.
	ExtName string
	ExtData []byte

	// HeaderOffset is where a HeaderField's bytes belong in the base header.
	HeaderOffset int

	// Keyword is the name a keyword update names -- by name in the log, as bits
	// in the base.
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
	// HeaderField carries one span of the base header, which is where next_uid
	// lives after the base was written: appended-then-expunged messages leave
	// it above anything the records show, and max(uid)+1 reissues a uid.
	HeaderField
	// ExtRecordSet carries everything about a tail-appended message beyond uid
	// and flags: its keywords, and for mdbox the map uid its body is at.
	ExtRecordSet
)

// appendRecordSize is the width of one record inside an append -- not the
// index's wider record size, since extensions follow as their own records.
// Reading appends at the index's width finds nothing at all.
const appendRecordSize = 8

// ReadChanges walks a transaction log from offset and returns the changes it
// carries, in order.
//
// base names the extensions a log refers to by id, whose intro may sit in a
// rotated-away file; without it a tail-appended message has no keywords and no
// way to find its bytes. One uid may be expunged twice: apply to a set.
func ReadChanges(b []byte, offset int, base []Extension) ([]Change, error) {
	changes, _, err := ReadChangesAndExtensions(b, offset, base)
	return changes, err
}

// ReadChangesAndExtensions also reports the extensions the log introduced, for
// a folder written but not yet flushed whose whole state is in the log. They
// carry a name and a width and no position: nothing to point into.
func ReadChangesAndExtensions(b []byte, offset int, base []Extension) ([]Change, []Extension, error) {
	var out []Change
	be := binary.BigEndian
	le := binary.LittleEndian
	// Which extension the ext-rec-updates that follow belong to, by the same
	// two forms the map log uses: named, or by an id an earlier intro gave.
	type known struct {
		name  string
		width int
	}
	registry := make([]known, 0, len(base))
	for _, e := range base {
		registry = append(registry, known{name: e.Name, width: int(e.RecordSize)})
	}
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
			return out, nil, fmt.Errorf("dboxindex: record at %d claims %d bytes, past the end of a %d-byte file", pos, size, len(b))
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
		// Not exercised by the fixtures: a current store writes only the
		// guid-carrying expunge. Here for an older log; removing it changes
		// no test.
		case typeExpunge | expungeProt:
			for i := 0; i+8 <= len(data); i += 8 {
				first, last := le.Uint32(data[i:]), le.Uint32(data[i+4:])
				if last < first || last-first > uint32(len(b)) {
					return out, nil, fmt.Errorf("dboxindex: expunge range %d..%d at offset %d", first, last, pos)
				}
				for uid := first; uid <= last; uid++ {
					out = append(out, Change{Type: Expunged, UID: uid})
				}
			}
		case typeHeaderUpdate:
			// {offset, size, data}, padded to four; several share one record.
			for i := 0; i+4 <= len(data); {
				off := int(le.Uint16(data[i:]))
				size := int(le.Uint16(data[i+2:]))
				if i+4+size > len(data) {
					return out, nil, fmt.Errorf("dboxindex: header update at %d claims %d bytes past the record", pos, size)
				}
				out = append(out, Change{
					Type:         HeaderField,
					HeaderOffset: off,
					ExtData:      append([]byte(nil), data[i+4:i+4+size]...),
				})
				i += 4 + size
				i += (4 - i%4) % 4
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
			if currentExt == "" || currentWidth == 0 {
				break
			}
			stride := (4 + currentWidth + 3) &^ 3
			for i := 0; i+stride <= len(data); i += stride {
				val := make([]byte, currentWidth)
				copy(val, data[i+4:i+4+currentWidth])
				out = append(out, Change{
					Type: ExtRecordSet, UID: le.Uint32(data[i:]),
					ExtName: currentExt, ExtData: val,
				})
			}

		case typeFlagUpdate:
			// uid1, uid2, add, remove, then padding the reference reserves.
			const stride = 12
			for i := 0; i+stride <= len(data); i += stride {
				first, last := le.Uint32(data[i:]), le.Uint32(data[i+4:])
				if last < first {
					return out, nil, fmt.Errorf("dboxindex: flag update range %d..%d at offset %d", first, last, pos)
				}
				add, remove := data[i+8], data[i+9]
				if add == 0 && remove == 0 {
					// A modseq-only bump carries neither mask; recorded as a
					// change it would read as flags that moved.
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
				return out, nil, fmt.Errorf("dboxindex: keyword update at %d names %d bytes it does not have", pos, nameSize)
			}
			name := string(data[fixed : fixed+nameSize])
			var kind ChangeType
			switch data[0] {
			case modifyAdd:
				kind = KeywordAdded
			case modifyRemove:
				kind = KeywordRemoved
			default:
				// Replace, which is never written for keywords. Refused, not
				// guessed: either guess changes a keyword nobody asked about.
				return out, nil, fmt.Errorf("dboxindex: keyword update at %d has modify type %d", pos, data[0])
			}
			// The uid ranges begin after the name, aligned to four bytes.
			at := (fixed + nameSize + 3) &^ 3
			for i := at; i+8 <= len(data); i += 8 {
				first, last := le.Uint32(data[i:]), le.Uint32(data[i+4:])
				if last < first {
					return out, nil, fmt.Errorf("dboxindex: keyword range %d..%d at offset %d", first, last, pos)
				}
				for uid := first; uid <= last; uid++ {
					out = append(out, Change{Type: kind, UID: uid, Keyword: name})
				}
			}

		case typeKeywordReset:
			for i := 0; i+8 <= len(data); i += 8 {
				first, last := le.Uint32(data[i:]), le.Uint32(data[i+4:])
				if last < first {
					return out, nil, fmt.Errorf("dboxindex: keyword reset range %d..%d at offset %d", first, last, pos)
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
	exts := make([]Extension, 0, len(registry))
	for _, k := range registry {
		exts = append(exts, Extension{Name: k.name, RecordSize: uint16(k.width)})
	}
	return out, exts, nil
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

// Apply folds a log's changes onto the base's records; keywordNames is the base
// table a tail-appended message's bitmask indexes. A sync re-reads from
// tail_offset and repeats what the base absorbed, so a duplicate append and an
// unknown expunge are expected, and log order is kept.
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
				// The expected overlap, not a second message; the base's
				// record carries the flags applied since.
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
				// Neither in the base nor appended here: another mailbox's.
				continue
			}
			// Remove, then add. Reversed, "replace everything with \Seen" --
			// remove 0xff, add \Seen -- leaves no flags at all.
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

		case ExtRecordSet:
			i, have := at[c.UID]
			if !have {
				continue
			}
			if out[i].ExtData == nil {
				out[i].ExtData = map[string][]byte{}
			}
			out[i].ExtData[c.ExtName] = c.ExtData
			if c.ExtName == "keywords" && len(keywordNames) > 0 {
				// Without the names the mask says nothing.
				out[i].Keywords = keywordsFromMask(c.ExtData, keywordNames)
			}
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

// Header offsets a conversion cares about, in the base header both the file and
// the log's header updates address.
const (
	hdrOffUIDValidity = 24
	hdrOffNextUID     = 28
)

// HeaderState is what a folder's header says about identity and numbering:
// which UID space this is, and where the next message goes.
type HeaderState struct {
	UIDValidity uint32
	NextUID     uint32
}

// ApplyHeader folds the log onto the base's header values. next_uid is the
// largest of the base, a header update, and each appended uid plus one: the
// append moves the counter rather than journalling it, so deriving it from the
// surviving records reissues a uid.
func ApplyHeader(base HeaderState, changes []Change) HeaderState {
	out := base
	for _, c := range changes {
		if c.Type == Appended && c.UID >= out.NextUID {
			out.NextUID = c.UID + 1
		}
		if c.Type != HeaderField {
			continue
		}
		set := func(off int, dst *uint32) {
			if c.HeaderOffset <= off && off+4 <= c.HeaderOffset+len(c.ExtData) {
				*dst = binary.LittleEndian.Uint32(c.ExtData[off-c.HeaderOffset:])
			}
		}
		set(hdrOffUIDValidity, &out.UIDValidity)
		var next uint32
		set(hdrOffNextUID, &next)
		if next > out.NextUID {
			out.NextUID = next
		}
	}
	return out
}
