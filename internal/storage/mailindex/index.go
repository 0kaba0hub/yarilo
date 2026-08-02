package mailindex

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// File is the parsed contents of one .index file (header + ext
// table + records). Designed for whole-file read/write — Phase 1
// does not implement streaming partial reads. Higher layers
// (per-folder index driver, mdbox map) keep their own in-memory
// state and call Recreate when they need to flush.
type File struct {
	Header     Header
	Extensions []Extension
	Records    []*Record
	Layout     RecordLayout
}

// Open reads and parses the .index file at path. Returns
// (file, nil) on success, (nil, err) on any I/O or
// validation failure. Use Read for an already-opened io.Reader.
func Open(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("mailindex/open: %w", err)
	}
	defer f.Close()
	return Read(f)
}

// Read parses the full index from an io.Reader. The reader is
// consumed up to (but not past) the byte after the last record.
// Returns an empty File with zeroed records when the input
// contains only header + ext (a freshly initialised index).
func Read(r io.Reader) (*File, error) {
	// 1. Base header.
	hdrBuf := make([]byte, HeaderMinSize)
	if _, err := io.ReadFull(r, hdrBuf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("mailindex/read: header truncated: %w", ErrShortRead)
		}
		return nil, fmt.Errorf("mailindex/read: header: %w", err)
	}
	hdr, err := DecodeHeaderBytes(hdrBuf)
	if err != nil {
		return nil, fmt.Errorf("mailindex/read: %w", err)
	}
	// 2. Extended header region. Length is
	// HeaderSize - BaseHeaderSize, both straight from the header.
	if hdr.HeaderSize < uint32(hdr.BaseHeaderSize) {
		return nil, fmt.Errorf("mailindex/read: HeaderSize=%d < BaseHeaderSize=%d: %w",
			hdr.HeaderSize, hdr.BaseHeaderSize, ErrCorrupted)
	}
	extLen := hdr.HeaderSize - uint32(hdr.BaseHeaderSize)
	var exts []Extension
	if extLen > 0 {
		extBuf := make([]byte, extLen)
		if _, err := io.ReadFull(r, extBuf); err != nil {
			return nil, fmt.Errorf("mailindex/read: ext header: %w", err)
		}
		exts, err = DecodeExtHeaders(extBuf)
		if err != nil {
			return nil, fmt.Errorf("mailindex/read: %w", err)
		}
	}
	// 3. Records — repeat hdr.RecordSize-byte chunks until EOF.
	if hdr.RecordSize == 0 {
		return nil, fmt.Errorf("mailindex/read: header has zero RecordSize: %w", ErrCorrupted)
	}
	// Rebuild the layout from the extensions we just read so
	// EncodeRecord/DecodeRecord can find each extension's
	// (record_offset, record_size). We don't trust the
	// in-record offsets from the on-disk ext table blindly —
	// re-deriving them lets us catch corrupt layouts.
	layout, err := ComputeRecordLayout(exts)
	if err != nil {
		return nil, fmt.Errorf("mailindex/read: derive layout: %w", err)
	}
	// The canonical reader uses the on-disk ext_header.record_offset
	// as ground truth. If they disagree, the on-disk layout wins
	// because that's the offset records were actually written
	// at. We log/return ErrCorrupted so the caller can decide.
	if !layoutOffsetsMatch(exts, layout.Extensions) {
		// Preserve the on-disk layout exactly — important for
		// drop-in compat with files produced by other writers.
		layout = RecordLayout{
			RecordSize: hdr.RecordSize,
			Extensions: exts,
		}
	} else if layout.RecordSize != hdr.RecordSize {
		// Computed layout agrees on offsets but disagrees on
		// total size — that's a bug in our own writer.
		return nil, fmt.Errorf("mailindex/read: header RecordSize=%d, layout=%d: %w",
			hdr.RecordSize, layout.RecordSize, ErrCorrupted)
	}
	recBuf := make([]byte, hdr.RecordSize)
	var records []*Record
	for {
		_, err := io.ReadFull(r, recBuf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("mailindex/read: trailing partial record: %w", ErrShortRead)
			}
			return nil, fmt.Errorf("mailindex/read: record: %w", err)
		}
		rec, err := DecodeRecord(recBuf, layout)
		if err != nil {
			return nil, fmt.Errorf("mailindex/read: decode record: %w", err)
		}
		records = append(records, &rec)
	}
	return &File{
		Header:     hdr,
		Extensions: exts,
		Records:    records,
		Layout:     layout,
	}, nil
}

// layoutOffsetsMatch reports whether the on-disk extensions
// agree with the layout we'd compute fresh. Both slices are
// expected to be sorted by RecordOffset.
func layoutOffsetsMatch(onDisk, computed []Extension) bool {
	if len(onDisk) != len(computed) {
		return false
	}
	byName := make(map[string]Extension, len(computed))
	for _, e := range computed {
		byName[e.Name] = e
	}
	for _, e := range onDisk {
		c, ok := byName[e.Name]
		if !ok {
			return false
		}
		if c.RecordOffset != e.RecordOffset || c.RecordSize != e.RecordSize {
			return false
		}
	}
	return true
}

// NewFile builds an empty in-memory File ready for Recreate.
// Caller passes the desired extensions; this function computes
// the layout, populates header.HeaderSize and header.RecordSize
// to match, and returns a File with no records.
func NewFile(indexID uint32, exts []Extension) (*File, error) {
	layout, err := ComputeRecordLayout(exts)
	if err != nil {
		return nil, err
	}
	extBytes, err := EncodeExtHeaders(layout.Extensions)
	if err != nil {
		return nil, err
	}
	hdr := NewHeader(indexID)
	hdr.RecordSize = layout.RecordSize
	hdr.HeaderSize = uint32(HeaderMinSize) + uint32(len(extBytes))
	return &File{
		Header:     hdr,
		Extensions: layout.Extensions,
		Layout:     layout,
	}, nil
}

// AddHeaderExtension appends a header-only extension (RecordSize 0) and fixes up
// Header.HeaderSize, Header.RecordSize and Layout so Recreate accepts the file.
// It is a no-op when an extension with the same name already exists. Used to
// backfill extensions onto indexes written before that extension existed —
// simply appending to Extensions is not enough, because Recreate rejects a file
// whose HeaderSize no longer matches the encoded extension headers.
func (f *File) AddHeaderExtension(name string, hdrData []byte, recordAlign uint16, resetID uint32) error {
	return f.addExtension(name, hdrData, 0, recordAlign, resetID)
}

// AddRecordExtension appends an extension that also carries per-record bytes.
// Existing records have no bytes for it and encode as zeros on the next write,
// which is what makes adding one to an old index safe. No-op when present.
func (f *File) AddRecordExtension(name string, hdrData []byte, recordSize, recordAlign uint16, resetID uint32) error {
	return f.addExtension(name, hdrData, recordSize, recordAlign, resetID)
}

func (f *File) addExtension(name string, hdrData []byte, recordSize, recordAlign uint16, resetID uint32) error {
	for i := range f.Extensions {
		if f.Extensions[i].Name == name {
			return nil
		}
	}
	exts := append(append([]Extension(nil), f.Extensions...), Extension{
		Name:        name,
		HdrSize:     uint32(len(hdrData)),
		HdrData:     hdrData,
		RecordSize:  recordSize,
		RecordAlign: recordAlign,
		ResetID:     resetID,
	})
	layout, err := ComputeRecordLayout(exts)
	if err != nil {
		return err
	}
	extBytes, err := EncodeExtHeaders(layout.Extensions)
	if err != nil {
		return err
	}
	f.Extensions = layout.Extensions
	f.Layout = layout
	f.Header.RecordSize = layout.RecordSize
	f.Header.HeaderSize = uint32(HeaderMinSize) + uint32(len(extBytes))
	return nil
}

// ToRecreateInput packages the in-memory file into a
// RecreateInput ready to pass to Recreate. Convenience for
// callers that read, mutate, and write back the same file.
func (f *File) ToRecreateInput(path string) RecreateInput {
	return RecreateInput{
		Path:       path,
		Header:     f.Header,
		Extensions: f.Extensions,
		Records:    f.Records,
	}
}
