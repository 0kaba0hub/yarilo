// Package crlf turns bare LF into CRLF as bytes are read, without holding the
// message in memory.
//
// Mail this server writes is CRLF-terminated before it reaches the disk, so
// nothing here changes it. A record another dbox v2 implementation wrote can be
// stored with bare LF, and serving those bytes as they lie puts bare LF on the
// wire, which RFC 3501 does not allow (#1527).
//
// Streaming rather than rewriting on import: converting the stored bytes would
// be exactly the conversion the record format exists to avoid.
package crlf

import (
	"bytes"
	"io"
)

// Reader wraps r and terminates every line with CRLF.
//
// Idempotent: a CR already in front of an LF is left alone, so text that is
// already CRLF passes through byte for byte and a second wrapping changes
// nothing. That property is what lets the wrapper sit on a path where most
// bodies need no conversion at all.
//
// A body that needs no conversion is read straight into the caller's buffer and
// costs one scan and nothing else -- no intermediate buffer, no allocation. The
// holdover below is allocated the first time a bare LF is actually found, so
// ordinary mail never pays for it. That matters because this sits on the fetch
// path, where reading a 512 KB body to serve its header was itself a defect
// (#1517).
type Reader struct {
	r    io.Reader
	held []byte // source bytes read but not yet converted
	last byte   // last byte handed out, to decide whether an LF needs a CR
}

// New returns a Reader over r.
func New(r io.Reader) *Reader { return &Reader{r: r} }

func (c *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Anything held back from a previous call is converted first: it is
	// already-read source, and re-reading is not possible.
	if len(c.held) > 0 {
		n, used := c.convert(p, c.held)
		c.held = c.held[used:]
		if len(c.held) == 0 {
			c.held = nil
		}
		return n, nil
	}

	n, err := c.r.Read(p)
	if n == 0 {
		return 0, err
	}
	src := p[:n]

	// The common case: nothing to insert. The bytes are already where the
	// caller wants them.
	if !needsConversion(src, c.last) {
		c.last = src[n-1]
		return n, err
	}

	// Something must be inserted, so the converted form may not fit. Copy the
	// source aside, convert as much as the caller's buffer takes, and hold the
	// rest for the next call.
	held := make([]byte, n)
	copy(held, src)
	out, used := c.convert(p, held)
	if used < len(held) {
		c.held = held[used:]
		// The read error, if any, is not lost: it surfaces once everything
		// held has been handed out and the next Read reaches the source again.
		return out, nil
	}
	return out, err
}

// needsConversion reports whether src contains an LF that is not preceded by a
// CR, given the byte handed out before it.
func needsConversion(src []byte, last byte) bool {
	for i := 0; ; {
		j := bytes.IndexByte(src[i:], '\n')
		if j < 0 {
			return false
		}
		at := i + j
		prev := last
		if at > 0 {
			prev = src[at-1]
		}
		if prev != '\r' {
			return true
		}
		i = at + 1
	}
}

// convert writes the CRLF form of src into dst, and reports how many bytes of
// src were consumed. It stops when dst is full.
func (c *Reader) convert(dst, src []byte) (written, used int) {
	for used < len(src) {
		b := src[used]
		if b == '\n' && c.last != '\r' {
			if written+1 > len(dst) {
				return written, used
			}
			// The CR goes out and the LF is left unconsumed: on the next pass
			// last is CR, so the LF is written plainly. Splitting the pair this
			// way keeps progress on a one-byte buffer, where holding both back
			// would loop forever.
			dst[written] = '\r'
			written++
			c.last = '\r'
			continue
		}
		if written+1 > len(dst) {
			return written, used
		}
		dst[written] = b
		written++
		used++
		c.last = b
	}
	return written, used
}
