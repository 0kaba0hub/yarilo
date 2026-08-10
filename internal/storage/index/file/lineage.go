package file

import (
	"encoding/binary"
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
const (
	lineageHdrSize     = 16
	lineageUnknown     = 0 // no extension, or a base written before it existed
	lineageRecordAlign = 4
)

type lineageHdr struct {
	Lineage       uint32
	FoldedLineage uint32
	FoldedOffset  uint64
}

func encodeLineageHdr(h lineageHdr) []byte {
	b := make([]byte, lineageHdrSize)
	binary.LittleEndian.PutUint32(b[0:4], h.Lineage)
	binary.LittleEndian.PutUint32(b[4:8], h.FoldedLineage)
	binary.LittleEndian.PutUint64(b[8:16], h.FoldedOffset)
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
	if len(b) >= lineageHdrSize {
		h.FoldedOffset = binary.LittleEndian.Uint64(b[8:16])
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
