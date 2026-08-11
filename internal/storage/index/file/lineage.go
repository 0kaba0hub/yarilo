package file

import (
	"encoding/binary"
	"io"
	"os"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// The base index and its log are paired by a lineage, so a reader can tell
// "this log holds only what was written after the base I am holding" from
// "this log belongs to a base that has already replaced mine".
//
// It is carried in a header extension rather than in a new file format. The
// index is the compatible on-disk format, extensions are how it is extended,
// and an unknown extension is skipped by a reader that does not know it — so
// nothing is converted, nothing is refused, and the file stays readable by
// anything else that reads this format. An index without the extension simply
// has no lineage, which readers must treat as "cannot prove freshness without
// the lock" rather than as an error; the next flush adds it.
//
// Why this is needed at all: a reader that drops the cross-process lock must
// not load a torn view while another process is mid-compaction. The lock
// prevented that by serialising. The pairing prevents it by making the state
// self-describing: the log says which base it belongs to, and the base says how
// far into the previous log it already reaches.
const extNameLineage = "lineage"

// lineage extension layout (16-byte header, no per-record bytes):
//
//	uint32 lineage        // the log written after this base carries this in its FileSeq
//	uint32 folded_lineage // the lineage of the log this base absorbed
//	uint64 folded_offset  // how far into that log the base already reaches
//	uint64 records_digest // over the records the base holds
const (
	lineageHdrSize     = 24
	lineageHdrMinSize  = 16
	lineageUnknown     = 0 // no extension, or a base written before it existed
	lineageRecordAlign = 4
)

type lineageHdr struct {
	Lineage       uint32
	FoldedLineage uint32
	FoldedOffset  uint64
	// RecordsDigest is over the records the base holds. It is what lets a
	// reader prove, rather than assume, that a rewritten base holds the records
	// it already has: several paths rewrite the base while folding the same
	// log, and offsets alone cannot tell those from a plain compaction. The
	// same reasoning as the mdbox map (#1228), and the same trap avoided.
	RecordsDigest uint64
}

func encodeLineageHdr(h lineageHdr) []byte {
	b := make([]byte, lineageHdrSize)
	binary.LittleEndian.PutUint32(b[0:4], h.Lineage)
	binary.LittleEndian.PutUint32(b[4:8], h.FoldedLineage)
	binary.LittleEndian.PutUint64(b[8:16], h.FoldedOffset)
	binary.LittleEndian.PutUint64(b[16:24], h.RecordsDigest)
	return b
}

// decodeLineageHdr reads the extension. A short or absent header yields the
// zero value, which means "unknown" — the conservative answer, and the one
// every index written before this extension gives.
func decodeLineageHdr(b []byte) lineageHdr {
	var h lineageHdr
	if len(b) >= 8 {
		h.Lineage = binary.LittleEndian.Uint32(b[0:4])
		h.FoldedLineage = binary.LittleEndian.Uint32(b[4:8])
	}
	if len(b) >= lineageHdrMinSize {
		h.FoldedOffset = binary.LittleEndian.Uint64(b[8:16])
	}
	if len(b) >= lineageHdrSize {
		h.RecordsDigest = binary.LittleEndian.Uint64(b[16:24])
	}
	return h
}

// readLineage returns the pairing recorded in the open base.
func readLineage(f *mailindex.File) lineageHdr {
	if f == nil {
		return lineageHdr{}
	}
	for i := range f.Extensions {
		if f.Extensions[i].Name == extNameLineage {
			return decodeLineageHdr(f.Extensions[i].HdrData)
		}
	}
	return lineageHdr{}
}

// setLineage writes the pairing into the base, adding the extension when the
// index predates it.
func setLineage(f *mailindex.File, h lineageHdr) error {
	data := encodeLineageHdr(h)
	for i := range f.Extensions {
		if f.Extensions[i].Name == extNameLineage {
			f.Extensions[i].HdrData = data
			f.Extensions[i].HdrSize = uint32(len(data))
			return nil
		}
	}
	return f.AddHeaderExtension(extNameLineage, data, lineageRecordAlign, 0)
}

// logLineageOf reads the lineage a log announces in its header. Zero when the
// log is absent, empty or unreadable — all of which mean "proves nothing".
func logLineageOf(indexPath string) uint32 {
	f, err := os.Open(indexPath + ".log")
	if err != nil {
		return lineageUnknown
	}
	defer f.Close()
	h, err := mailindex.DecodeLogHeader(f)
	if err != nil {
		return lineageUnknown
	}
	return h.FileSeq
}

// replayStart says where a reader should begin applying a log whose header
// carries logLineage, against a base carrying h.
//
//   - the base's own lineage: the log holds only transactions written after the
//     base, so it is replayed whole;
//   - the lineage the base folded: replay resumes past the folded offset —
//     replaying from the start would apply everything the base already contains
//     a second time, which is survivable only as long as every transaction type
//     happens to be idempotent;
//   - anything else, or no lineage at all: the pairing proves nothing, so the
//     caller falls back to what it did before.
func replayStart(h lineageHdr, logLineage uint32) (offset int64, paired bool) {
	if h.Lineage == lineageUnknown || logLineage == lineageUnknown {
		return 0, false
	}
	switch logLineage {
	case h.Lineage:
		return int64(mailindex.LogHeaderSize), true
	case h.FoldedLineage:
		if h.FoldedOffset < uint64(mailindex.LogHeaderSize) {
			return int64(mailindex.LogHeaderSize), true
		}
		return int64(h.FoldedOffset), true
	default:
		return 0, false
	}
}

// digestRecords hashes what a reader would serve from the base: every record's
// uid, flags and extension bytes, in file order. FNV-1a — a change detector for
// a file we wrote ourselves, not a defence against a forged one.
//
// It runs over records already in memory, so proving "this rewritten base holds
// what I hold" costs no I/O beyond the header peek.
func digestRecords(f *mailindex.File) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	write := func(b []byte) {
		for _, c := range b {
			h ^= uint64(c)
			h *= prime64
		}
	}
	var scratch [8]byte
	for _, rec := range f.Records {
		binary.LittleEndian.PutUint32(scratch[0:4], rec.UID)
		binary.LittleEndian.PutUint32(scratch[4:8], uint32(rec.Flags))
		write(scratch[:])
		for _, ext := range f.Extensions {
			write([]byte(ext.Name))
			write(rec.Ext[ext.Name])
		}
	}
	return h
}

// peekLineage reads the pairing out of a base without reading its records: the
// fixed header says how long the extended-header region is, and the lineage
// lives there. A few hundred bytes instead of the whole index, which is the
// point -- deciding whether a rewritten base needs reading at all must not cost
// reading it.
func peekLineage(path string) (lineageHdr, error) {
	f, err := os.Open(path)
	if err != nil {
		return lineageHdr{}, err
	}
	defer f.Close()
	hdrBuf := make([]byte, mailindex.HeaderMinSize)
	if _, err := io.ReadFull(f, hdrBuf); err != nil {
		return lineageHdr{}, err
	}
	hdr, err := mailindex.DecodeHeaderBytes(hdrBuf)
	if err != nil {
		return lineageHdr{}, err
	}
	if hdr.HeaderSize <= uint32(mailindex.HeaderMinSize) {
		return lineageHdr{}, nil
	}
	extBuf := make([]byte, hdr.HeaderSize-uint32(mailindex.HeaderMinSize))
	if _, err := io.ReadFull(f, extBuf); err != nil {
		return lineageHdr{}, err
	}
	exts, err := mailindex.DecodeExtHeaders(extBuf)
	if err != nil {
		return lineageHdr{}, err
	}
	for i := range exts {
		if exts[i].Name == extNameLineage {
			return decodeLineageHdr(exts[i].HdrData), nil
		}
	}
	return lineageHdr{}, nil
}
