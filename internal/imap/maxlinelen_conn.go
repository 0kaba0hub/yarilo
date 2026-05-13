package imap

import (
	"bufio"
	"fmt"
	"net"
)

// maxLineLenListener wraps a net.Listener and enforces a per-line byte limit
// on every accepted connection (imap_max_line_length, Dovecot default 64 KB).
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

// maxLineLenConn enforces a maximum command line length.
// Lines exceeding the limit close the connection immediately.
type maxLineLenConn struct {
	net.Conn
	br      *bufio.Reader
	pending []byte
	limit   int
}

func (c *maxLineLenConn) Read(b []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(b, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	line, err := c.br.ReadString('\n')
	if len(line) > c.limit {
		c.Conn.Close()
		return 0, fmt.Errorf("imap: command line length %d exceeds limit %d", len(line), c.limit)
	}
	if len(line) > 0 {
		n := copy(b, []byte(line))
		if n < len(line) {
			c.pending = []byte(line)[n:]
		}
		return n, err
	}
	return 0, err
}
