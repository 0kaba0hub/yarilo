// Package director — PeerDialer connects to sibling director replicas and
// propagates ring and user-directory changes so all replicas converge on the
// same state without a central coordinator.
package director

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

// PeerDialer maintains outgoing connections to peer director replicas.
// For each address in Peers it keeps a persistent, auto-reconnecting TCP
// connection.  The dial loop parses the server's HOST-HAND-* handshake to
// seed our local ring with the peer's current backend set, then processes
// unsolicited pushes (RING-CHANGE, USER-MOVED, USER-KICKED, PING) until the
// connection is lost.
type PeerDialer struct {
	srv       *Server
	peers     []string
	tlsCfg    *tls.Config
	localIP   string
	localPort int
}

// NewPeerDialer creates a PeerDialer bound to srv.
// localIP and localPort are sent in the ME handshake line so peers can
// log our identity; they have no functional effect.
func NewPeerDialer(srv *Server, peers []string, tlsCfg *tls.Config, localIP string, localPort int) *PeerDialer {
	return &PeerDialer{
		srv:       srv,
		peers:     peers,
		tlsCfg:    tlsCfg,
		localIP:   localIP,
		localPort: localPort,
	}
}

// Start spawns one goroutine per peer address and returns immediately.
// Each goroutine reconnects with exponential back-off (2 s → 60 s) until ctx
// is cancelled.
func (d *PeerDialer) Start(ctx context.Context) {
	for _, addr := range d.peers {
		addr := addr
		go d.RunPeer(ctx, addr)
	}
}

func (d *PeerDialer) RunPeer(ctx context.Context, addr string) {
	backoff := 2 * time.Second
	for {
		if err := d.connectPeer(ctx, addr); err != nil && ctx.Err() == nil {
			slog.Warn("director: peer lost", "peer", addr, "err", err, "reconnect_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

func (d *PeerDialer) connectPeer(ctx context.Context, addr string) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if d.tlsCfg != nil {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, d.tlsCfg)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	slog.Info("director: peer connected", "peer", addr)

	rd := bufio.NewReaderSize(conn, 4096)

	// Read server handshake: VERSION … HOST-HAND-START … HOST … HOST-HAND-END … DONE
	inHandshake := false
	for {
		line, rErr := rd.ReadString('\n')
		if rErr != nil {
			return fmt.Errorf("handshake read: %w", rErr)
		}
		line = strings.TrimRight(line, "\n")
		if line == "DONE" {
			break
		}
		switch {
		case line == "HOST-HAND-START":
			inHandshake = true
		case line == "HOST-HAND-END":
			inHandshake = false
		case inHandshake && strings.HasPrefix(line, "HOST\t"):
			d.applyHandshakeHost(line)
		}
	}

	// Send client handshake. PEER (#700) marks this connection as a peer
	// director replica, not a login proxy — a plain login client's generic
	// cluster/proto dialer never sends this line — so the accepting side
	// can stop a peer-originated broadcast (USER-KICKED) from being
	// relayed back out to peer connections and ping-ponging forever.
	ts := time.Now().Unix()
	for _, s := range []string{
		fmt.Sprintf("VERSION\t%s\t%d\t%d", protoName, majorVer, minorVer),
		fmt.Sprintf("ME\t%s\t%d\t%d", d.localIP, d.localPort, ts),
		"PEER\t1",
		"DONE",
	} {
		if _, wErr := fmt.Fprintf(conn, "%s\n", s); wErr != nil {
			return fmt.Errorf("handshake send: %w", wErr)
		}
	}

	// Main loop: process pushes from peer.
	for {
		line, rErr := rd.ReadString('\n')
		if rErr != nil {
			return fmt.Errorf("peer read: %w", rErr)
		}
		line = strings.TrimRight(line, "\n")
		if err := d.handlePeerLine(conn, line); err != nil {
			return err
		}
	}
}

// applyHandshakeHost registers a backend received in the server's opening
// handshake.  Wire format: HOST\t{ip}\t{port}\t{tag}\tD{down_ts}\tU{up_ts}\t{host}
// Up state is derived from timestamps: lastUp >= lastDown → Up, otherwise Down.
func (d *PeerDialer) applyHandshakeHost(line string) {
	fields := strings.Split(line, "\t")
	if len(fields) < 3 {
		return
	}
	ip := fields[1]
	port, err := strconv.Atoi(fields[2])
	if err != nil {
		return
	}
	tag := ""
	if len(fields) >= 4 {
		tag = fields[3]
	}
	var lastDown, lastUp int64
	for _, f := range fields[4:] {
		if len(f) < 2 {
			continue
		}
		v, pErr := strconv.ParseInt(f[1:], 10, 64)
		if pErr != nil {
			continue
		}
		switch f[0] {
		case 'D':
			lastDown = v
		case 'U':
			lastUp = v
		}
	}
	// lastDown==0: backend never went down → Up.
	// lastUp > lastDown: came up after last down → Up.
	// lastDown >= lastUp (incl. equal): went down at or after last up → Down.
	up := lastDown == 0 || lastUp > lastDown
	d.srv.ring.AddBackend(&ring.Backend{
		IP: ip, Port: port, Tag: tag, Up: up,
		LastUp: lastUp, LastDown: lastDown,
	})
	slog.Debug("director: peer handshake backend", "ip", ip, "port", port, "tag", tag, "up", up)
}

func (d *PeerDialer) handlePeerLine(conn net.Conn, line string) error {
	if line == "" {
		return nil
	}
	fields := strings.Split(line, "\t")
	switch fields[0] {
	case "PING":
		if _, err := fmt.Fprintf(conn, "PONG\n"); err != nil {
			return fmt.Errorf("pong: %w", err)
		}
	case "RING-CHANGE":
		d.applyRingChange(fields)
	case "USER-MOVED":
		d.applyUserMoved(fields)
	case "USER-KICKED":
		d.applyUserKicked(fields)
	case "USER-KILLED-EVERYWHERE":
		if len(fields) >= 2 {
			slog.Debug("director: peer user-killed-everywhere", "hash", fields[1])
		}
	}
	return nil
}

// applyRingChange processes: RING-CHANGE\t{ip}\t{event}\t{tag}
// "up"    → re-enable backend already known from handshake
// "down"  → remove backend from ring
// "flush" → mark backend as unavailable for new lookups
func (d *PeerDialer) applyRingChange(fields []string) {
	if len(fields) < 3 {
		return
	}
	ip, event := fields[1], fields[2]
	tag := ""
	if len(fields) >= 4 {
		tag = fields[3]
	}
	ts := time.Now().Unix()
	switch event {
	case "up":
		if !d.srv.ring.SetUp(ip, true, ts) {
			// Backend not in our ring (arrived after handshake, no port info).
			// It will be picked up on the next reconnect handshake.
			slog.Warn("director: peer RING-CHANGE up for unknown backend", "ip", ip, "tag", tag)
		} else {
			slog.Info("director: peer ring-change up", "ip", ip, "tag", tag)
		}
	case "down":
		d.srv.ring.RemoveBackend(ip)
		slog.Info("director: peer ring-change down", "ip", ip)
	case "flush":
		d.srv.ring.SetUp(ip, false, ts)
		slog.Info("director: peer ring-change flush", "ip", ip)
	}
}

// applyUserMoved processes: USER-MOVED\t{user}\t{ip}\t{port}
func (d *PeerDialer) applyUserMoved(fields []string) {
	if len(fields) < 4 {
		return
	}
	user, ip, portStr := fields[1], fields[2], fields[3]
	addr := net.JoinHostPort(ip, portStr)
	d.srv.userDir.Set(user, addr, false)
	slog.Debug("director: peer user-moved", "user", user, "backend", addr)
}

// applyUserKicked processes: USER-KICKED\t{user}
// Re-broadcasts to local login clients only (#700) so their sessions are
// terminated — NOT to other peer connections, which would relay it back
// out and ping-pong forever in a full-mesh topology (the origin director's
// own handleUserKick already broadcast directly to every peer).
func (d *PeerDialer) applyUserKicked(fields []string) {
	if len(fields) < 2 {
		return
	}
	user := fields[1]
	d.srv.broadcastToLogins(fmt.Sprintf("USER-KICKED\t%s", user))
	slog.Info("director: peer user-kicked, broadcasting locally", "user", user)
}
