package dboxv2

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"time"
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

// StoredRecord is one message found by walking a file's records.
type StoredRecord struct {
	Offset int64
	Body   []byte
	GUID   [16]byte
	// OrigMailbox is the B trailer key: the folder the message was first
	// saved to. It is not where the message is now -- nothing rewrites it
	// when a message is moved -- so it is a hint for a rebuild and not a
	// location.
	OrigMailbox string
	Received    time.Time
}

// WalkRecords visits every record in a dbox file in order.
//
// For a reader with no index: the records are self-describing, each one giving
// its own length, so a file can be walked without knowing anything about what
// references it. That is what makes recovery from the store alone possible, and
// what makes it lossy -- a record carries no flags and no keywords, and the
// folder it names is the one it was first saved to.
func WalkRecords(r io.ReaderAt, size int64, visit func(StoredRecord) error) error {
	headerSize, err := FileHeaderSize(r)
	if err != nil {
		return err
	}
	line := make([]byte, 64)
	n, _ := r.ReadAt(line, 0)
	lf := bytes.IndexByte(line[:n], '\n')
	if lf < 0 {
		return fmt.Errorf("dboxv2: file carries no header line")
	}

	for off := int64(lf + 1); off < size; {
		hdr := make([]byte, headerSize)
		if _, err := r.ReadAt(hdr, off); err != nil {
			return fmt.Errorf("dboxv2: read record header at %d: %w", off, err)
		}
		mh, err := decodeMessageHeader(hdr)
		if err != nil {
			return fmt.Errorf("dboxv2: at %d: %w", off, err)
		}
		body := make([]byte, mh.Size)
		bodyAt := off + int64(headerSize)
		if _, err := r.ReadAt(body, bodyAt); err != nil {
			return fmt.Errorf("dboxv2: read body at %d: %w", bodyAt, err)
		}

		rec := StoredRecord{Offset: off, Body: body}
		trailerAt := bodyAt + int64(mh.Size)
		end, err := readTrailerAt(r, trailerAt, size, &rec)
		if err != nil {
			return err
		}
		if err := visit(rec); err != nil {
			return err
		}
		off = end
	}
	return nil
}

// readTrailerAt parses the metadata block and returns where the next record
// starts.
func readTrailerAt(r io.ReaderAt, at, size int64, rec *StoredRecord) (int64, error) {
	limit := size - at
	if limit <= 0 {
		return 0, fmt.Errorf("dboxv2: no trailer at %d", at)
	}
	if limit > 64*1024 {
		limit = 64 * 1024
	}
	buf := make([]byte, limit)
	n, err := r.ReadAt(buf, at)
	if n == 0 && err != nil {
		return 0, fmt.Errorf("dboxv2: read trailer at %d: %w", at, err)
	}
	buf = buf[:n]
	if !bytes.HasPrefix(buf, []byte(magicPost)) {
		return 0, fmt.Errorf("dboxv2: no metadata block at %d", at)
	}
	pos := len(magicPost)
	for {
		nl := bytes.IndexByte(buf[pos:], '\n')
		if nl < 0 {
			return 0, fmt.Errorf("dboxv2: trailer at %d does not end", at)
		}
		line := buf[pos : pos+nl]
		pos += nl + 1
		if len(line) == 0 {
			// The blank line closes the block; the next record starts here.
			return at + int64(pos), nil
		}
		value := string(bytes.TrimSpace(line[1:]))
		switch line[0] {
		case metaKeyGUID:
			if raw, derr := hexDecode(value); derr == nil && len(raw) == 16 {
				copy(rec.GUID[:], raw)
			}
		case metaKeyReceived:
			if v, perr := strconv.ParseInt(value, 16, 64); perr == nil {
				rec.Received = time.Unix(v, 0).UTC()
			}
		case metaKeyOrigMailbox:
			// Verbatim: a folder name may contain spaces.
			rec.OrigMailbox = string(line[1:])
		}
	}
}
