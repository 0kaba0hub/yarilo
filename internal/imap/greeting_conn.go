package imap

import (
	"net"
	"strings"
)

// greetingListener wraps a net.Listener to rewrite the first
// "IMAP server ready" text in go-imap/v2's greeting line.
type greetingListener struct {
	net.Listener
	greeting string
}

func (l *greetingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &greetingConn{Conn: c, replacement: l.greeting}, nil
}

// greetingConn rewrites the first "IMAP server ready" occurrence in Write output.
// go-imap/v2 sends the greeting as a single Write call:
//
//   - OK [CAPABILITY ...] IMAP server ready\r\n
type greetingConn struct {
	net.Conn
	replacement string
	done        bool
}

func (c *greetingConn) Unwrap() net.Conn { return c.Conn }

func (c *greetingConn) Write(b []byte) (int, error) {
	if !c.done {
		s := string(b)
		if strings.Contains(s, "IMAP server ready") {
			s = strings.ReplaceAll(s, "IMAP server ready", c.replacement)
			c.done = true
			n, err := c.Conn.Write([]byte(s))
			if err != nil {
				return n, err
			}
			return len(b), nil
		}
	}
	return c.Conn.Write(b)
}
