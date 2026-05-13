package smtp

import (
	"bufio"
	"net"
	"strings"
	"sync"

	"github.com/0kaba0hub/yarilo/internal/xclient"
)

// xclientListener wraps a net.Listener so every accepted connection is an
// xclientConn that transparently handles the XCLIENT SMTP extension.
type xclientListener struct{ net.Listener }

func (l *xclientListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return newXClientConn(c), nil
}

// xclientConn wraps net.Conn to handle XCLIENT (RFC / Postfix extension).
//
// Read side: lines starting with "XCLIENT " are intercepted, parsed (updating
// remoteAddr), answered with "220 2.0.0 OK\r\n", and hidden from go-smtp.
//
// Write side: go-smtp calls textproto.PrintfLine which flushes after every
// EHLO capability line, so each "250-..." arrives as its own Write call.
// When we detect the final "250 " after one or more "250-" lines we inject
// "250-XCLIENT ADDR NAME\r\n" first, advertising the capability to the peer.
type xclientConn struct {
	net.Conn
	br         *bufio.Reader
	pending    []byte
	inMulti250 bool // true while inside a 250- multi-line response

	mu         sync.RWMutex
	remoteAddr net.Addr
}

func newXClientConn(c net.Conn) *xclientConn {
	return &xclientConn{
		Conn:       c,
		br:         bufio.NewReader(c),
		remoteAddr: c.RemoteAddr(),
	}
}

// RemoteAddr returns the XCLIENT-updated address (or the real TCP addr before
// any XCLIENT command is received).
func (c *xclientConn) RemoteAddr() net.Addr {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.remoteAddr
}

// Read returns data to go-smtp, intercepting XCLIENT lines.
func (c *xclientConn) Read(b []byte) (int, error) {
	for {
		if len(c.pending) > 0 {
			n := copy(b, c.pending)
			c.pending = c.pending[n:]
			return n, nil
		}

		line, err := c.br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(strings.ToUpper(trimmed), "XCLIENT ") {
				c.handleXClient(trimmed)
				c.Conn.Write([]byte("220 2.0.0 OK\r\n")) //nolint:errcheck
				if err != nil {
					return 0, err
				}
				continue
			}
			n := copy(b, []byte(line))
			if n < len(line) {
				c.pending = []byte(line)[n:]
			}
			return n, err
		}
		if err != nil {
			return 0, err
		}
	}
}

// Write intercepts go-smtp's output to inject the XCLIENT capability line
// into EHLO multi-line responses.
func (c *xclientConn) Write(b []byte) (int, error) {
	line := strings.TrimRight(string(b), "\r\n")
	switch {
	case strings.HasPrefix(line, "250-"):
		c.inMulti250 = true
	case strings.HasPrefix(line, "250 ") && c.inMulti250:
		c.inMulti250 = false
		if _, err := c.Conn.Write([]byte("250-XCLIENT ADDR NAME\r\n")); err != nil {
			return 0, err
		}
	default:
		c.inMulti250 = false
	}
	return c.Conn.Write(b)
}

func (c *xclientConn) handleXClient(line string) {
	attrs, err := xclient.Parse(line)
	if err != nil {
		return
	}
	if attrs.Addr == "" {
		return
	}
	ip := net.ParseIP(attrs.Addr)
	if ip == nil {
		return
	}
	c.mu.Lock()
	c.remoteAddr = &net.TCPAddr{IP: ip}
	c.mu.Unlock()
}
