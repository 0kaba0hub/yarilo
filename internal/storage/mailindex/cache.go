// The index cache file (yarilo.index.cache): the immutable half of the
// index design. yarilo.index holds what changes (flags, keywords, modseq);
// this file holds what a message can never change (envelope, body structure,
// derived sizes), so a listing never opens a message file (#1030).
//
// Byte-compatible with the reference's mail_cache_header / mail_cache_record
// (mail-cache-private.h), full eleven-field header included: the field table
// lives inside the file at field_header_offset, which is what makes a field
// id meaningful outside the process that assigned it, and compat_sizeof_uoff_t
// guards against an implementation the file cannot serve.
//
// What byte compatibility buys is INSPECTABILITY -- our cache reads with the
// reference's tooling and vice versa -- and deliberately does NOT buy data
// reuse: cached values are parsing results, and the producer is part of
// their identity. A file written by the reference carries producer byte 0
// and is rejected at open exactly like any other producer mismatch, then
// rebuilt; the cache is derived, so nothing is lost. Do not "fix" the
// generation check to accept 0 for migration's sake -- that silently
// restores trust in a foreign parser.
//
// The cache has no vote on its own validity. Four levels, all owned by the
// index or the producing code:
//
//	indexid       — must match the paired index; a mismatch is garbage.
//	file_seq      — must match the "cache" extension's reset_id; a purge
//	                bumps both, invalidating every stored offset at once.
//	record        — the offset lives in the index record; expunge and
//	                reconcile drop it there.
//	producer gen  — one byte in the reference's unused header slot, bumped
//	                when the PARSER changes: what is stored is the result of
//	                parsing, and a parser fix makes a cached value wrong
//	                against current code while every other level still holds.
package mailindex

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// CacheFileName is the on-disk cache filename beside yarilo.index.
const CacheFileName = "yarilo.index.cache"

const (
	// CacheMajorVersion / CacheMinorVersion mirror the reference's format
	// version: major mismatch means "do not read", minor is backwards
	// compatible.
	CacheMajorVersion = 1
	CacheMinorVersion = 1
	// cacheCompatSizeofUoffT is sizeof(uoff_t) of the writing
	// implementation. Offsets here are uint64-capable file positions; the
	// reference refuses a file whose writer disagreed.
	cacheCompatSizeofUoffT = 8
	// CacheProducerGen is the generation of the code that PRODUCES cached
	// values. Bump on any change to envelope/body-structure parsing that
	// alters output for the same bytes; a mismatch invalidates the file
	// exactly as an indexid mismatch does.
	CacheProducerGen = 1

	cacheHeaderSize = 32
)

// Cache decision bytes, verbatim from the reference.
const (
	CacheDecisionNo     uint8 = 0x00
	CacheDecisionTemp   uint8 = 0x01
	CacheDecisionYes    uint8 = 0x02
	CacheDecisionForced uint8 = 0x80
)

// Cache field types, verbatim from the reference.
const (
	CacheFieldFixedSize uint8 = iota
	CacheFieldVariableSize
	CacheFieldString
	CacheFieldBitmask
	CacheFieldHeader
)

// ErrCacheInvalid marks a cache file that must be treated as absent: wrong
// pair, wrong generation, wrong implementation. Never fatal — the cache is
// derived data, and the caller rebuilds lazily.
var ErrCacheInvalid = errors.New("mailindex: cache file invalid")

// CacheHeader is the 32-byte file header, field-for-field the reference's
// mail_cache_header. ProducerGen occupies the reference's unused byte.
type CacheHeader struct {
	MajorVersion      uint8
	CompatSizeofUoffT uint8
	MinorVersion      uint8
	ProducerGen       uint8
	IndexID           uint32
	FileSeq           uint32
	// ContinuedRecordCount counts records linked behind another record
	// (prev_offset != 0) rather than pointed at from the index directly.
	ContinuedRecordCount uint32
	RecordCount          uint32
	// BackwardsCompatUsedFileSize is unused, kept for byte layout.
	BackwardsCompatUsedFileSize uint32
	DeletedRecordCount          uint32
	// FieldHeaderOffset points at the first field-definition table. Zero
	// means none written yet.
	FieldHeaderOffset uint32
}

func (h *CacheHeader) encode() []byte {
	b := make([]byte, cacheHeaderSize)
	b[0], b[1], b[2], b[3] = h.MajorVersion, h.CompatSizeofUoffT, h.MinorVersion, h.ProducerGen
	le := binary.LittleEndian
	le.PutUint32(b[4:], h.IndexID)
	le.PutUint32(b[8:], h.FileSeq)
	le.PutUint32(b[12:], h.ContinuedRecordCount)
	le.PutUint32(b[16:], h.RecordCount)
	le.PutUint32(b[20:], h.BackwardsCompatUsedFileSize)
	le.PutUint32(b[24:], h.DeletedRecordCount)
	le.PutUint32(b[28:], h.FieldHeaderOffset)
	return b
}

func decodeCacheHeader(b []byte) (CacheHeader, error) {
	if len(b) < cacheHeaderSize {
		return CacheHeader{}, fmt.Errorf("mailindex: cache header short read (%d bytes): %w", len(b), ErrCacheInvalid)
	}
	le := binary.LittleEndian
	return CacheHeader{
		MajorVersion:                b[0],
		CompatSizeofUoffT:           b[1],
		MinorVersion:                b[2],
		ProducerGen:                 b[3],
		IndexID:                     le.Uint32(b[4:]),
		FileSeq:                     le.Uint32(b[8:]),
		ContinuedRecordCount:        le.Uint32(b[12:]),
		RecordCount:                 le.Uint32(b[16:]),
		BackwardsCompatUsedFileSize: le.Uint32(b[20:]),
		DeletedRecordCount:          le.Uint32(b[24:]),
		FieldHeaderOffset:           le.Uint32(b[28:]),
	}, nil
}

// CacheField is one entry of the in-file field-definition table.
type CacheField struct {
	Name     string
	Type     uint8
	Decision uint8
	// Size is the fixed size for CacheFieldFixedSize/Bitmask fields;
	// 0xffffffff on disk marks variable-sized fields.
	Size     uint32
	LastUsed uint32
}

// CacheFieldValue is one field's data in a record.
type CacheFieldValue struct {
	FieldID uint32
	Data    []byte
}

// CacheFile is an open yarilo.index.cache. Not goroutine-safe: callers hold
// the same per-folder lock every index write already holds. Readers open and
// close within a call — no long-lived descriptors, the NFS rule optimize
// depends on (#1176).
type CacheFile struct {
	f      *os.File
	hdr    CacheHeader
	fields []CacheField
	// byName maps a field name to its id (= position in fields).
	byName map[string]uint32
}

// CreateCache writes a fresh cache file for the given index identity.
// fileSeq must equal the "cache" extension's reset_id in the paired index;
// the first file conventionally uses the current UNIX time.
func CreateCache(path string, indexID, fileSeq uint32) (*CacheFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mailindex: create cache: %w", err)
	}
	hdr := CacheHeader{
		MajorVersion:      CacheMajorVersion,
		CompatSizeofUoffT: cacheCompatSizeofUoffT,
		MinorVersion:      CacheMinorVersion,
		ProducerGen:       CacheProducerGen,
		IndexID:           indexID,
		FileSeq:           fileSeq,
	}
	if _, err := f.Write(hdr.encode()); err != nil {
		f.Close()
		return nil, fmt.Errorf("mailindex: create cache: %w", err)
	}
	return &CacheFile{f: f, hdr: hdr, byName: map[string]uint32{}}, nil
}

// OpenCache opens and validates an existing cache against its paired index.
// Every validation failure wraps ErrCacheInvalid: the caller treats the file
// as absent and rebuilds — never as an error to surface to a client.
func OpenCache(path string, indexID, expectFileSeq uint32) (*CacheFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err // includes os.IsNotExist for the caller
	}
	hb := make([]byte, cacheHeaderSize)
	n, err := io.ReadFull(f, hb)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mailindex: cache header (%d bytes): %w", n, ErrCacheInvalid)
	}
	hdr, err := decodeCacheHeader(hb)
	if err != nil {
		f.Close()
		return nil, err
	}
	switch {
	case hdr.MajorVersion != CacheMajorVersion:
		err = fmt.Errorf("mailindex: cache major %d, want %d: %w", hdr.MajorVersion, CacheMajorVersion, ErrCacheInvalid)
	case hdr.CompatSizeofUoffT != cacheCompatSizeofUoffT:
		err = fmt.Errorf("mailindex: cache sizeof(uoff_t) %d, want %d: %w", hdr.CompatSizeofUoffT, cacheCompatSizeofUoffT, ErrCacheInvalid)
	case hdr.IndexID != indexID:
		err = fmt.Errorf("mailindex: cache indexid %d, index %d: %w", hdr.IndexID, indexID, ErrCacheInvalid)
	case hdr.FileSeq != expectFileSeq:
		err = fmt.Errorf("mailindex: cache file_seq %d, reset_id %d: %w", hdr.FileSeq, expectFileSeq, ErrCacheInvalid)
	case hdr.ProducerGen != CacheProducerGen:
		// The one divergence the pair identity cannot see: the parser
		// changed, so every stored value is wrong against current code.
		err = fmt.Errorf("mailindex: cache producer gen %d, code %d: %w", hdr.ProducerGen, CacheProducerGen, ErrCacheInvalid)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	c := &CacheFile{f: f, hdr: hdr, byName: map[string]uint32{}}
	if err := c.loadFields(); err != nil {
		f.Close()
		return nil, err
	}
	return c, nil
}

// Close closes the descriptor. The header counters are flushed by the
// operations that change them, so Close has nothing to write.
func (c *CacheFile) Close() error { return c.f.Close() }

// Header returns a copy of the current header.
func (c *CacheFile) Header() CacheHeader { return c.hdr }

// Fields returns the field-definition table in id order.
func (c *CacheFile) Fields() []CacheField {
	out := make([]CacheField, len(c.fields))
	copy(out, c.fields)
	return out
}

/* --- field-definition table ---------------------------------------------- */

// loadFields walks the next_offset chain to the newest field table.
func (c *CacheFile) loadFields() error {
	off := c.hdr.FieldHeaderOffset
	if off == 0 {
		return nil
	}
	for {
		next, fields, err := c.readFieldTable(off)
		if err != nil {
			return err
		}
		c.fields = fields
		if next == 0 {
			break
		}
		off = next
	}
	c.byName = make(map[string]uint32, len(c.fields))
	for i, fl := range c.fields {
		c.byName[fl.Name] = uint32(i)
	}
	return nil
}

// readFieldTable decodes one mail_cache_header_fields block:
//
//	next_offset u32 (packed) | size u32 | fields_count u32 |
//	last_used[count] u32 | size[count] u32 | type[count] u8 |
//	decision[count] u8 | names: NUL-separated
func (c *CacheFile) readFieldTable(off uint32) (next uint32, fields []CacheField, err error) {
	fixed := make([]byte, 12)
	if _, err := c.f.ReadAt(fixed, int64(off)); err != nil {
		return 0, nil, fmt.Errorf("mailindex: cache field table at %d: %w", off, ErrCacheInvalid)
	}
	le := binary.LittleEndian
	next = unpackCacheOffset(le.Uint32(fixed[0:]))
	size := le.Uint32(fixed[4:])
	count := le.Uint32(fixed[8:])
	if size < 12 || count > (size-12)/10 {
		return 0, nil, fmt.Errorf("mailindex: cache field table size %d count %d: %w", size, count, ErrCacheInvalid)
	}
	body := make([]byte, size-12)
	if _, err := c.f.ReadAt(body, int64(off)+12); err != nil {
		return 0, nil, fmt.Errorf("mailindex: cache field table body: %w", ErrCacheInvalid)
	}
	lastUsed := body[0 : 4*count]
	sizes := body[4*count : 8*count]
	types := body[8*count : 9*count]
	decisions := body[9*count : 10*count]
	names := strings.Split(strings.TrimRight(string(body[10*count:]), "\x00"), "\x00")
	if uint32(len(names)) < count {
		return 0, nil, fmt.Errorf("mailindex: cache field table names %d < %d: %w", len(names), count, ErrCacheInvalid)
	}
	fields = make([]CacheField, count)
	for i := uint32(0); i < count; i++ {
		fields[i] = CacheField{
			Name:     names[i],
			Type:     types[i],
			Decision: decisions[i],
			Size:     le.Uint32(sizes[4*i:]),
			LastUsed: le.Uint32(lastUsed[4*i:]),
		}
	}
	return next, fields, nil
}

// AddFields appends a new field table carrying the existing fields plus the
// given ones and links it from the previous table (or the header). Returns
// the id of the first added field. Existing names are not re-added.
func (c *CacheFile) AddFields(add []CacheField) (uint32, error) {
	firstNew := uint32(len(c.fields))
	merged := c.Fields()
	for _, fl := range add {
		if _, dup := c.byName[fl.Name]; dup {
			continue
		}
		if fl.Type == CacheFieldVariableSize || fl.Type == CacheFieldString || fl.Type == CacheFieldHeader {
			fl.Size = 0xffffffff
		}
		merged = append(merged, fl)
		c.byName[fl.Name] = uint32(len(merged) - 1)
	}
	if uint32(len(merged)) == firstNew {
		return firstNew, nil // nothing new
	}

	le := binary.LittleEndian
	count := uint32(len(merged))
	var names []byte
	for _, fl := range merged {
		names = append(names, fl.Name...)
		names = append(names, 0)
	}
	size := 12 + 10*count + uint32(len(names))
	if pad := size & 3; pad != 0 {
		size += 4 - pad
	}
	buf := make([]byte, size)
	le.PutUint32(buf[4:], size)
	le.PutUint32(buf[8:], count)
	for i, fl := range merged {
		le.PutUint32(buf[12+4*i:], fl.LastUsed)
		le.PutUint32(buf[12+4*int(count)+4*i:], fl.Size)
		buf[12+8*int(count)+i] = fl.Type
		buf[12+9*int(count)+i] = fl.Decision
	}
	copy(buf[12+10*count:], names)

	off, err := c.appendAligned(buf)
	if err != nil {
		return 0, err
	}
	// Link: the previous newest table points at this one; with none, the
	// header does. Either way the chain converges on the newest table.
	if c.hdr.FieldHeaderOffset == 0 {
		c.hdr.FieldHeaderOffset = off
		if err := c.writeHeader(); err != nil {
			return 0, err
		}
	} else {
		tail, err := c.newestTableOffset()
		if err != nil {
			return 0, err
		}
		var lb [4]byte
		le.PutUint32(lb[:], packCacheOffset(off))
		if _, err := c.f.WriteAt(lb[:], int64(tail)); err != nil {
			return 0, fmt.Errorf("mailindex: cache link field table: %w", err)
		}
	}
	c.fields = merged
	return firstNew, nil
}

// newestTableOffset walks the chain and returns the offset of the last table.
func (c *CacheFile) newestTableOffset() (uint32, error) {
	off := c.hdr.FieldHeaderOffset
	for {
		var nb [4]byte
		if _, err := c.f.ReadAt(nb[:], int64(off)); err != nil {
			return 0, fmt.Errorf("mailindex: cache field chain: %w", ErrCacheInvalid)
		}
		next := unpackCacheOffset(binary.LittleEndian.Uint32(nb[:]))
		if next == 0 {
			return off, nil
		}
		off = next
	}
}

// FieldID resolves a field name to its id, or ok=false.
func (c *CacheFile) FieldID(name string) (uint32, bool) {
	id, ok := c.byName[name]
	return id, ok
}

/* --- records -------------------------------------------------------------- */

// AppendRecord appends one cache record and returns its offset — the value
// the caller stores into the index's "cache" extension for the message.
// prevOffset links an earlier record for the same message (0 for none); a
// reader merges the chain newest-first, first value per field winning.
func (c *CacheFile) AppendRecord(prevOffset uint32, values []CacheFieldValue) (uint32, error) {
	le := binary.LittleEndian
	body := make([]byte, 0, 64)
	for _, v := range values {
		if int(v.FieldID) >= len(c.fields) {
			return 0, fmt.Errorf("mailindex: cache record names unknown field %d", v.FieldID)
		}
		var fb [4]byte
		le.PutUint32(fb[:], v.FieldID)
		body = append(body, fb[:]...)
		fl := c.fields[v.FieldID]
		if fl.Size == 0xffffffff {
			le.PutUint32(fb[:], uint32(len(v.Data)))
			body = append(body, fb[:]...)
		} else if uint32(len(v.Data)) != fl.Size {
			return 0, fmt.Errorf("mailindex: cache field %q wants %d bytes, got %d", fl.Name, fl.Size, len(v.Data))
		}
		body = append(body, v.Data...)
		if pad := len(v.Data) & 3; pad != 0 {
			body = append(body, make([]byte, 4-pad)...)
		}
	}
	rec := make([]byte, 8+len(body))
	le.PutUint32(rec[0:], prevOffset)
	le.PutUint32(rec[4:], uint32(len(rec)))
	copy(rec[8:], body)

	off, err := c.appendAligned(rec)
	if err != nil {
		return 0, err
	}
	c.hdr.RecordCount++
	if prevOffset != 0 {
		c.hdr.ContinuedRecordCount++
	}
	if err := c.writeHeader(); err != nil {
		return 0, err
	}
	return off, nil
}

// ReadRecord returns the merged field values reachable from offset,
// following the prev_offset chain. The newest record wins per field.
func (c *CacheFile) ReadRecord(offset uint32) (map[uint32][]byte, error) {
	out := make(map[uint32][]byte)
	le := binary.LittleEndian
	seen := 0
	for offset != 0 {
		if seen++; seen > 1024 {
			return nil, fmt.Errorf("mailindex: cache record chain too long: %w", ErrCacheInvalid)
		}
		var rh [8]byte
		if _, err := c.f.ReadAt(rh[:], int64(offset)); err != nil {
			return nil, fmt.Errorf("mailindex: cache record at %d: %w", offset, ErrCacheInvalid)
		}
		prev := le.Uint32(rh[0:])
		size := le.Uint32(rh[4:])
		if size < 8 || size > 1<<26 {
			return nil, fmt.Errorf("mailindex: cache record size %d: %w", size, ErrCacheInvalid)
		}
		body := make([]byte, size-8)
		if _, err := c.f.ReadAt(body, int64(offset)+8); err != nil {
			return nil, fmt.Errorf("mailindex: cache record body: %w", ErrCacheInvalid)
		}
		for p := 0; p+4 <= len(body); {
			id := le.Uint32(body[p:])
			p += 4
			if int(id) >= len(c.fields) {
				return nil, fmt.Errorf("mailindex: cache record field %d: %w", id, ErrCacheInvalid)
			}
			fl := c.fields[id]
			var dlen uint32
			if fl.Size == 0xffffffff {
				if p+4 > len(body) {
					return nil, fmt.Errorf("mailindex: cache record truncated: %w", ErrCacheInvalid)
				}
				dlen = le.Uint32(body[p:])
				p += 4
			} else {
				dlen = fl.Size
			}
			if p+int(dlen) > len(body) {
				return nil, fmt.Errorf("mailindex: cache record truncated: %w", ErrCacheInvalid)
			}
			if _, have := out[id]; !have {
				out[id] = append([]byte(nil), body[p:p+int(dlen)]...)
			}
			p += int(dlen)
			if pad := dlen & 3; pad != 0 {
				p += int(4 - pad)
			}
		}
		offset = prev
	}
	return out, nil
}

// MarkDeleted counts an expunged message's record so purge knows how much
// dead weight the file carries.
func (c *CacheFile) MarkDeleted() error {
	c.hdr.DeletedRecordCount++
	return c.writeHeader()
}

/* --- low-level ------------------------------------------------------------ */

func (c *CacheFile) writeHeader() error {
	if _, err := c.f.WriteAt(c.hdr.encode(), 0); err != nil {
		return fmt.Errorf("mailindex: cache header write: %w", err)
	}
	return nil
}

// appendAligned appends buf at a 4-aligned EOF and returns its offset.
func (c *CacheFile) appendAligned(buf []byte) (uint32, error) {
	end, err := c.f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("mailindex: cache seek: %w", err)
	}
	if pad := end & 3; pad != 0 {
		if _, err := c.f.Write(make([]byte, 4-pad)); err != nil {
			return 0, fmt.Errorf("mailindex: cache pad: %w", err)
		}
		end += 4 - pad
	}
	if end > 0x3fffffff {
		return 0, fmt.Errorf("mailindex: cache file full (packed offsets carry 28 bits): purge required")
	}
	if _, err := c.f.Write(buf); err != nil {
		return 0, fmt.Errorf("mailindex: cache append: %w", err)
	}
	return uint32(end), nil
}

// packCacheOffset / unpackCacheOffset are the reference's
// mail_index_uint32_to_offset / mail_index_offset_to_uint32: a 4-aligned
// 30-bit offset spread over four 7-bit groups with the high bit of every
// byte set, so a partially-written value can never look valid.
func packCacheOffset(off uint32) uint32 {
	off >>= 2
	v := 0x00000080 | (off & 0x0000007f) |
		0x00008000 | ((off & 0x00003f80) >> 7 << 8) |
		0x00800000 | ((off & 0x001fc000) >> 14 << 16) |
		0x80000000 | ((off & 0x0fe00000) >> 21 << 24)
	// The reference stores this big-endian on the wire.
	return v>>24 | v>>8&0xff00 | v<<8&0xff0000 | v<<24
}

func unpackCacheOffset(stored uint32) uint32 {
	v := stored>>24 | stored>>8&0xff00 | stored<<8&0xff0000 | stored<<24
	if v&0x80808080 != 0x80808080 {
		return 0
	}
	return ((v & 0x0000007f) |
		(v&0x00007f00)>>8<<7 |
		(v&0x007f0000)>>16<<14 |
		(v&0x7f000000)>>24<<21) << 2
}
