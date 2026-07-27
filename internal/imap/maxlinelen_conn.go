package imap

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// maxLineLenListener wraps a net.Listener and enforces a per-line byte limit
// on every accepted connection (imap_max_line_length, default 64 KB).
type maxLineLenListener struct {
	net.Listener
	limit int
}

func (l *maxLineLenListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &maxLineLenConn{Conn: c, br: bufio.NewReader(c), limit: l.limit}, nil
}

// maxLineLenConn enforces imap_max_line_length on command lines only. IMAP
// literals ({N} / {N+}) are the wire mechanism for non-line-oriented data
// (APPEND bodies, large LOGIN args) and MUST be exempt from the limit: their
// declared byte ranges are passed through uncounted. Applying the limit below
// the protocol parser without this distinction would count an APPEND body as a
// single oversized "line" and drop the connection.
//
// On a genuine oversized command line the connection is closed, but a tagged
// BAD is written first so the client sees a protocol response rather than a
// bare disconnect.
type maxLineLenConn struct {
	net.Conn
	br      *bufio.Reader
	pending []byte
	// literal counts down the bytes of a declared literal still to pass through
	// without applying the line-length limit.
	literal int64
	limit   int
}

// Unwrap exposes the wrapped conn so the server can walk the wrapper chain to
// the *loginproto.PreambleConn carrying pre-auth state (#830). Since #828 put
// this wrapper ABOVE the PreambleListener, without this the walk stopped here
// and every co-located session started unauthenticated.
func (c *maxLineLenConn) Unwrap() net.Conn { return c.Conn }

func (c *maxLineLenConn) Read(b []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(b, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}

	// Literal mode: stream the declared bytes straight through, uncounted.
	if c.literal > 0 {
		max := len(b)
		if int64(max) > c.literal {
			max = int(c.literal)
		}
		n, err := c.br.Read(b[:max])
		c.literal -= int64(n)
		return n, err
	}

	line, err := c.br.ReadString('\n')
	if len(line) > c.limit {
		c.writeTaggedBad(line)
		c.Conn.Close()
		return 0, fmt.Errorf("imap: command line length %d exceeds limit %d", len(line), c.limit)
	}
	if n := literalSize(line); n > 0 {
		c.literal = n
	}
	if len(line) > 0 {
		n := copy(b, line)
		if n < len(line) {
			c.pending = []byte(line)[n:]
		}
		return n, err
	}
	return 0, err
}

// writeTaggedBad best-effort emits a tagged BAD for an over-limit command line
// so the client gets a protocol response before the socket closes. The tag is
// the first space-delimited token; a malformed line falls back to "*".
func (c *maxLineLenConn) writeTaggedBad(line string) {
	tag := "*"
	if f := strings.Fields(line); len(f) > 0 && len(f[0]) <= 64 {
		tag = f[0]
	}
	fmt.Fprintf(c.Conn, "%s BAD [TOOBIG] Command line too long\r\n", tag)
}

// literalSize returns the byte count of a literal declared at the end of an
// IMAP command line ("... {123}\r\n" or non-synchronizing "... {123+}\r\n").
// Returns 0 when the line does not end with a literal marker.
func literalSize(line string) int64 {
	s := strings.TrimRight(line, "\r\n")
	if !strings.HasSuffix(s, "}") {
		return 0
	}
	open := strings.LastIndexByte(s, '{')
	if open < 0 {
		return 0
	}
	num := s[open+1 : len(s)-1]
	num = strings.TrimSuffix(num, "+") // non-synchronizing literal
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
