package imap

import (
	"bufio"
	"net"
	"strings"
	"sync"

	"github.com/0kaba0hub/yarilo/internal/xclient"
)

// xclientImapListener wraps a net.Listener so every accepted connection is an
// xclientImapConn that transparently handles the XCLIENT IMAP extension.
// Only connections whose remote IP matches one of trustedNets may send XCLIENT;
// an empty slice means no peer is trusted (XCLIENT is silently discarded).
type xclientImapListener struct {
	net.Listener
	trustedNets []*net.IPNet
}

func (l *xclientImapListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return newXClientImapConn(c, l.trustedNets), nil
}

// xclientImapConn wraps net.Conn to handle the XCLIENT pre-auth IMAP command.
//
// Read side: IMAP commands of the form "tag XCLIENT key=val …" are intercepted,
// parsed (updating remoteAddr when the peer is trusted), answered with
// "tag OK XCLIENT\r\n", and hidden from go-imap/v2.
//
// Write side: when go-imap/v2 sends "* CAPABILITY …" to a trusted peer,
// "XCLIENT" is appended to the capability list so the relay knows it can use it.
type xclientImapConn struct {
	net.Conn
	br          *bufio.Reader
	pending     []byte
	trustedNets []*net.IPNet

	mu         sync.RWMutex
	remoteAddr net.Addr
}

func newXClientImapConn(c net.Conn, trustedNets []*net.IPNet) *xclientImapConn {
	return &xclientImapConn{
		Conn:        c,
		br:          bufio.NewReader(c),
		remoteAddr:  c.RemoteAddr(),
		trustedNets: trustedNets,
	}
}

// RemoteAddr returns the XCLIENT-updated address (or the real TCP addr).
func (c *xclientImapConn) RemoteAddr() net.Addr {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.remoteAddr
}

// Read returns data to go-imap/v2, intercepting XCLIENT commands.
func (c *xclientImapConn) Read(b []byte) (int, error) {
	for {
		if len(c.pending) > 0 {
			n := copy(b, c.pending)
			c.pending = c.pending[n:]
			return n, nil
		}

		line, err := c.br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			fields := strings.Fields(trimmed)
			// IMAP command: "tag XCLIENT key=val …"
			if len(fields) >= 2 && strings.ToUpper(fields[1]) == "XCLIENT" {
				tag := fields[0]
				if c.isTrusted() {
					c.handleXClient(trimmed)
				}
				c.Conn.Write([]byte(tag + " OK XCLIENT\r\n")) //nolint:errcheck
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

// isTrusted reports whether the TCP peer is in trustedNets.
func (c *xclientImapConn) isTrusted() bool {
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

// Write intercepts go-imap/v2's CAPABILITY response to inject "XCLIENT"
// for trusted peers, so the relay knows it may send XCLIENT commands.
func (c *xclientImapConn) Write(b []byte) (int, error) {
	if c.isTrusted() {
		line := strings.TrimRight(string(b), "\r\n")
		if strings.HasPrefix(line, "* CAPABILITY ") {
			return c.Conn.Write([]byte(line + " XCLIENT\r\n"))
		}
	}
	return c.Conn.Write(b)
}

func (c *xclientImapConn) handleXClient(line string) {
	// line: "tag XCLIENT ADDR=x.x.x.x [...]" — strip tag to get "XCLIENT …"
	_, rest, _ := strings.Cut(line, " ")
	attrs, err := xclient.Parse(rest)
	if err != nil || attrs.Addr == "" {
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
