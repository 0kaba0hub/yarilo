// Package dboxindex reads the index files another dbox v2 implementation
// writes, for the one-shot import in #1524. Read-only and offline: a store
// whose server has been stopped, never one shared with a running instance.
package dboxindex

import (
	"encoding/binary"
	"fmt"
)

// Header says what the base index is and how much of the log it holds. Taken
// alone it is behind; the log alone has lost whatever rotation removed.
type Header struct {
	MajorVersion   uint8
	MinorVersion   uint8
	BaseHeaderSize uint16
	HeaderSize     uint32
	RecordSize     uint32

	IndexID       uint32
	Flags         uint32
	UIDValidity   uint32
	NextUID       uint32
	MessagesCount uint32

	LogFileSeq        uint32
	LogFileTailOffset uint32
	LogFileHeadOffset uint32
}

// majorVersion is the format this reader understands: a minor version may add
// fields, a major one moves them.
const majorVersion = 7

// minBaseHeader is the smallest header this reader can make sense of; a longer
// one is read through BaseHeaderSize and its unknown tail ignored.
const minBaseHeader = 76

// ParseHeader reads the base index header.
func ParseHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < minBaseHeader {
		return h, fmt.Errorf("dboxindex: index file is %d bytes, too short for a header", len(b))
	}
	h.MajorVersion = b[0]
	h.MinorVersion = b[1]
	if h.MajorVersion != majorVersion {
		return h, fmt.Errorf("dboxindex: index major version %d, this reader knows %d", h.MajorVersion, majorVersion)
	}
	le := binary.LittleEndian
	h.BaseHeaderSize = le.Uint16(b[2:])
	h.HeaderSize = le.Uint32(b[4:])
	h.RecordSize = le.Uint32(b[8:])
	h.IndexID = le.Uint32(b[16:])
	h.Flags = le.Uint32(b[20:])
	h.UIDValidity = le.Uint32(b[24:])
	h.NextUID = le.Uint32(b[28:])
	h.MessagesCount = le.Uint32(b[32:])
	h.LogFileSeq = le.Uint32(b[60:])
	h.LogFileTailOffset = le.Uint32(b[64:])
	h.LogFileHeadOffset = le.Uint32(b[68:])

	if int(h.BaseHeaderSize) < minBaseHeader {
		return h, fmt.Errorf("dboxindex: base header claims %d bytes, fewer than the fields it must carry", h.BaseHeaderSize)
	}
	if h.HeaderSize < uint32(h.BaseHeaderSize) {
		return h, fmt.Errorf("dboxindex: header size %d is smaller than its own base %d", h.HeaderSize, h.BaseHeaderSize)
	}
	if uint32(len(b)) < h.HeaderSize {
		return h, fmt.Errorf("dboxindex: header says %d bytes and the file is %d", h.HeaderSize, len(b))
	}
	if h.RecordSize == 0 {
		return h, fmt.Errorf("dboxindex: record size is zero")
	}
	return h, nil
}

// Record is one message as the base index carries it: the two fields every
// index has, everything else living in extensions.
type Record struct {
	UID   uint32
	Flags uint8

	// Keywords is filled by the caller from the keywords extension: the base
	// carries a bitmask and the log names, one thing only once both are read.
	Keywords []string

	// ExtData holds extension bytes that arrived in the log, not the base.
	ExtData map[string][]byte

	// Raw is the whole record: which extensions a store carries is the store's
	// business, so nothing is copied out in advance.
	Raw []byte
}

// ParseRecords reads the base index's record array, which is where an import
// gets its message list: the appends that created them are in log files
// rotation has since deleted.
func ParseRecords(b []byte, h Header) ([]Record, error) {
	start := int(h.HeaderSize)
	if start > len(b) {
		return nil, fmt.Errorf("dboxindex: records start at %d, past a %d-byte file", start, len(b))
	}
	area := b[start:]
	if uint32(len(area))/h.RecordSize < h.MessagesCount {
		return nil, fmt.Errorf("dboxindex: header counts %d messages and the file holds room for %d",
			h.MessagesCount, uint32(len(area))/h.RecordSize)
	}
	out := make([]Record, 0, h.MessagesCount)
	for i := uint32(0); i < h.MessagesCount; i++ {
		r := area[i*h.RecordSize:]
		rec := r[:h.RecordSize]
		out = append(out, Record{UID: binary.LittleEndian.Uint32(rec), Flags: rec[4], Raw: rec})
	}
	return out, nil
}
