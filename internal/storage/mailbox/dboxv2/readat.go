package dboxv2

import (
	"bytes"
	"fmt"
	"io"
)

// FileHeaderSize reads the message-header size a dbox file announces in its
// first line, the M field of "2 M1e C<stamp>".
//
// Exported for a reader that is not a driver -- the import in #1524 walks a
// store another implementation wrote and has to take the size from the file
// rather than from any constant of ours.
func FileHeaderSize(r io.ReaderAt) (int, error) {
	first := make([]byte, 64)
	n, err := r.ReadAt(first, 0)
	if err != nil && n == 0 {
		return 0, fmt.Errorf("dboxv2: read file header: %w", err)
	}
	lf := bytes.IndexByte(first[:n], '\n')
	if lf < 0 {
		return 0, fmt.Errorf("dboxv2: file carries no header line")
	}
	size, perr := parseFileHeaderSize(first[:lf+1])
	if perr != nil {
		return 0, fmt.Errorf("dboxv2: file header announces no usable M: %w", perr)
	}
	return size, nil
}

// ReadRecordBodyAt returns the message body of the record at offset.
//
// headerSize comes from FileHeaderSize: a record in the middle of a file has no
// header line of its own, so the size cannot be read from beside it, and it is
// not a constant of this build either -- a store written elsewhere announces
// its own.
func ReadRecordBodyAt(r io.ReaderAt, offset int64, headerSize int) ([]byte, error) {
	if headerSize < messageHeaderSize {
		return nil, fmt.Errorf("dboxv2: header size %d is smaller than a message header", headerSize)
	}
	hdr := make([]byte, headerSize)
	if _, err := r.ReadAt(hdr, offset); err != nil {
		return nil, fmt.Errorf("dboxv2: read message header at %d: %w", offset, err)
	}
	mh, err := decodeMessageHeader(hdr)
	if err != nil {
		return nil, fmt.Errorf("dboxv2: at %d: %w", offset, err)
	}
	body := make([]byte, mh.Size)
	if _, err := r.ReadAt(body, offset+int64(headerSize)); err != nil {
		return nil, fmt.Errorf("dboxv2: read body at %d: %w", offset, err)
	}
	return body, nil
}
