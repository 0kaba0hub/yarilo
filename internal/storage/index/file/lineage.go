package file

import (
	"encoding/binary"
	"io"
	"os"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// The base index and its log are paired by a lineage extension: wire layout
// and the full freshness contract in INTERNALS.md §7. An index without it has
// no lineage, which readers treat as "cannot prove freshness without the lock"
// rather than as an error; the next flush adds it.
const extNameLineage = "lineage"

const (
	lineageHdrSize    = 24
	lineageHdrMinSize = 16
	lineageUnknown    = 0 // no extension, or a base written before it existed
	// legacyLogLineage is the FileSeq every pre-extension log carries; minted
	// lineages start above it (§7), so a base can never mistake one for its own.
	legacyLogLineage   = 1
	lineageRecordAlign = 4
)

type lineageHdr struct {
	Lineage       uint32
	FoldedLineage uint32
	FoldedOffset  uint64
	// RecordsDigest lets a reader prove, not assume, that a rewritten base holds
	// what it already has -- same reasoning as the mdbox map (#1228).
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

// replayStart says where to begin applying a log carrying logLineage against a
// base carrying h, by the §7 lineage table: the base's own lineage replays
// whole, the folded one resumes past folded_offset, anything else falls back.
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

// digestRecords hashes what a reader would serve, in file order, with FNV-1a --
// a change detector for our own file, not a defence against a forged one. Runs
// over records already in memory, so the proof costs no I/O beyond the header.
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

// peekExtHeaders reads only the base file's extension headers -- header plus
// the extension header block, never the records. Used where a handle must
// refresh what the extension HEADERS say without re-reading a base whose
// records it already holds.
func peekExtHeaders(path string) ([]mailindex.Extension, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hdrBuf := make([]byte, mailindex.HeaderMinSize)
	if _, err := io.ReadFull(f, hdrBuf); err != nil {
		return nil, err
	}
	hdr, err := mailindex.DecodeHeaderBytes(hdrBuf)
	if err != nil {
		return nil, err
	}
	if hdr.HeaderSize <= uint32(mailindex.HeaderMinSize) {
		return nil, nil
	}
	extBuf := make([]byte, hdr.HeaderSize-uint32(mailindex.HeaderMinSize))
	if _, err := io.ReadFull(f, extBuf); err != nil {
		return nil, err
	}
	return mailindex.DecodeExtHeaders(extBuf)
}

// peekLineage reads the pairing out of a base without reading its records: the
// fixed header says how long the extended-header region is, and the lineage
// lives there -- a few hundred bytes instead of the whole index, since deciding
// whether a rewritten base needs reading at all must not cost reading it.
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
