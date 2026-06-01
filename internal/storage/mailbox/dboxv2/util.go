package dboxv2

import (
	"bufio"
	"errors"
	"io"
)

// byteReader wraps any io.Reader as a buffered byte-by-byte +
// delimiter-aware reader. Single bufio.Reader internally, just so
// tests and metadata parsing don't each spin up their own.
type byteReader struct{ br *bufio.Reader }

func newByteReader(r io.Reader) *byteReader {
	if br, ok := r.(*bufio.Reader); ok {
		return &byteReader{br: br}
	}
	return &byteReader{br: bufio.NewReader(r)}
}

func (b *byteReader) Read(p []byte) (int, error) { return b.br.Read(p) }

func (b *byteReader) ReadByte() (byte, error) { return b.br.ReadByte() }

// ReadUntil reads up to AND INCLUDING delim. The returned slice
// is a fresh copy (not the internal buffer's view) so callers may
// retain it past the next Read. Returns ErrUnexpectedEOF when EOF
// hits before delim is found.
func (b *byteReader) ReadUntil(delim byte) ([]byte, error) {
	line, err := b.br.ReadSlice(delim)
	if errors.Is(err, bufio.ErrBufferFull) {
		// Long line — drain the slow path via ReadBytes which
		// allocates a fresh slice and never returns ErrBufferFull.
		out, rerr := b.br.ReadBytes(delim)
		return out, rerr
	}
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(line))
	copy(out, line)
	return out, nil
}
