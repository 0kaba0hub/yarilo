package lmtp

import (
	"bufio"
	"net"
	"strings"
	"sync"

	"github.com/0kaba0hub/yarilo/internal/xclient"
)

// xclientListener wraps a net.Listener so every accepted connection transparently
// handles the XCLIENT LMTP extension (Postfix-compatible).
// Only peers whose IP matches trustedNets may send XCLIENT commands.
type xclientListener struct {
	net.Listener
	trustedNets []*net.IPNet
}

func (l *xclientListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return newXClientConn(c, l.trustedNets), nil
}

// xclientConn wraps net.Conn for XCLIENT handling in LMTP sessions.
//
// Read: lines beginning with "XCLIENT " are intercepted, parsed (updating
// remoteAddr when the peer is trusted), replied to with "220 2.0.0 OK\r\n",
// and hidden from go-smtp.
//
// Write: after LHLO, go-smtp sends a "250-" multi-line block followed by
// "250 ". We intercept the final "250 " and inject
// "250-XCLIENT ADDR NAME\r\n" before it so the upstream sees the capability.
type xclientConn struct {
	net.Conn
	br         *bufio.Reader
	pending    []byte
	inMulti250 bool

	trustedNets []*net.IPNet

	mu         sync.RWMutex
	remoteAddr net.Addr
}

func newXClientConn(c net.Conn, trustedNets []*net.IPNet) *xclientConn {
	return &xclientConn{
		Conn:        c,
		br:          bufio.NewReader(c),
		remoteAddr:  c.RemoteAddr(),
		trustedNets: trustedNets,
	}
}

func (c *xclientConn) RemoteAddr() net.Addr {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.remoteAddr
}

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
				if c.isTrusted() {
					c.handleXClient(trimmed)
				}
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

func (c *xclientConn) isTrusted() bool {
	if len(c.trustedNets) == 0 {
		return false
	}
	tcpAddr, ok := c.Conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return false
	}
	for _, n := range c.trustedNets {
		if n.Contains(tcpAddr.IP) {
			return true
		}
	}
	return false
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
