package jmaplogin

import (
	"crypto/tls"
	"fmt"
	"net"
	"strconv"

	"github.com/yarilomail/yarilo/internal/cluster/proto"
)

// StaticRouter sends every user to one address. The standalone shape has no
// director, so the route comes from config instead of a lookup.
type StaticRouter struct{ Addr string }

func (s StaticRouter) Backend(string, string) (string, error) {
	if s.Addr == "" {
		return "", fmt.Errorf("jmap-login: no backend address configured")
	}
	return s.Addr, nil
}

// DirectorRouter asks yarilo-director which pod owns the user. One user is
// pinned to one pod for every protocol, so this returns the same pod the user's
// IMAP session would reach.
type DirectorRouter struct {
	Addr    string
	TLS     *tls.Config
	Tag     string
	LocalIP string
	// Port overrides the port the ring reports, since the backend's JMAP
	// container listens on its own number rather than the ring's.
	Port int
}

func (d DirectorRouter) Backend(username, sessionID string) (string, error) {
	var (
		c   *proto.Conn
		err error
	)
	if d.TLS != nil {
		c, err = proto.DialTLS(d.Addr, d.LocalIP, 0, d.TLS)
	} else {
		c, err = proto.Dial(d.Addr, d.LocalIP, 0)
	}
	if err != nil {
		return "", fmt.Errorf("jmap-login: director dial: %w", err)
	}
	defer c.Close() //nolint:errcheck

	res, err := c.Lookup(sessionID, username, d.Tag, service)
	if err != nil {
		return "", fmt.Errorf("jmap-login: director lookup: %w", err)
	}
	return d.applyPort(res.Addr), nil
}

// applyPort swaps in the JMAP container port, keeping the host the ring chose.
func (d DirectorRouter) applyPort(addr string) string {
	if d.Port == 0 || addr == "" {
		return addr
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return net.JoinHostPort(host, strconv.Itoa(d.Port))
}
